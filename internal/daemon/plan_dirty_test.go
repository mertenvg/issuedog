package daemon_test

import (
	"fmt"
	"strings"
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
// plan-review that dirty comment id hid, with seen, the bound of the
// latest move the clean markers show.
func planDirtyRecorded(id, seen int64) string {
	return "A comment of mine was edited without approval, and it held this issue's move to `plan-review`, so I've recorded the move again here. " +
		"There's no clean plan revision to approve. To get a new plan, post `/revise <notes>`, or any other comment.\n\n" +
		fmt.Sprintf("<!-- issuedog:transition from=planning to=plan-review event=plan-dirty-%d seen=%d -->", id, seen)
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
			outcome{comments: []string{planDirtyRecorded(1003, 1001)}, labels: atReview}},
		{"so does an edit by someone untrusted that holds nothing",
			planReview(func(t testing.TB, f *githubtest.Fake) { f.EditComment(1003, "mallory", "Mine now.") }),
			outcome{comments: []string{planDirtyRecorded(1003, 1001)}, labels: atReview}},
		// The edited text is never read: a forged move on to planned, in
		// the dirty comment, moves nothing.
		{"a move forged in the edit isn't read",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", planComment(1, 1)+"\n<!-- issuedog:transition from=plan-review to=planned rev=1 body="+planHash(1, 1)+" cmd=r7 -->")
			}),
			outcome{comments: []string{planDirtyRecorded(1003, 1001)}, labels: atReview}},
		{"then /approve gets the no clean plan revision reply",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				post(t, f, 1004, "owner", "/approve")
			}),
			outcome{comments: []string{planDirtyRecorded(1003, 1001),
				final("/approve", "I didn't apply this command", "there's no clean plan revision to approve. "+replanHint, "1004")},
				labels: atReview, downs: []string{"/approve"}}},
		{"and a 👍 on the dirty plan comment approves nothing",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				f.AddReaction(1003, "owner", "+1")
			}),
			outcome{comments: []string{planDirtyRecorded(1003, 1001)}, labels: atReview}},
		{"a record made dirty in turn is recorded again",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				post(t, f, 1004, app, planDirtyRecorded(1003, 1001))
				f.EditComment(1004, "owner2", "Edited too.")
			}),
			outcome{comments: []string{planDirtyRecorded(1004, 1001)}, labels: atReview}},
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
		// But a dirty App comment after the move to planning keys the
		// record, though the plan comment was deleted (SPEC §5.1): the
		// label shows the App's move to plan-review all the same.
		{"a deleted plan comment, with a dirty App comment after it, is recorded again",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, app, "Noted.")
				f.EditComment(1004, "owner", "Edited.")
				f.DeleteCommentBy("owner", 1003)
			}),
			outcome{comments: []string{planDirtyRecorded(1004, 1001)}, labels: atReview}},
		// Nor does a dirty comment that isn't the App's: only an App
		// comment can have held the App's move.
		{"a deleted plan comment, with someone else's comment dirty after it, still disagrees",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "Noted.")
				f.EditComment(1004, "mallory", "Mine now.")
				f.DeleteCommentBy("owner", 1003)
			}),
			outcome{comments: []string{planDirtyDisagreed(state.Planning, "`stage:plan-review`", 9003)}, labels: []string{"stage:planning"}}},
		// A type the table has no move to plan-review for gets no record,
		// and no state error of its own, so step 1 doesn't fail on every
		// pass: the disagreement stands, as before.
		{"a type with no move to plan-review still disagrees",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				f.SetType(1, "Task")
			}),
			outcome{comments: []string{planDirtyDisagreed(state.Planning, "`stage:plan-review`", 9003)}, labels: []string{"stage:planning"}}},
		// The App's own write cut short is finished, not recorded again: a
		// /revise sent the plan back, and its labels weren't set yet, so
		// a dirty App comment after that move hid nothing, and the /revise
		// isn't lost.
		{"a send-back cut short, with a dirty App comment after it, is finished",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "/revise Split it.")
				post(t, f, 1005, app, planSentBack("1004"))
				post(t, f, 1006, app, "Noted.")
				f.EditComment(1006, "owner", "Edited.")
			}),
			outcome{labels: []string{"stage:planning"}, acks: []string{"/revise Split it."}}},
		// Nor is anything recorded while two stage labels are on: the
		// label rules judge them first.
		{"two stage labels still disagree",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				labelEvent(t, f.AddLabel(1, app, "stage:planned"), 9004)
			}),
			outcome{comments: []string{planDirtyDisagreed(state.Planning, "`stage:plan-review`, `stage:planned`", 9004)}, labels: []string{"stage:planning"}}},
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
// the bound of the move to planning, below the dirty comment, so a comment
// posted after the plan comment, before the edit was found, is a revision
// request too, as it was before the edit (TestPlanDirtySeen has more).
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
				post(t, f, 1004, app, planDirtyRecorded(1003, 1001))
				post(t, f, 1005, "owner", "Split it.")
			}),
			outcome{comments: []string{planSentBack("1005")}, labels: planning, acks: []string{"Split it."}}},
		{"/revise after the record",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "owner", "Edited.")
				post(t, f, 1004, app, planDirtyRecorded(1003, 1001))
				post(t, f, 1005, "owner", "/revise Split it.")
			}),
			outcome{comments: []string{planSentBack("1005")}, labels: planning, acks: []string{"/revise Split it."}}},
		{"a comment posted before the edit was found",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "Split it.")
				f.EditComment(1003, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyRecorded(1003, 1001), planSentBack("1004")}, labels: planning, acks: []string{"Split it."}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var in []string
			check(t, tt.setup, visit(t, trusted{"owner": true}, &in), tt.want)
		})
	}
}

