package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"gateway/internal/config"
	"gateway/internal/registry"
)

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

// Router handles request filtering, ordering, attempt loops, and failover.
type Router struct {
	Config   *config.Config
	Registry *registry.Registry
	Sessions *SessionStore
	Client   *http.Client
	Logger   *slog.Logger
}

// New creates an initialized Router.
func New(cfg *config.Config, reg *registry.Registry, client *http.Client, logger *slog.Logger) *Router {
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

	return &Router{
		Config:   cfg,
		Registry: reg,
		Sessions: NewSessionStore(),
		Client:   client,
		Logger:   logger,
	}
}

// FilterEligible applies hard constraints to identify usable candidate buckets.
func (r *Router) FilterEligible(req *ParsedRequest, now time.Time) (eligible []*registry.Bucket, inCooldownOnly []*registry.Bucket) {
	// 1. Determine candidate bucket keys
	var candidates []*registry.Bucket

	requested := req.Model
	if requested == "" {
		requested = r.Config.UnknownModel
	}

	// Direct bucket match (e.g. "groq/openai/gpt-oss-20b")
	if b := r.Registry.GetBucket(requested); b != nil {
		candidates = append(candidates, b)
	} else {
		// ModelID without provider prefix
		for _, b := range r.Registry.AllBuckets() {
			if b.ModelID == requested {
				candidates = append(candidates, b)
			}
		}
	}

	// Alias lookup
	if len(candidates) == 0 {
		keys, ok := r.Registry.GetAlias(requested)
		if !ok {
			// Unknown model fallback to configured unknown_model ("auto")
			keys, _ = r.Registry.GetAlias(r.Config.UnknownModel)
		}
		for _, k := range keys {
			if b := r.Registry.GetBucket(k); b != nil {
				candidates = append(candidates, b)
			}
		}
	}

	// If still empty (e.g. "auto" with no predefined alias list), consider all buckets
	if len(candidates) == 0 {
		candidates = r.Registry.AllBuckets()
	}

	requiredContext := req.EstimatedTokens + req.MaxTokens

	for _, b := range candidates {
		snap := b.Snapshot(now)

		// Hard constraint 1: provider has key (or is local)
		if !snap.HasKey && !snap.Local {
			continue
		}

		// Hard constraint 2: private mode check
		if r.Config.PrivateMode && snap.TrainsOnData {
			continue
		}

		// Hard constraint 3: tools and vision support
		if req.HasTools && !snap.SupportsTools {
			continue
		}
		if req.HasImages && !snap.SupportsVision {
			continue
		}
		// Tool-using agent requests require capable models (Tier >= 2)
		if req.HasTools && snap.Tier < 2 {
			continue
		}

		// Hard constraint 4: context window
		if snap.ContextWindow > 0 && snap.ContextWindow < requiredContext {
			continue
		}

		// Hard constraint 5: not disabled and circuit breaker not open
		if snap.Disabled || snap.State == "breaker_open" {
			continue
		}

		// Hard constraint 6: cooldown
		if snap.State == "cooldown" {
			inCooldownOnly = append(inCooldownOnly, b)
			continue
		}

		eligible = append(eligible, b)
	}

	return eligible, inCooldownOnly
}

