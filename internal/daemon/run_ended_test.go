package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// ready reads issues ns and runs step 1 on each, as a pass of the loop
// does, for Work.
func (g *rig) ready(ns ...int) []daemon.Ready {
	g.t.Helper()
	var ready []daemon.Ready
	for _, n := range ns {
		s := g.snapshot(n)
		in, err := g.d.Step.Run(context.Background(), s)
		if err != nil {
			g.t.Fatal(err)
		}
		ready = append(ready, daemon.Ready{S: s, Inputs: in})
	}
	return ready
}

// endLease writes the lease under root as the record of run id on issue 1,
// ended with n failed attempts in the round of the answer seen, as end
// writes it before it removes the run's files.
func endLease(t testing.TB, root, id string, seen int64, n int) {
	t.Helper()
	l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: state.Clarifying, RunID: id, Clone: filepath.Join(root, "runs", id), Seen: seen,
		AttemptKey: fmt.Sprintf("clarifying-%d", seen), Attempt: n, Failed: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "state", "lease.json"), l, 0o600); err != nil {
		t.Fatal(err)
	}
}

// postsOn matches the posts of comments on issue n.
func postsOn(n int) func(githubtest.Request) bool {
	return func(r githubtest.Request) bool {
		return r.Method == http.MethodPost && r.Path == fmt.Sprintf("/issues/%d/comments", n)
	}
}

// A run that isn't counted, whose lease can't be removed once its result
// is gone, is still no attempt, as the lease is marked ended before
// anything is removed. With max_attempts 1, a blocked run gives only its
// transition error, and a result dropped for an edited input no cap error.
func TestUncountedRunWhoseLeaseOutlivesCleanup(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"blocked", "dropped"} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			if name == "blocked" {
				f.SetType(1, "Weird")
			}
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.d.Config.Loop.MaxAttempts = 1
			g.stage.pending = always
			g.claude(okOut, 0)
			if err := os.Mkdir(g.path("state", "runs"), 0o700); err != nil {
				t.Fatal(err)
			}
			// The result can be saved and removed, but the lease can't be
			// removed or replaced.
			g.stage.during = func() {
				if name == "dropped" {
					f.EditComment(answer, "owner", "1b")
				}
				if err := os.Chmod(g.path("state"), 0o500); err != nil {
					t.Error(err)
				}
			}
			t.Cleanup(func() {
				if err := os.Chmod(g.path("state"), 0o700); err != nil {
					t.Error(err)
				}
			})
			if err := g.work(1); err == nil || !strings.Contains(err.Error(), "lease") {
				t.Fatalf("Work = %v, want the lease's error", err)
			}
			g.stage.during = nil
			if err := os.Chmod(g.path("state"), 0o700); err != nil {
				t.Fatal(err)
			}
			g.workN(3)
			errs := errorMarkers(f, 1)
			switch name {
			case "blocked":
				if g.runs() != 1 || len(errs) != 1 || !strings.Contains(errs[0], "kind=transition") {
					t.Errorf("%d runs, errors %q; want one run, and only its transition error", g.runs(), errs)
				}
			case "dropped":
				if last := newestComment(f, 1); g.runs() != 2 || len(errs) != 0 || last != specWritten("run2", answer) {
					t.Errorf("%d runs, errors %q, newest comment %q; want no error, and run2, on the edit, applied", g.runs(), errs, last)
				}
			}
			if left := g.leftovers(); len(left) != 0 || g.lease().RunID != "" {
				t.Errorf("left %v and lease %+v; want nothing", left, g.lease())
			}
		})
	}
}

