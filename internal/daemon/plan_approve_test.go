package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Approving a plan by 👍 or /approve (#268): at plan-review, a trusted 👍
// on the latest plan revision, or a trusted /approve after it, moves the
// issue to planned, as a spec's approval moves it to planning. Plan
// comments are posted as the planner's run posts them, by a move with
// Move.PlanRev.

// planText is the text of plan revision rev of issue n.
func planText(n, rev int) string {
	return fmt.Sprintf("### Plan for #%d (rev %d)\n\nT1. Do it.", n, rev)
}

// planAbove is plan revision rev's comment above its last line, as a move
// with PlanRev from planning writes it: the text and the transition
// marker, keyed run=p<rev>.
func planAbove(n, rev int) string {
	return fmt.Sprintf("%s\n\n<!-- issuedog:transition from=planning to=plan-review rev=%d run=p%d -->", planText(n, rev), rev, rev)
}

// planHash is plan revision rev's hash (state.PlanHash).
func planHash(n, rev int) string { return state.BodyHash(planAbove(n, rev)) }

// planComment is plan revision rev's whole comment.
func planComment(n, rev int) string {
	return fmt.Sprintf("%s\n<!-- issuedog:revision kind=plan rev=%d body=%s -->", planAbove(n, rev), rev, planHash(n, rev))
}

// moveOn applies m to issue n from a fresh client, as a run's move would,
// and fails t unless its comment's ID is want.
func moveOn(t testing.TB, f *githubtest.Fake, n int, m daemon.Move, want int64) {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	a := &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}
	if err := a.Transition(context.Background(), s, clean, m); err != nil {
		t.Fatal(err)
	}
	if got := s.Comments[len(s.Comments)-1].ID; got != want {
		t.Fatalf("move's comment ID = %d, want %d", got, want)
	}
}

// postPlan posts plan revision rev on issue n, with the planner's move from
// planning to plan-review, as comment want.
func postPlan(t testing.TB, f *githubtest.Fake, n, rev int, want int64) {
	t.Helper()
	moveOn(t, f, n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid,
		Key: state.Key{Field: "run", ID: fmt.Sprintf("p%d", rev)}, PlanRev: rev, Text: planText(n, rev)}, want)
}

// replan sends issue 1 back to planning, comment back, and posts plan
// revision rev, comment back+1.
func replan(t testing.TB, f *githubtest.Fake, rev int, back int64) {
	t.Helper()
	moveOn(t, f, 1, daemon.Move{To: state.Planning, Trigger: state.TriggerPlanRevise, Key: state.Key{Field: "run", ID: fmt.Sprintf("b%d", rev)}}, back)
	postPlan(t, f, 1, rev, back+1)
}

// planReview files issue 1, an epic with body "body", at planning, set by
// the App: label event 9001, comment 1001, revision 1 of its spec, and
// comment 1002, its approval. Comment 1003 is plan revision 1, posted with
// the planner's move to plan-review. Then it runs more, and forgets the
// writes, so a pass's are counted from 1.
func planReview(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Epic")
		labelEvent(t, f.AddLabel(1, app, "stage:planning"), 9001)
		post(t, f, 1001, app, revision(1, "body"))
		post(t, f, 1002, app, approvedRev(1, "owner's 👍", "r9", "1001"))
		postPlan(t, f, 1, 1, 1003)
		more(t, f)
		f.ForgetWrites()
	})
}

// planApproved is the comment approving plan revision rev of issue 1, by
// key, with seen.
func planApproved(rev int, by, key, seen string) string {
	return fmt.Sprintf("Revision %d of the plan is approved, by %s.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planned rev=%d body=%s cmd=%s seen=%s -->", rev, by, rev, planHash(1, rev), key, seen)
}

const replanHint = "To get a new plan, post `/revise`, or any other comment"

