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

const (
	// minCompletionTokens is the room a model must leave for output after the
	// prompt for it to be a useful candidate.
	minCompletionTokens = 256
	// softCapTTL is how long a size ceiling learned from a "request too large"
	// error without a durable limit stays in force.
	softCapTTL = 60 * time.Second
	// defaultTTFTTimeout is the maximum time to wait for the first chunk in a stream
	defaultTTFTTimeout = 8 * time.Second
)

// promptMargin pads the prompt estimate before comparing it with a context window.
func promptMargin(estTokens int) int { return estTokens / 8 }

// promptFits reports whether the prompt plus a minimal completion fits the window.
func promptFits(window int, req *ParsedRequest, estTokens int) bool {
	room := req.MaxTokens
	if room > minCompletionTokens {
		room = minCompletionTokens
	}
	return window >= estTokens+promptMargin(estTokens)+room
}

// maxCompletionFor returns the largest completion that fits the window after
// the (padded) prompt, or 0 if the window is unknown.
func maxCompletionFor(window int, req *ParsedRequest) int {
	if window <= 0 {
		return 0
	}
	return window - req.EstimatedTokens - req.EstimatedTokens/8
}

// Router handles request filtering, ordering, attempt loops, and failover.
type Router struct {
	Config     *config.Config
	Registry   *registry.Registry
	Sessions   *SessionStore
	Client     *http.Client
	Logger     *slog.Logger
	Calibrator *TokenCalibrator
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
		Config:     cfg,
		Registry:   reg,
		Sessions:   NewSessionStore(),
		Client:     client,
		Logger:     logger,
		Calibrator: NewTokenCalibrator(),
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

		// Calibrate estimated tokens for this provider
		estTokens := r.Calibrator.Calibrate(b.Provider.Name, req.EstimatedTokens)

		// Hard constraint 4: context window.
		if snap.ContextWindow > 0 && !promptFits(snap.ContextWindow, req, estTokens) {
			continue
		}

		// Hard constraint 4b: size ceiling learned from earlier failures
		if snap.MaxPromptTokens > 0 && estTokens >= snap.MaxPromptTokens {
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

		// Hard constraint 7: proactive token budget
		if snap.BudgetTokens >= 0 && int64(estTokens) > snap.BudgetTokens {
			inCooldownOnly = append(inCooldownOnly, b)
			continue
		}

		// Hard constraint 8: local rate limits (RPM, TPM, RPD)
		remReq, remTok, remDaily, waitReq, waitTok, waitDaily := b.LocalQuotaLocked(now)
		if (b.RPM > 0 && remReq <= 0) || (b.TPM > 0 && int64(estTokens) > remTok) || (b.RPD > 0 && remDaily <= 0) {
			inCooldownOnly = append(inCooldownOnly, b)
			_ = waitReq
			_ = waitTok
			_ = waitDaily
			continue
		}

		eligible = append(eligible, b)
	}

	return eligible, inCooldownOnly
}

// waitUntil returns when a bucket that FilterEligible placed in the
// "not now" list should become usable: its cooldown end, end of provider token window,
// or local rate limit reset.
func (r *Router) waitUntil(b *registry.Bucket, now time.Time) time.Time {
	snap := b.Snapshot(now)
	if snap.State == "cooldown" {
		return snap.CooldownUntil
	}
	if !snap.BudgetResetAt.IsZero() {
		return snap.BudgetResetAt
	}

	_, _, _, waitReq, waitTok, waitDaily := b.LocalQuotaLocked(now)
	maxWait := waitReq
	if waitTok > maxWait {
		maxWait = waitTok
	}
	if waitDaily > maxWait {
		maxWait = waitDaily
	}
	if maxWait > 0 {
		return now.Add(maxWait)
	}

	return now
}

// recordTooLarge handles errors that mean "this request is too big" (HTTP 413,
// or a 400 saying "request too large"). The bucket is NOT cooled down, since
// smaller requests still work; instead it learns a size ceiling.
func (r *Router) recordTooLarge(b *registry.Bucket, req *ParsedRequest, body []byte, sentMax int, now time.Time) {
	if limit, _, ok := ParseRequestLimit(body); ok {
		// The provider's limit covers prompt + completion; convert it into a
		// ceiling on the prompt estimate, but never below half the limit so a
		// huge max_tokens cannot drive the ceiling to ~0.
		cap := limit - sentMax
		if cap < limit/2 {
			cap = limit / 2
		}
		b.RecordHardCap(cap)
		return
	}
	estTokens := r.Calibrator.Calibrate(b.Provider.Name, req.EstimatedTokens)
	b.RecordSoftCap(estTokens, now, now.Add(softCapTTL))
}

