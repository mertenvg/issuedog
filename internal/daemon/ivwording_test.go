package daemon_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// These are #229's tests: a question's bold, the letters and options the
// interviewer's checks hold its output to, and the rejection and lost-reply
// texts after an edit.

// ivWordingAsked posts round 1 of ivRound(1, "") on issue n, readable as
// SPEC §9.1 posts it, having seen comment seen, then login's answer, and
// returns the answer's ID.
func ivWordingAsked(f *githubtest.Fake, n int, seen int64, login, answer string) int64 {
	f.AddComment(n, app, ivRoundText(1, 1)+"\n\n"+ivRoundMarker(1, 1, "iv1", seen))
	return f.AddComment(n, login, answer)
}

// ivWordingDecision is a decision on Q1.1 by alice, in round 1, with answer.
func ivWordingDecision(answer string) map[string]any {
	return map[string]any{"question": "Q1.1", "answer": answer, "summary": "Yes.", "by": "alice", "round": 1}
}

// ivWordingDuplicated is ivRound(1, ""), with others as its question's
// options after the recommended "Yes".
func ivWordingDuplicated(others ...string) map[string]any {
	return map[string]any{"outcome": "questions", "questions": []map[string]any{{"text": "Q1?", "blocks": []int{1},
		"recommended": map[string]any{"text": "Yes", "why": "it's simplest"}, "others": others}}}
}

// TestIvWordingCheck: a decision's letter must name one of its question's
// options, as its round comment reads back, and a question's options must
// differ, ignoring case and spacing. Each broken rule names its cause.
func TestIvWordingCheck(t *testing.T) {
	t.Parallel()
	readable := func(f *githubtest.Fake, n int, entry int64) {
		f.AddLabel(n, "owner", "type:epic")
		ivWordingAsked(f, n, entry, "alice", "1b")
	}
	unreadable := func(f *githubtest.Fake, n int, entry int64) {
		f.AddLabel(n, "owner", "type:epic")
		ivAsked(f, n, 1, 1, entry)
		f.AddComment(n, "alice", "1d")
	}
	epic := func(f *githubtest.Fake, n int, _ int64) { f.AddLabel(n, "owner", "type:epic") }
	tests := []struct {
		name  string
		setup func(f *githubtest.Fake, n int, entry int64)
		out   map[string]any
		want  string // in the error, or "" for none
	}{
		{"a letter past a question's 2 options", readable, ivSpec(ivWordingDecision("c")),
			`rule decision-answer: decision 1: Q1.1 had 2 options, a to b, so it has no option "c"`},
		{"the last of them", readable, ivSpec(ivWordingDecision("b")), ""},
		{"free text", readable, ivSpec(ivWordingDecision("free")), ""},
		{"a letter on a round whose comment doesn't read back", unreadable, ivSpec(ivWordingDecision("d")), ""},
		{"the recommended option twice", epic, ivWordingDuplicated(" yes "),
			`rule options: question 1: option b is the same as option a, "Yes"`},
		{"two other options alike but for case and spacing", epic, ivWordingDuplicated("Both  ways", "No", "both ways"),
			`rule options: question 1: option d is the same as option b, "Both  ways"`},
		{"options that differ", epic, ivWordingDuplicated("No", "Both", "Neither"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, entry := ivFiled(f, "", false, "from=intake to=clarifying event=5")
			tt.setup(f, n, entry)
			d := ivDaemon(t, f, newOrigin(t))
			d.Config.Types = "labels"
			in := ivInput(t, d, n)
			err := (&daemon.Interviewer{Daemon: d}).Check(context.Background(), daemon.Result{RunID: "run1", S: in.S, Dirt: in.Dirt, Output: ivJSON(t, tt.out)})
			if tt.want == "" && err != nil || tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)) {
				t.Errorf("Check = %v, want %q", err, tt.want)
			}
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("Check wrote %v", w)
			}
		})
	}
}

