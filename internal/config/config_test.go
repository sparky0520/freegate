package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Listen != "127.0.0.1:8080" {
		t.Errorf("expected listen 127.0.0.1:8080, got %s", cfg.Listen)
	}
	if len(cfg.Providers) != 5 {
		t.Errorf("expected 5 default providers, got %d", len(cfg.Providers))
	}
	foundDeepInfra := false
	for _, p := range cfg.Providers {
		if p.Name == "deepinfra" && p.APIKeyEnv == "DEEPINFRA_API_KEY" {
			foundDeepInfra = true
			if p.TrainsOnData {
				t.Error("expected deepinfra trains_on_data to be false")
			}
		}
	}
	if !foundDeepInfra {
		t.Error("expected deepinfra provider in DefaultConfig")
	}
}

func TestLoadModelMetadataEmbedded(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	models, err := LoadModelMetadata("")
	if err != nil {
		t.Fatalf("unexpected error loading metadata: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("expected embedded models to be non-empty")
	}

	foundGroq := false
	foundDeepInfra := false
	for _, m := range models {
		if m.Provider == "groq" && m.ID == "openai/gpt-oss-20b" {
			foundGroq = true
			if m.Tier != 2 {
				t.Errorf("expected tier 2, got %d", m.Tier)
			}
		}
		if m.Provider == "deepinfra" && m.ID == "deepseek-ai/DeepSeek-V3" {
			foundDeepInfra = true
			if m.Tier != 3 {
				t.Errorf("expected tier 3, got %d", m.Tier)
			}
		}
	}
	if !foundGroq {
		t.Error("expected to find groq/openai/gpt-oss-20b in embedded metadata")
	}
	if !foundDeepInfra {
		t.Error("expected to find deepinfra/deepseek-ai/DeepSeek-V3 in embedded metadata")
	}
}

func TestLoadConfigCustom(t *testing.T) {
	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "config.toml")
	tomlData := `
listen = "127.0.0.1:9090"
private_mode = true
unknown_model = "fast"
max_wait_seconds = 15

[[provider]]
name = "custom"
base_url = "https://example.com/v1"
api_key_env = "CUSTOM_KEY"
trains_on_data = false
local = false
`
	if err := os.WriteFile(confPath, []byte(tomlData), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(confPath)
	if err != nil {
		t.Fatalf("LoadConfig failed: %v", err)
	}
	if cfg.Listen != "127.0.0.1:9090" {
		t.Errorf("expected 127.0.0.1:9090, got %s", cfg.Listen)
	}
	if !cfg.PrivateMode {
		t.Error("expected private_mode true")
	}
	if cfg.UnknownModel != "fast" {
		t.Errorf("expected unknown_model fast, got %s", cfg.UnknownModel)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Name != "custom" {
		t.Errorf("unexpected providers: %+v", cfg.Providers)
	}
}
