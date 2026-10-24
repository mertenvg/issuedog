package daemon_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A send-back's unassign waits for the pass's stage work (SPEC §9.6,
// #333): when the planner posts the next revision in the pass whose step 1
// sent the plan back, the epic waits on the owner again, so notify stays
// assigned with no write. A send-back whose planner run fails still
// unassigns, after the run.

// assignSendBackStart starts l, as a restart does (planReviewRunLoop.start),
// with run IDs that follow the fake claude's count of runs, so a restart
// doesn't name a run as one before it.
func assignSendBackStart(l *planReviewRunLoop) {
	l.start()
	g := l.g
	g.r.NewID = func() string { return "run" + strconv.Itoa(g.runs()+1) }
}

// assignSendBackLoop starts planReviewRunAlone's loop on f, o and root:
// mertenvg's epic #1 at plan-review with plan revision 1, listed in the
// digest, #2, and assigned to mertenvg. The fake claude prints plan
// revision 1, then outs, one a run. Then mertenvg's comment sends the plan
// back, which it returns the ID of. The fake's write log starts after it,
// so Replay counts the writes of the passes after.
func assignSendBackLoop(t *testing.T, f *githubtest.Fake, o *origin, root string, outs ...map[string]any) (*planReviewRunLoop, int64) {
	t.Helper()
	f.SetPermission(planReviewRunOwner, "admin")
	epic, entry := planReviewRunEpic(f)
	out := planReviewRunOutput(t, nil)
	l := &planReviewRunLoop{t: t, f: f, o: o, root: root}
	assignSendBackStart(l)
	l.outs(append([]map[string]any{out}, outs...)...)
	l.step("the first pass", 1, "POST /issues", "POST /graphql", "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	l.planComment("plan revision 1", l.newest("plan revision 1", epic), epic, 1, 1, entry, out)
	l.step("the epic's plan listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")
	assignWant(t, f, "the epic's plan listed", map[int][]string{epic: {planReviewRunOwner}})
	notes := f.AddComment(epic, planReviewRunOwner, planReviewRunNotes)
	f.ForgetWrites()
	return l, notes
}

// assignSendBackWrites are the writes of the pass in which comment notes
// sends #1's plan back, with the digest, #2, edited for the plan's item
// ended, then of the planner's run, which posts the next revision, but no
// assignee write.
func assignSendBackWrites(notes int64) []string {
	return []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		discRunReact(notes), "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning"}
}

// The send-back's pass gets plan revision 2 from the planner, which waits
// on mertenvg, so he stays assigned with no unassign and no assign. The
// pass after lists revision 2 in the digest, with no assignee write
// either, and an unchanged pass makes no request but conditional GETs
// answered 304. Replay passes for the send-back's pass, on a restart,
// whatever write a crash comes at: a crash in the planner's apply keeps
// mertenvg too, as its result is left for the next pass to apply.
func TestAssignSendBackReplay(t *testing.T) {
	t.Parallel()
	o := planReviewRunOrigin(t)
	f := newFake(t)
	l, notes := assignSendBackLoop(t, f, o, t.TempDir(), planReviewRunOutput(t, nil))
	l.step("the send-back", 2, assignSendBackWrites(notes)...)
	l.labels("the send-back", 1, "stage:plan-review", "type:epic")
	assignWant(t, f, "the send-back", map[int][]string{1: {planReviewRunOwner}})
	l.step("plan revision 2 listed", 2, "PATCH /issues/2")
	if got, want := l.digest(2), []string{"#1 Plan approval p2 1"}; !slices.Equal(got, want) {
		t.Errorf("plan revision 2 listed: items %q; want %q", got, want)
	}
	assignWant(t, f, "plan revision 2 listed", map[int][]string{1: {planReviewRunOwner}})
	l.quiet("plan revision 2")

	// Each fake gets its own state directory, kept for its passes, as the
	// state volume outlives a crash.
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		roots[f] = t.TempDir()
		assignSendBackLoop(t, f, o, roots[f], planReviewRunOutput(t, nil))
		return f
	}, func(f *githubtest.Fake) error {
		l := &planReviewRunLoop{t: t, f: f, o: o, root: roots[f]}
		assignSendBackStart(l)
		// The digest shows a stage run's writes only on the next pass
		// (SPEC §8.1), so a pass after a crash would list revision 2 where
		// the clean pass didn't: it's off here, and assignment alone is on.
		l.g.d.Digest = false
		return l.g.d.Iterate(context.Background())
	})
}

