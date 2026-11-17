package router

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ParseRetryAfter parses a Retry-After header which can be seconds (integer or float) or an HTTP-date.
// If missing or unparseable, it defaults to 30s.
func ParseRetryAfter(val string, now time.Time) time.Time {
	val = strings.TrimSpace(val)
	if val == "" {
		return now.Add(30 * time.Second)
	}

	// 1. Try integer seconds
	if secs, err := strconv.ParseInt(val, 10, 64); err == nil && secs >= 0 {
		return now.Add(time.Duration(secs) * time.Second)
	}

	// 2. Try float seconds
	if fSecs, err := strconv.ParseFloat(val, 64); err == nil && fSecs >= 0 {
		return now.Add(time.Duration(fSecs * float64(time.Second)))
	}

	// 3. Try HTTP Date (RFC1123, RFC850, ANSIC)
	if t, err := http.ParseTime(val); err == nil {
		if t.After(now) {
			return t
		}
		return now.Add(30 * time.Second)
	}

	// Default fallback
	return now.Add(30 * time.Second)
}

func parseResetDuration(val string, now time.Time) time.Duration {
	val = strings.TrimSpace(val)
	if val == "" {
		return 0
	}
	// Try Go duration string (e.g. "6m0s", "100ms", "2s")
	if d, err := time.ParseDuration(val); err == nil && d > 0 {
		return d
	}
	// Try float / int seconds
	if fSecs, err := strconv.ParseFloat(val, 64); err == nil && fSecs > 0 {
		return time.Duration(fSecs * float64(time.Second))
	}
	// Try epoch timestamp
	if epoch, err := strconv.ParseInt(val, 10, 64); err == nil && epoch > now.Unix() {
		return time.Unix(epoch, 0).Sub(now)
	}
	return 0
}

// RateLimitHeaderInfo holds detailed rate limit header values.
type RateLimitHeaderInfo struct {
	RemainingRequests int64
	RemainingTokens   int64
	ResetRequests     time.Duration
	ResetTokens       time.Duration
	ResetGeneral      time.Duration
}

// ParseDetailedRateLimitHeaders parses all dimension-specific rate-limit headers.
func ParseDetailedRateLimitHeaders(header http.Header, now time.Time) RateLimitHeaderInfo {
	info := RateLimitHeaderInfo{
		RemainingRequests: -1,
		RemainingTokens:   -1,
	}

	// Remaining requests
	for _, k := range []string{
		"x-ratelimit-remaining-requests",
		"ratelimit-remaining-requests",
		"x-ratelimit-remaining",
	} {
		if v := header.Get(k); v != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				info.RemainingRequests = n
				break
			}
		}
	}

	// Remaining tokens
	for _, k := range []string{
		"x-ratelimit-remaining-tokens",
		"ratelimit-remaining-tokens",
	} {
		if v := header.Get(k); v != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				info.RemainingTokens = n
				break
			}
		}
	}

	// Reset requests
	for _, k := range []string{
		"x-ratelimit-reset-requests",
		"ratelimit-reset-requests",
	} {
		if v := header.Get(k); v != "" {
			if d := parseResetDuration(v, now); d > 0 {
				info.ResetRequests = d
				break
			}
		}
	}

	// Reset tokens
	for _, k := range []string{
		"x-ratelimit-reset-tokens",
		"ratelimit-reset-tokens",
	} {
		if v := header.Get(k); v != "" {
			if d := parseResetDuration(v, now); d > 0 {
				info.ResetTokens = d
				break
			}
		}
	}

	// Reset general
	for _, k := range []string{
		"x-ratelimit-reset",
		"ratelimit-reset",
	} {
		if v := header.Get(k); v != "" {
			if d := parseResetDuration(v, now); d > 0 {
				info.ResetGeneral = d
				break
			}
		}
	}

	return info
}

// ParseRateLimitHeaders parses rate-limit headers from providers.
// Returns remReq (-1 if not present), remTok (-1 if not present), and reset duration.
// The reset duration matches the dimension that hit zero, avoiding cooling down for 2s
// when token limits require 60s.
func ParseRateLimitHeaders(header http.Header, now time.Time) (int64, int64, time.Duration) {
	info := ParseDetailedRateLimitHeaders(header, now)
	var resetDur time.Duration

	if info.RemainingTokens == 0 && info.ResetTokens > 0 {
		resetDur = info.ResetTokens
	} else if info.RemainingRequests == 0 && info.ResetRequests > 0 {
		resetDur = info.ResetRequests
	} else if info.ResetGeneral > 0 {
		resetDur = info.ResetGeneral
	} else if info.ResetTokens > 0 {
		resetDur = info.ResetTokens
	} else if info.ResetRequests > 0 {
		resetDur = info.ResetRequests
	}

	return info.RemainingRequests, info.RemainingTokens, resetDur
}

