package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// heldRig files the interview, issue 1, with mallory's unguarded foo, and
// a rig on it whose claude succeeds, with max_attempts as given.
func heldRig(t *testing.T, maxAttempts int) (*githubtest.Fake, *rig, int64) {
	f := newFake(t)
	answer := interview(f)
	f.AddLabel(1, "mallory", "foo") // unguarded, so it stands
	f.Advance(time.Hour)
	o := newOrigin(t)
	o.commit(config.Path, fmt.Sprintf("types: labels\nloop:\n  max_attempts: %d\n", maxAttempts))
	g := newRig(t, f, o, t.TempDir())
	g.claude(okOut, 0)
	return f, g, answer
}

// pass runs one Iterate on g, which must succeed.
func (g *rig) pass() {
	g.t.Helper()
	if err := g.d.Iterate(context.Background()); err != nil {
		g.t.Fatalf("Iterate: %v", err)
	}
}

// A label a rename puts on during a run holds the run's apply back, with
// its lease and result kept, for as long as the label is held back: no
// failed apply, no attempt and no cap error, even at max_attempts 1. Once
// the label is removed, the result is applied.
func TestHeldLabelDuringApplyWaits(t *testing.T) {
	for _, max := range []int{1, 3} {
		t.Run(fmt.Sprintf("max_attempts %d", max), func(t *testing.T) {
			f, g, answer := heldRig(t, max)
			g.stage.during = func() { f.RenameLabel("foo", "priority:p0") }
			g.pass()
			for i := range 6 {
				on := slices.Contains(f.State().Issues[0].Labels, "priority:p0")
				l := g.lease()
				switch {
				case on && (l.RunID != "run1" || l.Failed || l.ApplyFailures != 0):
					t.Fatalf("pass %d: lease %+v while priority:p0 is held back; want run1's, kept as it was", i, l)
				case on && !slices.Contains(g.leftovers(), "state/runs/run1.json"):
					t.Fatalf("pass %d: left %v; want run1's result kept", i, g.leftovers())
				}
				if g.runs() != 1 || len(errorMarkers(f, 1)) != 0 {
					t.Fatalf("pass %d: %d runs, errors %q; want one run and no error", i, g.runs(), errorMarkers(f, 1))
				}
				f.Advance(2 * time.Minute)
				g.pass()
			}
			is := f.State().Issues[0]
			if !slices.Equal(is.Labels, []string{"stage:spec-review"}) || !slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool {
				return c.Body == specWritten("run1", answer)
			}) {
				t.Errorf("labels %q; want priority:p0 removed and run1's result applied, to spec-review", is.Labels)
			}
			if g.runs() != 1 || len(errorMarkers(f, 1)) != 0 || len(g.leftovers()) != 0 {
				t.Errorf("%d runs, errors %q, left %v; want one run, no error, and nothing left", g.runs(), errorMarkers(f, 1), g.leftovers())
			}
		})
	}
}

// An issue with a label held back starts no run, and uses no attempt,
// until the label is removed.
func TestHeldLabelStartsNoRun(t *testing.T) {
	f, g, answer := heldRig(t, 1)
	f.RenameLabel("foo", "priority:p0")
	for i := range 3 {
		if i > 0 {
			f.Advance(2 * time.Minute)
		}
		g.pass()
		if g.runs() != 0 || g.lease().RunID != "" {
			t.Fatalf("pass %d: %d runs, lease %+v; want no run while priority:p0 is held back", i, g.runs(), g.lease())
		}
	}
	f.Advance(2 * time.Minute)
	g.pass()
	is := f.State().Issues[0]
	if g.runs() != 1 || !slices.Equal(is.Labels, []string{"stage:spec-review"}) || len(errorMarkers(f, 1)) != 0 {
		t.Errorf("%d runs, labels %q, errors %q; want priority:p0 removed, then one run, applied", g.runs(), is.Labels, errorMarkers(f, 1))
	}
	if last := is.Comments[len(is.Comments)-1].Body; last != specWritten("run1", answer) {
		t.Errorf("newest comment %q; want run1's result", last)
	}
}

// While one issue's lease waits on a held label, no other issue's run
// starts, since there's one lease; once it's applied, the next runs.
func TestHeldLeaseHoldsOtherRuns(t *testing.T) {
	f, g, _ := heldRig(t, 3)
	interview(f) // issue 2
	g.stage.during = func() { f.RenameLabel("foo", "priority:p0") }
	g.pass() // issue 1's run, held back at its apply
	g.stage.during = nil
	for i := range 3 {
		f.Advance(time.Minute)
		g.pass()
		if g.runs() != 1 || g.lease().RunID != "run1" {
			t.Fatalf("pass %d: %d runs, lease %+v; want issue 2 to wait for run1's apply", i, g.runs(), g.lease())
		}
	}
	for range 4 {
		f.Advance(time.Minute)
		g.pass()
	}
	if g.runs() != 2 || !slices.Equal(f.State().Issues[0].Labels, []string{"stage:spec-review"}) ||
		!slices.Equal(f.State().Issues[1].Labels, []string{"stage:spec-review"}) {
		t.Errorf("%d runs, labels %q and %q; want both runs applied", g.runs(), f.State().Issues[0].Labels, f.State().Issues[1].Labels)
	}
}

// A closed issue's unexplained labels are forgotten, as are those of an
// issue gone to another repo, so when it's back with the label still on,
// the label waits the whole time again.
func TestClosedIssueForgetsUnexplained(t *testing.T) {
	for name, tt := range map[string]struct{ away, back func(f *githubtest.Fake) }{
		"closed": {func(f *githubtest.Fake) { f.Close(1) }, func(f *githubtest.Fake) { f.Reopen(1) }},
		"transferred": {func(f *githubtest.Fake) { f.Transfer(1, "x/y") }, func(f *githubtest.Fake) {
			f.Transfer(1, "o/r")
			f.AddComment(1, "owner", "it's back")
		}},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "ready", "do it", "priority:p2")
			f.AddLabel(1, "mallory", "foo")
			f.Advance(time.Hour)
			d := newDaemon(t, f, newOrigin(t), nil)
			iterate(t, d)
			f.RenameLabel("foo", "priority:p0")
			f.AddComment(1, "mallory", "so it's read again")
			iterate(t, d) // first held back
			tt.away(f)
			iterate(t, d)
			iterate(t, d) // a gone issue is dropped on its second read
			f.Advance(github.UnexplainedWait + time.Minute)
			tt.back(f)
			iterate(t, d)
			if !slices.Contains(f.State().Issues[0].Labels, "priority:p0") || len(f.Writes()) != 0 {
				t.Fatalf("labels %q, writes %+v once it's back; want priority:p0 held back again, nothing written", f.State().Issues[0].Labels, f.Writes())
			}
			f.Advance(github.UnexplainedWait)
			iterate(t, d)
			if slices.Contains(f.State().Issues[0].Labels, "priority:p0") {
				t.Errorf("labels %q five minutes after it's back; want priority:p0 removed", f.State().Issues[0].Labels)
			}
		})
	}
}