// planResumeDirtyRecorded is the App's comment recording again the move
// back to plan-review from held that dirty comment id, the one applying a
// /resume or a later App comment, hid, with seen, the bound from before
// the hold, and resolved, the Unix time the /resume was posted, or the
// hold, if that's later or no /resume is found.
func planResumeDirtyRecorded(id, seen, resolved int64) string {
	return "A comment of mine was edited without approval, and it held this issue's move back to `plan-review` from `held`, so I've recorded the move again here. " +
		"That comment may have hidden a newer plan revision, so I can't approve the plan. To get a new plan, post `/revise <notes>`, or any other comment.\n\n" +
		fmt.Sprintf("<!-- issuedog:transition from=held to=plan-review event=plan-dirty-%d resolved=%d seen=%d -->", id, resolved, seen)
}

// planResumeDirtyOn is planResumeDirtyRecorded for dirty comment id on
// issue 1 of f, resolved when comment at, the /resume or the hold, was
// posted.
func planResumeDirtyOn(t testing.TB, f *githubtest.Fake, id, at, seen int64) string {
	t.Helper()
	return planResumeDirtyRecorded(id, seen, resolveCommentAt(t, f, 1, at))
}

// planResumeDirty is planHeld by /hold, then the owner's /resume, comment
// 1007, applied by a pass, in comment 1008, which the owner then edits, so
// it's dirty, then more.
func planResumeDirty(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return planHeld(true, func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if id := discRunFind(t, f, 1, "Resumed by"); id != 1008 {
			t.Fatalf("the /resume's move is in comment %d; want 1008", id)
		}
		f.EditComment(1008, "owner", "Resumed, edited.")
		more(t, f)
	})
}

