package registry

import (
	"os"
	"sync"
	"time"

	"gateway/internal/config"
)

type tokenUsageEntry struct {
	at     time.Time
	tokens int
}

// BucketSnapshot is an immutable view of a bucket's state.
type BucketSnapshot struct {
	Key                 string        `json:"key"`
	Provider            string        `json:"provider"`
	Model               string        `json:"model"`
	Tier                int           `json:"tier"`
	SupportsTools       bool          `json:"supports_tools"`
	SupportsVision      bool          `json:"supports_vision"`
	ContextWindow       int           `json:"context_window"`
	RPM                 int           `json:"rpm,omitempty"`
	TPM                 int           `json:"tpm,omitempty"`
	RPD                 int           `json:"rpd,omitempty"`
	TrainsOnData        bool          `json:"trains_on_data"`
	Local               bool          `json:"local"`
	HasKey              bool          `json:"has_key"`
	State               string        `json:"state"` // "ok", "cooldown", "breaker_open", "half_open", "disabled"
	Disabled            bool          `json:"disabled"`
	DisabledReason      string        `json:"disabled_reason,omitempty"`
	DisabledUntil       time.Time     `json:"disabled_until,omitempty"`
	CooldownUntil       time.Time     `json:"cooldown_until,omitempty"`
	CooldownRemainingS  int           `json:"cooldown_remaining_seconds"`
	BreakerOpenUntil    time.Time     `json:"breaker_open_until,omitempty"`
	RemainingRequests   int64         `json:"remaining_requests"`
	RemainingTokens     int64         `json:"remaining_tokens"`
	BudgetTokens        int64         `json:"budget_tokens"` // RemainingTokens while the reported window is live, else -1
	BudgetResetAt       time.Time     `json:"budget_reset_at,omitempty"`
	MaxPromptTokens     int           `json:"max_prompt_tokens"` // learned ceiling on estimated prompt tokens, 0 = unknown
	ConsecutiveFailures int           `json:"consecutive_failures"`
	LatencyMs           int           `json:"latency_ms,omitempty"`
	LastError           string        `json:"last_error,omitempty"`
}

// Bucket represents one (provider, model_id) pair.
type Bucket struct {
	// Static metadata
	Provider       *config.ProviderConfig
	ModelID        string
	Tier           int
	SupportsTools  bool
	SupportsVision bool
	ContextWindow  int
	RPM            int
	TPM            int
	RPD            int

	mu                  sync.RWMutex
	disabled            bool
	disabledReason      string
	disabledUntil       time.Time
	cooldownUntil       time.Time
	breakerOpenUntil    time.Time
	breakerTrips        int
	halfOpenProbeInFlight bool
	remainingRequests   int64
	remainingTokens     int64
	budgetResetAt       time.Time // when remainingTokens/remainingRequests stop being trustworthy
	hardPromptCap       int       // learned, permanent (context-length errors, provider-stated limits)
	softPromptCap       int       // learned, temporary (size-related rate-limit errors)
	softPromptCapUntil  time.Time
	consecutiveFailures int
	lastError           string
	lastUpdated         time.Time

	// Local sliding-window usage tracking for RPM, TPM, RPD
	reqHistory   []time.Time
	tokenHistory []tokenUsageEntry

	// Latency EWMA
	ewmaLatency time.Duration
}

// NewBucket creates a Bucket with initial state.
func NewBucket(p *config.ProviderConfig, m *config.ModelMetadata) *Bucket {
	return &Bucket{
		Provider:          p,
		ModelID:           m.ID,
		Tier:              m.Tier,
		SupportsTools:     m.SupportsTools,
		SupportsVision:    m.SupportsVision,
		ContextWindow:     m.ContextWindow,
		RPM:               m.RPM,
		TPM:               m.TPM,
		RPD:               m.RPD,
		remainingRequests: -1,
		remainingTokens:   -1,
	}
}

// Key returns "provider/model_id".
func (b *Bucket) Key() string {
	return b.Provider.Name + "/" + b.ModelID
}

// HasKey checks if an API key is available in the environment (or provider is local).
func (b *Bucket) HasKey() bool {
	if b.Provider.Local {
		return true
	}
	if b.Provider.APIKeyEnv == "" {
		return false
	}
	return os.Getenv(b.Provider.APIKeyEnv) != ""
}

// APIKey returns the current key from the environment.
func (b *Bucket) APIKey() string {
	if b.Provider.Local || b.Provider.APIKeyEnv == "" {
		return ""
	}
	return os.Getenv(b.Provider.APIKeyEnv)
}