// OrderBuckets sorts eligible buckets according to session stickiness, complexity, tier, and remaining quota.
func (r *Router) OrderBuckets(req *ParsedRequest, buckets []*registry.Bucket, now time.Time) []*registry.Bucket {
	if len(buckets) <= 1 {
		return buckets
	}

	// Check session stickiness
	var stickyKey string
	if req.SessionID != "" {
		stickyKey, _ = r.Sessions.GetStickyBucket(req.SessionID, now)
	}

	var stickyBucket *registry.Bucket
	var others []*registry.Bucket

	for _, b := range buckets {
		if stickyKey != "" && b.Key() == stickyKey {
			stickyBucket = b
		} else {
			others = append(others, b)
		}
	}

	// Sort others
	isAuto := req.Model == "auto" || req.Model == "" || r.Registry.GetBucket(req.Model) == nil

	sort.SliceStable(others, func(i, j int) bool {
		bI := others[i]
		bJ := others[j]
		snapI := bI.Snapshot(now)
		snapJ := bJ.Snapshot(now)

		if isAuto {
			// Complex/code/tools: tier 3 down to 1; Simple: tier 1 up to 3
			if req.IsComplex {
				if snapI.Tier != snapJ.Tier {
					return snapI.Tier > snapJ.Tier
				}
			} else {
				if snapI.Tier != snapJ.Tier {
					return snapI.Tier < snapJ.Tier
				}
			}

			// Tie-break 1: remaining requests
			if snapI.RemainingRequests != snapJ.RemainingRequests {
				return snapI.RemainingRequests > snapJ.RemainingRequests
			}
			// Tie-break 2: remaining tokens
			if snapI.RemainingTokens != snapJ.RemainingTokens {
				return snapI.RemainingTokens > snapJ.RemainingTokens
			}
			// Tie-break 3: deterministic name
			return bI.Key() < bJ.Key()
		}

		// For other aliases, use alias's own order; tie-break only if remaining quota differs
		if snapI.RemainingRequests != snapJ.RemainingRequests && snapI.RemainingRequests >= 0 && snapJ.RemainingRequests >= 0 {
			return snapI.RemainingRequests > snapJ.RemainingRequests
		}
		if snapI.RemainingTokens != snapJ.RemainingTokens && snapI.RemainingTokens >= 0 && snapJ.RemainingTokens >= 0 {
			return snapI.RemainingTokens > snapJ.RemainingTokens
		}
		return false // keep stable original alias order
	})

	if stickyBucket != nil {
		return append([]*registry.Bucket{stickyBucket}, others...)
	}
	return others
}

// sanitizeMessagesRaw removes non-standard assistant fields (such as reasoning_content, thought, reasoning)
// that strict providers (like Groq) reject with HTTP 400.
func sanitizeMessagesRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var msgs []map[string]any
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return raw
	}
	modified := false
	for _, m := range msgs {
		role, _ := m["role"].(string)
		if role == "assistant" {
			for _, field := range []string{"reasoning_content", "thought", "reasoning"} {
				if _, ok := m[field]; ok {
					delete(m, field)
					modified = true
				}
			}
		}
	}
	if !modified {
		return raw
	}
	cleaned, err := json.Marshal(msgs)
	if err != nil {
		return raw
	}
	return cleaned
}

