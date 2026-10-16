package daemon_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// visit is a visit to issue 1 as the loop makes it: Run, with the approval
// and revision handlers and the owners trusted, then ReviewVisit. It keeps
// the bodies of Run's Inputs in *in.
func visit(t *testing.T, tr trusted, in *[]string) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		st := &daemon.Step{Applier: a, Trust: tr, Reactions: daemon.ApproveByReaction(tr),
			Commands: map[string]daemon.Handler{"approve": daemon.ApproveCommand(), "revise": daemon.ReviseCommand()}}
		got, err := st.Run(context.Background(), s)
		if err != nil {
			return err
		}
		*in = nil
		for _, c := range got {
			*in = append(*in, c.Body)
		}
		return st.ReviewVisit(context.Background(), s)
	}
}

var owners = trusted{"owner": true, "owner2": true}

// sentBack is the comment sending the spec back on comment id, by owner.
func sentBack(id string) string {
	return "The spec is sent back for revision, with owner's comment as the notes.\n\n" +
		"<!-- issuedog:transition from=spec-review to=clarifying cmd=" + id + " seen=" + id + " -->"
}

// final is the reply to a comment rejected for good, which suggests no new
// comment, after lead.
func final(quote, lead, reason, id string) string {
	return "> " + quote + "\n\n" + lead + ": " + reason + ".\n\n<!-- issuedog:reply reason=rejected cmd=" + id + " -->"
}

// overridden is the App's comment recording the owner's move to stage to by
// hand, with the marker's fields.
func overridden(to state.Stage, fields string) string {
	return "owner set `stage:" + string(to) + "` by hand, so I've recorded it as an owner override.\n\n" +
		"<!-- issuedog:transition " + fields + " -->"
}

// movedOn is the reply to a revision request, comment id, rejected at stage,
// after the stage moved on from spec-review.
func movedOn(quote, id string, stage state.Stage) string {
	return final(quote, "I didn't send the spec back for revision",
		"this issue is at `"+string(stage)+"` now, and a comment sends the spec back only at `spec-review`", id)
}

// lostReply is review with request 1002 👎'd but its reply lost, then the
// owner's move to stage to by hand, labeled event 9003.
func lostReply(to state.Stage) func(testing.TB) *githubtest.Fake {
	return review(func(t testing.TB, f *githubtest.Fake) {
		post(t, f, 1002, "owner", "too long")
		f.AddReaction(1002, app, "-1")
		handSet(t, f, state.SpecReview, to, 9003)
	})
}

// handSet has the owner set issue 1's stage to to by hand, from from, and
// fails t unless the labeled event's ID is want.
func handSet(t testing.TB, f *githubtest.Fake, from, to state.Stage, want int64) {
	t.Helper()
	f.RemoveLabel(1, "owner", "stage:"+string(from))
	labelEvent(t, f.AddLabel(1, "owner", "stage:"+string(to)), want)
}

// approved files issue 1 as review does, with revision 2 (comment 1003)
// after a remark (1002), approved by the owner's 👍 (reaction 1004, marker
// 1005). Then it runs more.
func approved(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return review(func(t testing.TB, f *githubtest.Fake) {
		post(t, f, 1002, "owner", "a remark")
		post(t, f, 1003, app, revision(2, "body"))
		f.AddReaction(1003, "owner", "+1")
		post(t, f, 1005, app, approvedRev(2, "owner's 👍", "r1004", "1003"))
		f.AddLabel(1, app, "stage:planning")
		f.RemoveLabel(1, app, "stage:spec-review")
		more(t, f)
	})
}

// authored files issue 1 as review does, but by author.
func authored(author string, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.AddIssue(author, "Title", "body")
		f.SetType(1, "Story")
		f.AddLabel(1, app, "stage:spec-review")
		post(t, f, 1001, app, revision(1, "body"))
		more(t, f)
		return f
	}
}