// IsAvailable checks if the bucket can accept requests right now.
func (b *Bucket) IsAvailable(now time.Time) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.disabled {
		if b.disabledUntil.IsZero() || now.Before(b.disabledUntil) {
			return false
		}
	}
	if !b.cooldownUntil.IsZero() && now.Before(b.cooldownUntil) {
		return false
	}
	if !b.breakerOpenUntil.IsZero() {
		if now.Before(b.breakerOpenUntil) {
			return false
		}
		// If half-open, only available if probe not already running
		if b.consecutiveFailures >= 3 && b.halfOpenProbeInFlight {
			return false
		}
	}
	return true
}

// State returns the current bucket state: "disabled", "cooldown", "breaker_open", "half_open", or "ok".
func (b *Bucket) State(now time.Time) string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.disabled {
		if b.disabledUntil.IsZero() || now.Before(b.disabledUntil) {
			return "disabled"
		}
	}
	if !b.cooldownUntil.IsZero() && now.Before(b.cooldownUntil) {
		return "cooldown"
	}
	if !b.breakerOpenUntil.IsZero() {
		if now.Before(b.breakerOpenUntil) {
			return "breaker_open"
		}
		if b.consecutiveFailures >= 3 {
			return "half_open"
		}
	}
	return "ok"
}

// TryAcquireProbe attempts to acquire the single allowed half-open probe if the
// breaker is in half-open state. Returns true if acquired.
func (b *Bucket) TryAcquireProbe(now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.disabled {
		if b.disabledUntil.IsZero() || now.Before(b.disabledUntil) {
			return false
		}
	}
	if !b.cooldownUntil.IsZero() && now.Before(b.cooldownUntil) {
		return false
	}
	if !b.breakerOpenUntil.IsZero() && !now.Before(b.breakerOpenUntil) && b.consecutiveFailures >= 3 {
		if !b.halfOpenProbeInFlight {
			b.halfOpenProbeInFlight = true
			return true
		}
		return false
	}
	return false
}

// ReleaseProbe cancels an acquired half-open probe (e.g. if request was cancelled before outcome).
func (b *Bucket) ReleaseProbe() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.halfOpenProbeInFlight = false
}

// RecordSuccess updates stats on successful completion. Closes circuit breaker if half-open.
func (b *Bucket) RecordSuccess(remReq, remTok int64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutiveFailures = 0
	b.breakerTrips = 0
	b.breakerOpenUntil = time.Time{}
	b.halfOpenProbeInFlight = false
	b.lastError = ""
	b.lastUpdated = now
	if remReq >= 0 {
		b.remainingRequests = remReq
	}
	if remTok >= 0 {
		b.remainingTokens = remTok
	}
}

// RecordCooldown places the bucket into cooldown until a specific time.
func (b *Bucket) RecordCooldown(until time.Time, reason string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.cooldownUntil = until
	b.lastError = reason
	b.lastUpdated = now
}

// RecordFailure records a failure. 3 consecutive failures trips the breaker.
// If already in half-open state or tripped, failure causes exponential backoff (60s, 120s, 240s, up to 10m).
func (b *Bucket) RecordFailure(errStr string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.lastError = errStr
	b.lastUpdated = now

	// If in half-open or already tripped, fail immediately back to breaker_open with increased backoff
	if b.halfOpenProbeInFlight || b.consecutiveFailures >= 3 {
		b.halfOpenProbeInFlight = false
		b.consecutiveFailures++
		b.breakerTrips++
		multiplier := 1 << (b.breakerTrips - 1)
		if multiplier > 10 {
			multiplier = 10
		}
		backoff := 60 * time.Second * time.Duration(multiplier)
		if backoff > 10*time.Minute {
			backoff = 10 * time.Minute
		}
		b.breakerOpenUntil = now.Add(backoff)
		return
	}

	b.consecutiveFailures++
	if b.consecutiveFailures >= 3 {
		b.breakerTrips = 1
		b.breakerOpenUntil = now.Add(60 * time.Second)
	}
}

// RecordLatency updates the exponentially weighted moving average (EWMA) latency.
func (b *Bucket) RecordLatency(d time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.ewmaLatency <= 0 {
		b.ewmaLatency = d
	} else {
		b.ewmaLatency = time.Duration(0.8*float64(b.ewmaLatency) + 0.2*float64(d))
	}
}

// Latency returns the current EWMA latency.
func (b *Bucket) Latency() time.Duration {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.ewmaLatency
}

