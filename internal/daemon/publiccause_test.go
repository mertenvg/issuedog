package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// The state errors' public comments quote only causes the daemon wrote
// itself (#320).

// publicCauseLogged is the attempt cap's paragraph for a cause the daemon
// didn't write, and publicCauseLease the lease's PublicFailure for it.
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

// publicCauseQuote is the attempt cap's paragraph quoting text, a cause
// the daemon wrote, fenced.
func publicCauseQuote(text string) string {
	return "\n\nThe last attempt failed:\n\n```text\n" + text + "\n```\n\n"
}

// Claude's own text, which no comment may quote, in a result's text or
// subtype: a local path, GitHub's text, a marker and a mention.
const (
	publicCauseHostile    = "Stuck in /work/run1 and /state/runs/run1.json: api.github.com said 422 Validation Failed <!-- issuedog:transition to=done --> @alice"
	publicCauseHostileOut = `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"` + publicCauseHostile + `"}`
	publicCauseHostileSub = `{"type":"result","subtype":"` + publicCauseHostile + `","is_error":true,"result":""}`
)

// publicCauseHostileLeaks are the parts of publicCauseHostile a comment
// mustn't have.
var publicCauseHostileLeaks = []string{"/work/", "/state/", "api.github.com", "422", "Validation Failed", "issuedog:transition", "@alice", "Stuck"}

// An attempt cap reached after the runner's failure, which names a local
// path, or after an apply's, which quotes GitHub's error text, says only
// that the daemon's log has the cause, and so does the lease. One reached
// after claude's own failure, a subtype the daemon names, quotes it,
// whether claude exited 0 or not, but none of claude's text beside it,
// and nor does one whose subtype isn't one claude is known to write
// (#441). A timeout is named from config, not by the context's error.
// The pass after, unchanged, writes nothing, and runs nothing.
func TestPublicCauseAttempts(t *testing.T) {
	t.Parallel()
	const maxTurns = `run failed with subtype "error_max_turns"`
	for _, tt := range []struct {
		name  string
		setup func(t *testing.T, g *rig)
		leaks []string // what the comment mustn't have
		quote string   // the comment's paragraph on the cause
		lease string
	}{
		{"the runner, a local path", func(t *testing.T, g *rig) {
			publicCauseUnsaved(t, g)
			g.claude(okOut, 0)
		}, []string{"/state/", "permission denied", "temp result"}, publicCauseLogged, publicCauseLease},
		{"an apply, GitHub's text", func(_ *testing.T, g *rig) {
			g.stage.applyErr = errors.New("PATCH https://api.github.com/repos/o/r/issues/1: 422 Validation Failed")
			g.claude(okOut, 0)
		}, []string{"api.github.com", "422", "Validation Failed"}, publicCauseLogged, publicCauseLease},
		{"claude's subtype", func(_ *testing.T, g *rig) { g.claude(failOut, 0) }, []string{"/state/"},
			publicCauseQuote(maxTurns), maxTurns},
		{"claude's subtype, as it exits non-zero", func(_ *testing.T, g *rig) { g.claude(failOut, 1) }, []string{"/state/", "exit status"},
			publicCauseQuote(maxTurns), maxTurns},
		{"claude's text, as it exits non-zero", func(_ *testing.T, g *rig) { g.claude(publicCauseHostileOut, 1) },
			append([]string{"exit status"}, publicCauseHostileLeaks...), publicCauseQuote(maxTurns), maxTurns},
		{"claude's text as the subtype", func(_ *testing.T, g *rig) { g.claude(publicCauseHostileSub, 0) },
			publicCauseHostileLeaks, publicCauseLogged, publicCauseLease},
		{"claude's text as the subtype, as it exits non-zero", func(_ *testing.T, g *rig) { g.claude(publicCauseHostileSub, 1) },
			append([]string{"exit status"}, publicCauseHostileLeaks...), publicCauseLogged, publicCauseLease},
		{"a timeout", func(t *testing.T, g *rig) {
			g.d.Config.Stages.Timeouts.Interviewer = 200 * time.Millisecond
			if err := os.WriteFile(g.path("claude", "sleep"), []byte("120"), 0o644); err != nil {
				t.Fatal(err)
			}
			g.claude(okOut, 0)
		}, []string{"/state/", "/runs/", "run1", "deadline", "signal"}, publicCauseQuote("run timed out after 200ms"), "run timed out after 200ms"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			answer := interview(f)
			g := newRig(t, f, newOrigin(t), t.TempDir())
			g.d.Config.Loop.MaxAttempts = 1
			tt.setup(t, g)
			// A failed attempt, even the last apply's, isn't the pass's
			// error, as failed logs its cause (#441).
			if err := g.work(1); err != nil {
				t.Fatalf("Work = %v, want none", err)
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
			if l := g.lease(); !l.Failed || l.PublicFailure != tt.lease {
				t.Errorf("the lease %+v; want it failed, its last failure %q", l, tt.lease)
			}
			writes, runs := len(f.Writes()), g.runs()
			if err := g.work(1); err != nil || len(f.Writes()) != writes || g.runs() != runs {
				t.Errorf("the unchanged pass = %v, writes %+v, %d runs; want no write and no run", err, f.Writes()[writes:], g.runs()-runs)
			}
		})
	}
}

