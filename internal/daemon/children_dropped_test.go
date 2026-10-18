package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The rest of an approved revision (#195's T8, #297): its new children
// filed with their links, each dropped item's child closed as not
// planned with everything below it, deepest first, and unlinked, a
// changed child's links to its siblings following the revision, and last
// the revision's record, after which a visit reads nothing more.

// droppedStoryRevision is the golden story revision (T1 kept, T2
// changed, T3 dropped, T4 new), with the changed T2 depending on T4
// alone: its link to the kept T1's child goes, and one to T4's comes.
func droppedStoryRevision(t testing.TB) map[string]any {
	t.Helper()
	out := plGolden(t, "story-revision")
	plItem(t, out, 1)["depends_on"] = []any{"T4"}
	return out
}

// droppedStory returns a setup for Replay: changedStory with
// droppedStoryRevision as plan revision 2, so T3's child, the human task,
// linked, is dropped. Before the revision, T2's child, #3, is blocked by
// T1's, #2, and T3's, #4, as revision 1's T2 depends on both, and by the
// bug filed by hand, #5, and the bug is blocked by T3's child, each link
// added by hand. Then edit, if set, changes the issues.
func droppedStory(edit func(*githubtest.Fake, changedIDs)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		changedStoryWith(t, f, false, func(f *githubtest.Fake, r changedIDs) {
			f.SetBlockedBy(r.t2, r.t1)
			f.SetBlockedBy(r.t2, r.t3)
			f.SetBlockedBy(r.t2, r.other)
			f.SetBlockedBy(r.other, r.t3)
			if edit != nil {
				edit(f, r)
			}
		}, changedPlanData(t, droppedStoryRevision(t)))
		return f
	}
}

// droppedReason is issue n's state_reason, as GitHub gives it.
func droppedReason(t *testing.T, f *githubtest.Fake, n int) string {
	t.Helper()
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	return is.GetStateReason()
}

// droppedNoWrite fails t if f has a write to issue n.
func droppedNoWrite(t *testing.T, f *githubtest.Fake, n int, what string) {
	t.Helper()
	for _, w := range f.Writes() {
		if strings.HasPrefix(w.Path, fmt.Sprintf("/issues/%d/", n)) || w.Path == fmt.Sprintf("/issues/%d", n) {
			t.Errorf("#%d, %s, got the write %+v", n, what, w)
		}
	}
}

// droppedRecords are the bodies of issue n's comments that carry a
// created marker.
func droppedRecords(f *githubtest.Fake, n int) []string {
	var records []string
	for _, c := range f.State().Issues[n-1].Comments {
		if strings.Contains(c.Body, "<!-- issuedog:created ") {
			records = append(records, c.Body)
		}
	}
	return records
}

