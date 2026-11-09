package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"gateway/internal/config"
	"gateway/internal/provider"
	"gateway/internal/registry"
)

type statusResponse struct {
	Buckets []registry.BucketSnapshot `json:"buckets"`
	Aliases map[string][]string       `json:"aliases"`
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// StatusCommand fetches and prints the status table from a running gateway.
func StatusCommand(addr string, out io.Writer) error {
	url := fmt.Sprintf("http://%s/status", addr)
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Host = "127.0.0.1"

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not connect to gateway at %s: %w", addr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gateway returned status %d: %s", resp.StatusCode, string(body))
	}

	var status statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return fmt.Errorf("failed to decode /status JSON: %w", err)
	}

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "BUCKET\tTIER\tSTATE\tREMAINING (REQ/TOK)\tCOOLDOWN\tLAST ERROR")

	for _, b := range status.Buckets {
		remReq := "-"
		if b.RemainingRequests >= 0 {
			remReq = fmt.Sprintf("%d", b.RemainingRequests)
		}
		remTok := "-"
		if b.RemainingTokens >= 0 {
			remTok = fmt.Sprintf("%d", b.RemainingTokens)
		}
		remaining := fmt.Sprintf("%s/%s", remReq, remTok)

		cooldown := "-"
		if b.CooldownRemainingS > 0 {
			cooldown = fmt.Sprintf("%ds", b.CooldownRemainingS)
		}

		lastErr := "-"
		if b.Disabled && b.DisabledReason != "" {
			lastErr = b.DisabledReason
		} else if b.LastError != "" {
			lastErr = b.LastError
		}

		state := b.State
		if !b.HasKey && !b.Local {
			state = "no_key"
		}

		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\n",
			b.Key,
			b.Tier,
			state,
			remaining,
			cooldown,
			lastErr,
		)
	}
	_ = w.Flush()
	return nil
}

// ModelsCommand fetches and lists all aliases and models.
func ModelsCommand(addr string, out io.Writer) error {
	url := fmt.Sprintf("http://%s/v1/models", addr)
	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Host = "127.0.0.1"

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("could not connect to gateway at %s: %w", addr, err)
	}
	defer resp.Body.Close()

	var mResp modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		return fmt.Errorf("failed to decode /v1/models JSON: %w", err)
	}

	var aliases, concrete []string
	for _, item := range mResp.Data {
		if item.OwnedBy == "freegate" {
			aliases = append(aliases, item.ID)
		} else {
			concrete = append(concrete, item.ID)
		}
	}

	fmt.Fprintln(out, "Aliases:")
	for _, a := range aliases {
		fmt.Fprintf(out, "  - %s\n", a)
	}

	fmt.Fprintln(out, "\nUsable Concrete Models:")
	for _, m := range concrete {
		fmt.Fprintf(out, "  - %s\n", m)
	}

	return nil
}

const defaultExampleConfig = `# Freegate Configuration
listen = "127.0.0.1:8080"
private_mode = false
unknown_model = "auto"
max_wait_seconds = 10
# local_api_key = "" # uncomment to require Bearer key for clients

[[provider]]
name = "groq"
base_url = "https://api.groq.com/openai/v1"
api_key_env = "GROQ_API_KEY"
trains_on_data = false

[[provider]]
name = "gemini"
base_url = "https://generativelanguage.googleapis.com/v1beta/openai"
api_key_env = "GEMINI_API_KEY"
trains_on_data = true

[[provider]]
name = "openrouter"
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
trains_on_data = true

[[provider]]
name = "ollama"
base_url = "http://127.0.0.1:11434/v1"
local = true
trains_on_data = false
`

