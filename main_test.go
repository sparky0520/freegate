package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHostValidation(t *testing.T) {
	tests := []struct {
		host    string
		allowed bool
	}{
		{"localhost", true},
		{"localhost:8080", true},
		{"127.0.0.1", true},
		{"127.0.0.1:8080", true},
		{"::1", true},
		{"[::1]", true},
		{"[::1]:8080", true},
		{"evil.com", false},
		{"evil.com:8080", false},
		{"example.org", false},
		{"192.168.1.1", false},
	}

	for _, tc := range tests {
		got := isAllowedHost(tc.host)
		if got != tc.allowed {
			t.Errorf("isAllowedHost(%q) = %v; want %v", tc.host, got, tc.allowed)
		}
	}
}

func TestHandleChatPassthroughSuccess(t *testing.T) {
	expectedAPIKey := "test-groq-key-12345"
	expectedResponseBody := `{"id":"chatcmpl-123","choices":[{"message":{"role":"assistant","content":"Hello world!"}}]}`
	requestPayload := `{"model":"llama-3.3-70b-versatile","messages":[{"role":"user","content":"hi"}]}`

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", r.Header.Get("Content-Type"))
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+expectedAPIKey {
			t.Errorf("expected Authorization 'Bearer %s', got '%s'", expectedAPIKey, auth)
		}

		body, _ := io.ReadAll(r.Body)
		if string(body) != requestPayload {
			t.Errorf("expected body %s, got %s", requestPayload, string(body))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Custom-Header", "test-val")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(expectedResponseBody))
	}))
	defer fakeUpstream.Close()

	gw := NewGateway(fakeUpstream.URL, expectedAPIKey, fakeUpstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestPayload))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-original-key") // should be rewritten

	rec := httptest.NewRecorder()
	gw.handleChat(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", resp.Header.Get("Content-Type"))
	}
	if resp.Header.Get("X-Custom-Header") != "test-val" {
		t.Errorf("expected X-Custom-Header test-val, got %s", resp.Header.Get("X-Custom-Header"))
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != expectedResponseBody {
		t.Errorf("expected body %s, got %s", expectedResponseBody, string(body))
	}
}

func TestHandleChatUpstream401(t *testing.T) {
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"Missing API key","type":"auth_error"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fakeUpstream.Close()

	// Gateway with no API key
	gw := NewGateway(fakeUpstream.URL, "", fakeUpstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Host = "localhost:8080"
	rec := httptest.NewRecorder()

	gw.handleChat(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected status 401, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var errResp map[string]map[string]any
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("failed to parse JSON error: %v", err)
	}
	if errResp["error"]["message"] != "Missing API key" {
		t.Errorf("unexpected error message: %v", errResp["error"]["message"])
	}
}

func TestHandleChatInvalidHost(t *testing.T) {
	gw := NewGateway("http://dummy", "key", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	req.Host = "rebind-attack.com"
	rec := httptest.NewRecorder()

	gw.handleChat(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected status 403 for invalid host, got %d", resp.StatusCode)
	}
}

func TestHandleChatMethodNotAllowed(t *testing.T) {
	gw := NewGateway("http://dummy", "key", nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()

	gw.handleChat(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected status 405 for GET, got %d", resp.StatusCode)
	}
}

func TestHandleChatClientCancel(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(3 * time.Second):
			t.Error("timeout waiting for upstream cancellation")
		}
	}))
	defer fakeUpstream.Close()

	gw := NewGateway(fakeUpstream.URL, "key", fakeUpstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)).WithContext(ctx)
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		gw.handleChat(rec, req)
		close(done)
	}()

	// Wait briefly then cancel client request
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-upstreamCanceled:
		// Upstream request detected cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not detect context cancellation")
	}

	<-done
}

func TestHandleChatStreamingSuccess(t *testing.T) {
	chunks := []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\n",
		"data: [DONE]\n\n",
	}

	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("expected flusher")
		}
		flusher.Flush()

		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk))
			flusher.Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	defer fakeUpstream.Close()

	gw := NewGateway(fakeUpstream.URL, "test-key", fakeUpstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	reqBody := `{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()

	gw.handleChat(rec, req)

	resp := rec.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}

	body, _ := io.ReadAll(resp.Body)
	expected := strings.Join(chunks, "")
	if string(body) != expected {
		t.Errorf("expected body %q, got %q", expected, string(body))
	}
}

func TestHandleChatStreamingIncremental(t *testing.T) {
	chunk1 := "data: chunk1\n\n"
	chunk2 := "data: chunk2\n\n"

	canSendChunk2 := make(chan struct{})
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()

		_, _ = w.Write([]byte(chunk1))
		flusher.Flush()

		<-canSendChunk2
		_, _ = w.Write([]byte(chunk2))
		flusher.Flush()
	}))
	defer fakeUpstream.Close()

	// Use an in-memory pipe or test server to verify incremental streaming to client
	gw := NewGateway(fakeUpstream.URL, "key", fakeUpstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))

	gwServer := httptest.NewServer(http.HandlerFunc(gw.handleChat))
	defer gwServer.Close()

	clientReq, err := http.NewRequest(http.MethodPost, gwServer.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	clientReq.Host = "127.0.0.1"

	resp, err := gwServer.Client().Do(clientReq)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, len(chunk1))
	_, err = io.ReadFull(resp.Body, buf)
	if err != nil {
		t.Fatalf("failed to read first chunk: %v", err)
	}
	if string(buf) != chunk1 {
		t.Fatalf("expected %q, got %q", chunk1, string(buf))
	}

	// At this point, chunk 1 has arrived BEFORE chunk 2 is sent
	close(canSendChunk2)

	buf2 := make([]byte, len(chunk2))
	_, err = io.ReadFull(resp.Body, buf2)
	if err != nil {
		t.Fatalf("failed to read second chunk: %v", err)
	}
	if string(buf2) != chunk2 {
		t.Fatalf("expected %q, got %q", chunk2, string(buf2))
	}
}

func TestHandleChatStreamingClientCancel(t *testing.T) {
	upstreamCanceled := make(chan struct{})
	fakeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		flusher.Flush()

		_, _ = w.Write([]byte("data: chunk1\n\n"))
		flusher.Flush()

		select {
		case <-r.Context().Done():
			close(upstreamCanceled)
		case <-time.After(3 * time.Second):
			t.Error("timeout waiting for upstream cancellation in streaming")
		}
	}))
	defer fakeUpstream.Close()

	gw := NewGateway(fakeUpstream.URL, "key", fakeUpstream.Client(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	gwServer := httptest.NewServer(http.HandlerFunc(gw.handleChat))
	defer gwServer.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwServer.URL+"/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req.Host = "127.0.0.1"

	resp, err := gwServer.Client().Do(req)
	if err != nil {
		t.Fatalf("client request failed: %v", err)
	}
	defer resp.Body.Close()

	// Read first chunk
	buf := make([]byte, 14) // len("data: chunk1\n\n")
	_, err = io.ReadFull(resp.Body, buf)
	if err != nil {
		t.Fatalf("failed to read chunk: %v", err)
	}

	// Now cancel client context (simulates Ctrl-C)
	cancel()

	select {
	case <-upstreamCanceled:
		// Upstream correctly detected client cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not detect context cancellation on stream abort")
	}
}

