package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// revision is a revision comment's markers, for rev of body, with a move to
// spec-review.
func revision(rev int, body string) string {
	h := state.BodyHash(body)
	return fmt.Sprintf("<!-- issuedog:transition from=clarifying to=spec-review rev=%d body=%s run=r%d -->\n"+
		"<!-- issuedog:revision rev=%d body=%s -->", rev, h, rev, rev, h)
}

// review files issue 1, a story with body "body", at spec-review, set by the
// App: label event 9001, and comment 1001, revision 1 of the body. Then it
// runs more.
func review(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Story")
		labelEvent(t, f.AddLabel(1, app, "stage:spec-review"), 9001)
		post(t, f, 1001, app, revision(1, "body"))
		more(t, f)
	})
}

// approvePass runs step 1 on issue 1 with the approval handlers, and the
// owners and, to show it's ignored anyway, the App trusted. /priority only
// gets a 👍.
func approvePass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		tr := trusted{"owner": true, "owner2": true, app: true}
		ack := daemon.Handler{Apply: func(ctx context.Context, a *daemon.Applier, c daemon.Cmd) error { return a.Ack(ctx, c.Comment.ID) }}
		st := &daemon.Step{Applier: a, Trust: tr, Commands: map[string]daemon.Handler{"approve": daemon.ApproveCommand(), "priority": ack},
			Reactions: daemon.ApproveByReaction(tr)}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// noted is body under the note MarkDirty writes for mallory's edit. Mallory
// can write it herself, and then MarkDirty leaves the body alone.
func noted(body string) string {
	return "<!-- issuedog:dirty by=mallory -->\n> ⚠️ issuedog ignores this issue description: " +
		"**mallory** edited it without approval.\n\n" + body
}

// dirtyReview is review with the body made dirty by mallory, under her own
// note, and then, if owner is set, edited by the owner. Comment 1002 is
// revision 2 of the body as it ends up, so its hash matches. Then it runs more.
func dirtyReview(owner bool, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return review(func(t testing.TB, f *githubtest.Fake) {
		body := noted("body")
		f.EditBody(1, "mallory", body)
		if owner {
			body = noted("body, the owner's")
			f.EditBody(1, "owner", body)
		}
		post(t, f, 1002, app, revision(2, body))
		more(t, f)
	})
}

// approveBoth posts the owner's /approve, comment 1003, then adds their 👍
// on comment 1002, dirtyReview's revision 2.
func approveBoth(t testing.TB, f *githubtest.Fake) {
	post(t, f, 1003, "owner", "/approve")
	f.AddReaction(1002, "owner", "+1")
}

// dirtyBody is /approve's reason at spec-review for mallory's edit to the
// body. It says no revision follows, and how to get a new spec (#229), so
// its reply is final. It names mallory as code, as the hold does (#525).
const dirtyBody = "the issue description has an edit by `mallory` that wasn't approved, so I can't approve it, " +
	"and I won't post a revision of it. To get a new spec, which replaces the edit, post `/revise <notes>`, or any other comment"

// dirtyBodySomeone is dirtyBody where the body's last edit doesn't confirm
// mallory as its editor, as after the owner's edit of it, so it names
// "someone" (#602).
var dirtyBodySomeone = strings.Replace(dirtyBody, "`mallory`", "someone", 1)

// ivWordingFollows ends /approve's reason at spec-review for a stale
// revision when ReviewVisit posts the next one (#229).
const ivWordingFollows = ". I'll post a new revision next. To approve it, react 👍 to it, or post `/approve` after it"

// ivWordingApproveFinal is the final reply to /approve, comment id, for
// reason.
func ivWordingApproveFinal(reason, id string) string {
	return final("/approve", "I didn't apply this command", reason, id)
}

// approvedRev is the comment approving revision rev of "body" by key, with
// seen.
func approvedRev(rev int, by, key, seen string) string {
	return fmt.Sprintf("Revision %d of the spec is approved, by %s.\n\n"+
		"<!-- issuedog:transition from=spec-review to=planning rev=%d body=%s cmd=%s seen=%s -->", rev, by, rev, state.BodyHash("body"), key, seen)
}

