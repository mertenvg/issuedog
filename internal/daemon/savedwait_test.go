package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/logr/v2"
)

// This file is #517: the saved result a restart left waits for a pass
// whose step 1 finishes on its issue (SPEC §8.2), also when a /hold on it
// waits for an ancestor's command, or its read found it gone; it's
// dropped, uncounted, when that pass's read found the issue held; and
// each wait is logged once.

// savedWaitAncestor files J, issue 1, the interview, under A, issue 2, at
// ready, with under root the lease and valid result of run saved on J,
// left by a restart. The owner's /hold on A comes before the owner's /hold
// on J. Step 1 visits J first, so J's /hold waits for A's
// (holdOrderWaits, SPEC §6.4's Holds stack). A's type is one the pipeline
// doesn't know, so the App rejects A's /hold, which holds nothing and
// writes nothing on J, and whose reply doesn't depend on J's stage, so a
// replay that handles J's /hold first writes the same.
func savedWaitAncestor(tb testing.TB, root string) *githubtest.Fake {
	f := newFake(tb)
	saveRun(tb, root, "saved", okOut, interview(f))
	a := fileAt(f, "ready", "A part.")
	f.SetType(a, "Weird")
	f.SetParent(1, a)
	f.AddComment(a, "owner", "/hold")
	f.AddComment(1, "owner", "/hold")
	return f
}

// savedWaitNothing reports whether f's issue 1 has nothing of run saved.
func savedWaitNothing(f *githubtest.Fake) bool {
	return !slices.ContainsFunc(f.State().Issues[0].Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, " run=saved ") })
}

// savedWaitAncestorPasses runs the two passes after the restart that
// savedWaitAncestor sets up, on a new rig on f with its state under root.
// The first leaves J's /hold waiting, and the saved result waiting with
// it, counted, and no error, as the wait is logged (Runs.Work); the second
// applies J's /hold, which drops the result. Neither runs claude. With
// clean, as on a first try, the wait must be counted once; a replay after
// a crash in the first pass may handle J's /hold, and drop the result,
// sooner.
func savedWaitAncestorPasses(t *testing.T, f *githubtest.Fake, o *origin, root string, clean bool) error {
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	if err := g.d.Iterate(context.Background()); err != nil {
		return fmt.Errorf("the first pass: %w", err)
	}
	l := g.lease()
	waits := l.RunID == "saved" && !l.Failed && l.ApplyFailures >= 1 && slices.Contains(g.leftovers(), "state/runs/saved.json")
	if clean && (!waits || l.ApplyFailures != 1) || !clean && l.RunID == "saved" && !waits {
		return fmt.Errorf("after the first pass: lease %+v, left %v; want the saved result waiting, counted", l, g.leftovers())
	}
	if !savedWaitNothing(f) {
		return fmt.Errorf("after the first pass: comments %+v; want nothing of run saved applied over J's waiting /hold", f.State().Issues[0].Comments)
	}
	if err := g.d.Iterate(context.Background()); err != nil {
		return fmt.Errorf("the second pass: %w", err)
	}
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:held"}) || !savedWaitNothing(f) {
		return fmt.Errorf("after the second pass: labels %v, comments %+v; want J's /hold applied, and nothing of run saved", is.Labels, is.Comments)
	}
	if left := g.leftovers(); len(left) != 0 || g.runs() != 0 {
		return fmt.Errorf("after the second pass: left %v after %d runs; want the saved result dropped, and no run", left, g.runs())
	}
	return nil
}

// TestSavedWaitAncestorHoldReplay: a saved result on J, whose /hold waits
// for the /hold posted before it on its parent A, isn't applied in that
// pass, which leaves the /hold unhandled (SPEC §8.2): it waits with its
// lease, and J's /hold, handled on the next pass, drops it. Both passes
// are safe to replay after a crash at any write. Then a pass reads the
// App's writes once, and the one after, with nothing changed, writes
// nothing and sends only conditional GETs that GitHub answers 304.
func TestSavedWaitAncestorHoldReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		root := t.TempDir()
		f := savedWaitAncestor(tb, root)
		roots[f] = root
		return f
	}, func(f *githubtest.Fake) error { return savedWaitAncestorPasses(t, f, o, roots[f], false) })

	root := t.TempDir()
	f := savedWaitAncestor(t, root)
	if err := savedWaitAncestorPasses(t, f, o, root, true); err != nil {
		t.Fatal(err)
	}
	if body := newestComment(f, 2); !strings.Contains(body, "reason=rejected") {
		t.Errorf("#2: newest comment %q; want A's /hold rejected", body)
	}
	g := newRig(t, f, o, root)
	g.pass()
	from, writes := len(f.Requests()), len(f.Writes())
	g.pass()
	reqs := f.Requests()[from:]
	if len(reqs) == 0 {
		t.Fatal("the unchanged pass sent no requests")
	}
	for _, r := range reqs {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if w := f.Writes(); len(w) != writes {
		t.Errorf("the unchanged pass wrote %+v; want nothing", w[writes:])
	}
}

