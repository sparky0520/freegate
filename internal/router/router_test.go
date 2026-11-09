package router

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gateway/internal/config"
	"gateway/internal/registry"
)

// Acceptance check test for Milestone 4:
// First fake returns 429 with Retry-After: 5, second succeeds;
// the client sees one success; the first bucket shows a cooldown of about 5s.
func TestMilestone4AcceptanceCheck(t *testing.T) {
	// Fake 1: returns 429 with Retry-After: 5
	fake1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"Rate limit exceeded","type":"rate_limit_error"}}`))
	}))
	defer fake1.Close()

	// Fake 2: returns 200 OK
	fake2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"success from fake2"}}]}`))
	}))
	defer fake2.Close()

	t.Setenv("KEY1", "val1")
	t.Setenv("KEY2", "val2")

	cfg := &config.Config{
		UnknownModel: "auto",
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake1.URL, APIKeyEnv: "KEY1"},
			{Name: "p2", BaseURL: fake2.URL, APIKeyEnv: "KEY2"},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/model1", "p2/model2"}},
		},
	}
	metadata := []config.ModelMetadata{
		{Provider: "p1", ID: "model1", Tier: 2, ContextWindow: 32768},
		{Provider: "p2", ID: "model2", Tier: 2, ContextWindow: 32768},
	}

	reg := registry.New(cfg, metadata)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rtr := New(cfg, reg, fake1.Client(), logger)

	reqPayload := `{"model":"auto","messages":[{"role":"user","content":"hi"}]}`
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqPayload))
	parsed, err := ParseRequest(httpReq, []byte(reqPayload))
	if err != nil {
		t.Fatalf("ParseRequest failed: %v", err)
	}

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	resp := rec.Result()

	// 1. Client sees one success (200 OK)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected client to see status 200, got %d. Body: %s", resp.StatusCode, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "success from fake2") {
		t.Errorf("expected body from fake2, got: %s", rec.Body.String())
	}

	// 2. Check X-Freegate-Attempts header
	attempts := resp.Header.Get("X-Freegate-Attempts")
	if !strings.Contains(attempts, "p1/model1:429") || !strings.Contains(attempts, "p2/model2:ok") {
		t.Errorf("expected attempts 'p1/model1:429,p2/model2:ok', got %q", attempts)
	}

	// 3. First bucket shows a cooldown of about 5s
	b1 := reg.GetBucket("p1/model1")
	if b1 == nil {
		t.Fatal("bucket p1/model1 not found")
	}
	snap1 := b1.Snapshot(time.Now())
	if snap1.State != "cooldown" {
		t.Fatalf("expected bucket 1 to be in cooldown state, got %s", snap1.State)
	}
	if snap1.CooldownRemainingS < 4 || snap1.CooldownRemainingS > 6 {
		t.Errorf("expected cooldown around 5s, got %d seconds", snap1.CooldownRemainingS)
	}

	// Second bucket should be OK
	b2 := reg.GetBucket("p2/model2")
	snap2 := b2.Snapshot(time.Now())
	if snap2.State != "ok" {
		t.Errorf("expected bucket 2 state ok, got %s", snap2.State)
	}
}

func TestFailoverOn5xx(t *testing.T) {
	fake500 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fake500.Close()

	fake200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer fake200.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake500.URL, Local: true},
			{Name: "p2", BaseURL: fake200.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/m1", "p2/m2"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
		{Provider: "p2", ID: "m2", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake500.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto"}`))
	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("X-Freegate-Attempts"), "p1/m1:500") {
		t.Errorf("expected p1/m1:500 attempt, got %s", rec.Header().Get("X-Freegate-Attempts"))
	}
}

func TestDisableOn401(t *testing.T) {
	fake401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer fake401.Close()

	fake200 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer fake200.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake401.URL, Local: true},
			{Name: "p2", BaseURL: fake200.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/m1", "p2/m2"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
		{Provider: "p2", ID: "m2", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake401.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto"}`))
	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	// Bucket p1/m1 should now be disabled
	if !reg.GetBucket("p1/m1").Snapshot(time.Now()).Disabled {
		t.Error("expected p1/m1 to be disabled after 401")
	}
}

func TestContextLength400Retry(t *testing.T) {
	fakeSmall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"maximum context length exceeded"}}`))
	}))
	defer fakeSmall.Close()

	fakeLarge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"handled large context"}}]}`))
	}))
	defer fakeLarge.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fakeSmall.URL, Local: true},
			{Name: "p2", BaseURL: fakeLarge.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/small", "p2/large"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "small", Tier: 2, ContextWindow: 4096},
		{Provider: "p2", ID: "large", Tier: 2, ContextWindow: 32768},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fakeSmall.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto"}`))
	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after context length retry, got %d", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("X-Freegate-Attempts"), "400_context_length") {
		t.Errorf("expected 400_context_length attempt recorded, got %s", rec.Header().Get("X-Freegate-Attempts"))
	}
}

