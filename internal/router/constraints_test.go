package router

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"gateway/internal/config"
	"gateway/internal/registry"
)

// Milestone 5 Acceptance Check 1:
// A tool-using request never routes to a bucket with supports_tools = false.
func TestToolsConstraintNeverRoutesToNoTools(t *testing.T) {
	var fake1Called int32
	var fake2Called int32

	fake1NoTools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fake1Called, 1)
		t.Error("CRITICAL: request with tools routed to bucket with supports_tools=false!")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"bad"}}]}`))
	}))
	defer fake1NoTools.Close()

	fake2WithTools := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&fake2Called, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"tools-ok"}}]}`))
	}))
	defer fake2WithTools.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake1NoTools.URL, Local: true},
			{Name: "p2", BaseURL: fake2WithTools.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/m1", "p2/m2"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 1, SupportsTools: false},
		{Provider: "p2", ID: "m2", Tier: 2, SupportsTools: true},
	}

	reg := registry.New(cfg, meta)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rtr := New(cfg, reg, fake1NoTools.Client(), logger)

	toolReq := `{"model":"auto","tools":[{"type":"function","function":{"name":"calc"}}],"messages":[{"role":"user","content":"calc 2+2"}]}`
	parsed, err := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(toolReq))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if atomic.LoadInt32(&fake1Called) != 0 {
		t.Errorf("fake1 (no tools) was called %d times", atomic.LoadInt32(&fake1Called))
	}
	if atomic.LoadInt32(&fake2Called) != 1 {
		t.Errorf("expected fake2 (with tools) to be called once, got %d", atomic.LoadInt32(&fake2Called))
	}

	// Test when ONLY fake1 (no tools) is available
	cfgOnlyNoTools := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake1NoTools.URL, Local: true},
		},
	}
	regOnlyNoTools := registry.New(cfgOnlyNoTools, []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 1, SupportsTools: false},
	})
	rtrOnlyNoTools := New(cfgOnlyNoTools, regOnlyNoTools, fake1NoTools.Client(), logger)

	rec2 := httptest.NewRecorder()
	rtrOnlyNoTools.Route(context.Background(), parsed, rec2)

	if rec2.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when no tool-supporting bucket exists, got %d", rec2.Code)
	}
	if atomic.LoadInt32(&fake1Called) != 0 {
		t.Errorf("fake1 (no tools) was called %d times even when only one available", atomic.LoadInt32(&fake1Called))
	}
}

// Milestone 5 Acceptance Check 2:
// Private mode never reaches a trains_on_data = true provider even when everything else is exhausted.
func TestPrivateModeNeverReachesTrainingProvider(t *testing.T) {
	var privateCalled int32
	var trainingCalled int32

	// Private provider: fails with 500 error
	fakePrivate := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&privateCalled, 1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer fakePrivate.Close()

	// Training provider: would succeed with 200 OK, but MUST NEVER BE REACHED
	fakeTraining := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&trainingCalled, 1)
		t.Error("DATA LEAK: private mode routed request to provider with trains_on_data=true!")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"leaked"}}]}`))
	}))
	defer fakeTraining.Close()

	cfg := &config.Config{
		PrivateMode: true, // ENFORCE PRIVATE MODE
		Providers: []config.ProviderConfig{
			{Name: "private-p", BaseURL: fakePrivate.URL, Local: true, TrainsOnData: false},
			{Name: "training-p", BaseURL: fakeTraining.URL, Local: true, TrainsOnData: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"private-p/priv-m", "training-p/train-m"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "private-p", ID: "priv-m", Tier: 2},
		{Provider: "training-p", ID: "train-m", Tier: 3},
	}

	reg := registry.New(cfg, meta)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rtr := New(cfg, reg, fakePrivate.Client(), logger)

	reqPayload := `{"model":"auto","messages":[{"role":"user","content":"sensitive secret prompt"}]}`
	parsed, err := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(reqPayload))
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	// Result must be failure (Bad Gateway) because private provider failed and no other private provider exists
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway when private provider fails, got %d", rec.Code)
	}

	if atomic.LoadInt32(&privateCalled) != 1 {
		t.Errorf("expected private provider to be called once, got %d", atomic.LoadInt32(&privateCalled))
	}

	// CRITICAL CHECK: Training provider must have received 0 requests
	if atomic.LoadInt32(&trainingCalled) != 0 {
		t.Fatalf("VIOLATION: training provider was called %d times under private mode!", atomic.LoadInt32(&trainingCalled))
	}
}

func TestVisionConstraintNeverRoutesToNoVision(t *testing.T) {
	var noVisionCalled int32
	var visionCalled int32

	fakeNoVision := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&noVisionCalled, 1)
		t.Error("routed image request to no-vision bucket")
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeNoVision.Close()

	fakeVision := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&visionCalled, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"vision-ok"}}]}`))
	}))
	defer fakeVision.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fakeNoVision.URL, Local: true},
			{Name: "p2", BaseURL: fakeVision.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/m1", "p2/m2"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 1, SupportsVision: false},
		{Provider: "p2", ID: "m2", Tier: 2, SupportsVision: true},
	}

	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fakeVision.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	imageReq := `{"model":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,..."}}]}]}`
	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(imageReq))

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if atomic.LoadInt32(&noVisionCalled) != 0 {
		t.Errorf("no-vision was called %d times", atomic.LoadInt32(&noVisionCalled))
	}
	if atomic.LoadInt32(&visionCalled) != 1 {
		t.Errorf("expected vision bucket called once, got %d", atomic.LoadInt32(&visionCalled))
	}
}