// What a crash or a failed removal leaves once the lease is marked ended
// is removed by the restart, which counts nothing: the run's files, and
// the lease, which holds no count. With max_attempts 1, a blocked run
// gives only its transition error.
func TestEndedRunLeftoversRemoved(t *testing.T) {
	t.Parallel()
	blockedRig := func(f *githubtest.Fake, o *origin, root string) *rig {
		g := newRig(t, f, o, root)
		g.d.Config.Loop.MaxAttempts = 1
		g.stage.pending = always
		g.claude(okOut, 0)
		return g
	}
	check := func(t *testing.T, g *rig) {
		t.Helper()
		if errs := errorMarkers(g.f, 1); g.runs() != 1 || len(errs) != 1 || !strings.Contains(errs[0], "kind=transition") {
			t.Errorf("%d runs, errors %q; want one run, and only its transition error", g.runs(), errs)
		}
		if left := g.leftovers(); len(left) != 0 || g.lease().RunID != "" {
			t.Errorf("left %v and lease %+v; want nothing", left, g.lease())
		}
	}
	t.Run("the result and lease", func(t *testing.T) {
		f := newFake(t)
		interview(f)
		f.SetType(1, "Weird")
		o, root := newOrigin(t), t.TempDir()
		g := blockedRig(f, o, root)
		// Once the result is saved and the run blocked, it can't be removed.
		f.BeforeRequest(postsOn(1), func() {
			if err := os.Chmod(g.path("state", "runs"), 0o500); err != nil {
				t.Error(err)
			}
		})
		t.Cleanup(func() {
			if err := os.Chmod(g.path("state", "runs"), 0o700); err != nil {
				t.Error(err)
			}
		})
		if err := g.work(1); err == nil {
			t.Fatal("Work = nil, want the error removing the result")
		}
		if l := g.lease(); l.RunID != "run1" || !l.Failed || l.Attempt != 0 {
			t.Fatalf("lease %+v, want run1's, ended with no count", l)
		}
		if err := os.Chmod(g.path("state", "runs"), 0o700); err != nil {
			t.Fatal(err)
		}
		g = blockedRig(f, o, root)
		g.workN(2)
		check(t, g)
	})
	t.Run("the lease", func(t *testing.T) {
		f := newFake(t)
		answer := interview(f)
		f.SetType(1, "Weird")
		o, root := newOrigin(t), t.TempDir()
		blockedRig(f, o, root).workN(1)
		endLease(t, root, "run1", answer, 0) // as a crash before its removal left it
		g := blockedRig(f, o, root)
		g.workN(2)
		check(t, g)
	})
}

// badSchemaStage is a Stage whose schema doesn't compile.
type badSchemaStage struct{ *fakeStage }

func (badSchemaStage) Schema() []byte { return []byte(`{"type": 5}`) }

// A Stage whose schema doesn't compile starts no run, as nothing could
// check its result. It's a setup failure, so no attempt, reported once.
func TestBadSchemaStartsNoRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = always
	g.r.Stages[state.Clarifying] = badSchemaStage{g.stage}
	g.claude(okOut, 0)
	var setup *agent.SetupError
	if err := g.work(1); !errors.As(err, &setup) {
		t.Fatalf("Work = %v, want the setup error", err)
	}
	for range 3 {
		if err := g.work(1); err != nil {
			t.Errorf("Work = %v, want nil: it's reported", err)
		}
	}
	if errs := errorMarkers(f, 1); g.runs() != 0 || len(errs) != 0 || len(g.leftovers()) != 0 || g.lease().RunID != "" {
		t.Errorf("%d runs, errors %q, left %v, lease %+v; want no run, no error, and nothing left", g.runs(), errs, g.leftovers(), g.lease())
	}
}

