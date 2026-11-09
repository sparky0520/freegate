package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStatusCommand(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("expected /status, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"buckets": [
				{
					"key": "groq/gpt-oss-20b",
					"provider": "groq",
					"model": "gpt-oss-20b",
					"tier": 2,
					"state": "ok",
					"has_key": true,
					"remaining_requests": 100,
					"remaining_tokens": 50000,
					"cooldown_remaining_seconds": 0
				},
				{
					"key": "gemini/gemini-2.0-flash",
					"provider": "gemini",
					"model": "gemini-2.0-flash",
					"tier": 2,
					"state": "disabled",
					"disabled": true,
					"disabled_reason": "provider unreachable",
					"has_key": true,
					"remaining_requests": -1,
					"remaining_tokens": -1,
					"cooldown_remaining_seconds": 0
				}
			]
		}`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	var out bytes.Buffer

	err := StatusCommand(addr, &out)
	if err != nil {
		t.Fatalf("StatusCommand failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "groq/gpt-oss-20b") {
		t.Errorf("expected output to contain groq/gpt-oss-20b, got:\n%s", output)
	}
	if !strings.Contains(output, "gemini/gemini-2.0-flash") {
		t.Errorf("expected output to contain gemini/gemini-2.0-flash, got:\n%s", output)
	}
	if !strings.Contains(output, "provider unreachable") {
		t.Errorf("expected output to contain disabled reason, got:\n%s", output)
	}
}

func TestModelsCommand(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("expected /v1/models, got %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{"id": "auto", "owned_by": "freegate"},
				{"id": "fast", "owned_by": "freegate"},
				{"id": "groq/gpt-oss-20b", "owned_by": "groq"}
			]
		}`))
	}))
	defer ts.Close()

	addr := strings.TrimPrefix(ts.URL, "http://")
	var out bytes.Buffer

	err := ModelsCommand(addr, &out)
	if err != nil {
		t.Fatalf("ModelsCommand failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "auto") || !strings.Contains(output, "groq/gpt-oss-20b") {
		t.Errorf("unexpected output:\n%s", output)
	}
}

func TestInitCommand(t *testing.T) {
	// Temporarily redirect UserConfigDir via HOME or temp dir
	tmpDir := t.TempDir()
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", tmpDir)

	var out bytes.Buffer
	err := InitCommand(&out)
	if err != nil {
		t.Fatalf("InitCommand failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "config.toml") || !strings.Contains(output, "models.toml") {
		t.Errorf("unexpected output from InitCommand: %s", output)
	}

	// Running init again should report skipped/exists
	var out2 bytes.Buffer
	err = InitCommand(&out2)
	if err != nil {
		t.Fatalf("second InitCommand failed: %v", err)
	}
	if !strings.Contains(out2.String(), "skipped") {
		t.Errorf("expected files to be skipped on second init, got: %s", out2.String())
	}
}

func TestTestCommand(t *testing.T) {
	fakeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}))
	defer fakeServer.Close()

	t.Setenv("MOCK_KEY", "dummy-key")

	tmpDir := t.TempDir()
	confPath := filepath.Join(tmpDir, "config.toml")
	modelsPath := filepath.Join(tmpDir, "models.toml")

	confContent := fmt.Sprintf(`
[[provider]]
name = "mock"
base_url = "%s"
api_key_env = "MOCK_KEY"
`, fakeServer.URL)

	modelsContent := `
[[model]]
provider = "mock"
id = "mock-model"
tier = 2
`

	_ = os.WriteFile(confPath, []byte(confContent), 0644)
	_ = os.WriteFile(modelsPath, []byte(modelsContent), 0644)

	var out bytes.Buffer
	err := TestCommand(confPath, modelsPath, &out)
	if err != nil {
		t.Fatalf("TestCommand failed: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "[OK]") || !strings.Contains(output, "mock") {
		t.Errorf("expected OK output for mock provider, got: %s", output)
	}
}