// TestIvWordingFailsAttempt: output with a letter past its question's
// options, or with duplicate options, is a failed attempt (SPEC §8.3),
// with nothing posted, and the lease names the rule broken.
func TestIvWordingFailsAttempt(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		answer string // posted after a readable round 1, if set
		out    map[string]any
		want   string
	}{
		{"a letter past the options", "1c", ivSpec(ivWordingDecision("c")), "rule decision-answer"},
		{"duplicate options", "", ivWordingDuplicated("YES"), "rule options"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n, entry := ivFiled(f, "epic", false, "from=intake to=clarifying event=5")
			if tt.answer != "" {
				// Step 1 has 👍'd the answer, so the run is all that writes.
				f.AddReaction(ivWordingAsked(f, n, entry, "alice", tt.answer), app, "+1")
			}
			g := ivRig(t, f)
			g.claude(ivResult(t, tt.out), 0)
			before := f.State()
			if err := g.work(n); err != nil {
				t.Fatal(err)
			}
			if after := f.State(); !reflect.DeepEqual(after.Issues, before.Issues) {
				t.Errorf("the issue changed:\n%+v\nwant\n%+v", after.Issues, before.Issues)
			}
			if l := g.lease(); g.runs() != 1 || !l.Failed || l.Attempt != 1 || l.ApplyFailures != 0 || !strings.Contains(l.PublicFailure, tt.want) {
				t.Errorf("after %d runs, lease %+v; want one failed attempt, for %q", g.runs(), l, tt.want)
			}
		})
	}
}

// TestIvWordingApproveRejections: /approve's reply at spec-review after an
// edit says whether a revision follows, and what the owner can do, and
// never asks for a new comment: a revision follows only a trusted edit,
// and then the visit posts it after the reply (#229).
func TestIvWordingApproveRejections(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:spec-review"}
	h := state.BodyHash("body")
	untrusted := func(why string) string {
		return why + ". The description was last edited by mallory, who isn't trusted, so I won't post a revision of it. " +
			"To get one, edit the description yourself, or post `/revise <notes>`, or any other comment, for a new spec"
	}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"an untrusted author's own edit: no revision follows",
			authored("mallory", func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body v2")
				post(t, f, 1002, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingApproveFinal(untrusted("the issue description changed after revision 1, "+
				"so it no longer matches that revision's hash, `"+h+"`"), "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"no revision, on an untrusted author's body: none follows",
			func(t testing.TB) *githubtest.Fake {
				f := githubtest.New(t, "o/r", slug)
				f.SetPermission("owner", "admin")
				f.AddIssue("mallory", "Title", "body")
				f.SetType(1, "Story")
				f.AddLabel(1, app, "stage:spec-review")
				post(t, f, 1001, app, "<!-- issuedog:transition from=clarifying to=spec-review run=a -->")
				post(t, f, 1002, "owner", "/approve")
				return f
			},
			outcome{comments: []string{ivWordingApproveFinal(untrusted("there's no revision to approve"), "1002")},
				labels: atReview, downs: []string{"/approve"}}},
		{"a hidden revision, on an untrusted author's body: none follows",
			authored("mallory", func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "Noted.")
				f.EditComment(1002, "mallory", forged("Noted."))
				post(t, f, 1003, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingApproveFinal(untrusted("a comment of mine after revision 1 was edited without approval or deleted, "+
				"so it may hide a newer revision"), "1003")}, labels: atReview, downs: []string{"/approve"}}},
		{"the owner's edit: the revision follows the reply",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "owner", "body v2")
				post(t, f, 1002, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingApproveFinal("the issue description changed after revision 1, "+
				"so it no longer matches that revision's hash, `"+h+"`"+ivWordingFollows, "1002"), posted(2, "body v2", "", "", "1002")},
				labels: atReview, downs: []string{"/approve"}}},
		{"an /approve posted before a revision that's stale since: the reply doesn't say to approve that (#399)",
			authored("mallory", func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/approve")
				post(t, f, 1003, app, revision(2, "body"))
				f.EditBody(1, "mallory", "body v2")
			}),
			outcome{comments: []string{ivWordingApproveFinal(untrusted("the issue description changed after revision 2, "+
				"so it no longer matches that revision's hash, `"+h+"`"), "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"an untrusted edit that makes the body dirty: no revision follows",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body v2")
				post(t, f, 1002, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingApproveFinal(dirtyBody, "1002")}, labels: atReview, downs: []string{"/approve"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var in []string
			check(t, tt.setup, visit(t, owners, &in), tt.want)
		})
	}
}

