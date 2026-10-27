package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// TestPhase1PinHoldWhileGateShut pins what epic #1's verifier found
// untested (#228): /hold while a usage limit shuts the gate. Stage work
// waits, but step 1 doesn't (SPEC §3.3), so the /hold applies in the pass
// it's posted in, with its 👍, and claude never runs. Once the gate opens,
// the held issue still gets no run, though its stage has work pending, and
// /resume lets it run again. A run's saved result left by a restart is
// dropped, not applied, once the /hold in the same pass has held the issue.
func TestPhase1PinHoldWhileGateShut(t *testing.T) {
	t.Parallel()
	for name, saved := range map[string]bool{"no lease": false, "a saved result": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			answer := interview(f)
			root := t.TempDir()
			if saved {
				saveRun(t, root, "saved", okOut, answer)
			}
			g := newRig(t, f, newOrigin(t), root)
			g.claude(okOut, 0)
			g.stage.pending = always
			reset := githubtest.Start.Add(time.Hour)
			if !g.r.Gate.Observe(g.now, true, fmt.Sprintf("usage limit reached|%d", reset.Unix())) {
				t.Fatal("the gate didn't shut")
			}
			hold := f.AddComment(1, "owner", "/hold")
			g.pass()
			is := f.State().Issues[0]
			if !slices.Equal(is.Labels, []string{"stage:held"}) || g.runs() != 0 {
				t.Fatalf("labels %v after %d runs; want the /hold applied while the gate is shut, and no run", is.Labels, g.runs())
			}
			if i := slices.IndexFunc(is.Comments, func(c githubtest.CommentState) bool { return c.Body == "/hold" }); i < 0 {
				t.Errorf("comments %+v; want the /hold", is.Comments)
			} else if got := is.Comments[i].Reactions; !slices.Equal(got, []string{app + " +1"}) {
				t.Errorf("reactions to the /hold = %v; want the App's 👍", got)
			}
			if last := newestComment(f, 1); !strings.Contains(last, fmt.Sprintf(" to=held at=clarifying reason=owner cmd=%d ", hold)) {
				t.Errorf("newest comment %q; want the hold's move", last)
			}
			if slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return c.Body == specWritten("saved", answer) }) ||
				len(g.leftovers()) != 0 {
				t.Errorf("comments %+v, left %v; want no saved result applied to the held issue, and nothing left", is.Comments, g.leftovers())
			}

			g.now = reset.Add(time.Second)
			writes := len(f.Writes())
			for range 2 {
				g.pass()
			}
			if g.runs() != 0 || len(f.Writes()) != writes {
				t.Fatalf("with the gate open: %d runs, writes %+v; want no run on the held issue, and no write", g.runs(), f.Writes()[writes:])
			}

			f.AddComment(1, "owner", "/resume")
			for range 2 {
				g.pass()
			}
			if labels := f.State().Issues[0].Labels; g.runs() != 1 || !slices.Equal(labels, []string{"stage:spec-review"}) {
				t.Errorf("after /resume: %d runs, labels %v; want one run, applied", g.runs(), labels)
			}
		})
	}
}

// phase1PinHoldSaved files the interview, issue 1, with owner's /hold on
// it, and under root the lease and valid result of run saved on it, left
// by a restart.
func phase1PinHoldSaved(tb testing.TB, root string) *githubtest.Fake {
	f := newFake(tb)
	saveRun(tb, root, "saved", okOut, interview(f))
	f.AddComment(1, "owner", "/hold")
	return f
}

// phase1PinHoldRig is a new rig on f, as after a restart, with its state
// under root and its gate shut by a usage limit, whose claude would
// succeed.
func phase1PinHoldRig(t *testing.T, f *githubtest.Fake, o *origin, root string) (*rig, error) {
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	g.stage.pending = always
	if !g.r.Gate.Observe(g.now, true, fmt.Sprintf("usage limit reached|%d", githubtest.Start.Add(time.Hour).Unix())) {
		return nil, errors.New("the gate didn't shut")
	}
	return g, nil
}

