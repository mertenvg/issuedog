package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// errorMarkers lists the state errors on issue n, by their markers.
func errorMarkers(f *githubtest.Fake, n int) []string {
	var keys []string
	for _, c := range f.State().Issues[n-1].Comments {
		if i := strings.Index(c.Body, "<!-- issuedog:error"); i >= 0 {
			keys = append(keys, c.Body[i:])
		}
	}
	return keys
}

// capError is the attempt cap's marker for the round of answer.
func capError(answer int64) string {
	return fmt.Sprintf("<!-- issuedog:error kind=attempts key=clarifying-%d -->", answer)
}

// snapshot reads issue n afresh.
func (g *rig) snapshot(n int) *github.Snapshot {
	g.t.Helper()
	s, err := g.d.Step.Client.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		g.t.Fatal(err)
	}
	return s
}

// A result that keeps failing to apply, such as with a 422, is tried
// max_attempts times, counted in the lease, then counts as a failed
// attempt. So the attempt cap takes over, and other issues run.
func TestApplyFailuresAreBounded(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	first := interview(f)
	interview(f)
	f.AddLabel(1, app, "priority:p1")
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nloop:\n  max_attempts: 2\n")
	g := newRig(t, f, o, t.TempDir())
	g.stage.applyErr = errors.New("422 body is too long")
	g.claude(okOut, 0)
	pass := func() error { return g.d.Iterate(context.Background()) }
	if err := pass(); err == nil || !strings.Contains(err.Error(), "422") || g.lease().ApplyFailures != 1 || g.lease().Failed {
		t.Fatalf("the first pass = %v, with lease %+v; want the apply error, and the lease kept, counting it", err, g.lease())
	}
	for range 3 {
		if err := pass(); err == nil {
			t.Fatal("a pass with a failing apply returned no error")
		}
	}
	// Two runs on #1, each applied twice: the cap.
	if want := []string{capError(first)}; g.runs() != 2 || !reflect.DeepEqual(errorMarkers(f, 1), want) {
		t.Fatalf("after 4 passes: %d runs, errors %q; want 2 runs and %q", g.runs(), errorMarkers(f, 1), want)
	}
	if err := pass(); err == nil || g.runs() != 3 || g.lease().Issue != 2 {
		t.Errorf("the next pass = %v, with %d runs, the lease on #%d; want #2's run, failing to apply", err, g.runs(), g.lease().Issue)
	}
}

// The count of failed attempts outlives a restart, kept in the lease: a
// failed run's, and a crashed run's, which counts when it's reset.
func TestRestartKeepsAttemptCount(t *testing.T) {
	t.Parallel()
	t.Run("failed runs", func(t *testing.T) {
		f := newFake(t)
		answer := interview(f)
		o, root := newOrigin(t), t.TempDir()
		var g *rig
		for range 6 {
			g = newRig(t, f, o, root)
			g.d.Config.Loop.MaxAttempts = 2
			g.stage.pending = func(daemon.Input) bool { return true }
			g.claude(failOut, 1)
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
		}
		if want := []string{capError(answer)}; g.runs() != 2 || !reflect.DeepEqual(errorMarkers(f, 1), want) {
			t.Errorf("after 6 restarts: %d runs, errors %q; want 2 runs and %q", g.runs(), errorMarkers(f, 1), want)
		}
	})
	t.Run("crashed runs", func(t *testing.T) {
		f := newFake(t)
		answer := interview(f)
		o, root := newOrigin(t), t.TempDir()
		for n := 1; n <= 2; n++ {
			saveAttempt(t, root, "crashed"+strconv.Itoa(n), "", answer, n)
			g := newRig(t, f, o, root)
			g.d.Config.Loop.MaxAttempts = 2
			g.stage.pending = func(daemon.Input) bool { return false }
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			if l := g.lease(); !l.Failed || l.Attempt != n || l.AttemptKey != fmt.Sprintf("clarifying-%d", answer) || len(g.leftovers()) != 0 {
				t.Errorf("restart %d: lease %+v, left %v; want a failed record of attempt %d, and the clone gone", n, l, g.leftovers(), n)
			}
			if got, want := len(errorMarkers(f, 1)), n-1; got != want || g.runs() != 0 {
				t.Errorf("restart %d: %d errors after %d runs; want %d, and no run", n, got, g.runs(), want)
			}
		}
	})
}