// A saved result that hit a usage limit, found after a restart, closes the
// gate and isn't counted, so with max_attempts 1 the stage runs again once
// the gate opens.
func TestResumedUsageLimitIsNoAttempt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	root := t.TempDir()
	reset := githubtest.Start.Add(time.Hour)
	saveRun(t, root, "saved", limitOut("error_during_execution", reset), answer)
	g := newRig(t, f, newOrigin(t), root)
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = always
	g.claude(okOut, 0)
	g.workN(2)
	if g.r.Gate.Open(g.now) || !g.r.Gate.OpenAt().Equal(reset) || g.runs() != 0 || len(errorMarkers(f, 1)) != 0 {
		t.Fatalf("the gate opens at %s, %d runs, errors %q; want the gate shut until %s, no run and no error",
			g.r.Gate.OpenAt(), g.runs(), errorMarkers(f, 1), reset)
	}
	if left := g.leftovers(); len(left) != 0 || g.lease().RunID != "" {
		t.Errorf("left %v and lease %+v; want nothing", left, g.lease())
	}
	g.now = reset
	g.workN(1)
	if last := newestComment(f, 1); g.runs() != 1 || last != specWritten("run1", answer) {
		t.Errorf("after the reset: %d runs, newest comment %q; want run1 applied", g.runs(), last)
	}
}

// When the cap's state error keeps failing to post on one issue, the
// failure is logged and the next issue runs. Once the post works, the cap
// is posted once.
func TestCapPostFailureDoesNotStarveOthers(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	f := newFake(t)
	interview(f)
	f.AddLabel(1, app, "priority:p1")
	second := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = always
	g.claude(failOut, 1)
	ctx := context.Background()
	f.AnswerOnce(postsOn(1), http.StatusBadGateway)
	if err := g.r.Work(ctx, g.ready(1, 2)); err == nil {
		t.Fatal("Work = nil, want the failed post")
	}
	g.claude(okOut, 0)
	var logs strings.Builder
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.E))
	for range 2 {
		f.AnswerOnce(postsOn(1), http.StatusBadGateway)
		if err := g.r.Work(ctx, g.ready(1, 2)); err != nil {
			t.Error(err)
		}
	}
	logr.Wait()
	stop()
	if !strings.Contains(logs.String(), "#1: no interviewer run, as it failed loop.max_attempts times, but posting that failed again") {
		t.Errorf("logs %q; want the failed post logged", logs.String())
	}
	if errs := errorMarkers(f, 1); g.runs() != 2 || len(errs) != 0 || newestComment(f, 2) != specWritten("run2", second) {
		t.Fatalf("%d runs, errors on #1 %q, newest comment on #2 %q; want #2 run and applied, and no error yet",
			g.runs(), errs, newestComment(f, 2))
	}
	for range 2 {
		if err := g.r.Work(ctx, g.ready(1, 2)); err != nil {
			t.Fatal(err)
		}
	}
	if errs := errorMarkers(f, 1); g.runs() != 2 || len(errs) != 1 || !strings.Contains(errs[0], "kind=attempts") {
		t.Errorf("%d runs, errors on #1 %q; want the cap posted once, and no run", g.runs(), errs)
	}
}

// Replay over the cap's state error, posted by RunStage once a restart
// finds the cap reached.
func TestCapRetryReplay(t *testing.T) {
	t.Parallel()
	replayRuns(t, func(t testing.TB, f *githubtest.Fake, root string) {
		endLease(t, root, "failed", interview(f), 1)
	}, func(g *rig) {
		g.d.Config.Loop.MaxAttempts = 1
		g.stage.pending = always
		g.claude(failOut, 1)
	})
}

// A run whose move the table lacks keeps the count of the failures before
// it, across restarts, so the next failure in the round can be the last.
func TestBlockedKeepsEarlierCount(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	o, root := newOrigin(t), t.TempDir()
	var g *rig
	pass := func(out string, exit int) {
		t.Helper()
		g = newRig(t, f, o, root)
		g.d.Config.Loop.MaxAttempts = 2
		g.stage.pending = always
		g.claude(out, exit)
		g.workN(1)
	}
	pass(failOut, 1) // attempt 1 fails
	f.SetType(1, "Weird")
	pass(okOut, 0) // attempt 2 is blocked
	if l := g.lease(); !l.Failed || l.Attempt != 1 {
		t.Errorf("after the blocked run the lease is %+v, want the record of attempt 1", l)
	}
	f.SetType(1, "Epic")
	pass(failOut, 1) // attempt 2 fails: the cap
	if errs := errorMarkers(f, 1); g.runs() != 3 || len(errs) != 2 || !strings.Contains(errs[0], "kind=transition") || errs[1] != capError(answer) {
		t.Errorf("%d runs, errors %q; want 3 runs, the transition error, then the cap", g.runs(), errs)
	}
}

