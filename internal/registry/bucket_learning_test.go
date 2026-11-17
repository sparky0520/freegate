package registry

import (
	"testing"
	"time"

	"gateway/internal/config"
)

func newTestBucket() *Bucket {
	return NewBucket(&config.ProviderConfig{Name: "p", Local: true}, &config.ModelMetadata{Provider: "p", ID: "m", Tier: 2, ContextWindow: 131072})
}

func TestHardCapKeepsSmallestAndNeverExpires(t *testing.T) {
	b := newTestBucket()
	now := time.Now()
	if got := b.Snapshot(now).MaxPromptTokens; got != 0 {
		t.Fatalf("new bucket should have no ceiling, got %d", got)
	}
	b.RecordHardCap(8000)
	b.RecordHardCap(12000) // larger: ignored
	b.RecordHardCap(0)     // invalid: ignored
	b.RecordHardCap(6000)
	if got := b.Snapshot(now.Add(24 * time.Hour)).MaxPromptTokens; got != 6000 {
		t.Fatalf("expected smallest hard cap 6000 to persist, got %d", got)
	}
}

func TestSoftCapExpires(t *testing.T) {
	b := newTestBucket()
	now := time.Now()
	b.RecordSoftCap(5000, now, now.Add(time.Minute))
	if got := b.Snapshot(now.Add(30 * time.Second)).MaxPromptTokens; got != 5000 {
		t.Fatalf("soft cap should apply inside its TTL, got %d", got)
	}
	if got := b.Snapshot(now.Add(61 * time.Second)).MaxPromptTokens; got != 0 {
		t.Fatalf("soft cap should expire, got %d", got)
	}
	// A hard cap outlives, and combines (min) with, a soft cap.
	b.RecordHardCap(9000)
	b.RecordSoftCap(4000, now, now.Add(time.Minute))
	if got := b.Snapshot(now.Add(10 * time.Second)).MaxPromptTokens; got != 4000 {
		t.Fatalf("smaller active soft cap should win, got %d", got)
	}
	if got := b.Snapshot(now.Add(2 * time.Minute)).MaxPromptTokens; got != 9000 {
		t.Fatalf("hard cap should remain after the soft cap expires, got %d", got)
	}
}

// A stale low token count must never lock a bucket out: it is only reported
// while the provider's window is live.
func TestBudgetTokensExpireWithWindow(t *testing.T) {
	b := newTestBucket()
	now := time.Now()

	if got := b.Snapshot(now).BudgetTokens; got != -1 {
		t.Fatalf("no window recorded: budget should be unknown (-1), got %d", got)
	}

	b.RecordSuccess(5, 120, now)
	b.RecordBudgetWindow(20*time.Second, now)
	if s := b.Snapshot(now.Add(10 * time.Second)); s.BudgetTokens != 120 || s.BudgetResetAt.IsZero() {
		t.Fatalf("budget should be live inside the window, got %+v", s)
	}
	if got := b.Snapshot(now.Add(21 * time.Second)).BudgetTokens; got != -1 {
		t.Fatalf("budget should expire with its window, got %d", got)
	}

	// Non-positive reset defaults to a 60s window.
	b.RecordBudgetWindow(0, now)
	if got := b.Snapshot(now.Add(59 * time.Second)).BudgetTokens; got != 120 {
		t.Fatalf("default window should be ~60s, got %d at 59s", got)
	}
	if got := b.Snapshot(now.Add(61 * time.Second)).BudgetTokens; got != -1 {
		t.Fatalf("default window should end by 61s, got %d", got)
	}
}
