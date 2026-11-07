package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is a failed verifier run (SPEC §8.3): it gives no verdict and
// spends none of the implementer's attempts. It's tried again on the same
// head at the same effort, with nothing written, and the last of
// loop.max_attempts in a row hands the task off at needs-human.

// verifierFailedFields is the hand-off for the verifier's failed runs on
// task n from its fields on, with the cc line for login.
func verifierFailedFields(n int, login string) string {
	return "**Goal:** Find out why the verifier's runs on this task fail, and fix that, so it can judge the task's head.\n" +
		"**Why it blocks:** #" + strconv.Itoa(n) + " can't proceed until what made its verifier runs fail is fixed\n" +
		"**Steps:**\n" +
		"1. Read why the last run failed, above, or in the daemon's log.\n" +
		"2. Fix what made it fail, such as a limit in `stages` in `.issuedog/config.yaml`, or the prompt in `.issuedog/prompts/verifier.md`.\n\n" +
		"**Links:**\nNone.\n\n" +
		"**Done when:** What made the verifier's runs fail is fixed.\n" +
		"**Estimated time:** It depends on why the runs failed.\n\n" +
		"When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`.\n\n" +
		"cc @" + login
}

// verifierFailedLogged is what the hand-off says of a cause only the
// daemon's log has.
const verifierFailedLogged = "The last run failed; see the daemon's log for why."

// verifierFailedQuoted is what the hand-off says of a cause the daemon
// wrote.
func verifierFailedQuoted(cause string) string {
	return "The last run failed:\n\n```text\n" + cause + "\n```"
}

// verifierFailedHandoff is the comment of the hand-off on task n, after
// runs failed runs, the last of them for the reason why, with the cc line
// for login, keyed by the move into verifying, comment move, with seen.
func verifierFailedHandoff(n, runs int, why, login string, move, seen int64) string {
	return fmt.Sprintf("The verifier's %d runs on this head failed, so I've stopped running it here. "+
		"That says nothing about the head, and uses none of this task's attempts.\n\n", runs) +
		why + "\n\n" + verifierFailedFields(n, login) + "\n\n" + verifierFailedMarker(move, seen)
}

// verifierFailedMarker is the marker of the hand-off's move.
func verifierFailedMarker(move, seen int64) string {
	return fmt.Sprintf("<!-- issuedog:transition from=verifying to=needs-human event=averifying-%d seen=%d -->", move, seen)
}

// verifierFailedRun is one way a verifier run fails, as the fake claude's
// run k: what it prints, its exit, or a sleep past stages.timeouts.verifier.
type verifierFailedRun struct {
	name  string
	out   string
	exit  string
	sleep bool
	why   string // what the hand-off says of it
}

// set makes the fake claude's run k fail this way on v.
func (fr verifierFailedRun) set(v *verdictRig, k int) {
	v.t.Helper()
	v.out(k, fr.out)
	for name, data := range map[string]string{"exit": fr.exit, "sleep": map[bool]string{true: "30"}[fr.sleep]} {
		if data == "" {
			continue
		}
		if err := os.WriteFile(v.path("claude", name+"."+strconv.Itoa(k)), []byte(data), 0o644); err != nil {
			v.t.Fatal(err)
		}
	}
}

// verifierFailedRuns are the ways a run fails with nothing in its output
// the daemon can call a verdict. The last two say only the log has why, as
// what they end with isn't the daemon's text, and that's the run's own: a
// subtype claude isn't known to write, planted with a marker and a mention,
// and an exit status.
func verifierFailedRuns() []verifierFailedRun {
	return []verifierFailedRun{
		{name: "past stages.timeouts.verifier", out: failOut, sleep: true, why: verifierFailedQuoted("run timed out after 1s")},
		{name: "error_max_turns", out: failOut, why: verifierFailedQuoted(`run failed with subtype "error_max_turns"`)},
		{name: "is_error", out: `{"type":"result","subtype":"success","is_error":true,"result":"` + verifierHostile + `"}`,
			why: verifierFailedQuoted("run reported an error")},
		{name: "an unknown subtype", out: publicCauseHostileSub, why: verifierFailedLogged},
		{name: "a non-zero exit with no result", out: verifierHostile, exit: "1", why: verifierFailedLogged},
	}
}