// The owner's 👍 on a story's revision that drops the human task T3 and
// adds T4: T4's child, #6, is filed, blocked by the kept T1's #2. T3's
// child, #4, is closed as not planned, with a comment naming the revision
// and linking its plan, and its links go both ways: the bug, #5, is no
// longer blocked by it, nor is the changed T2's #3. T2's links to its
// siblings follow the revision, which has it depend on T4 alone: its link
// to T1's #2 goes, and one to T4's #6 comes, while its link to the bug,
// which isn't the plan's, stays. The kept T1's child, closed, gets no
// write. The record of revision 2 is the last write: T1's, T2's and T4's
// children, the dropped T3's left out. A crash at any write, before or
// after it applies, writes nothing twice.
func TestChildrenDropped(t *testing.T) {
	t.Parallel()
	setup := droppedStory(nil)
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	issues := f.State().Issues
	p, plan := changedApproval(t, f)
	if len(issues) != 6 {
		t.Fatalf("%d issues, want one filed, for T4", len(issues))
	}
	if t4 := issues[5]; !strings.Contains(t4.Body, "<!-- issuedog:origin plan=#1 rev=2 item=T4 -->") || !slices.Equal(t4.BlockedBy, []int{2}) ||
		!slices.Contains(issues[0].SubIssues, 6) {
		t.Errorf("the new child is blocked by %v, and issue 1's sub-issues are %v, with body\n%s\nwant T4's, linked, blocked by T1's #2",
			t4.BlockedBy, issues[0].SubIssues, t4.Body)
	}

	t3 := issues[3]
	if !t3.Closed || droppedReason(t, f, 4) != "not_planned" || len(t3.BlockedBy) != 0 {
		t.Errorf("T3's child is closed %t, as %q, blocked by %v; want it closed as not planned, unlinked", t3.Closed, droppedReason(t, f, 4), t3.BlockedBy)
	}
	text := fmt.Sprintf("Closed as not planned, since #1's plan, rev 2, drops T3 ([the plan](https://github.com/o/r/issues/1#issuecomment-%d)).", plan)
	if got := t3.Comments[len(t3.Comments)-1].Body; !strings.HasPrefix(got, text+"\n\n") {
		t.Errorf("T3's child's newest comment is\n%q\nwant it to start\n%q", got, text)
	}
	want := map[string]string{"from": "needs-human", "to": "closed", "reason": "not_planned", "event": fmt.Sprintf("p%d", p)}
	if m := changedLastMarker(t, f, 4); !reflect.DeepEqual(m.Fields, want) {
		t.Errorf("T3's child's newest marker has %v, want %v", m.Fields, want)
	}
	if got := issues[4].BlockedBy; len(got) != 0 {
		t.Errorf("the bug is blocked by %v, want nothing: T3's child is closed", got)
	}
	if got := issues[2].BlockedBy; !slices.Equal(got, []int{5, 6}) {
		t.Errorf("T2's child is blocked by %v, want the bug's #5, which isn't the plan's, and T4's #6", got)
	}
	droppedNoWrite(t, f, 2, "the kept T1's child, closed")

	recs := droppedRecords(f, 1)
	wantRec := "I've applied plan rev 2 to the children, which are now:\n- T1 #2: Record when each quote expires\n" +
		"- T2 #3: Refuse an expired quote at booking, saying when it expired\n- T4 #6: Give the quote lifetime a default\n\n" +
		"<!-- issuedog:created rev=2 children=2,3,6 -->"
	if len(recs) != 1 || recs[0] != wantRec {
		t.Errorf("issue 1's records are %q, want only\n%q", recs, wantRec)
	}
	if w := f.Writes(); w[len(w)-1].Method != http.MethodPost || w[len(w)-1].Path != "/issues/1/comments" {
		t.Errorf("the last write is %+v, want the record, on issue 1", w[len(w)-1])
	}
	noSearch(t, f)
}

// droppedEpicRevision is the epic's plan revision 2: S1 dropped, S2
// kept, and S3 new, depending on the kept S2. Revision 1 had S1 and S2
// (twoStories), S2 depending on S1.
func droppedEpicRevision(t testing.TB, items ...map[string]any) map[string]any {
	t.Helper()
	out := plGolden(t, "epic-plan")
	s3 := plItem(t, out, 2)
	s3["status"], s3["depends_on"] = "new", []any{"S2"}
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	if items == nil {
		items = []map[string]any{
			{"id": "S1", "title": "Quotes expire after 24 hours", "status": "dropped"},
			{"id": "S2", "title": "Customers see when a quote expires", "status": "kept"},
			s3,
		}
	}
	list := make([]any, len(items))
	for i, it := range items {
		list[i] = it
	}
	plan["items"] = list
	return out
}

