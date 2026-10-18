package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// originHuman is a human task's origin marker (SPEC §16, S3's human=1).
const originHuman = "<!-- issuedog:origin plan=#195 rev=1 item=T9 human=1 -->"

// originRepost is the App's comment that carries origin marker m again,
// once the body it was in no longer counts (#290).
func originRepost(m string) string {
	return "The origin marker below keeps this issue in its plan.\n\n" + m
}

// originQuiet runs d until it has read back its own writes, then once
// more, and fails t unless that last pass sends only conditional GETs,
// answered 304, and writes nothing.
func originQuiet(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
	t.Helper()
	iterate(t, d) // reads back whatever the pass before wrote
	writes, histories, from := len(f.Writes()), f.HistoryQueries(), len(f.Requests())
	iterate(t, d)
	for _, r := range f.Requests()[from:] {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if len(f.Writes()) != writes || f.HistoryQueries() != histories {
		t.Errorf("the unchanged pass wrote %+v and sent %d history queries; want none", f.Writes()[writes:], f.HistoryQueries()-histories)
	}
}

// An App task whose body the owner edited, with an App comment carrying
// its origin (#290), gets no state error at needs-human, and stays there.
// Once the App's writes are read back, a pass where nothing changed sends
// only conditional GETs, answered 304, and writes nothing: reading origin
// from a comment reads nothing the snapshot doesn't have. The type check
// judges the type alone (typeErrCause), so the class shows in the digest's
// plan order instead (TestOriginPlanOrder) and in state's tests.
func TestOriginInAnAppComment(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := f.AddIssue(app, "T9. Turn on the App", "The human task.\n\n"+originHuman)
	f.AddLabel(n, app, "type:task")
	f.AddLabel(n, app, "stage:needs-human")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=needs-human event=p500 -->")
	f.EditBody(n, "owner", "The human task, put better.")
	f.AddComment(n, app, originRepost(originHuman))
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	for _, c := range commentBodies(f, n) {
		if strings.Contains(c, "issuedog:error ") {
			t.Errorf("comment %q; want no state error", c)
		}
	}
	originQuiet(t, f, d)
	if l := f.State().Issues[n-1].Labels; !slices.Equal(l, []string{"stage:needs-human", "type:task"}) {
		t.Errorf("labels = %q; want the task left at needs-human", l)
	}
}

// originPlanStory files a story by the App, item S<item> of the plan on
// #40, at plan-review with plan revision 1 of its own plan, and with
// priority. If edited, the owner then edits its body, so the body's origin
// no longer counts, and the App posts the marker again in a comment. It
// returns the story's number.
func originPlanStory(f *githubtest.Fake, item int, priority string, edited bool) int {
	origin := fmt.Sprintf("<!-- issuedog:origin plan=#40 rev=1 item=S%d -->", item)
	n := f.AddIssue(app, fmt.Sprintf("S%d. A story", item), "The story.\n\n"+origin)
	for _, l := range []string{"type:story", "priority:" + priority, "stage:plan-review"} {
		f.AddLabel(n, app, l)
	}
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=planning event=p400 -->")
	plan := fmt.Sprintf("### Plan for #%d (rev 1)\n\nThe plan.\n\ncc @%s\n\n<!-- issuedog:transition from=planning to=plan-review run=p%d -->", n, digestRunOwner, n)
	f.AddComment(n, app, plan+"\n<!-- issuedog:revision kind=plan rev=1 body="+state.BodyHash(plan)+" -->")
	if edited {
		f.EditBody(n, digestRunOwner, "The story, put better.")
		f.AddComment(n, app, originRepost(origin))
	}
	return n
}

// The digest's plan order reads origin wherever it lives (#290, #270): a
// story whose body the owner edited, with its origin in an App comment,
// groups with its sibling from the same plan, in item order, where the
// sibling alone would rank. Without it, S1 would rank alone at p2, after
// the epic. A pass where nothing changed makes no request but conditional
// GETs.
func TestOriginPlanOrder(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(digestRunOwner, "admin")
	s1 := originPlanStory(f, 1, "p2", true)
	s2 := originPlanStory(f, 2, "p1", false)
	epic, _ := assignPlanReview(f, digestRunOwner, digestRunOwner)
	f.AddLabel(epic, app, "priority:p1")
	d := digestRunDaemon(t, f, digestRunOrigin(t), nil, nil)
	iterate(t, d)
	digest := slices.IndexFunc(f.State().Issues, func(is githubtest.IssueState) bool { return is.Title == "issuedog digest" })
	if digest < 0 {
		t.Fatalf("no digest among %+v", f.State().Issues)
	}
	want := []string{
		fmt.Sprintf("#%d Plan approval p2 1", s1),
		fmt.Sprintf("#%d Plan approval p1 1", s2),
		fmt.Sprintf("#%d Plan approval p1 1", epic),
	}
	if got := digestRunItems(f.State().Issues[digest].Body); !slices.Equal(got, want) {
		t.Errorf("items = %q; want %q", got, want)
	}
	originQuiet(t, f, d)
}