// A leftover lease is finished while a usage limit shuts the gate, and
// nothing calls claude.
func TestResumeWhileGateShut(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{"result": okOut, "no result": ""} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			saveRun(t, root, "saved", out, answer)
			g := newRig(t, f, newOrigin(t), root)
			g.claude(okOut, 0)
			reset := githubtest.Start.Add(time.Hour)
			if !g.r.Gate.Observe(g.now, true, fmt.Sprintf("usage limit reached|%d", reset.Unix())) {
				t.Fatal("the gate didn't shut")
			}
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			comments := f.State().Issues[0].Comments
			if applied := comments[len(comments)-1].Body == specWritten("saved", answer); applied != (out != "") || g.runs() != 0 || len(g.leftovers()) != 0 {
				t.Errorf("applied %t, %d runs, left %v; want the result applied if any, no run, and nothing left", applied, g.runs(), g.leftovers())
			}
		})
	}
}

// A move starts a new count of attempts, even with no inputs.
func TestMoveResetsAttemptCount(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 2
	g.stage.pending = func(daemon.Input) bool { return true }
	g.claude(failOut, 1)
	work := func() {
		t.Helper()
		if err := g.r.Work(context.Background(), []daemon.Ready{{S: g.snapshot(1)}}); err != nil {
			t.Fatal(err)
		}
	}
	work()
	f.AddComment(1, app, "<!-- issuedog:transition from=clarifying to=clarifying run=other -->")
	work()
	if errs := errorMarkers(f, 1); g.runs() != 2 || len(errs) != 0 {
		t.Errorf("%d runs and errors %q; want 2 runs, a failure each side of the move, and no cap", g.runs(), errs)
	}
}

// A move the table lacks, such as for an issue of unknown type, gets one
// state error and ends the run, which is no attempt and no error of Work's:
// no run starts until the issue changes. Live or resumed, nothing is left.
func TestApplyStateError(t *testing.T) {
	t.Parallel()
	t.Run("live", func(t *testing.T) {
		f := newFake(t)
		answer := interview(f)
		f.SetType(1, "Weird")
		g := newRig(t, f, newOrigin(t), t.TempDir())
		g.d.Config.Loop.MaxAttempts = 3
		g.stage.pending = func(daemon.Input) bool { return true }
		g.claude(okOut, 0)
		for range 4 {
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
		}
		errs := errorMarkers(f, 1)
		key := fmt.Sprintf("<!-- issuedog:error kind=transition key=clarifying-%d-", answer)
		if g.runs() != 1 || len(errs) != 1 || !strings.HasPrefix(errs[0], key) || len(g.leftovers()) != 0 || g.lease().RunID != "" {
			t.Errorf("%d runs, errors %q, left %v, lease %+v; want one run, one error %s…, and nothing left",
				g.runs(), errs, g.leftovers(), g.lease(), key)
		}
		// Fixing the type changes the issue: the stage runs again.
		f.SetType(1, "Epic")
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
		comments := f.State().Issues[0].Comments
		if last := comments[len(comments)-1].Body; g.runs() != 2 || !strings.Contains(last, "to=spec-review run=run2 ") {
			t.Errorf("after the type is fixed: %d runs, newest comment %q; want run2 applied", g.runs(), last)
		}
	})
	t.Run("resumed", func(t *testing.T) {
		f := newFake(t)
		answer := interview(f)
		f.SetType(1, "Weird")
		root := t.TempDir()
		saveRun(t, root, "saved", okOut, answer)
		g := newRig(t, f, newOrigin(t), root)
		if err := g.work(1); err != nil {
			t.Errorf("Work = %v, want nil: Transition logged the state error", err)
		}
		if errs := errorMarkers(f, 1); len(errs) != 1 || len(g.leftovers()) != 0 || g.lease().RunID != "" {
			t.Errorf("errors %q, left %v and lease %+v; want the transition error, and nothing left", errs, g.leftovers(), g.lease())
		}
	})
}