// Replay passes for the attempt cap's error quoting claude's subtype, as
// claude exits 0 or not, and a timeout (#441).
func TestPublicCauseAttemptsReplay(t *testing.T) {
	t.Parallel()
	for name, more := range map[string]func(g *rig){
		"claude's subtype":                       func(g *rig) { g.claude(failOut, 0) },
		"claude's subtype, as it exits non-zero": func(g *rig) { g.claude(publicCauseHostileOut, 1) },
		"a timeout": func(g *rig) {
			g.d.Config.Stages.Timeouts.Interviewer = 100 * time.Millisecond
			if err := os.WriteFile(g.path("claude", "sleep"), []byte("120"), 0o644); err != nil {
				g.t.Fatal(err)
			}
			g.claude(okOut, 0)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			replayRuns(t, func(_ testing.TB, f *githubtest.Fake, _ string) { interview(f) }, func(g *rig) {
				g.d.Config.Loop.MaxAttempts = 1
				more(g)
			})
		})
	}
}

// publicCauseTyped is a failed attempt whose cause the daemon wrote
// itself, past a check's rules (#441): setup files issue 1 and leaves a
// run's lease on it under root, as a daemon that stopped before applying
// it would, and stage registers the stage that resumes it. pending is
// whether that stage still has work on the issue: only then does a pass
// post a cap error whose post failed before (RunStage), so Replay holds.
// An issue its stage no longer runs on has none, by definition.
type publicCauseTyped struct {
	name    string
	setup   func(t testing.TB, f *githubtest.Fake, root string)
	stage   func(g *rig)
	quote   string // the cause the attempt cap's error quotes
	pending bool
}

// publicCauseTypedCases are the causes the daemon writes itself past a
// check's rules, each reached on a restart, as each is the same on the
// run that found it: no valid result after a restart, an issue its stage
// no longer runs on, read again, and a move that doesn't fit.
func publicCauseTypedCases() []publicCauseTyped {
	ivEntry := "from=intake to=clarifying event=5"
	return []publicCauseTyped{
		{"no valid result after a restart", func(t testing.TB, f *githubtest.Fake, root string) {
			saveAttempt(t, root, "saved", "", interview(f), 1)
		}, func(*rig) {}, "no valid result after a restart", true},
		{"the interviewer, on the owner's task", func(t testing.TB, f *githubtest.Fake, root string) {
			_, entry := ivFiled(f, "task", false, ivEntry)
			saveAttempt(t, root, "saved", ivResult(t, ivRound(1, "")), entry, 1)
		}, func(g *rig) { g.interviewer() },
			`the interviewer no longer runs on #1, with type "task" and a parent false: it runs only on an epic, a story with no parent or an untyped issue, ` +
				"and after a hold from planning, plan-review or planned on a story with a parent", false},
		{"the interviewer's move, which doesn't fit", func(t testing.TB, f *githubtest.Fake, root string) {
			_, entry := ivFiled(f, "", true, ivEntry)
			saveAttempt(t, root, "saved", ivResult(t, specOut("story", "")), entry, 1)
		}, func(g *rig) { g.interviewer() },
			`the interviewer's spec on #1: an issue of type "story" with a parent true can't move: look up clarifying → spec-review for story on spec-written: no such transition`, true},
		{"the planner, on the owner's task", func(t testing.TB, f *githubtest.Fake, root string) {
			n, seen := plFiled(f, "task", false, plEntry)
			changedSaveLease(t, root, n, seen, ivResult(t, plGolden(t, "story-plan")))
		}, func(g *rig) { g.planner(false) },
			`the planner no longer runs on #1, with type "task" and a parent false: it runs only on an epic or a story`, false},
		{"the planner, with no approved plan to revise", func(t testing.TB, f *githubtest.Fake, root string) {
			n, seen := plFiled(f, "story", false, plEntry, "from=planning to=planned rev=1 run=p0", "from=planned to=planning by=owner event=8")
			changedSaveLease(t, root, n, seen, ivResult(t, plGolden(t, "story-plan")))
		}, func(g *rig) { g.planner(false) },
			"the planner no longer runs on #1: it was planned before, but it has no approved plan revision whose data reads back, so it can't be revised", false},
	}
}