// Each way of failing, with loop.max_attempts 2 and 3: the head gets
// another run at the same effort after each one, with nothing written and
// no move, until the last in a row, which moves the task verifying →
// needs-human on attempt-cap, keyed by the attempt key, with seen= the
// newest comment the run read, and a hand-off in the daemon's words alone.
// There's no state error. An unchanged pass after it costs only 304s, and
// no run starts.
func TestVerifierFailedRunRetries(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{2, 3} {
		for _, fr := range verifierFailedRuns() {
			t.Run(strconv.Itoa(limit)+" attempts, "+fr.name, func(t *testing.T) {
				t.Parallel()
				f := newFake(t)
				v := newVerdictRigWith(t, f, limit, "stages:\n  timeouts: {verifier: 1s}\n", "")
				head := v.head()
				n, move := verifierTask(f, head)
				f.AddLabel(n, "owner", "verifier-effort:low")
				for k := 1; k <= limit; k++ {
					fr.set(v, k)
				}
				v.pass() // the checks
				writes := len(f.Writes())
				for k := 1; k <= limit; k++ {
					if k < limit && (pickStage(f, n) != "stage:verifying" || len(f.Writes()) != writes) {
						t.Fatalf("before run %d: at %s, new writes %+v; want verifying, and none", k, pickStage(f, n), f.Writes()[writes:])
					}
					v.pass()
					l := v.leaseOf(strconv.Itoa(k))
					if v.runs() != k || l.Attempt != k || l.Start != head || l.AttemptKey != "verifying-"+strconv.FormatInt(move, 10) {
						t.Fatalf("after pass %d: %d runs, lease %+v; want attempt %d of the move's round, on %s", k, v.runs(), l, k, head)
					}
					v.checkFlags(k, "", "low", "")
				}
				if want := verifierFailedHandoff(n, limit, fr.why, wholeRunOwner, move, move); verifierNewest(f, n) != want {
					t.Errorf("the newest comment =\n%s\nwant\n%s", verifierNewest(f, n), want)
				}
				text, _, _ := strings.Cut(verifierNewest(f, n), "<!-- issuedog:transition from=verifying ")
				for _, leak := range publicCauseHostileLeaks {
					if strings.Contains(text, leak) {
						t.Errorf("the hand-off =\n%s\nwant nothing of the run's output, such as %q", text, leak)
					}
				}
				if pickStage(f, n) != "stage:needs-human" || len(verifierMoves(f, n)) != 0 || len(errorMarkers(f, n)) != 0 {
					t.Errorf("at %s, the run's moves %+v, errors %q; want needs-human, and none of either", pickStage(f, n), verifierMoves(f, n), errorMarkers(f, n))
				}
				v.passes(2)
				v.quietPass("after the hand-off")
				if v.runs() != limit {
					t.Errorf("%d runs, want %d: the task at needs-human gets none", v.runs(), limit)
				}
			})
		}
	}
}

// A run that gives a result after failed runs ends the count: its pass is
// recorded as any pass is, the task stays at verifying, and no hand-off
// comes, with the run the last allowed.
func TestVerifierFailedRunThenAPass(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 2)
	head := v.head()
	n, _ := verifierTask(f, head)
	v.out(1, failOut)
	v.out(2, verifierPass(t, "Fine."))
	v.passes(3)
	if got := passComments(f, n); v.runs() != 2 || len(got) != 1 || pickStage(f, n) != "stage:verifying" {
		t.Fatalf("%d runs, the verdicts %q, at %s; want two, one, still at verifying", v.runs(), got, pickStage(f, n))
	}
	if l := v.lease(); l.RunID != "" {
		t.Errorf("the lease = %+v; want none: the count ended with the result", l)
	}
	v.pass()
	v.quietPass("after the pass")
}

// The task's round doesn't change, whatever fails and however many, across
// a restart too: the implementer's attempt 1 makes a head, and two failed
// verifier runs, with a restart between, leave its round where it was. The
// next verifier run's request_changes names attempt 1 on its move, and the
// run after it is the implementer's attempt 2, at the effort of attempt 2,
// with that finding. The verifier's own effort never rises.
func TestVerifierFailedRunsKeepTheRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerifierRoundRig(t, f, 3)
	commits := ivResult(t, implementerOutput(t, "commits"))
	v.out(1, commits)
	v.out(2, failOut)
	v.out(3, failOut)
	v.out(4, ivResult(t, verifierOutput(t, "request_changes")))
	v.out(5, commits)
	n := verifierRoundTask(f)

	v.passes(3)
	v.checkRun(2, state.Verifying, 1, "low")
	r := v.restart()
	r.passes(3) // the checks again, run 3, and run 4
	r.checkRun(3, state.Verifying, 2, "low")
	r.checkRun(4, state.Verifying, 3, "low")
	moves := verifierMoves(f, n)
	if len(moves) != 1 || moves[0].Fields["attempt"] != "1" || moves[0].Fields["to"] != "in-progress" {
		t.Fatalf("the verifier's moves = %+v; want one, back to in-progress, of attempt 1", moves)
	}
	r.pass()
	r.checkRun(5, state.InProgress, 2, "medium")
	if input := r.roundInput(5); !strings.Contains(input, "## The findings of verify attempt 1, which this run must fix") {
		t.Errorf("run 5's input.md =\n%s\nwant the verifier's findings of attempt 1", input)
	}
}

