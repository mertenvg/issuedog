package daemon_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// The state errors' public comments quote only causes the daemon wrote
// itself (#320).

// publicCauseLogged is the attempt cap's paragraph for a cause the daemon
// didn't write, and publicCauseLease the lease's LastFailure for it.
const (
	publicCauseLogged = "\n\nThe last attempt failed; see the daemon's log for why.\n\n"
	publicCauseLease  = "(in the daemon's log)"
)

// publicCauseUnsaved makes the runs directory under g's state read-only,
// so the runner can't save a run's result: its error names the local path.
func publicCauseUnsaved(t *testing.T, g *rig) {
	t.Helper()
	dir := g.path("state", "runs")
	if err := os.MkdirAll(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
}

// An attempt cap reached after the runner's failure, which names a local
// path, or after an apply's, which quotes GitHub's error text, says only
// that the daemon's log has the cause, and so does the lease. One reached
// after claude's own failure, a subtype the daemon names, quotes it.
func TestPublicCauseAttempts(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, g *rig)
		leaks []string // what the comment mustn't have
		quote string   // the comment's paragraph on the cause
		lease string
		fails bool // Work returns the apply's error, as a failed apply does
	}{
		{"the runner, a local path", func(t *testing.T, g *rig) {
			publicCauseUnsaved(t, g)
			g.claude(okOut, 0)
		}, []string{"/state/", "permission denied", "temp result"}, publicCauseLogged, publicCauseLease, false},
		{"an apply, GitHub's text", func(_ *testing.T, g *rig) {
			g.stage.applyErr = errors.New("PATCH https://api.github.com/repos/o/r/issues/1: 422 Validation Failed")
			g.claude(okOut, 0)
		}, []string{"api.github.com", "422", "Validation Failed"}, publicCauseLogged, publicCauseLease, true},
		{"claude's subtype", func(_ *testing.T, g *rig) { g.claude(failOut, 0) }, []string{"/state/"},
			"\n\nThe last attempt failed:\n\n```text\nrun failed with subtype \"error_max_turns\"\n```\n\n", `run failed with subtype "error_max_turns"`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			answer := interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.d.Config.Loop.MaxAttempts = 1
			tt.setup(t, g)
			if err := g.work(1); (err != nil) != tt.fails {
				t.Fatalf("Work = %v, want an error: %t", err, tt.fails)
			}
			got := newestComment(f, 1)
			if want := capText(1) + tt.quote + capError(answer); got != want {
				t.Errorf("the attempt cap's error =\n%s\nwant\n%s", got, want)
			}
			for _, leak := range append(tt.leaks, g.root) {
				if strings.Contains(got, leak) {
					t.Errorf("the attempt cap's error has %q: %s", leak, got)
				}
			}
			if l := g.lease(); !l.Failed || l.LastFailure != tt.lease {
				t.Errorf("the lease %+v; want it failed, its last failure %q", l, tt.lease)
			}
		})
	}
}

// Replay passes for the attempt cap's error quoting claude's subtype.
func TestPublicCauseAttemptsReplay(t *testing.T) {
	t.Parallel()
	replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) { interview(f) }, func(g *rig) {
		g.d.Config.Loop.MaxAttempts = 1
		g.claude(failOut, 0)
	})
}

// The runner's failure, which the attempt cap's error doesn't quote, is
// logged in full, local path and all, once.
func TestPublicCauseAttemptLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	f := newFake(t)
	interview(f)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Config.Loop.MaxAttempts = 1
	publicCauseUnsaved(t, g)
	g.claude(okOut, 0)
	var logs strings.Builder
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.Monitor))
	err := g.work(1)
	logr.Wait()
	stop()
	if err != nil {
		t.Fatal(err)
	}
	if want := "creating a temp result file: open " + g.path("state", "runs", "run1."); strings.Count(logs.String(), want) != 1 ||
		!strings.Contains(newestComment(f, 1), publicCauseLogged) {
		t.Errorf("logs %q; want the runner's error, from %q, once, and the comment pointing there", logs.String(), want)
	}
}

// A lease from before #320, whose last_failure may hold the runner's text,
// gives the attempt cap's error no cause: its key is no longer read.
func TestPublicCauseOldLease(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	root := t.TempDir()
	l, err := json.Marshal(map[string]any{"issue": 1, "stage": "clarifying", "run_id": "old", "failed": true, "attempt": 2,
		"attempt_key": fmt.Sprintf("clarifying-%d", answer), "last_failure": "run old: open /state/runs/old.json: permission denied"})
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
	if got, want := newestComment(f, 1), capText(2)+"\n\n"+capError(answer); got != want || g.runs() != 0 {
		t.Errorf("after %d runs, the attempt cap's error =\n%s\nwant\n%s", g.runs(), got, want)
	}
}

// The closer question's error, GitHub's text, which its state error
// doesn't quote, is logged in full as the error is posted, once.
func TestPublicCauseCloserLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	var event int64
	f, o := closerErrMerge(&event)(t), digestNotifyOrigin(t)
	var logs strings.Builder
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.E))
	err := closerErrRun(t, o, 3, false)(f)
	logr.Wait()
	stop()
	if err != nil {
		t.Fatal(err)
	}
	if got := closingComments(f, 1, 1); len(got) != 1 || strings.Contains(got[0], closingFailure) ||
		!strings.Contains(got[0], "\n\nThe last question failed; see the daemon's log for why.\n\n") {
		t.Errorf("comments %q; want the state error, pointing to the log, without GitHub's text", got)
	}
	if n := strings.Count(logs.String(), closerErrCause(event)); n != 1 {
		t.Errorf("logged the question's error %d times, want once; logs %q", n, logs.String())
	}
}

// The edit history read's error, GitHub's text, which its state error
// doesn't quote, is logged in full on each pass it fails, once.
func TestPublicCauseDigestHistoryLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	f, o := newFake(t), newOrigin(t)
	f.AddIssue(app, "issuedog digest", "gone")
	d := digestDaemon(t, f, o, nil)
	var logs strings.Builder
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.E))
	for range 3 {
		f.FailGraphQL(digestHistoryFailure)
		iterate(t, d)
	}
	logr.Wait()
	stop()
	if c := f.State().Issues[0].Comments; len(c) != 1 || strings.Contains(c[0].Body, digestHistoryFailure) ||
		!strings.Contains(c[0].Body, "\n\nThe last read failed; see the daemon's log for why.\n\n") {
		t.Errorf("comments %+v; want the state error, pointing to the log, without GitHub's text", c)
	}
	if n := strings.Count(logs.String(), digestHistoryFailure); n != 3 {
		t.Errorf("logged the read's error %d times, want once on each of 3 passes; logs %q", n, logs.String())
	}
}