func TestReviseAndEdits(t *testing.T) {
	atReview, clarifying := []string{"stage:spec-review"}, []string{"stage:clarifying"}
	changed := "the issue description changed after revision 1, so it no longer matches that revision's hash, `" + state.BodyHash("body") + "`"
	for _, tt := range []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		trust  trusted
		want   outcome
		inputs []string
	}{
		{"/revise sends the spec back, with its comment as the notes",
			review(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/revise shorter") }), owners,
			outcome{comments: []string{sentBack("1002")}, labels: clarifying, acks: []string{"/revise shorter"}},
			[]string{"/revise shorter"}},
		{"a plain comment after the latest revision does too, and beats a 👍 in the same poll",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				post(t, f, 1003, "owner", "too long <!-- x -->")
			}), owners,
			outcome{comments: []string{sentBack("1003")}, labels: clarifying, acks: []string{"too long <!-- x -->"}},
			[]string{"too long <!-- x -->"}},
		{"a comment after the request is the new round's answer",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				post(t, f, 1003, "owner2", "and add X")
			}), owners,
			outcome{comments: []string{sentBack("1002")}, labels: clarifying, acks: []string{"too long", "and add X"}},
			[]string{"too long", "and add X"}},
		{"that answer's latest text counts until its round ends, as any answer's",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				post(t, f, 1003, "owner2", "and add X")
				post(t, f, 1004, app, sentBack("1002"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
				f.AddReaction(1002, app, "+1")
				f.AddReaction(1003, app, "+1")
				f.EditComment(1003, "owner2", "and add Y")
			}), owners,
			outcome{labels: clarifying, acks: []string{"too long", "and add Y"}}, []string{"too long", "and add Y"}},
		{"a request edited after it was used gets one reply, and is no longer the notes",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				post(t, f, 1003, app, sentBack("1002"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "owner", "too short")
			}), owners,
			outcome{comments: []string{edited("too short", "1002")}, labels: clarifying, acks: []string{"too short"}}, nil},
		{"a request made dirty after it was used is no longer the notes",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				post(t, f, 1003, app, sentBack("1002"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
				f.AddReaction(1002, app, "+1")
				f.EditComment(1002, "mallory", "too short")
			}), owners, outcome{labels: clarifying, acks: []string{forged("too short")}}, nil},
		{"a comment before the latest revision isn't a request",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "a remark")
				post(t, f, 1003, app, "<!-- issuedog:revision rev=2 body="+state.BodyHash("body")+" -->")
			}), owners, outcome{labels: atReview}, nil},
		{"nor is one from before the stage, and a missing revision is posted",
			filed(nil, func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Story")
				post(t, f, 1001, "owner", "a remark")
				f.AddLabel(1, app, "stage:spec-review")
				post(t, f, 1002, app, "<!-- issuedog:transition from=clarifying to=spec-review run=a -->")
			}), owners, outcome{comments: []string{posted(1, "body", "", "", "1002")}, labels: atReview}, nil},
		{"untrusted, App and dirty comments aren't requests",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "mallory", "too long")
				post(t, f, 1003, app, "too long")
				post(t, f, 1004, "owner", "too long")
				f.EditComment(1004, "mallory", "too long")
			}), owners, outcome{labels: atReview}, nil},
		{"a request the table can't apply is rejected",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.SetParent(1, f.AddIssue("owner", "Epic", "epic"))
				post(t, f, 1002, "owner", "too long")
			}), owners,
			outcome{comments: []string{final("too long", "I didn't send the spec back for revision", "only an epic, or a story with no parent, goes from `spec-review` "+
				"to `clarifying` on a revision request, and this is a story issue", "1002")}, labels: atReview, downs: []string{"too long"}}, nil},
		{"a 👎'd request whose reply was lost gets it again, worded the same",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.SetParent(1, f.AddIssue("owner", "Epic", "epic"))
				post(t, f, 1002, "owner", "too long")
				f.AddReaction(1002, app, "-1")
			}), owners,
			outcome{comments: []string{final("too long", "I didn't send the spec back for revision", "only an epic, or a story with no parent, goes from `spec-review` "+
				"to `clarifying` on a revision request, and this is a story issue", "1002")}, labels: atReview, downs: []string{"too long"}}, nil},
		{"a 👎'd request whose reply was lost gets it after a stage change, worded for the stage now",
			lostReply(state.Clarifying), owners,
			outcome{comments: []string{overridden(state.Clarifying, "from=spec-review to=clarifying by=owner event=9003"), movedOn("too long", "1002", state.Clarifying)},
				labels: clarifying, downs: []string{"too long"}}, nil},
		{"so does one moved on to plan-review, not saying plans aren't handled yet",
			lostReply(state.PlanReview), owners,
			outcome{comments: []string{overridden(state.PlanReview, "from=spec-review to=plan-review by=owner event=9003"), movedOn("too long", "1002", state.PlanReview)},
				labels: []string{"stage:plan-review"}, downs: []string{"too long"}}, nil},
		{"and one held by hand",
			lostReply(state.Held), owners,
			outcome{comments: []string{overridden(state.Held, "from=spec-review to=held at=spec-review reason=owner by=owner event=9003 seen=1001"), movedOn("too long", "1002", state.Held)},
				labels: []string{"stage:held"}, downs: []string{"too long"}}, nil},
		{"and one moved on to planning",
			lostReply(state.Planning), owners,
			outcome{comments: []string{overridden(state.Planning, "from=spec-review to=planning by=owner event=9003"), movedOn("too long", "1002", state.Planning)},
				labels: []string{"stage:planning"}, downs: []string{"too long"}}, nil},
		{"a comment posted at another stage is no request after a stage change",
			approved(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1006, "owner", "also Y")
				handSet(t, f, state.Planning, state.Clarifying, 9005)
			}), owners,
			outcome{comments: []string{overridden(state.Clarifying, "from=planning to=clarifying by=owner event=9005")}, labels: clarifying}, nil},
		{"a comment the owner posted after setting clarifying by hand, before the pass, gets no reply",
			review(func(t testing.TB, f *githubtest.Fake) {
				handSet(t, f, state.SpecReview, state.Clarifying, 9003)
				post(t, f, 1002, "owner", "also consider X")
			}), owners,
			outcome{comments: []string{overridden(state.Clarifying, "from=spec-review to=clarifying by=owner event=9003")}, labels: clarifying}, nil},
		{"nor one after setting planning by hand",
			review(func(t testing.TB, f *githubtest.Fake) {
				handSet(t, f, state.SpecReview, state.Planning, 9003)
				post(t, f, 1002, "owner", "plan it with Y")
			}), owners,
			outcome{comments: []string{overridden(state.Planning, "from=spec-review to=planning by=owner event=9003")}, labels: []string{"stage:planning"}}, nil},
		{"nor a request posted just before the owner held the issue by hand",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				handSet(t, f, state.SpecReview, state.Held, 9003)
			}), owners,
			outcome{comments: []string{overridden(state.Held, "from=spec-review to=held at=spec-review reason=owner by=owner event=9003 seen=1001")}, labels: []string{"stage:held"}}, nil},
		{"nor, once the stage moved on, a request by a user trusted only since",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "newbie", "looks fine to me")
				if id := f.AddReaction(1001, "owner", "+1"); id != 1003 {
					t.Fatalf("reaction ID = %d, want 1003", id)
				}
				post(t, f, 1004, app, approvedRev(1, "owner's 👍", "r1003", "1002"))
				f.AddLabel(1, app, "stage:planning")
				f.RemoveLabel(1, app, "stage:spec-review")
			}), trusted{"owner": true, "newbie": true}, outcome{labels: []string{"stage:planning"}}, nil},
		{"nor an untrusted comment, after a stage change",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "rando", "too long")
				handSet(t, f, state.SpecReview, state.Clarifying, 9003)
			}), owners,
			outcome{comments: []string{overridden(state.Clarifying, "from=spec-review to=clarifying by=owner event=9003")}, labels: clarifying}, nil},
		{"nor an answer not yet 👍'd when the owner moved the stage on by hand",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				post(t, f, 1003, app, sentBack("1002"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
				post(t, f, 1004, "owner", "add X")
				f.AddReaction(1002, app, "+1")
				handSet(t, f, state.Clarifying, state.Planning, 9005)
			}), owners,
			outcome{comments: []string{overridden(state.Planning, "from=clarifying to=planning by=owner event=9005")},
				labels: []string{"stage:planning"}, acks: []string{"too long"}}, nil},
		{"nor old comments on an issue adopted at planning",
			filed(nil, func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Story")
				post(t, f, 1001, "owner", "old remark 1")
				post(t, f, 1002, "owner", "old remark 2")
				labelEvent(t, f.AddLabel(1, "owner", "stage:planning"), 9001)
			}), owners,
			outcome{comments: []string{overridden(state.Planning, "from=intake to=planning by=owner event=9001")}, labels: []string{"stage:planning"}}, nil},
		{"a lost reply comes before the revision for a body edit",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.SetParent(1, f.AddIssue("owner", "Epic", "epic"))
				post(t, f, 1002, "owner", "too long")
				f.AddReaction(1002, app, "-1")
				f.EditBody(1, "owner", "body 2")
			}), owners,
			outcome{comments: []string{final("too long", "I didn't send the spec back for revision", "only an epic, or a story with no parent, goes from `spec-review` "+
				"to `clarifying` on a revision request, and this is a story issue", "1002"), posted(2, "body 2", "", "", "1002")},
				labels: atReview, downs: []string{"too long"}}, nil},
		{"nor a remark from before requests were handled, left before an /approve",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "nice, ship it")
				post(t, f, 1003, "owner", "/approve")
				post(t, f, 1004, app, approvedRev(1, "owner's `/approve`", "1003", "1003"))
				f.AddLabel(1, app, "stage:planning")
				f.RemoveLabel(1, app, "stage:spec-review")
				f.AddReaction(1003, app, "+1")
			}), owners, outcome{labels: []string{"stage:planning"}, acks: []string{"/approve"}}, nil},
		{"an applied request, and the answer after it, are unaffected once the spec is back at review",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				post(t, f, 1003, app, sentBack("1002"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
				post(t, f, 1004, "owner", "add X")
				post(t, f, 1005, app, revision(2, "body"))
				f.AddLabel(1, app, "stage:spec-review")
				f.RemoveLabel(1, app, "stage:clarifying")
				f.AddReaction(1002, app, "+1")
				f.AddReaction(1004, app, "+1")
			}), owners, outcome{labels: atReview, acks: []string{"too long", "add X"}}, nil},
		{"/revise at plan-review is rejected",
			at(state.PlanReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/revise shorter") }), owners,
			outcome{comments: []string{rejected("/revise shorter", "revising a plan isn't handled yet", "1002")},
				labels: []string{"stage:plan-review"}, downs: []string{"/revise shorter"}}, nil},
		{"a comment posted during the run that wrote the spec, after its seen, is a request",
			filed(nil, func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Story")
				f.AddLabel(1, app, "stage:spec-review")
				post(t, f, 1001, app, "<!-- issuedog:transition from=intake to=clarifying run=a -->")
				post(t, f, 1002, "owner", "1a")
				post(t, f, 1003, "owner", "also X")
				h := state.BodyHash("body")
				post(t, f, 1004, app, "<!-- issuedog:transition from=clarifying to=spec-review rev=1 body="+h+" run=b seen=1002 -->\n"+
					"<!-- issuedog:revision rev=1 body="+h+" -->")
			}), owners,
			outcome{comments: []string{sentBack("1003")}, labels: clarifying, acks: []string{"also X"}}, []string{"also X"}},
		{"a comment after an /approve is planning's input, with no reply",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/approve")
				post(t, f, 1003, "owner", "also Y")
			}), owners,
			outcome{comments: []string{approvedRev(1, "owner's `/approve`", "1002", "1002")}, labels: []string{"stage:planning"}, acks: []string{"/approve"}},
			[]string{"also Y"}},
		{"after an approval, comments are planning's input, not earlier ones, and an edit posts no revision",
			approved(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1006, "owner", "also Y")
				post(t, f, 1007, "mallory", "also Z")
				post(t, f, 1008, "owner", "/hold") // nothing handles it, so it waits
				f.EditBody(1, "owner", "body v2")
			}), owners, outcome{labels: []string{"stage:planning"}}, []string{"also Y"}},
		{"a command that resumed clarifying isn't notes",
			at(state.Clarifying, func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/hold")
				post(t, f, 1003, app, "<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=1002 -->")
				f.AddReaction(1002, app, "+1")
				post(t, f, 1005, "owner", "/resume")
				post(t, f, 1006, app, "<!-- issuedog:transition from=held to=clarifying cmd=1005 -->")
				f.AddReaction(1005, app, "+1")
			}), owners, outcome{labels: clarifying, acks: []string{"/hold", "/resume"}}, nil},
		{"an owner's edit posts the next revision, so an older 👍 or /approve approves nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				post(t, f, 1003, "owner", "/approve")
				f.EditBody(1, "owner", "body v2")
			}), owners,
			outcome{comments: []string{rejected("/approve", changed, "1003"), posted(2, "body v2", "", "", "1003")}, labels: atReview, downs: []string{"/approve"}}, nil},
		{"an untrusted edit posts no revision, and approves nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				post(t, f, 1003, "owner", "/approve")
				f.EditBody(1, "mallory", "body v2")
			}), owners,
			outcome{comments: []string{rejected("/approve", dirtyBody, "1003")}, labels: atReview, downs: []string{"/approve"}}, nil},
		{"an untrusted author's own edit posts no revision, and approves nothing",
			authored("mallory", func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				f.EditBody(1, "mallory", "body v2")
			}), owners, outcome{labels: atReview}, nil},
		{"nor does a hidden revision's, when the untrusted author edited the body last",
			authored("mallory", func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body")
				post(t, f, 1002, app, "Noted.")
				f.EditComment(1002, "mallory", forged("Noted."))
			}), owners, outcome{labels: atReview}, nil},
		{"nor does a bot's, even one Trust names",
			authored("other[bot]", func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "other[bot]", "body v2") }),
			trusted{"owner": true, "other[bot]": true}, outcome{labels: atReview}, nil},
		{"the owner's edit of a body someone else filed posts the next revision (epic flow 5, #169)",
			authored("filer", func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				f.EditBody(1, "owner", "body v2")
			}), owners, outcome{comments: []string{posted(2, "body v2", "", "", "1001")}, labels: atReview}, nil},
		{"so does another trusted user's edit of the owner's body",
			review(func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner2", "body v2") }), owners,
			outcome{comments: []string{posted(2, "body v2", "", "", "1001")}, labels: atReview}, nil},
		{"an edit by a user who has since lost trust posts none, and approves nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "owner2", "body v2")
				f.EditBody(1, "owner", "body v3")
				post(t, f, 1002, "owner", "/approve")
			}), trusted{"owner": true},
			outcome{comments: []string{rejected("/approve", strings.Replace(dirtyBody, "mallory", "owner2", 1), "1002")}, labels: atReview, downs: []string{"/approve"}}, nil},
		{"the App's edit of an untrusted author's body posts the next revision",
			authored("mallory", func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, app, "body v2") }), owners,
			outcome{comments: []string{posted(2, "body v2", "", "", "1001")}, labels: atReview}, nil},
		{"a dirty latest revision is posted again",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "owner", "body v2")
				post(t, f, 1002, app, revision(2, "body v2"))
				f.EditComment(1002, "mallory", forged(revision(2, "body v2")))
			}), owners, outcome{comments: []string{posted(2, "body v2", "", "", "1001")}, labels: atReview}, nil},
		{"a revision hidden by a dirty App comment is posted again",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "Noted.")
				f.EditComment(1002, "mallory", forged("Noted."))
			}), owners, outcome{comments: []string{posted(2, "body", "", "", "1001")}, labels: atReview}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var in []string
			check(t, tt.setup, visit(t, tt.trust, &in), tt.want)
			if !reflect.DeepEqual(in, tt.inputs) {
				t.Errorf("inputs = %q, want %q", in, tt.inputs)
			}
		})
	}
}