// blocked's state error quotes Apply's error, which may hold text the App
// didn't write, so it's escaped.
func TestBlockedTextIsEscaped(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.applyErr = fmt.Errorf("no row for <!-- issuedog:transition to=done -->: %w", state.ErrNoTransition)
	g.claude(okOut, 0)
	g.workN(1)
	if body := newestComment(f, 1); strings.Count(body, "<!--") != 1 || !strings.Contains(body, "no row for &lt;!-- issuedog:transition to=done -->") {
		t.Errorf("the error says %q; want Apply's error escaped, and only the marker unescaped", body)
	}
}

// A failure to post blocked's state error is returned, and the run's
// result kept, so the next pass posts it with no run.
func TestBlockedPostFailureIsReturned(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.pending = always
	g.stage.applyErr = fmt.Errorf("no row: %w", state.ErrNoTransition)
	g.claude(okOut, 0)
	f.AnswerOnce(postsOn(1), http.StatusBadGateway)
	if err := g.work(1); err == nil || len(errorMarkers(f, 1)) != 0 {
		t.Fatalf("Work = %v, errors %q; want the failed post, and none posted", err, errorMarkers(f, 1))
	}
	if l := g.lease(); l.RunID != "run1" || l.Failed {
		t.Errorf("lease %+v, want run1's, kept", l)
	}
	g.workN(2)
	if errs := errorMarkers(f, 1); g.runs() != 1 || len(errs) != 1 || len(g.leftovers()) != 0 {
		t.Errorf("%d runs, errors %q, left %v; want one run, one error, and nothing left", g.runs(), errs, g.leftovers())
	}
}

// Replay over a live run whose move the table lacks, with its state error
// posted by Transition, or by blocked for a move not by Result.Move.
func TestBlockedRunReplay(t *testing.T) {
	t.Parallel()
	for name, more := range map[string]func(g *rig){
		"by Transition": func(*rig) {},
		"by blocked":    func(g *rig) { g.stage.applyErr = fmt.Errorf("no row: %w", state.ErrNoTransition) },
	} {
		t.Run(name, func(t *testing.T) {
			replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) {
				interview(f)
				f.SetType(1, "Weird")
			}, func(g *rig) {
				g.stage.pending = always
				g.claude(okOut, 0)
				more(g)
			})
		})
	}
}

// A lease with no result, whose issue is found gone, is dropped with its
// clone, and the daemon drops the issue within two passes.
func TestResumeFindsIssueGone(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	root := t.TempDir()
	g := newRig(t, f, newOrigin(t), root)
	g.stage.pending = func(daemon.Input) bool { return false }
	var work [][]int
	g.d.Work = func(ctx context.Context, ready []daemon.Ready) error {
		ns := []int{}
		for _, r := range ready {
			ns = append(ns, r.S.Number)
		}
		work = append(work, ns)
		return g.r.Work(ctx, ready)
	}
	iterate(t, g.d)
	saveRun(t, root, "lost", "", answer)
	f.DeleteIssue()
	iterate(t, g.d) // resume's 404
	if left := g.leftovers(); len(left) != 0 || g.lease().RunID != "" {
		t.Errorf("left %v and lease %+v; want the lease and clone gone", left, g.lease())
	}
	iterate(t, g.d) // the second 404: dropped
	from := len(f.Requests())
	iterate(t, g.d)
	if !reflect.DeepEqual(work, [][]int{{1}, {1}, {}, {}}) || g.runs() != 0 {
		t.Errorf("Work got %v, with %d runs; want the deleted issue once more, then never, and no run", work, g.runs())
	}
	if r := readsOf(f, from, 1); len(r) != 0 {
		t.Errorf("read deleted issue 1 again: %+v", r)
	}
}