func TestNonContext400ReturnsImmediately(t *testing.T) {
	fakeBadReq := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid temperature"}}`))
	}))
	defer fakeBadReq.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fakeBadReq.URL, Local: true},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fakeBadReq.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto"}`))
	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid temperature") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func TestFailoverOnTPMLimit(t *testing.T) {
	fakeTPMError := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"error": {
				"message": "Request too large for model openai/gpt-oss-120b in organization org_123 on tokens per minute (TPM): Limit 8000, Requested 8576, please reduce your message size and try again.",
				"type": "invalid_request_error",
				"code": "rate_limit_exceeded"
			}
		}`))
	}))
	defer fakeTPMError.Close()

	fakeSuccess := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"commit and push completed"}}]}`))
	}))
	defer fakeSuccess.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "groq", BaseURL: fakeTPMError.URL, Local: true},
			{Name: "backup", BaseURL: fakeSuccess.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"groq/gpt-120b", "backup/gpt-20b"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "groq", ID: "gpt-120b", Tier: 3, SupportsTools: true},
		{Provider: "backup", ID: "gpt-20b", Tier: 2, SupportsTools: true},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fakeTPMError.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	toolBody := `{"model":"auto","tools":[{"type":"function","function":{"name":"push"}}],"messages":[{"role":"user","content":"commit and push"}]}`
	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(toolBody))
	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after TPM failover, got %d. Body: %s", rec.Code, rec.Body.String())
	}

	if !strings.Contains(rec.Body.String(), "commit and push completed") {
		t.Errorf("expected body from backup model, got %s", rec.Body.String())
	}

	attempts := rec.Header().Get("X-Freegate-Attempts")
	if !strings.Contains(attempts, "400_tpm_limit") || !strings.Contains(attempts, "backup/gpt-20b:ok") {
		t.Errorf("expected attempts to show 400_tpm_limit failover, got %s", attempts)
	}

	// First bucket should be in cooldown
	snap1 := reg.GetBucket("groq/gpt-120b").Snapshot(time.Now())
	if snap1.State != "cooldown" {
		t.Errorf("expected gpt-120b to be placed in cooldown, got state %s", snap1.State)
	}
}

func TestProactiveRateLimitCooldown(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-ratelimit-remaining-requests", "0")
		w.Header().Set("x-ratelimit-reset-requests", "10s")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"last request"}}]}`))
	}))
	defer fake.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake.URL, Local: true},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto"}`))
	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Verify proactive cooldown was set because remaining requests was 0
	b := reg.GetBucket("p1/m1")
	snap := b.Snapshot(time.Now())
	if snap.State != "cooldown" {
		t.Errorf("expected proactive cooldown, got state %s", snap.State)
	}
	if snap.CooldownRemainingS < 8 || snap.CooldownRemainingS > 11 {
		t.Errorf("expected around 10s cooldown, got %d", snap.CooldownRemainingS)
	}
}

func TestWaitVsSwitch(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"waited and succeeded"}}]}`))
	}))
	defer fake.Close()

	cfg := &config.Config{
		MaxWaitSeconds: 3,
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake.URL, Local: true},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Set bucket in cooldown for 100ms
	b := reg.GetBucket("p1/m1")
	b.RecordCooldown(time.Now().Add(100*time.Millisecond), "test wait", time.Now())

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto"}`))
	rec := httptest.NewRecorder()

	start := time.Now()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 after wait, got %d", rec.Code)
	}
	if time.Since(start) < 90*time.Millisecond {
		t.Errorf("expected router to wait for cooldown, elapsed: %v", time.Since(start))
	}
}

func TestFailoverOn400BadRequest(t *testing.T) {
	// Fake 1: returns 400 Bad Request (e.g. model-specific tool format rejection)
	fake1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Failed to call a function. Unsupported format."}}`))
	}))
	defer fake1.Close()

	// Fake 2: returns 200 OK
	fake2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"succeeded on fallback"}}]}`))
	}))
	defer fake2.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake1.URL, Local: true},
			{Name: "p2", BaseURL: fake2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/m1", "p2/m2"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
		{Provider: "p2", ID: "m2", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake1.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto","messages":[{"role":"user","content":"test"}]}`))
	rec := httptest.NewRecorder()

	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after failing over on 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "succeeded on fallback") {
		t.Errorf("expected response from fake2, got: %s", rec.Body.String())
	}
	attempts := rec.Header().Get("X-Freegate-Attempts")
	if !strings.Contains(attempts, "p1/m1:400") || !strings.Contains(attempts, "p2/m2:ok") {
		t.Errorf("expected attempts to show failover from 400 to ok, got: %s", attempts)
	}
}

func TestQuotaExhaustion10mCooldown(t *testing.T) {
	// Fake 1: returns 429 quota exhaustion without Retry-After
	fake1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"You exceeded your current quota, please check your plan and billing details."}}`))
	}))
	defer fake1.Close()

	// Fake 2: returns 200 OK
	fake2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer fake2.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "gemini", BaseURL: fake1.URL, Local: true},
			{Name: "fallback", BaseURL: fake2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"gemini/flash", "fallback/model"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "gemini", ID: "flash", Tier: 1},
		{Provider: "fallback", ID: "model", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake1.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto","messages":[{"role":"user","content":"test"}]}`))
	rec := httptest.NewRecorder()

	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	snap := reg.GetBucket("gemini/flash").Snapshot(time.Now())
	if snap.CooldownRemainingS < 500 {
		t.Errorf("expected 10-minute cooldown on quota exhaustion, got %d seconds", snap.CooldownRemainingS)
	}
}

func TestAll400sReturns400(t *testing.T) {
	fake1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid parameter foo"}}`))
	}))
	defer fake1.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake1.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/m1"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake1.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"auto","messages":[{"role":"user","content":"test"}]}`))
	rec := httptest.NewRecorder()

	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when all candidates return 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Invalid parameter foo") {
		t.Errorf("expected original error message in response body, got: %s", rec.Body.String())
	}
}

func TestSanitizeReasoningContentInAssistantMessages(t *testing.T) {
	fakeStrict := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "reasoning_content") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"property 'reasoning_content' is unsupported"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer fakeStrict.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "strict", BaseURL: fakeStrict.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"strict/m1"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "strict", ID: "m1", Tier: 2},
	}
	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fakeStrict.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	reqBody := `{"model":"auto","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello","reasoning_content":"my deep thoughts"}]}`
	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(reqBody))
	rec := httptest.NewRecorder()

	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after sanitizing reasoning_content, got %d: %s", rec.Code, rec.Body.String())
	}
}
