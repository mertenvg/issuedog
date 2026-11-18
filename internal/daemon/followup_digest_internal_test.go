package daemon

import (
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// followUpDigestSnap is open issue 1, a follow-up the App filed at intake,
// typed typ, its body ending in the origin marker origin, under parent, or
// none for 0.
func followUpDigestSnap(typ, origin string, parent int, labels ...string) *github.Snapshot {
	s := waitSnap(1, append([]string{"priority:p3"}, labels...))
	s.Author, s.Type, s.Parent = waitApp, typ, parent
	s.Body = "Refuse it.\n\ncc @owner\n\n" + origin
	return s
}

// followUpDigestOrigin is a follow-up's origin marker, naming verify #7.
const followUpDigestOrigin = "<!-- issuedog:origin verify=#7 run=run3 item=F1 -->"

// A follow-up at intake, a task, is one item: followup-1, ranked between
// an approval and a question, named Follow-up, which a restart reads back
// and the digest doesn't mention, as its body's cc line did. With no
// addendum, it links the issue, and names only a 👍 there; with one, it
// links the addendum on its story, by its comment, and names both.
func TestWaitFindFollowUp(t *testing.T) {
	t.Parallel()
	s := followUpDigestSnap("task", followUpDigestOrigin, 0, "stage:intake")
	it := waitFindOne(t, waitIn(s, Dirt{}))
	want := []string{"A follow-up the verifier found on #7 waits for your approval.",
		"React 👍 to the issue to approve it, and it goes to `ready`.", "Close the issue to drop it."}
	if it.Kind != waitFollowUp || it.Key != "followup-1" || it.Link != "https://github.com/o/r/issues/1" || it.Comment != 0 || it.Priority != 3 ||
		!slices.Equal(waitTexts(it.Lines), want) {
		t.Errorf("the item is a %s keyed %s linking %s, comment %d, p%d, with lines %q; want a followup keyed followup-1 linking the issue, comment 0, p3, with %q",
			it.Kind, it.Key, it.Link, it.Comment, it.Priority, waitTexts(it.Lines), want)
	}
	for _, l := range it.Lines {
		if l.Copied {
			t.Errorf("line %q is copied; want the App's own words", l.Text)
		}
	}
	in := waitIn(followUpDigestSnap("task", followUpDigestOrigin, 2, "stage:intake"), Dirt{})
	in.Addendum = 1001
	story := waitFindOne(t, in)
	want[1] = "React 👍 to its addendum on #2, or to the issue, to approve it, and it goes to `ready`."
	if story.Key != "followup-1" || story.Link != "https://github.com/o/r/issues/2#issuecomment-1001" || story.Comment != 1001 ||
		!slices.Equal(waitTexts(story.Lines), want) {
		t.Errorf("in a story, the item is keyed %s linking %s, comment %d, with lines %q; want followup-1 linking the addendum, 1001, with %q",
			story.Key, story.Link, story.Comment, waitTexts(story.Lines), want)
	}
	if !(waitApproval.rank() < waitFollowUp.rank() && waitFollowUp.rank() < waitQuestion.rank()) || waitFollowUp.String() != "followup" ||
		digestRenderKindName(waitFollowUp) != "Follow-up" {
		t.Errorf("a follow-up ranks %d, between %d and %d, named %s and %q; want it between an approval and a question, followup and Follow-up",
			waitFollowUp.rank(), waitApproval.rank(), waitQuestion.rank(), waitFollowUp, digestRenderKindName(waitFollowUp))
	}
	if k, ok := digestKeptKind("Follow-up"); !ok || k != waitFollowUp {
		t.Errorf("digestKeptKind(Follow-up) = %s, %t; want followup, as a restart reads it back", k, ok)
	}
	if digestRenderMentions(it) {
		t.Error("a new follow-up gets the digest's mention; want none, as its body's cc line mentioned notify")
	}
}

// Only a follow-up a 👍 would approve is an item (followUpApprovable): not
// one approved, at ready, one retyped as a bug, which the type check
// reports, a closed one, or a plan's task at intake, before its first move.
func TestWaitFindFollowUpNone(t *testing.T) {
	t.Parallel()
	closed := followUpDigestSnap("task", followUpDigestOrigin, 0, "stage:intake")
	closed.Closed = true
	approved := followUpDigestSnap("task", followUpDigestOrigin, 0, "stage:ready")
	approved.Comments = []github.Comment{{ID: 101, Author: waitApp, Body: "<!-- issuedog:transition from=intake to=ready cmd=r9 -->"}}
	for name, s := range map[string]*github.Snapshot{
		"approved":  approved,
		"retyped":   followUpDigestSnap("bug", followUpDigestOrigin, 0, "stage:intake"),
		"closed":    closed,
		"plan task": followUpDigestSnap("task", "<!-- issuedog:origin plan=#9 rev=1 item=T1 -->", 0, "stage:intake"),
	} {
		items, err := waitFind(waitIn(s, Dirt{}))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if slices.ContainsFunc(items, func(it waitItem) bool { return it.Kind == waitFollowUp }) {
			t.Errorf("%s: the items are %+v; want no follow-up", name, items)
		}
	}
}

// A closed issue as listed was a follow-up waiting for its approval only
// as the App's issue at stage:intake whose body has the App's follow-up
// origin: not a person's issue with the same marker, nor the App's at
// ready, nor a plan's task at intake.
func TestAssignFollowUpListed(t *testing.T) {
	t.Parallel()
	listed := func(login, typ, body string, labels ...string) *gogithub.Issue {
		is := &gogithub.Issue{User: &gogithub.User{Login: new(login), Type: new(typ)}, Body: new(body)}
		for _, l := range labels {
			is.Labels = append(is.Labels, &gogithub.Label{Name: l})
		}
		return is
	}
	body := "Refuse it.\n\n" + followUpDigestOrigin
	for name, tc := range map[string]struct {
		is   *gogithub.Issue
		want bool
	}{
		"waiting":       {listed(waitApp.Login, "Bot", body, "priority:p3", "Stage:Intake"), true},
		"a person's":    {listed("owner", "User", body, "stage:intake"), false},
		"approved":      {listed(waitApp.Login, "Bot", body, "stage:ready"), false},
		"a plan's task": {listed(waitApp.Login, "Bot", strings.Replace(body, "verify=#7 run=run3 item=F1", "plan=#9 rev=1 item=T1", 1), "stage:intake"), false},
	} {
		if got := assignFollowUpListed(tc.is, waitSlug); got != tc.want {
			t.Errorf("%s: assignFollowUpListed = %t; want %t", name, got, tc.want)
		}
	}
}