// TestIvWordingRepliesMatchByMarker: a reply already posted with an
// earlier wording, before #229, still counts by its reply marker, so
// nothing is posted again, and its 👎 isn't added twice.
func TestIvWordingRepliesMatchByMarker(t *testing.T) {
	t.Parallel()
	old := "> /approve\n\nI didn't apply this command: the issue description changed after revision 1, " +
		"so it no longer matches that revision's hash, `" + state.BodyHash("body") + "`. Post a new comment to try again.\n\n" +
		"<!-- issuedog:reply reason=rejected cmd=1002 -->"
	for _, tt := range []struct {
		name, body string // body is the /approve's text in the end
	}{
		{"with its 👎", "/approve"},
		{"edited after its 👎", "/approve now"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := authored("mallory", func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body v2")
				post(t, f, 1002, "owner", "/approve")
				f.AddReaction(1002, app, "-1")
				post(t, f, 1004, app, old)
				if tt.body != "/approve" {
					f.EditComment(1002, "owner", tt.body)
				}
			})
			var in []string
			check(t, setup, visit(t, owners, &in), outcome{labels: []string{"stage:spec-review"}, downs: []string{tt.body}})
		})
	}
}

// ivWordingLostFinal is the reply posted again to a 👎'd comment, quoted,
// edited after the 👎, whose reply was lost, when its new text would be
// rejected for reason, a Final one: it asks for no new comment (#399).
func ivWordingLostFinal(quote, reason, id string) string {
	return final(quote, "I didn't act on this comment, and my reply saying why was lost. "+
		"It was edited after I rejected it, so I've ignored the edit. Its new text wouldn't apply either", reason, id)
}