// #83's verifier's case: request 1002 is rejected, as the issue is a story
// with a parent, but the reply's write fails after the 👎. The owner sets
// the stage by hand, and the next pass records the move to clarifying and
// posts the reply, worded for the stage now. The setup runs the failed pass,
// so Replay covers the repost.
func TestLostReplyAfterStageChange(t *testing.T) {
	var in []string
	setup := func(tb testing.TB) *githubtest.Fake {
		f := review(func(t testing.TB, f *githubtest.Fake) {
			f.SetParent(1, f.AddIssue("owner", "Epic", "epic"))
			post(t, f, 1002, "owner", "too long")
		})(tb)
		f.FailWrite(2)
		if err := visit(t, owners, &in)(f); err == nil {
			tb.Fatal("the first pass succeeded, want its reply to fail")
		}
		w := f.Writes()
		if len(w) != 2 || w[0].Path != "/issues/comments/1002/reactions" || w[0].Failed || !w[1].Failed {
			tb.Fatalf("first pass writes %+v, want the 👎 on 1002, then the failed reply", w)
		}
		f.FailWrite(0)
		f.ForgetWrites()
		handSet(tb, f, state.SpecReview, state.Clarifying, 9003)
		return f
	}
	check(t, setup, visit(t, owners, &in), outcome{
		comments: []string{overridden(state.Clarifying, "from=spec-review to=clarifying by=owner event=9003"), movedOn("too long", "1002", state.Clarifying)},
		labels:   []string{"stage:clarifying"}, downs: []string{"too long"}})
	if in != nil {
		t.Errorf("inputs = %q, want none", in)
	}
}