// After a run the issue is read again, though the run wrote nothing.
func TestIssueReadAgainAfterRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	pending := true
	g.stage.pending = func(daemon.Input) bool { return pending }
	g.claude(failOut, 1)
	iterate(t, g.d)
	pending = false
	from := len(f.Requests())
	iterate(t, g.d)
	if g.runs() != 1 || len(readsOf(f, from, 1)) == 0 {
		t.Errorf("%d runs, and the next pass read %+v; want one run, then issue 1 read again", g.runs(), readsOf(f, from, 1))
	}
}

// An issue with the cap's state error gets no run, even with no count of
// its attempts, such as after a restart that lost the lease.
func TestReportedCapNeedsNoCount(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	o, root := newOrigin(t), t.TempDir()
	g := newRig(t, f, o, root)
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = always
	g.claude(failOut, 1)
	g.workN(1)
	if err := os.Remove(g.path("state", "lease.json")); err != nil {
		t.Fatal(err)
	}
	g = newRig(t, f, o, root)
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = always
	g.workN(2)
	if errs := errorMarkers(f, 1); g.runs() != 1 || len(errs) != 1 {
		t.Errorf("%d runs, errors %q; want one run, then the cap, and no more", g.runs(), errs)
	}
}

// A lease whose result, or issue, can't be read stays for the next pass:
// neither is a failed attempt.
func TestResumeReadErrors(t *testing.T) {
	t.Parallel()
	for name, fail := range map[string]func(t *testing.T, f *githubtest.Fake, g *rig){
		"result": func(t *testing.T, _ *githubtest.Fake, g *rig) {
			if err := os.MkdirAll(g.path("state", "runs", "lost.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		},
		"issue": func(_ *testing.T, f *githubtest.Fake, _ *rig) {
			f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1" }, http.StatusBadGateway)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			saveRun(t, root, "lost", "", answer)
			g := newRig(t, f, newOrigin(t), root)
			g.stage.pending = always
			g.claude(okOut, 0)
			ready := g.ready(1)
			fail(t, f, g)
			if err := g.r.Work(context.Background(), ready); err == nil {
				t.Error("Work = nil, want the read error")
			}
			if l := g.lease(); l.RunID != "lost" || l.Failed || g.runs() != 0 || len(errorMarkers(f, 1)) != 0 {
				t.Errorf("lease %+v, %d runs, errors %q; want the lease kept, uncounted, and no run", l, g.runs(), errorMarkers(f, 1))
			}
		})
	}
}

// chmod sets path's mode, and puts it back to 0700 when the test ends.
func chmod(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0o700); err != nil {
			t.Error(err)
		}
	})
}

// A saved usage-limit result found after a restart keeps the count of the
// failures before it, so with max_attempts 2 the next failure is the cap.
func TestResumedUsageLimitKeepsCount(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	root := t.TempDir()
	reset := githubtest.Start.Add(time.Hour)
	saveAttempt(t, root, "saved", limitOut("error_during_execution", reset), answer, 2)
	g := newRig(t, f, newOrigin(t), root)
	g.d.Config.Loop.MaxAttempts = 2
	g.stage.pending = always
	g.claude(failOut, 1)
	g.workN(1)
	if l := g.lease(); !l.Failed || l.Attempt != 1 {
		t.Errorf("lease %+v, want saved's, ended with a count of 1", l)
	}
	g.now = reset
	g.workN(2)
	if errs := errorMarkers(f, 1); g.runs() != 1 || len(errs) != 1 || errs[0] != capError(answer) {
		t.Errorf("%d runs, errors %q; want one run, then the cap", g.runs(), errs)
	}
}

