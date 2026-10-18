package daemon_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Adopted children (#372, from #195's story verification): an issue a
// person filed, linked under a plan's parent, whose clean App comment
// carries its origin marker, as #195's does for #224's plan, is the child
// of the item that marker names, for the planner's revision and for
// ChildrenVisit alike. A revision keeps, changes or drops it like any
// child. The same marker in a person's comment, or in an App comment
// someone else edited, counts for nothing.

// adoptedIDs are the issues and comments adoptedStory files.
type adoptedIDs struct {
	n             int   // the story
	plan, approve int64 // plan revision 1's comment, and its approval
	t1            int   // T1's child, the App's, closed as completed
	t2, t3        int   // T2's and T3's children, each a person's, adopted
	other         int   // a bug filed by hand, with no origin
	hold          int64 // owner's /hold on T2's child, if held
	rev           int64 // plan revision 2's comment, if posted
}

// adoptedOwnText is the body owner wrote for T2's child, which a
// changed child's rewrite replaces whole.
const adoptedOwnText = "Owner's own words: refuse a quote past its time."

// adoptedComment is the App's comment that adopts an issue a person filed
// as item id of issue 1's plan revision 1, with marker's further fields.
func adoptedComment(id, more string) string {
	return fmt.Sprintf("Adopted by #1's plan, rev 1, as %s.\n\n<!-- issuedog:origin plan=#1 rev=1 item=%s%s -->", id, id, more)
}

// adoptedStory files owner's story "Quotes" as issue 1, planned before,
// as revStory does: the golden story plan as revision 1, approved into
// planned, then brought back to planning by the owner. Its children are
// T1's, the App's, linked and closed as completed; T2's, #3, which owner
// filed, at ready; and T3's, #4, a human task owner filed, at
// needs-human. Each of T2's and T3's is linked, and has the App's comment
// adopting it, which carries its origin, and its first move, keyed by
// revision 1's approval. A bug owner filed, #5, is linked too, with no
// origin. If held, owner's /hold holds T2's child from ready. Then edit,
// if set, changes the issues.
func adoptedStory(t testing.TB, f *githubtest.Fake, held bool, edit func(*githubtest.Fake, adoptedIDs)) adoptedIDs {
	t.Helper()
	var r adoptedIDs
	r.n, _ = plFiled(f, "story", false, plEntry)
	r.plan = revMove(t, f, r.n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"},
		PlanRev: 1, Text: planText(r.n, 1), Markers: []github.Marker{planData(t, threeTasks)}})
	ms := github.ParseMarkers(f.State().Issues[r.n-1].Comments[len(f.State().Issues[r.n-1].Comments)-1].Body)
	r.approve = f.AddComment(r.n, app, fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r9 rev=1 body=%s -->",
		ms[len(ms)-1].Fields["body"]))
	r.t1 = revChild(f, r.n, "T1", "Record when each quote expires", "stage:done", true)
	f.CloseAs(r.t1, app, "completed")
	adopt := func(title, body, id, more, stage string) int {
		c := f.AddIssue("owner", title, body, "type:task")
		f.AddLabel(c, app, "stage:"+stage)
		f.SetParent(c, r.n)
		f.AddComment(c, app, adoptedComment(id, more))
		f.AddComment(c, app, fmt.Sprintf("Created.\n\n<!-- issuedog:transition from=intake to=%s event=p%d -->", stage, r.approve))
		return c
	}
	r.t2 = adopt("Refuse an expired quote at booking", adoptedOwnText, "T2", "", "ready")
	r.t3 = adopt("Set the quote lifetime in production", "Owner's own human task.", "T3", " human=1", "needs-human")
	r.other = f.AddIssue("owner", "A bug found on the way", "", "type:bug")
	f.SetParent(r.other, r.n)
	if held {
		r.hold = f.AddComment(r.t2, "owner", "/hold")
		f.AddComment(r.t2, app, fmt.Sprintf("On hold.\n\n<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=%d -->", r.hold))
		f.AddReaction(r.hold, app, "+1")
		f.RemoveLabel(r.t2, app, "stage:ready")
		f.AddLabel(r.t2, app, "stage:held")
	}
	if edit != nil {
		edit(f, r)
	}
	f.AddComment(r.n, app, "Moved.\n\n<!-- issuedog:transition "+revByOwner+" -->")
	f.RemoveLabel(r.n, app, "stage:plan-review")
	f.AddLabel(r.n, app, "stage:planning")
	return r
}

