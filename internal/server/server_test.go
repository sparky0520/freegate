package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gateway/internal/config"
	"gateway/internal/registry"
)

func TestHealthz(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), registry.New(config.DefaultConfig(), nil), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("expected 'ok', got %q", rec.Body.String())
	}
}

func TestStatusEndpoint(t *testing.T) {
	cfg := config.DefaultConfig()
	metadata := []config.ModelMetadata{
		{Provider: "groq", ID: "openai/gpt-oss-20b", Tier: 2},
	}
	reg := registry.New(cfg, metadata)
	srv := NewServer(cfg, reg, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var status map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatalf("failed to decode status: %v", err)
	}

	buckets, ok := status["buckets"].([]any)
	if !ok || len(buckets) != 1 {
		t.Fatalf("expected 1 bucket in status, got %v", status["buckets"])
	}
}

func TestModelsEndpoint(t *testing.T) {
	t.Setenv("TEST_KEY", "abc")
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", APIKeyEnv: "TEST_KEY", BaseURL: "http://dummy"},
		},
	}
	metadata := []config.ModelMetadata{
		{Provider: "p1", ID: "test-model", Tier: 1},
	}
	reg := registry.New(cfg, metadata)
	srv := NewServer(cfg, reg, nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var mResp map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&mResp); err != nil {
		t.Fatalf("failed to decode models: %v", err)
	}

	data, ok := mResp["data"].([]any)
	if !ok {
		t.Fatalf("expected data array in models response: %v", mResp)
	}

	// Should have aliases (auto, fast, etc.) + p1/test-model
	foundModel := false
	for _, item := range data {
		mMap := item.(map[string]any)
		if mMap["id"] == "p1/test-model" {
			foundModel = true
		}
	}
	if !foundModel {
		t.Fatalf("expected to find p1/test-model in data, got %v", data)
	}
}

func TestLocalAPIKeyAuth(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LocalAPIKey = "secret123"
	srv := NewServer(cfg, registry.New(cfg, nil), nil, nil)

	// Missing auth header
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when local_api_key set and no header, got %d", rec.Code)
	}

	// Correct auth header
	reqAuth := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	reqAuth.Host = "127.0.0.1:8080"
	reqAuth.Header.Set("Authorization", "Bearer secret123")
	recAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recAuth, reqAuth)
	if recAuth.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct local_api_key, got %d", recAuth.Code)
	}
}

func TestChatCompletionsPassthroughWithRegistry(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if m["model"] != "target-model-id" {
			t.Errorf("expected model rewritten to target-model-id, got %v", m["model"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"Hello from upstream"}}]}`))
	}))
	defer fakeServerClose(fakeUpstream)

	t.Setenv("UPSTREAM_KEY", "key-xyz")
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "up", BaseURL: fakeUpstream.URL, APIKeyEnv: "UPSTREAM_KEY"},
		},
	}
	metadata := []config.ModelMetadata{
		{Provider: "up", ID: "target-model-id", Tier: 2},
	}
	reg := registry.New(cfg, metadata)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, reg, fakeUpstream.Client(), logger)

	// Send request with virtual alias "auto"
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if !strings.Contains(rec.Body.String(), "Hello from upstream") {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
}

func fakeServerClose(s *httptest.Server) {
	s.Close()
}

func TestHostCheck(t *testing.T) {
	srv := NewServer(config.DefaultConfig(), registry.New(config.DefaultConfig(), nil), nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Host = "attacker.com"
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for attacker host, got %d", rec.Code)
	}
}
