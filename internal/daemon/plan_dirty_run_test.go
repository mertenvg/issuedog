package daemon_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// A plan comment made dirty (#315), in a whole run through the real
// Planner and step 1 wired as main wires it (planReviewRunAlone).

// planDirtyRunItem is the digest's item for the epic, 1, at plan-review with
// no clean plan revision, linking comment id.
func planDirtyRunItem(id int64) string {
	return "**Plan approval**, priority p2, unblocks 1 open issue (itself included)\n\n" +
		"- The plan waits for review, with no clean revision to approve.\n" +
		"- It can't be approved yet: there's no clean plan revision to approve: none was posted, or it was edited without approval or deleted.\n" +
		"- Post `/revise <notes>`, or any other comment, to send it back and get a new plan.\n" +
		fmt.Sprintf("- Answer here: https://github.com/o/r/issues/1#issuecomment-%d\n", id)
}

// planDirtyRunNoError fails t if any comment on issue 1 carries a state
// error.
func planDirtyRunNoError(t *testing.T, l *planReviewRunLoop, step string) {
	t.Helper()
	for _, c := range l.comments(1) {
		if strings.Contains(c.Body, "<!-- issuedog:error ") {
			t.Errorf("%s: comment %d carries a state error: %q", step, c.ID, c.Body)
		}
	}
}

// mertenvg's edit of the plan comment makes it dirty, which used to put
// the epic back at planning, with a disagreement, for a planner run of
// its own. Now the pass records the move to plan-review again, notes the
// edit on the plan comment, and lists the plan as having no clean
// revision, with no state error and no run, and an unchanged pass costs no
// rate limit. A 👍 on the dirty plan comment approves nothing, and
// /approve gets the "no clean plan revision" reply. Then a comment, or
// /revise, sends the plan back, and the planner's next run posts a new
// one. A restart writes nothing.
func TestPlanDirtyRun(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, back string
	}{
		{"a comment", planReviewRunNotes},
		{"/revise", "/revise " + planReviewRunNotes},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			nextOut := planReviewRunOutput(t, nil)
			l, rev1, _ := planReviewRunAlone(t, nextOut)
			f := l.f
			owner := []string{planReviewRunOwner}

			f.EditComment(rev1.ID, planReviewRunOwner, "### Plan for #1 (rev 1)\n\nMine now.")
			l.step("the plan comment made dirty", 1, "POST /issues/1/comments", fmt.Sprintf("PATCH /issues/comments/%d", rev1.ID), "PATCH /issues/2")
			record := l.newest("the plan comment made dirty", 1)
			if want := planDirtyRecorded(rev1.ID); record.Body != want {
				t.Errorf("the plan comment made dirty: the newest comment =\n%s\nwant\n%s", record.Body, want)
			}
			l.labels("the plan comment made dirty", 1, "stage:plan-review", "type:epic")
			planDirtyRunNoError(t, l, "the plan comment made dirty")
			if got, want := l.digest(2), []string{"#1 Plan approval p2 1"}; !slices.Equal(got, want) || !strings.Contains(f.State().Issues[1].Body, planDirtyRunItem(record.ID)) {
				t.Errorf("the plan comment made dirty: items %q, the digest =\n%s\nwant %q, with\n%s", got, f.State().Issues[1].Body, want, planDirtyRunItem(record.ID))
			}
			assignWant(t, f, "the plan comment made dirty", map[int][]string{1: owner})
			l.quiet("the plan comment made dirty")

			f.AddReaction(rev1.ID, planReviewRunOwner, "+1")
			l.step("a 👍 on the dirty plan comment", 1)
			l.labels("a 👍 on the dirty plan comment", 1, "stage:plan-review", "type:epic")

			approve := f.AddComment(1, planReviewRunOwner, "/approve")
			l.step("the /approve", 1, discRunReact(approve), "POST /issues/1/comments")
			if added, want := l.added(1), []string{final("/approve", "I didn't apply this command",
				"there's no clean plan revision to approve. "+replanHint, strconv.FormatInt(approve, 10))}; !slices.Equal(added, want) {
				t.Errorf("the /approve: added %q; want %q", added, want)
			}
			l.reactions("the /approve", 1, approve, app+" -1")
			l.labels("the /approve", 1, "stage:plan-review", "type:epic")
			l.quiet("the /approve")

			back := f.AddComment(1, planReviewRunOwner, tt.back)
			l.step("sent back", 2, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
				discRunReact(back), "PATCH /issues/2",
				"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
			added := l.added(1)
			sentBack := fmt.Sprintf("The plan is sent back for revision, with %s's comment as the notes.\n\n"+
				"<!-- issuedog:transition from=plan-review to=planning cmd=%d seen=%d -->", planReviewRunOwner, back, back)
			if len(added) != 2 || added[0] != sentBack {
				t.Fatalf("sent back: added %q; want the move back to planning\n%s\nthen a new plan revision", added, sentBack)
			}
			// The dirty plan comment is no revision (SPEC §7.4), so the new
			// plan is revision 1 again, one more than the latest the
			// planner can read.
			cs := l.comments(1)
			moved, plan := cs[len(cs)-2], cs[len(cs)-1]
			hash := l.planComment("the new plan", plan, 1, 1, 2, moved.ID, nextOut)
			l.labels("the new plan", 1, "stage:plan-review", "type:epic")
			planDirtyRunNoError(t, l, "the new plan")
			// The next pass lists it, approvable, and the epic keeps its
			// assignee, as the new plan was posted in the pass that sent the
			// old one back (SPEC §9.6).
			l.step("the new plan listed", 2, "PATCH /issues/2")
			if want := fmt.Sprintf("- Revision 1 of the plan, `%s`, waits for approval.\n"+
				"- React 👍 to the plan comment, or post `/approve`, to approve it.\n", hash); !strings.Contains(f.State().Issues[1].Body, want) {
				t.Errorf("the new plan listed: the digest =\n%s\nwant\n%s", f.State().Issues[1].Body, want)
			}
			assignWant(t, f, "the new plan listed", map[int][]string{1: owner})
			l.quiet("the new plan")
			l.restart("the end")
		})
	}
}

