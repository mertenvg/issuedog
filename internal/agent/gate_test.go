package agent

import (
	"testing"
	"time"
)

const pattern = "(?i)(usage|session) limit"

func newGate(t *testing.T) *Gate {
	t.Helper()
	g, err := NewGate(pattern)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var start = time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)

func TestGateDetectsUsageLimits(t *testing.T) {
	for _, tt := range []struct {
		failed bool
		text   string
		want   bool
	}{
		{true, "Claude AI usage limit reached", true},
		{true, "You've hit your session limit", true},
		{true, "API Error: 500 Internal Server Error", false},
		{true, "error_max_turns", false},
		// A successful run may mention usage limits, for example while
		// working on this code. That is not a usage limit.
		{false, "Implemented the usage limit gate.", false},
	} {
		g := newGate(t)
		if got := g.Observe(start, tt.failed, tt.text); got != tt.want {
			t.Errorf("Observe(failed=%v, %q) = %v, want %v", tt.failed, tt.text, got, tt.want)
		}
		if g.Open(start) == tt.want {
			t.Errorf("%q: Open = %v right after, want %v", tt.text, g.Open(start), !tt.want)
		}
	}
}

func TestGateInvalidPattern(t *testing.T) {
	if _, err := NewGate("(unclosed"); err == nil {
		t.Fatal("compiled an invalid pattern, want an error")
	}
}

func TestGateResetTime(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		text string
		want time.Time
	}{
		{"Claude AI usage limit reached|1760184000", time.Unix(1760184000, 0)},
		{"Usage limit reached ∙ resets 3pm", time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)},
		{"usage limit reached, resets at 3:30 PM", time.Date(2026, 10, 11, 15, 30, 0, 0, time.UTC)},
		{"usage limit · resets 15:00", time.Date(2026, 10, 11, 15, 0, 0, 0, time.UTC)},
		// 9am has already passed today, so it means tomorrow.
		{"usage limit · resets 9am", time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)},
		{"usage limit · resets 12am", time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)},
		{"usage limit · resets 3pm (Europe/Amsterdam)", time.Date(2026, 10, 11, 15, 0, 0, 0, ams)},
	} {
		g := newGate(t)
		now := start
		if tt.text == "Claude AI usage limit reached|1760184000" {
			now = time.Unix(1760184000, 0).Add(-time.Hour)
		}
		if !g.Observe(now, true, tt.text) {
			t.Errorf("%q: not detected", tt.text)
			continue
		}
		if !g.OpenAt().Equal(tt.want) {
			t.Errorf("%q: opens at %v, want %v", tt.text, g.OpenAt(), tt.want)
		}
		if g.Open(tt.want.Add(-time.Second)) || !g.Open(tt.want) {
			t.Errorf("%q: gate doesn't open exactly at the reset time", tt.text)
		}
	}
}

func TestGateUnreadableResetFallsBackToBackoff(t *testing.T) {
	for _, text := range []string{
		"usage limit reached|1000000000",          // in the past
		"usage limit reached|9999999999",          // too far ahead
		"usage limit · resets 13pm",               // not a clock time
		"usage limit · resets 3pm (Not/AZone)",    // unknown zone
		"usage limit · resets Oct 14 at 3pm here", // date formats aren't read
	} {
		g := newGate(t)
		g.Observe(start, true, text)
		if want := start.Add(15 * time.Minute); !g.OpenAt().Equal(want) {
			t.Errorf("%q: opens at %v, want the 15m backoff %v", text, g.OpenAt(), want)
		}
	}
}

func TestGateBackoffSequence(t *testing.T) {
	g := newGate(t)
	now := start
	var got []time.Duration
	for range 5 {
		g.Observe(now, true, "usage limit reached")
		got = append(got, g.OpenAt().Sub(now))
		now = g.OpenAt()
	}
	want := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("waits = %v, want %v", got, want)
		}
	}

	// A run that doesn't hit the limit resets the backoff.
	g.Observe(now, false, "done")
	g.Observe(now, true, "usage limit reached")
	if wait := g.OpenAt().Sub(now); wait != 15*time.Minute {
		t.Errorf("wait after a success = %v, want 15m", wait)
	}
}

// TestGateStubLoop runs a stub of the daemon loop with a fake clock. While the
// gate is closed, steps 1 and 2 keep running and no stage work starts.
// Usage-limit failures don't count as attempts.
func TestGateStubLoop(t *testing.T) {
	g := newGate(t)
	now := start
	step1, step2, stageRuns, attempts, closedPasses := 0, 0, 0, 0, 0
	outcomes := []struct {
		failed bool
		text   string
	}{
		{true, "usage limit reached"},
		{true, "usage limit reached"},
		{true, "tests failed"},
		{false, "done"},
	}

	for tick := 0; tick < 200 && stageRuns < len(outcomes); tick++ {
		step1++ // process comments, reactions and labels
		step2++ // reconcile PRs
		if g.Open(now) {
			o := outcomes[stageRuns]
			stageRuns++
			if !g.Observe(now, o.failed, o.text) && o.failed {
				attempts++
			}
		} else {
			closedPasses++
		}
		now = now.Add(time.Minute)
	}

	if stageRuns != len(outcomes) {
		t.Fatalf("stage runs = %d, want %d", stageRuns, len(outcomes))
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 (only the test failure counts)", attempts)
	}
	// Waits of 15m and 30m with a one-minute pass leave the gate closed for
	// 14 + 29 passes, and steps 1 and 2 ran on every one of them.
	if closedPasses != 43 {
		t.Errorf("closed passes = %d, want 43", closedPasses)
	}
	if step1 != stageRuns+closedPasses || step2 != step1 {
		t.Errorf("steps 1 and 2 ran %d and %d times over %d passes, want every pass", step1, step2, stageRuns+closedPasses)
	}
}