// RecordLocalUsage records local request and token usage for proactive rate limiting.
func (b *Bucket) RecordLocalUsage(tokens int, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Prune request history older than 24h
	cutoff24h := now.Add(-24 * time.Hour)
	idxReq := 0
	for idxReq < len(b.reqHistory) && b.reqHistory[idxReq].Before(cutoff24h) {
		idxReq++
	}
	if idxReq > 0 {
		b.reqHistory = append([]time.Time(nil), b.reqHistory[idxReq:]...)
	}
	b.reqHistory = append(b.reqHistory, now)

	// Prune token history older than 1m
	cutoff1m := now.Add(-time.Minute)
	idxTok := 0
	for idxTok < len(b.tokenHistory) && b.tokenHistory[idxTok].at.Before(cutoff1m) {
		idxTok++
	}
	if idxTok > 0 {
		b.tokenHistory = append([]tokenUsageEntry(nil), b.tokenHistory[idxTok:]...)
	}
	if tokens > 0 {
		b.tokenHistory = append(b.tokenHistory, tokenUsageEntry{at: now, tokens: tokens})
	}
}

// LocalQuotaLocked calculates remaining local RPM, TPM, RPD and their reset wait times.
// Caller holds b.mu (or RLock).
func (b *Bucket) LocalQuotaLocked(now time.Time) (remReq, remTok, remDaily int64, waitReq, waitTok, waitDaily time.Duration) {
	remReq = -1
	remTok = -1
	remDaily = -1

	// RPM check
	if b.RPM > 0 {
		cutoff1m := now.Add(-time.Minute)
		count1m := 0
		var oldest1m time.Time
		for _, t := range b.reqHistory {
			if !t.Before(cutoff1m) {
				count1m++
				if oldest1m.IsZero() || t.Before(oldest1m) {
					oldest1m = t
				}
			}
		}
		remReq = int64(b.RPM - count1m)
		if remReq <= 0 {
			remReq = 0
			if !oldest1m.IsZero() {
				waitReq = time.Minute - now.Sub(oldest1m)
			}
			if waitReq <= 0 {
				waitReq = time.Second
			}
		}
	}

	// TPM check
	if b.TPM > 0 {
		cutoff1m := now.Add(-time.Minute)
		sumTokens := 0
		var oldestTok time.Time
		for _, e := range b.tokenHistory {
			if !e.at.Before(cutoff1m) {
				sumTokens += e.tokens
				if oldestTok.IsZero() || e.at.Before(oldestTok) {
					oldestTok = e.at
				}
			}
		}
		remTok = int64(b.TPM - sumTokens)
		if remTok <= 0 {
			remTok = 0
			if !oldestTok.IsZero() {
				waitTok = time.Minute - now.Sub(oldestTok)
			}
			if waitTok <= 0 {
				waitTok = time.Second
			}
		}
	}

	// RPD check
	if b.RPD > 0 {
		cutoff24h := now.Add(-24 * time.Hour)
		count24h := 0
		for _, t := range b.reqHistory {
			if !t.Before(cutoff24h) {
				count24h++
			}
		}
		remDaily = int64(b.RPD - count24h)
		if remDaily <= 0 {
			remDaily = 0
			nextMidnight := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day()+1, 0, 0, 0, 0, time.UTC)
			waitDaily = nextMidnight.Sub(now)
			if waitDaily <= 0 {
				waitDaily = time.Minute
			}
		}
	}

	return
}

