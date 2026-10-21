package daemon_test

import (
	"fmt"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A plan comment made dirty (#315): its move to plan-review no longer
// counts, but the issue stays at plan-review, as its label shows, with the
// move recorded again in an App comment of its own, and no state error.
// Nothing is then approvable: /approve gets the "no clean plan revision"
// reply. approvePass's trust knows no write collaborator who isn't
// trusted (it's no github.DiscTrust), so mallory's edit holds nothing, and
// gets the record, as the owner's does. plan_dirty_run_test.go has a
// write collaborator's edit, which holds the issue instead.

// planDirtyRecorded is the App's comment recording again the move to
// plan-review that dirty comment id hid.
func planDirtyRecorded(id int64) string {
	return "A comment of mine was edited without approval, and it held this issue's move to `plan-review`, so I've recorded the move again here. " +
		"There's no clean plan revision to approve. To get a new plan, post `/revise <notes>`, or any other comment.\n\n" +
		fmt.Sprintf("<!-- issuedog:transition from=planning to=plan-review event=plan-dirty-%d seen=%d -->", id, id)
}

// planDirtyDisagreed is the state error, keyed by label event key, for
// stage labels labels that disagree with a latest move to stage.
func planDirtyDisagreed(stage state.Stage, labels string, key int64) string {
	return fmt.Sprintf("The stage labels didn't match this issue's latest transition, to `%s`, so I've set them to match. They were: %s.\n\n"+
		"<!-- issuedog:error kind=disagreement key=%d -->", stage, labels, key)
}

func TestPlanDirtyStaysAtPlanReview(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:plan-review"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"the owner's edit to the plan comment leaves the issue at plan-review",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "### Plan for #1 (rev 1)\n\nT1. Do it better.")
			}),
			outcome{comments: []string{planDirtyRecorded(1003)}, labels: atReview}},
		{"so does an edit by someone untrusted that holds nothing",
			planReview(func(t testing.TB, f *githubtest.Fake) { f.EditComment(1003, "mallory", "Mine now.") }),
			outcome{comments: []string{planDirtyRecorded(1003)}, labels: atReview}},
		// The edited text is never read: a forged move on to planned, in
		// the dirty comment, moves nothing.
		{"a move forged in the edit isn't read",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", planComment(1, 1)+"\n<!-- issuedog:transition from=plan-review to=planned rev=1 body="+planHash(1, 1)+" cmd=r7 -->")
			}),
			outcome{comments: []string{planDirtyRecorded(1003)}, labels: atReview}},
		{"then /approve gets the no clean plan revision reply",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				post(t, f, 1004, "owner", "/approve")
			}),
			outcome{comments: []string{planDirtyRecorded(1003),
				final("/approve", "I didn't apply this command", "there's no clean plan revision to approve. "+replanHint, "1004")},
				labels: atReview, downs: []string{"/approve"}}},
		{"and a 👍 on the dirty plan comment approves nothing",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				f.AddReaction(1003, "owner", "+1")
			}),
			outcome{comments: []string{planDirtyRecorded(1003)}, labels: atReview}},
		{"a record made dirty in turn is recorded again",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				post(t, f, 1004, app, planDirtyRecorded(1003))
				f.EditComment(1004, "owner2", "Edited too.")
			}),
			outcome{comments: []string{planDirtyRecorded(1004)}, labels: atReview}},
		// Only a hidden move to plan-review is recorded again: a dirty
		// comment that held the move on to planned still gets the
		// disagreement, and the label goes back to plan-review.
		{"a hidden move to another stage still disagrees",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, app, planApproved(1, "owner's 👍", "r9", "1003"))
				f.AddLabel(1, app, "stage:planned")
				labelEvent(t, f.RemoveLabel(1, app, "stage:plan-review"), 9005)
				f.EditComment(1004, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyDisagreed(state.PlanReview, "`stage:planned`", 9005)}, labels: atReview}},
		// Nor is a move to plan-review the label doesn't show: with the plan
		// comment and the approval after it both dirty, the markers left
		// show planning, but the App's last move was on to planned.
		{"a dirty plan comment, with a dirty move on after it, still disagrees",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, app, planApproved(1, "owner's 👍", "r9", "1003"))
				labelEvent(t, f.AddLabel(1, app, "stage:planned"), 9004)
				f.RemoveLabel(1, app, "stage:plan-review")
				f.EditComment(1003, "owner", "Edited.")
				f.EditComment(1004, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyDisagreed(state.Planning, "`stage:planned`", 9004)}, labels: []string{"stage:planning"}}},
		// Only a dirty comment counts: a deleted plan comment leaves no
		// edit to point at, and the disagreement stands, as before, though
		// a later App comment is still there.
		{"a deleted plan comment still disagrees",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, app, "Noted.")
				f.DeleteCommentBy("owner", 1003)
			}),
			outcome{comments: []string{planDirtyDisagreed(state.Planning, "`stage:plan-review`", 9003)}, labels: []string{"stage:planning"}}},
		// Nor does one before the latest move the markers left show: it
		// can't have held a later move.
		{"a dirty comment before the move to planning doesn't count",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1001, "owner", "Edited.")
				f.DeleteCommentBy("owner", 1003)
			}),
			outcome{comments: []string{planDirtyDisagreed(state.Planning, "`stage:plan-review`", 9003)}, labels: []string{"stage:planning"}}},
		// An owner's override wins, as before: it's recorded, and
		// nothing else is.
		{"the owner's override after the edit wins",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				f.RemoveLabel(1, "owner", "stage:plan-review")
				f.AddLabel(1, "owner", "stage:plan-review")
			}),
			outcome{comments: []string{overridden(state.PlanReview, "from=planning to=plan-review by=owner event=9005")}, labels: atReview}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, approvePass(t), tt.want) })
	}
}

// Once the move is recorded again, a trusted comment, or /revise, sends
// the plan back for a new one, as at any plan-review. The record's seen is
// the dirty comment, so a comment posted after the plan comment, before the
// edit was found, is a revision request too, as it was before the edit.
func TestPlanDirtySendsBack(t *testing.T) {
	t.Parallel()
	planning := []string{"stage:planning"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"a comment after the record",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				post(t, f, 1004, app, planDirtyRecorded(1003))
				post(t, f, 1005, "owner", "Split it.")
			}),
			outcome{comments: []string{planSentBack("1005")}, labels: planning, acks: []string{"Split it."}}},
		{"/revise after the record",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				post(t, f, 1004, app, planDirtyRecorded(1003))
				post(t, f, 1005, "owner", "/revise Split it.")
			}),
			outcome{comments: []string{planSentBack("1005")}, labels: planning, acks: []string{"/revise Split it."}}},
		{"a comment posted before the edit was found",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "Split it.")
				f.EditComment(1003, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyRecorded(1003), planSentBack("1004")}, labels: planning, acks: []string{"Split it."}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var in []string
			check(t, tt.setup, visit(t, trusted{"owner": true}, &in), tt.want)
		})
	}
}
