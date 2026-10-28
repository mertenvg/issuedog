package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/logr/v2"
)

// This file is #517: the saved result a restart left waits for a pass
// whose step 1 finishes on its issue (SPEC §8.2), also when a /hold on it
// waits for an ancestor's command, or its read found it gone; it's
// dropped, uncounted, when the issue's last snapshot shows it held; and
// each wait is logged once. Then #593: an ancestor of a type the pipeline
// knows, and a parent's failed visit that wrote to the issue. Then #634:
// a parent's visit that failed, or waits for its next pass, before its
// /hold reached the issue.

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
	savedWaitUnchanged(t, f, o, root)
}

// savedWaitUnchanged runs a pass on a new rig on f, with its state under
// root, which reads the App's writes once, and then one with nothing
// changed, which must write nothing and send only conditional GETs that
// GitHub answers 304.
func savedWaitUnchanged(t *testing.T, f *githubtest.Fake, o *origin, root string) {
	t.Helper()
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

// savedWaitOrdinaryAncestor is savedWaitAncestor with A a task, a type
// the pipeline knows, so the App applies A's /hold, which holds A and J.
func savedWaitOrdinaryAncestor(tb testing.TB, root string) *githubtest.Fake {
	f := newFake(tb)
	saveRun(tb, root, "saved", okOut, interview(f))
	a := fileAt(f, "ready", "A part.")
	f.SetType(a, "Task")
	f.SetParent(1, a)
	f.AddComment(a, "owner", "/hold")
	f.AddComment(1, "owner", "/hold")
	return f
}

// TestSavedWaitOrdinaryAncestorHoldReplay is TestSavedWaitAncestorHoldReplay
// with an ancestor of a type the pipeline knows. J's /hold waits for A's,
// which holds A and J, so the saved result on J isn't applied in that
// pass (SPEC §8.2), and waits with its lease. The next pass applies J's
// /hold on J, held already, which writes no marker there (SPEC §6.4), and
// drops the result. Both passes are safe to replay after a crash at any
// write, and an unchanged pass sends only conditional GETs answered 304.
func TestSavedWaitOrdinaryAncestorHoldReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		root := t.TempDir()
		f := savedWaitOrdinaryAncestor(tb, root)
		roots[f] = root
		return f
	}, func(f *githubtest.Fake) error { return savedWaitAncestorPasses(t, f, o, roots[f], false) })

	root := t.TempDir()
	f := savedWaitOrdinaryAncestor(t, root)
	if err := savedWaitAncestorPasses(t, f, o, root, true); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[1].Labels; !slices.Contains(l, "stage:held") {
		t.Errorf("#2: labels %v; want A's /hold applied", l)
	}
	if !slices.ContainsFunc(f.State().Issues[0].Comments, func(c githubtest.CommentState) bool {
		return c.Body == "/hold" && slices.Contains(c.Reactions, app+" +1")
	}) {
		t.Errorf("#1: comments %+v; want J's /hold 👍'd", f.State().Issues[0].Comments)
	}
	savedWaitUnchanged(t, f, o, root)
}

// TestSavedWaitTouchedByFailedParent: a saved result on J, whose own
// step 1 succeeds, isn't applied in a pass whose visit of its parent A
// failed after it began writing to J, here on the move of A's /hold
// there: that /hold is half done, and would drop the result. It waits
// with its lease, counted as a failure to apply (SPEC §8.2), and the next
// pass applies A's /hold, which drops it. An unchanged pass after sends
// only conditional GETs answered 304.
func TestSavedWaitTouchedByFailedParent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	f := newFake(t)
	saveRun(t, root, "saved", okOut, interview(f))
	a := fileAt(f, "ready", "A part.")
	f.SetType(a, "Task")
	f.SetParent(1, a)
	f.AddComment(a, "owner", "/hold")
	o := newOrigin(t)
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusInternalServerError)
	if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the pass = %v; want A's step 1's 500", err)
	}
	if l := g.lease(); l.RunID != "saved" || l.Failed || l.ApplyFailures != 1 || !slices.Contains(g.leftovers(), "state/runs/saved.json") || g.runs() != 0 {
		t.Fatalf("lease %+v, left %v after %d runs; want the saved result waiting, counted once, and no run", l, g.leftovers(), g.runs())
	}
	if !savedWaitNothing(f) {
		t.Fatalf("#1: comments %+v; want nothing of run saved applied over A's half-done /hold", f.State().Issues[0].Comments)
	}
	g.pass()
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || !savedWaitNothing(f) {
		t.Errorf("#1: labels %v, comments %+v; want A's /hold applied, and nothing of run saved", l, f.State().Issues[0].Comments)
	}
	if left := g.leftovers(); len(left) != 0 || g.runs() != 0 {
		t.Errorf("left %v after %d runs; want the saved result dropped, and no run", left, g.runs())
	}
	savedWaitUnchanged(t, f, o, root)
}

