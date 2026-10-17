package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// always is a Stage.Pending that's always true.
func always(daemon.Input) bool { return true }

// newestComment is the body of issue n's newest comment.
func newestComment(f *githubtest.Fake, n int) string {
	cs := f.State().Issues[n-1].Comments
	return cs[len(cs)-1].Body
}

// A failure to end a run whose move the table lacks is returned, not lost
// with the state error.
func TestBlockedRunCleanupFailureIsReturned(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	f.SetType(1, "Weird")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	if err := os.Mkdir(g.path("state", "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The lease can't be removed once the run has started.
	g.stage.during = func() {
		if err := os.Chmod(g.path("state"), 0o500); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() {
		if err := os.Chmod(g.path("state"), 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := g.work(1); err == nil || daemon.StateError(err) || !strings.Contains(err.Error(), "lease") {
		t.Errorf("Work = %v, want the error removing the lease", err)
	}
	if errs := errorMarkers(f, 1); len(errs) != 1 || !strings.Contains(errs[0], "kind=transition") {
		t.Errorf("errors %q, want the one transition error", errs)
	}
}

// A run whose result can't be saved has run claude, so it's an attempt.
func TestSaveFailureIsAnAttempt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = always
	g.claude(okOut, 0)
	if err := os.Mkdir(g.path("state", "runs"), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(g.path("state", "runs"), 0o700); err != nil {
			t.Error(err)
		}
	})
	for range 4 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	if errs := errorMarkers(f, 1); g.runs() != 1 || len(errs) != 1 || errs[0] != capError(answer) {
		t.Errorf("%d runs, errors %q; want one run, then the cap", g.runs(), errs)
	}
}

// When posting the cap's state error fails, the next pass posts it and
// runs nothing.
func TestCapPostRetriedWithoutARun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 2
	g.stage.pending = always
	g.claude(failOut, 1)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	f.AnswerOnce(func(r githubtest.Request) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/comments")
	}, http.StatusBadGateway)
	if err := g.work(1); err == nil {
		t.Fatal("Work = nil, want the failed post")
	}
	for range 3 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	if errs := errorMarkers(f, 1); g.runs() != 2 || len(errs) != 1 || errs[0] != capError(answer) {
		t.Errorf("%d runs, errors %q; want 2 runs, then the cap posted once", g.runs(), errs)
	}
}

// The cap's state error is posted on the issue as it is after the run:
// none on one held, by label or marker, closed or gone meanwhile.
func TestCapSkipsAnIssueChangedDuringTheRun(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(f *githubtest.Fake){
		"held by label": func(f *githubtest.Fake) { f.AddLabel(1, "owner", "stage:held") },
		"held by marker": func(f *githubtest.Fake) {
			f.AddComment(1, app, "<!-- issuedog:transition from=clarifying to=held event=9 -->")
		},
		"closed": func(f *githubtest.Fake) { f.Close(1) },
		"gone":   func(f *githubtest.Fake) { f.DeleteIssue() },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.d.Config.Loop.MaxAttempts = 1
			g.stage.pending = always
			g.stage.during = func() { change(f) }
			g.claude(failOut, 1)
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			if name == "gone" {
				return // nothing to post on, and no error
			}
			if errs := errorMarkers(f, 1); len(errs) != 0 {
				t.Errorf("errors %q, want none on an issue %s during the run", errs, name)
			}
		})
	}
}

// Replay over a resumed result whose move the table lacks: the one state
// error is posted once, keyed by the move, never by the run.
func TestBlockedResumeReplay(t *testing.T) {
	t.Parallel()
	replayRuns(t, func(t testing.TB, f *githubtest.Fake, root string) {
		saveRun(t, root, "saved", okOut, interview(f))
		f.SetType(1, "Weird")
	}, func(g *rig) { g.stage.pending = always })
}

// A Stage whose Apply returns a state error without Result.Move's key is
// blocked the same way: one state error, and one run.
func TestBlockedWithoutMoveKey(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.pending = always
	g.stage.applyErr = fmt.Errorf("no row: %w", state.ErrNoTransition)
	g.claude(okOut, 0)
	for range 4 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	key := fmt.Sprintf("<!-- issuedog:error kind=transition key=clarifying-%d-", answer)
	if errs := errorMarkers(f, 1); g.runs() != 1 || len(errs) != 1 || !strings.HasPrefix(errs[0], key) {
		t.Errorf("%d runs, errors %q; want one run and one error %s…", g.runs(), errs, key)
	}
}

