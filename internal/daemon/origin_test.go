package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// originHuman is a human task's origin marker (SPEC §16), whose human=1
// makes the task a human task (SPEC §5).
const originHuman = "<!-- issuedog:origin plan=#195 rev=1 item=T9 human=1 -->"

// originRepost is the App's comment that carries origin marker m again,
// once the body it was in no longer counts (#290), as originRepost in
// internal/daemon posts it (#291).
func originRepost(m string) string {
	return "This issue's description was edited, so I've copied its `origin` marker here to keep tracking it in its plan.\n\n" + m
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

// Reposting a lost origin (#291, SPEC §6.4): step 1 posts the App's origin
// marker again in a comment once an App-filed issue's body stops giving
// it, so the issue keeps its class and its place in its plan.

const (
	// originT2 is a plan task's origin marker.
	originT2 = "<!-- issuedog:origin plan=#195 rev=1 item=T2 -->"
	// originCheck is a human task's origin marker with a file check, its
	// fields in the order the App writes them (github.FormatMarker) and
	// its values percent-encoded: docs/a b.md, containing <!--%.
	originCheck = "<!-- issuedog:origin plan=#195 rev=1 item=T9 check=file contains=%3C!--%25 human=1 value=docs/a%20b.md -->"
)

// originTask files issue 1, the App's task at stage, its body ending in
// origin marker m, with the App's move there, then runs edit. The owner is
// an admin and mallory a writer, as discPass's trust has them.
func originTask(stage, m string, edit func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.SetPermission("owner", "admin")
		f.SetPermission("mallory", "write")
		n := f.AddIssue(app, "T2. A task", "The task.\n\n"+m)
		f.SetType(n, "task")
		f.AddLabel(n, app, "stage:"+stage)
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to="+stage+" event=p500 -->")
		edit(t, f)
		return f
	}
}

// originDerived is issue n's state as the fake has it now, read fresh,
// with its dirty content under discPass's trust.
func originDerived(t *testing.T, f *githubtest.Fake, n int) state.Issue {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	d, err := (&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}).Dirty(context.Background(), s, discTrust{})
	if err != nil {
		t.Fatal(err)
	}
	return state.Derive(s, slug, d.Has)
}

// originClass fails t unless issue n's Origin is want, read from a
// comment, not the body, and its class is class.
func originClass(t *testing.T, f *githubtest.Fake, n int, when string, want state.Origin, class state.Class) {
	t.Helper()
	is := originDerived(t, f, n)
	got := is.Origin
	if got.Comment == state.BodyID {
		t.Errorf("%s: origin %+v is read from the body; want it from a comment", when, got)
	}
	got.Comment = 0
	if got != want {
		t.Errorf("%s: origin = %+v; want %+v", when, got, want)
	}
	if c, err := state.ClassOf(is.Item); err != nil || c != class {
		t.Errorf("%s: class = %s, %v; want %s", when, c, err, class)
	}
}

// originAdded is the bodies of the comments on issue n of f after the
// first had of them.
func originAdded(f *githubtest.Fake, n, had int) []string {
	var added []string
	for _, c := range f.State().Issues[n-1].Comments[had:] {
		added = append(added, c.Body)
	}
	return added
}

// An untrusted write collaborator's edit that removes the marker holds the
// issue, as before, and one App comment reposts the origin from the App's
// last version, before the hold's comment. The issue keeps its class
// through the hold, and after the owner's /resume ends it, with no second
// repost. Replay passes.
func TestOriginRepostUntrustedEdit(t *testing.T) {
	t.Parallel()
	setup := originTask("ready", originT2, func(_ testing.TB, f *githubtest.Fake) {
		f.EditBody(1, "mallory", "The task, as mallory sees it.")
	})
	githubtest.Replay(t, setup, discPass(t, 1))
	f := run(t, setup, discPass(t, 1))
	added := originAdded(f, 1, 1)
	if len(added) != 2 || added[0] != originRepost(originT2) ||
		!strings.HasPrefix(added[1], "On hold for discussion, from `ready`: `mallory` edited the issue description") {
		t.Fatalf("new comments %q; want the repost of %s, then the hold", added, originT2)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Errorf("labels %q; want stage:held", l)
	}
	want := state.Origin{Plan: 195, Rev: 1, Item: "T2"}
	originClass(t, f, 1, "held", want, state.ClassTask)
	f.AddComment(1, "owner", "/resume")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("labels after /resume %q, newest comment %q; want stage:ready", l, newestComment(f, 1))
	}
	originClass(t, f, 1, "after the hold", want, state.ClassTask)
	writes := len(f.Writes())
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != writes {
		t.Errorf("a pass after the hold wrote %+v; want nothing", w[writes:])
	}
	if n := strings.Count(strings.Join(originAdded(f, 1, 0), "\n"), originRepost(originT2)); n != 1 {
		t.Errorf("%d reposts; want 1", n)
	}
}