// savedWaitFamily files J2, issue 1, the interview, and J1, issue 3, a
// task at ready, under A, issue 2, a task at ready, J1 linked first, with
// under root the lease and valid result of run saved on J2, left by a
// restart, and the owner's /hold on A. A's /hold walks J1 before J2, so a
// visit of A that stops on J1 doesn't reach J2. A is visited before J1.
// j1, if set, then changes J1.
func savedWaitFamily(tb testing.TB, root string, j1 func(f *githubtest.Fake, n int)) *githubtest.Fake {
	f := newFake(tb)
	saveRun(tb, root, "saved", okOut, interview(f))
	a := fileAt(f, "ready", "A part.")
	f.SetType(a, "Task")
	n := fileAt(f, "ready", "J1 part.")
	f.SetType(n, "Task")
	f.SetParent(n, a)
	f.SetParent(1, a)
	f.AddComment(a, "owner", "/hold")
	if j1 != nil {
		j1(f, n)
	}
	return f
}

// savedWaitFamilyPasses runs two passes on a new rig on f, with its state
// under root, and f.Advance(between) between them. The second must leave
// J2 held by A's /hold, with nothing of run saved applied, its result
// dropped, and no run. With waits, as on a first try whose first pass
// stops on J1, that pass must leave the saved result waiting, counted
// once, and nothing of it applied; a replay after a crash may get past J1
// sooner.
func savedWaitFamilyPasses(t *testing.T, f *githubtest.Fake, o *origin, root string, between time.Duration, waits bool) error {
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	if err := g.d.Iterate(context.Background()); err != nil {
		return fmt.Errorf("the first pass: %w", err)
	}
	if l := g.lease(); waits && (l.RunID != "saved" || l.Failed || l.ApplyFailures != 1 || !slices.Contains(g.leftovers(), "state/runs/saved.json")) {
		return fmt.Errorf("after the first pass: lease %+v, left %v; want the saved result waiting, counted once", l, g.leftovers())
	}
	if waits && !savedWaitNothing(f) {
		return fmt.Errorf("after the first pass: comments %+v; want nothing of run saved applied over A's unfinished /hold", f.State().Issues[0].Comments)
	}
	f.Advance(between)
	if err := g.d.Iterate(context.Background()); err != nil {
		return fmt.Errorf("the second pass: %w", err)
	}
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:held"}) || !savedWaitNothing(f) {
		return fmt.Errorf("after the second pass: labels %v, comments %+v; want A's /hold applied, and nothing of run saved", is.Labels, is.Comments)
	}
	if left := g.leftovers(); len(left) != 0 || g.runs() != 0 {
		return fmt.Errorf("after the second pass: left %v after %d runs; want the saved result dropped, and no run", left, g.runs())
	}
	return nil
}

// savedWaitFamilyReplay is githubtest.Replay of savedWaitFamilyPasses on
// savedWaitFamily(j1), each try with a root of its own. A replay may get
// past J1 on its first pass, so none checks that pass: a result applied a
// pass early shows in the second's.
func savedWaitFamilyReplay(t *testing.T, o *origin, j1 func(f *githubtest.Fake, n int), between time.Duration) {
	t.Helper()
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		root := t.TempDir()
		f := savedWaitFamily(tb, root, j1)
		roots[f] = root
		return f
	}, func(f *githubtest.Fake) error { return savedWaitFamilyPasses(t, f, o, roots[f], between, false) })
}

// TestSavedWaitUnreachedByFailedParentReplay: a saved result on J2, whose
// own step 1 succeeds, isn't applied in a pass whose visit of its parent
// A failed on J1, before A's /hold reached J2: that /hold would write to
// J2 too, and would drop the result (SPEC §8.2). The Replay crashes A's
// visit at each write, J1's among them, and each crash must end as the
// clean run does, the result dropped, not applied a pass early. Then a
// failure on J1's move comment: the result waits with its lease, counted
// as a failure to apply, and the next pass applies A's /hold, which drops
// it. An unchanged pass after sends only conditional GETs answered 304.
func TestSavedWaitUnreachedByFailedParentReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	savedWaitFamilyReplay(t, o, nil, 0)

	root := t.TempDir()
	f := savedWaitFamily(t, root, nil)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/3/comments" }, http.StatusInternalServerError)
	g := newRig(t, f, o, root)
	g.claude(okOut, 0)
	if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the pass = %v; want A's step 1's 500 on J1", err)
	}
	if l := g.lease(); l.RunID != "saved" || l.Failed || l.ApplyFailures != 1 || !slices.Contains(g.leftovers(), "state/runs/saved.json") || g.runs() != 0 {
		t.Fatalf("lease %+v, left %v after %d runs; want the saved result waiting, counted once, and no run", l, g.leftovers(), g.runs())
	}
	if !savedWaitNothing(f) {
		t.Fatalf("#1: comments %+v; want nothing of run saved applied over A's half-done /hold", f.State().Issues[0].Comments)
	}
	g.pass()
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) || !savedWaitNothing(f) {
		t.Errorf("#1: labels %v, comments %+v; want A's /hold applied, and nothing of run saved", l, f.State().Issues[0].Comments)
	}
	if left := g.leftovers(); len(left) != 0 || g.runs() != 0 {
		t.Errorf("left %v after %d runs; want the saved result dropped, and no run", left, g.runs())
	}
	savedWaitUnchanged(t, f, o, root)
}