// A change of parent changes the issue's class, so it lifts the block.
func TestParentChangeLiftsTheBlock(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	f.SetType(1, "Weird")
	parent := f.AddIssue("owner", "Parent", "")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.pending = always
	g.claude(okOut, 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	f.SetParent(1, parent)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 2 {
		t.Errorf("%d runs, want a second once the parent changed", g.runs())
	}
}

// A crash in finish, after the result went but before the lease did, left
// a run that was applied: a restart counts no failure, and the next round
// runs.
func TestCrashInFinishIsNoFailure(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	o, root := newOrigin(t), t.TempDir()
	g := newRig(t, f, o, root)
	g.stage.moreQuestions = true
	g.claude(okOut, 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	saveAttempt(t, root, "run1", "", answer, 1) // the lease is back, the result gone
	g = newRig(t, f, o, root)
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.moreQuestions = true
	g.stage.pending = always
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if errs := errorMarkers(f, 1); len(errs) != 0 || g.runs() != 2 || !strings.Contains(newestComment(f, 1), "run=run1 ") {
		t.Errorf("errors %q, %d runs, newest comment %q; want no error, and the next round's run applied", errs, g.runs(), newestComment(f, 1))
	}
}

// An input deleted during the run drops its result, as an edited one does.
func TestDeletedInputDropsResult(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() { f.DeleteComment(answer) }
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if last := newestComment(f, 1); g.runs() != 1 || strings.Contains(last, "run=run1") || len(g.leftovers()) != 0 {
		t.Errorf("%d runs, newest comment %q, left %v; want run1's result dropped", g.runs(), last, g.leftovers())
	}
}

// The run's seen is the newest comment it was given, not just its newest
// input.
func TestSeenIsTheNewestComment(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	late := f.AddComment(1, "mallory", "a late remark")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	if err := g.work(1); err != nil {
		t.Fatal(err)
	}
	if last := newestComment(f, 1); last != specWritten("run1", late) {
		t.Errorf("newest comment %q, want run1's move with seen=%d", last, late)
	}
}

// stopCtx is a context that reads as canceled once stopped is set, though
// its Done never closes, so nothing it started is killed.
type stopCtx struct {
	context.Context
	stopped *atomic.Bool
}

func (c stopCtx) Err() error {
	if c.stopped.Load() {
		return context.Canceled
	}
	return c.Context.Err()
}

// stopWriter sets stopped when written to.
type stopWriter struct{ stopped *atomic.Bool }

func (w stopWriter) Write(p []byte) (int, error) {
	w.stopped.Store(true)
	return len(p), nil
}

// A run that finishes as the daemon stops keeps its lease and result, and
// the restart applies the result without running claude again.
func TestResultFinishedDuringShutdownIsKept(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	o, root := newOrigin(t), t.TempDir()
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	if err := os.WriteFile(g.path("claude", "stderr"), []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	stopped := &atomic.Bool{}
	g.r.Runner.Stderr = stopWriter{stopped}
	s := g.snapshot(1)
	if err := g.r.Work(stopCtx{context.Background(), stopped}, []daemon.Ready{{S: s, Inputs: s.Comments[1:2]}}); err == nil {
		t.Error("Work = nil, want the cancellation")
	}
	if l := g.lease(); l.RunID != "run1" || l.Failed {
		t.Fatalf("lease %+v, want run1's, kept", l)
	}
	if _, err := os.Stat(g.path("state", "runs", "run1.json")); err != nil {
		t.Fatalf("run1's result: %v", err)
	}
	if err := newRig(t, f, o, root).work(1); err != nil {
		t.Fatal(err)
	}
	if last := newestComment(f, 1); g.runs() != 1 || last != specWritten("run1", answer) {
		t.Errorf("%d runs, newest comment %q; want run1's result applied after the restart, with no run", g.runs(), last)
	}
}

// A success starts a new count, even when the next round has the same key.
func TestSuccessResetsTheCount(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 2
	g.stage.pending = always
	g.stage.moreQuestions = true
	for _, out := range []struct {
		out  string
		exit int
	}{{failOut, 1}, {okOut, 0}, {failOut, 1}} {
		g.claude(out.out, out.exit)
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	if errs := errorMarkers(f, 1); g.runs() != 3 || len(errs) != 0 {
		t.Errorf("%d runs, errors %q; want 3 runs, and no cap: one failure since the success", g.runs(), errs)
	}
}

// priority:p3 comes after an issue with no priority, which is p2.
func TestWorkRunsP3Last(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	interview(f)
	f.AddLabel(1, app, "priority:p3")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	var ready []daemon.Ready
	for n := 1; n <= 2; n++ {
		s := g.snapshot(n)
		ready = append(ready, daemon.Ready{S: s, Inputs: s.Comments[1:2]})
	}
	if err := g.r.Work(context.Background(), ready); err != nil {
		t.Fatal(err)
	}
	if g.runs() != 1 || !strings.Contains(newestComment(f, 2), "run=run1") {
		t.Errorf("%d runs, #2's newest comment %q; want one run, on #2", g.runs(), newestComment(f, 2))
	}
}