// TestPhase1PinHoldDropsSavedReplay: a pass that applies a /hold while
// the gate is shut, and drops the saved result a restart left on the same
// issue, is safe to replay after a crash at any write. The /hold and the
// drop are each replayed on their own (TestIterateHoldReplay,
// TestDiscDuringRunReplay); here they share the pass. Each pass ends with
// the result dropped, not applied, and nothing of the run left. After the
// clean pass, the next reads the App's writes once, and the one after
// that, with nothing changed, writes nothing and sends only conditional
// GETs that GitHub answers 304.
func TestPhase1PinHoldDropsSavedReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		root := t.TempDir()
		f := phase1PinHoldSaved(tb, root)
		roots[f] = root
		return f
	}, func(f *githubtest.Fake) error {
		g, err := phase1PinHoldRig(t, f, o, roots[f])
		if err != nil {
			return err
		}
		if err := g.d.Iterate(context.Background()); err != nil {
			return fmt.Errorf("the pass: %w", err)
		}
		if left := g.leftovers(); len(left) != 0 || g.runs() != 0 {
			return fmt.Errorf("the pass left %v after %d runs; want the saved result dropped, and no run", left, g.runs())
		}
		return nil
	})

	root := t.TempDir()
	f := phase1PinHoldSaved(t, root)
	g, err := phase1PinHoldRig(t, f, o, root)
	if err != nil {
		t.Fatal(err)
	}
	g.pass()
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:held"}) ||
		slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, " run=saved ") }) {
		t.Fatalf("labels %v, comments %+v; want the /hold applied, and nothing of the saved result", is.Labels, is.Comments)
	}
	if left := g.leftovers(); len(left) != 0 || g.runs() != 0 {
		t.Fatalf("left %v after %d runs; want the saved result dropped, and no run", left, g.runs())
	}
	g.pass()
	from, writes := len(f.Requests()), len(f.Writes())
	g.pass()
	reqs := f.Requests()[from:]
	if len(reqs) == 0 {
		t.Fatal("the unchanged pass sent no requests")
	}
	for _, r := range reqs {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if w := f.Writes(); len(w) != writes {
		t.Errorf("the unchanged pass wrote %+v; want nothing", w[writes:])
	}
}

// phase1PinHoldUnsaved reports whether issue 1 of f has neither the hold
// nor anything of the saved result: it's at clarifying, as before the
// pass, with no comment from run saved.
func phase1PinHoldUnsaved(f *githubtest.Fake) bool {
	is := f.State().Issues[0]
	return slices.Equal(is.Labels, []string{"stage:clarifying"}) &&
		!slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, " run=saved ") })
}

// TestPhase1PinHoldReadFailKeepsSaved: a failed read of the issue a
// restart left a saved result on skips step 1 on it, so the /hold posted
// on it isn't handled. That pass doesn't apply the result: it waits with
// its lease, counted as a failure to apply (SPEC §8.2). The next pass
// applies the /hold, which drops the result.
func TestPhase1PinHoldReadFailKeepsSaved(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := phase1PinHoldSaved(t, root)
	g := newRig(t, f, newOrigin(t), root)
	g.claude(okOut, 0)
	g.stage.pending = always
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1" }, http.StatusInternalServerError)
	if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the pass whose read failed = %v; want the read's 500", err)
	}
	if l := g.lease(); l.RunID != "saved" || l.Failed || l.ApplyFailures != 1 || !slices.Contains(g.leftovers(), "state/runs/saved.json") || g.runs() != 0 {
		t.Fatalf("lease %+v, left %v after %d runs; want the saved result kept with its lease, counted once, and no run", l, g.leftovers(), g.runs())
	}
	if !phase1PinHoldUnsaved(f) {
		t.Fatalf("issue %+v; want nothing of the /hold or the saved result yet", f.State().Issues[0])
	}
	g.pass()
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:held"}) ||
		slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, " run=saved ") }) {
		t.Fatalf("labels %v, comments %+v; want the /hold applied, and nothing of the saved result", is.Labels, is.Comments)
	}
	if left := g.leftovers(); len(left) != 0 || g.runs() != 0 {
		t.Fatalf("left %v after %d runs; want the saved result dropped, and no run", left, g.runs())
	}
}

// TestPhase1PinHoldFailingStep1Bounded: a saved result on an issue whose
// step 1 keeps failing, here on the /hold's comment, waits with its lease,
// and no other run starts, but each wait counts as a failure to apply
// (SPEC §8.2). So after loop.max_attempts passes the run is a failed
// attempt, its result dropped, not applied, and stage work goes on: the
// next issue runs in the same pass.
func TestPhase1PinHoldFailingStep1Bounded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := phase1PinHoldSaved(t, root)
	answer := interview(f)
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nloop:\n  max_attempts: 2\n")
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	g.stage.pending = always
	pass := func() error {
		f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusInternalServerError)
		return g.d.Iterate(context.Background())
	}
	if err := pass(); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the first pass = %v; want step 1's 500", err)
	}
	if l := g.lease(); l.RunID != "saved" || l.Failed || l.ApplyFailures != 1 || !slices.Contains(g.leftovers(), "state/runs/saved.json") || g.runs() != 0 {
		t.Fatalf("after the first pass: lease %+v, left %v after %d runs; want the saved result waiting, counted once, and no run on #2",
			l, g.leftovers(), g.runs())
	}
	if !phase1PinHoldUnsaved(f) {
		t.Fatalf("issue %+v after the first pass; want nothing of the /hold or the saved result", f.State().Issues[0])
	}
	if err := pass(); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the second pass = %v; want step 1's 500", err)
	}
	if left := g.leftovers(); len(left) != 0 || g.runs() != 1 || !specApplied(f, 2, "run1", answer) {
		t.Fatalf("after the second pass: left %v after %d runs; want run saved a failed attempt, nothing of it left, and run1 applied on #2",
			left, g.runs())
	}
	if !phase1PinHoldUnsaved(f) || len(errorMarkers(f, 1)) != 0 {
		t.Errorf("issue %+v after the second pass; want nothing of the saved result, and no state error", f.State().Issues[0])
	}
}