// A pass after failed runs names the attempt of the move into verifying
// too, across a restart, not the verifier's own count.
func TestVerifierFailedRunThenPassAttempt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, _ := verifierTask(f, head)
	v.out(1, failOut)
	v.out(2, verifierPass(t, "Fine."))
	v.passes(2)
	r := v.restart()
	r.passes(2)
	got := passComments(f, n)
	if r.leaseOf("2").Attempt != 2 || len(got) != 1 || !strings.Contains(got[0], " attempt=1 ") {
		t.Errorf("run 2's lease %+v, the verdicts %q; want attempt 2, and one naming attempt=1", r.leaseOf("2"), got)
	}
}

// On the round's last attempt, a failed verifier run isn't the attempt
// failing: with the head on attempt 2 of 2, the run fails, no hand-off
// comes, and the verdict after it is what decides. Its request_changes is
// the verify cap's, naming 2 attempts.
func TestVerifierFailedRunOnTheLastAttempt(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 2)
	head := v.head()
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, app, "stage:verifying")
	f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r1 head="+head+" -->")
	v.out(1, failOut)
	v.out(2, ivResult(t, verifierOutput(t, "request_changes")))
	v.passes(2)
	if got := pickStage(f, n); got != "stage:verifying" {
		t.Fatalf("after the failed run, the task is at %s; want verifying", got)
	}
	v.pass()
	if body := verifierNewest(f, n); pickStage(f, n) != "stage:needs-human" || !strings.HasPrefix(body, "This task's 2 attempts in this round failed") {
		t.Errorf("at %s, the newest comment =\n%s\nwant needs-human, on the verify cap's hand-off", pickStage(f, n), body)
	}
}

// contextFailsVerifier is the Verifier, but its Context fails while fail
// gives an error, which is a setup failure.
type contextFailsVerifier struct {
	*daemon.Verifier
	fail func() error
}

func (c contextFailsVerifier) Context(ctx context.Context, in daemon.Input) (map[string][]byte, error) {
	if err := c.fail(); err != nil {
		return nil, err
	}
	files, err := c.Verifier.Context(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("the verifier's context: %w", err)
	}
	return files, nil
}

// A context read that fails starts no run and counts nothing, on any pass:
// with loop.max_attempts at 1 and the head at the cap's edge, no hand-off
// comes of it, and once the read works the head gets its run, attempt 1.
func TestVerifierContextFailureIsUncounted(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 1)
	head := v.head()
	n, _ := verifierTask(f, head)
	v.out(1, verifierPass(t, "Fine."))
	readErr := errors.New("the prompt can't be read")
	v.r.Stages[state.Verifying] = contextFailsVerifier{Verifier: &daemon.Verifier{Daemon: v.d}, fail: func() error { return readErr }}
	v.pass()
	for range 2 {
		var setup *agent.SetupError
		if err := v.d.Iterate(context.Background()); !errors.As(err, &setup) {
			t.Fatalf("the pass gave %v; want a setup failure", err)
		}
		if l := v.lease(); v.runs() != 0 || l.RunID != "" || pickStage(f, n) != "stage:verifying" || len(errorMarkers(f, n)) != 0 {
			t.Fatalf("after the failure: %d runs, lease %+v, at %s, errors %q; want none, none, verifying, none", v.runs(), l, pickStage(f, n), errorMarkers(f, n))
		}
	}
	readErr = nil
	v.pass()
	if l := v.leaseOf("1"); v.runs() != 1 || l.Attempt != 1 || len(passComments(f, n)) != 1 {
		t.Errorf("once the read works: %d runs, lease %+v, %d verdicts; want one, attempt 1, one", v.runs(), l, len(passComments(f, n)))
	}
}

