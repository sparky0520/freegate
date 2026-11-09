package server

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"gateway/internal/config"
	"gateway/internal/registry"
	"gateway/internal/router"
)

// Server handles gateway HTTP endpoints.
type Server struct {
	Config   *config.Config
	Registry *registry.Registry
	Router   *router.Router
	Client   *http.Client
	Logger   *slog.Logger
}

// NewServer creates a new HTTP server handler for the gateway.
func NewServer(cfg *config.Config, reg *registry.Registry, client *http.Client, logger *slog.Logger) *Server {
	if client == nil {
		client = &http.Client{
			Transport: &http.Transport{
				DialContext: (&net.Dialer{
					Timeout: 10 * time.Second,
				}).DialContext,
				ResponseHeaderTimeout: 60 * time.Second,
			},
		}
	}
	if logger == nil {
		logger = slog.Default()
	}

	rtr := router.New(cfg, reg, client, logger)

	return &Server{
		Config:   cfg,
		Registry: reg,
		Router:   rtr,
		Client:   client,
		Logger:   logger,
	}
}

// Handler returns the http.Handler for all routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/status", s.handleStatus)
	return mux
}

func isAllowedHost(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func writeOpenAIError(w http.ResponseWriter, statusCode int, message, errType, errCode string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    errType,
			"code":    errCode,
		},
	})
}

func (s *Server) checkAuth(r *http.Request) bool {
	if s.Config.LocalAPIKey == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	return auth == "Bearer "+s.Config.LocalAPIKey
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if !isAllowedHost(r.Host) {
		writeOpenAIError(w, http.StatusForbidden, "Forbidden: invalid host", "invalid_request_error", "forbidden")
		return
	}

	now := time.Now()
	snapshots := s.Registry.Snapshot(now)
	aliases := s.Registry.Aliases()

	resp := map[string]any{
		"buckets": snapshots,
		"aliases": aliases,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if !isAllowedHost(r.Host) {
		writeOpenAIError(w, http.StatusForbidden, "Forbidden: invalid host", "invalid_request_error", "forbidden")
		return
	}
	if !s.checkAuth(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "Invalid local API key", "invalid_request_error", "invalid_api_key")
		return
	}

	now := time.Now()
	created := now.Unix()

	var models []openAIModel

	// 1. Add alias names
	for alias := range s.Registry.Aliases() {
		models = append(models, openAIModel{
			ID:      alias,
			Object:  "model",
			Created: created,
			OwnedBy: "freegate",
		})
	}

	// 2. Add currently usable concrete models
	for _, b := range s.Registry.AllBuckets() {
		snap := b.Snapshot(now)
		if snap.HasKey && !snap.Disabled {
			models = append(models, openAIModel{
				ID:      snap.Key,
				Object:  "model",
				Created: created,
				OwnedBy: snap.Provider,
			})
		}
	}

	resp := map[string]any{
		"object": "list",
		"data":   models,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if !isAllowedHost(r.Host) {
		writeOpenAIError(w, http.StatusForbidden, "Forbidden: invalid host header", "invalid_request_error", "forbidden")
		return
	}
	if !s.checkAuth(r) {
		writeOpenAIError(w, http.StatusUnauthorized, "Invalid local API key", "invalid_request_error", "invalid_api_key")
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed: expected POST", "invalid_request_error", "method_not_allowed")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 20*1024*1024)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Failed to read request body: "+err.Error(), "invalid_request_error", "bad_request")
		return
	}

	parsed, err := router.ParseRequest(r, bodyBytes)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Invalid JSON in request body", "invalid_request_error", "bad_request")
		return
	}

	s.Router.Route(r.Context(), parsed, w)
}