// droppedEpic returns a setup for Replay: owner's epic "Quotes", issue 1,
// planned before into twoStories, as plan revision 1, approved, with its
// stories, each the App's, linked under it, with its first move, keyed
// by the approval: S1, #2, at planned with its own tasks, and S2, #3, at
// planning, blocked by S1's #2, as its item depends on S1. Below S1, each
// linked under it: its T1, #4, at ready; its T2, #5, held by owner's
// /hold from ready, blocked by #4; and its T3, #6, closed as completed.
// Below #5, a bug the owner filed, #7, with no stage. The owner's issue
// #8, outside the plan, blocks S1's #2 and the closed #6, and is blocked
// by #4. Then the owner brings the epic back to planning, and revision,
// plan revision 2, is posted at plan-review, with the owner's 👍 on it,
// and the writes are forgotten.
func droppedEpic(revision func(testing.TB) map[string]any) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		n, _ := plFiled(f, "epic", false, plEntry)
		revMove(t, f, n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"},
			PlanRev: 1, Text: planText(n, 1), Markers: []github.Marker{planData(t, twoStories)}})
		cs := f.State().Issues[n-1].Comments
		ms := github.ParseMarkers(cs[len(cs)-1].Body)
		approve := f.AddComment(n, app, fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r9 rev=1 body=%s -->",
			ms[len(ms)-1].Fields["body"]))
		story := func(id, title, stage string) int {
			c := f.AddIssue(app, title, fmt.Sprintf("The story.\n\n<!-- issuedog:origin plan=#%d rev=1 item=%s -->", n, id), "type:story", "stage:"+stage)
			f.SetParent(c, n)
			f.AddComment(c, app, fmt.Sprintf("Created.\n\n<!-- issuedog:transition from=intake to=planning event=p%d -->", approve))
			return c
		}
		s1 := story("S1", "S1. Quotes expire after 24 hours", "planned")
		s1Approve := f.AddComment(s1, app, "Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r8 rev=1 -->")
		s2 := story("S2", "S2. Customers see when a quote expires", "planning")
		f.SetBlockedBy(s2, s1)
		task := func(id, title string) int {
			c := f.AddIssue(app, title, fmt.Sprintf("The task.\n\n<!-- issuedog:origin plan=#%d rev=1 item=%s -->", s1, id), "type:task", "stage:ready")
			f.SetParent(c, s1)
			f.AddComment(c, app, fmt.Sprintf("Created.\n\n<!-- issuedog:transition from=intake to=ready event=p%d -->", s1Approve))
			return c
		}
		t1 := task("T1", "Record when each quote expires")
		t2 := task("T2", "Refuse an expired quote at booking")
		hold := f.AddComment(t2, "owner", "/hold")
		f.AddComment(t2, app, fmt.Sprintf("On hold.\n\n<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=%d -->", hold))
		f.AddReaction(hold, app, "+1")
		f.RemoveLabel(t2, app, "stage:ready")
		f.AddLabel(t2, app, "stage:held")
		f.SetBlockedBy(t2, t1)
		t3 := task("T3", "Set the quote lifetime in production")
		f.CloseAs(t3, app, "completed")
		bug := f.AddIssue("owner", "Expiry is off by an hour", "Found while testing.", "type:bug")
		f.SetParent(bug, t2)
		outside := f.AddIssue("owner", "Document the quote API", "Docs.")
		f.SetBlockedBy(s1, outside)
		f.SetBlockedBy(t3, outside)
		f.SetBlockedBy(outside, t1)
		f.AddComment(n, app, "Moved.\n\n<!-- issuedog:transition "+revByOwner+" -->")
		f.RemoveLabel(n, app, "stage:plan-review")
		f.AddLabel(n, app, "stage:planning")
		c := revMove(t, f, n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p2"},
			PlanRev: 2, Text: planText(n, 2), Markers: []github.Marker{changedPlanData(t, revision(t))}})
		f.AddReaction(c, "owner", "+1")
		f.ForgetWrites()
		return f
	}
}

