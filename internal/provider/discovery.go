package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"gateway/internal/config"
	"gateway/internal/registry"
)

// DiscoveredModel represents a model reported by a provider's /models endpoint.
type DiscoveredModel struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"context_window,omitempty"`
}

type modelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		ContextWindow int    `json:"context_window"`
		MaxModelLen   int    `json:"max_model_len"`
	} `json:"data"`
}

// DiscoverProvider queries GET {base_url}/models for a single provider.
func DiscoverProvider(ctx context.Context, p *config.ProviderConfig, client *http.Client, logger *slog.Logger) ([]DiscoveredModel, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("User-Agent", "freegate/0.1")
	if p.Name == "openrouter" || strings.Contains(p.BaseURL, "openrouter.ai") {
		req.Header.Set("HTTP-Referer", "http://localhost:8080")
		req.Header.Set("X-Title", "Freegate")
	}
	if !p.Local && p.APIKeyEnv != "" {
		key := os.Getenv(p.APIKeyEnv)
		if key == "" {
			return nil, fmt.Errorf("no API key set in %s", p.APIKeyEnv)
		}
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, string(body))
	}

	var mResp modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&mResp); err != nil {
		return nil, fmt.Errorf("decode json response: %w", err)
	}

	var models []DiscoveredModel
	for _, item := range mResp.Data {
		cw := item.ContextWindow
		if cw <= 0 && item.MaxModelLen > 0 {
			cw = item.MaxModelLen
		}
		models = append(models, DiscoveredModel{
			ID:            item.ID,
			ContextWindow: cw,
		})
	}

	return models, nil
}

// RunDiscovery runs discovery for all configured providers and updates the registry.
func RunDiscovery(ctx context.Context, cfg *config.Config, reg *registry.Registry, client *http.Client, logger *slog.Logger) {
	for i := range cfg.Providers {
		p := &cfg.Providers[i]

		// If remote provider has no API key configured, skip discovery quietly
		if !p.Local && p.APIKeyEnv != "" && os.Getenv(p.APIKeyEnv) == "" {
			logger.Debug("skipping discovery: no API key in environment",
				"provider", p.Name,
				"env", p.APIKeyEnv,
			)
			continue
		}

		models, err := DiscoverProvider(ctx, p, client, logger)
		if err != nil {
			// Discovery failure must never stop the gateway
			logger.Warn("discovery failed for provider",
				"provider", p.Name,
				"err", err,
			)
			for _, b := range reg.AllBuckets() {
				if b.Provider.Name == p.Name {
					b.RecordFailure(fmt.Sprintf("discovery failed: %v", err), time.Now())
				}
			}
			continue
		}

		logger.Info("discovered models for provider",
			"provider", p.Name,
			"count", len(models),
		)

		discoveredSet := make(map[string]DiscoveredModel)
		for _, m := range models {
			discoveredSet[m.ID] = m
			if strings.HasPrefix(m.ID, "models/") {
				discoveredSet[strings.TrimPrefix(m.ID, "models/")] = m
			}
		}

		// Check configured buckets for this provider
		for _, b := range reg.AllBuckets() {
			if b.Provider.Name != p.Name {
				continue
			}

			dm, found := discoveredSet[b.ModelID]
			if !found {
				logger.Warn("configured model not reported in provider discovery",
					"provider", p.Name,
					"model", b.ModelID,
				)
				// Drop/disable metadata entries whose model no longer exists
				b.Disable("model not found in provider discovery")
			} else {
				if dm.ContextWindow > 0 {
					b.UpdateContextWindow(dm.ContextWindow)
				}
			}
		}

		// Log new unclassified models at info level
		for id := range discoveredSet {
			key := p.Name + "/" + id
			if reg.GetBucket(key) == nil {
				logger.Info("discovered new unclassified model",
					"provider", p.Name,
					"model", id,
				)
			}
		}
	}
}

// StartDiscoveryWorker starts a background loop running discovery at startup and every 6 hours.
func StartDiscoveryWorker(ctx context.Context, cfg *config.Config, reg *registry.Registry, client *http.Client, logger *slog.Logger) {
	// Initial run
	RunDiscovery(ctx, cfg, reg, client, logger)

	// Recurring 6 hour schedule
	ticker := time.NewTicker(6 * time.Hour)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				RunDiscovery(ctx, cfg, reg, client, logger)
			}
		}
	}()
}
