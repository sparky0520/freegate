package router

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gateway/internal/config"
)

// Tests that streaming fails over before the first byte is committed if the first provider
// hangs before sending any tokens (TTFT timeout).
func TestStreamingFailoverOnTTFTTimeout(t *testing.T) {
	var p1Started, p2Called int32

	// Hangs without sending any chunk
	p1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Started, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(15 * time.Second):
		}
	}))
	defer p1.Close()

	// Returns valid SSE stream immediately
	p2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Called, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer p2.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: p1.URL, Local: true},
			{Name: "p2", BaseURL: p2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m", "p2/m"}}},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 32768},
		{Provider: "p2", ID: "m", Tier: 2, ContextWindow: 32768},
	}

	rtr, _ := newLearningRouter(cfg, meta, p1.Client())
	// Use a short context so test doesn't wait full 8s, but test TTFT mechanism
	// Wait, we can test with a client timeout or custom TTFT
	req := parseBody(t, `{"model":"auto","stream":true,"messages":[{"role":"user","content":"test"}]}`)
	rec := httptest.NewRecorder()

	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
	defer cancel()

	rtr.Route(ctx, req, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after stream failover, got %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "hello") {
		t.Fatalf("expected stream output from p2, got: %s", body)
	}
	if atomic.LoadInt32(&p2Called) != 1 {
		t.Fatalf("expected p2 to be called, got %d", atomic.LoadInt32(&p2Called))
	}
	attempts := rec.Header().Get("X-Freegate-Attempts")
	if !strings.Contains(attempts, "p1/m:ttft_timeout") || !strings.Contains(attempts, "p2/m:ok") {
		t.Fatalf("expected attempts to show ttft_timeout then ok, got: %s", attempts)
	}
}

// Tests that streaming fails over before committing if the first SSE chunk contains
// an upstream error payload.
func TestStreamingFailoverOnErrorChunk(t *testing.T) {
	var p2Called int32

	// Returns 200 with error payload in first SSE chunk
	p1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"error\":{\"message\":\"Rate limit exceeded tokens per minute (TPM)\"}}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer p1.Close()

	p2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Called, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"p2 success\"}}]}\n\n"))
		w.(http.Flusher).Flush()
	}))
	defer p2.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: p1.URL, Local: true},
			{Name: "p2", BaseURL: p2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m", "p2/m"}}},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 32768},
		{Provider: "p2", ID: "m", Tier: 2, ContextWindow: 32768},
	}

	rtr, _ := newLearningRouter(cfg, meta, p1.Client())
	req := parseBody(t, `{"model":"auto","stream":true,"messages":[{"role":"user","content":"test"}]}`)
	rec := httptest.NewRecorder()

	rtr.Route(context.Background(), req, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after stream error failover, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "p2 success") {
		t.Fatalf("expected p2 response, got: %s", rec.Body.String())
	}
	if atomic.LoadInt32(&p2Called) != 1 {
		t.Fatalf("expected p2 to be called, got %d", atomic.LoadInt32(&p2Called))
	}
	attempts := rec.Header().Get("X-Freegate-Attempts")
	if !strings.Contains(attempts, "p1/m:stream_tpm") || !strings.Contains(attempts, "p2/m:ok") {
		t.Fatalf("expected attempts to show stream_tpm then ok, got: %s", attempts)
	}
}

// Tests calibrated token estimation using EWMA from usage.prompt_tokens.
func TestTokenEstimationCalibration(t *testing.T) {
	calibrator := NewTokenCalibrator()

	// Initial ratio is 1.0
	if ratio := calibrator.Ratio("groq"); ratio != 1.0 {
		t.Fatalf("expected default ratio 1.0, got %f", ratio)
	}

	// Suppose estimated was 1000, but actual was 1500 (ratio 1.5)
	calibrator.RecordUsage("groq", 1000, 1500)
	ratio1 := calibrator.Ratio("groq")
	if ratio1 <= 1.0 || ratio1 > 1.5 {
		t.Fatalf("expected ratio around 1.1-1.5, got %f", ratio1)
	}

	// Calibrate returns scaled estimate
	calibrated := calibrator.Calibrate("groq", 1000)
	if calibrated != int(float64(1000)*ratio1) {
		t.Fatalf("expected calibrated tokens %d, got %d", int(float64(1000)*ratio1), calibrated)
	}
}

