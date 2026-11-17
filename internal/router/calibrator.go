package router

import (
	"math"
	"sync"
)

// TokenCalibrator maintains an exponentially weighted moving average (EWMA) of
// actual_prompt_tokens / estimated_prompt_tokens per provider.
type TokenCalibrator struct {
	mu     sync.RWMutex
	ratios map[string]float64
}

// NewTokenCalibrator creates an initialized TokenCalibrator.
func NewTokenCalibrator() *TokenCalibrator {
	return &TokenCalibrator{
		ratios: make(map[string]float64),
	}
}

// Ratio returns the current calibration ratio for a provider (default 1.0).
func (c *TokenCalibrator) Ratio(provider string) float64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.ratios[provider]
	if !ok || r <= 0 {
		return 1.0
	}
	return r
}

// RecordUsage updates the provider ratio using response usage.prompt_tokens.
func (c *TokenCalibrator) RecordUsage(provider string, estimated, actual int) {
	if estimated <= 0 || actual <= 0 {
		return
	}
	sample := float64(actual) / float64(estimated)
	// Sane boundaries to prevent anomalous outliers
	if sample < 0.2 {
		sample = 0.2
	} else if sample > 4.0 {
		sample = 4.0
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.ratios[provider]
	if !ok || old <= 0 {
		c.ratios[provider] = sample
	} else {
		// EWMA with alpha = 0.2
		c.ratios[provider] = 0.8*old + 0.2*sample
	}
}

// Calibrate returns the calibrated prompt token estimate for a provider.
func (c *TokenCalibrator) Calibrate(provider string, estimated int) int {
	if estimated <= 0 {
		return estimated
	}
	ratio := c.Ratio(provider)
	calibrated := int(math.Round(float64(estimated) * ratio))
	if calibrated <= 0 {
		return 1
	}
	return calibrated
}