// The attempt cap's error quotes each cause the daemon wrote itself, past
// a check's rules, fenced, and the lease keeps it (#441). The pass after,
// unchanged, writes nothing, and runs nothing.
func TestPublicCauseTyped(t *testing.T) {
	t.Parallel()
	for _, tt := range publicCauseTypedCases() {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, root := newFake(t), t.TempDir()
			tt.setup(t, f, root)
			g := newRig(t, f, newOrigin(t), root)
			tt.stage(g)
			g.d.Config.Loop.MaxAttempts = 1
			if err := g.work(1); err != nil {
				t.Fatalf("Work = %v, want none", err)
			}
			got := newestComment(f, 1)
			if !strings.Contains(got, publicCauseQuote(tt.quote)) || !strings.Contains(got, "<!-- issuedog:error kind=attempts ") ||
				strings.Contains(got, root) || g.runs() != 0 {
				t.Errorf("after %d runs, the attempt cap's error =\n%s\nwant it to quote %q, fenced", g.runs(), got, tt.quote)
			}
			if l := g.lease(); !l.Failed || l.PublicFailure != tt.quote {
				t.Errorf("the lease %+v; want it failed, its last failure %q", l, tt.quote)
			}
			writes := len(f.Writes())
			if err := g.work(1); err != nil || len(f.Writes()) != writes || g.runs() != 0 {
				t.Errorf("the unchanged pass = %v, writes %+v, %d runs; want no write and no run", err, f.Writes()[writes:], g.runs())
			}
		})
	}
}

// Replay passes for the attempt cap's error quoting each cause the daemon
// wrote itself, past a check's rules (#441), on an issue whose stage still
// has work pending: on any other, a cap error whose post failed isn't
// posted again, as no run would start there either.
func TestPublicCauseTypedReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range publicCauseTypedCases() {
		if !tt.pending {
			continue
		}
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			replayRuns(t, tt.setup, func(g *rig) {
				tt.stage(g)
				g.d.Config.Loop.MaxAttempts = 1
			})
		})
	}
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

// The last failure to apply a result, GitHub's text, which the attempt
// cap's error doesn't quote, is logged in full, once, as the attempt
// fails, and isn't the pass's error too, which the loop would log again
// (SPEC §5.3, #441).
func TestPublicCauseApplyLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	const cause = "PATCH https://api.github.com/repos/o/r/issues/1: 422 Validation Failed"
	f := newFake(t)
	interview(f)
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nloop:\n  max_attempts: 1\n") // Iterate loads config from main
	g := newRig(t, f, o, t.TempDir())
	g.stage.applyErr = errors.New(cause)
	g.claude(okOut, 0)
	var logs strings.Builder
	stop := logr.AddWriter(&logs, logr.WithFilter(logr.Monitor))
	err := g.d.Iterate(context.Background())
	logr.Wait()
	stop()
	if err != nil || strings.Count(logs.String(), cause) != 1 || !strings.Contains(newestComment(f, 1), publicCauseLogged) {
		t.Errorf("Iterate = %v, logs %q; want no error, the apply's error logged once, and the comment pointing there", err, logs.String())
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