// The owner's 👍 on an epic's revision that drops the story S1, with its
// tasks below it, keeps S2, and adds S3: S3's child, #9, is filed, linked
// and blocked by the kept S2's #3. S1's subtree closes post-order, so
// deepest first: its T1's #4, then the bug #7 below its T2, then its T2's
// #5, held, whose hold the close ends, then S1's #2 itself, each as not
// planned, each with a comment naming the revision, the ones below S1
// naming it too. The T3 that was closed already, #6, gets no write, and
// keeps its link to #8, which isn't the plan's. No link to a closed one
// remains: the kept S2 is no longer blocked by S1, nor #8 by #4, nor #5
// by #4, and S1 isn't blocked by #8. The record lists S2's and S3's
// children. A crash at any write writes nothing twice.
func TestChildrenDroppedStory(t *testing.T) {
	t.Parallel()
	setup := droppedEpic(func(t testing.TB) map[string]any { return droppedEpicRevision(t) })
	githubtest.Replay(t, setup, childrenPass(t, twoStories))
	f := run(t, setup, childrenPass(t, twoStories))
	issues := f.State().Issues
	p, plan := changedApproval(t, f)
	if len(issues) != 9 {
		t.Fatalf("%d issues, want one filed, for S3", len(issues))
	}
	if s3 := issues[8]; s3.Title != "S3. Expired quotes in the monthly report" || !slices.Equal(s3.BlockedBy, []int{3}) ||
		!slices.Contains(s3.Labels, "stage:planning") || !slices.Equal(issues[0].SubIssues, []int{2, 3, 9}) {
		t.Errorf("the new child is %q, labelled %v, blocked by %v, and the epic's sub-issues %v; want S3's, at planning, blocked by S2's #3, linked",
			s3.Title, s3.Labels, s3.BlockedBy, issues[0].SubIssues)
	}

	var closes []string
	for _, w := range f.Writes() {
		if w.Method == http.MethodPatch {
			closes = append(closes, w.Path)
		}
	}
	if want := []string{"/issues/4", "/issues/7", "/issues/5", "/issues/2"}; !slices.Equal(closes, want) {
		t.Errorf("the issues closed, in order, are %q; want %q, deepest first", closes, want)
	}
	link := fmt.Sprintf("([the plan](https://github.com/o/r/issues/1#issuecomment-%d)).", plan)
	for _, c := range []struct {
		n    int
		from string
		text string
	}{
		{2, "planned", "Closed as not planned, since #1's plan, rev 2, drops S1 " + link},
		{4, "ready", "Closed as not planned, since #1's plan, rev 2, drops S1, #2, which this issue is below " + link},
		{5, "held", "Closed as not planned, since #1's plan, rev 2, drops S1, #2, which this issue is below " + link},
		{7, "intake", "Closed as not planned, since #1's plan, rev 2, drops S1, #2, which this issue is below " + link},
	} {
		is := issues[c.n-1]
		if !is.Closed || droppedReason(t, f, c.n) != "not_planned" {
			t.Errorf("#%d is closed %t, as %q; want it closed as not planned", c.n, is.Closed, droppedReason(t, f, c.n))
		}
		if got := is.Comments[len(is.Comments)-1].Body; !strings.HasPrefix(got, c.text+"\n\n") {
			t.Errorf("#%d's newest comment is\n%q\nwant it to start\n%q", c.n, got, c.text)
		}
		want := map[string]string{"from": c.from, "to": "closed", "reason": "not_planned", "event": fmt.Sprintf("p%d", p)}
		if m := changedLastMarker(t, f, c.n); !reflect.DeepEqual(m.Fields, want) {
			t.Errorf("#%d's newest marker has %v, want %v", c.n, m.Fields, want)
		}
	}
	for n, want := range map[int][]int{2: nil, 3: nil, 4: nil, 5: nil, 6: {8}, 7: nil, 8: nil} {
		if got := issues[n-1].BlockedBy; !slices.Equal(got, want) {
			t.Errorf("#%d is blocked by %v, want %v", n, got, want)
		}
	}
	if issues[2].Closed {
		t.Error("the kept S2's #3 is closed, want it open")
	}
	droppedNoWrite(t, f, 6, "closed before the revision")

	wantRec := "I've applied plan rev 2 to the children, which are now:\n- S2 #3: Customers see when a quote expires\n" +
		"- S3 #9: Expired quotes in the monthly report\n\n<!-- issuedog:created rev=2 children=3,9 -->"
	if recs := droppedRecords(f, 1); len(recs) != 1 || recs[0] != wantRec {
		t.Errorf("the epic's records are %q, want only\n%q", recs, wantRec)
	}
}

// An issue in a dropped subtree whose close the table refuses, here the
// bug #7, of an issue type the pipeline doesn't know, gets that reported
// once, on it, and is left open, with its links; the rest of the subtree
// still closes, and the record still stands, so a later pass writes
// nothing more. A crash at any write writes nothing twice.
func TestChildrenDroppedStateError(t *testing.T) {
	t.Parallel()
	epic := droppedEpic(func(t testing.TB) map[string]any { return droppedEpicRevision(t) })
	setup := func(t testing.TB) *githubtest.Fake {
		f := epic(t)
		f.SetType(7, "Feature")
		return f
	}
	githubtest.Replay(t, setup, childrenPass(t, twoStories))
	f := run(t, setup, childrenPass(t, twoStories))
	issues := f.State().Issues
	if bug := issues[6]; bug.Closed || len(bug.Comments) != 1 || !strings.Contains(bug.Comments[0].Body, "<!-- issuedog:error ") {
		t.Errorf("the bug is closed %t, with comments %+v; want it open, with one error comment", bug.Closed, bug.Comments)
	}
	for _, n := range []int{2, 4, 5} {
		if !issues[n-1].Closed {
			t.Errorf("#%d is open, want it closed", n)
		}
	}
	if recs := droppedRecords(f, 1); len(recs) != 1 || !strings.HasSuffix(recs[0], "<!-- issuedog:created rev=2 children=3,9 -->") {
		t.Errorf("the epic's records are %q, want one listing #3 and #9", recs)
	}
}