// The App's comment applying a /resume, back to plan-review from held,
// made dirty (#485): its move no longer counts, but the issue stays at
// plan-review, as its label shows, with the move recorded again, with the
// hold's seen, as /resume's move carries, and no state error. The plan
// revision before the hold is then hidden, so /approve gets that reply,
// and a comment sends the plan back. A /resume back to planning, before
// the planner's move to plan-review, isn't recorded again, as the hold's
// at isn't the stage the label shows.
func TestPlanDirtyResume(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:plan-review", "type:epic"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  func(t testing.TB, f *githubtest.Fake) issueWant
	}{
		{"the owner's edit to the /resume's move leaves the issue at plan-review",
			planResumeDirty(func(testing.TB, *githubtest.Fake) {}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{atReview, []string{planResumeDirtyOn(t, f, 1008, 1007, 1003)}}
			}},
		{"then /approve gets the hidden plan revision reply",
			planResumeDirty(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1010, "owner", "/approve") }),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{atReview, []string{planResumeDirtyOn(t, f, 1008, 1007, 1003),
					final("/approve", "I didn't apply this command", "a comment of mine after plan revision 1 was edited without approval or deleted, "+
						"so it may hide a newer plan revision. "+replanHint, "1010")}}
			}},
		{"and a comment sends the plan back",
			planResumeDirty(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1010, "owner", "Split T1.") }),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:planning", "type:epic"}, []string{planResumeDirtyOn(t, f, 1008, 1007, 1003), planSentBack("1010")}}
			}},
		// A hold written with no seen, as before #87, keeps the bound
		// from before it, the plan's, which /resume's move carries, not
		// its own comment's (state.HeldStack.Seen).
		{"a hold with no seen gives the record the bound from before it",
			revisePlanReview(func(t testing.TB, f *githubtest.Fake) {
				// The owner shows as a collaborator, so the record's resolved
				// is their /resume's time (reviewDirtyResumeAt).
				f.SetPermission("owner", "admin")
				post(t, f, 1005, "owner", "/hold")
				f.AddReaction(1005, app, "+1")
				post(t, f, 1007, app, moved("On hold by owner, from `plan-review`.", "from=plan-review to=held at=plan-review reason=owner cmd=1005"))
				f.AddLabel(1, app, "stage:held")
				f.RemoveLabel(1, app, "stage:plan-review")
				post(t, f, 1008, "owner", "/resume")
				f.AddReaction(1008, app, "+1")
				post(t, f, 1010, app, moved("Resumed by owner, back to `plan-review`.", "from=held to=plan-review cmd=1008 seen=1001"))
				f.AddLabel(1, app, "stage:plan-review")
				f.RemoveLabel(1, app, "stage:held")
				f.EditComment(1010, "owner", "Edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:plan-review"}, []string{planResumeDirtyOn(t, f, 1010, 1008, 1001)}}
			}},
		{"a /resume back to planning, with the plan after it, still disagrees",
			planDirtyResumedToPlanning,
			func(testing.TB, *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:held"}, []string{planDirtyDisagreed(state.Held, "`stage:plan-review`", 9008)}}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolveCheck(t, tt.setup, 1, func(f *githubtest.Fake) issueWant { return tt.want(t, f) })
		})
	}
}

// planDirtyResumedToPlanning files issue 1, the owner's epic with body
// "body", held from planning and resumed back there, then at plan-review
// by the planner's move: spec revision 1 (comment 1001), approved by the
// owner's 👍 (reaction 1002, marker 1003), the owner's /hold (1004), 👍'd
// by the App, and its move (1006), the owner's /resume (1007), 👍'd too,
// and its move (1009), and plan revision 1 (1010). The App moved the
// labels with each move, plan-review's in event 9008. Then the owner edits
// the /resume's move and the plan comment, so the markers left show the
// hold, from planning.
func planDirtyResumedToPlanning(t testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Epic")
		f.AddLabel(1, app, "stage:spec-review")
		post(t, f, 1001, app, revision(1, "body"))
		f.AddReaction(1001, "owner", "+1")
		post(t, f, 1003, app, approvedRev(1, "owner's 👍", "r1002", "1001"))
		f.AddLabel(1, app, "stage:planning")
		f.RemoveLabel(1, app, "stage:spec-review")
		post(t, f, 1004, "owner", "/hold")
		f.AddReaction(1004, app, "+1")
		post(t, f, 1006, app, moved("On hold by owner, from `planning`.", "from=planning to=held at=planning reason=owner cmd=1004 seen=1001"))
		f.AddLabel(1, app, "stage:held")
		f.RemoveLabel(1, app, "stage:planning")
		post(t, f, 1007, "owner", "/resume")
		f.AddReaction(1007, app, "+1")
		post(t, f, 1009, app, moved("Resumed by owner, back to `planning`.", "from=held to=planning cmd=1007 seen=1001"))
		f.AddLabel(1, app, "stage:planning")
		f.RemoveLabel(1, app, "stage:held")
		post(t, f, 1010, app, revisePlanComment(1, "1009"))
		labelEvent(t, f.AddLabel(1, app, "stage:plan-review"), 9008)
		f.RemoveLabel(1, app, "stage:planning")
		f.EditComment(1009, "owner", "Edited.")
		f.EditComment(1010, "owner", "Edited.")
	})(t)
}