// TestIvWordingLostReplyAfterEdit: a 👎'd command whose reply was lost
// gets it again, worded from the issue as it is, unless it was edited
// after its 👎: then the edit is ignored, and the reply says so, rather
// than judging the edited text (#229). A request edited so is answered
// the same way. The reply asks for a new comment only when one could
// apply: when the new text would be rejected for a reason a new comment
// can't change, it gives that reason instead (#399, SPEC §9.2). A reason
// that holds only for when the comment was posted, before the latest
// revision or at a stage since moved on, is one a new comment changes, so
// the reply asks for one (#474).
func TestIvWordingLostReplyAfterEdit(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:spec-review"}
	storyOnly := "only an epic, or a story with no parent, goes from `spec-review` to `clarifying` on a revision request, and this is a story issue"
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"an /approve edited after its 👎 into one that would apply",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body v2")
				post(t, f, 1002, "owner", "/approve")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "/revise shorter")
			}),
			outcome{comments: []string{ivWordingLostEdited("/revise shorter", "1002")}, labels: atReview, downs: []string{"/revise shorter"}}},
		{"a request edited after its 👎",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.SetParent(1, f.AddIssue("owner", "Epic", "epic"))
				post(t, f, 1002, "owner", "too long")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "too short")
			}),
			outcome{comments: []string{ivWordingLostFinal("too short", storyOnly, "1002")}, labels: atReview, downs: []string{"too short"}}},
		{"a request edited after its 👎 into a command that would apply",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingLostEdited("/approve", "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"an /approve edited after its 👎 into one rejected the same way",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body v2")
				post(t, f, 1002, "owner", "/approve x")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingLostFinal("/approve", dirtyBody, "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"an /approve edited after its 👎, posted before the latest revision: a new one could apply, so the reply asks",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/approve x")
				f.AddReaction(1002, app, "-1")
				post(t, f, 1004, app, revision(2, "body"))
				f.EditComment(1002, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingLostEdited("/approve", "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"likewise at plan-review, before the latest plan revision",
			planReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1004, "owner", "/approve x")
				f.AddReaction(1004, app, "-1")
				replan(t, f, 2, 1006)
				f.EditComment(1004, "owner", "/approve")
			}),
			outcome{comments: []string{ivWordingLostEdited("/approve", "1004")}, labels: []string{"stage:plan-review"}, downs: []string{"/approve"}}},
		{"a request edited after its 👎, posted at a review stage since moved on to another: a new one could apply there, so the reply asks",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "too short")
				handSet(t, f, state.SpecReview, state.PlanReview, 9003)
			}),
			outcome{comments: []string{overridden(state.PlanReview, "from=spec-review to=plan-review by=owner event=9003"), ivWordingLostEdited("too short", "1002")},
				labels: []string{"stage:plan-review"}, downs: []string{"too short"}}},
		{"a request edited after its 👎, posted at a stage since moved on to planning: a new one couldn't apply either, so the reply gives the reason",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "too short")
				handSet(t, f, state.SpecReview, state.Planning, 9003)
			}),
			outcome{comments: []string{overridden(state.Planning, "from=spec-review to=planning by=owner event=9003"),
				ivWordingLostFinal("too short", movedOnReason(state.SpecReview, state.Planning), "1002")},
				labels: []string{"stage:planning"}, downs: []string{"too short"}}},
		{"likewise moved on to in-progress",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "too short")
				handSet(t, f, state.SpecReview, state.InProgress, 9003)
			}),
			outcome{comments: []string{overridden(state.InProgress, "from=spec-review to=in-progress by=owner event=9003"),
				ivWordingLostFinal("too short", movedOnReason(state.SpecReview, state.InProgress), "1002")},
				labels: []string{"stage:in-progress"}, downs: []string{"too short"}}},
		{"a command edited after its 👎 into plain text, posted at a stage since moved on: no request, so the reply asks",
			filed(nil, func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Story")
				labelEvent(t, f.AddLabel(1, app, "stage:spec-review"), 9001)
				post(t, f, 1001, app, "<!-- issuedog:transition from=intake to=clarifying run=a -->")
				post(t, f, 1002, "owner", "/frob")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "too long")
				post(t, f, 1004, app, revision(1, "body"))
			}),
			outcome{comments: []string{ivWordingLostEdited("too long", "1002")}, labels: atReview, downs: []string{"too long"}}},
		{"an /approve edited after its 👎 into a bad argument, which a new comment can fix",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/approve")
				f.AddReaction(1002, app, "-1")
				f.EditComment(1002, "owner", "/approve x")
			}),
			outcome{comments: []string{ivWordingLostEdited("/approve x", "1002")}, labels: atReview, downs: []string{"/approve x"}}},
		{"an /approve edited before its 👎 is worded again from the issue",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body v2")
				post(t, f, 1002, "owner", "/approve x")
				f.EditComment(1002, "owner", "/approve")
				f.AddReaction(1002, app, "-1")
			}),
			outcome{comments: []string{ivWordingApproveFinal(dirtyBody, "1002")}, labels: atReview, downs: []string{"/approve"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var in []string
			check(t, tt.setup, visit(t, owners, &in), tt.want)
		})
	}
}

// TestIvWordingPlanDirtyBody: at plan-review, /approve's reply for a dirty
// body says a new plan won't clear the edit, and asks for no new comment.
func TestIvWordingPlanDirtyBody(t *testing.T) {
	t.Parallel()
	want := ivWordingApproveFinal("the issue description has an edit by mallory that wasn't approved, so I can't approve the plan. "+
		"Nothing at `planning` or `plan-review` rewrites the description, so sending the plan back won't clear the edit. "+
		"To move on, set the stage by hand", "1005")
	setup := planReview(func(t testing.TB, f *githubtest.Fake) {
		f.EditBody(1, "mallory", noted("body"))
		f.AddReaction(1003, "owner", "+1") // 1004, and it approves nothing
		post(t, f, 1005, "owner", "/approve")
	})
	check(t, setup, approvePass(t), outcome{comments: []string{want}, labels: []string{"stage:plan-review"}, downs: []string{"/approve"}})
}