const defaultExampleModels = `# Freegate Model Metadata
# Each model must have an entry here to be routable by the gateway.

[[model]]
provider = "groq"
id = "openai/gpt-oss-20b"
tier = 2
supports_tools = true
supports_vision = false
context_window = 131072

[[model]]
provider = "groq"
id = "openai/gpt-oss-120b"
tier = 3
supports_tools = true
supports_vision = false
context_window = 131072

[[model]]
provider = "gemini"
id = "gemini-flash-latest"
tier = 2
supports_tools = true
supports_vision = true
context_window = 1048576

[[model]]
provider = "gemini"
id = "gemini-3.8-flash"
tier = 2
supports_tools = true
supports_vision = true
context_window = 1048576

[[model]]
provider = "gemini"
id = "gemini-2.5-pro"
tier = 3
supports_tools = true
supports_vision = true
context_window = 2097152

[[model]]
provider = "openrouter"
id = "liquid/lfm-3-1.7b:free"
tier = 1
supports_tools = true
supports_vision = false
context_window = 262144

[[model]]
provider = "openrouter"
id = "nvidia/nemotron-3.5-lightning:free"
tier = 2
supports_tools = true
supports_vision = false
context_window = 131072

[[model]]
provider = "openrouter"
id = "cohere/north-mini-code:free"
tier = 3
supports_tools = true
supports_vision = false
context_window = 131072

[[model]]
provider = "ollama"
id = "llama3:latest"
tier = 1
supports_tools = true
supports_vision = false
context_window = 8192
`

// InitCommand creates the default configuration and models files in ~/.config/freegate.
func InitCommand(out io.Writer) error {
	confDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("resolve user config directory: %w", err)
	}

	targetDir := filepath.Join(confDir, "freegate")
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return fmt.Errorf("create directory %s: %w", targetDir, err)
	}

	confFile := filepath.Join(targetDir, "config.toml")
	if _, err := os.Stat(confFile); os.IsNotExist(err) {
		if err := os.WriteFile(confFile, []byte(defaultExampleConfig), 0644); err != nil {
			return fmt.Errorf("write config file: %w", err)
		}
		fmt.Fprintf(out, "Created: %s\n", confFile)
	} else {
		fmt.Fprintf(out, "Exists:  %s (skipped)\n", confFile)
	}

	modelsFile := filepath.Join(targetDir, "models.toml")
	if _, err := os.Stat(modelsFile); os.IsNotExist(err) {
		if err := os.WriteFile(modelsFile, []byte(defaultExampleModels), 0644); err != nil {
			return fmt.Errorf("write models file: %w", err)
		}
		fmt.Fprintf(out, "Created: %s\n", modelsFile)
	} else {
		fmt.Fprintf(out, "Exists:  %s (skipped)\n", modelsFile)
	}

	fmt.Fprintln(out, "\nInitialization complete! Next steps:")
	fmt.Fprintln(out, "  1. Set your provider API keys (e.g. export GROQ_API_KEY=\"...\")")
	fmt.Fprintln(out, "  2. Test your keys with: freegate test")
	fmt.Fprintln(out, "  3. Start the gateway:    freegate start")
	return nil
}

func parseErrorMessage(body []byte) string {
	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return errResp.Error.Message
	}

	var errList []struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errList); err == nil && len(errList) > 0 && errList[0].Error.Message != "" {
		return errList[0].Error.Message
	}

	msg := strings.TrimSpace(string(body))
	if len(msg) > 120 {
		msg = msg[:117] + "..."
	}
	return msg
}

