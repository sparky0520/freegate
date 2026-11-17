package router

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gateway/internal/config"
	"gateway/internal/registry"
)

func newLearningRouter(cfg *config.Config, meta []config.ModelMetadata, client *http.Client) (*Router, *registry.Registry) {
	reg := registry.New(cfg, meta)
	return New(cfg, reg, client, slog.New(slog.NewTextHandler(io.Discard, nil))), reg
}

func parseBody(t *testing.T, body string) *ParsedRequest {
	t.Helper()
	p, err := ParseRequest(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil), []byte(body))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	return p
}

func chatBody(content string, extra string) string {
	return `{"model":"auto",` + extra + `"messages":[{"role":"user","content":"` + content + `"}]}`
}

func okHandler(calls *int32) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			atomic.AddInt32(calls, 1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}
}

// A client that goes away mid-request says nothing about the provider's health,
// so it must never count toward the circuit breaker.
func TestClientCancelDoesNotTripBreaker(t *testing.T) {
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The server only notices a client disconnect after the body is read.
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second): // safety net so a failing test cannot hang
		}
	}))
	defer hang.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: hang.URL, Local: true}},
		Aliases:   map[string]config.AliasConfig{"auto": {Models: []string{"p1/m"}}},
	}
	rtr, reg := newLearningRouter(cfg, []config.ModelMetadata{{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 32768}}, hang.Client())

	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		rtr.Route(ctx, parseBody(t, chatBody("hi", "")), httptest.NewRecorder())
		cancel()
	}

	snap := reg.GetBucket("p1/m").Snapshot(time.Now())
	if snap.ConsecutiveFailures != 0 || snap.State != "ok" {
		t.Fatalf("client cancels must not count as provider failures: failures=%d state=%s", snap.ConsecutiveFailures, snap.State)
	}
}

// After a context-length error the bucket learns a size ceiling: the next request
// of that size skips it (no wasted attempt), while small requests still use it.
func TestContextLengthErrorLearnsCeiling(t *testing.T) {
	var p1Calls, p2Calls int32
	p1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"maximum context length exceeded"}}`))
	}))
	defer p1.Close()
	p2 := httptest.NewServer(okHandler(&p2Calls))
	defer p2.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: p1.URL, Local: true},
			{Name: "p2", BaseURL: p2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m", "p2/m"}}},
	}
	// p1 advertises a much larger window than it really has.
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 131072},
		{Provider: "p2", ID: "m", Tier: 2, ContextWindow: 131072},
	}
	rtr, reg := newLearningRouter(cfg, meta, p1.Client())

	big := chatBody(strings.Repeat("a", 20000), "") // ~5000 estimated tokens

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parseBody(t, big), rec)
	if rec.Code != http.StatusOK || atomic.LoadInt32(&p1Calls) != 1 {
		t.Fatalf("first big request: code=%d p1Calls=%d", rec.Code, atomic.LoadInt32(&p1Calls))
	}
	if st := reg.GetBucket("p1/m").Snapshot(time.Now()); st.State != "ok" || st.MaxPromptTokens == 0 {
		t.Fatalf("p1 should stay usable and have learned a ceiling, got state=%s ceiling=%d", st.State, st.MaxPromptTokens)
	}

	// Same size again: p1 must be skipped entirely.
	rec = httptest.NewRecorder()
	rtr.Route(context.Background(), parseBody(t, big), rec)
	if rec.Code != http.StatusOK || atomic.LoadInt32(&p1Calls) != 1 {
		t.Fatalf("second big request should skip p1: code=%d p1Calls=%d", rec.Code, atomic.LoadInt32(&p1Calls))
	}

	// A small request is still routed to p1 first (it would 400 here, then fail over).
	rec = httptest.NewRecorder()
	rtr.Route(context.Background(), parseBody(t, chatBody("hi", "")), rec)
	if atomic.LoadInt32(&p1Calls) != 2 {
		t.Fatalf("small request should still reach p1, p1Calls=%d", atomic.LoadInt32(&p1Calls))
	}
}

// A 413 depends on the request's size. It must not cool the whole bucket down.
func TestPayloadTooLargeDoesNotCooldownBucket(t *testing.T) {
	var p1Calls int32
	p1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&p1Calls, 1)
		body, _ := io.ReadAll(r.Body)
		if len(body) > 2000 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"payload too large"}}`))
			return
		}
		okHandler(nil)(w, r)
	}))
	defer p1.Close()
	p2 := httptest.NewServer(okHandler(nil))
	defer p2.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: p1.URL, Local: true},
			{Name: "p2", BaseURL: p2.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m", "p2/m"}}},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 131072},
		{Provider: "p2", ID: "m", Tier: 2, ContextWindow: 131072},
	}
	rtr, reg := newLearningRouter(cfg, meta, p1.Client())

	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), parseBody(t, chatBody(strings.Repeat("a", 6000), "")), rec)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("X-Freegate-Attempts"), "p1/m:413") {
		t.Fatalf("expected 413 on p1 then success on p2, got %d attempts=%s", rec.Code, rec.Header().Get("X-Freegate-Attempts"))
	}
	if st := reg.GetBucket("p1/m").Snapshot(time.Now()); st.State != "ok" {
		t.Fatalf("413 must not cool the bucket down, state=%s", st.State)
	}

	rec = httptest.NewRecorder()
	rtr.Route(context.Background(), parseBody(t, chatBody("small", "")), rec)
	if !strings.HasSuffix(rec.Header().Get("X-Freegate-Attempts"), "p1/m:ok") {
		t.Fatalf("small request should be served by p1, attempts=%s", rec.Header().Get("X-Freegate-Attempts"))
	}
}