// TestSavedWaitGoneReadUncounted: a read that finds the issue a restart
// left a saved result on gone, once, which may be wrong, runs no step 1
// on it, so the /hold posted on it isn't handled. That pass doesn't apply
// the result, though apply's own read would find the issue: it waits with
// its lease, uncounted, as the next read drops the issue or runs step 1 on
// it, and no other run starts, though #2 has work pending (SPEC §8.2). The
// next pass applies the /hold, which drops the result.
func TestSavedWaitGoneReadUncounted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := phase1PinHoldSaved(t, root)
	interview(f)
	g := newRig(t, f, newOrigin(t), root)
	g.claude(okOut, 0)
	g.stage.pending = always
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1" }, http.StatusNotFound)
	g.pass()
	if l := g.lease(); l.RunID != "saved" || l.Failed || l.ApplyFailures != 0 || !slices.Contains(g.leftovers(), "state/runs/saved.json") || g.runs() != 0 {
		t.Fatalf("lease %+v, left %v after %d runs; want the saved result kept with its lease, uncounted, and no run", l, g.leftovers(), g.runs())
	}
	if !phase1PinHoldUnsaved(f) {
		t.Fatalf("issue %+v; want nothing of the /hold or the saved result yet", f.State().Issues[0])
	}
	g.pass()
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:held"}) || !savedWaitNothing(f) {
		t.Fatalf("labels %v, comments %+v; want the /hold applied, and nothing of the saved result", is.Labels, is.Comments)
	}
	if l := g.lease(); l.RunID == "saved" {
		t.Errorf("lease %+v; want run saved's result dropped", l)
	}
}

// TestSavedWaitHeldDropped: a saved result a restart left on an issue
// that's held, whose step 1 fails, here on the owner's /resume's move, is
// dropped, uncounted, with nothing of it written, as on startup (SPEC
// §8.2): every way out of the hold writes a move above the run's seen, so
// apply would drop it, whatever step 1 left unhandled. So it's no failed
// attempt, even at loop.max_attempts 1, and no state error. The next pass
// applies the /resume.
func TestSavedWaitHeldDropped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := newFake(t)
	answer := interview(f)
	f.AddComment(1, "owner", "/hold")
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nloop:\n  max_attempts: 1\n")
	newRig(t, f, o, root).pass()
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("labels %v; want the /hold applied", l)
	}
	saveRun(t, root, "saved", okOut, answer)
	f.AddComment(1, "owner", "/resume")
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusInternalServerError)
	if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the pass = %v; want step 1's 500", err)
	}
	if l, left := g.lease(), g.leftovers(); l.RunID != "" || len(left) != 0 || g.runs() != 0 {
		t.Fatalf("lease %+v, left %v after %d runs; want the saved result dropped, uncounted, and no run", l, left, g.runs())
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || !savedWaitNothing(f) || len(errorMarkers(f, 1)) != 0 {
		t.Fatalf("labels %v, comments %+v; want the issue held, nothing of the saved result, and no state error", l, f.State().Issues[0].Comments)
	}
	g.pass()
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) || !savedWaitNothing(f) || g.runs() != 0 {
		t.Errorf("labels %v after %d runs; want the /resume applied, nothing of the saved result, and no run", l, g.runs())
	}
}

// TestSavedWaitLoggedOnce: a saved result's wait for a pass whose step 1
// fails on its issue is logged once, as a warning, and isn't the pass's
// error too: that's step 1's own.
func TestSavedWaitLoggedOnce(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	root := t.TempDir()
	f := phase1PinHoldSaved(t, root)
	g := newRig(t, f, newOrigin(t), root)
	g.claude(okOut, 0)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusInternalServerError)
	var warnings bytes.Buffer
	stop := logr.AddWriter(&warnings, logr.WithFilter(logr.W))
	err := g.d.Iterate(context.Background())
	logr.Wait()
	stop()
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "didn't finish") {
		t.Errorf("the pass = %v; want step 1's 500, and not the wait", err)
	}
	if got := warnings.String(); strings.Count(got, "step 1 didn't finish on #1") != 1 || !strings.Contains(got, "applying run saved on #1 failed, 1 of 3 times") {
		t.Errorf("the pass's warnings = %q; want the wait once", got)
	}
	if l := g.lease(); l.RunID != "saved" || l.ApplyFailures != 1 {
		t.Errorf("lease %+v; want the saved result waiting, counted once", l)
	}
}

// TestSavedWaitLeaseWriteFails: a wait whose count can't be written to the
// lease is the pass's error, and no other run starts.
func TestSavedWaitLeaseWriteFails(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := savedWaitAncestor(t, root)
	interview(f)
	g := newRig(t, f, newOrigin(t), root)
	g.claude(okOut, 0)
	g.stage.pending = always
	if err := os.Chmod(g.path("state"), 0o500); err != nil {
		t.Fatal(err)
	}
	err := g.d.Iterate(context.Background())
	if err := os.Chmod(g.path("state"), 0o700); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, os.ErrPermission) || !strings.Contains(err.Error(), "keep the lease of run saved on #1") {
		t.Errorf("the pass = %v; want the failure to keep the lease", err)
	}
	if l := g.lease(); l.RunID != "saved" || l.ApplyFailures != 0 || g.runs() != 0 || !savedWaitNothing(f) {
		t.Errorf("lease %+v after %d runs; want the lease as it was, no run, and nothing of run saved applied", l, g.runs())
	}
}