// The owner's edit of the App's comment applying a /resume, and mallory's
// comment after it, in one poll (#566): the pass records the move back to
// plan-review again, with the /resume's time as resolved, which bounds the
// discussion triggers as the hidden move's /resume did (discFloor), so
// mallory's comment, after the /resume, still holds the issue from
// plan-review. The bound is in the record, so it stands once the dirty
// comment is deleted. With two dirty App comments after the hold, it's
// still the /resume's time, before both, so a comment between them, which
// a lagging read didn't hold for, holds the issue too (#595).
func TestPlanDirtyResumeTrigger(t *testing.T) {
	t.Parallel()
	held := []string{"stage:held", "type:epic"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  func(t testing.TB, f *githubtest.Fake) issueWant
	}{
		{"mallory's comment in the poll of the edit holds the issue",
			planResumeDirty(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1010, "mallory", planHoldMallory) }),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{held, []string{planResumeDirtyOn(t, f, 1008, 1007, 1003),
					discHeld("plan-review", "`mallory` posted comment 1010",
						"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1010 seen=1003")}}
			}},
		// The record's pass read the issue before mallory's comment, as a
		// lagging read may, and then the owner deletes the dirty comment.
		{"and one before the record, once the dirty comment is deleted",
			planResumeDirty(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1010, "mallory", planHoldMallory)
				post(t, f, 1011, app, planResumeDirtyOn(t, f, 1008, 1007, 1003))
				f.DeleteCommentBy("owner", 1008)
			}),
			func(testing.TB, *githubtest.Fake) issueWant {
				return issueWant{held, []string{discHeld("plan-review", "`mallory` posted comment 1010",
					"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1010 seen=1003")}}
			}},
		{"a comment between two dirty App comments holds the issue",
			revisePlanReview(func(t testing.TB, f *githubtest.Fake) {
				f.SetPermission("mallory", "write")
				// The owner shows as a collaborator, so the record's resolved
				// is their /resume's time (reviewDirtyResumeAt).
				f.SetPermission("owner", "admin")
				post(t, f, 1005, "owner", "/hold")
				f.AddReaction(1005, app, "+1")
				post(t, f, 1007, app, moved("On hold by owner, from `plan-review`.", "from=plan-review to=held at=plan-review reason=owner cmd=1005 seen=1001"))
				f.AddLabel(1, app, "stage:held")
				f.RemoveLabel(1, app, "stage:plan-review")
				post(t, f, 1008, "owner", "/resume")
				f.AddReaction(1008, app, "+1")
				post(t, f, 1010, app, moved("Resumed by owner, back to `plan-review`.", "from=held to=plan-review cmd=1008 seen=1001"))
				f.AddLabel(1, app, "stage:plan-review")
				f.RemoveLabel(1, app, "stage:held")
				post(t, f, 1011, "mallory", planHoldMallory)
				post(t, f, 1012, app, "Noted.")
				f.EditComment(1010, "owner", "Edited.")
				f.EditComment(1012, "owner", "Edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:held"}, []string{planResumeDirtyOn(t, f, 1012, 1008, 1001),
					discHeld("plan-review", "`mallory` posted comment 1011",
						"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1011 seen=1001")}}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolveCheck(t, tt.setup, 1, func(f *githubtest.Fake) issueWant { return tt.want(t, f) })
		})
	}
}

// planResumeApplied is revisePlanReview held by the owner's /hold (comment
// 1005), 👍'd by the App, its move in 1007, then the owner's /resume
// (1008), 👍'd too, applied in 1010, with the App's labels for each move,
// the owner a collaborator and mallory a writer. Then it runs more.
func planResumeApplied(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return revisePlanReview(func(t testing.TB, f *githubtest.Fake) {
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		post(t, f, 1005, "owner", "/hold")
		f.AddReaction(1005, app, "+1")
		post(t, f, 1007, app, moved("On hold by owner, from `plan-review`.", "from=plan-review to=held at=plan-review reason=owner cmd=1005 seen=1001"))
		f.AddLabel(1, app, "stage:held")
		f.RemoveLabel(1, app, "stage:plan-review")
		post(t, f, 1008, "owner", "/resume")
		f.AddReaction(1008, app, "+1")
		post(t, f, 1010, app, moved("Resumed by owner, back to `plan-review`.", "from=held to=plan-review cmd=1008 seen=1001"))
		f.AddLabel(1, app, "stage:plan-review")
		f.RemoveLabel(1, app, "stage:held")
		more(t, f)
	})
}