// A usage limit on a verifier run isn't counted, after a failed run too:
// the gate closes, the count stays where the failed run left it, and no
// hand-off comes at loop.max_attempts runs. Once the gate opens, the head's
// next run is the one that reaches the cap.
func TestVerifierUsageLimitAfterAFailedRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 2)
	head := v.head()
	n, move := verifierTask(f, head)
	reset := v.now.Add(time.Hour)
	v.out(1, failOut)
	v.out(2, limitOut("error_max_turns", reset))
	v.out(3, failOut)
	v.passes(3)
	if v.runs() != 2 || v.r.Gate.Open(v.now) || pickStage(f, n) != "stage:verifying" {
		t.Fatalf("after the limit: %d runs, the gate open %t, at %s; want two, closed, and verifying", v.runs(), v.r.Gate.Open(v.now), pickStage(f, n))
	}
	if l := v.lease(); !l.Failed || l.Attempt != 1 {
		t.Errorf("the lease = %+v; want ended with one failed attempt", l)
	}
	v.pass()
	if v.runs() != 2 {
		t.Errorf("with the gate closed: %d runs, want two", v.runs())
	}
	v.now = reset
	v.pass()
	if l := v.leaseOf("3"); v.runs() != 3 || l.Attempt != 2 {
		t.Fatalf("once the gate opens: %d runs, run 3's lease %+v; want three, attempt 2", v.runs(), l)
	}
	want := verifierFailedHandoff(n, 2, verifierFailedQuoted(`run failed with subtype "error_max_turns"`), wholeRunOwner, move, move)
	if got := verifierNewest(f, n); got != want {
		t.Errorf("the newest comment =\n%s\nwant\n%s", got, want)
	}
}

// A shutdown during a verifier run isn't counted: the run ends with no
// lease left, and the head gets another run, attempt 1.
func TestVerifierShutdownIsUncounted(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 1)
	head := v.head()
	n, _ := verifierTask(f, head)
	v.out(1, verifierPass(t, "Fine.")) // a run cancelled before it starts prints nothing
	v.out(2, verifierPass(t, "Fine."))
	v.pass()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	real := &daemon.Verifier{Daemon: v.d}
	v.r.Stages[state.Verifying] = commentingVerifier{Verifier: real, during: cancel}
	if err := v.d.Iterate(ctx); err == nil {
		t.Fatal("the pass the daemon stopped in gave no error")
	}
	if l := v.lease(); l.RunID != "" || pickStage(f, n) != "stage:verifying" {
		t.Fatalf("after the shutdown: lease %+v, at %s; want none, at verifying", l, pickStage(f, n))
	}
	v.r.Stages[state.Verifying] = real
	v.pass()
	if got := passComments(f, n); len(got) != 1 {
		t.Errorf("after the shutdown, the verdicts = %q; want one", got)
	}
}

// verifierNoResult leaves a run of task 1 under v's root with a lease and
// a clone but no result, as a daemon that died during the run would.
func verifierNoResult(t testing.TB, v *verdictRig, head string, seen int64) {
	t.Helper()
	verifierSaved(t, v.root, head, "", seen)
	if err := os.Remove(v.path("state", "runs", "saved.json")); err != nil {
		t.Fatal(err)
	}
}

// A restart that finds a verifier run's lease and no result counts a failed
// run: it writes nothing below the cap, and the next run is the head's
// next attempt. On the last, it's the hand-off, with the daemon's own words
// for why.
func TestVerifierRestartWithNoResult(t *testing.T) {
	t.Parallel()
	t.Run("below the cap", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		v := newVerdictRig(t, f, 2)
		head := v.head()
		n, _ := verifierTask(f, head)
		verifierNoResult(t, v, head, 1001)
		r := v.restart()
		r.out(1, verifierPass(t, "Fine."))
		r.pass()
		if l, cs := r.lease(), f.State().Issues[n-1].Comments; !l.Failed || l.Attempt != 1 || pickStage(f, n) != "stage:verifying" || len(cs) != 1 {
			t.Fatalf("after the restart: lease %+v, at %s, comments %+v; want one failed attempt, at verifying, and only the move", l, pickStage(f, n), cs)
		}
		r.pass()
		if l := r.leaseOf("1"); l.Attempt != 2 || len(passComments(f, n)) != 1 {
			t.Errorf("the next run's lease %+v, the verdicts %q; want attempt 2, and one", l, passComments(f, n))
		}
	})
	t.Run("the last of them", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		v := newVerdictRig(t, f, 1)
		head := v.head()
		n, move := verifierTask(f, head)
		verifierNoResult(t, v, head, move)
		r := v.restart()
		r.pass()
		want := verifierFailedHandoff(n, 1, verifierFailedQuoted("no valid result after a restart"), wholeRunOwner, move, move)
		if got := verifierNewest(f, n); got != want || pickStage(f, n) != "stage:needs-human" || r.runs() != 0 {
			t.Errorf("after the restart, at %s, with %d runs, the newest comment =\n%s\nwant needs-human, none, and\n%s", pickStage(f, n), r.runs(), got, want)
		}
	})
}

