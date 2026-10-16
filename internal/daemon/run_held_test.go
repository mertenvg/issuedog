package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
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

// specApplied reports whether issue n is at spec-review with run's result.
func specApplied(f *githubtest.Fake, n int, run string, answer int64) bool {
	is := f.State().Issues[n-1]
	return slices.Contains(is.Labels, "stage:spec-review") &&
		slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return c.Body == specWritten(run, answer) })
}

// A label a rename puts on during a run holds the run's apply back, with
// its lease and result kept, uncounted, but for github.HoldBound only:
// then the result is applied with the label left out, with no attempt and
// no cap error, even at max_attempts 1. The label is removed later.
func TestHeldLabelDuringApplyWaitsBriefly(t *testing.T) {
	for _, max := range []int{1, 3} {
		t.Run(fmt.Sprintf("max_attempts %d", max), func(t *testing.T) {
			f, g, answer := heldRig(t, max)
			g.stage.during = func() { f.RenameLabel("foo", "priority:p0") }
			g.pass()
			for _, wait := range []time.Duration{0, 10 * time.Second, github.HoldBound - 10*time.Second - time.Second} {
				f.Advance(wait)
				if wait > 0 {
					g.pass()
				}
				if l := g.lease(); l.RunID != "run1" || l.Failed || l.ApplyFailures != 0 || !slices.Contains(g.leftovers(), "state/runs/run1.json") {
					t.Fatalf("%v into the hold: lease %+v, left %v; want run1's lease and result kept as they were", wait, l, g.leftovers())
				}
			}
			f.Advance(time.Second)
			g.pass()
			if !specApplied(f, 1, "run1", answer) || !slices.Contains(f.State().Issues[0].Labels, "priority:p0") {
				t.Fatalf("labels %q at the end of the hold; want run1 applied, priority:p0 left out but still on", f.State().Issues[0].Labels)
			}
			if g.runs() != 1 || len(errorMarkers(f, 1)) != 0 || len(g.leftovers()) != 0 {
				t.Errorf("%d runs, errors %q, left %v; want one run, no error, and nothing left", g.runs(), errorMarkers(f, 1), g.leftovers())
			}
			f.Advance(github.UnexplainedWait)
			g.pass()
			if slices.Contains(f.State().Issues[0].Labels, "priority:p0") {
				t.Errorf("labels %q after the wait; want priority:p0 removed", f.State().Issues[0].Labels)
			}
		})
	}
}

// An issue with a label held back starts no run, and uses no attempt, for
// github.HoldBound; then it runs with the label left out of its settings.
func TestHeldLabelStartsNoRunWhileHeld(t *testing.T) {
	f, g, answer := heldRig(t, 1)
	f.RenameLabel("foo", "effort:max")
	for _, wait := range []time.Duration{0, 10 * time.Second, github.HoldBound - 10*time.Second - time.Second} {
		f.Advance(wait)
		g.pass()
		if g.runs() != 0 || g.lease().RunID != "" {
			t.Fatalf("%v into the hold: %d runs, lease %+v; want no run", wait, g.runs(), g.lease())
		}
	}
	f.Advance(time.Second)
	g.pass()
	if g.runs() != 1 || !specApplied(f, 1, "run1", answer) || len(errorMarkers(f, 1)) != 0 {
		t.Fatalf("%d runs, labels %q, errors %q; want one run, applied, with no error", g.runs(), f.State().Issues[0].Labels, errorMarkers(f, 1))
	}
	var l daemon.Lease
	if err := json.Unmarshal([]byte(g.file("lease.1")), &l); err != nil {
		t.Fatal(err)
	}
	if l.Effort == "max" {
		t.Errorf("the run's lease %+v took its effort from the unexplained effort:max", l)
	}
}

// While one issue's lease waits on a held label, no other issue's run
// starts, as there's one lease, but for github.HoldBound only.
func TestHeldLeaseHoldsOtherRunsBriefly(t *testing.T) {
	f, g, answer := heldRig(t, 3)
	answer2 := interview(f) // issue 2
	g.stage.during = func() { f.RenameLabel("foo", "priority:p0") }
	g.pass() // issue 1's run, held back at its apply
	g.stage.during = nil
	f.Advance(github.HoldBound - time.Second)
	g.pass()
	if g.runs() != 1 || g.lease().RunID != "run1" {
		t.Fatalf("%d runs, lease %+v within the hold; want issue 2 to wait for run1's apply", g.runs(), g.lease())
	}
	f.Advance(time.Second)
	g.pass()
	if g.runs() != 2 || !specApplied(f, 1, "run1", answer) || !specApplied(f, 2, "run2", answer2) {
		t.Errorf("%d runs, labels %q and %q at the end of the hold; want run1 applied, then run2 on issue 2",
			g.runs(), f.State().Issues[0].Labels, f.State().Issues[1].Labels)
	}
}

// A label renamed to a new pipeline name every four minutes, while a run's
// lease waits on it, holds stage work up for github.HoldBound only: the
// result is applied, and the other issue runs.
func TestRenamedAgainAndAgainDoesNotStall(t *testing.T) {
	f, g, answer := heldRig(t, 3)
	answer2 := interview(f) // issue 2
	g.stage.during = func() { f.RenameLabel("foo", "scope:z0") }
	g.pass()
	g.stage.during = nil
	cur := "scope:z0"
	for minute := 1; minute <= 20; minute++ {
		if minute%4 == 0 {
			next := fmt.Sprintf("scope:z%d", minute)
			f.RenameLabel(cur, next)
			cur = next
		}
		f.Advance(time.Minute)
		g.pass()
		if minute == 1 && (g.runs() != 2 || !specApplied(f, 1, "run1", answer) || !specApplied(f, 2, "run2", answer2)) {
			t.Fatalf("a minute on: %d runs; want run1 applied and issue 2 run", g.runs())
		}
	}
	if !slices.Contains(f.State().Issues[0].Labels, cur) || len(errorMarkers(f, 1)) != 0 {
		t.Errorf("labels %q, errors %q; want %s left on, never removed, and no error", f.State().Issues[0].Labels, errorMarkers(f, 1), cur)
	}
}