// A provider-reported remaining-token budget filters out requests that cannot fit,
// is waited on (not dropped) when it is the only option, and expires on its own.
func TestTokenBudgetFilter(t *testing.T) {
	srv := httptest.NewServer(okHandler(nil))
	defer srv.Close()

	cfg := &config.Config{
		MaxWaitSeconds: 10,
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: srv.URL, Local: true},
			{Name: "p2", BaseURL: srv.URL, Local: true},
		},
		Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m", "p2/m"}}},
	}
	meta := []config.ModelMetadata{
		{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 131072},
		{Provider: "p2", ID: "m", Tier: 2, ContextWindow: 131072},
	}
	rtr, reg := newLearningRouter(cfg, meta, srv.Client())

	now := time.Now()
	b1 := reg.GetBucket("p1/m")
	b1.RecordSuccess(10, 100, now)
	b1.RecordBudgetWindow(30*time.Second, now)

	bigReq := parseBody(t, chatBody(strings.Repeat("a", 4000), "")) // ~1000 est tokens > 100 left
	eligible, waiting := rtr.FilterEligible(bigReq, now)
	if len(eligible) != 1 || eligible[0].Key() != "p2/m" || len(waiting) != 1 || waiting[0].Key() != "p1/m" {
		t.Fatalf("big prompt should skip p1 (waiting), use p2: eligible=%v waiting=%v", keysOf(eligible), keysOf(waiting))
	}

	smallReq := parseBody(t, chatBody("hi", ""))
	if eligible, _ := rtr.FilterEligible(smallReq, now); len(eligible) != 2 {
		t.Fatalf("small prompt fits the remaining budget, expected both eligible, got %v", keysOf(eligible))
	}

	// The budget is only trusted until the reported window ends.
	if eligible, _ := rtr.FilterEligible(bigReq, now.Add(31*time.Second)); len(eligible) != 2 {
		t.Fatalf("budget must expire with its window, got %v", keysOf(eligible))
	}

	// With p2 unavailable the wait is longer than max_wait, so the client gets a
	// 429 whose Retry-After reflects the budget window rather than a 503.
	reg.GetBucket("p2/m").RecordCooldown(now.Add(time.Hour), "test", now)
	rec := httptest.NewRecorder()
	rtr.Route(context.Background(), bigReq, rec)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d: %s", rec.Code, rec.Body.String())
	}
}

func keysOf(bs []*registry.Bucket) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Key())
	}
	return out
}