// An ended lease with a count, resumed in passes with no run, keeps it for
// a later restart.
func TestEndedLeaseKeptAcrossPasses(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	o, root := newOrigin(t), t.TempDir()
	rig := func() *rig {
		g := newRig(t, f, o, root)
		g.d.Config.Loop.MaxAttempts = 2
		g.stage.pending = always
		g.claude(failOut, 1)
		return g
	}
	g := rig()
	g.workN(1) // attempt 1 fails
	g.stage.pending = func(daemon.Input) bool { return false }
	g.workN(2)
	g = rig()
	g.workN(1) // attempt 2 fails: the cap
	if errs := errorMarkers(f, 1); g.runs() != 2 || len(errs) != 1 || errs[0] != capError(answer) {
		t.Errorf("%d runs, errors %q; want 2 runs, then the cap", g.runs(), errs)
	}
}

// A failure to finish a run whose result is applied isn't a failed apply:
// with max_attempts 1, it gives no cap error, and the next pass finishes.
func TestFinishFailureIsNoAttempt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 1
	g.stage.pending = always
	g.claude(okOut, 0)
	if err := os.Mkdir(g.path("state", "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	// Once the result is saved and being applied, it can't be removed.
	f.BeforeRequest(postsOn(1), func() {
		if err := os.Chmod(g.path("state", "runs"), 0o500); err != nil {
			t.Error(err)
		}
	})
	t.Cleanup(func() {
		if err := os.Chmod(g.path("state", "runs"), 0o700); err != nil {
			t.Error(err)
		}
	})
	if err := g.work(1); err == nil || !strings.Contains(err.Error(), "finish run run1") {
		t.Fatalf("Work = %v, want the error finishing run1", err)
	}
	if err := os.Chmod(g.path("state", "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	g.workN(2)
	if errs := errorMarkers(f, 1); len(errs) != 0 || g.lease().RunID != "" || len(g.leftovers()) != 0 || newestComment(f, 1) != specWritten("run1", answer) {
		t.Errorf("errors %q, lease %+v, left %v, newest comment %q; want run1 applied, no error, and nothing left",
			errs, g.lease(), g.leftovers(), newestComment(f, 1))
	}
}

// Errors ending a run are returned: removing an ended lease's files or the
// lease itself in resume, and ending a failed run.
func TestEndErrorsAreReturned(t *testing.T) {
	t.Parallel()
	t.Run("ended lease's files", func(t *testing.T) {
		f := newFake(t)
		answer := interview(f)
		root := t.TempDir()
		saveRun(t, root, "x", failOut, answer)
		endLease(t, root, "x", answer, 1)
		g := newRig(t, f, newOrigin(t), root)
		chmod(t, g.path("state", "runs"), 0o500)
		if err := g.r.Work(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "resume ended run x") {
			t.Errorf("Work = %v, want the error removing x's result", err)
		}
	})
	t.Run("ended lease", func(t *testing.T) {
		f := newFake(t)
		answer := interview(f)
		root := t.TempDir()
		endLease(t, root, "x", answer, 0)
		g := newRig(t, f, newOrigin(t), root)
		chmod(t, g.path("state"), 0o500)
		if err := g.r.Work(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "resume ended run x") {
			t.Errorf("Work = %v, want the error removing the lease", err)
		}
	})
	t.Run("failed run", func(t *testing.T) {
		f := newFake(t)
		interview(f)
		g := newRig(t, f, newOrigin(t), t.TempDir())
		g.claude(failOut, 1)
		// The lease can't be replaced once the run has started.
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
		if err := g.work(1); err == nil || !strings.Contains(err.Error(), "record the failed attempt: end run run1") {
			t.Errorf("Work = %v, want the error ending run1", err)
		}
	})
}