// The owner's edit of a plan task's body, even one that keeps the marker's
// text, reposts it, with no hold, and the task keeps its class. Replay
// passes.
func TestOriginRepostOwnerEdit(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"an edit that removes the marker": "The task, put better.",
		"an edit that keeps its text":     "The task, put better.\n\n" + originT2,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			setup := originTask("ready", originT2, func(_ testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", body) })
			githubtest.Replay(t, setup, discPass(t, 1))
			f := run(t, setup, discPass(t, 1))
			if added := originAdded(f, 1, 1); !slices.Equal(added, []string{originRepost(originT2)}) {
				t.Errorf("new comments %q; want only the repost of %s", added, originT2)
			}
			if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:ready"}) || is.Body != body {
				t.Errorf("labels %q and body %q; want stage:ready, and the body as the owner left it", is.Labels, is.Body)
			}
			originClass(t, f, 1, "after the repost", state.Origin{Plan: 195, Rev: 1, Item: "T2"}, state.ClassTask)
		})
	}
}

// A human task's repost carries every field of its marker as the App
// wrote it: human=1 and its check's, values still percent-encoded. So it
// stays a human task, and its check reads back whole, as /done runs it
// (TestOriginRepostCheckRuns). Replay passes.
func TestOriginRepostHumanCheck(t *testing.T) {
	t.Parallel()
	setup := originTask("needs-human", originCheck, func(_ testing.TB, f *githubtest.Fake) {
		f.EditBody(1, "owner", "The human task, put better.")
	})
	githubtest.Replay(t, setup, discPass(t, 1))
	f := run(t, setup, discPass(t, 1))
	if added := originAdded(f, 1, 1); !slices.Equal(added, []string{originRepost(originCheck)}) {
		t.Errorf("new comments %q; want only the repost of %s", added, originCheck)
	}
	originClass(t, f, 1, "after the repost", state.Origin{Plan: 195, Rev: 1, Item: "T9", Human: true,
		Check: state.Check{Kind: state.CheckFile, Value: "docs/a b.md", Contains: "<!--%"}}, state.ClassHumanTask)
}

// The marker comes from the newest version of the body the App wrote that
// has one: the App's rewrite, or, once that version is deleted from the
// history, the original post. Replay passes.
func TestOriginRepostHistory(t *testing.T) {
	t.Parallel()
	const rev2 = "<!-- issuedog:origin plan=#195 rev=2 item=T2 -->"
	for _, tt := range []struct {
		name    string
		deleted bool
		want    string
	}{
		{"the App's last version", false, rev2},
		{"the App's last version deleted", true, originT2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := originTask("ready", originT2, func(_ testing.TB, f *githubtest.Fake) {
				f.EditBody(1, app, "The task, rewritten.\n\n"+rev2)
				f.EditBody(1, "owner", "The task, put better.")
				if tt.deleted {
					f.DeleteRevision(1, 1) // the App's rewrite: the original post is 0
				}
			})
			githubtest.Replay(t, setup, discPass(t, 1))
			f := run(t, setup, discPass(t, 1))
			if added := originAdded(f, 1, 1); !slices.Equal(added, []string{originRepost(tt.want)}) {
				t.Errorf("new comments %q; want only the repost of %s", added, tt.want)
			}
		})
	}
}