// TestCommand sends a tiny request to each configured provider to verify keys.
func TestCommand(confPath, modelsPath string, out io.Writer) error {
	config.LoadDotEnv(".env")

	cfg, err := config.LoadConfig(confPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	metadata, err := config.LoadModelMetadata(modelsPath)
	if err != nil {
		return fmt.Errorf("load models: %w", err)
	}

	// Index metadata models by provider
	providerMetadata := make(map[string][]config.ModelMetadata)
	for _, m := range metadata {
		providerMetadata[m.Provider] = append(providerMetadata[m.Provider], m)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	fmt.Fprintln(out, "Testing configured providers:")

	var readyCount, totalProviders int

	for _, p := range cfg.Providers {
		totalProviders++

		// Check API key presence
		if !p.Local && p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
			fmt.Fprintf(out, "  [SKIP] %-12s: $%s not set in environment\n", p.Name, p.APIKeyEnv)
			continue
		}

		// Perform discovery to find real models currently offered by provider
		ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
		discovered, discErr := provider.DiscoverProvider(ctx, &p, client, nil)
		cancel()

		if discErr != nil {
			// Check if local daemon connection refused
			if p.Local {
				var opErr *net.OpError
				if strings.Contains(discErr.Error(), "connection refused") || errors.As(discErr, &opErr) {
					fmt.Fprintf(out, "  [SKIP] %-12s (local): daemon not running at %s (optional)\n", p.Name, p.BaseURL)
					continue
				}
			}
			if strings.Contains(discErr.Error(), "401") || strings.Contains(discErr.Error(), "403") {
				fmt.Fprintf(out, "  [FAIL] %-12s: authentication error (check $%s)\n", p.Name, p.APIKeyEnv)
				continue
			}
		}

		// Build candidate model list for this provider
		var candidateModels []string
		metaModels := providerMetadata[p.Name]

		// Priority 1: models in metadata that exist in discovery (or all metadata if discovery failed)
		discoveredMap := make(map[string]bool)
		for _, dm := range discovered {
			discoveredMap[dm.ID] = true
		}

		for _, mm := range metaModels {
			if len(discovered) == 0 || discoveredMap[mm.ID] || discoveredMap["models/"+mm.ID] {
				candidateModels = append(candidateModels, mm.ID)
			}
		}

		// Priority 2: if metadata models were not in discovery, select from discovered models
		if len(candidateModels) == 0 && len(discovered) > 0 {
			// Prefer :free models for openrouter
			for _, dm := range discovered {
				if strings.HasSuffix(dm.ID, ":free") {
					candidateModels = append(candidateModels, dm.ID)
				}
			}
			if len(candidateModels) == 0 {
				candidateModels = append(candidateModels, discovered[0].ID)
			}
		}

		// Fallback: any metadata model
		if len(candidateModels) == 0 && len(metaModels) > 0 {
			candidateModels = append(candidateModels, metaModels[0].ID)
		}

		if len(candidateModels) == 0 {
			fmt.Fprintf(out, "  [SKIP] %-12s: no models configured or discovered\n", p.Name)
			continue
		}

		// Attempt test requests across candidate models until one succeeds
		var providerOK bool
		var lastErrText string

		for _, modelID := range candidateModels {
			start := time.Now()
			url := strings.TrimRight(p.BaseURL, "/") + "/chat/completions"
			body := map[string]any{
				"model":      modelID,
				"max_tokens": 5,
				"messages": []map[string]string{
					{"role": "user", "content": "ping"},
				},
			}
			bodyBytes, _ := json.Marshal(body)

			reqCtx, reqCancel := context.WithTimeout(context.Background(), 8*time.Second)
			req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, bytes.NewReader(bodyBytes))
			if err != nil {
				reqCancel()
				lastErrText = err.Error()
				continue
			}

			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "freegate/0.1")
			if p.Name == "openrouter" || strings.Contains(p.BaseURL, "openrouter.ai") {
				req.Header.Set("HTTP-Referer", "http://localhost:8080")
				req.Header.Set("X-Title", "Freegate")
			}
			if !p.Local && p.APIKeyEnv != "" {
				req.Header.Set("Authorization", "Bearer "+os.Getenv(p.APIKeyEnv))
			}

			resp, err := client.Do(req)
			elapsed := time.Since(start).Round(time.Millisecond)
			if err != nil {
				reqCancel()
				lastErrText = fmt.Sprintf("timeout / connection error: %v (%v)", err, elapsed)
				continue
			}

			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			resp.Body.Close()
			reqCancel()

			if resp.StatusCode == http.StatusOK {
				fmt.Fprintf(out, "  [OK]   %-12s (%s): 200 OK (%v)\n", p.Name, modelID, elapsed)
				providerOK = true
				readyCount++
				break
			}

			errMsg := parseErrorMessage(respBody)
			lastErrText = fmt.Sprintf("HTTP %d - %s (%v)", resp.StatusCode, errMsg, elapsed)
			// If model not found (404), temporary spike (503), or model-specific rate limit (429), try next candidate model
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusServiceUnavailable || resp.StatusCode == http.StatusTooManyRequests {
				continue
			}
			// Other errors (auth 401/403, bad request, etc.) stop further attempts for this provider
			break
		}

		if !providerOK {
			testedModel := candidateModels[0]
			fmt.Fprintf(out, "  [FAIL] %-12s (%s): %s\n", p.Name, testedModel, lastErrText)
		}
	}

	fmt.Fprintf(out, "\nResult: %d/%d configured providers ready.\n", readyCount, totalProviders)
	return nil
}