func TestApprovePlan(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:plan-review"}
	planned := []string{"stage:planned"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"a trusted 👍 on the latest plan revision approves it",
			planReview(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1003, "owner", "+1") }),
			outcome{comments: []string{planApproved(1, "owner's 👍", "r1004", "1003")}, labels: planned}},
		{"the earliest trusted 👍 approves, not the App's, another bot's, an untrusted one or a 👎",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1003, app, "+1")
				f.AddReaction(1003, "helper[bot]", "+1")
				f.AddReaction(1003, "mallory", "+1")
				f.AddReaction(1003, "owner", "-1")
				f.AddReaction(1003, "owner2", "+1") // 1008
				f.AddReaction(1003, "owner", "+1")
			}),
			outcome{comments: []string{planApproved(1, "owner2's 👍", "r1008", "1003")}, labels: planned, acks: []string{planComment(1, 1)}}},
		{"its seen is the newest comment read",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "Looks good.")
				f.AddReaction(1003, "owner", "+1")
			}),
			outcome{comments: []string{planApproved(1, "owner's 👍", "r1005", "1004")}, labels: planned}},
		{"a 👍 on an earlier plan revision approves nothing",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1003, "owner", "+1")
				replan(t, f, 2, 1005)
			}),
			outcome{labels: atReview}},
		{"a 👍 on the latest of two plan revisions approves it",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1003, "owner", "+1")
				replan(t, f, 2, 1005)
				f.AddReaction(1006, "owner2", "+1")
			}),
			outcome{comments: []string{planApproved(2, "owner2's 👍", "r1007", "1006")}, labels: planned}},
		{"a 👍 on the spec revision approves nothing at plan-review",
			planReview(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") }),
			outcome{labels: atReview}},
		{"a trusted /approve after the latest plan revision approves it",
			planReview(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1004, "owner", "/approve") }),
			outcome{comments: []string{planApproved(1, "owner's `/approve`", "1004", "1004")}, labels: planned, acks: []string{"/approve"}}},
		{"an untrusted /approve is ignored",
			planReview(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1004, "mallory", "/approve") }),
			outcome{labels: atReview}},
		{"an /approve before the latest plan revision is rejected",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "/approve")
				replan(t, f, 2, 1005)
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "it was posted before plan revision 2, the latest. "+
				"To approve that, react 👍 to it, or post `/approve` again", "1004")}, labels: atReview, downs: []string{"/approve"}}},
		{"an /approve before the latest plan revision that the table can't apply gets the table's reason, which holds for a new one too",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "/approve")
				replan(t, f, 2, 1005)
				f.SetType(1, "Bug")
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "only an epic, or a story, goes from `plan-review` "+
				"to `planned` on approval, and this is a new-bug issue", "1004")}, labels: atReview, downs: []string{"/approve"}}},
		{"a dirty App comment after the plan revision hides it: a 👍 approves nothing and /approve is rejected",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, app, "Noted.")
				f.EditComment(1004, "mallory", forged("Noted."))
				f.AddReaction(1003, "owner", "+1")
				post(t, f, 1006, "owner", "/approve")
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "a comment of mine after plan revision 1 was edited "+
				"without approval or deleted, so it may hide a newer plan revision. "+replanHint, "1006")}, labels: atReview, downs: []string{"/approve"}}},
		{"a deleted App comment after the plan revision hides it too",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, app, "Noted.")
				f.DeleteCommentBy("mallory", 1004)
				f.AddReaction(1003, "owner", "+1")
			}),
			outcome{labels: atReview}},
		// A dirty plan comment hides its own move to plan-review, so the
		// issue is at plan-review again only by a later move, here the
		// owner's override (SPEC §5.1), which wins over the App's record
		// of the hidden move (#315, plan_dirty_test.go).
		{"a dirty plan comment is no revision: with no other, /approve is rejected",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, "mallory", forged(planComment(1, 1)))
				f.RemoveLabel(1, "owner", "stage:plan-review")
				f.AddLabel(1, "owner", "stage:plan-review")
				f.AddReaction(1003, "owner", "+1")
				post(t, f, 1005, "owner", "/approve")
			}),
			outcome{comments: []string{overridden(state.PlanReview, "from=planning to=plan-review by=owner event=9005"),
				final("/approve", "I didn't apply this command", "there's no clean plan revision to approve. "+replanHint, "1005")},
				labels: atReview, downs: []string{"/approve"}}},
		{"a dirty plan comment hides the plan revision before it: a 👍 on that approves nothing, and /approve is rejected",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				replan(t, f, 2, 1004)
				f.EditComment(1005, "mallory", forged(planComment(1, 2)))
				f.RemoveLabel(1, "owner", "stage:plan-review")
				f.AddLabel(1, "owner", "stage:plan-review")
				f.AddReaction(1003, "owner", "+1")
				post(t, f, 1007, "owner", "/approve")
			}),
			outcome{comments: []string{overridden(state.PlanReview, "from=planning to=plan-review by=owner event=9009"),
				final("/approve", "I didn't apply this command", "a comment of mine after plan revision 1 was edited "+
					"without approval or deleted, so it may hide a newer plan revision. "+replanHint, "1007")},
				labels: atReview, downs: []string{"/approve"}}},
		{"once the plan comment no longer matches its hash, a 👍 approves nothing and /approve is rejected",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1003, app, strings.Replace(planComment(1, 1), "Do it.", "Do it now.", 1))
				f.AddReaction(1003, "owner", "+1")
				post(t, f, 1005, "owner", "/approve")
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "the comment of plan revision 1 was edited after I posted it, "+
				"so it no longer matches that revision's hash, `"+planHash(1, 1)+"`. "+replanHint, "1005")}, labels: atReview, downs: []string{"/approve"}}},
		{"while the body is dirty, a 👍 approves nothing and /approve is rejected",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", noted("body"))
				f.AddReaction(1003, "owner", "+1")
				post(t, f, 1005, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingApproveFinal("the issue description has an edit by mallory that wasn't approved, "+
				"so I can't approve the plan. Nothing at `planning` or `plan-review` rewrites the description, "+
				"so sending the plan back won't clear the edit. To move on, set the stage by hand", "1005")}, labels: atReview, downs: []string{"/approve"}}},
		{"a trusted edit to the body leaves the plan approvable",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "owner", "body v2")
				f.AddReaction(1003, "owner", "+1")
			}),
			outcome{comments: []string{planApproved(1, "owner's 👍", "r1004", "1003")}, labels: planned}},
		{"an /approve the table can't apply is rejected, naming the classes it moves",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Bug")
				post(t, f, 1004, "owner", "/approve")
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "only an epic, or a story, goes from `plan-review` "+
				"to `planned` on approval, and this is a new-bug issue", "1004")}, labels: atReview, downs: []string{"/approve"}}},
		{"a 👍 the table can't apply is reported once, and doesn't fail the pass",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Bug")
				f.AddReaction(1003, "owner", "+1")
			}),
			outcome{comments: []string{"I can't move this issue from `plan-review` to `planned` on plan-approved: " +
				"the transition table (SPEC §7.2) has no such row for a new-bug issue. It stays at `plan-review`.\n\n" +
				"<!-- issuedog:error kind=transition key=r1004 -->"}, labels: atReview}},
		{"on an issue of an unknown type, /approve is rejected and a 👍 is ignored",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Feature")
				f.AddReaction(1003, "owner", "+1")
				post(t, f, 1005, "owner", "/approve")
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "I can't tell what kind of issue this is: "+
				"class of type \"feature\": unknown type", "1005")}, labels: atReview, downs: []string{"/approve"}}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, approvePass(t), tt.want) })
	}
}

