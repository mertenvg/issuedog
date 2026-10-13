package daemon_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
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
// owners and, to show it's ignored anyway, the App trusted.
func approvePass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		tr := trusted{"owner": true, "owner2": true, app: true}
		st := &daemon.Step{Applier: a, Trust: tr, Commands: map[string]daemon.Handler{"approve": daemon.ApproveCommand()},
			Reactions: daemon.ApproveByReaction(tr)}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// approved is the comment approving revision 1 of "body" by key.
func approved(by, key string) string {
	return fmt.Sprintf("Revision 1 of the spec is approved, by %s.\n\n"+
		"<!-- issuedog:transition from=spec-review to=planning rev=1 body=%s cmd=%s -->", by, state.BodyHash("body"), key)
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
	atReview := []string{"stage:spec-review"}
	planning := []string{"stage:planning"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"a trusted 👍 on the latest revision approves it",
			review(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "owner", "+1") }),
			outcome{comments: []string{approved("owner's 👍", "r1002")}, labels: planning}},
		{"the earliest trusted 👍 approves, not the App's, an untrusted one or a 👎",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, app, "+1")
				f.AddReaction(1001, "mallory", "+1")
				f.AddReaction(1001, "owner", "-1")
				f.AddReaction(1001, "owner2", "+1") // 1005
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{comments: []string{approved("owner2's 👍", "r1005")}, labels: planning, acks: []string{revision(1, "body")}}},
		{"an untrusted 👍 approves nothing",
			review(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1001, "mallory", "+1") }),
			outcome{labels: atReview}},
		{"a 👍 on an earlier revision approves nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				post(t, f, 1003, app, revision(2, "body"))
			}),
			outcome{labels: atReview}},
		{"a dirty revision isn't one: its 👍 is ignored, and the one before it is the latest",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, revision(2, "body"))
				f.EditComment(1002, "mallory", forged(revision(2, "body")))
				f.AddReaction(1002, "owner", "+1")
				f.AddReaction(1001, "owner2", "+1") // 1004
			}),
			outcome{comments: []string{approved("owner2's 👍", "r1004")}, labels: planning}},
		{"a 👍 approves nothing once the body changed",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "owner", "body, edited")
				f.AddReaction(1001, "owner", "+1")
			}),
			outcome{labels: atReview}},
		{"removing the 👍 once the approval is applied changes nothing",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner", "+1")
				f.AddReaction(1001, "owner2", "+1")
				post(t, f, 1004, app, approved("owner's 👍", "r1002"))
				f.AddLabel(1, app, "stage:planning")
				f.RemoveLabel(1, app, "stage:spec-review")
				if !f.RemoveReaction(1001, 1002) {
					t.Fatal("no reaction 1002 to remove")
				}
			}),
			outcome{labels: planning}},
		{"a trusted /approve after the latest revision approves it",
			review(func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			outcome{comments: []string{approved("owner's `/approve`", "1002")}, labels: planning, acks: []string{"/approve"}}},
		{"an /approve beats a 👍 in the same poll",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1001, "owner2", "+1")
				post(t, f, 1003, "owner", "/approve")
			}),
			outcome{comments: []string{approved("owner's `/approve`", "1003")}, labels: planning, acks: []string{"/approve"}}},
		{"an /approve once the body changed is rejected",
			review(func(t testing.TB, f *githubtest.Fake) {
				f.EditBody(1, "owner", "body, edited")
				post(t, f, 1002, "owner", "/approve")
			}),
			outcome{comments: []string{rejected("/approve", "the issue description changed after revision 1, "+
				"so it no longer matches that revision's hash, `"+state.BodyHash("body")+"`", "1002")},
				labels: atReview, downs: []string{"/approve"}}},
		{"an /approve before the latest revision is rejected",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, "owner", "/approve")
				post(t, f, 1003, app, revision(2, "body"))
			}),
			outcome{comments: []string{rejected("/approve", "it was posted before revision 2, the latest. "+
				"To approve that, react 👍 to it, or post `/approve` again", "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"an /approve with no revision is rejected",
			at(state.SpecReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			outcome{comments: []string{rejected("/approve", "there's no revision to approve yet", "1002")}, labels: atReview, downs: []string{"/approve"}}},
		{"an /approve at plan-review is rejected",
			at(state.PlanReview, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1002, "owner", "/approve") }),
			outcome{comments: []string{rejected("/approve", "approving a plan isn't handled yet", "1002")},
				labels: []string{"stage:plan-review"}, downs: []string{"/approve"}}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, approvePass(t), tt.want) })
	}
}

// revisionPass posts a revision on issue 1 with m.
func revisionPass(t *testing.T, m daemon.Move) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		a, s, err := applier(t, f)
		if err != nil {
			return err
		}
		d, err := a.Dirty(context.Background(), s)
		if err != nil {
			return err
		}
		return a.PostRevision(context.Background(), s, d.Has, m)
	}
}

// posted is the comment PostRevision posts for rev of body, after text and
// with the transition marker move, if any.
func posted(rev int, body, text, move string) string {
	h := state.BodyHash(body)
	if text != "" {
		text += "\n\n"
	}
	if move != "" {
		move += "\n"
	}
	return fmt.Sprintf("%s**Revision %d** of the spec, `%s`.\n\n"+
		"- To approve it, react 👍 to this comment, or post `/approve`.\n"+
		"- To send it back, post `/revise <notes>`, or any other comment.\n\n"+
		"%s<!-- issuedog:revision rev=%d body=%s -->", text, rev, h, move, rev, h)
}

func TestPostRevision(t *testing.T) {
	atReview := []string{"stage:spec-review"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		m     daemon.Move
		want  outcome
	}{
		{"the first revision", filed(nil, func(testing.TB, *githubtest.Fake) {}), daemon.Move{},
			outcome{comments: []string{posted(1, "body", "", "")}}},
		{"none while the latest revision has the body's hash", review(func(testing.TB, *githubtest.Fake) {}), daemon.Move{},
			outcome{labels: atReview}},
		{"the next once the body changed, its text escaped",
			review(func(t testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", "body v2") }),
			daemon.Move{Text: "Edited <!-- issuedog:revision rev=9 body=x -->"},
			outcome{comments: []string{posted(2, "body v2", "Edited &lt;!-- issuedog:revision rev=9 body=x -->", "")}, labels: atReview}},
		{"a dirty latest revision is posted again, with its number",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, revision(2, "body v2"))
				f.EditComment(1002, "mallory", forged(revision(2, "body v2")))
				f.EditBody(1, "owner", "body v2")
			}),
			daemon.Move{}, outcome{comments: []string{posted(2, "body v2", "", "")}, labels: atReview}},
		{"with a move, in one comment, even when the hash is the same",
			review(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1002, app, "<!-- issuedog:transition from=spec-review to=clarifying run=q -->")
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:spec-review")
			}),
			daemon.Move{To: state.SpecReview, Trigger: state.TriggerSpecWritten, Key: state.Key{Field: "run", ID: "s"}, Text: "Answered."},
			outcome{comments: []string{posted(2, "body", "Answered.",
				"<!-- issuedog:transition from=clarifying to=spec-review rev=2 body="+state.BodyHash("body")+" run=s -->")}, labels: atReview}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, revisionPass(t, tt.m), tt.want) })
	}
}
