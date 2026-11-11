package daemon_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A task's round of attempts with the verifier in it (SPEC §8.3): a
// request_changes is the implementer attempt before it failing verify, so
// it counts in the one round with failed runs and failed checks, and each
// retry runs one effort level higher with the latest findings.

// verifierRoundRig is a verdictRig whose implementer starts at effort low,
// raised up to high, and whose verifier runs at low, with a third command,
// which fails while failChecks is on.
type verifierRoundRig struct {
	*verdictRig
	flag string
}

// newVerifierRoundRig builds a verifierRoundRig on f.
func newVerifierRoundRig(t *testing.T, f *githubtest.Fake, attempts int) *verifierRoundRig {
	t.Helper()
	flag := filepath.Join(t.TempDir(), "fail")
	v := newVerdictRigWith(t, f, attempts, "stages:\n  effort: {implementer: low, verifier: low}\n  max_effort: high\n", `, "! test -f '`+flag+`'"`)
	return &verifierRoundRig{verdictRig: v, flag: flag}
}

// failChecks makes the checks fail, or pass again.
func (v *verifierRoundRig) failChecks(fail bool) {
	v.t.Helper()
	if !fail {
		if err := os.Remove(v.flag); err != nil {
			v.t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(v.flag, nil, 0o644); err != nil {
		v.t.Fatal(err)
	}
}

// verifierRoundTask files a task the owner set to in-progress.
func verifierRoundTask(f *githubtest.Fake) int {
	n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	return n
}

// checkRun fails the test unless claude's run k was a run at stage, of
// attempt, at effort.
func (v *verdictRig) checkRun(k int, stage state.Stage, attempt int, effort string) {
	v.t.Helper()
	l := v.leaseOf(strconv.Itoa(k))
	if l.Stage != stage || l.Attempt != attempt {
		v.t.Errorf("run %d's lease = %+v; want attempt %d at %s", k, l, attempt, stage)
	}
	if got := v.roundEffort(k); got != effort {
		v.t.Errorf("run %d's --effort = %q, want %q", k, got, effort)
	}
}

// A round mixing all three: attempt 1 is a run that fails, attempt 2's head
// gets the verifier's request_changes, and attempt 3's fails the checks,
// which is the round's last. Each implementer run is one effort level
// higher than the one before (low, medium, high), and the verifier's stays
// at its own. The findings move carries attempt=2, its comment names
// attempt 2 of 3, and attempt 3's input.md holds it. The checks' failure
// then takes the verify cap's hand-off, as the round's count reached it.
func TestVerifierRoundRunThenVerdictThenChecks(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerifierRoundRig(t, f, 3)
	commits := ivResult(t, implementerOutput(t, "commits"))
	v.out(1, failOut)
	v.out(2, commits)
	v.out(3, ivResult(t, verifierOutput(t, "request_changes")))
	v.out(4, commits)
	n := verifierRoundTask(f)

	v.pass()
	v.checkRun(1, state.InProgress, 1, "low")
	v.pass()
	v.checkRun(2, state.InProgress, 2, "medium")
	head := v.trimmed("commit.2")
	if got := pickStage(f, n); got != "stage:verifying" {
		t.Fatalf("after run 2, the task is at %s, want verifying", got)
	}
	v.passes(2)
	v.checkRun(3, state.Verifying, 1, "low")
	if got := pickStage(f, n); got != "stage:in-progress" {
		t.Fatalf("after the verifier, the task is at %s, want in-progress", got)
	}
	moves := verifierMoves(f, n)
	if len(moves) != 1 || moves[0].Fields["attempt"] != "2" || moves[0].Fields["head"] != head {
		t.Fatalf("the verifier's moves = %+v; want one, of attempt 2 on %s", moves, head)
	}
	if body := verifierNewest(f, n); !strings.HasPrefix(body, "The verifier asked for changes on attempt 2 of 3, at `"+head+"`.\n\n### ") {
		t.Errorf("the comment =\n%s\nwant it to name attempt 2 of 3", body)
	}

	v.pass()
	v.checkRun(4, state.InProgress, 3, "high")
	input := v.roundInput(4)
	if !strings.Contains(input, "## The findings of verify attempt 2") ||
		!strings.Contains(input, "The verifier asked for changes on attempt 2 of 3, at `"+head+"`.") ||
		!strings.Contains(input, "`TestBookExpiredQuote` builds its quote") {
		t.Errorf("run 4's input.md =\n%s\nwant the verifier's findings of attempt 2", input)
	}
	v.failChecks(true)
	v.pass()
	if got := pickStage(f, n); got != "stage:needs-human" {
		t.Fatalf("after the failed checks, the task is at %s, want needs-human", got)
	}
	if body := verifierNewest(f, n); !strings.HasPrefix(body, "This task's 3 attempts in this round failed, so I've stopped running the implementer here.\n\nVerify failed on attempt 3 of 3,") {
		t.Errorf("the hand-off =\n%s\nwant the checks' findings of attempt 3", body)
	}
	if v.runs() != 4 {
		t.Errorf("%d runs, want four", v.runs())
	}
}

// The same three the other way round: attempt 1's head fails the checks,
// attempt 2 is a run that fails, and attempt 3's head gets a
// request_changes, the round's last, so the verify cap's hand-off gives the
// verifier's findings. That releases the worker: the next task is picked,
// and a pass after it writes nothing on this one. /done starts a new round,
// at effort low, with no findings.
func TestVerifierRoundChecksThenRunThenVerdict(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerifierRoundRig(t, f, 3)
	commits := ivResult(t, implementerOutput(t, "commits"))
	v.out(1, commits)
	v.out(2, failOut)
	v.out(3, commits)
	v.out(4, ivResult(t, verifierOutput(t, "request_changes")))
	v.out(5, commits)
	n := verifierRoundTask(f)

	v.pass()
	v.checkRun(1, state.InProgress, 1, "low")
	v.failChecks(true)
	v.pass()
	if got := pickStage(f, n); got != "stage:in-progress" {
		t.Fatalf("after the failed checks, the task is at %s, want in-progress", got)
	}
	v.failChecks(false)
	v.pass()
	v.checkRun(2, state.InProgress, 2, "medium")
	if input := v.roundInput(2); !strings.Contains(input, "## The findings of verify attempt 1") {
		t.Errorf("run 2's input.md =\n%s\nwant the checks' findings of attempt 1", input)
	}
	v.pass()
	v.checkRun(3, state.InProgress, 3, "high")
	head := v.trimmed("commit.3")
	if input := v.roundInput(3); !strings.Contains(input, "## The findings of verify attempt 1") {
		t.Errorf("run 3's input.md =\n%s\nwant the same findings: they stand for a failed run's retry", input)
	}
	v.passes(2)
	v.checkRun(4, state.Verifying, 1, "low")
	if got := pickStage(f, n); got != "stage:needs-human" {
		t.Fatalf("after the verdict, the task is at %s, want needs-human", got)
	}
	want := "This task's 3 attempts in this round failed, so I've stopped running the implementer here.\n\n" +
		"The verifier asked for changes on attempt 3 of 3, at `" + head + "`.\n\n### A quote past its expiry time can't be booked\n\n"
	if body := verifierNewest(f, n); !strings.HasPrefix(body, want) {
		t.Errorf("the hand-off =\n%s\nwant it to start\n%s", body, want)
	}
	if m := verifierMoves(f, n); len(m) != 1 || m[0].Fields["to"] != "needs-human" {
		t.Errorf("the verifier's moves = %+v; want the one to needs-human", m)
	}
	v.passes(2)
	v.quietPass("after the hand-off")

	f.AddComment(n, wholeRunOwner, "/done")
	v.passes(2)
	v.checkRun(5, state.InProgress, 1, "low")
	if input := v.roundInput(5); strings.Contains(input, "findings of verify") {
		t.Errorf("run 5's input.md =\n%s\nwant no findings: it's a new round", input)
	}
}

// The round's count lives on the move, so a restart between attempts reads
// it back: after a request_changes on attempt 1, the daemon restarts, and
// the next run is attempt 2, one level higher, with the findings. Its
// request_changes, on a head of its own, then goes on to attempt 3.
func TestVerifierRoundRestart(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerifierRoundRig(t, f, 3)
	commits := ivResult(t, implementerOutput(t, "commits"))
	request := ivResult(t, verifierOutput(t, "request_changes"))
	v.out(1, commits)
	v.out(2, request)
	v.out(3, commits)
	v.out(4, request)
	v.out(5, commits)
	n := verifierRoundTask(f)

	v.passes(3)
	if got := pickStage(f, n); got != "stage:in-progress" {
		t.Fatalf("after the verdict, the task is at %s, want in-progress", got)
	}
	r := v.restart()
	r.pass()
	r.checkRun(3, state.InProgress, 2, "medium")
	if input := r.roundInput(3); !strings.Contains(input, "## The findings of verify attempt 1") {
		t.Errorf("run 3's input.md =\n%s\nwant the verifier's findings of attempt 1", input)
	}
	r.passes(2)
	r.checkRun(4, state.Verifying, 1, "low")
	r.pass()
	r.checkRun(5, state.InProgress, 3, "high")
	if m := verifierMoves(f, n); len(m) != 2 || m[1].Fields["attempt"] != "2" {
		t.Errorf("the verifier's moves = %+v; want two, the last of attempt 2", m)
	}
}

// A saved request_changes across a restart applies as a live one does, and
// a saved one on the last attempt hands off: the count is the move's.
func TestVerifierRoundSavedAtTheCap(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerifierRoundRig(t, f, 2)
	commits := ivResult(t, implementerOutput(t, "commits"))
	request := ivResult(t, verifierOutput(t, "request_changes"))
	v.out(1, commits)
	v.out(2, request)
	v.out(3, commits)
	v.out(4, request)
	n := verifierRoundTask(f)
	v.passes(5)
	f.FailWrite(len(f.Writes()) + 1)
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose hand-off failed gave no error")
	}
	if got := pickStage(f, n); got != "stage:verifying" || v.runs() != 4 {
		t.Fatalf("after the failed hand-off: at %s, %d runs; want verifying, four", got, v.runs())
	}
	r := v.restart()
	r.pass()
	if got := pickStage(f, n); got != "stage:needs-human" || r.runs() != 4 {
		t.Errorf("after the restart: at %s, %d runs; want needs-human, four", got, r.runs())
	}
	if m := verifierMoves(f, n); len(m) != 2 || m[1].Fields["to"] != "needs-human" {
		t.Errorf("the verifier's moves = %+v; want two, the last to needs-human", m)
	}
}

// A task back on the verifier's findings holds the worker and costs a pass
// nothing while stage work waits: with the gate closed, an unchanged pass
// makes only conditional reads and starts no run.
func TestVerifierFindingsQuietPass(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, _ := verifierTask(f, head)
	v.out(1, ivResult(t, verifierOutput(t, "request_changes")))
	v.passes(2)
	if got := pickStage(f, n); got != "stage:in-progress" {
		t.Fatalf("after the verdict, the task is at %s, want in-progress", got)
	}
	v.r.Gate.Observe(v.now, true, "Claude usage limit reached|"+strconv.FormatInt(v.now.Unix()+3600, 10))
	if v.r.Gate.Open(v.now) {
		t.Fatal("the gate is open")
	}
	v.pass()
	v.quietPass("after the findings move")
	if v.runs() != 1 {
		t.Errorf("%d runs, want one", v.runs())
	}
}

// An owner's move into in-progress starts a round, though a verifier's
// findings came before it: the next run is attempt 1 at effort low, with no
// findings in its input.md.
func TestVerifierRoundOwnerMoveStartsRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerifierRoundRig(t, f, 3)
	commits := ivResult(t, implementerOutput(t, "commits"))
	v.out(1, commits)
	v.out(2, ivResult(t, verifierOutput(t, "request_changes")))
	v.out(3, commits)
	v.out(4, commits)
	n := verifierRoundTask(f)

	v.passes(3)
	v.pass()
	v.checkRun(3, state.InProgress, 2, "medium")
	if got := pickStage(f, n); got != "stage:verifying" {
		t.Fatalf("after run 3, the task is at %s, want verifying", got)
	}
	f.RemoveLabel(n, wholeRunOwner, "stage:verifying")
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	v.pass()
	v.checkRun(4, state.InProgress, 1, "low")
	if input := v.roundInput(4); strings.Contains(input, "findings of verify") {
		t.Errorf("run 4's input.md =\n%s\nwant no findings: the owner's move started a round", input)
	}
}

// daemon.Verifier is FailClosed, and no other Stage is: only its runs turn
// unreadable output into an output of their own.
func TestOnlyTheVerifierFailsClosed(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	for at, st := range v.r.Stages {
		if _, ok := st.(daemon.FailClosed); ok != (at == state.Verifying) {
			t.Errorf("the Stage at %s is FailClosed: %t, want %t", at, ok, at == state.Verifying)
		}
	}
	if _, ok := v.r.Unlabeled.(daemon.FailClosed); ok {
		t.Error("the Unlabeled stage is FailClosed; want it not")
	}
}
