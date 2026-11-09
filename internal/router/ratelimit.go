package router

import (
	"bytes"
	"encoding/json"
	"net/http"
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

// ParseRateLimitHeaders parses rate-limit headers from providers.
// Returns remReq (-1 if not present), remTok (-1 if not present), and reset duration.
func ParseRateLimitHeaders(header http.Header, now time.Time) (int64, int64, time.Duration) {
	remReq := int64(-1)
	remTok := int64(-1)
	var resetDur time.Duration

	// Check remaining requests
	for _, k := range []string{
		"x-ratelimit-remaining-requests",
		"ratelimit-remaining-requests",
		"x-ratelimit-remaining",
	} {
		if v := header.Get(k); v != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				remReq = n
				break
			}
		}
	}

	// Check remaining tokens
	for _, k := range []string{
		"x-ratelimit-remaining-tokens",
		"ratelimit-remaining-tokens",
	} {
		if v := header.Get(k); v != "" {
			if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil {
				remTok = n
				break
			}
		}
	}

	// Check reset duration
	for _, k := range []string{
		"x-ratelimit-reset-requests",
		"x-ratelimit-reset-tokens",
		"x-ratelimit-reset",
		"ratelimit-reset",
	} {
		if v := header.Get(k); v != "" {
			v = strings.TrimSpace(v)
			// Try Go duration string (e.g. "6m0s", "100ms", "2s")
			if d, err := time.ParseDuration(v); err == nil && d > 0 {
				resetDur = d
				break
			}
			// Try float / int seconds
			if fSecs, err := strconv.ParseFloat(v, 64); err == nil && fSecs > 0 {
				resetDur = time.Duration(fSecs * float64(time.Second))
				break
			}
			// Try epoch timestamp
			if epoch, err := strconv.ParseInt(v, 10, 64); err == nil && epoch > now.Unix() {
				resetDur = time.Unix(epoch, 0).Sub(now)
				break
			}
		}
	}

	return remReq, remTok, resetDur
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