// With every version of the App's gone from the history, nothing is
// posted, and one warning is logged: the next pass over the same body
// logs nothing more and reads no history. A new edit is looked at again.
func TestOriginRepostNone(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.CaptureTo).
	f := originTask("ready", originT2, func(_ testing.TB, f *githubtest.Fake) {
		f.EditBody(1, "owner", "The task, put better.")
		f.DeleteRevision(1, 0)
	})(t)
	c := f.Client(t)
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug, Notify: []string{"owner"}},
		Trust: discTrust{}, Commands: daemon.Commands(discTrust{})}
	pass := func() {
		t.Helper()
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Run(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	var warnings bytes.Buffer
	stop := logtest.CaptureTo(t, &warnings, logr.W)
	pass()
	histories := f.HistoryQueries()
	pass()
	logr.Wait()
	const warning = "o/r#1: the App filed it, and its description has lost its origin marker"
	if got := warnings.String(); strings.Count(got, warning) != 1 {
		t.Errorf("warnings = %q; want one, %q", got, warning)
	}
	if q := f.HistoryQueries() - histories; q != 0 {
		t.Errorf("the second pass sent %d history queries; want none", q)
	}
	f.EditBody(1, "owner", "The task, put better again.")
	pass()
	logr.Wait()
	stop()
	if got := warnings.String(); strings.Count(got, warning) != 2 {
		t.Errorf("warnings = %q; want a second, for the new edit", got)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v; want none", w)
	}
}

// An edit of the reposted comment makes it dirty, so it gives no origin,
// and the origin is reposted exactly once more. Replay passes.
func TestOriginRepostEdited(t *testing.T) {
	t.Parallel()
	setup := originTask("ready", originT2, func(_ testing.TB, f *githubtest.Fake) {
		f.EditBody(1, "owner", "The task, put better.")
		f.EditComment(f.AddComment(1, app, originRepost(originT2)), "owner", originRepost(originT2)+" Edited.")
	})
	githubtest.Replay(t, setup, discPass(t, 1))
	f := run(t, setup, discPass(t, 1))
	cs := originAdded(f, 1, 1)
	if len(cs) != 2 || !strings.HasPrefix(cs[0], "<!-- issuedog:dirty by=owner -->") || cs[1] != originRepost(originT2) {
		t.Errorf("comments after the move %q; want the edited repost, noted, then one new repost", cs)
	}
	originClass(t, f, 1, "after the new repost", state.Origin{Plan: 195, Rev: 1, Item: "T2"}, state.ClassTask)
}

// A replay of the plan's approval matches each child, a human task whose
// origin lives only in a reposted comment included, and files no second
// issue: whether the owner's edit removed the marker, or mallory's, whose
// text the App's note escapes.
func TestOriginRepostChildMatched(t *testing.T) {
	t.Parallel()
	for _, editor := range []string{"owner", "mallory"} {
		t.Run(editor, func(t *testing.T) {
			t.Parallel()
			f := planAtReview(threeTasks, thumbsUp)(t)
			if err := childrenPass(t, threeTasks)(f); err != nil {
				t.Fatal(err)
			}
			checkChildren(t, f, threeTasks)
			const child = 4 // T3, the human task
			had := len(f.State().Issues[child-1].Comments)
			f.EditBody(child, editor, "The human task, put better.")
			if err := discPass(t, child)(f); err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(originAdded(f, child, had), func(c string) bool {
				return strings.HasPrefix(c, originRepost("<!-- issuedog:origin plan=#1 rev=1 item=T3 "))
			}) {
				t.Fatalf("#%d's new comments %q; want the repost of its origin", child, originAdded(f, child, had))
			}
			if o := originDerived(t, f, child).Origin; o.Plan != 1 || o.Item != "T3" || o.Comment == state.BodyID {
				t.Fatalf("#%d's origin = %+v; want item T3 of #1's plan, from the repost", child, o)
			}
			issues, writes := len(f.State().Issues), len(f.Writes())
			if err := childrenPass(t, threeTasks)(f); err != nil {
				t.Fatal(err)
			}
			if got := len(f.State().Issues); got != issues {
				t.Errorf("%d issues after the replay; want %d, no second child", got, issues)
			}
			if w := f.Writes(); len(w) != writes {
				t.Errorf("the replay wrote %+v; want nothing", w[writes:])
			}
		})
	}
}

// Through the loop: a pass over an App task that keeps its origin reads no
// history. Once the owner edits it out, the pass that reposts it reads the
// body's history only once, as Dirty does, and the repost uses that one.
// After the App's comment is read back, a pass where nothing changed sends
// only conditional GETs, answered 304, writes nothing and reads no
// history.
func TestOriginRepostQuiet(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := f.AddIssue(app, "T2. A task", "The task.\n\n"+originT2)
	f.AddLabel(n, app, "type:task")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready event=p500 -->")
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	if q, w := f.HistoryQueries(), f.Writes(); q != 0 || len(w) != 0 {
		t.Fatalf("a pass over the task as the App filed it sent %d history queries and wrote %+v; want none", q, w)
	}
	f.EditBody(n, "owner", "The task, put better.")
	iterate(t, d)
	if q := f.HistoryQueries(); q != 1 {
		t.Errorf("the pass that reposts sent %d history queries; want 1", q)
	}
	if added := originAdded(f, n, 1); !slices.Equal(added, []string{originRepost(originT2)}) {
		t.Errorf("new comments %q; want only the repost", added)
	}
	originQuiet(t, f, d)
	if c := strings.Count(strings.Join(originAdded(f, n, 0), "\n"), originRepost(originT2)); c != 1 {
		t.Errorf("%d reposts; want 1", c)
	}
}

// Nothing is reposted, and no history read beyond what Dirty reads, for an
// issue the App didn't file, even one whose original post has an origin
// marker of its own, nor for an App issue whose body is read and has
// none: one never edited, or one the App edited last, after the owner,
// trusted.
func TestOriginRepostNotDue(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		edit func(f *githubtest.Fake, n int)
	}{
		{"the owner's issue, with a marker of the owner's, edited", nil},
		{"the App's issue with no origin, never edited", func(*githubtest.Fake, int) {}},
		{"the App's issue with no origin, the App's edit last", func(f *githubtest.Fake, n int) {
			f.EditBody(n, "owner", "Edited.")
			f.EditBody(n, app, "Rewritten.")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", slug)
			f.SetPermission("owner", "admin")
			var n int
			if tt.edit == nil {
				n = f.AddIssue("owner", "A task", "The task.\n\n"+originT2)
				f.EditBody(n, "owner", "The task, put better.")
			} else {
				n = f.AddIssue(app, "A task", "The task.")
				tt.edit(f, n)
			}
			f.SetType(n, "task")
			f.AddLabel(n, app, "stage:ready")
			f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
			f.ForgetWrites()
			c := f.Client(t)
			s, err := c.Snapshot(context.Background(), "o", "r", n)
			if err != nil {
				t.Fatal(err)
			}
			// Dirty alone, on an Applier of its own, which keeps no
			// history for step 1's, gives the queries step 1 may send.
			before := f.HistoryQueries()
			if _, err := (&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}).Dirty(context.Background(), s, trusted{"owner": true}); err != nil {
				t.Fatal(err)
			}
			dirty := f.HistoryQueries() - before
			st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: trusted{"owner": true}, Commands: daemon.Commands(trusted{"owner": true})}
			before = f.HistoryQueries()
			if _, err := st.Run(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes %+v; want none", w)
			}
			if q := f.HistoryQueries() - before; q != dirty {
				t.Errorf("step 1 sent %d history queries; want %d, Dirty's alone", q, dirty)
			}
		})
	}
}

