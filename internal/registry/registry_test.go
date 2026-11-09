package registry

import (
	"testing"
	"time"

	"gateway/internal/config"
)

func TestBucketStateMachine(t *testing.T) {
	p := &config.ProviderConfig{
		Name:      "test-p",
		APIKeyEnv: "TEST_KEY",
	}
	m := &config.ModelMetadata{
		Provider:      "test-p",
		ID:            "model-1",
		Tier:          2,
		ContextWindow: 4096,
	}

	b := NewBucket(p, m)
	now := time.Now()

	// Initial state
	if !b.IsAvailable(now) {
		t.Fatal("expected newly created bucket to be available")
	}
	if b.State(now) != "ok" {
		t.Fatalf("expected state ok, got %s", b.State(now))
	}

	// 1 failure - still available
	b.RecordFailure("500 internal error", now)
	if !b.IsAvailable(now) {
		t.Fatal("expected bucket to remain available after 1 failure")
	}

	// 2nd failure - still available
	b.RecordFailure("500 internal error", now)
	if !b.IsAvailable(now) {
		t.Fatal("expected bucket to remain available after 2 failures")
	}

	// 3rd failure - breaker trips
	b.RecordFailure("500 internal error", now)
	if b.IsAvailable(now) {
		t.Fatal("expected circuit breaker to be open after 3 consecutive failures")
	}
	if b.State(now) != "breaker_open" {
		t.Fatalf("expected state breaker_open, got %s", b.State(now))
	}

	// After 61 seconds, breaker resets
	future := now.Add(61 * time.Second)
	if !b.IsAvailable(future) {
		t.Fatal("expected bucket to become available after breaker timeout")
	}

	// Cooldown test
	b.RecordCooldown(future.Add(30*time.Second), "rate limit 429", future)
	if b.IsAvailable(future) {
		t.Fatal("expected bucket in cooldown to not be available")
	}
	if b.State(future) != "cooldown" {
		t.Fatalf("expected state cooldown, got %s", b.State(future))
	}

	// Success resets failures and cooldown
	afterCooldown := future.Add(31 * time.Second)
	b.RecordSuccess(100, 50000, afterCooldown)
	if !b.IsAvailable(afterCooldown) {
		t.Fatal("expected bucket to be available after cooldown and success")
	}
	snap := b.Snapshot(afterCooldown)
	if snap.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures after success, got %d", snap.ConsecutiveFailures)
	}
	if snap.RemainingRequests != 100 {
		t.Errorf("expected 100 remaining requests, got %d", snap.RemainingRequests)
	}

	// Permanent disable
	b.Disable("invalid api key")
	if b.IsAvailable(afterCooldown.Add(time.Second)) {
		t.Fatal("expected disabled bucket to not be available")
	}
	if b.State(afterCooldown.Add(time.Second)) != "disabled" {
		t.Fatalf("expected state disabled, got %s", b.State(afterCooldown.Add(time.Second)))
	}
}

func TestRegistryAliases(t *testing.T) {
	cfg := &config.Config{
		Providers: []config.ProviderConfig{
			{Name: "p1", BaseURL: "http://p1"},
			{Name: "p2", BaseURL: "http://p2", Local: true},
		},
	}
	metadata := []config.ModelMetadata{
		{Provider: "p1", ID: "small", Tier: 1},
		{Provider: "p1", ID: "large", Tier: 3},
		{Provider: "p2", ID: "local-mod", Tier: 2},
	}

	reg := New(cfg, metadata)

	if len(reg.AllBuckets()) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(reg.AllBuckets()))
	}

	auto, ok := reg.GetAlias("auto")
	if !ok || len(auto) != 3 {
		t.Fatalf("expected 3 models in auto alias, got %v", auto)
	}

	fast, ok := reg.GetAlias("fast")
	if !ok || len(fast) != 2 {
		t.Fatalf("expected 2 models in fast alias, got %v", fast)
	}

	local, ok := reg.GetAlias("local")
	if !ok || len(local) != 1 || local[0] != "p2/local-mod" {
		t.Fatalf("expected local alias to have p2/local-mod, got %v", local)
	}

	// Test DisableProvider
	reg.DisableProvider("p1", "auth failed")
	b := reg.GetBucket("p1/small")
	if b == nil || !b.Snapshot(time.Now()).Disabled {
		t.Error("expected p1/small to be disabled")
	}
}