// A restart keeps the count, as the lease keeps other stages': each run
// failed before one is the head's next attempt after it, in the same round
// of attempts, the checks run again first, and the last one hands off.
func TestVerifierFailedRunCountAcrossRestarts(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, move := verifierTask(f, head)
	for k := 1; k <= 3; k++ {
		v.out(k, failOut)
	}
	v.passes(2)
	for k := 2; k <= 3; k++ {
		ran := v.checksRan()
		v = v.restart()
		v.passes(2)
		if l := v.leaseOf(strconv.Itoa(k)); v.runs() != k || l.Attempt != k || l.AttemptKey != "verifying-"+strconv.FormatInt(move, 10) || v.checksRan() != ran+1 {
			t.Fatalf("after restart %d: %d runs, lease %+v, the commands at %d; want attempt %d, and %d", k-1, v.runs(), l, v.checksRan(), k, ran+1)
		}
	}
	if got := pickStage(f, n); got != "stage:needs-human" {
		t.Errorf("the task is at %s, want needs-human", got)
	}
}

// A hand-off whose write fails isn't lost, and starts no run: the next
// pass that would run the verifier makes it instead, with the same words,
// the same pass after a restart once the checks run again, as the lease
// keeps why the last run failed.
func TestVerifierHandoffWriteFails(t *testing.T) {
	t.Parallel()
	for _, restart := range []bool{false, true} {
		t.Run("restart "+strconv.FormatBool(restart), func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 1)
			head := v.head()
			n, move := verifierTask(f, head)
			v.out(1, failOut)
			v.pass()
			f.FailWrite(len(f.Writes()) + 1)
			if err := v.d.Iterate(context.Background()); err == nil {
				t.Fatal("the pass whose hand-off failed gave no error")
			}
			if pickStage(f, n) != "stage:verifying" || v.runs() != 1 {
				t.Fatalf("after the failed write: at %s, %d runs; want verifying, one", pickStage(f, n), v.runs())
			}
			if restart {
				v = v.restart()
				v.pass() // the checks again
			}
			v.pass()
			want := verifierFailedHandoff(n, 1, verifierFailedQuoted(`run failed with subtype "error_max_turns"`), wholeRunOwner, move, move)
			if got := verifierNewest(f, n); got != want || pickStage(f, n) != "stage:needs-human" || v.runs() != 1 {
				t.Errorf("at %s, with %d runs, the newest comment =\n%s\nwant needs-human, one, and\n%s", pickStage(f, n), v.runs(), got, want)
			}
		})
	}
}

// Failed runs that reach a loop.max_attempts lowered since hand off in
// place of a run, counting the runs made, above the limit, and saying why
// the last failed, from what the daemon kept.
func TestVerifierFailedRunsLoweredLimit(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, move := verifierTask(f, head)
	v.out(1, failOut)
	v.out(2, failOut)
	v.passes(3)
	if got := pickStage(f, n); got != "stage:verifying" || v.runs() != 2 {
		t.Fatalf("after two failed runs: at %s, %d runs; want verifying, two", got, v.runs())
	}
	// Work doesn't load the config from main, as a pass does.
	v.d.Config.Loop.MaxAttempts = 1
	if err := v.work(n); err != nil {
		t.Fatal(err)
	}
	want := verifierFailedHandoff(n, 2, verifierFailedQuoted(`run failed with subtype "error_max_turns"`), wholeRunOwner, move, move)
	if got := verifierNewest(f, n); got != want || pickStage(f, n) != "stage:needs-human" || v.runs() != 2 {
		t.Errorf("at %s, with %d runs, the newest comment =\n%s\nwant needs-human, two, and\n%s", pickStage(f, n), v.runs(), got, want)
	}
}