// /done on a human task whose origin lives only in the repost runs the
// reposted check (#285): it closes the task once the check passes, and
// replies with what it found when it fails, the task staying open.
func TestOriginRepostDone(t *testing.T) {
	t.Parallel()
	for _, pass := range []bool{true, false} {
		t.Run(fmt.Sprint("passes=", pass), func(t *testing.T) {
			t.Parallel()
			f := humanTask(doneLabelCheck, func(_ testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", "Add it, put better.") })(t)
			if pass {
				f.AddRepoLabel("needs-design")
			}
			d := doneDaemon(t, f, newOrigin(t))
			iterate(t, d)
			marker := "<!-- issuedog:origin plan=#9 rev=1 item=T2 check=label human=1 value=needs-design -->"
			if c := newestComment(f, 1); c != originRepost(marker) {
				t.Fatalf("newest comment %q; want the repost of %s", c, marker)
			}
			id := f.AddComment(1, "owner", "/done")
			iterate(t, d)
			want := rejected("/done", "its \"Done when\" check didn't pass. This repo has no label `needs-design`", fmt.Sprint(id))
			if pass {
				want = doneClosed("The label `needs-design` exists.", id)
			}
			if is := f.State().Issues[0]; is.Closed != pass || newestComment(f, 1) != want {
				t.Errorf("closed %t, newest comment %q; want %t and %q", is.Closed, newestComment(f, 1), pass, want)
			}
		})
	}
}
