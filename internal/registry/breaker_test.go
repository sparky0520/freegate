package registry

import (
	"testing"
	"time"

	"gateway/internal/config"
)

func TestCircuitBreakerExponentialBackoffAndHalfOpen(t *testing.T) {
	b := newTestBucket()
	now := time.Now()

	// Initial state
	if b.State(now) != "ok" {
		t.Fatalf("expected state ok, got %s", b.State(now))
	}

	// 1st failure
	b.RecordFailure("err 1", now)
	if b.State(now) != "ok" {
		t.Fatalf("expected ok after 1 failure, got %s", b.State(now))
	}

	// 2nd failure
	b.RecordFailure("err 2", now)
	if b.State(now) != "ok" {
		t.Fatalf("expected ok after 2 failures, got %s", b.State(now))
	}

	// 3rd failure: trips breaker to breaker_open for 60s
	b.RecordFailure("err 3", now)
	if b.State(now) != "breaker_open" {
		t.Fatalf("expected breaker_open after 3 failures, got %s", b.State(now))
	}
	snap := b.Snapshot(now)
	if snap.BreakerOpenUntil.Sub(now) != 60*time.Second {
		t.Fatalf("expected initial backoff 60s, got %v", snap.BreakerOpenUntil.Sub(now))
	}

	// At 30s: still breaker_open and not available
	t30 := now.Add(30 * time.Second)
	if b.IsAvailable(t30) {
		t.Fatal("bucket should not be available during breaker_open")
	}
	if b.TryAcquireProbe(t30) {
		t.Fatal("should not acquire probe during breaker_open")
	}

	// At 61s: breaker has entered half_open
	t61 := now.Add(61 * time.Second)
	if b.State(t61) != "half_open" {
		t.Fatalf("expected half_open at 61s, got %s", b.State(t61))
	}
	if !b.IsAvailable(t61) {
		t.Fatal("bucket should be available for single probe in half_open")
	}

	// Probe acquisition: only ONE probe allowed through
	if !b.TryAcquireProbe(t61) {
		t.Fatal("expected to acquire probe in half_open")
	}
	// Second concurrent attempt must be rejected
	if b.TryAcquireProbe(t61) {
		t.Fatal("second concurrent probe must be rejected")
	}
	if b.IsAvailable(t61) {
		t.Fatal("bucket should not be available while probe is in flight")
	}

	// If probe fails: immediately trips breaker again with backoff = 120s!
	b.RecordFailure("probe failed", t61)
	if b.State(t61) != "breaker_open" {
		t.Fatalf("expected breaker_open after probe failed, got %s", b.State(t61))
	}
	snap = b.Snapshot(t61)
	if snap.BreakerOpenUntil.Sub(t61) != 120*time.Second {
		t.Fatalf("expected second backoff 120s, got %v", snap.BreakerOpenUntil.Sub(t61))
	}

	// At t61 + 121s: half_open again
	t182 := t61.Add(121 * time.Second)
	if b.State(t182) != "half_open" {
		t.Fatalf("expected half_open at 182s, got %s", b.State(t182))
	}
	if !b.TryAcquireProbe(t182) {
		t.Fatal("expected to acquire probe at 182s")
	}

	// This time probe succeeds!
	b.RecordSuccess(10, 1000, t182)
	if b.State(t182) != "ok" {
		t.Fatalf("expected state ok after successful probe, got %s", b.State(t182))
	}
	snap = b.Snapshot(t182)
	if snap.ConsecutiveFailures != 0 {
		t.Fatalf("expected consecutiveFailures reset to 0, got %d", snap.ConsecutiveFailures)
	}
}

func TestTimedDisableForModelNotFound(t *testing.T) {
	b := newTestBucket()
	now := time.Now()

	// Disable until 30 minutes from now
	until := now.Add(30 * time.Minute)
	b.DisableUntil(until, "model not found 404")

	if b.State(now) != "disabled" {
		t.Fatalf("expected state disabled, got %s", b.State(now))
	}
	if b.IsAvailable(now) {
		t.Fatal("bucket should not be available while disabled")
	}

	// After 30 minutes + 1 second, bucket automatically recovers
	future := now.Add(30*time.Minute + time.Second)
	if b.State(future) != "ok" {
		t.Fatalf("expected state ok after timed disable expiration, got %s", b.State(future))
	}
	if !b.IsAvailable(future) {
		t.Fatal("bucket should become available after timed disable expiration")
	}
}

func TestLocalRateLimiterSlidingWindow(t *testing.T) {
	p := &config.ProviderConfig{Name: "gemini", Local: true}
	m := &config.ModelMetadata{
		Provider:      "gemini",
		ID:            "gemini-flash-latest",
		Tier:          3,
		ContextWindow: 1048576,
		RPM:           2,
		TPM:           100,
		RPD:           5,
	}
	b := NewBucket(p, m)
	now := time.Now()

	// Initially all quota available
	remReq, remTok, remDaily, _, _, _ := b.LocalQuotaLocked(now)
	if remReq != 2 || remTok != 100 || remDaily != 5 {
		t.Fatalf("unexpected initial quota: req=%d tok=%d daily=%d", remReq, remTok, remDaily)
	}

	// Record 1 request with 60 tokens
	b.RecordLocalUsage(60, now)
	remReq, remTok, _, _, _, _ = b.LocalQuotaLocked(now)
	if remReq != 1 || remTok != 40 {
		t.Fatalf("after 1 req: req=%d tok=%d", remReq, remTok)
	}

	// Record 2nd request with 40 tokens -> RPM and TPM exhausted
	b.RecordLocalUsage(40, now.Add(5*time.Second))
	remReq, remTok, _, waitReq, waitTok, _ := b.LocalQuotaLocked(now.Add(10*time.Second))
	if remReq != 0 || remTok != 0 {
		t.Fatalf("exhausted: req=%d tok=%d", remReq, remTok)
	}
	if waitReq <= 0 || waitTok <= 0 {
		t.Fatalf("expected positive wait times, got waitReq=%v waitTok=%v", waitReq, waitTok)
	}

	// After 1 minute from first request, first request slides out
	t65 := now.Add(65 * time.Second)
	remReq, remTok, _, _, _, _ = b.LocalQuotaLocked(t65)
	if remReq != 1 || remTok != 60 {
		t.Fatalf("after 65s: expected req=1 tok=60, got req=%d tok=%d", remReq, remTok)
	}
}