// mallory, a write collaborator CODEOWNERS doesn't trust, edits the plan
// comment: her edit holds the epic for discussion from plan-review, as
// any edit of hers to the App's content does (SPEC §6.4), and the move
// to plan-review isn't recorded again. mertenvg's /resume takes it back
// to plan-review, by a move of its own, which needs no record either, and
// the plan has no clean revision.
func TestPlanDirtyRunUntrusted(t *testing.T) {
	t.Parallel()
	l, rev1, _ := planReviewRunAlone(t)
	f := l.f
	f.EditComment(rev1.ID, "mallory", "Mine now.")
	l.step("mallory's edit", 1, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		fmt.Sprintf("PATCH /issues/comments/%d", rev1.ID), "PATCH /issues/2")
	hold := l.newest("mallory's edit", 1)
	if !strings.HasPrefix(hold.Body, "On hold for discussion, from `plan-review`: `mallory` edited") {
		t.Errorf("mallory's edit: the newest comment =\n%s\nwant the hold", hold.Body)
	}
	l.labels("mallory's edit", 1, "stage:held", "type:epic")
	l.quiet("held")

	resume := f.AddComment(1, planReviewRunOwner, "/resume")
	l.step("the /resume", 1, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:held", discRunReact(resume),
		"PATCH /issues/2")
	l.labels("the /resume", 1, "stage:plan-review", "type:epic")
	cs := l.comments(1)
	if slices.ContainsFunc(cs, func(c github.Comment) bool { return strings.Contains(c.Body, "event=plan-dirty-") }) {
		t.Errorf("the /resume: a move recorded again, in %+v", cs)
	}
	resumed := cs[len(cs)-1]
	if !strings.Contains(f.State().Issues[1].Body, planDirtyRunItem(resumed.ID)) {
		t.Errorf("the /resume: the digest =\n%s\nwant\n%s", f.State().Issues[1].Body, planDirtyRunItem(resumed.ID))
	}
	planDirtyRunNoError(t, l, "the /resume")
	l.quiet("resumed")
	l.restart("the end")
}