// adoptedApproved is adoptedStory with the golden story revision posted
// as plan revision 2 at plan-review, with owner's 👍 on it, and the
// writes forgotten: T1 kept, the adopted T2 changed, the adopted T3
// dropped, and T4 new.
func adoptedApproved(t testing.TB, f *githubtest.Fake, held bool, edit func(*githubtest.Fake, adoptedIDs)) adoptedIDs {
	t.Helper()
	r := adoptedStory(t, f, held, edit)
	r.rev = revMove(t, f, r.n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p2"},
		PlanRev: 2, Text: planText(r.n, 2), Markers: []github.Marker{changedPlanData(t, plGolden(t, "story-revision"))}})
	f.AddReaction(r.rev, "owner", "+1")
	f.ForgetWrites()
	return r
}

// adoptedSetup is adoptedApproved on a new fake, for Replay.
func adoptedSetup(held bool, edit func(*githubtest.Fake, adoptedIDs)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		adoptedApproved(t, f, held, edit)
		return f
	}
}

// adoptedOrigin is the Origin Derive reads for issue n, from a fresh
// snapshot, with its dirty content as owner's trust has it.
func adoptedOrigin(t *testing.T, f *githubtest.Fake, n int) state.Origin {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	a := &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}
	d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
	if err != nil {
		t.Fatal(err)
	}
	return state.Derive(s, slug, d.Has).Origin
}

// The planner's context for a revision lists the adopted T2 and T3 among
// the children of revision 1, each by its item, and leaves them out of
// the other sub-issues. A person's issue whose origin marker is in a
// person's comment, and one whose App comment someone else edited, are
// other sub-issues: neither marker counts.
func TestAdoptedPlannerContext(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	var decoys [2]int
	r := adoptedStory(t, f, false, func(f *githubtest.Fake, r adoptedIDs) {
		decoys[0] = f.AddIssue("owner", "A note with its own marker", "", "type:task")
		f.SetParent(decoys[0], r.n)
		f.AddComment(decoys[0], "owner", "<!-- issuedog:origin plan=#1 rev=1 item=T5 -->")
		decoys[1] = f.AddIssue("owner", "A task whose App comment was edited", "", "type:task")
		f.SetParent(decoys[1], r.n)
		f.EditComment(f.AddComment(decoys[1], app, adoptedComment("T6", "")), "mallory", adoptedComment("T6", ""))
	})
	d := ivDaemon(t, f, newOrigin(t))
	files := planContext(t, d, r.n)
	want := fmt.Sprintf("## Children of plan revision 1\n\n- T1: #%d, Record when each quote expires (type task, closed as completed)\n"+
		"- T2: #%d, Refuse an expired quote at booking (type task, `stage:ready`)\n"+
		"- T3: #%d, Set the quote lifetime in production (type task, `stage:needs-human`)\n\n", r.t1, r.t2, r.t3)
	if !strings.Contains(files["children.md"], want) {
		t.Errorf("children.md lacks\n%s\nin\n%s", want, files["children.md"])
	}
	others := files["children.md"][strings.Index(files["children.md"], "## Other sub-issues"):]
	for _, n := range []int{r.t2, r.t3} {
		if strings.Contains(others, fmt.Sprintf("- #%d: ", n)) {
			t.Errorf("the other sub-issues list the adopted #%d:\n%s", n, others)
		}
	}
	for _, n := range []int{r.other, decoys[0], decoys[1]} {
		if !strings.Contains(others, fmt.Sprintf("- #%d: ", n)) {
			t.Errorf("the other sub-issues lack #%d, whose origin doesn't count:\n%s", n, others)
		}
	}
	for _, id := range []string{"T5", "T6"} {
		if strings.Contains(files["children.md"], "- "+id+": ") {
			t.Errorf("children.md lists a child of item %s, from a marker that doesn't count:\n%s", id, files["children.md"])
		}
	}
}