// planResumeAccepted is planHeld by /hold (comment 1004, its move in 1005),
// then mallory's comment 1007 during the hold, and the owner's /resume
// (1008), which a pass applies, in 1009, back to plan-review, as it accepts
// every open trigger before it (SPEC §6.4). Then the owner edits 1009, so
// it's dirty, and more runs.
func planResumeAccepted(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return planHeld(true, func(t testing.TB, f *githubtest.Fake) {
		post(t, f, 1007, "mallory", planHoldMallory)
		post(t, f, 1008, "owner", "/resume")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if id := discRunFind(t, f, 1, "from=held to=plan-review cmd=1008 "); id != 1009 {
			t.Fatalf("the /resume's move back to plan-review is in comment %d; want 1009", id)
		}
		f.EditComment(1009, "owner", "Resumed, edited.")
		more(t, f)
	})
}

// The record of a /resume's move that a dirty comment hid is resolved when
// the /resume was posted, floored at the hold (#595): the hidden move's cmd
// bounded the discussion triggers so (discFloor), and the dirty comment
// can't say which /resume it was, so it's the newest clean one by a trusted
// user, below the dirty comment, that the App didn't reject. So a trigger
// after the /resume holds the issue: one before a later dirty App comment,
// once the owner deletes the App's comment applying the /resume, with both
// in one poll, and one between the /resume and that comment, which that
// pass's read missed. A comment during the owner hold, which the /resume
// accepted, doesn't hold the issue again, but holds it once that /resume is
// deleted too, as the floor is then the hold, failing closed. A /resume the
// App rejected isn't the one, nor is an earlier one, which a trigger after
// it outlived, nor one by someone untrusted, nor one from before the hold,
// which leaves the floor at the hold: a trigger the hold recorded, which
// the deleted /resume accepted, doesn't count again.
func TestPlanDirtyResumeFloor(t *testing.T) {
	t.Parallel()
	held, atReview := []string{"stage:held", "type:epic"}, []string{"stage:plan-review", "type:epic"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  func(t testing.TB, f *githubtest.Fake) issueWant
	}{
		{"the /resume's move deleted, mallory's comment and a later App comment edited, in one poll",
			planResumeApplied(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1011, "mallory", planHoldMallory)
				post(t, f, 1012, app, "Noted.")
				f.DeleteCommentBy("owner", 1010)
				f.EditComment(1012, "owner", "Edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:held"}, []string{planResumeDirtyOn(t, f, 1012, 1008, 1001),
					discHeld("plan-review", "`mallory` posted comment 1011",
						"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1011 seen=1001")}}
			}},
		{"a comment between the /resume and the App's comment applying it",
			revisePlanReview(func(t testing.TB, f *githubtest.Fake) {
				f.SetPermission("owner", "admin")
				f.SetPermission("mallory", "write")
				post(t, f, 1005, "owner", "/hold")
				f.AddReaction(1005, app, "+1")
				post(t, f, 1007, app, moved("On hold by owner, from `plan-review`.", "from=plan-review to=held at=plan-review reason=owner cmd=1005 seen=1001"))
				f.AddLabel(1, app, "stage:held")
				f.RemoveLabel(1, app, "stage:plan-review")
				post(t, f, 1008, "owner", "/resume")
				f.AddReaction(1008, app, "+1")
				post(t, f, 1010, "mallory", planHoldMallory)
				post(t, f, 1011, app, moved("Resumed by owner, back to `plan-review`.", "from=held to=plan-review cmd=1008 seen=1001"))
				f.AddLabel(1, app, "stage:plan-review")
				f.RemoveLabel(1, app, "stage:held")
				f.EditComment(1011, "owner", "Edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:held"}, []string{planResumeDirtyOn(t, f, 1011, 1008, 1001),
					discHeld("plan-review", "`mallory` posted comment 1010",
						"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1010 seen=1001")}}
			}},
		{"a comment during the owner hold, which the /resume accepted, doesn't hold the issue again",
			planResumeAccepted(func(testing.TB, *githubtest.Fake) {}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{atReview, []string{planResumeDirtyOn(t, f, 1009, 1008, 1003)}}
			}},
		{"but holds it once that /resume is deleted, at the hold",
			planResumeAccepted(func(_ testing.TB, f *githubtest.Fake) { f.DeleteCommentBy("owner", 1008) }),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{held, []string{planResumeDirtyOn(t, f, 1009, 1005, 1003),
					discHeld("plan-review", "`mallory` posted comment 1007",
						"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1007 seen=1003")}}
			}},
		{"a /resume the App rejected isn't the one",
			planResumeApplied(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1011, "mallory", planHoldMallory)
				post(t, f, 1012, "owner", "/resume")
				post(t, f, 1013, app, rejected("/resume", "nothing here or in its open sub-issues is on an owner hold", "1012"))
				post(t, f, 1014, app, "Noted.")
				f.DeleteCommentBy("owner", 1010)
				f.EditComment(1014, "owner", "Edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:held"}, []string{planResumeDirtyOn(t, f, 1014, 1008, 1001),
					discHeld("plan-review", "`mallory` posted comment 1011",
						"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1011 seen=1001")}}
			}},
		// The owner's first /resume (1007) ends the hold, but mallory's
		// comment after it keeps the issue held, as 1010 records, and the
		// second /resume (1009) accepts it, in 1012.
		{"of two /resumes, it's the newest",
			planHeld(true, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1007, "owner", "/resume")
				post(t, f, 1008, "mallory", planHoldMallory)
				post(t, f, 1009, "owner", "/resume")
				if err := resolvePass(t, 1)(f); err != nil {
					t.Fatal(err)
				}
				if id := discRunFind(t, f, 1, "<!-- issuedog:ended reason=discussion cmd=1007 "); id != 1010 {
					t.Fatalf("the first /resume's record is in comment %d; want 1010", id)
				}
				if id := discRunFind(t, f, 1, "from=held to=plan-review cmd=1009 "); id != 1012 {
					t.Fatalf("the second /resume's move back to plan-review is in comment %d; want 1012", id)
				}
				f.EditComment(1012, "owner", "Resumed, edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{atReview, []string{planResumeDirtyOn(t, f, 1012, 1009, 1003)}}
			}},
		{"nor is a /resume by someone untrusted",
			planResumeApplied(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1011, "mallory", planHoldMallory)
				post(t, f, 1012, "eve", "/resume")
				post(t, f, 1013, app, "Noted.")
				f.DeleteCommentBy("owner", 1010)
				f.EditComment(1013, "owner", "Edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{[]string{"stage:held"}, []string{planResumeDirtyOn(t, f, 1013, 1008, 1001),
					discHeld("plan-review", "`mallory` posted comment 1011",
						"from=plan-review to=held at=plan-review origin=plan-review reason=discussion event=c1011 seen=1001")}}
			}},
		// The owner's first /resume (1007) ends the first hold, in 1008.
		// Mallory's comment and the owner's /hold, in one poll, hold the
		// issue for discussion, in 1012, which records mallory's comment,
		// then the second /resume (1014) accepts it, in 1015. With that
		// /resume deleted, the first is from before the hold.
		{"a /resume from before the hold leaves the floor at the hold",
			planHeld(true, func(t testing.TB, f *githubtest.Fake) {
				pass := func() {
					if err := resolvePass(t, 1)(f); err != nil {
						t.Fatal(err)
					}
				}
				post(t, f, 1007, "owner", "/resume")
				pass()
				post(t, f, 1010, "mallory", planHoldMallory)
				post(t, f, 1011, "owner", "/hold")
				pass()
				if !strings.Contains(newestComment(f, 1), "reason=discussion event=c1010 ") {
					t.Fatalf("newest comment %q; want the discussion hold for comment 1010", newestComment(f, 1))
				}
				post(t, f, 1014, "owner", "/resume")
				pass()
				if id := discRunFind(t, f, 1, "from=held to=plan-review cmd=1014 "); id != 1015 {
					t.Fatalf("the second /resume's move back to plan-review is in comment %d; want 1015", id)
				}
				f.DeleteCommentBy("owner", 1014)
				f.EditComment(1015, "owner", "Resumed, edited.")
			}),
			func(t testing.TB, f *githubtest.Fake) issueWant {
				return issueWant{atReview, []string{planResumeDirtyOn(t, f, 1015, 1012, 1003)}}
			}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			resolveCheck(t, tt.setup, 1, func(f *githubtest.Fake) issueWant { return tt.want(t, f) })
		})
	}
}