// A revision that drops every child leaves none to list: its record has
// children=none, which stands, so a later pass writes nothing more, and
// both stories' subtrees are closed.
func TestChildrenDroppedAll(t *testing.T) {
	t.Parallel()
	setup := droppedEpic(func(t testing.TB) map[string]any {
		return droppedEpicRevision(t,
			map[string]any{"id": "S1", "title": "Quotes expire after 24 hours", "status": "dropped"},
			map[string]any{"id": "S2", "title": "Customers see when a quote expires", "status": "dropped"})
	})
	githubtest.Replay(t, setup, childrenPass(t, twoStories))
	f := run(t, setup, childrenPass(t, twoStories))
	issues := f.State().Issues
	for _, n := range []int{2, 3, 4, 5, 7} {
		if !issues[n-1].Closed {
			t.Errorf("#%d is open, want it closed", n)
		}
	}
	want := "I've applied plan rev 2 to the children, which are now:\n- none\n\n<!-- issuedog:created rev=2 children=none -->"
	if recs := droppedRecords(f, 1); len(recs) != 1 || recs[0] != want {
		t.Errorf("the epic's records are %q, want only\n%q", recs, want)
	}
}

// A kept child that isn't found, as when someone unlinked it from the
// story by hand and it hasn't been updated since the approval, doesn't
// stop the record (#297's decision; #362 would find it): here T3's
// child, never linked, which the revision keeps, and which the new T4 and
// the changed T2 depend on. T4's child is filed, blocked by nothing, and
// T2's is blocked by T1's alone, as no link can name T3's. T3's child gets
// no write. The record lists T1's, T2's and T4's children, and names T3
// as left as it was, so a later pass writes nothing and reads nothing
// more. A crash at any write writes nothing twice.
func TestChildrenDroppedUnfound(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		out := plGolden(t, "story-revision")
		plItem(t, out, 1)["depends_on"] = []any{"T1", "T3"}
		plItem(t, out, 2)["status"] = "kept"
		plItem(t, out, 3)["depends_on"] = []any{"T3"}
		return revApprovedWith(t, changedPlanData(t, out))
	}
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	issues := f.State().Issues
	if len(issues) != 6 {
		t.Fatalf("%d issues, want one filed, for T4", len(issues))
	}
	if got := issues[5].BlockedBy; len(got) != 0 {
		t.Errorf("T4's child is blocked by %v, want nothing: T3's child isn't found", got)
	}
	if got := issues[2].BlockedBy; !slices.Equal(got, []int{2}) {
		t.Errorf("T2's child is blocked by %v, want T1's #2 alone", got)
	}
	droppedNoWrite(t, f, 4, "T3's child, not found")
	want := "I've applied plan rev 2 to the children, which are now:\n- T1 #2: Record when each quote expires\n" +
		"- T2 #3: Refuse an expired quote at booking, saying when it expired\n- T4 #6: Give the quote lifetime a default\n\n" +
		"I found no child of T3, so this revision left it as it was.\n\n<!-- issuedog:created rev=2 children=2,3,6 -->"
	if recs := droppedRecords(f, 1); len(recs) != 1 || recs[0] != want {
		t.Errorf("issue 1's records are %q, want only\n%q", recs, want)
	}
}

// A changed child closed between the plan and its approval gets no write
// at all: no rewrite, no labels, no move and no links, though the
// revision changes it and its item depends on other items. The other
// changed child, T3's, is still rewritten, and the record still lists the
// closed one, as the child of an item the revision doesn't drop.
func TestChildrenDroppedClosedChanged(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		r := changedStory(t, f, false, nil)
		f.CloseAs(r.t2, "owner", "completed")
		return f
	}
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	droppedNoWrite(t, f, 3, "T2's child, closed")
	if t3 := f.State().Issues[3]; !strings.Contains(t3.Body, "rev=2 item=T3") {
		t.Errorf("T3's child has body\n%s\nwant it rewritten from revision 2", t3.Body)
	}
	if recs := droppedRecords(f, 1); len(recs) != 1 || !strings.HasSuffix(recs[0], "<!-- issuedog:created rev=2 children=2,3,4,6 -->") {
		t.Errorf("issue 1's records are %q, want one listing #2, #3, #4 and #6", recs)
	}
}