// outcome is what a pass on issue 1 should leave: the comments it adds, the
// labels, and the comments the App has 👍'd and 👎'd.
type outcome struct {
	comments, labels, acks, downs []string
}

// check runs pass under Replay, then once more and again on a fresh fake,
// and compares what it leaves with want.
func check(t *testing.T, setup func(testing.TB) *githubtest.Fake, pass func(*githubtest.Fake) error, want outcome) {
	t.Helper()
	githubtest.Replay(t, setup, pass)
	before := len(setup(t).State().Issues[0].Comments)
	got := run(t, setup, pass).State().Issues[0]
	var o outcome
	for i, c := range got.Comments {
		if i >= before {
			o.comments = append(o.comments, c.Body)
		}
		if slices.Contains(c.Reactions, app+" +1") {
			o.acks = append(o.acks, c.Body)
		}
		if slices.Contains(c.Reactions, app+" -1") {
			o.downs = append(o.downs, c.Body)
		}
	}
	o.labels = got.Labels
	if !reflect.DeepEqual(o, want) {
		t.Errorf("got\n%q\nwant\n%q", o, want)
	}
}

func TestApprove(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:spec-review"}
	planning := []string{"stage:planning"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"a trusted 👍 on the latest revision approves it",
			review(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") }),
			outcome{comments: []string{approvedRev(1, "owner's 👍", "r1002", "1001")}, labels: planning}},
		{"the earliest trusted 👍 approves, not the App's, an untrusted one or a 👎",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, app, "+1")
				f.AddReaction(1001, "mallory", "+1")
				f.AddReaction(1001, "owner", "-1")
				f.AddReaction(1001, "owner2", "+1") // 1005
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{approvedRev(1, "owner2's 👍", "r1005", "1001")}, labels: planning, acks: []string{revision(1, "body")}}},
		{"a 👍 on an earlier revision approves nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				post(t, f, 1003, app, revision(2, "body"))
			}),
			outcome{labels: atReview}},
		{"removing the 👍 once the approval is applied changes nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				f.AddReaction(1001, "owner2", "+1")
				post(t, f, 1004, app, approvedRev(1, "owner's 👍", "r1002", "1001"))
				f.AddLabel(1, app, "stage:planning")
				f.RemoveLabel(1, app, "stage:spec-review")
				if !f.RemoveReaction(1001, 1002) {
					t.Fatal("no reaction 1002 to remove")
				}
			}),
			outcome{labels: planning}},
		{"a trusted /approve after the latest revision approves it",
			review(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			outcome{comments: []string{approvedRev(1, "owner's `/approve`", "1002", "1002")}, labels: planning, acks: []string{"/approve"}}},
		{"once the body changed, a 👍 approves nothing and /approve is rejected",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "owner", "body, edited")
				post(t, f, 1002, "owner", "/approve")
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{ivWordingApproveFinal("the issue description changed after revision 1, "+
				"so it no longer matches that revision's hash, `"+state.BodyHash("body")+"`"+ivWordingFollows, "1002")},
				labels: atReview, downs: []string{"/approve"}}},
		{"an /approve before the latest revision is rejected",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/approve")
				post(t, f, 1003, app, revision(2, "body"))
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "it was posted before revision 2, the latest. "+
				"To approve that, react 👍 to it, or post `/approve` again", "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"an /approve with no revision is rejected",
			at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			outcome{comments: []string{ivWordingApproveFinal("there's no revision to approve yet"+ivWordingFollows, "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"while the body is dirty, under a note mallory wrote herself, a 👍 approves nothing and /approve is rejected",
			dirtyReview(false, approveBoth), outcome{comments: []string{ivWordingApproveFinal(dirtyBody, "1003")}, labels: atReview, downs: []string{"/approve"}}},
		{"likewise even after the owner edits the body, which leaves mallory unconfirmed as its editor",
			dirtyReview(true, approveBoth), outcome{comments: []string{ivWordingApproveFinal(dirtyBodySomeone, "1003")}, labels: atReview, downs: []string{"/approve"}}},
		{"an /approve the table can't apply is rejected, and a later command is still processed",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.SetParent(1, f.AddIssue("owner", "Epic", "epic"))
				post(t, f, 1002, "owner", "/approve")
				post(t, f, 1003, "owner", "/priority p1")
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "only an epic, or a story with no parent, goes from `spec-review` "+
				"to `planning` on approval, and this is a story issue", "1002")},
				labels: atReview, acks: []string{"/priority p1"}, downs: []string{"/approve"}}},
		{"on an issue of an unknown type, /approve is rejected and a 👍 is ignored, and passes don't fail",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.SetType(1, "Feature")
				post(t, f, 1002, "owner", "/approve")
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "I can't tell what kind of issue this is: "+
				"class of type \"feature\": unknown type", "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"a 👍 the table can't apply is reported once, and doesn't fail the pass",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.SetParent(1, f.AddIssue("owner", "Epic", "epic"))
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{"I can't move this issue from `spec-review` to `planning` on spec-approved: " +
				"the transition table (SPEC §7.2) has no such row for a story issue. It stays at `spec-review`.\n\n" +
				"<!-- issuedog:error kind=transition key=r1002 -->"}, labels: atReview}},
		{"an /approve at plan-review with no plan revision is rejected, as a plan's",
			at(state.PlanReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			outcome{comments: []string{final("/approve", "I didn't apply this command", "there's no clean plan revision to approve. "+
				"To get a new plan, post `/revise`, or any other comment", "1002")},
				labels: []string{"stage:plan-review"}, downs: []string{"/approve"}}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, approvePass(t), tt.want) })
	}
}