// scoreBucket calculates an intelligent ranking score based on tier, context headroom,
// remaining quota, latency EWMA, and error rate.
func (r *Router) scoreBucket(b *registry.Bucket, req *ParsedRequest, now time.Time) float64 {
	snap := b.Snapshot(now)
	estTokens := r.Calibrator.Calibrate(b.Provider.Name, req.EstimatedTokens)

	var score float64

	// 1. Tier score
	if req.IsComplex {
		score += float64(snap.Tier) * 100.0
	} else {
		score += float64(4-snap.Tier) * 100.0
	}

	// 2. Context headroom preference for large requests (> 4k tokens)
	if estTokens >= 4000 && snap.ContextWindow > 0 {
		headroom := float64(snap.ContextWindow - estTokens)
		if headroom > 0 {
			// Ratio of available window remaining
			headroomRatio := headroom / float64(snap.ContextWindow)
			// Boost models with huge headroom (e.g. Gemini 1M window for 100k prompt vs Groq 131k window)
			score += headroomRatio * 80.0
			if snap.ContextWindow >= 500000 {
				score += 30.0 // bonus for mega-context models on large requests
			}
		} else {
			score -= 100.0 // tight or no headroom
		}
	}

	// 3. Remaining quota: unknown (-1) is treated as healthy, not zero!
	if snap.RemainingRequests >= 0 {
		if snap.RemainingRequests < 5 {
			score -= float64(5-snap.RemainingRequests) * 15.0
		} else {
			score += 10.0
		}
	} else {
		// Unknown (-1) quota: treat as healthy
		score += 10.0
	}

	// Remaining tokens
	if snap.RemainingTokens >= 0 {
		if snap.RemainingTokens < int64(estTokens)*2 {
			score -= 30.0
		}
	}

	// 4. Latency EWMA: slight preference for faster models
	if snap.LatencyMs > 0 {
		score -= float64(snap.LatencyMs) / 200.0
	}

	// 5. Recent failures penalty
	score -= float64(snap.ConsecutiveFailures) * 40.0

	return score
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

		if isAuto {
			scoreI := r.scoreBucket(bI, req, now)
			scoreJ := r.scoreBucket(bJ, req, now)
			if scoreI != scoreJ {
				return scoreI > scoreJ
			}
			return bI.Key() < bJ.Key()
		}

		snapI := bI.Snapshot(now)
		snapJ := bJ.Snapshot(now)

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
		minCooldown := r.waitUntil(inCooldown[0], now)
		for _, b := range inCooldown[1:] {
			u := r.waitUntil(b, now)
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
			r.logRouteExhaustion(inCooldown, now)
			minCooldown := r.waitUntil(inCooldown[0], now)
			for _, b := range inCooldown[1:] {
				if u := r.waitUntil(b, now); u.Before(minCooldown) {
					minCooldown = u
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

	// The client's original max_tokens, so a clamp for one bucket does not leak
	// into the next attempt.
	origMaxRaw := req.ReqMap[req.MaxTokensKey]

	var lastErrResp *http.Response
	var lastErrBody []byte
	var lastErrStatusCode int
	lastFailureReason := ""

	for i, bucket := range ordered {
		if i > 0 {
			r.logBucketChange(ordered[i-1], bucket, lastFailureReason)
		}
		attemptTime := time.Now()

		// Half-Open circuit breaker: if bucket is in half-open state, ensure we acquire probe
		isHalfOpen := bucket.State(attemptTime) == "half_open"
		if isHalfOpen {
			if !bucket.TryAcquireProbe(attemptTime) {
				// Another concurrent request is already probing this bucket; skip it
				continue
			}
		}

		// Prepare rewritten request body
		newModelRaw, _ := json.Marshal(bucket.ModelID)
		req.ReqMap["model"] = newModelRaw
		if msgsRaw, ok := req.ReqMap["messages"]; ok {
			req.ReqMap["messages"] = sanitizeMessagesRaw(msgsRaw)
		}
		// Clamp an explicit max_tokens to what this bucket's window leaves after
		// the prompt, instead of letting a large client default cause a 400.
		sentMax := 0
		if req.MaxTokensKey != "" {
			req.ReqMap[req.MaxTokensKey] = origMaxRaw
			sentMax = req.MaxTokens
			if allowed := maxCompletionFor(bucket.ContextWindow, req); allowed > 0 && allowed < sentMax {
				if clamped, mErr := json.Marshal(allowed); mErr == nil {
					req.ReqMap[req.MaxTokensKey] = clamped
					sentMax = allowed
				}
			}
		}
		rewrittenBytes, err := json.Marshal(req.ReqMap)
		if err != nil {
			rewrittenBytes = req.RawBody
		}

		upstreamURL := strings.TrimRight(bucket.Provider.BaseURL, "/") + "/chat/completions"
		upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, upstreamURL, bytes.NewReader(rewrittenBytes))
		if err != nil {
			if isHalfOpen {
				bucket.ReleaseProbe()
			}
			attempts = append(attempts, fmt.Sprintf("%s:req_err", bucket.Key()))
			lastFailureReason = "request_error"
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
			if ctx.Err() != nil {
				if isHalfOpen {
					bucket.ReleaseProbe()
				}
				// The client went away; that says nothing about this provider, so
				// it must not count toward the circuit breaker.
				r.Logger.Info("client canceled request", "bucket", bucket.Key(), "duration", time.Since(start))
				return
			}
			bucket.RecordFailure(err.Error(), attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:conn_err", bucket.Key()))
			r.Logger.Warn("upstream connection error", "bucket", bucket.Key(), "err", err)
			lastFailureReason = "connection_error"
			continue
		}

		// 429 Rate Limit
		if resp.StatusCode == http.StatusTooManyRequests {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusTooManyRequests
			cooldownUntil := ParseRetryAfter(resp.Header.Get("Retry-After"), attemptTime)
			if IsDailyQuotaError(body) {
				// Daily limit: cool down until midnight UTC
				nextMidnight := time.Date(attemptTime.UTC().Year(), attemptTime.UTC().Month(), attemptTime.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
				cooldownUntil = nextMidnight
				r.Logger.Warn("upstream daily quota exhausted, cooling down until midnight UTC", "bucket", bucket.Key(), "until", cooldownUntil)
				lastFailureReason = "daily_quota_exhausted"
			} else if IsQuotaExhaustionError(body) {
				if cooldownUntil.Sub(attemptTime) < 10*time.Minute {
					cooldownUntil = attemptTime.Add(10 * time.Minute)
				}
				r.Logger.Warn("upstream quota exhausted, cooling down for 10m", "bucket", bucket.Key())
				lastFailureReason = "quota_exhausted"
			} else {
				r.Logger.Warn("rate limited upstream", "bucket", bucket.Key(), "cooldown_until", cooldownUntil)
				lastFailureReason = "rate_limited"
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
			r.recordTooLarge(bucket, req, body, sentMax, attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:413", bucket.Key()))
			r.Logger.Warn("upstream payload too large, failing over", "bucket", bucket.Key())
			lastFailureReason = "payload_too_large"
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
			lastFailureReason = "upstream_error"
			continue
		}

		// 401 / 403 Auth Error
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = resp.StatusCode

			// 403 can happen when free models rotate or on model permission limits
			if resp.StatusCode == http.StatusForbidden && (bucket.Provider.Name == "openrouter" || strings.Contains(string(body), "free") || strings.Contains(string(body), "model")) {
				bucket.DisableUntil(attemptTime.Add(30*time.Minute), fmt.Sprintf("model 403 forbidden: %s", ParseErrorMessage(body)))
				r.Logger.Warn("model 403 forbidden (temporary rotation), disabled for 30m", "bucket", bucket.Key(), "status", resp.StatusCode)
			} else {
				bucket.Disable(fmt.Sprintf("auth error HTTP %d", resp.StatusCode))
				r.Logger.Error("provider auth error, disabled bucket", "bucket", bucket.Key(), "status", resp.StatusCode)
			}
			attempts = append(attempts, fmt.Sprintf("%s:%d", bucket.Key(), resp.StatusCode))
			lastFailureReason = "authentication_error"
			continue
		}

		// 402 Payment Required
		if resp.StatusCode == http.StatusPaymentRequired {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusPaymentRequired
			bucket.RecordCooldown(attemptTime.Add(10*time.Minute), "402 payment required", attemptTime)
			attempts = append(attempts, fmt.Sprintf("%s:402", bucket.Key()))
			r.Logger.Warn("upstream payment required, cooling down for 10m", "bucket", bucket.Key(), "error", ParseErrorMessage(body))
			lastFailureReason = "payment_required"
			continue
		}

		// 404 Model Not Found
		if resp.StatusCode == http.StatusNotFound {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusNotFound
			// OpenRouter free models rotate, so use a timed disable (30m) with re-probe
			bucket.DisableUntil(attemptTime.Add(30*time.Minute), "model not found 404")
			attempts = append(attempts, fmt.Sprintf("%s:404", bucket.Key()))
			r.Logger.Warn("upstream model not found, disabled for 30m", "bucket", bucket.Key())
			lastFailureReason = "model_not_found"
			continue
		}

		// 400 Bad Request
		if resp.StatusCode == http.StatusBadRequest {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			resp.Body.Close()
			lastErrBody = body
			lastErrStatusCode = http.StatusBadRequest

			// "Request too large" depends on this request's size, not on the bucket's
			// health: learn a size ceiling and fail over without a cooldown.
			if IsRequestTooLarge(resp.StatusCode, body) {
				r.recordTooLarge(bucket, req, body, sentMax, attemptTime)
				attempts = append(attempts, fmt.Sprintf("%s:400_tpm_limit", bucket.Key()))
				r.Logger.Warn("request too large for model, failing over", "bucket", bucket.Key())
				lastFailureReason = "token_rate_limited"
				continue
			}

			// Check if this 400 is actually a TPM / rate limit error from provider
			if IsRateOrTPMLimitError(resp.StatusCode, body) {
				bucket.RecordCooldown(attemptTime.Add(60*time.Second), "TPM limit exceeded", attemptTime)
				attempts = append(attempts, fmt.Sprintf("%s:400_tpm_limit", bucket.Key()))
				r.Logger.Warn("model TPM/rate limit exceeded on 400, failing over", "bucket", bucket.Key())
				lastFailureReason = "token_rate_limited"
				continue
			}

			if IsContextLengthError(resp.StatusCode, body) {
				attempts = append(attempts, fmt.Sprintf("%s:400_context_length", bucket.Key()))
				// Learn that prompts this large do not fit here, so the next request
				// of this size skips the bucket instead of failing again. Smaller
				// requests still use it.
				estTokens := r.Calibrator.Calibrate(bucket.Provider.Name, req.EstimatedTokens)
				bucket.RecordHardCap(estTokens)
				if cw, ok := ParseContextWindowLimit(body); ok {
					bucket.UpdateContextWindow(cw)
				}
				lastFailureReason = "context_length_exceeded"
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
			lastFailureReason = "bad_request"
			continue
		}

		// Streaming verification: peek at first chunk with TTFT timeout BEFORE committing headers!
		var firstChunk []byte
		var readErr error

		if req.Stream {
			type chunkResult struct {
				data []byte
				err  error
			}
			ch := make(chan chunkResult, 1)
			buf := make([]byte, 4096)

			go func() {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					d := make([]byte, n)
					copy(d, buf[:n])
					ch <- chunkResult{data: d, err: err}
				} else {
					ch <- chunkResult{data: nil, err: err}
				}
			}()

			select {
			case <-ctx.Done():
				resp.Body.Close()
				if isHalfOpen {
					bucket.ReleaseProbe()
				}
				r.Logger.Info("client canceled during stream TTFT wait", "bucket", bucket.Key())
				return
			case <-time.After(defaultTTFTTimeout):
				resp.Body.Close()
				bucket.RecordFailure("time to first token timeout (8s)", attemptTime)
				attempts = append(attempts, fmt.Sprintf("%s:ttft_timeout", bucket.Key()))
				r.Logger.Warn("upstream streaming TTFT timeout, failing over", "bucket", bucket.Key())
				lastFailureReason = "stream_timeout"
				continue
			case res := <-ch:
				firstChunk = res.data
				readErr = res.err
			}

			if len(firstChunk) == 0 && readErr != nil {
				resp.Body.Close()
				bucket.RecordFailure("stream closed before first chunk: "+readErr.Error(), attemptTime)
				attempts = append(attempts, fmt.Sprintf("%s:stream_err", bucket.Key()))
				r.Logger.Warn("upstream stream closed before first chunk, failing over", "bucket", bucket.Key(), "err", readErr)
				lastFailureReason = "stream_error"
				continue
			}

			// Check if first chunk contains an SSE error payload!
			if IsStreamErrorPayload(firstChunk) {
				resp.Body.Close()
				if IsRateOrTPMLimitError(http.StatusOK, firstChunk) {
					bucket.RecordCooldown(attemptTime.Add(60*time.Second), "streaming rate limit error", attemptTime)
					attempts = append(attempts, fmt.Sprintf("%s:stream_tpm", bucket.Key()))
					lastFailureReason = "token_rate_limited"
				} else {
					bucket.RecordFailure("streaming error payload: "+string(firstChunk), attemptTime)
					attempts = append(attempts, fmt.Sprintf("%s:stream_err_payload", bucket.Key()))
					lastFailureReason = "stream_error"
				}
				r.Logger.Warn("upstream stream error payload in first chunk, failing over", "bucket", bucket.Key())
				continue
			}
		}

		// Success! (200 OK)
		attempts = append(attempts, fmt.Sprintf("%s:ok", bucket.Key()))
		attemptsHeader := strings.Join(attempts, ",")

		// Record response latency
		duration := time.Since(attemptTime)
		bucket.RecordLatency(duration)

		// Record session stickiness
		if req.SessionID != "" {
			r.Sessions.SetStickyBucket(req.SessionID, bucket.Key(), attemptTime)
		}

		// Parse proactive rate-limit headers
		remReq, remTok, resetDur := ParseRateLimitHeaders(resp.Header, attemptTime)
		detailedHeaders := ParseDetailedRateLimitHeaders(resp.Header, attemptTime)
		bucket.RecordSuccess(remReq, remTok, attemptTime)

		// Record budget window using token reset duration if available
		budgetReset := detailedHeaders.ResetTokens
		if budgetReset <= 0 {
			budgetReset = detailedHeaders.ResetGeneral
		}
		if remTok >= 0 {
			bucket.RecordBudgetWindow(budgetReset, attemptTime)
		}
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
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()

			// Extract usage to calibrate token estimation and update local counters
			var usageResp struct {
				Usage struct {
					PromptTokens int `json:"prompt_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(body, &usageResp); err == nil && usageResp.Usage.PromptTokens > 0 {
				r.Calibrator.RecordUsage(bucket.Provider.Name, req.EstimatedTokens, usageResp.Usage.PromptTokens)
				bucket.RecordLocalUsage(usageResp.Usage.PromptTokens, attemptTime)
			} else {
				bucket.RecordLocalUsage(req.EstimatedTokens, attemptTime)
			}

			w.WriteHeader(resp.StatusCode)
			_, _ = w.Write(body)
			r.Logger.Info("handled chat request",
				"bucket", bucket.Key(),
				"stream", false,
				"attempts", attemptsHeader,
				"duration", time.Since(start),
			)
			return
		}

		// Streaming SSE: firstChunk was already verified
		bucket.RecordLocalUsage(req.EstimatedTokens, attemptTime)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(resp.StatusCode)

		flusher, _ := w.(http.Flusher)
		if len(firstChunk) > 0 {
			if _, wErr := w.Write(firstChunk); wErr != nil {
				resp.Body.Close()
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}

		if readErr == nil {
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
	if ctx.Err() == nil {
		r.logRouteExhaustion(ordered, time.Now())
	}

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

func (r *Router) logBucketChange(from, to *registry.Bucket, reason string) {
	if from.ModelID != to.ModelID {
		r.Logger.Info("model changed",
			"from", from.ModelID,
			"to", to.ModelID,
			"reason", reason,
		)
	}
	if from.Provider.Name != to.Provider.Name {
		r.Logger.Info("provider changed",
			"from", from.Provider.Name,
			"to", to.Provider.Name,
			"model", to.ModelID,
			"reason", reason,
		)
	}
}

func (r *Router) logRouteExhaustion(buckets []*registry.Bucket, now time.Time) {
	if len(buckets) == 0 {
		return
	}

	providers := make(map[string]struct{})
	var routes []string
	var retryAt time.Time
	allLimitsReached := true
	for _, bucket := range buckets {
		snapshot := bucket.Snapshot(now)
		routes = append(routes, snapshot.Key)
		switch snapshot.LastError {
		case "429 rate limit", "TPM limit exceeded", "402 payment required", "proactive rate limit 0 remaining":
		default:
			allLimitsReached = false
		}
		providers[snapshot.Provider] = struct{}{}
		if snapshot.CooldownUntil.After(now) &&
			(retryAt.IsZero() || snapshot.CooldownUntil.Before(retryAt)) {
			retryAt = snapshot.CooldownUntil
		}
	}

	providerNames := make([]string, 0, len(providers))
	for name := range providers {
		providerNames = append(providerNames, name)
	}
	sort.Strings(providerNames)

	var retryAfter time.Duration
	if !retryAt.IsZero() {
		retryAfter = retryAt.Sub(now).Round(time.Second)
	}

	title := "ALL ELIGIBLE ROUTES EXHAUSTED"
	if allLimitsReached {
		title = "ALL PROVIDER LIMITS REACHED"
	}
	border := "+" + strings.Repeat("-", len(title)+4) + "+"
	r.Logger.Warn(border)
	r.Logger.Warn("|  " + title + "  |")
	r.Logger.Warn(border)
	r.Logger.Warn("route exhaustion summary",
		"providers", strings.Join(providerNames, ","),
		"routes", strings.Join(routes, ","),
		"retry_after", retryAfter,
	)
}