// TestSavedWaitUnreachedByHeldBackParentReplay is
// TestSavedWaitUnreachedByFailedParentReplay with A's visit stopped on J1
// by a label a rename put on J1, which no event explains yet: J1's labels
// are held back (ErrLabelsHeld), so A's visit waits for its next pass,
// with A's /hold unfinished, though that's no error. The saved result on
// J2 waits with its lease, counted, until a pass after github.HoldBound,
// which leaves the label out and applies A's /hold, which drops it. Both
// passes are safe to replay after a crash at any write, and an unchanged
// pass sends only conditional GETs answered 304.
func TestSavedWaitUnreachedByHeldBackParentReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	j1 := func(f *githubtest.Fake, n int) {
		f.AddLabel(n, "mallory", "foo") // unguarded, so it stands
		f.RenameLabel("foo", "priority:p0")
	}
	savedWaitFamilyReplay(t, o, j1, github.HoldBound)

	root := t.TempDir()
	f := savedWaitFamily(t, root, j1)
	if err := savedWaitFamilyPasses(t, f, o, root, github.HoldBound, true); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[2].Labels; !slices.Contains(l, "stage:held") || !slices.Contains(l, "priority:p0") {
		t.Errorf("#3: labels %v; want J1 held, with priority:p0 left out but still on", l)
	}
	// Until it has stayed unexplained for github.UnexplainedWait, which a
	// rig's memory counts from its first pass, and is removed, the label
	// left out is checked again on each pass.
	g := newRig(t, f, o, root)
	g.pass()
	f.Advance(github.UnexplainedWait)
	g.pass()
	if l := f.State().Issues[2].Labels; slices.Contains(l, "priority:p0") {
		t.Errorf("#3: labels %v after github.UnexplainedWait; want priority:p0 removed", l)
	}
	savedWaitUnchanged(t, f, o, root)
}

// TestSavedWaitUnreachedByClosedAgainParentReplay is
// TestSavedWaitUnreachedByFailedParentReplay with A's visit stopped on J1
// by mallory's reopen of J1, completed, which A's /hold judges first, so
// it closes J1 again (closingErrClosedAgain): A's visit waits for its next
// pass, with A's /hold unfinished, though that's no error. The saved
// result on J2 waits with its lease, counted, and the next pass applies
// A's /hold, which leaves the closed J1 alone and drops the result. Both
// passes are safe to replay after a crash at any write, and an unchanged
// pass sends only conditional GETs answered 304.
func TestSavedWaitUnreachedByClosedAgainParentReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	j1 := func(f *githubtest.Fake, n int) {
		f.CloseBy(n, "owner")
		f.AddLabel(n, app, "stage:completed")
		f.RemoveLabel(n, app, "stage:ready")
		f.ReopenBy(n, "mallory")
	}
	savedWaitFamilyReplay(t, o, j1, 0)

	root := t.TempDir()
	f := savedWaitFamily(t, root, j1)
	if err := savedWaitFamilyPasses(t, f, o, root, 0, true); err != nil {
		t.Fatal(err)
	}
	if is := f.State().Issues[2]; !is.Closed || !slices.Equal(is.Labels, []string{"stage:completed"}) {
		t.Errorf("#3: closed %v, labels %v; want J1 closed again, still completed", is.Closed, is.Labels)
	}
	savedWaitUnchanged(t, f, o, root)
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
	// Not parallel: it reads the process-wide logr output (logtest.Capture).
	root := t.TempDir()
	f := phase1PinHoldSaved(t, root)
	g := newRig(t, f, newOrigin(t), root)
	g.claude(okOut, 0)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" }, http.StatusInternalServerError)
	stop := logtest.Capture(t, logr.W)
	err := g.d.Iterate(context.Background())
	warnings := stop()
	if err == nil || !strings.Contains(err.Error(), "500") || strings.Contains(err.Error(), "didn't finish") {
		t.Errorf("the pass = %v; want step 1's 500, and not the wait", err)
	}
	if got := warnings; strings.Count(got, "step 1 didn't finish on #1") != 1 || !strings.Contains(got, "applying run saved on #1 failed, 1 of 3 times") {
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
