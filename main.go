package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultUpstream = "https://api.groq.com/openai/v1/chat/completions"

// Hop-by-hop headers which should not be forwarded by a proxy.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// Gateway handles proxying requests to the upstream LLM provider.
type Gateway struct {
	UpstreamURL string
	APIKey      string
	Client      *http.Client
	Logger      *slog.Logger
}

// NewGateway creates a Gateway with default timeout settings.
func NewGateway(upstreamURL, apiKey string, client *http.Client, logger *slog.Logger) *Gateway {
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
	return &Gateway{
		UpstreamURL: upstreamURL,
		APIKey:      apiKey,
		Client:      client,
		Logger:      logger,
	}
}

// isAllowedHost validates that the Host header is localhost / 127.0.0.1 / [::1] (DNS rebinding protection).
func isAllowedHost(hostHeader string) bool {
	host := hostHeader
	if h, _, err := net.SplitHostPort(hostHeader); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// writeOpenAIError writes an error response conforming to OpenAI's error format.
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

func (gw *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	// 1. Validate Host header (DNS rebinding protection)
	if !isAllowedHost(r.Host) {
		writeOpenAIError(w, http.StatusForbidden, "Forbidden: invalid host header", "invalid_request_error", "forbidden")
		gw.Logger.Warn("rejected request with invalid host", "host", r.Host)
		return
	}

	// 2. Validate Method
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "Method not allowed: expected POST", "invalid_request_error", "method_not_allowed")
		return
	}

	// 3. Limit request body to 20MB and read it
	r.Body = http.MaxBytesReader(w, r.Body, 20*1024*1024)
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Failed to read request body: "+err.Error(), "invalid_request_error", "bad_request")
		return
	}

	// 4. Parse JSON as map[string]json.RawMessage to inspect only needed fields (stream) while preserving raw fields
	var reqMap map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &reqMap); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "Invalid JSON in request body", "invalid_request_error", "bad_request")
		return
	}

	var isStream bool
	if streamRaw, ok := reqMap["stream"]; ok {
		_ = json.Unmarshal(streamRaw, &isStream)
	}

	// 5. Build upstream request preserving exact original body
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, gw.UpstreamURL, bytes.NewReader(bodyBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "Failed to create upstream request", "internal_error", "internal_error")
		return
	}

	// Set headers
	upstreamReq.Header.Set("Content-Type", "application/json")
	if gw.APIKey != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+gw.APIKey)
	}

	// 6. Send request to upstream
	resp, err := gw.Client.Do(upstreamReq)
	if err != nil {
		if errors.Is(r.Context().Err(), context.Canceled) {
			gw.Logger.Info("client canceled request before upstream responded", "duration", time.Since(start))
			return
		}
		writeOpenAIError(w, http.StatusBadGateway, "Upstream connection failed: "+err.Error(), "upstream_error", "bad_gateway")
		gw.Logger.Error("upstream connection error", "err", err, "duration", time.Since(start))
		return
	}
	defer resp.Body.Close()

	// 7. Copy response headers (excluding hop-by-hop headers)
	for k, vv := range resp.Header {
		if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	// 8. Stream or pass-through
	// If upstream failed with error code (>= 400) or client did not request stream, handle non-streaming
	if resp.StatusCode >= 400 || !isStream {
		w.WriteHeader(resp.StatusCode)
		_, copyErr := io.Copy(w, resp.Body)
		if copyErr != nil && !errors.Is(copyErr, context.Canceled) {
			gw.Logger.Error("error copying response body", "err", copyErr)
		}
		gw.Logger.Info("handled chat completion request",
			"stream", false,
			"status", resp.StatusCode,
			"duration", time.Since(start),
		)
		return
	}

	// Streaming SSE path
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(resp.StatusCode)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}

	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				gw.Logger.Info("client disconnected during streaming", "duration", time.Since(start))
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if readErr != io.EOF && !errors.Is(readErr, context.Canceled) && !errors.Is(r.Context().Err(), context.Canceled) {
				gw.Logger.Error("upstream stream read error", "err", readErr)
				errJSON, _ := json.Marshal(map[string]any{
					"error": map[string]string{
						"message": "Upstream streaming error: " + readErr.Error(),
						"type":    "upstream_error",
						"code":    "stream_error",
					},
				})
				_, _ = w.Write([]byte("data: " + string(errJSON) + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			break
		}
	}

	gw.Logger.Info("handled streaming chat completion request",
		"stream", true,
		"status", resp.StatusCode,
		"duration", time.Since(start),
	)
}

// loadDotEnv reads key=value pairs from a .env file and sets them if not already present.
func loadDotEnv(filepath string) {
	f, err := os.Open(filepath)
	if err != nil {
		return
	}
	defer f.Close()

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
	if err := scanner.Err(); err != nil {
		slog.Warn("failed to read .env file", "path", filepath, "err", err)
	}
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	noAuth := flag.Bool("no-auth", false, "do not send upstream authorization header (for testing 401)")
	flag.Parse()

	if !*noAuth {
		loadDotEnv(".env")
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	var apiKey string
	if !*noAuth {
		apiKey = os.Getenv("GROQ_API_KEY")
	}
	gw := NewGateway(defaultUpstream, apiKey, nil, logger)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", gw.handleChat)

	logger.Info("gateway listening", "addr", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server error", "err", err)
		os.Exit(1)
	}
}