// A result that fails to apply loop.max_attempts times is a failed run: a
// pass's comment that can't be written, kept for the next pass until then.
// With loop.max_attempts at 1 that's the last failed run, whose cause, a
// failed apply's, only the log has. At 2, it's a failed run below the cap,
// and the head gets its next run.
func TestVerifierResultFailsToApply(t *testing.T) {
	t.Parallel()
	t.Run("at the cap", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		v := newVerdictRig(t, f, 1)
		head := v.head()
		n, move := verifierTask(f, head)
		v.out(1, verifierPass(t, "Fine."))
		v.pass()
		f.FailWrite(len(f.Writes()) + 1)
		v.pass()
		want := verifierFailedHandoff(n, 1, verifierFailedLogged, wholeRunOwner, move, move)
		if got := verifierNewest(f, n); got != want || len(passComments(f, n)) != 0 {
			t.Errorf("the newest comment =\n%s\nwith %d verdicts; want none, and\n%s", got, len(passComments(f, n)), want)
		}
	})
	t.Run("below the cap", func(t *testing.T) {
		t.Parallel()
		f := newFake(t)
		v := newVerdictRig(t, f, 2)
		head := v.head()
		n, _ := verifierTask(f, head)
		v.out(1, verifierPass(t, "First."))
		v.out(2, verifierPass(t, "Second."))
		v.pass()
		for range 2 {
			f.FailWrite(len(f.Writes()) + 1)
			if err := v.d.Iterate(context.Background()); err != nil {
				t.Logf("the failed apply's pass: %v", err)
			}
		}
		if l := v.lease(); !l.Failed || l.Attempt != 1 || pickStage(f, n) != "stage:verifying" || v.runs() != 1 {
			t.Fatalf("after the second failed apply: lease %+v, at %s, %d runs; want one failed attempt, verifying, one run", l, pickStage(f, n), v.runs())
		}
		v.pass()
		if got := passComments(f, n); v.leaseOf("2").Attempt != 2 || len(got) != 1 || !strings.Contains(got[0], "Second.") {
			t.Errorf("run 2's lease %+v, the verdicts %q; want attempt 2, and run 2's", v.leaseOf("2"), got)
		}
	})
}

// A non-zero exit with the subtype that says the run gave up on its output
// is no readable verdict, but it saves no result. So when its first apply
// fails, the next pass finds none, and it's a failed run, not
// request_changes: nothing moves, none of the implementer's attempts goes,
// and the head gets another run. That's the safe side (SPEC §8.3).
func TestVerifierNoVerdictNotSaved(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, _ := verifierTask(f, head)
	v.out(1, `{"type":"result","subtype":"error_max_structured_output_retries","is_error":true,"result":""}`)
	if err := os.WriteFile(v.path("claude", "exit.1"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	v.out(2, verifierPass(t, "Fine."))
	v.pass()
	f.FailWrite(len(f.Writes()) + 1)
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose apply failed gave no error")
	}
	v.pass() // it counts the failed run, then gives the head its next one
	if got := passComments(f, n); v.leaseOf("2").Attempt != 2 || len(got) != 1 || pickStage(f, n) != "stage:verifying" || len(verifierMoves(f, n)) != 0 {
		t.Errorf("run 2's lease %+v, the verdicts %q, at %s, moves %+v; want attempt 2, one, still at verifying, and none",
			v.leaseOf("2"), got, pickStage(f, n), verifierMoves(f, n))
	}
}

// The hand-off ends as any does, through the whole run: /done sends the
// task to ready, a new round, the pick takes it again, and its run is the
// implementer's attempt 1 at the lowest effort, keyed by a new round, from
// the task's head. Its new head gets its own verifier runs, counted from 1.
func TestVerifierHandoffStartsARound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerifierRoundRig(t, f, 2)
	commits := ivResult(t, implementerOutput(t, "commits"))
	v.out(1, commits)
	v.out(2, failOut)
	v.out(3, failOut)
	v.out(4, commits)
	v.out(5, verifierPass(t, "Fine."))
	n := verifierRoundTask(f)
	v.passes(4)
	if got := pickStage(f, n); got != "stage:needs-human" || v.runs() != 3 {
		t.Fatalf("after two failed runs: at %s, %d runs; want needs-human, three", got, v.runs())
	}
	f.AddComment(n, wholeRunOwner, "/done")
	v.pass()
	v.checkRun(4, state.InProgress, 1, "low")
	if a, b := v.leaseOf("4").AttemptKey, v.leaseOf("1").AttemptKey; a == b {
		t.Errorf("run 4's attempt key = %s, run 1's too; want a new round", a)
	}
	if got := v.leaseOf("4").Start; got != v.trimmed("commit.1") {
		t.Errorf("run 4 starts from %s, want the task's head %s", got, v.trimmed("commit.1"))
	}
	v.passes(2)
	v.checkRun(5, state.Verifying, 1, "low")
	if got := passComments(f, n); len(got) != 1 {
		t.Errorf("the verdicts = %q; want one, on the new head", got)
	}
}

