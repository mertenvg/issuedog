package daemon_test

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// These tests pin what epic #1's verifier found untested (#228): the real
// Interviewer redoing a run whose result was lost or invalid. A result
// that's lost, isn't valid against the interviewer's schema, or that its
// Checker rejects writes nothing and is one failed attempt, so the
// Interviewer runs again on the same issue, having seen the same comment,
// and posts the round that run gives.

// phase1PinRedone checks that issue n's newest comment is round 1 of 2
// questions, posted by run, having seen comment entry, that it's the only
// comment the redo added to before, that claude ran runs times, and that
// no lease, result or clone is left, nor any state error posted.
func phase1PinRedone(t *testing.T, g *rig, n, before int, run string, entry int64, runs int) {
	t.Helper()
	if got, want := newestComment(g.f, n), ivRoundText(1, 2)+"\n\n"+ivRoundMarker(1, 2, run, entry); got != want {
		t.Errorf("newest comment =\n%s\nwant round 1, by %s:\n%s", got, run, want)
	}
	if got := len(g.f.State().Issues[n-1].Comments); got != before+1 {
		t.Errorf("%d comments posted; want only the round", got-before)
	}
	if g.runs() != runs || len(g.leftovers()) != 0 || len(errorMarkers(g.f, n)) != 0 {
		t.Errorf("%d runs, left %v, errors %q; want %d runs, nothing left, and no state error", g.runs(), g.leftovers(), errorMarkers(g.f, n), runs)
	}
}

// TestPhase1PinInterviewerRedoAfterRestart: a restart finds the
// Interviewer's lease with its result lost, invalid or rejected by its
// Checker. The run is deleted, as one failed attempt, and the Interviewer
// runs again: at once for a result lost or invalid; and on the next pass
// for one its Checker rejects, as the pass that judged a saved result
// gives its issue no stage work (Runs.Work).
func TestPhase1PinInterviewerRedoAfterRestart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		out    string
		passes int
	}{
		"lost":     {"", 1},
		"invalid":  {invalidOut, 1},                  // structured_output isn't the interviewer's
		"rejected": {ivResult(t, ivRound(8, "")), 2}, // valid, but over max_questions_per_round
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, entry := ivFiled(f, "epic", false, "from=intake to=clarifying event=5")
			root := t.TempDir()
			saveRun(t, root, name, tc.out, entry)
			g := newRig(t, f, newOrigin(t), root)
			g.interviewer()
			g.claude(ivResult(t, ivRound(2, "")), 0)
			before, writes := len(f.State().Issues[n-1].Comments), len(f.Writes())
			for pass := 1; pass <= tc.passes; pass++ {
				if pass > 1 {
					if l := g.lease(); len(f.Writes()) != writes || g.runs() != 0 || !l.Failed || l.Attempt != 1 {
						t.Fatalf("after pass %d: writes %+v, %d runs, lease %+v; want no write, no run, and one failed attempt",
							pass-1, f.Writes()[writes:], g.runs(), l)
					}
				}
				if err := g.work(n); err != nil {
					t.Fatal(err)
				}
			}
			phase1PinRedone(t, g, n, before, "run1", entry, 1)
		})
	}
}

// TestPhase1PinInterviewerRedoReplay: the redo after a lost result is safe
// to replay after a crash at any write.
func TestPhase1PinInterviewerRedoReplay(t *testing.T) {
	t.Parallel()
	replayRuns(t, func(t testing.TB, f *githubtest.Fake, root string) {
		_, entry := ivFiled(f, "epic", false, "from=intake to=clarifying event=5")
		saveRun(t, root, "lost", "", entry)
	}, func(g *rig) {
		g.interviewer()
		g.claude(ivResult(g.t, ivRound(2, "")), 0)
	})
}

// TestPhase1PinInterviewerRedoAfterBadRun: the Interviewer's run gives a
// result that isn't valid, or that its Checker rejects. The pass writes
// nothing and records one failed attempt, and the next pass runs the
// Interviewer again, whose round is posted.
func TestPhase1PinInterviewerRedoAfterBadRun(t *testing.T) {
	t.Parallel()
	for name, out := range map[string]string{
		"invalid":  invalidOut,
		"rejected": ivResult(t, ivRound(8, "")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, entry := ivFiled(f, "epic", false, "from=intake to=clarifying event=5")
			g := ivRig(t, f)
			g.claude(out, 0)
			before, writes := len(f.State().Issues[n-1].Comments), len(f.Writes())
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			if l := g.lease(); len(f.Writes()) != writes || g.runs() != 1 || !l.Failed || l.Attempt != 1 {
				t.Fatalf("after the bad run: writes %+v, %d runs, lease %+v; want no write, one run, and one failed attempt",
					f.Writes()[writes:], g.runs(), l)
			}
			g.claude(ivResult(t, ivRound(2, "")), 0)
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			phase1PinRedone(t, g, n, before, "run2", entry, 2)
		})
	}
}