// Telling a revision request costs no API call: Step reads the App's
// reactions to a plain comment only if it's a request, so not to a comment
// posted at another stage, an answer at clarifying included.
func TestRequestReadsNoReactions(t *testing.T) {
	f := review(func(t testing.TB, f *githubtest.Fake) {
		post(t, f, 1002, "owner", "/revise too long")
		post(t, f, 1003, app, sentBack("1002"))
		f.AddLabel(1, app, "stage:clarifying")
		f.RemoveLabel(1, app, "stage:spec-review")
		post(t, f, 1004, "owner", "add X")
		post(t, f, 1005, app, revision(2, "body"))
		f.AddLabel(1, app, "stage:spec-review")
		f.RemoveLabel(1, app, "stage:clarifying")
		post(t, f, 1006, "owner", "/approve")
		post(t, f, 1007, app, approvedRev(2, "owner's `/approve`", "1006", "1006"))
		f.AddLabel(1, app, "stage:planning")
		f.RemoveLabel(1, app, "stage:spec-review")
		post(t, f, 1008, "owner", "also Y")
		f.RemoveLabel(1, "owner", "stage:planning")
		f.AddLabel(1, "owner", "stage:clarifying")
		for _, id := range []int64{1002, 1004, 1006} {
			f.AddReaction(id, app, "+1")
		}
	})(t)
	var in []string
	if err := visit(t, owners, &in)(f); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Requests() {
		if r.Path == "/issues/comments/1004/reactions" || r.Path == "/issues/comments/1008/reactions" {
			t.Errorf("read %s, want no reactions read on a plain comment that isn't a request", r.Path)
		}
	}
}