// A send-back whose planner run asks a question round instead of posting
// the next revision leaves the epic at planning, waiting on mertenvg for
// his answer, so he stays assigned with no write in that pass (SPEC §9.6,
// #456), and the pass after lists the round in the digest, with no
// assignee write either. An unchanged pass makes no request but
// conditional GETs answered 304. Replay passes for the send-back's pass,
// on a restart, whatever write a crash comes at.
func TestAssignSendBackRoundReplay(t *testing.T) {
	t.Parallel()
	o := planReviewRunOrigin(t)
	f := newFake(t)
	l, notes := assignSendBackLoop(t, f, o, t.TempDir(), plGolden(t, "questions"))
	want := []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		discRunReact(notes), "PATCH /issues/2", "POST /issues/1/comments"}
	l.step("the send-back", 2, want...)
	l.labels("the send-back", 1, "stage:planning", "type:epic")
	if round := l.newest("the send-back", 1); !strings.HasPrefix(round.Body, "### Questions for #1 (round 1)\n\n") {
		t.Errorf("the send-back: #1's newest comment =\n%s\nwant the planner's round 1", round.Body)
	}
	assignWant(t, f, "the send-back", map[int][]string{1: {planReviewRunOwner}})
	l.step("round 1 listed", 2, "PATCH /issues/2")
	if got, want := l.digest(2), []string{"#1 Question round p2 1"}; !slices.Equal(got, want) {
		t.Errorf("round 1 listed: items %q; want %q", got, want)
	}
	assignWant(t, f, "round 1 listed", map[int][]string{1: {planReviewRunOwner}})
	l.quiet("round 1")

	// Each fake gets its own state directory, kept for its passes, as the
	// state volume outlives a crash.
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		roots[f] = t.TempDir()
		assignSendBackLoop(t, f, o, roots[f], plGolden(t, "questions"))
		return f
	}, func(f *githubtest.Fake) error {
		l := &planReviewRunLoop{t: t, f: f, o: o, root: roots[f]}
		assignSendBackStart(l)
		// The digest shows a stage run's writes only on the next pass
		// (SPEC §8.1), so a pass after a crash would list round 1 where
		// the clean pass didn't: it's off here, and assignment alone is on.
		l.g.d.Digest = false
		return l.g.d.Iterate(context.Background())
	})
}

// A run whose apply is held back by a label a rename put on during it
// (ErrLabelsHeld) waits for a later pass to apply it, and that pass
// judges the assignees, as for a failed apply (SPEC §9.6): owner, assigned
// to the interview at clarifying, stays assigned with no write in the
// run's pass, through the hold, and once the result is applied at
// spec-review.
func TestAssignHeldApplyKeepsNotify(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f) // #1, at clarifying, with its round answered
	f.SetAssignees(1, "owner")
	f.AddLabel(1, "mallory", "foo") // unguarded, so it stands
	f.Advance(time.Hour)
	g := newRig(t, f, digestNotifyOrigin(t), t.TempDir())
	g.d.Assign = true
	g.claude(okOut, 0)
	g.stage.during = func() { f.RenameLabel("foo", "priority:p0") }
	from := len(f.Writes())
	g.pass()
	if l := g.lease(); l.RunID != "run1" || specApplied(f, 1, "run1", answer) {
		t.Fatalf("the run's pass: lease %+v; want run1's kept, its result not applied, as priority:p0 is held back", l)
	}
	assignWant(t, f, "the run's pass", map[int][]string{1: {"owner"}})
	g.stage.during = nil
	f.Advance(github.HoldBound)
	g.pass()
	if !specApplied(f, 1, "run1", answer) {
		t.Fatalf("the hold's end: labels %q; want run1 applied at spec-review", f.State().Issues[0].Labels)
	}
	assignWant(t, f, "the hold's end", map[int][]string{1: {"owner"}})
	g.pass()
	assignWant(t, f, "the pass after", map[int][]string{1: {"owner"}})
	for _, w := range f.Writes()[from:] {
		if strings.Contains(w.Path, "/assignees") {
			t.Errorf("the passes wrote %s %s; want no assignee write", w.Method, w.Path)
		}
	}
	if g.runs() != 1 {
		t.Errorf("claude ran %d times; want once", g.runs())
	}
}

// An unassign at a stage whose stage work can't bring the issue back to
// the owner, such as ready, is made before Work, as every other assignee
// write is (SPEC §9.6): issue 2's comes before the run on issue 1, at
// clarifying, and its writes.
func TestAssignSendBackOnlyAtClarifyingOrPlanning(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f) // #1, at clarifying, with its round answered
	ready := fileAt(f, "ready", "do it")
	f.SetAssignees(ready, "owner")
	g := newRig(t, f, digestNotifyOrigin(t), t.TempDir())
	g.d.Assign = true
	g.claude(okOut, 0)
	from := len(f.Writes())
	g.pass()
	var got []string
	for _, w := range f.Writes()[from:] {
		got = append(got, w.Method+" "+w.Path)
	}
	if len(got) < 2 || got[0] != "DELETE /issues/2/assignees" || !slices.ContainsFunc(got[1:], func(w string) bool { return strings.Contains(w, " /issues/1/") }) {
		t.Errorf("the pass wrote %q; want issue 2's unassign first, then the run's writes to issue 1", got)
	}
	if g.runs() != 1 {
		t.Errorf("claude ran %d times; want once, on issue 1", g.runs())
	}
	assignWant(t, f, "the pass", nil)
}

// A send-back whose planner run fails leaves the epic at planning, waiting
// on nobody, so mertenvg is unassigned in that pass, after the run.
func TestAssignSendBackRunFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	// The fake claude has no output for run 2, so that run fails.
	l, notes := assignSendBackLoop(t, f, planReviewRunOrigin(t), t.TempDir())
	want := []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		discRunReact(notes), "PATCH /issues/2", "DELETE /issues/1/assignees"}
	l.step("the send-back", 2, want...)
	l.labels("the send-back", 1, "stage:planning", "type:epic")
	assignWant(t, f, "the send-back", nil)
}