// Route handles the attempt loop and proxying of a chat completion request.
func (r *Router) Route(ctx context.Context, req *ParsedRequest, w http.ResponseWriter) {
	start := time.Now()
	now := start

	eligible, inCooldown := r.FilterEligible(req, now)

	// Wait vs switch logic:
	// If no bucket eligible only because of cooldowns and soonest cooldown is within max_wait, wait
	if len(eligible) == 0 && len(inCooldown) > 0 {
		minCooldown := inCooldown[0].Snapshot(now).CooldownUntil
		for _, b := range inCooldown[1:] {
			u := b.Snapshot(now).CooldownUntil
			if u.Before(minCooldown) {
				minCooldown = u
			}
		}

		maxWait := time.Duration(r.Config.MaxWaitSeconds) * time.Second
		waitDur := minCooldown.Sub(now)

		if waitDur > 0 && waitDur <= maxWait {
			r.Logger.Info("all eligible buckets in cooldown, waiting for soonest reset",
				"wait", waitDur,
			)

			select {
			case <-ctx.Done():
				r.Logger.Info("client canceled while waiting for cooldown", "duration", time.Since(start))
				return
			case <-time.After(waitDur):
				now = time.Now()
				eligible, inCooldown = r.FilterEligible(req, now)
			}
		}
	}

	if len(eligible) == 0 {
		if len(inCooldown) > 0 {
			minCooldown := inCooldown[0].Snapshot(now).CooldownUntil
			for _, b := range inCooldown[1:] {
				if b.Snapshot(now).CooldownUntil.Before(minCooldown) {
					minCooldown = b.Snapshot(now).CooldownUntil
				}
			}
			retrySecs := int(minCooldown.Sub(now).Seconds())
			if retrySecs <= 0 {
				retrySecs = 1
			}

			w.Header().Set("Retry-After", fmt.Sprintf("%d", retrySecs))
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Freegate-Attempts", "none:cooldown")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{
					"message": "All eligible models are currently in rate-limit cooldown.",
					"type":    "rate_limit_error",
					"code":    "rate_limit_exceeded",
				},
			})
			return
		}

		hasAnyKey := false
		for _, b := range r.Registry.AllBuckets() {
			snap := b.Snapshot(now)
			if snap.HasKey && !snap.Local {
				hasAnyKey = true
				break
			}
		}

		errMsg := "No eligible model found matching request constraints (tools, vision, context window, or private mode)."
		errCode := "no_eligible_model"
		if !hasAnyKey {
			errMsg = "No provider API keys configured. Please add your API keys (GROQ_API_KEY, OPENROUTER_API_KEY, GEMINI_API_KEY) to .env or ~/.config/freegate/.env and restart Freegate."
			errCode = "no_api_keys"
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Freegate-Attempts", "none:ineligible")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]string{
				"message": errMsg,
				"type":    "invalid_request_error",
				"code":    errCode,
			},
		})
		return
	}

	ordered := r.OrderBuckets(req, eligible, now)

	var attempts []string
	maxAttempts := 4
	if len(ordered) < maxAttempts {
		maxAttempts = len(ordered)
	}

	var lastErrResp *http.Response
	var lastErrBody []byte
	var lastErrStatusCode int

	for i := 0; i < maxAttempts; i++ {
		bucket := ordered[i]
		attemptTime := time.Now()

		// Prepare rewritten request body
		newModelRaw, _ := json.Marshal(bucket.ModelID)
		req.ReqMap["model"] = newModelRaw
		if msgsRaw, ok := req.ReqMap["messages"]; ok {
			req.ReqMap["messages"] = sanitizeMessagesRaw(msgsRaw)
		}
		rewrittenBytes, err := json.Marshal(req.ReqMap)
		if err != nil {
			rewrittenBytes = req.RawBody
		}

		upstreamURL := strings.TrimRight(bucket.Provider.BaseURL, "/") + "/chat/completions"
		upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(rewrittenBytes))
		if err != nil {
			attempts = append(attempts, fmt.Sprintf("%s:req_err", bucket.Key()))
			continue
		}

		upstreamReq.Header.Set("Content-Type", "application/json")
		if key := bucket.APIKey(); key != "" {
			upstreamReq.Header.Set("Authorization", "Bearer "+key)
		}
		if bucket.Provider.Name == "openrouter" || strings.Contains(bucket.Provider.BaseURL, "openrouter.ai") {
			upstreamReq.Header.Set("HTTP-Referer", "http://localhost:8080")
			upstreamReq.Header.Set("X-Title", "Freegate")
		}

		resp, err := r.Client.Do(upstreamReq)
		if err != nil {
			bucket.RecordFailure(err.Error(), attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:conn_err", bucket.Key()))
			r.Logger.Warn("upstream connection error", "bucket", bucket.Key(), "err", err)
			continue
		}

		// 429 Rate Limit
		if resp.StatusCode == http.StatusTooManyRequests {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusTooManyRequests
			cooldownUntil := ParseRetryAfter(resp.Header.Get("Retry-After"), attemptTime)
			if IsQuotaExhaustionError(body) {
				cooldownUntil = attemptTime.Add(10 * time.Minute)
				r.Logger.Warn("upstream quota exhausted, cooling down for 10m", "bucket", bucket.Key())
			} else {
				r.Logger.Warn("rate limited upstream", "bucket", bucket.Key(), "cooldown_until", cooldownUntil)
			}
			bucket.RecordCooldown(cooldownUntil, "429 rate limit", attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:429", bucket.Key()))
			continue
		}

		// 413 Payload Too Large / Request Entity Too Large
		if resp.StatusCode == http.StatusRequestEntityTooLarge {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusRequestEntityTooLarge
			bucket.RecordCooldown(attemptTime.Add(60*time.Second), "413 payload too large", attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:413", bucket.Key()))
			r.Logger.Warn("upstream payload too large, failing over", "bucket", bucket.Key())
			continue
		}

		// 5xx Server Error
		if resp.StatusCode >= 500 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			bucket.RecordFailure(fmt.Sprintf("HTTP %d", resp.StatusCode), attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:%d", bucket.Key(), resp.StatusCode))
			r.Logger.Warn("upstream 5xx error", "bucket", bucket.Key(), "status", resp.StatusCode)
			continue
		}

		// 401 / 403 Auth Error
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = resp.StatusCode
			bucket.Disable(fmt.Sprintf("auth error HTTP %d", resp.StatusCode))
			attempts = append(attempts, fmt.Sprintf("%s:%d", bucket.Key(), resp.StatusCode))
			r.Logger.Error("provider auth error, disabled bucket", "bucket", bucket.Key(), "status", resp.StatusCode)
			continue
		}

		// 404 Model Not Found
		if resp.StatusCode == http.StatusNotFound {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusNotFound
			bucket.Disable("model not found 404")
			attempts = append(attempts, fmt.Sprintf("%s:404", bucket.Key()))
			r.Logger.Warn("upstream model not found", "bucket", bucket.Key())
			continue
		}

		// 400 Bad Request
		if resp.StatusCode == http.StatusBadRequest {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusBadRequest

			// Check if this 400 is actually a TPM / rate limit error from provider
			if IsRateOrTPMLimitError(resp.StatusCode, body) {
				bucket.RecordCooldown(attemptTime.Add(60*time.Second), "TPM limit exceeded", attemptTime)
				attempts = append(attempts, fmt.Sprintf("%s:400_tpm_limit", bucket.Key()))
				r.Logger.Warn("model TPM/rate limit exceeded on 400, failing over", "bucket", bucket.Key())
				continue
			}

			if IsContextLengthError(resp.StatusCode, body) {
				attempts = append(attempts, fmt.Sprintf("%s:400_context_length", bucket.Key()))
				// Look for a bucket with larger context window in subsequent attempts
				continue
			}

			// Provider-specific 400 (e.g. tool formatting quirks, parameter rejection):
			// Fail over to remaining candidate models.
			bucket.RecordCooldown(attemptTime.Add(60*time.Second), "400 bad request", attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:400", bucket.Key()))
			r.Logger.Warn("upstream returned 400 Bad Request, attempting failover",
				"bucket", bucket.Key(),
				"error", ParseErrorMessage(body),
			)
			continue
		}

		// Success! (200 OK)
		attempts = append(attempts, fmt.Sprintf("%s:ok", bucket.Key()))
		attemptsHeader := strings.Join(attempts, ",")

		// Record session stickiness
		if req.SessionID != "" {
			r.Sessions.SetStickyBucket(req.SessionID, bucket.Key(), attemptTime)
		}

		// Parse proactive rate-limit headers
		remReq, remTok, resetDur := ParseRateLimitHeaders(resp.Header, attemptTime)
		bucket.RecordSuccess(remReq, remTok, attemptTime)
		if (remReq == 0 || remTok == 0) && resetDur > 0 {
			bucket.RecordCooldown(attemptTime.Add(resetDur), "proactive rate limit 0 remaining", attemptTime)
		}

		// Forward response headers
		for k, vv := range resp.Header {
			if hopByHopHeaders[http.CanonicalHeaderKey(k)] {
				continue
			}
			for _, v := range vv {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-Freegate-Attempts", attemptsHeader)

		// Streaming vs non-streaming
		if !req.Stream {
			w.WriteHeader(resp.StatusCode)
			_, _ = io.Copy(w, resp.Body)
			resp.Body.Close()
			r.Logger.Info("handled chat request",
				"bucket", bucket.Key(),
				"stream", false,
				"attempts", attemptsHeader,
				"duration", time.Since(start),
			)
			return
		}

		// Streaming SSE
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
			n, rErr := resp.Body.Read(buf)
			if n > 0 {
				if _, wErr := w.Write(buf[:n]); wErr != nil {
					resp.Body.Close()
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rErr != nil {
				if rErr != io.EOF && !errors.Is(rErr, context.Canceled) && !errors.Is(ctx.Err(), context.Canceled) {
					errJSON, _ := json.Marshal(map[string]any{
						"error": map[string]string{
							"message": "Upstream streaming read error: " + rErr.Error(),
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
		resp.Body.Close()
		r.Logger.Info("handled streaming chat request",
			"bucket", bucket.Key(),
			"stream", true,
			"attempts", attemptsHeader,
			"duration", time.Since(start),
		)
		return
	}

	// All attempts exhausted
	attemptsHeader := strings.Join(attempts, ",")
	w.Header().Set("X-Freegate-Attempts", attemptsHeader)
	w.Header().Set("Content-Type", "application/json")

	if lastErrStatusCode == http.StatusBadRequest {
		w.WriteHeader(http.StatusBadRequest)
		if len(lastErrBody) > 0 {
			_, _ = w.Write(lastErrBody)
			return
		}
	} else {
		w.WriteHeader(http.StatusBadGateway)
	}

	msg := "All attempts to upstream providers failed."
	if len(lastErrBody) > 0 {
		msg = string(lastErrBody)
	} else if lastErrResp != nil {
		msg = fmt.Sprintf("Last error status: %d", lastErrResp.StatusCode)
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]string{
			"message": msg,
			"type":    "upstream_error",
			"code":    "bad_gateway",
		},
	})
}