// An oversized client max_tokens is clamped to what the window leaves instead of
// excluding the model, and the clamp for one bucket does not leak to the next.
func TestMaxTokensClampedToWindow(t *testing.T) {
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		t.Run(key, func(t *testing.T) {
			var gotMax1, gotMax2 int
			read := func(dst *int) http.HandlerFunc {
				return func(w http.ResponseWriter, r *http.Request) {
					var m map[string]json.RawMessage
					_ = json.NewDecoder(r.Body).Decode(&m)
					_ = json.Unmarshal(m[key], dst)
					if dst == &gotMax1 {
						w.WriteHeader(http.StatusInternalServerError) // force failover to p2
						return
					}
					okHandler(nil)(w, r)
				}
			}
			p1 := httptest.NewServer(read(&gotMax1))
			defer p1.Close()
			p2 := httptest.NewServer(read(&gotMax2))
			defer p2.Close()

			cfg := &config.Config{
				Providers: []config.ProviderConfig{
					{Name: "p1", BaseURL: p1.URL, Local: true},
					{Name: "p2", BaseURL: p2.URL, Local: true},
				},
				Aliases: map[string]config.AliasConfig{"auto": {Models: []string{"p1/m", "p2/m"}}},
			}
			meta := []config.ModelMetadata{
				{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 4000},
				{Provider: "p2", ID: "m", Tier: 2, ContextWindow: 200000},
			}
			rtr, _ := newLearningRouter(cfg, meta, p1.Client())

			req := parseBody(t, chatBody("hello", `"`+key+`":32000,`))
			rec := httptest.NewRecorder()
			rtr.Route(context.Background(), req, rec)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
			}

			want1 := maxCompletionFor(4000, req)
			if gotMax1 != want1 || want1 <= 0 || want1 >= 4000 {
				t.Errorf("p1 should receive a clamped %s=%d, got %d", key, want1, gotMax1)
			}
			if gotMax2 != 32000 {
				t.Errorf("clamp leaked into the next attempt: p2 got %s=%d, want 32000", key, gotMax2)
			}
		})
	}
}

// We never inject a max_tokens the client did not send.
func TestMaxTokensNotInjected(t *testing.T) {
	var sawKey bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&m)
		_, a := m["max_tokens"]
		_, b := m["max_completion_tokens"]
		sawKey = a || b
		okHandler(nil)(w, r)
	}))
	defer srv.Close()

	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "p1", BaseURL: srv.URL, Local: true}},
		Aliases:   map[string]config.AliasConfig{"auto": {Models: []string{"p1/m"}}},
	}
	rtr, _ := newLearningRouter(cfg, []config.ModelMetadata{{Provider: "p1", ID: "m", Tier: 2, ContextWindow: 4000}}, srv.Client())
	rtr.Route(context.Background(), parseBody(t, chatBody("hello", "")), httptest.NewRecorder())
	if sawKey {
		t.Fatal("router injected a max_tokens the client did not send")
	}
}

func TestParseRequestLimitAndTooLarge(t *testing.T) {
	body := []byte(`{"error":{"message":"Request too large for model x on tokens per minute (TPM): Limit 8000, Requested 8576, please reduce"}}`)
	limit, requested, ok := ParseRequestLimit(body)
	if !ok || limit != 8000 || requested != 8576 {
		t.Fatalf("got limit=%d requested=%d ok=%v", limit, requested, ok)
	}
	if _, _, ok := ParseRequestLimit([]byte(`{"error":{"message":"nope"}}`)); ok {
		t.Fatal("unexpected parse of a message without a limit")
	}
	if !IsRequestTooLarge(http.StatusBadRequest, body) || !IsRequestTooLarge(http.StatusRequestEntityTooLarge, nil) {
		t.Fatal("IsRequestTooLarge should match both shapes")
	}
	// A cumulative-window rate limit is not "request too large": it should still cool the bucket down.
	if IsRequestTooLarge(http.StatusBadRequest, []byte(`{"error":{"message":"Rate limit reached for model on tokens per minute (TPM): Limit 6000, Used 5200, Requested 1500"}}`)) {
		t.Fatal("window exhaustion must not be classified as request-too-large")
	}
}