// verifierHandoffTask is newFake with issue 1 a task the owner filed, at
// needs-human after the hand-off for the verifier's failed runs, with an
// earlier hand-off before it: its move to in-progress, 1001, the
// implementer's hand-off, 1002, a 👍 on it that sent the task back to
// ready, 1003, its next move to in-progress, 1004, the implementer's move
// to verifying, 1005, and the verifier's hand-off, 1006. Then it runs more.
func verifierHandoffTask(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return handbackTask(func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, app, "<!-- issuedog:transition from=needs-human to=ready cmd=r77 seen=1002 -->")
		f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress event=p6 -->")
		move := f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head=abc -->")
		if id := f.AddComment(1, app, verifierFailedHandoff(1, 3, verifierFailedQuoted("run timed out after 1s"), "owner", move, move)); id != 1006 {
			t.Fatalf("the hand-off's comment ID = %d, want 1006", id)
		}
		more(t, f)
	})
}

// A trusted /done, or a trusted 👍 on the hand-off, sends the task back to
// ready, a new round. An untrusted /done or 👍 does nothing, and nor does a
// trusted 👍 on the earlier hand-off.
func TestVerifierHandoffEnds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		more func(t testing.TB, f *githubtest.Fake)
		want string // the move to ready's marker, or "" for none
	}{
		{"a trusted /done", doneBy("owner", 1007), "<!-- issuedog:transition from=needs-human to=ready cmd=1007 seen=1007 -->"},
		{"an untrusted /done", doneBy("eve", 1007), ""},
		{"a trusted 👍", func(t testing.TB, f *githubtest.Fake) {
			if id := f.AddReaction(1006, "owner", "+1"); id != 1007 {
				t.Fatalf("owner's 👍's ID = %d, want 1007", id)
			}
		}, "<!-- issuedog:transition from=needs-human to=ready cmd=r1007 seen=1006 -->"},
		{"an untrusted 👍", func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1006, "eve", "+1") }, ""},
		{"a trusted 👍 on the earlier hand-off", func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") }, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := verifierHandoffTask(tt.more)(t)
			iterate(t, handbackDaemon(t, f, newOrigin(t)))
			is := f.State().Issues[0]
			if tt.want == "" {
				if w := f.Writes(); len(w) != 0 || !slices.Equal(is.Labels, []string{"stage:needs-human"}) {
					t.Errorf("writes %+v, labels %q; want none, at needs-human", w, is.Labels)
				}
				return
			}
			if last := is.Comments[len(is.Comments)-1].Body; !slices.Equal(is.Labels, []string{"stage:ready"}) || !strings.HasSuffix(last, tt.want) {
				t.Errorf("labels %q, newest comment\n%s\nwant ready, on a move ending %s", is.Labels, last, tt.want)
			}
		})
	}
}

// While a task waits on the hand-off, the digest lists it as the verifier's
// runs failing, with its Goal and "Done when" copied from the comment, how
// to finish it and a link to the comment, and notify is assigned. Both end
// on the pass whose 👍 sends the task back to ready.
func TestVerifierHandoffItem(t *testing.T) {
	t.Parallel()
	f, o := verifierHandoffTask(func(testing.TB, *githubtest.Fake) {})(t), digestNotifyOrigin(t)
	d := digestDaemon(t, f, o, nil)
	d.Assign = true
	d.Step.Reactions = daemon.HandoffByReaction(handbackTrust)
	iterate(t, d)
	digestN := len(f.State().Issues)
	if want := []string{"#1 Hand-off p2 1"}; !slices.Equal(handbackListed(f, digestN), want) {
		t.Errorf("the digest lists %q; want %q", handbackListed(f, digestN), want)
	}
	body := f.State().Issues[digestN-1].Body
	for _, line := range []string{
		"- A hand-off waits for you: the verifier's runs on this head failed, and the hand-off says why.\n",
		"- Goal: Find out why the verifier's runs on this task fail, and fix that, so it can judge the task's head.\n",
		"- Done when: What made the verifier's runs fail is fixed.\n",
		"- When it's done, reply `/done` on the issue or react 👍 to the hand-off.\n",
		"- Answer here: https://github.com/o/r/issues/1#issuecomment-1006\n",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("the digest has no line %q:\n%s", line, body)
		}
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("the task's assignees = %q; want owner", got)
	}
	f.AddReaction(1006, "owner", "+1")
	iterate(t, d)
	if got := handbackListed(f, digestN); len(got) != 0 {
		t.Errorf("after the 👍, the digest lists %q; want no hand-off", got)
	}
	if got := assignees(f, 1); len(got) != 0 {
		t.Errorf("after the 👍, the task's assignees = %q; want none", got)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("after the 👍, the task's labels = %q; want ready", l)
	}
}

