package provider

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gateway/internal/config"
	"gateway/internal/registry"
)

func TestDiscoverProviderSuccess(t *testing.T) {
	fakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("expected /models, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-secret-key" {
			t.Errorf("expected bearer token, got %s", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "model-a", "context_window": 65536},
				{"id": "model-b", "max_model_len": 32768},
				{"id": "unclassified-model"}
			]
		}`))
	}))
	defer fakeServer.Close()

	t.Setenv("MOCK_KEY", "test-secret-key")
	p := &config.ProviderConfig{
		Name:      "mock",
		BaseURL:   fakeServer.URL,
		APIKeyEnv: "MOCK_KEY",
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	models, err := DiscoverProvider(context.Background(), p, fakeServer.Client(), logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(models) != 3 {
		t.Fatalf("expected 3 models, got %d", len(models))
	}
	if models[0].ID != "model-a" || models[0].ContextWindow != 65536 {
		t.Errorf("unexpected model 0: %+v", models[0])
	}
	if models[1].ID != "model-b" || models[1].ContextWindow != 32768 {
		t.Errorf("unexpected model 1: %+v", models[1])
	}
}

func TestRunDiscoveryDisablesMissingModel(t *testing.T) {
	fakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "model-present", "context_window": 128000}
			]
		}`))
	}))
	defer fakeServer.Close()

	t.Setenv("TEST_KEY", "dummy")
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "testp", BaseURL: fakeServer.URL, APIKeyEnv: "TEST_KEY"},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "testp", ID: "model-present", ContextWindow: 4096},
		{Provider: "testp", ID: "model-removed", ContextWindow: 4096},
	}

	reg := registry.New(cfg, meta)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	RunDiscovery(context.Background(), cfg, reg, fakeServer.Client(), logger)

	bPresent := reg.GetBucket("testp/model-present")
	if bPresent == nil || bPresent.Snapshot(tNow()).Disabled {
		t.Error("expected model-present to be active")
	}
	if bPresent.ContextWindow != 128000 {
		t.Errorf("expected context_window updated to 128000, got %d", bPresent.ContextWindow)
	}

	bRemoved := reg.GetBucket("testp/model-removed")
	if bRemoved == nil || !bRemoved.Snapshot(tNow()).Disabled {
		t.Error("expected model-removed to be disabled after discovery")
	}
}

func TestRunDiscoveryUnreachableNotFatal(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "dead-provider", BaseURL: "http://127.0.0.1:54321", Local: true},
		},
	}
	meta := []config.ModelMetadata{
		{Provider: "dead-provider", ID: "some-model"},
	}

	reg := registry.New(cfg, meta)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Should log warning but not panic or fail
	RunDiscovery(context.Background(), cfg, reg, &http.Client{}, logger)

	b := reg.GetBucket("dead-provider/some-model")
	if b == nil {
		t.Fatal("expected bucket to exist")
	}
}

func tNow() time.Time {
	return time.Now()
}