// A failure to set a run up isn't the agent's, so it's no attempt.
func TestSetupFailureIsNoAttempt(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]func(g *rig){
		"context files":  func(g *rig) { g.stage.contextErr = errors.New("no template") },
		"no environment": func(g *rig) { g.r.Env = nil },
		"claude missing": func(g *rig) { g.r.Runner.Claude = g.path("none") },
		"unknown agent":  func(g *rig) { g.stage.agent = "librarian" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.d.Config.Loop.MaxAttempts = 1
			g.claude(failOut, 1)
			set(g)
			if err := g.work(1); err == nil {
				t.Error("Work = nil, want the setup error")
			}
			if errs := errorMarkers(f, 1); len(errs) != 0 || g.runs() != 0 || len(g.leftovers()) != 0 || g.lease().RunID != "" {
				t.Errorf("errors %q, %d runs, left %v, lease %+v; want no attempt counted, and nothing left", errs, g.runs(), g.leftovers(), g.lease())
			}
		})
	}
}

// A run that doesn't count keeps the count of the failures before it,
// across a restart.
func TestUncountedRunKeepsCount(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	o, root := newOrigin(t), t.TempDir()
	rig := func() *rig {
		g := newRig(t, f, o, root)
		g.d.Config.Loop.MaxAttempts = 2
		g.stage.pending = func(daemon.Input) bool { return true }
		g.claude(failOut, 1)
		return g
	}
	g := rig()
	if err := g.work(1); err != nil { // attempt 1 fails
		t.Fatal(err)
	}
	g = rig()
	g.r.Env = nil
	if err := g.work(1); err == nil { // attempt 2 isn't set up
		t.Fatal("Work = nil, want the setup error")
	}
	if l := g.lease(); !l.Failed || l.Attempt != 1 {
		t.Errorf("after the setup failure the lease is %+v, want the record of attempt 1", l)
	}
	if err := rig().work(1); err != nil { // attempt 2 fails: the cap
		t.Fatal(err)
	}
	if want := []string{capError(answer)}; g.runs() != 2 || !reflect.DeepEqual(errorMarkers(f, 1), want) {
		t.Errorf("%d runs, errors %q; want 2 runs and %q", g.runs(), errorMarkers(f, 1), want)
	}
}