// RecordBudgetWindow records when the remaining-quota numbers last reported by
// the provider stop being valid. A non-positive reset defaults to 60s, since
// token limits are per-minute windows on the providers this gateway targets.
// After the window passes, BudgetTokens in snapshots reads -1 (unknown), so a
// stale low value can never lock a bucket out indefinitely.
func (b *Bucket) RecordBudgetWindow(reset time.Duration, now time.Time) {
	if reset <= 0 {
		reset = 60 * time.Second
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.budgetResetAt = now.Add(reset)
}

// RecordHardCap learns a permanent ceiling on estimated prompt tokens, for
// example from a context-length error or a provider-stated per-request limit.
// The smallest non-zero cap wins. Unlike a cooldown, it only affects requests
// at least this large; smaller requests keep flowing to the bucket.
func (b *Bucket) RecordHardCap(tokens int) {
	if tokens <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.hardPromptCap == 0 || tokens < b.hardPromptCap {
		b.hardPromptCap = tokens
	}
}

// RecordSoftCap learns a temporary ceiling on estimated prompt tokens that
// expires at until. Used when an error says "request too large" but gives no
// durable limit (the cause may be a transient per-minute window).
func (b *Bucket) RecordSoftCap(tokens int, now, until time.Time) {
	if tokens <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.softPromptCap == 0 || now.After(b.softPromptCapUntil) || tokens < b.softPromptCap {
		b.softPromptCap = tokens
	}
	if until.After(b.softPromptCapUntil) {
		b.softPromptCapUntil = until
	}
}

// effectivePromptCapLocked returns the smallest active cap (0 = none). Caller holds b.mu.
func (b *Bucket) effectivePromptCapLocked(now time.Time) int {
	cap := b.hardPromptCap
	if b.softPromptCap > 0 && now.Before(b.softPromptCapUntil) {
		if cap == 0 || b.softPromptCap < cap {
			cap = b.softPromptCap
		}
	}
	return cap
}

// Disable marks the bucket permanently disabled with a reason.
func (b *Bucket) Disable(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.disabled = true
	b.disabledUntil = time.Time{}
	b.disabledReason = reason
	b.lastUpdated = time.Now()
}

// DisableUntil marks the bucket disabled until a specified expiration time.
func (b *Bucket) DisableUntil(until time.Time, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.disabled = true
	b.disabledUntil = until
	b.disabledReason = reason
	b.lastUpdated = time.Now()
}

// UpdateContextWindow updates the context window if learned from discovery or error.
func (b *Bucket) UpdateContextWindow(cw int) {
	if cw > 0 {
		b.mu.Lock()
		b.ContextWindow = cw
		b.mu.Unlock()
	}
}

// Snapshot returns an immutable snapshot of the bucket.
func (b *Bucket) Snapshot(now time.Time) BucketSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	cooldownSecs := 0
	if !b.cooldownUntil.IsZero() && b.cooldownUntil.After(now) {
		cooldownSecs = int(b.cooldownUntil.Sub(now).Seconds())
	}

	state := "ok"
	if b.disabled {
		if b.disabledUntil.IsZero() || now.Before(b.disabledUntil) {
			state = "disabled"
		}
	}
	if state != "disabled" {
		if !b.cooldownUntil.IsZero() && now.Before(b.cooldownUntil) {
			state = "cooldown"
		} else if !b.breakerOpenUntil.IsZero() {
			if now.Before(b.breakerOpenUntil) {
				state = "breaker_open"
			} else if b.consecutiveFailures >= 3 {
				state = "half_open"
			}
		}
	}

	remReq := b.remainingRequests
	remTok := b.remainingTokens
	localRemReq, localRemTok, _, _, waitTok, _ := b.LocalQuotaLocked(now)

	// If provider did not report headers, fallback to local tracking
	if remReq < 0 && localRemReq >= 0 {
		remReq = localRemReq
	}
	if remTok < 0 && localRemTok >= 0 {
		remTok = localRemTok
	}

	budgetTokens := int64(-1)
	var budgetResetAt time.Time
	if !b.budgetResetAt.IsZero() && now.Before(b.budgetResetAt) {
		budgetTokens = b.remainingTokens
		budgetResetAt = b.budgetResetAt
	} else if localRemTok >= 0 {
		budgetTokens = localRemTok
		if waitTok > 0 {
			budgetResetAt = now.Add(waitTok)
		} else {
			budgetResetAt = now.Add(time.Minute)
		}
	}

	latencyMs := 0
	if b.ewmaLatency > 0 {
		latencyMs = int(b.ewmaLatency.Milliseconds())
	}

	return BucketSnapshot{
		Key:                 b.Key(),
		Provider:            b.Provider.Name,
		Model:               b.ModelID,
		Tier:                b.Tier,
		SupportsTools:       b.SupportsTools,
		SupportsVision:      b.SupportsVision,
		ContextWindow:       b.ContextWindow,
		RPM:                 b.RPM,
		TPM:                 b.TPM,
		RPD:                 b.RPD,
		TrainsOnData:        b.Provider.TrainsOnData,
		Local:               b.Provider.Local,
		HasKey:              b.HasKey(),
		State:               state,
		Disabled:            b.disabled && (b.disabledUntil.IsZero() || now.Before(b.disabledUntil)),
		DisabledReason:      b.disabledReason,
		DisabledUntil:       b.disabledUntil,
		CooldownUntil:       b.cooldownUntil,
		CooldownRemainingS:  cooldownSecs,
		BreakerOpenUntil:    b.breakerOpenUntil,
		RemainingRequests:   remReq,
		RemainingTokens:     remTok,
		BudgetTokens:        budgetTokens,
		BudgetResetAt:       budgetResetAt,
		MaxPromptTokens:     b.effectivePromptCapLocked(now),
		ConsecutiveFailures: b.consecutiveFailures,
		LatencyMs:           latencyMs,
		LastError:           b.lastError,
	}
}