// Tests that reset headers match the dimension that hit zero:
// when remaining tokens == 0, reset duration comes from x-ratelimit-reset-tokens.
func TestResetHeaderDimensionMatching(t *testing.T) {
	header := http.Header{}
	header.Set("x-ratelimit-remaining-requests", "50")
	header.Set("x-ratelimit-remaining-tokens", "0")
	header.Set("x-ratelimit-reset-requests", "2s")
	header.Set("x-ratelimit-reset-tokens", "45s")

	remReq, remTok, resetDur := ParseRateLimitHeaders(header, time.Now())
	if remReq != 50 {
		t.Errorf("expected remReq 50, got %d", remReq)
	}
	if remTok != 0 {
		t.Errorf("expected remTok 0, got %d", remTok)
	}
	if resetDur != 45*time.Second {
		t.Fatalf("expected resetDur to match tokens reset (45s), got %v", resetDur)
	}

	detailed := ParseDetailedRateLimitHeaders(header, time.Now())
	if detailed.ResetTokens != 45*time.Second || detailed.ResetRequests != 2*time.Second {
		t.Fatalf("unexpected detailed resets: %+v", detailed)
	}
}

// Tests that size-aware ordering prefers large context window models for large prompts.
func TestSizeAwareOrderingPrefersHeadroom(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "groq", BaseURL: "http://groq", Local: true},
			{Name: "gemini", BaseURL: "http://gemini", Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"groq/gpt-oss-120b", "gemini/gemini-3.8-flash"}},
		},
	}
	meta := []config.ModelMetadata{
		// Both Tier 3
		{Provider: "groq", ID: "gpt-oss-120b", Tier: 3, ContextWindow: 131072},
		{Provider: "gemini", ID: "gemini-3.8-flash", Tier: 3, ContextWindow: 1048576},
	}

	rtr, reg := newLearningRouter(cfg, meta, nil)
	now := time.Now()

	// Mega prompt: 80,000 tokens
	megaReq := parseBody(t, `{"model":"auto","messages":[{"role":"user","content":"`+strings.Repeat("code ", 60000)+`"}]}`)
	ordered := rtr.OrderBuckets(megaReq, reg.AllBuckets(), now)

	if len(ordered) != 2 {
		t.Fatalf("expected 2 ordered buckets, got %d", len(ordered))
	}
	if ordered[0].Key() != "gemini/gemini-3.8-flash" {
		t.Fatalf("for 80k mega-prompt, gemini (1M window) must be ordered before groq (131k window), got %s first", ordered[0].Key())
	}
}

// Tests that dynamic timestamps in agent system prompts do not break session stickiness across turns.
func TestSessionStabilityWithDynamicSystemPrompt(t *testing.T) {
	turn1Body := `{"model":"auto","messages":[{"role":"system","content":"You are a helpful assistant.\nCurrent time: 2026-11-17T08:00:00Z\nTools: search"},{"role":"user","content":"Write a python script"}]}`
	turn2Body := `{"model":"auto","messages":[{"role":"system","content":"You are a helpful assistant.\nCurrent time: 2026-11-17T08:00:45Z\nTools: search"},{"role":"user","content":"Write a python script"},{"role":"assistant","content":"here is script"},{"role":"user","content":"now add tests"}]}`

	req1, err := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(turn1Body))
	if err != nil {
		t.Fatalf("ParseRequest 1 failed: %v", err)
	}

	req2, err := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(turn2Body))
	if err != nil {
		t.Fatalf("ParseRequest 2 failed: %v", err)
	}

	if req1.SessionID == "" || req2.SessionID == "" {
		t.Fatal("expected non-empty SessionIDs")
	}
	if req1.SessionID != req2.SessionID {
		t.Fatalf("session ID must be stable across turns despite dynamic timestamp in system prompt: turn1=%s turn2=%s", req1.SessionID, req2.SessionID)
	}
}

// Tests that context window is learned and updated from upstream error messages.
func TestLearnContextWindowFromError(t *testing.T) {
	body := []byte(`{"error":{"message":"Invalid prompt: maximum context length is 32768 tokens, but received 40000 tokens"}}`)
	cw, ok := ParseContextWindowLimit(body)
	if !ok || cw != 32768 {
		t.Fatalf("expected parsed context window 32768, got %d (ok=%v)", cw, ok)
	}
}