func TestContextWindowConstraint(t *testing.T) {
	var smallCalled int32
	var largeCalled int32

	fakeSmall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&smallCalled, 1)
		t.Error("routed large prompt to bucket with too small context window")
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeSmall.Close()

	fakeLarge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&largeCalled, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"large-ok"}}]}`))
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
		{Provider: "p1", ID: "small", Tier: 1, ContextWindow: 2000},
		{Provider: "p2", ID: "large", Tier: 2, ContextWindow: 32000},
	}

	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fakeLarge.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Long prompt: 2000 bytes -> 500 estimated tokens + 2000 max_tokens = 2500 required (exceeds 2000)
	longPrompt := strings.Repeat("hello ", 400)
	reqJSON := `{"model":"auto","max_tokens":2000,"messages":[{"role":"user","content":"` + longPrompt + `"}]}`
	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(reqJSON))

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if atomic.LoadInt32(&smallCalled) != 0 {
		t.Errorf("small context window was called %d times", atomic.LoadInt32(&smallCalled))
	}
	if atomic.LoadInt32(&largeCalled) != 1 {
		t.Errorf("expected large context window to be called, got %d", atomic.LoadInt32(&largeCalled))
	}
}

func TestSessionStickiness(t *testing.T) {
	var p1Called int32
	var p2Called int32

	fake1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Called, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"p1"}}]}`))
	}))
	defer fake1.Close()

	fake2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p2Called, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"p2"}}]}`))
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
		// Initially p1 is Tier 1 (preferred for simple), p2 is Tier 2
		{Provider: "p1", ID: "m1", Tier: 1},
		{Provider: "p2", ID: "m2", Tier: 2},
	}

	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake1.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	sessionID := "agent-chat-session-42"

	// Request 1: simple request with session ID -> routes to p1/m1 (Tier 1 preferred for simple)
	httpReq1 := httptest.NewRequest(http.MethodPost, "/", nil)
	httpReq1.Header.Set("X-Session-Id", sessionID)
	p1, _ := ParseRequest(httpReq1, []byte(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))

	rec1 := httptest.NewRecorder()
	rtr.Route(context.Background(), p1, rec1)

	if atomic.LoadInt32(&p1Called) != 1 {
		t.Fatalf("expected p1 to be called, got p1=%d, p2=%d", atomic.LoadInt32(&p1Called), atomic.LoadInt32(&p2Called))
	}

	// Request 2: now send a COMPLEX request with code fences in the SAME session!
	// Without stickiness, complex requests would prefer p2/m2 (Tier 2).
	// But because of session stickiness, it MUST remain sticky to p1/m1!
	httpReq2 := httptest.NewRequest(http.MethodPost, "/", nil)
	httpReq2.Header.Set("X-Session-Id", sessionID)
	complexBody := "{\"model\":\"auto\",\"messages\":[{\"role\":\"user\",\"content\":\"```python\\nprint(1)\\n```\"}]}"
	p2, _ := ParseRequest(httpReq2, []byte(complexBody))

	rec2 := httptest.NewRecorder()
	rtr.Route(context.Background(), p2, rec2)

	if atomic.LoadInt32(&p1Called) != 2 {
		t.Errorf("expected session stickiness to keep routing to p1, got p1=%d", atomic.LoadInt32(&p1Called))
	}
	if atomic.LoadInt32(&p2Called) != 0 {
		t.Errorf("expected p2 to not be called for stickied session, got %d", atomic.LoadInt32(&p2Called))
	}

	// Request 3: a different session sending complex request -> routes to p2/m2 (Tier 2)
	httpReq3 := httptest.NewRequest(http.MethodPost, "/", nil)
	httpReq3.Header.Set("X-Session-Id", "different-session")
	p3, _ := ParseRequest(httpReq3, []byte(complexBody))

	rec3 := httptest.NewRecorder()
	rtr.Route(context.Background(), p3, rec3)

	if atomic.LoadInt32(&p2Called) != 1 {
		t.Errorf("expected different session to route to p2, got %d", atomic.LoadInt32(&p2Called))
	}
}

func TestUnknownModelFallbackToAuto(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"fallback-ok"}}]}`))
	}))
	defer fake.Close()

	cfg := &config.Config{
		UnknownModel: "auto",
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: fake.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{
			"auto": {Models: []string{"p1/m1"}},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m1", Tier: 2},
	}

	reg := registry.New(cfg, meta)
	rtr := New(cfg, reg, fake.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	// App hardcodes "gpt-4o"
	parsed, _ := ParseRequest(httptest.NewRequest(http.MethodPost, "/", nil), []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parsed, rec)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when unknown model falls back to auto, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "fallback-ok") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}
