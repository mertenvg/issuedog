package agent

import (
	"bytes"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/logr/v2"
)

const gatePattern = "(?i)(usage|session) limit"

func newGate(t *testing.T) *Gate {
	t.Helper()
	g, err := NewGate(gatePattern)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var gateStart = time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)

func TestGateDetectsUsageLimits(t *testing.T) {
	if _, err := NewGate("(unclosed"); err == nil {
		t.Fatal("compiled an invalid pattern, want an error")
	}
	for _, tt := range []struct {
		failed bool
		text   string
		want   bool
	}{
		{true, "Claude AI usage limit reached", true},
		{true, "You've hit your session limit", true},
		{true, "API Error: 500 Internal Server Error", false},
		{true, "error_max_turns", false},
		// A successful run that mentions usage limits didn't hit one.
		{false, "Implemented the usage limit gate.", false},
	} {
		g := newGate(t)
		if got := g.Observe(gateStart, tt.failed, tt.text); got != tt.want {
			t.Errorf("Observe(failed=%v, %q) = %v, want %v", tt.failed, tt.text, got, tt.want)
		}
		if g.Open(gateStart) == tt.want {
			t.Errorf("%q: Open = %v right after, want %v", tt.text, g.Open(gateStart), !tt.want)
		}
	}
}

func TestGateResetTime(t *testing.T) {
	ams, err := time.LoadLocation("Europe/Amsterdam")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(mo time.Month, d, h, m, s int) time.Time { return time.Date(2026, mo, d, h, m, s, 0, time.UTC) }
	for _, tt := range []struct {
		now  time.Time // zero means gateStart
		text string
		want time.Time
	}{
		{time.Unix(1760180400, 0), "Claude AI usage limit reached|1760184000", time.Unix(1760184000, 0)},
		{time.Time{}, "Usage limit reached ∙ resets 3pm", utc(10, 11, 15, 0, 0)},
		{time.Time{}, "usage limit reached, resets at 3:30 PM", utc(10, 11, 15, 30, 0)},
		{time.Time{}, "usage limit · resets 15:00", utc(10, 11, 15, 0, 0)},
		{time.Time{}, "usage limit · resets 9am", utc(10, 12, 9, 0, 0)}, // passed today
		{time.Time{}, "usage limit · resets 12am", utc(10, 12, 0, 0, 0)},
		{time.Time{}, "usage limit · resets 3pm (Europe/Amsterdam)", time.Date(2026, 10, 11, 15, 0, 0, 0, ams)},
		{time.Time{}, "usage limit · resets Oct 14, 3pm (Europe/Amsterdam)", time.Date(2026, 10, 14, 15, 0, 0, 0, ams)},
		{time.Time{}, "usage limit · resets October 14 at 3:30pm (UTC)", utc(10, 14, 15, 30, 0)},
		{time.Time{}, "usage limit · resets Oct 13, 2026, 9am", utc(10, 13, 9, 0, 0)},
		// Tomorrow across a month end.
		{utc(10, 31, 22, 0, 0), "session limit · resets 1am (UTC)", utc(11, 1, 1, 0, 0)},
		// DST fall-back: at 1:40 EDT, 1:30am means the second 1:30, in EST.
		{utc(11, 1, 5, 40, 0), "session limit · resets 1:30am (America/New_York)", utc(11, 1, 6, 30, 0)},
		// A reset that passed seconds ago means the backoff, not tomorrow.
		{utc(10, 11, 15, 0, 5), "session limit · resets 3pm (UTC)", utc(10, 11, 15, 15, 5)},
		// The same just before midnight, and on the second 1am after DST ends.
		{utc(10, 12, 0, 1, 0), "session limit · resets 11:59pm (UTC)", utc(10, 12, 0, 16, 0)},
		{utc(11, 1, 6, 2, 0), "session limit · resets 1am (America/New_York)", utc(11, 1, 6, 17, 0)},
		// DST fall-back east of UTC: at 2:10 CEST, 2:30am means the first 2:30.
		{utc(10, 25, 0, 10, 0), "session limit · resets 2:30am (Europe/Amsterdam)", utc(10, 25, 0, 30, 0)},
	} {
		now := tt.now
		if now.IsZero() {
			now = gateStart
		}
		g := newGate(t)
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
	// Not parallel: it reads the process-wide logr output.
	for _, text := range []string{
		"usage limit reached|1000000000",        // in the past
		"usage limit reached|9999999999",        // too far ahead
		"usage limit · resets 13pm",             // not a clock time
		"usage limit · resets Oct 1, 3pm",       // next year: too far ahead
		"usage limit · resets 3pm (Not/AZone)",  // unknown zone
		"usage limit · resets Feb 30, 3pm",      // no such date
		"usage limit · resets Oct 9, 2026, 3pm", // a date that has passed
	} {
		var logs bytes.Buffer
		stop := logtest.CaptureTo(t, &logs, logr.All)
		g := newGate(t)
		g.Observe(gateStart, true, text)
		logr.Wait()
		stop()
		if want := gateStart.Add(15 * time.Minute); !g.OpenAt().Equal(want) || !strings.Contains(logs.String(), "can't be used") {
			t.Errorf("%q: opens at %v, logs %q; want the 15m backoff %v and why the reset was refused", text, g.OpenAt(), logs.String(), want)
		}
	}
}

func TestGateBackoffSequence(t *testing.T) {
	g := newGate(t)
	now := gateStart
	var got []time.Duration
	for range 5 {
		g.Observe(now, true, "usage limit reached")
		got = append(got, g.OpenAt().Sub(now))
		now = g.OpenAt()
	}
	if want := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 2 * time.Hour}; !slices.Equal(got, want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}

	// A run that doesn't hit the limit resets the backoff.
	g.Observe(now, false, "done")
	g.Observe(now, true, "usage limit reached")
	if wait := g.OpenAt().Sub(now); wait != 15*time.Minute {
		t.Errorf("wait after a success = %v, want 15m", wait)
	}
}

// TestGateStubLoop runs a stub daemon loop on a fake clock: while the gate is
// closed, steps 1 and 2 keep running, and usage limits don't count as attempts.
func TestGateStubLoop(t *testing.T) {
	g := newGate(t)
	now := gateStart
	step1, step2, stageRuns, attempts, closedPasses := 0, 0, 0, 0, 0
	outcomes := []string{"usage limit reached", "usage limit reached", "tests failed", "done"} // only "done" succeeds

	for tick := 0; tick < 200 && stageRuns < len(outcomes); tick++ {
		step1++ // process comments, reactions and labels
		step2++ // reconcile PRs
		if g.Open(now) {
			o := outcomes[stageRuns]
			stageRuns++
			if failed := o != "done"; !g.Observe(now, failed, o) && failed {
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
	// Waits of 15m and 30m at one pass a minute leave it closed for 14 + 29 passes.
	if closedPasses != 43 {
		t.Errorf("closed passes = %d, want 43", closedPasses)
	}
	if step1 != stageRuns+closedPasses || step2 != step1 {
		t.Errorf("steps 1 and 2 ran %d and %d times over %d passes, want every pass", step1, step2, stageRuns+closedPasses)
	}
}