// revisionPass posts a revision on issue 1 with m, and the cc line for
// notify.
func revisionPass(t *testing.T, m daemon.Move, notify []string) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
		if err != nil {
			return err
		}
		return a.PostRevision(context.Background(), s, d.Has, m, notify)
	}
}

// posted is the comment PostRevision posts for rev of body, after text and
// with the transition marker move, if any, and seen, if any.
func posted(rev int, body, text, move, seen string) string {
	h := state.BodyHash(body)
	if text != "" {
		text += "\n\n"
	}
	if move != "" {
		move += "\n"
	}
	if seen != "" {
		h += " seen=" + seen
	}
	return fmt.Sprintf("%s**Revision %d** of the spec, `%s`.\n\n"+
		"- To approve it, react 👍 to this comment, or post `/approve`.\n"+
		"- To send it back, post `/revise <notes>`, or any other comment.\n\n"+
		"%s<!-- issuedog:revision rev=%d body=%s -->", text, rev, state.BodyHash(body), move, rev, h)
}

func TestPostRevision(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:spec-review"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		m     daemon.Move
		want  outcome
	}{
		{"the first revision", filed(nil, func(testing.TB, *githubtest.Fake) {}), daemon.Move{},
			outcome{comments: []string{posted(1, "body", "", "", "")}}},
		{"none while the latest revision has the body's hash", review(func(testing.TB, *githubtest.Fake) {}), daemon.Move{},
			outcome{labels: atReview}},
		{"the next once the body changed, its text escaped",
			review(func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", "body v2") }),
			daemon.Move{Text: "Edited <!-- issuedog:revision rev=9 body=x -->"},
			outcome{comments: []string{posted(2, "body v2", "Edited &lt;!-- issuedog:revision rev=9 body=x -->", "", "1001")}, labels: atReview}},
		{"a dirty latest revision is posted again, with its number",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, revision(2, "body v2"))
				f.EditComment(1002, "mallory", forged(revision(2, "body v2")))
				f.EditBody(1, "owner", "body v2")
			}),
			daemon.Move{}, outcome{comments: []string{posted(2, "body v2", "", "", "1001")}, labels: atReview}},
		{"none while the body is dirty, even after the owner edits it",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "mallory", "body v2")
				f.EditBody(1, "owner", "body v3")
			}),
			daemon.Move{}, outcome{labels: atReview}},
		{"with a move and a dirty body, the move happens with no revision",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "<!-- issuedog:transition from=spec-review to=clarifying run=q -->")
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
				f.EditBody(1, "mallory", "body v2")
			}),
			daemon.Move{To: state.SpecReview, Trigger: state.TriggerSpecWritten, Key: state.Key{Field: "run", ID: "s"}, Text: "Answered."},
			outcome{comments: []string{"Answered.\n\nThere's no revision to approve: the issue description was edited without approval " +
				"(see the note on it).\n\n<!-- issuedog:transition from=clarifying to=spec-review run=s -->"}, labels: atReview}},
		{"with a move, in one comment, even when the hash is the same",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "<!-- issuedog:transition from=spec-review to=clarifying run=q -->")
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
			}),
			daemon.Move{To: state.SpecReview, Trigger: state.TriggerSpecWritten, Key: state.Key{Field: "run", ID: "s"}, Text: "Answered."},
			outcome{comments: []string{posted(2, "body", "Answered.",
				"<!-- issuedog:transition from=clarifying to=spec-review rev=2 body="+state.BodyHash("body")+" run=s -->", "")}, labels: atReview}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, revisionPass(t, tt.m, nil), tt.want) })
	}
}