// One label on every issue, renamed to a new pipeline name every four
// minutes, holds each issue back once, and Work gets every issue on every
// pass after that, without the label: no repo-wide stall. The label is
// removed in the end, as its issue has had unexplained labels for five
// minutes.
func TestRepoWideRenamesDoNotStall(t *testing.T) {
	f := newFake(t)
	for range 4 {
		n := fileAt(f, "ready", "do it", "priority:p2")
		f.AddLabel(n, "mallory", "foo")
	}
	f.Advance(time.Hour)
	d := newDaemon(t, f, newOrigin(t), nil)
	var got [][]daemon.Ready
	d.Work = func(_ context.Context, rs []daemon.Ready) error { got = append(got, rs); return nil }
	iterate(t, d)
	cur := "foo"
	for minute := range 20 {
		if minute%4 == 0 {
			next := fmt.Sprintf("scope:z%d", minute)
			f.RenameLabel(cur, next)
			cur = next
			if minute == 0 {
				for n := 1; n <= 4; n++ {
					f.AddComment(n, "eve", "poke") // no write access, so no discussion hold
				}
			}
		}
		f.Advance(time.Minute)
		iterate(t, d)
		rs := got[len(got)-1]
		if minute > 0 && len(rs) != 4 {
			t.Errorf("minute %d: Work got %d issues, want all 4", minute, len(rs))
		}
		for _, r := range rs {
			if slices.ContainsFunc(r.S.Labels, func(l string) bool { return strings.HasPrefix(l, "scope:") }) {
				t.Errorf("minute %d: Work got #%d with a renamed label: %q", minute, r.S.Number, r.S.Labels)
			}
		}
	}
	// Each issue had unexplained labels for five minutes, so its label, by
	// then a new name, is removed, with a revert comment.
	for _, is := range f.State().Issues {
		reverts := slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, "issuedog:revert key=scope") })
		if slices.ContainsFunc(is.Labels, func(l string) bool { return strings.HasPrefix(l, "scope:") }) || !reverts {
			t.Errorf("#%d: labels %q, a revert comment %v; want the renamed label removed, with one", is.Number, is.Labels, reverts)
		}
	}
}

// A type label or a stage label left out is acted on nowhere: Work gets the
// issue with no type from it, and a stage label left out beside the one the
// marker names gets no repair, nor a state error, until it's removed.
func TestLeftOutLabelsAreNotActedOn(t *testing.T) {
	for name, to := range map[string]string{"type": "type:epic", "stage": "stage:done"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			fileAt(f, "ready", "do it", "priority:p2")
			f.AddLabel(1, "mallory", "foo")
			f.Advance(time.Hour)
			d := newDaemon(t, f, newOrigin(t), nil)
			var got []*github.Snapshot
			d.Work = func(_ context.Context, rs []daemon.Ready) error {
				for _, r := range rs {
					got = append(got, r.S)
				}
				return nil
			}
			iterate(t, d)
			f.RenameLabel("foo", to)
			f.AddComment(1, "eve", "poke") // no write access, so no discussion hold
			for range 5 {
				f.Advance(time.Minute)
				iterate(t, d)
			}
			s := got[len(got)-1]
			if slices.Contains(s.Labels, to) || s.Type != "" {
				t.Errorf("Work got labels %q, type %q; want %s left out, and no type", s.Labels, s.Type, to)
			}
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes = %+v before the wait is up; want none", w)
			}
			f.Advance(time.Minute)
			iterate(t, d)
			var paths []string
			for _, w := range f.Writes() {
				paths = append(paths, w.Method+" "+w.Path)
			}
			if want := []string{"POST /issues/1/comments", "DELETE /issues/1/labels/" + to}; !slices.Equal(paths, want) {
				t.Errorf("writes = %q after the wait; want only the removal, %q", paths, want)
			}
		})
	}
}

// A closed issue's unexplained labels are forgotten, as are those of an
// issue gone to another repo, so when it's back with the label still on,
// the label waits the whole time again.
func TestClosedIssueForgetsUnexplained(t *testing.T) {
	for name, tt := range map[string]struct{ away, back func(f *githubtest.Fake) }{
		// The owner's close and reopen stand (T6): the close's closed stage
		// is written while it's away, and the hold for the reopen only once
		// the label rules hold back nothing.
		"closed": {func(f *githubtest.Fake) { f.CloseBy(1, "owner") }, func(f *githubtest.Fake) { f.ReopenBy(1, "owner") }},
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
			from := len(f.Writes())
			iterate(t, d)
			if !slices.Contains(f.State().Issues[0].Labels, "priority:p0") || len(f.Writes()) != from {
				t.Fatalf("labels %q, writes %+v once it's back; want priority:p0 kept for now, nothing written", f.State().Issues[0].Labels, f.Writes()[from:])
			}
			f.Advance(github.UnexplainedWait)
			iterate(t, d)
			if slices.Contains(f.State().Issues[0].Labels, "priority:p0") {
				t.Errorf("labels %q five minutes after it's back; want priority:p0 removed", f.State().Issues[0].Labels)
			}
		})
	}
}