// A pass where nothing changed on a task waiting on the hand-off makes one
// read for it beyond the poll and the issue's own: the hand-off's
// reactions, a conditional GET answered 304.
func TestVerifierHandoffUnchanged(t *testing.T) {
	t.Parallel()
	f := verifierHandoffTask(func(testing.TB, *githubtest.Fake) {})(t)
	d := handbackDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	from := len(f.Requests())
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if n := handbackReads(f, from, 1006); n != 1 {
		t.Errorf("a pass where nothing changed read the hand-off's reactions %d times; want once, answered 304", n)
	}
}

// The hand-off's move, a crash at each of its writes, then a restart: the
// replay ends with the same writes (SPEC §5.2). The clean pass makes it
// from a run a restart found no result of, and a pass after a crash in it
// makes it instead of a run, once the checks pass the head again.
func TestVerifierHandoffReplay(t *testing.T) {
	t.Parallel()
	// Every setup shares one origin and head, as a replay compares comments
	// whose markers name the head.
	first := newVerdictRig(t, newFake(t), 1)
	head := first.head()
	rigs := map[*githubtest.Fake]*verdictRig{}
	githubtest.Replay(t, func(testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.SetPermission(wholeRunOwner, "admin")
		v := &verdictRig{rig: newRig(t, f, first.o, t.TempDir()), o: first.o, cfg: first.cfg, ran: first.ran}
		v.wire()
		fetchStartHead(t, v.rig, v.o, head)
		_, move := verifierTask(f, head)
		verifierNoResult(t, v, head, move)
		rigs[f] = v
		return f
	}, func(f *githubtest.Fake) error {
		rigs[f] = rigs[f].restart()
		rigs[f].d.Digest, rigs[f].d.Assign = false, false // only the hand-off's writes count here
		for range 2 {
			if err := rigs[f].d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
	for f, v := range rigs {
		want := "The verifier's 1 runs on this head failed"
		if body := verifierNewest(f, 1); !strings.HasPrefix(body, want) || pickStage(f, 1) != "stage:needs-human" || v.runs() != 0 {
			t.Errorf("after the replay, at %s, with %d runs, the newest comment =\n%s\nwant needs-human, none, on the hand-off", pickStage(f, 1), v.runs(), body)
		}
	}
}

// Only the Stage at verifying is an AfterChecks Stage, so no other stage's
// cap is the verifier's hand-off: triage's, the Unlabeled stage's, included.
func TestOnlyTheVerifierRunsAfterChecks(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	for at, st := range v.r.Stages {
		if _, ok := st.(daemon.AfterChecks); ok != (at == state.Verifying) {
			t.Errorf("the Stage at %s is AfterChecks: %t, want %t", at, ok, at == state.Verifying)
		}
	}
	if _, ok := v.r.Unlabeled.(daemon.AfterChecks); ok {
		t.Error("the Unlabeled stage is AfterChecks; want it not")
	}
}

// A Stage at verifying that isn't an AfterChecks Stage gets the kind=attempts
// state error at the cap, as any other stage does, with no hand-off: the
// cap's hand-off there is the verifier's alone.
func TestCapAtVerifyingIsTheVerifiers(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := f.AddIssue("owner", "Refuse an expired quote", "Refuse it.", "type:task")
	f.AddLabel(n, app, "stage:verifying")
	f.AddComment(n, app, "<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head=abc -->")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.r.Stages = map[state.Stage]daemon.Stage{state.Verifying: &fakeStage{pending: func(daemon.Input) bool { return true }}}
	g.claude(failOut, 1)
	for range 3 {
		iterate(t, g.d)
	}
	if got := errorMarkers(f, n); g.runs() != 3 || len(got) != 1 || !strings.Contains(got[0], "kind=attempts key=verifying-") || pickStage(f, n) != "stage:verifying" {
		t.Errorf("%d runs, errors %q, at %s; want three, one kind=attempts, still at verifying", g.runs(), got, pickStage(f, n))
	}
}