// droppedDaemon is a daemon of o/r on f whose step approves a plan by
// the owner's 👍, as main wires it, and that adds the number of each
// issue it visits to visits, if set, and keeps the digest if digest is
// set.
func droppedDaemon(t *testing.T, f *githubtest.Fake, o *origin, visits *[]int, digest bool) *daemon.Daemon {
	t.Helper()
	d := newDaemon(t, f, o, nil)
	if digest {
		d = digestDaemon(t, f, o, nil)
	}
	approve := daemon.ApproveByReaction(d.Step.CurrentTrust())
	d.Step.Reactions = func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, dirt daemon.Dirt) error {
		if visits != nil {
			*visits = append(*visits, s.Number)
		}
		return approve(ctx, a, s, dirt)
	}
	d.Work = func(context.Context, []daemon.Ready) error { return nil }
	return d
}

// Through the loop, the pass that approves a revision that drops a child
// writes the record; the next pass writes nothing. Then a state error is
// posted on the story, which keeps it visited on every pass (#295's
// verify): the pass that reads it writes nothing, and the one after,
// unchanged, still visits the story, but sends only conditional GETs that
// GitHub answers 304, so nothing about the children is read.
func TestChildrenDroppedQuiet(t *testing.T) {
	t.Parallel()
	f := droppedStory(nil)(t)
	var visits []int
	d := droppedDaemon(t, f, newOrigin(t), &visits, false)
	iterate(t, d)
	if recs := droppedRecords(f, 1); len(recs) != 1 {
		t.Fatalf("after the approving pass, issue 1's records are %q, want one, of revision 2", recs)
	}
	// The pass after judges the close of T3's child, the App's, and sets
	// its closed stage (SPEC §7.2's move to closed), and writes nothing
	// else.
	n := len(f.Writes())
	iterate(t, d)
	if got, want := digestWrites(f, n), []string{"POST /issues/4/labels", "DELETE /issues/4/labels/stage:needs-human"}; !slices.Equal(got, want) {
		t.Errorf("the pass after wrote %q, want only %q, the close judge's", got, want)
	}
	if got := f.State().Issues[3].Labels; !slices.Contains(got, "stage:discarded") {
		t.Errorf("T3's child is labelled %v, want stage:discarded, as it's closed as not planned", got)
	}
	n = len(f.Writes())
	iterate(t, d)
	if w := f.Writes(); len(w) != n {
		t.Errorf("the pass after that wrote %+v, want nothing", w[n:])
	}
	f.AddComment(1, app, "I can't move this issue from `planned` to `closed` on human-task-done: it isn't a human task. It stays at `planned`.\n\n"+
		"<!-- issuedog:error kind=transition key=r777 -->")
	n = len(f.Writes())
	iterate(t, d)
	if w := f.Writes(); len(w) != n {
		t.Errorf("the pass that read the state error wrote %+v, want nothing", w[n:])
	}
	from, notModified := len(f.Requests()), f.NotModified()
	visits = nil
	iterate(t, d)
	if !slices.Contains(visits, 1) {
		t.Fatalf("an unchanged pass visited %v, want issue 1, whose state error is open", visits)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if f.NotModified() == notModified {
		t.Error("an unchanged pass got no 304")
	}
}

// A revised story whose children have all closed is a finished story in
// the digest once the revision's record stands, its children the ones
// the record lists: the kept T1's, the changed T2's and the new T4's, not
// the dropped T3's. While any of them, or the bug below the story, is
// open, it isn't listed.
func TestChildrenDroppedDigest(t *testing.T) {
	t.Parallel()
	f, o := droppedStory(nil)(t), digestNotifyOrigin(t)
	d := droppedDaemon(t, f, o, nil, true)
	iterate(t, d)
	if recs := droppedRecords(f, 1); len(recs) != 1 {
		t.Fatalf("after the approving pass, issue 1's records are %q, want one, of revision 2", recs)
	}
	digestN := doneDigest(t, f)
	iterate(t, d)
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("with T2's, T4's and the bug open, the digest lists %q; want no finished story", got)
	}
	f.CloseBy(3, "owner")
	f.CloseBy(6, "owner")
	iterate(t, d)
	iterate(t, d)
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("with the bug open below the story, the digest lists %q; want no finished story", got)
	}
	f.CloseBy(5, "owner")
	iterate(t, d)
	iterate(t, d)
	if got := doneListed(f, digestN); len(got) != 1 || !strings.HasPrefix(got[0], "#1 Finished story ") {
		t.Errorf("with every child closed, the digest lists %q; want #1, a finished story", got)
	}
	if body := f.State().Issues[digestN-1].Body; !strings.Contains(body, "- Its children: #2, #3, #6.") {
		t.Errorf("the digest has no line naming the record's children:\n%s", body)
	}
}