// ccPosted is posted, with the cc line cc after the revision, last before
// the markers.
func ccPosted(cc string, rev int, body, text, move, seen string) string {
	return strings.Replace(posted(rev, body, text, move, seen), " or any other comment.\n\n", " or any other comment.\n\n"+cc+"\n\n", 1)
}

// A revision comment's cc line is last, after the revision, as in the App's
// other comments (SPEC §9), so one with no other text starts with the
// revision (#332).
func TestPostRevisionCCLast(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:spec-review"}
	backAtClarifying := func(more func(testing.TB, *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
		return review(func(t testing.TB, f *githubtest.Fake) {
			post(t, f, 1002, app, "<!-- issuedog:transition from=spec-review to=clarifying run=q -->")
			f.AddLabel(1, app, "stage:clarifying")
			f.RemoveLabel(1, app, "stage:spec-review")
			more(t, f)
		})
	}
	move := func(text string) daemon.Move {
		return daemon.Move{To: state.SpecReview, Trigger: state.TriggerSpecWritten, Key: state.Key{Field: "run", ID: "s"}, Text: text}
	}
	transition := "<!-- issuedog:transition from=clarifying to=spec-review rev=2 body=" + state.BodyHash("body") + " run=s -->"
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		m     daemon.Move
		want  outcome
	}{
		{"with a move and no text, the comment starts with the revision and ends with the cc line",
			backAtClarifying(func(testing.TB, *githubtest.Fake) {}), move(""),
			outcome{comments: []string{ccPosted("cc @owner @other", 2, "body", "", transition, "")}, labels: atReview}},
		{"with a move and text, the text, the revision, then the cc line",
			backAtClarifying(func(testing.TB, *githubtest.Fake) {}), move("Answered."),
			outcome{comments: []string{ccPosted("cc @owner @other", 2, "body", "Answered.", transition, "")}, labels: atReview}},
		{"on its own, the cc line after the revision",
			review(func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", "body v2") }), daemon.Move{},
			outcome{comments: []string{ccPosted("cc @owner @other", 2, "body v2", "", "", "1001")}, labels: atReview}},
		{"with a move and a dirty body, the cc line after why there's no revision",
			backAtClarifying(func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "mallory", "body v2") }), move("Answered."),
			outcome{comments: []string{"Answered.\n\nThere's no revision to approve: the issue description was edited without approval " +
				"(see the note on it).\n\ncc @owner @other\n\n<!-- issuedog:transition from=clarifying to=spec-review run=s -->"}, labels: atReview}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			check(t, tt.setup, revisionPass(t, tt.m, []string{"owner", "other"}), tt.want)
		})
	}
}