// A revision that keeps T1, changes the adopted T2, drops the adopted T3
// and adds T4 passes the revision's checks, each child listed once, so
// it's posted as plan revision 2 and waits at plan-review: before #372,
// rule children refused it, as no child of T2 or T3 was found.
func TestAdoptedPlannerRevision(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	r := adoptedStory(t, f, false, nil)
	g := plRig(t, f, revPlanReview)
	g.claude(ivResult(t, plGolden(t, "story-revision")), 0)
	if err := g.work(r.n); err != nil {
		t.Fatal(err)
	}
	text, markers := plComment(t, f, r.n)
	if len(markers) != 3 || !strings.HasPrefix(markers[0], "<!-- issuedog:transition from=planning to=plan-review rev=2 run=run1 ") {
		t.Fatalf("markers %q, with text\n%s\nwant the move to plan-review with plan revision 2", markers, text)
	}
	for _, want := range []string{
		"\n\n**T2. Refuse an expired quote at booking, saying when it expired** · changed · scope:booking · p1\nOutcome: ",
		"\n\n**T3. Set the quote lifetime in production** · dropped\n\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the revision's text lacks %q:\n%s", want, text)
		}
	}
}

// The owner's 👍 on the revision applies it to the adopted children as to
// any other. T2's child, which owner filed, is rewritten whole from
// revision 2, title and body, with none of owner's text, and its labels
// set to the item's; its move back to ready carries the origin marker
// naming revision 2, which its body can't give, and says so, so Derive
// reads revision 2 from it. T3's, which owner filed, is closed as not
// planned. T4's child is filed, and the record lists T1's, T2's and T4's
// children. A crash at any write, before or after it applies, writes
// nothing twice. Once the record stands, a visit to the story sends no
// request: no adopted child is read again.
func TestAdoptedChildrenChangedDropped(t *testing.T) {
	t.Parallel()
	setup := adoptedSetup(false, nil)
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	issues := f.State().Issues
	p, plan := changedApproval(t, f)
	if len(issues) != 6 {
		t.Fatalf("%d issues, want one filed, for T4", len(issues))
	}
	t2 := issues[2]
	originMarker := "<!-- issuedog:origin plan=#1 rev=2 item=T2 -->"
	if t2.Title != "Refuse an expired quote at booking, saying when it expired" || t2.BodyEditedBy != app || t2.Author != "owner" ||
		!strings.HasPrefix(t2.Body, "<!-- issuedog:rewrite run=p2 -->\n**Parent:** #1   **Scope:** scope:booking   **Priority:** p1\n\n## Context\n\n") ||
		!strings.HasSuffix(t2.Body, "\n\n"+originMarker) || strings.Contains(t2.Body, "Owner's own words") {
		t.Errorf("T2's child is %q, by %q, last edited by %q, with body\n%s\nwant revision 2's T2, rewritten whole by the App, none of owner's text kept",
			t2.Title, t2.Author, t2.BodyEditedBy, t2.Body)
	}
	if want := []string{"priority:p1", "scope:booking", "stage:ready", "type:task"}; !reflect.DeepEqual(t2.Labels, want) {
		t.Errorf("T2's child is labelled %v, want %v", t2.Labels, want)
	}
	want := changedMoveText("T2", plan, "It was filed by hand, so this comment carries its `origin` marker, which its description can't: "+
		"I've rewritten the description whole from the plan, and the earlier one stays in its edit history. It goes back to `stage:ready`.") +
		fmt.Sprintf("\n\n<!-- issuedog:transition from=ready to=ready event=p%d -->\n%s", p, originMarker)
	if got := t2.Comments[len(t2.Comments)-1].Body; got != want {
		t.Errorf("T2's child's newest comment is\n%q\nwant\n%q", got, want)
	}
	if o := adoptedOrigin(t, f, 3); o.Plan != 1 || o.Rev != 2 || o.Item != "T2" || o.Comment == state.BodyID {
		t.Errorf("T2's child's origin is %+v; want item T2 of #1's plan, revision 2, from its move's comment", o)
	}
	if got := t2.BlockedBy; !slices.Equal(got, []int{2, 6}) {
		t.Errorf("T2's child is blocked by %v, want T1's #2 and T4's #6", got)
	}

	t3 := issues[3]
	if !t3.Closed || droppedReason(t, f, 4) != "not_planned" || t3.Body != "Owner's own human task." {
		t.Errorf("T3's child is closed %t, as %q, with body %q; want it closed as not planned, its body untouched", t3.Closed, droppedReason(t, f, 4), t3.Body)
	}
	text := fmt.Sprintf("Closed as not planned, since #1's plan, rev 2, drops T3 ([the plan](https://github.com/o/r/issues/1#issuecomment-%d)).", plan)
	if got := t3.Comments[len(t3.Comments)-1].Body; !strings.HasPrefix(got, text+"\n\n") {
		t.Errorf("T3's child's newest comment is\n%q\nwant it to start\n%q", got, text)
	}

	wantRec := "I've applied plan rev 2 to the children, which are now:\n- T1 #2: Record when each quote expires\n" +
		"- T2 #3: Refuse an expired quote at booking, saying when it expired\n- T4 #6: Give the quote lifetime a default\n\n" +
		"<!-- issuedog:created rev=2 children=2,3,6 -->"
	if recs := droppedRecords(f, 1); len(recs) != 1 || recs[0] != wantRec {
		t.Errorf("issue 1's records are %q, want only\n%q", recs, wantRec)
	}
	droppedNoWrite(t, f, 5, "the bug filed by hand")

	st := childrenStep(t, f, revisionPlan)
	s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	from := len(f.Requests())
	if err := st.ChildrenVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if r := f.Requests()[from:]; len(r) != 0 {
		t.Errorf("a visit to the story with its record sent %+v; want nothing", r)
	}
}