// With no clean plan revision, /approve is rejected, saying so and how to
// get a new plan, not that there's no plan yet: the only plan comment may
// have been made dirty or deleted (#270's note). The stage is set by hand
// here, so no plan comment holds it.
func TestApprovePlanNoRevision(t *testing.T) {
	t.Parallel()
	setup := at(state.PlanReview, func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Epic")
		post(t, f, 1002, "owner", "/approve")
	})
	check(t, setup, approvePass(t), outcome{comments: []string{final("/approve", "I didn't apply this command",
		"there's no clean plan revision to approve. "+replanHint, "1002")}, labels: []string{"stage:plan-review"}, downs: []string{"/approve"}})
}

// The move into planned names the revision it approved, so
// state.ApprovedPlan reads back the plan comment it approved, by 👍 or by
// /approve.
func TestApprovePlanReadsBack(t *testing.T) {
	t.Parallel()
	for _, more := range []func(t testing.TB, f *githubtest.Fake){
		func(t testing.TB, f *githubtest.Fake) {
			replan(t, f, 2, 1004)
			f.AddReaction(1005, "owner", "+1")
		},
		func(t testing.TB, f *githubtest.Fake) {
			replan(t, f, 2, 1004)
			post(t, f, 1006, "owner", "/approve")
		},
	} {
		f := run(t, planReview(more), approvePass(t))
		_, s, err := applier(t, f)
		if err != nil {
			t.Fatal(err)
		}
		if got := state.Derive(s, slug, clean).Stage; got != state.Planned {
			t.Errorf("stage = %s, want planned", got)
		}
		rev, c, ok := state.ApprovedPlan(s, slug, clean)
		if want := (state.Revision{Kind: state.PlanRevision, Rev: 2, Hash: planHash(1, 2), Comment: 1005}); !ok || rev != want || c.Body != planComment(1, 2) {
			t.Errorf("ApprovedPlan = %+v, %q, %v; want %+v in comment %q", rev, c.Body, ok, want, planComment(1, 2))
		}
	}
}

// A 👍 changes no issue's updated_at, so the visit to an issue at
// plan-review, as at spec-review, approves its plan. An unchanged pass over
// it asks only conditional GETs that GitHub answers 304, so it costs no
// rate limit.
func TestIterateApprovesAtPlanReview(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := f.AddIssue("owner", "Epic", "body")
	f.SetType(n, "Epic")
	f.AddLabel(n, app, "stage:planning")
	f.AddComment(n, app, revision(1, "body"))
	f.AddComment(n, app, "<!-- issuedog:transition from=spec-review to=planning rev=1 body="+state.BodyHash("body")+" cmd=r9 seen=1001 -->")
	postPlan(t, f, n, 1, 1003)
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @owner @owner2\n")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	// Wired as main wires it, once trust has loaded.
	d.Step.Commands["approve"] = daemon.ApproveCommand()
	d.Step.Reactions = daemon.ApproveByReaction(d.Step.CurrentTrust())
	iterate(t, d)

	from := len(f.Requests())
	iterate(t, d)
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if f.NotModified() == 0 {
		t.Error("an unchanged pass got no 304")
	}

	f.AddReaction(1003, "owner", "+1")
	iterate(t, d)
	if got := f.State().Issues[0].Labels; !reflect.DeepEqual(got, []string{"stage:planned"}) {
		t.Errorf("labels after owner's 👍 = %v, want stage:planned", got)
	}
}