// probe is #97's case: revision 1 of "body" with the owner's 👍 (1002),
// revision 2 of "body v2" (1003), and revision 3 of "body" again (1004),
// which mallory makes dirty. Then it runs more.
func probe(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return review(func(t testing.TB, f *githubtest.Fake) {
		f.AddReaction(1001, "owner", "+1")
		f.EditBody(1, "owner", "body v2")
		post(t, f, 1003, app, revision(2, "body v2"))
		f.EditBody(1, "owner", "body")
		post(t, f, 1004, app, revision(3, "body"))
		f.EditComment(1003, "mallory", forged(revision(2, "body v2")))
		f.EditComment(1004, "mallory", forged(revision(3, "body")))
		more(t, f)
	})
}

// visitPass is approvePass, then PostRevision on its own, as a visit at
// spec-review would run them.
func visitPass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		if err := approvePass(t)(f); err != nil {
			return err
		}
		return revisionPass(t, daemon.Move{}, nil)(f)
	}
}

const hidden = "a comment of mine after revision 1 was edited without approval or deleted, " +
	"so it may hide a newer revision. Approve the next revision I post instead"

// A dirty App comment after the latest clean revision may be a newer
// revision, so nothing approves until a fresh one follows it.
func TestApproveHiddenRevision(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:spec-review"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"the old 👍 doesn't approve, and a fresh revision is posted",
			probe(func(testing.TB, *githubtest.Fake) {}),
			outcome{comments: []string{posted(2, "body", "", "", "1001")}, labels: atReview}},
		{"an /approve is rejected, and a fresh revision is posted",
			probe(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1005, "owner", "/approve") }),
			outcome{comments: []string{final("/approve", "I didn't apply this command", hidden, "1005"), posted(2, "body", "", "", "1005")},
				labels: atReview, downs: []string{"/approve"}}},
		{"once a fresh revision follows, a 👍 on it approves",
			probe(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1005, app, revision(2, "body"))
				f.AddReaction(1005, "owner2", "+1") // 1006
			}),
			outcome{comments: []string{approvedRev(2, "owner2's 👍", "r1006", "1005")}, labels: []string{"stage:planning"}}},
		{"any dirty App comment after the latest revision hides it",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "Noted.")
				f.EditComment(1002, "mallory", forged("Noted."))
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{posted(2, "body", "", "", "1001")}, labels: atReview}},
		{"a dirty comment by anyone else doesn't",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "Noted.")
				f.EditComment(1002, "mallory", "Noted!")
				f.AddReaction(1001, "owner", "+1") // 1003
			}),
			outcome{comments: []string{approvedRev(1, "owner's 👍", "r1003", "1002")}, labels: []string{"stage:planning"}}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, visitPass(t), tt.want) })
	}
}

var errTrust = errors.New("trust is down")

type failTrust struct{}

func (failTrust) Trusted(context.Context, string) (bool, error) { return false, errTrust }