// A shutdown while claude runs is no attempt: the run's files go.
func TestShutdownDuringRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 1
	g.claude(failOut, 1)
	if err := os.WriteFile(g.path("claude", "sleep"), []byte("30"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { // once claude has started
		for _, err := os.Stat(g.path("claude", "count")); err != nil; _, err = os.Stat(g.path("claude", "count")) {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	s := g.snapshot(1)
	if err := g.r.Work(ctx, []daemon.Ready{{S: s, Inputs: s.Comments[1:2]}}); err == nil {
		t.Error("Work = nil, want the cancellation")
	}
	if errs := errorMarkers(f, 1); len(errs) != 0 || len(g.leftovers()) != 0 || g.lease().RunID != "" {
		t.Errorf("errors %q, left %v, lease %+v; want no attempt counted, and nothing left", errs, g.leftovers(), g.lease())
	}
}

// An issue deleted during a run is found gone by the read before the
// apply, so the daemon drops it within two passes.
func TestApplyFindsIssueGone(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = f.DeleteIssue
	iterate(t, g.d)
	if g.runs() != 1 || len(g.leftovers()) != 0 {
		t.Fatalf("%d runs, left %v; want one, its result dropped", g.runs(), g.leftovers())
	}
	iterate(t, g.d) // the second 404: dropped
	from := len(f.Requests())
	iterate(t, g.d)
	if r := readsOf(f, from, 1); len(r) != 0 {
		t.Errorf("read deleted issue 1 again: %+v", r)
	}
}

// The read before a run skips an issue closed or moved since step 1 read it.
func TestPreRunReadSkips(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(f *githubtest.Fake){
		"closed": func(f *githubtest.Fake) { f.Close(1) },
		"moved": func(f *githubtest.Fake) {
			f.RemoveLabel(1, app, "stage:clarifying")
			f.AddLabel(1, app, "stage:spec-review")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.claude(okOut, 0)
			s := g.snapshot(1)
			change(f)
			if err := g.r.Work(context.Background(), []daemon.Ready{{S: s, Inputs: s.Comments[1:2]}}); err != nil || g.runs() != 0 {
				t.Errorf("Work = %v, with %d runs; want no run", err, g.runs())
			}
		})
	}
}

// Pending is asked again after the trust check: an issue whose only
// inputs are from someone no longer trusted gets no run.
func TestPendingAfterTrustCheck(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	x := f.AddComment(1, "x", "x's answer")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.pending = func(in daemon.Input) bool { return len(in.Inputs) > 0 }
	g.claude(okOut, 0)
	s := g.snapshot(1)
	in := slices.DeleteFunc(slices.Clone(s.Comments), func(c github.Comment) bool { return c.ID != x })
	if err := g.r.Work(context.Background(), []daemon.Ready{{S: s, Inputs: in}}); err != nil || g.runs() != 0 {
		t.Errorf("Work = %v, with %d runs; want none on an untrusted input", err, g.runs())
	}
}

// Only an edit to one of the run's inputs drops its result: an untrusted
// user editing their own comment during the run doesn't. (An edited input
// is TestAnswerEditedDuringRun.)
func TestUntrustedEditDuringRunApplies(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	remark := f.AddComment(1, "eve", "just a remark") // no write access, so no discussion hold
	answer := f.AddComment(1, "owner", "1b")
	f.AddReaction(answer, app, "+1")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.claude(okOut, 0)
	g.stage.during = func() { f.EditComment(remark, "eve", "an edited remark") }
	iterate(t, g.d)
	comments := f.State().Issues[0].Comments
	if last := comments[len(comments)-1].Body; g.runs() != 1 || g.file("prompt.1") != "1a\n1b" || last != specWritten("run1", answer) {
		t.Errorf("%d runs on %q, newest comment %q; want run1 on both answers, applied", g.runs(), g.file("prompt.1"), last)
	}
}

// A failed run logs the last 4 KB of claude's stderr, as text, whatever its
// exit code, and so do one whose output fails the schema (SPEC §3.3) and
// one that hits a usage limit. A run with no stderr logs none.
func TestFailedRunLogsStderr(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	long := "HEAD" + strings.Repeat("x", 5000) + "\x1b[31mTAIL"
	for name, tt := range map[string]struct {
		out, stderr string
		exit        int
		logged      bool
	}{
		"failed":          {out: failOut, stderr: long, exit: 1, logged: true},
		"usage limit":     {out: limitOut("error_during_execution", githubtest.Start.Add(time.Hour)), stderr: long, exit: 1, logged: true},
		"failed, exit 0":  {out: failOut, stderr: long, exit: 0, logged: true},
		"invalid":         {out: invalidOut, stderr: long, exit: 0, logged: true},
		"succeeded":       {out: okOut, stderr: long, exit: 0, logged: false},
		"failed, nothing": {out: failOut, stderr: "", exit: 1, logged: false},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFake(t)
			interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.claude(tt.out, tt.exit)
			if err := os.WriteFile(g.path("claude", "stderr"), []byte(tt.stderr), 0o644); err != nil {
				t.Fatal(err)
			}
			var logs strings.Builder
			stop := logr.AddWriter(&logs, logr.WithFilter(logr.W))
			err := g.work(1)
			logr.Wait()
			stop()
			if err != nil {
				t.Fatal(err)
			}
			if logged := strings.Contains(logs.String(), "claude wrote to stderr"); logged != tt.logged {
				t.Fatalf("logged stderr: %t, want %t; logs %q", logged, tt.logged, logs.String())
			}
			_, tail, found := strings.Cut(logs.String(), fmt.Sprintf("the last 4096 of %d bytes claude wrote to stderr:\n", len(long)))
			if found != tt.logged {
				t.Fatalf("logged the stderr tail: %t, want %t; logs %q", found, tt.logged, logs.String())
			}
			if !found {
				return
			}
			want := strings.Repeat("x", 4096-len("\x1b[31mTAIL")) + "�[31mTAIL"
			if !strings.HasPrefix(tail, want) || strings.Contains(tail, "HEAD") {
				t.Errorf("logged tail %q…; want the last 4096 bytes, its control characters replaced", tail[:min(len(tail), 80)])
			}
		})
	}
}