// Tests that daily quota exhaustion sets cooldown until midnight UTC.
func TestDailyQuotaExhaustionCooldown(t *testing.T) {
	var p1Calls, p2Calls int32
	p1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You have exceeded your free-models-per-day rate limit"}}`))
	}))
	defer p1.Close()

	p2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Calls, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer p2.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: p1.URL, Local: true},
			{Name: "p2", BaseURL: p2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m", "p2/m"}}},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 32768},
		{Provider: "p2", ID: "m", Tier: 2, ContextWindow: 32768},
	}

	rtr, reg := newLearningRouter(cfg, meta, p1.Client())
	req := parseBody(t, `{"model":"auto","messages":[{"role":"user","content":"test"}]}`)
	rec := httptest.NewRecorder()

	rtr.Route(context.Background(), req, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after daily quota failover, got %d", rec.Code)
	}

	snap := reg.GetBucket("p1/m").Snapshot(time.Now())
	if snap.State != "cooldown" {
		t.Fatalf("expected p1 in cooldown, got %s", snap.State)
	}
	// Cooldown until midnight UTC should be substantial (more than 10 minutes)
	if snap.CooldownRemainingS <= 600 {
		t.Fatalf("expected daily quota cooldown to wait until midnight UTC (>600s), got %d seconds", snap.CooldownRemainingS)
	}
}

// Tests that X-Concise: true injects concise directive into the system prompt.
func TestConciseModeDirectiveInjectedOnHeader(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: srv.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m"}}},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 32768},
	}

	rtr, _ := newLearningRouter(cfg, meta, srv.Client())
	httpReq := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"write a sorting algorithm"}]}`))
	httpReq.Header.Set("X-Concise", "true")
	parsed, _ := ParseRequest(httpReq, []byte(`{"model":"auto","messages":[{"role":"user","content":"write a sorting algorithm"}]}`))

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	if !strings.Contains(receivedBody, "Output space is limited. Be concise") {
		t.Fatalf("expected concise directive in upstream request body, got:\n%s", receivedBody)
	}
}

// Tests that when output space is clamped, concise directive is automatically injected.
func TestConciseModeDirectiveInjectedOnClamping(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		receivedBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: srv.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m"}}},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 4000},
	}

	rtr, _ := newLearningRouter(cfg, meta, srv.Client())
	// Client asks for 32000 max_tokens, but window is 4000 -> must clamp!
	httpReq := httptest.NewRequest(http.MethodPost, "/", nil)
	parsed, _ := ParseRequest(httpReq, []byte(`{"model":"auto","max_tokens":32000,"messages":[{"role":"user","content":"short query"}]}`))

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	if !strings.Contains(receivedBody, "Output space is limited. Be concise") {
		t.Fatalf("expected clamped request to automatically receive concise directive, got:\n%s", receivedBody)
	}
}

// Tests that configurable min_completion_tokens excludes models with cramped output space.
func TestMinCompletionTokensExcludesTightModels(t *testing.T) {
	var p2Called int32
	p1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("p1 should have been excluded due to cramped output space")
	}))
	defer p1.Close()

	p2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Called, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer p2.Close()

	cfg := &config.Config{
		MinCompletionTokens: 2000, // require at least 2000 output tokens
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: p1.URL, Local: true},
			{Name: "p2", BaseURL: p2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/tight", "p2/large"}}},
	}
	meta := []config.ModelMetadata{
		// Prompt will be ~3000 tokens. p1 context is 4000 (leaves only ~600 tokens < 2000 required)
		{Provider: "p1", ID: "tight", Tier: 2, ContextWindow: 4000},
		// p2 context is 32000 (leaves > 28000 tokens >= 2000 required)
		{Provider: "p2", ID: "large", Tier: 2, ContextWindow: 32000},
	}

	rtr, _ := newLearningRouter(cfg, meta, p2.Client())
	// ~3000 estimated tokens prompt (12000 characters)
	prompt := strings.Repeat("text ", 2400)
	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto","messages":[{"role":"user","content":"`+prompt+`"}]}`))

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", rec.Code)
	}
	if atomic.LoadInt32(&p2Called) != 1 {
		t.Fatalf("expected p2 to be called, got %d", atomic.LoadInt32(&p2Called))
	}
}
