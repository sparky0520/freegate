package config

import (
	"bufio"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

//go:embed models.default.toml
var defaultModelsData []byte

// Config holds the gateway configuration.
type Config struct {
	Listen         string                 `toml:"listen"`
	PrivateMode    bool                   `toml:"private_mode"`
	UnknownModel   string                 `toml:"unknown_model"`
	MaxWaitSeconds int                    `toml:"max_wait_seconds"`
	LocalAPIKey    string                 `toml:"local_api_key"`
	Providers      []ProviderConfig       `toml:"provider"`
	Aliases        map[string]AliasConfig `toml:"alias"`
}

// ProviderConfig describes an upstream LLM provider.
type ProviderConfig struct {
	Name         string `toml:"name"`
	BaseURL      string `toml:"base_url"`
	APIKeyEnv    string `toml:"api_key_env"`
	TrainsOnData bool   `toml:"trains_on_data"`
	Local        bool   `toml:"local"`
}

// AliasConfig describes a named model alias.
type AliasConfig struct {
	Models []string `toml:"models"`
}

// ModelMetadata describes a single model supported by the gateway.
type ModelMetadata struct {
	Provider       string `toml:"provider"`
	ID             string `toml:"id"`
	Tier           int    `toml:"tier"`
	SupportsTools  bool   `toml:"supports_tools"`
	SupportsVision bool   `toml:"supports_vision"`
	ContextWindow  int    `toml:"context_window"`
}

type modelsFile struct {
	Models []ModelMetadata `toml:"model"`
}

// DefaultConfig returns the default configuration.
func DefaultConfig() *Config {
	return &Config{
		Listen:         "127.0.0.1:8080",
		PrivateMode:    false,
		UnknownModel:   "auto",
		MaxWaitSeconds: 10,
		Providers: []ProviderConfig{
			{
				Name:         "groq",
				BaseURL:      "https://api.groq.com/openai/v1",
				APIKeyEnv:    "GROQ_API_KEY",
				TrainsOnData: false,
			},
			{
				Name:         "gemini",
				BaseURL:      "https://generativelanguage.googleapis.com/v1beta/openai",
				APIKeyEnv:    "GEMINI_API_KEY",
				TrainsOnData: true,
			},
			{
				Name:         "openrouter",
				BaseURL:      "https://openrouter.ai/api/v1",
				APIKeyEnv:    "OPENROUTER_API_KEY",
				TrainsOnData: true,
			},
			{
				Name:         "ollama",
				BaseURL:      "http://127.0.0.1:11434/v1",
				Local:        true,
				TrainsOnData: false,
			},
		},
		Aliases: map[string]AliasConfig{
			"auto":      {Models: []string{}},
			"fast":      {Models: []string{}},
			"best-code": {Models: []string{}},
			"local":     {Models: []string{}},
		},
	}
}

// DefaultConfigPath returns ~/.config/freegate/config.toml.
func DefaultConfigPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "freegate", "config.toml"), nil
}

// DefaultModelsPath returns ~/.config/freegate/models.toml.
func DefaultModelsPath() (string, error) {
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(configDir, "freegate", "models.toml"), nil
}

// LoadConfig loads configuration from path or default location.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		defaultPath, err := DefaultConfigPath()
		if err == nil {
			path = defaultPath
		}
	}

	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("read config file: %w", err)
	}

	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("parse config TOML: %w", err)
	}

	// Ensure defaults if empty
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8080"
	}
	if cfg.UnknownModel == "" {
		cfg.UnknownModel = "auto"
	}
	if cfg.MaxWaitSeconds <= 0 {
		cfg.MaxWaitSeconds = 10
	}
	if cfg.Aliases == nil {
		cfg.Aliases = make(map[string]AliasConfig)
	}

	return cfg, nil
}

// LoadModelMetadata loads model metadata from user file if present, or fallback to embedded models.default.toml.
func LoadModelMetadata(path string) ([]ModelMetadata, error) {
	var data []byte
	var err error

	if path == "" {
		defaultPath, defErr := DefaultModelsPath()
		if defErr == nil && defaultPath != "" {
			if _, statErr := os.Stat(defaultPath); statErr == nil {
				path = defaultPath
			}
		}
	}

	if path != "" {
		data, err = os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("read models file: %w", err)
		}
	}

	if len(data) == 0 {
		data = defaultModelsData
	}

	var mf modelsFile
	if err := toml.Unmarshal(data, &mf); err != nil {
		return nil, fmt.Errorf("parse models TOML: %w", err)
	}

	return mf.Models, nil
}

// LoadDotEnv reads key=value pairs from .env files and sets them if not already present.
// It checks the specified path, ~/.config/freegate/.env, and the user config dir.
func LoadDotEnv(customPath string) {
	paths := []string{customPath}
	if homeDir, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(homeDir, ".config", "freegate", ".env"))
		paths = append(paths, filepath.Join(homeDir, ".env"))
	}
	if confDir, err := os.UserConfigDir(); err == nil {
		paths = append(paths, filepath.Join(confDir, "freegate", ".env"))
	}

	for _, p := range paths {
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}

		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			parts := strings.SplitN(line, "=", 2)
			if len(parts) != 2 {
				continue
			}
			key := strings.TrimSpace(parts[0])
			val := strings.TrimSpace(parts[1])
			val = strings.Trim(val, `"'`)
			if os.Getenv(key) == "" {
				_ = os.Setenv(key, val)
			}
		}
		f.Close()
	}
}