// An error reading trust or the reactions, or making the approval's write,
// fails the pass, wrapped, and writes nothing.
func TestApproveByReactionErrors(t *testing.T) {
	t.Parallel()
	status := func(err error, code int) bool {
		var e *gogithub.ErrorResponse
		return errors.As(err, &e) && e.Response.StatusCode == code
	}
	for _, tt := range []struct {
		name  string
		trust github.Trust
		gone  bool // delete the revision comment after the snapshot
		fail  int  // FailWrite's n
		want  func(error) bool
	}{
		{"trust", failTrust{}, false, 0, func(err error) bool {
			return errors.Is(err, errTrust) && strings.HasPrefix(err.Error(), "approvals on o/r#1: trust owner: ")
		}},
		{"reactions", trusted{"owner": true}, true, 0, func(err error) bool {
			return status(err, http.StatusNotFound) && strings.HasPrefix(err.Error(), "approvals on o/r#1: ")
		}},
		{"the approval's marker", trusted{"owner": true}, false, 1, func(err error) bool {
			return status(err, http.StatusInternalServerError) && strings.HasPrefix(err.Error(), "approve: move o/r#1 to planning")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := review(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") })(t)
			a, s, err := applier(t, f)
			if err != nil {
				t.Fatal(err)
			}
			if tt.gone {
				f.DeleteComment(1001)
			}
			f.FailWrite(tt.fail)
			if err := daemon.ApproveByReaction(tt.trust)(context.Background(), a, s, daemon.Dirt{}); !tt.want(err) {
				t.Errorf("err = %v, want it wrapped", err)
			}
			for _, w := range f.Writes() {
				if !w.Failed {
					t.Errorf("write %+v, want none", w)
				}
			}
		})
	}
}

// planRev is a plan comment, plan revision rev of text, its last line the
// plan revision marker with the hash of the text above it, as a move with
// PlanRev writes it. It has no transition marker, so the issue stays where
// it is: only a stage set by hand after planning brings a plan comment to
// spec-review, and the readers below read only revision markers.
func planRev(rev int, text string) string {
	return fmt.Sprintf("%s\n<!-- issuedog:revision kind=plan rev=%d body=%s -->", text, rev, state.BodyHash(text))
}

// At spec-review, a plan revision after the latest spec revision is never
// taken for it (SPEC §7.4, Two kinds): approval by 👍 or /approve, a
// revision request and ReviewVisit read the spec revision only, and a spec
// revision posted after a plan revision is numbered from the spec revisions
// alone.
func TestSpecReadersIgnorePlanRevision(t *testing.T) {
	t.Parallel()
	atReview, planning, clarifying := []string{"stage:spec-review"}, []string{"stage:planning"}, []string{"stage:clarifying"}
	plan := planRev(1, "### Plan for #1 (rev 1)\n\nT1. Do it.")
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"a 👍 on the spec revision approves it, not the plan revision after it",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, plan)
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{approvedRev(1, "owner's 👍", "r1003", "1001")}, labels: planning}},
		{"a 👍 on the plan revision approves no spec, and no spec revision is posted",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, plan)
				f.AddReaction(1002, "owner", "+1")
			}),
			outcome{labels: atReview}},
		{"an /approve after the plan revision approves the spec revision",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, plan)
				post(t, f, 1003, "owner", "/approve")
			}),
			outcome{comments: []string{approvedRev(1, "owner's `/approve`", "1003", "1003")}, labels: planning, acks: []string{"/approve"}}},
		{"an /approve before the plan revision is after the latest spec revision, so it approves",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/approve")
				post(t, f, 1003, app, plan)
			}),
			outcome{comments: []string{approvedRev(1, "owner's `/approve`", "1002", "1002")}, labels: planning, acks: []string{"/approve"}}},
		{"a comment before the plan revision is a revision request on the spec revision",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "too long")
				post(t, f, 1003, app, plan)
			}),
			outcome{comments: []string{sentBack("1002")}, labels: clarifying, acks: []string{"too long"}}},
		{"once the body changes, the next spec revision is numbered from the spec revisions alone",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, planRev(3, "A plan."))
				f.EditBody(1, "owner", "body v2")
			}),
			outcome{comments: []string{posted(2, "body v2", "", "", "1001")}, labels: atReview}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var in []string
			check(t, tt.setup, visit(t, owners, &in), tt.want)
		})
	}
}

// ReviewVisit reads the spec revision, not a plan revision after it: while
// the spec revision still has the body's hash, it's current, so ReviewVisit
// asks no one's trust, and writes nothing. A plan revision's hash never has
// the body's, so read as the spec's it would be stale every visit.
func TestReviewVisitIgnoresPlanRevision(t *testing.T) {
	t.Parallel()
	f := review(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, app, planRev(1, "A plan.")) })(t)
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&daemon.Step{Applier: a, Trust: failTrust{}}).ReviewVisit(context.Background(), s); err != nil {
		t.Errorf("ReviewVisit = %v, want no trust asked", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
}