// IsContextLengthError checks if a 400 error was caused by exceeding context length.
func IsContextLengthError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	lower := bytes.ToLower(body)
	patterns := [][]byte{
		[]byte("context_length_exceeded"),
		[]byte("maximum context length"),
		[]byte("context window"),
		[]byte("prompt is too long"),
		[]byte("too many tokens"),
		[]byte("max_tokens is too large"),
		[]byte("exceeds the context"),
	}
	for _, p := range patterns {
		if bytes.Contains(lower, p) {
			return true
		}
	}
	return false
}

var contextLengthRe = regexp.MustCompile(`(?i)(?:maximum context length|context window|context length)\s*(?:is|of|:)?\s*(\d+)`)

// ParseContextWindowLimit extracts the maximum context length from error messages if present.
func ParseContextWindowLimit(body []byte) (int, bool) {
	m := contextLengthRe.FindSubmatch(body)
	if m == nil {
		return 0, false
	}
	cw, err := strconv.Atoi(string(m[1]))
	if err != nil || cw <= 0 {
		return 0, false
	}
	return cw, true
}

// IsRateOrTPMLimitError checks if an error (such as 400, 413, or 429) was caused by TPM, RPM, or quota rate limits.
func IsRateOrTPMLimitError(statusCode int, body []byte) bool {
	if statusCode == http.StatusTooManyRequests || statusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	lower := bytes.ToLower(body)
	patterns := [][]byte{
		[]byte("tokens per minute"),
		[]byte("(tpm)"),
		[]byte("requests per minute"),
		[]byte("(rpm)"),
		[]byte("rate_limit_exceeded"),
		[]byte("rate limit"),
		[]byte("request too large for model"),
		[]byte("exceeded your current quota"),
		[]byte("too many requests"),
	}
	for _, p := range patterns {
		if bytes.Contains(lower, p) {
			return true
		}
	}
	return false
}

// IsQuotaExhaustionError checks if a 429 error indicates account quota exhaustion rather than a brief RPM spike.
func IsQuotaExhaustionError(body []byte) bool {
	lower := bytes.ToLower(body)
	patterns := [][]byte{
		[]byte("exceeded your current quota"),
		[]byte("insufficient_quota"),
		[]byte("quota exceeded"),
		[]byte("check your plan and billing"),
		[]byte("credit balance is too low"),
		[]byte("free-models-per-day"),
	}
	for _, p := range patterns {
		if bytes.Contains(lower, p) {
			return true
		}
	}
	return false
}

// IsDailyQuotaError reports whether an error indicates daily quota exhaustion
// (which should cool down until midnight UTC or the daily reset time).
func IsDailyQuotaError(body []byte) bool {
	lower := bytes.ToLower(body)
	patterns := [][]byte{
		[]byte("free-models-per-day"),
		[]byte("daily limit"),
		[]byte("per day"),
		[]byte("requests per day"),
		[]byte("(rpd)"),
		[]byte("exceeded your daily"),
	}
	for _, p := range patterns {
		if bytes.Contains(lower, p) {
			return true
		}
	}
	return false
}

// ParseErrorMessage extracts human-readable error text from an upstream JSON response.
func ParseErrorMessage(body []byte) string {
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

var requestLimitRe = regexp.MustCompile(`(?i)limit\s+(\d+)\s*,\s*requested\s+(\d+)`)

// IsRequestTooLarge reports whether an error says this single request is too
// big for the model (as opposed to the account having used up its rate window).
// Such failures depend on the request's size, so they must not put the whole
// bucket into cooldown: smaller requests would still succeed.
func IsRequestTooLarge(statusCode int, body []byte) bool {
	if statusCode == http.StatusRequestEntityTooLarge {
		return true
	}
	return bytes.Contains(bytes.ToLower(body), []byte("request too large"))
}

// ParseRequestLimit extracts the per-request token limit from messages shaped
// like "... Limit 8000, Requested 8576 ...". ok is false if none is present.
func ParseRequestLimit(body []byte) (limit, requested int, ok bool) {
	m := requestLimitRe.FindSubmatch(body)
	if m == nil {
		return 0, 0, false
	}
	l, err1 := strconv.Atoi(string(m[1]))
	r, err2 := strconv.Atoi(string(m[2]))
	if err1 != nil || err2 != nil || l <= 0 {
		return 0, 0, false
	}
	return l, r, true
}

// IsStreamErrorPayload checks if an SSE chunk starts with an error payload
// (e.g. data: {"error": ...} or {"error": ...})
func IsStreamErrorPayload(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(bytes.TrimPrefix(trimmed, []byte("data:")))
	}
	if bytes.HasPrefix(trimmed, []byte(`{"error"`)) {
		return true
	}
	return false
}