// A comment that lands after a pass read the issue, before the App's write,
// isn't lost: the write's seen is the newest comment the pass read. Replay
// can't cover these, as a crash lets the next pass read the comment first,
// which changes what it does; the writes are under Replay above.
func TestCommentDuringPass(t *testing.T) {
	for _, tt := range []struct {
		name     string
		setup    func(testing.TB) *githubtest.Fake
		late     string // posted by the owner after the read
		comments []string
		labels   []string
		inputs   []string
	}{
		{"after a 👍 approval, even with the App's reply between, it is planning's input",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				post(t, f, 1003, "owner", "/frob")
			}), "also Y",
			[]string{"also Y", rejected("/frob", unknownFrob, "1003"), approvedRev(1, "owner's 👍", "r1002", "1003")},
			[]string{"stage:planning"}, []string{"also Y"}},
		{"after a revision posted on its own, it is a revision request",
			review(func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", "body v2") }), "too long",
			[]string{"too long", posted(2, "body v2", "", "", "1001"), sentBack("1002")}, []string{"stage:clarifying"}, []string{"too long"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.setup(t)
			before := len(f.State().Issues[0].Comments)
			var in []string
			late := func(f *githubtest.Fake) error {
				a, s, err := applier(t, f)
				if err != nil {
					return err
				}
				f.AddComment(1, "owner", tt.late)
				st := &daemon.Step{Applier: a, Trust: owners, Reactions: daemon.ApproveByReaction(owners),
					Commands: map[string]daemon.Handler{"approve": daemon.ApproveCommand(), "revise": daemon.ReviseCommand()}}
				if _, err := st.Run(context.Background(), s); err != nil {
					return err
				}
				return st.ReviewVisit(context.Background(), s)
			}
			if err := late(f); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := visit(t, owners, &in)(f); err != nil {
					t.Fatal(err)
				}
			}
			got := f.State().Issues[0]
			var added []string
			for _, c := range got.Comments[before:] {
				added = append(added, c.Body)
			}
			if !reflect.DeepEqual(added, tt.comments) || !reflect.DeepEqual(got.Labels, tt.labels) || !reflect.DeepEqual(in, tt.inputs) {
				t.Errorf("comments %q, labels %q, inputs %q; want %q, %q, %q", added, got.Labels, in, tt.comments, tt.labels, tt.inputs)
			}
		})
	}
}

// ReviewVisit fails, wrapped, when it can't read trust, and writes nothing.
func TestReviewVisitTrustError(t *testing.T) {
	f := review(func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", "body v2") })(t)
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	err = (&daemon.Step{Applier: a, Trust: failTrust{}}).ReviewVisit(context.Background(), s)
	if !errors.Is(err, errTrust) || !strings.HasPrefix(err.Error(), "revision of o/r#1: trust owner: ") {
		t.Errorf("err = %v, want errTrust, wrapped", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
}