// capText is the attempt cap's text for the interviewer at clarifying,
// with limit attempts, before its marker and any last failure.
func capText(limit int) string {
	return fmt.Sprintf("The interviewer run failed %d times in a row, so I've stopped running it here. "+
		"New input, or a move to another stage, lets it run again.", limit)
}

// The attempt cap's error names why the last attempt failed (SPEC §5.3),
// for the interviewer as for every stage: its check's broken rule, fenced.
// A run that times out names that.
func TestAttemptCapNamesLastFailure(t *testing.T) {
	t.Parallel()
	t.Run("a broken rule", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		n, entry := ivFiled(f, "epic", false, "from=intake to=clarifying event=5")
		g := ivRig(t, f)
		g.d.Config.Loop.MaxAttempts = 1
		g.claude(ivResult(t, ivRound(8, "")), 0)
		if err := g.work(n); err != nil {
			t.Fatal(err)
		}
		want := capText(1) + "\n\nThe last attempt failed:\n\n```text\n" +
			"its result failed the interviewer stage's check: check interviewer output: rule question-count: 8 questions, over the limit of 7\n```\n\n" +
			fmt.Sprintf("<!-- issuedog:error kind=attempts key=clarifying-%d -->", entry)
		if got := newestComment(f, n); got != want {
			t.Errorf("the attempt cap's error =\n%s\nwant\n%s", got, want)
		}
	})
	t.Run("claude's failure", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		answer := interview(f)
		g := newRig(t, f, newOrigin(t), t.TempDir())
		g.d.Config.Loop.MaxAttempts = 2
		g.stage.pending = func(daemon.Input) bool { return true }
		g.claude(failOut, 1)
		for range 2 {
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
		}
		want := capText(2) + "\n\nThe last attempt failed:\n\n```text\nrun run2: running claude: exit status 1\n```\n\n" + capError(answer)
		if got := newestComment(f, 1); got != want || g.lease().LastFailure != "run run2: running claude: exit status 1" {
			t.Errorf("the attempt cap's error =\n%s\nwant\n%s\nwith the lease's last failure %q", got, want, g.lease().LastFailure)
		}
	})
}

// After a restart whose cap error wasn't posted yet, the cap's error names
// the last failure the ended lease kept, fenced, with a marker in it
// escaped, a fence in it inside a longer one, and its mention inert. A
// lease with no last failure, as one written before the field, gets the
// text without it.
func TestAttemptCapAfterRestart(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, last, want string
	}{
		{"a last failure", "rule x: @alice said <!-- issuedog:transition to=done --> ```go",
			"\n\nThe last attempt failed:\n\n````text\nrule x: @alice said &lt;!-- issuedog:transition to=done --> ```go\n````"},
		{"no last failure", "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			l, err := json.Marshal(daemon.Lease{Issue: 1, Stage: "clarifying", RunID: "old", Failed: true, Attempt: 2,
				AttemptKey: fmt.Sprintf("clarifying-%d", answer), LastFailure: tt.last})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(root, "state"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "state", "lease.json"), l, 0o600); err != nil {
				t.Fatal(err)
			}
			g := newRig(t, f, newOrigin(t), root)
			g.d.Config.Loop.MaxAttempts = 2
			g.claude(okOut, 0)
			if err := g.work(1); err != nil {
				t.Fatal(err)
			}
			if got, want := newestComment(f, 1), capText(2)+tt.want+"\n\n"+capError(answer); got != want || g.runs() != 0 {
				t.Errorf("after %d runs, the attempt cap's error =\n%s\nwant\n%s", g.runs(), got, want)
			}
		})
	}
}
