package registry

import (
	"os"
	"sync"
	"time"

	"gateway/internal/config"
)

// BucketSnapshot is an immutable view of a bucket's state.
type BucketSnapshot struct {
	Key                 string    `json:"key"`
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	Tier                int       `json:"tier"`
	SupportsTools       bool      `json:"supports_tools"`
	SupportsVision      bool      `json:"supports_vision"`
	ContextWindow       int       `json:"context_window"`
	TrainsOnData        bool      `json:"trains_on_data"`
	Local               bool      `json:"local"`
	HasKey              bool      `json:"has_key"`
	State               string    `json:"state"` // "ok", "cooldown", "breaker_open", "disabled"
	Disabled            bool      `json:"disabled"`
	DisabledReason      string    `json:"disabled_reason,omitempty"`
	CooldownUntil       time.Time `json:"cooldown_until,omitempty"`
	CooldownRemainingS  int       `json:"cooldown_remaining_seconds"`
	BreakerOpenUntil    time.Time `json:"breaker_open_until,omitempty"`
	RemainingRequests   int64     `json:"remaining_requests"`
	RemainingTokens     int64     `json:"remaining_tokens"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	LastError           string    `json:"last_error,omitempty"`
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

	mu                  sync.RWMutex
	disabled            bool
	disabledReason      string
	cooldownUntil       time.Time
	breakerOpenUntil    time.Time
	remainingRequests   int64
	remainingTokens     int64
	consecutiveFailures int
	lastError           string
	lastUpdated         time.Time
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
		return false
	}
	if !b.cooldownUntil.IsZero() && now.Before(b.cooldownUntil) {
		return false
	}
	if !b.breakerOpenUntil.IsZero() && now.Before(b.breakerOpenUntil) {
		return false
	}
	return true
}

// State returns the current bucket state: "disabled", "breaker_open", "cooldown", or "ok".
func (b *Bucket) State(now time.Time) string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.disabled {
		return "disabled"
	}
	if !b.breakerOpenUntil.IsZero() && now.Before(b.breakerOpenUntil) {
		return "breaker_open"
	}
	if !b.cooldownUntil.IsZero() && now.Before(b.cooldownUntil) {
		return "cooldown"
	}
	return "ok"
}

// RecordSuccess updates stats on successful completion.
func (b *Bucket) RecordSuccess(remReq, remTok int64, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutiveFailures = 0
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

// RecordFailure records a connection failure or 5xx. 3 in a row opens circuit breaker for 60s.
func (b *Bucket) RecordFailure(errStr string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.consecutiveFailures++
	b.lastError = errStr
	b.lastUpdated = now

	if b.consecutiveFailures >= 3 {
		b.breakerOpenUntil = now.Add(60 * time.Second)
	}
}

// Disable marks the bucket permanently disabled with a reason.
func (b *Bucket) Disable(reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.disabled = true
	b.disabledReason = reason
	b.lastUpdated = time.Now()
}

// UpdateContextWindow updates the context window if learned from discovery.
func (b *Bucket) UpdateContextWindow(cw int) {
	if cw > 0 {
		b.ContextWindow = cw
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
		state = "disabled"
	} else if !b.breakerOpenUntil.IsZero() && now.Before(b.breakerOpenUntil) {
		state = "breaker_open"
	} else if !b.cooldownUntil.IsZero() && now.Before(b.cooldownUntil) {
		state = "cooldown"
	}

	return BucketSnapshot{
		Key:                 b.Key(),
		Provider:            b.Provider.Name,
		Model:               b.ModelID,
		Tier:                b.Tier,
		SupportsTools:       b.SupportsTools,
		SupportsVision:      b.SupportsVision,
		ContextWindow:       b.ContextWindow,
		TrainsOnData:        b.Provider.TrainsOnData,
		Local:               b.Provider.Local,
		HasKey:              b.HasKey(),
		State:               state,
		Disabled:            b.disabled,
		DisabledReason:      b.disabledReason,
		CooldownUntil:       b.cooldownUntil,
		CooldownRemainingS:  cooldownSecs,
		BreakerOpenUntil:    b.breakerOpenUntil,
		RemainingRequests:   b.remainingRequests,
		RemainingTokens:     b.remainingTokens,
		ConsecutiveFailures: b.consecutiveFailures,
		LastError:           b.lastError,
	}
}