// An adopted child whose body an untrusted user made dirty is rewritten
// whole, its rewrite marker naming who made it dirty, and a crash just
// after the rewrite doesn't rewrite it again: the App's rewrite, clean,
// names revision 2, so the replay goes on to the move. A held adopted
// child stays held, and its held → held move carries the origin marker
// naming revision 2. An origin marker owner wrote in the body, naming
// revision 2 with human=1, counts for nothing: the body is still
// rewritten, and the move carries the App's marker, with no human=1.
func TestAdoptedChildChangedDirtyHeld(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		held       bool
		edit       func(*githubtest.Fake, adoptedIDs)
		head, gone string
	}{
		"dirty": {false, func(f *githubtest.Fake, r adoptedIDs) { f.EditBody(r.t2, "eve", "Eve's words.") },
			"<!-- issuedog:rewrite run=p2 replaced=eve -->\n", "Eve's words"},
		"held": {true, nil, "<!-- issuedog:rewrite run=p2 -->\n", adoptedOwnText},
		"a marker in owner's body": {false, func(f *githubtest.Fake, r adoptedIDs) {
			f.EditBody(r.t2, "owner", "Owner's words.\n\n<!-- issuedog:origin plan=#1 rev=2 item=T2 human=1 -->")
		}, "<!-- issuedog:rewrite run=p2 -->\n", "Owner's words"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			setup := adoptedSetup(tt.held, tt.edit)
			githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
			f := run(t, setup, childrenPass(t, revisionPlan))
			t2 := f.State().Issues[2]
			if !strings.HasPrefix(t2.Body, tt.head) || strings.Contains(t2.Body, tt.gone) || strings.Contains(t2.Body, "human=1") {
				t.Errorf("T2's child's body is\n%s\nwant the App's rewrite, starting %q, with no %q", t2.Body, tt.head, tt.gone)
			}
			if updates := slices.DeleteFunc(f.Writes(), func(w githubtest.Write) bool { return w.Method != "PATCH" || w.Path != "/issues/3" }); len(updates) != 1 {
				t.Errorf("T2's child got %d edits, %+v; want one rewrite", len(updates), updates)
			}
			last := t2.Comments[len(t2.Comments)-1].Body
			if !strings.HasSuffix(last, "\n<!-- issuedog:origin plan=#1 rev=2 item=T2 -->") {
				t.Errorf("T2's child's newest comment is\n%s\nwant its move, carrying its origin, naming revision 2", last)
			}
			if tt.held && !strings.Contains(last, "<!-- issuedog:transition from=held to=held ") {
				t.Errorf("T2's child's newest comment is\n%s\nwant a held → held move", last)
			}
			if o := adoptedOrigin(t, f, 3); o.Rev != 2 || o.Item != "T2" {
				t.Errorf("T2's child's origin is %+v; want item T2, revision 2", o)
			}
		})
	}
}
