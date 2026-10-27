package daemon_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// An answered round's unassign waits for the pass's stage work (SPEC
// §9.6, #456): when the planner asks the next round in the pass that finds
// the answer, the epic waits on the owner again, so notify stays assigned
// with no write. A round re-applied after an apply cut short, with an
// answer since, waits on nobody, so the unassign is made.

// assignRoundAnswer is mertenvg's answer to a planner round.
const assignRoundAnswer = "1a 2a"

// assignRoundLoop starts a planReviewRunLoop on f, o and root, as a
// restart does (assignSendBackStart): mertenvg's epic #1 at planning,
// whose planner run asks round 1, which the digest, #2, lists, and which
// assigns mertenvg. The fake claude prints round 1, then outs, one a run.
// Then mertenvg answers round 1, with the comment it returns the ID of.
// The fake's write log starts after it, so Replay counts the writes of
// the passes after.
func assignRoundLoop(t *testing.T, f *githubtest.Fake, o *origin, root string, outs ...map[string]any) (*planReviewRunLoop, int64) {
	t.Helper()
	f.SetPermission(planReviewRunOwner, "admin")
	epic, _ := planReviewRunEpic(f)
	l := &planReviewRunLoop{t: t, f: f, o: o, root: root}
	assignSendBackStart(l)
	l.outs(append([]map[string]any{plGolden(t, "questions")}, outs...)...)
	l.step("round 1", 1, "POST /issues", "POST /graphql", "PATCH /issues/2", "POST /issues/1/comments")
	l.step("round 1 listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")
	assignWant(t, f, "round 1 listed", map[int][]string{epic: {planReviewRunOwner}})
	answer := f.AddComment(epic, planReviewRunOwner, assignRoundAnswer)
	f.ForgetWrites()
	return l, answer
}

// assignRoundWrites are the writes of the pass that finds answer to round
// 1: step 1's 👍 on it, the digest, #2, edited for round 1's item ended,
// then the planner's run on the answer, which asks round 2, but no
// assignee write.
func assignRoundWrites(answer int64) []string {
	return []string{discRunReact(answer), "PATCH /issues/2", "POST /issues/1/comments"}
}

// The answer's pass gets round 2 from the planner, which waits on
// mertenvg, so he stays assigned with no unassign and no assign. The pass
// after lists round 2 in the digest, with no assignee write either, and
// an unchanged pass makes no request but conditional GETs answered 304.
// Replay passes for the answer's pass, on a restart, whatever write a
// crash comes at: a crash in the planner's apply keeps mertenvg too, as
// its result is left for the next pass to apply.
func TestAssignAnsweredRoundReplay(t *testing.T) {
	t.Parallel()
	o := planReviewRunOrigin(t)
	f := newFake(t)
	l, answer := assignRoundLoop(t, f, o, t.TempDir(), plGolden(t, "questions"))
	l.step("the answer", 2, assignRoundWrites(answer)...)
	l.labels("the answer", 1, "stage:planning", "type:epic")
	if round := l.newest("the answer", 1); !strings.HasPrefix(round.Body, "### Questions for #1 (round 2)\n\n") {
		t.Errorf("the answer: #1's newest comment =\n%s\nwant the planner's round 2", round.Body)
	}
	assignWant(t, f, "the answer", map[int][]string{1: {planReviewRunOwner}})
	l.step("round 2 listed", 2, "PATCH /issues/2")
	if got, want := l.digest(2), []string{"#1 Question round p2 1"}; !slices.Equal(got, want) {
		t.Errorf("round 2 listed: items %q; want %q", got, want)
	}
	assignWant(t, f, "round 2 listed", map[int][]string{1: {planReviewRunOwner}})
	l.quiet("round 2")

	// Each fake gets its own state directory, kept for its passes, as the
	// state volume outlives a crash.
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		roots[f] = t.TempDir()
		assignRoundLoop(t, f, o, roots[f], plGolden(t, "questions"))
		return f
	}, func(f *githubtest.Fake) error {
		l := &planReviewRunLoop{t: t, f: f, o: o, root: roots[f]}
		assignSendBackStart(l)
		// The digest shows a stage run's writes only on the next pass
		// (SPEC §8.1), so a pass after a crash would list round 2 where
		// the clean pass didn't: it's off here, and assignment alone is on.
		l.g.d.Digest = false
		return l.g.d.Iterate(context.Background())
	})
}

// A planner run whose round 2 is posted, but whose apply fails, as when
// GitHub's answer to the post is lost, keeps mertenvg assigned that pass,
// for the next to judge (SPEC §9.6). His answer to round 2 comes before
// that pass, which only finishes the result, as its move is written: round
// 2 then waits on nobody, so he is unassigned, after the stage work.
func TestAssignReappliedRoundAnswered(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	l, answer := assignRoundLoop(t, f, planReviewRunOrigin(t), t.TempDir(), plGolden(t, "questions"))
	want := assignRoundWrites(answer)
	f.FailWriteAfter(len(want))
	if err := l.g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "apply run run2") {
		t.Fatalf("round 2's post: Iterate = %v; want run2's apply failed", err)
	}
	if got := digestWrites(f, 0); !slices.Equal(got, want) {
		t.Errorf("round 2's post: writes %q; want %q", got, want)
	}
	if round := l.newest("round 2's post", 1); !strings.HasPrefix(round.Body, "### Questions for #1 (round 2)\n\n") {
		t.Fatalf("round 2's post: #1's newest comment =\n%s\nwant the planner's round 2", round.Body)
	}
	assignWant(t, f, "round 2's post", map[int][]string{1: {planReviewRunOwner}})
	answer = f.AddComment(1, planReviewRunOwner, assignRoundAnswer)
	l.step("the re-apply", 2, discRunReact(answer), "DELETE /issues/1/assignees")
	assignWant(t, f, "the re-apply", nil)
}
