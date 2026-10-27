package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Adopted children through more revisions (#374, from #372's verify): a
// revision keeps an adopted child with no write, a later revision changes
// one it changed before, and its origin stays the latest revision's once
// someone edits the move's comment that carried it, as step 1 posts the
// marker of its rewritten body again (SPEC §6.4). A sub-issue in another
// repo is never read as this repo's issue of that number (SPEC §5.2).

// adoptedMovedText is the line an adopted child's move comment adds, as
// adoptedMoveText in internal/daemon has it.
const adoptedMovedText = "It was filed by hand, so this comment carries its `origin` marker, which its description can't: " +
	"I've rewritten the description whole from the plan, and the earlier one stays in its edit history."

// adoptedRepostLine is the App's line above the origin marker it reposts
// for an adopted child, as adoptedRepostText in internal/daemon has it.
const adoptedRepostLine = "I rewrote this issue's description from its plan, but it was filed by hand, so the `origin` marker there isn't read, " +
	"and no comment of mine that's still read carries it. So I've copied it here, to keep tracking the issue in its plan."

// adoptedSetItem makes it item i of out's plan.
func adoptedSetItem(t testing.TB, out map[string]any, i int, it map[string]any) {
	t.Helper()
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	items, ok := plan["items"].([]any)
	if !ok || i >= len(items) {
		t.Fatalf("plan %v has no item %d", plan, i)
	}
	items[i] = it
}

// adoptedKeptRevision is the golden story revision with the adopted T3
// kept rather than dropped: its data is revision 1's entry for it in full,
// as a revision's data carries a kept item's approved entry
// (planPrior.keptData, #351). T1 is kept, the adopted T2 changed, and T4
// new.
func adoptedKeptRevision(t testing.TB) map[string]any {
	t.Helper()
	out := plGolden(t, "story-revision")
	t3 := plItem(t, plGolden(t, "story-plan"), 2)
	t3["status"] = "kept"
	adoptedSetItem(t, out, 2, t3)
	return out
}

// adoptedAgainTitle is the adopted T2's title in adoptedAgainRevision.
const adoptedAgainTitle = "Refuse an expired quote at booking, saying when and why it expired"

// adoptedAgainCriterion is the criterion adoptedAgainRevision adds to the
// adopted T2.
const adoptedAgainCriterion = "The expired error says whether the quote's lifetime or a fare change ended it."

// adoptedAgainRevision is plan revision 3 after adoptedKeptRevision: the
// adopted T2 changed again, its title, outcome and a new criterion now
// saying why the quote expired, and T1, the adopted T3 and T4 kept, each
// with its entry from revision 2 in full.
func adoptedAgainRevision(t testing.TB) map[string]any {
	t.Helper()
	out := adoptedKeptRevision(t)
	t2 := plItem(t, out, 1)
	t2["title"] = adoptedAgainTitle
	t2["outcome"] = "Booking an expired quote returns an expired error that says when and why it expired, with a link to a new quote, and books nothing."
	criteria, ok := t2["criteria"].([]any)
	if !ok {
		t.Fatalf("T2 %v has no criteria", t2)
	}
	t2["criteria"] = append(slices.Clone(criteria), adoptedAgainCriterion)
	plItem(t, out, 3)["status"] = "kept"
	return out
}

// adoptedApprovalOf returns, of issue 1's comments, the move into planned
// that approved plan revision rev, which keys the changed children's
// moves, and that revision's plan comment, which their comments link.
func adoptedApprovalOf(t *testing.T, f *githubtest.Fake, rev int) (approve, plan int64) {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		switch {
		case c.Author.Login != app:
		case strings.Contains(c.Body, fmt.Sprintf("from=plan-review to=planned rev=%d ", rev)):
			approve = c.ID
		case strings.Contains(c.Body, fmt.Sprintf("<!-- issuedog:revision kind=plan rev=%d ", rev)):
			plan = c.ID
		}
	}
	if approve == 0 || plan == 0 {
		t.Fatalf("issue 1's approval of plan revision %d is comment %d, and its plan comment %d; want both", rev, approve, plan)
	}
	return approve, plan
}

// adoptedNewest is the body of issue n's newest comment.
func adoptedNewest(f *githubtest.Fake, n int) string {
	cs := f.State().Issues[n-1].Comments
	return cs[len(cs)-1].Body
}

// adoptedKeptT3 fails t unless the adopted T3's child, #4, is as
// adoptedStory left it: open at needs-human, with its origin, item T3 of
// revision 1 with human=1, read from the App's comment that adopted it,
// and no write in the pass f's writes are from.
func adoptedKeptT3(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	droppedNoWrite(t, f, 4, "the adopted T3's child, kept")
	if t3 := f.State().Issues[3]; t3.Closed || !slices.Equal(t3.Labels, []string{"stage:needs-human", "type:task"}) {
		t.Errorf("T3's child is closed %t, labelled %v; want it open at needs-human", t3.Closed, t3.Labels)
	}
	o := adoptedOrigin(t, f, 4)
	if o.Comment == state.BodyID {
		t.Errorf("T3's child's origin %+v is read from its body; want it from the App's comment", o)
	}
	o.Comment = 0
	if want := (state.Origin{Plan: 1, Rev: 1, Item: "T3", Human: true}); o != want {
		t.Errorf("T3's child's origin is %+v; want %+v", o, want)
	}
}

// A revision that keeps the adopted T3 writes nothing to its child, which
// stays open at needs-human with the origin of the App's comment that
// adopted it, naming revision 1, and the record lists it among the
// children, while the adopted T2 is changed as ever. A crash at any
// write, before or after it applies, writes nothing twice.
func TestAdoptedChildKept(t *testing.T) {
	t.Parallel()
	setup := func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		adoptedApprovedWith(tb, f, false, nil, changedPlanData(tb, adoptedKeptRevision(tb)))
		return f
	}
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	adoptedKeptT3(t, f)
	if o := adoptedOrigin(t, f, 3); o.Rev != 2 || o.Item != "T2" {
		t.Errorf("T2's child's origin is %+v; want item T2, revision 2", o)
	}
	wantRec := "<!-- issuedog:created rev=2 children=2,3,4,6 -->"
	if recs := droppedRecords(f, 1); len(recs) != 1 || !strings.HasSuffix(recs[0], "\n- T3 #4: Set the quote lifetime in production\n- T4 #6: Give the quote lifetime a default\n\n"+wantRec) {
		t.Errorf("issue 1's records are %q, want one listing the kept T3's #4, ending\n%q", recs, wantRec)
	}
}

// adoptedAgainSetup is adoptedKeptRevision approved as plan revision 2
// and applied by a pass, then the story brought back to planning by the
// owner, and adoptedAgainRevision posted as plan revision 3 at
// plan-review, with the owner's 👍 on it, and the writes forgotten. t is
// the test's, for the pass's client.
func adoptedAgainSetup(t *testing.T) func(testing.TB) *githubtest.Fake {
	return func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		r := adoptedApprovedWith(tb, f, false, nil, changedPlanData(tb, adoptedKeptRevision(tb)))
		if err := childrenPass(t, revisionPlan)(f); err != nil {
			tb.Fatal(err)
		}
		f.AddComment(r.n, app, "Moved.\n\n<!-- issuedog:transition from=planned to=planning event=32 by=owner -->")
		f.RemoveLabel(r.n, app, "stage:planned")
		f.AddLabel(r.n, app, "stage:planning")
		rev := revMove(tb, f, r.n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p3"},
			PlanRev: 3, Text: planText(r.n, 3), Markers: []github.Marker{changedPlanData(tb, adoptedAgainRevision(tb))}})
		f.AddReaction(rev, "owner", "+1")
		f.ForgetWrites()
		return f
	}
}

// A second revision that changes the adopted T2 again rewrites its child
// once more, though its body is the App's rewrite from revision 2 already:
// title and whole body from revision 3, the rewrite marker naming run p3
// and the origin revision 3, and its move back to ready carries that
// origin, so Derive reads revision 3 from it. The kept T3's child gets no
// write, and the record lists it. A crash at any write, before or after
// it applies, writes nothing twice.
func TestAdoptedChildChangedAgain(t *testing.T) {
	t.Parallel()
	setup := adoptedAgainSetup(t)
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	p, plan := adoptedApprovalOf(t, f, 3)
	t2 := f.State().Issues[2]
	originMarker := "<!-- issuedog:origin plan=#1 rev=3 item=T2 -->"
	if t2.Title != adoptedAgainTitle || t2.BodyEditedBy != app || t2.Author != "owner" ||
		!strings.HasPrefix(t2.Body, "<!-- issuedog:rewrite run=p3 -->\n**Parent:** #1   **Scope:** scope:booking   **Priority:** p1\n\n## Context\n\n") ||
		!strings.HasSuffix(t2.Body, "\n\n"+originMarker) || !strings.Contains(t2.Body, "\n- "+adoptedAgainCriterion+"\n") {
		t.Errorf("T2's child is %q, by %q, last edited by %q, with body\n%s\nwant revision 3's T2, rewritten whole by the App",
			t2.Title, t2.Author, t2.BodyEditedBy, t2.Body)
	}
	want := fmt.Sprintf("Rewritten from #1's plan, rev 3, as T2, since [the plan](https://github.com/o/r/issues/1#issuecomment-%d) changed it. "+
		"%s It goes back to `stage:ready`.\n\n<!-- issuedog:transition from=ready to=ready event=p%d -->\n%s", plan, adoptedMovedText, p, originMarker)
	if got := adoptedNewest(f, 3); got != want {
		t.Errorf("T2's child's newest comment is\n%q\nwant\n%q", got, want)
	}
	if o := adoptedOrigin(t, f, 3); o.Plan != 1 || o.Rev != 3 || o.Item != "T2" || o.Comment == state.BodyID {
		t.Errorf("T2's child's origin is %+v; want item T2 of #1's plan, revision 3, from its move's comment", o)
	}
	adoptedKeptT3(t, f)
	if recs := droppedRecords(f, 1); len(recs) != 2 || !strings.HasSuffix(recs[1], "<!-- issuedog:created rev=3 children=2,3,4,6 -->") {
		t.Errorf("issue 1's records are %q, want revision 2's, then revision 3's, listing #2, #3, #4 and #6", recs)
	}
}

// adoptedChildStep is a pass that reads issue n and runs step 1 on it, as
// Iterate's visit does. t is the test's, for the client.
func adoptedChildStep(t *testing.T, n int) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		st := childrenStep(t, f, revisionPlan)
		s, err := st.Client.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			return err
		}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// adoptedChangedSetup is adoptedApproved with its revision applied to the
// children by a pass, then edit run on f, and the writes forgotten. t is
// the test's, for the pass's client.
func adoptedChangedSetup(t *testing.T, edit func(testing.TB, *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		adoptedApproved(tb, f, false, nil)
		if err := childrenPass(t, revisionPlan)(f); err != nil {
			tb.Fatal(err)
		}
		edit(tb, f)
		f.ForgetWrites()
		return f
	}
}

// Once the owner edits the comment of the adopted T2's move, which carried
// its origin naming revision 2, that comment is dirty, so Derive would
// fall back to the App's comment that adopted it, naming revision 1. Step
// 1 on the child posts the marker of its rewritten body again, in a new
// App comment, so it reads revision 2. Untouched, the move's comment
// gives it, and step 1 posts nothing. A crash at any write, before or
// after it applies, writes nothing twice.
func TestAdoptedRepostEditedMove(t *testing.T) {
	t.Parallel()
	originMarker := "<!-- issuedog:origin plan=#1 rev=2 item=T2 -->"
	t.Run("the move's comment edited", func(t *testing.T) {
		t.Parallel()
		setup := adoptedChangedSetup(t, func(tb testing.TB, f *githubtest.Fake) {
			s, err := f.Client(tb).Snapshot(context.Background(), "o", "r", 3)
			if err != nil {
				tb.Fatal(err)
			}
			move := s.Comments[len(s.Comments)-1]
			if !strings.HasSuffix(move.Body, originMarker) {
				tb.Fatalf("T2's child's newest comment is %q; want its move, carrying its origin", move.Body)
			}
			f.EditComment(move.ID, "owner", move.Body)
		})
		before := setup(t)
		if o := adoptedOrigin(t, before, 3); o.Rev != 1 {
			t.Errorf("before step 1, T2's child's origin is %+v; want revision 1's, from the comment that adopted it", o)
		}
		had := len(before.State().Issues[2].Comments)
		githubtest.Replay(t, setup, adoptedChildStep(t, 3))
		f := run(t, setup, adoptedChildStep(t, 3))
		if added := originAdded(f, 3, had); !slices.Equal(added, []string{adoptedRepostLine + "\n\n" + originMarker}) {
			t.Errorf("T2's child's new comments are %q; want only the repost of %s", added, originMarker)
		}
		if o := adoptedOrigin(t, f, 3); o.Plan != 1 || o.Rev != 2 || o.Item != "T2" || o.Comment == state.BodyID {
			t.Errorf("T2's child's origin is %+v; want item T2 of #1's plan, revision 2, from the repost", o)
		}
	})
	t.Run("untouched", func(t *testing.T) {
		t.Parallel()
		f := adoptedChangedSetup(t, func(testing.TB, *githubtest.Fake) {})(t)
		if err := adoptedChildStep(t, 3)(f); err != nil {
			t.Fatal(err)
		}
		if w := f.Writes(); len(w) != 0 {
			t.Errorf("step 1 on T2's child wrote %+v; want nothing", w)
		}
	})
}

// Should step 1 on the adopted T2's child come between its rewrite and its
// move, as after a crash cut the revision's pass short at the move's
// comment, Derive reads revision 1 from the App's comment that adopted it,
// so step 1 posts the rewritten body's marker, naming revision 2. The
// story's next pass then moves the child with no marker of its own, and
// no word on why it carries one, as the repost gives revision 2 already.
// A crash at any write, before or after it applies, writes nothing twice.
func TestAdoptedRepostBeforeMove(t *testing.T) {
	t.Parallel()
	// at is the write of the move's comment on T2's child, #3, in the
	// revision's pass.
	clean := adoptedSetup(false, nil)(t)
	if err := childrenPass(t, revisionPlan)(clean); err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(clean.Writes(), func(w githubtest.Write) bool { return w.Method == "POST" && w.Path == "/issues/3/comments" }) + 1
	if at == 0 {
		t.Fatalf("the revision's pass wrote %+v; want a comment on #3", clean.Writes())
	}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := adoptedSetup(false, nil)(tb)
		f.FailWrite(at)
		if err := childrenPass(t, revisionPlan)(f); err == nil {
			tb.Fatal("the revision's pass, failing at the move's comment, returned no error")
		}
		f.FailWrite(0)
		f.ForgetWrites()
		return f
	}
	before := setup(t)
	if t2 := before.State().Issues[2]; !strings.HasPrefix(t2.Body, "<!-- issuedog:rewrite run=p2 -->\n") {
		t.Fatalf("T2's child's body is\n%s\nwant the App's rewrite", t2.Body)
	}
	had := len(before.State().Issues[2].Comments)
	pass := func(f *githubtest.Fake) error {
		if err := adoptedChildStep(t, 3)(f); err != nil {
			return err
		}
		return childrenPass(t, revisionPlan)(f)
	}
	githubtest.Replay(t, setup, pass)
	f := run(t, setup, pass)
	p, plan := changedApproval(t, f)
	originMarker := "<!-- issuedog:origin plan=#1 rev=2 item=T2 -->"
	want := []string{adoptedRepostLine + "\n\n" + originMarker,
		changedMoveText("T2", plan, "It goes back to `stage:ready`.") + fmt.Sprintf("\n\n<!-- issuedog:transition from=ready to=ready event=p%d -->", p)}
	if added := originAdded(f, 3, had); !slices.Equal(added, want) {
		t.Errorf("T2's child's new comments are\n%q\nwant\n%q", added, want)
	}
	if o := adoptedOrigin(t, f, 3); o.Plan != 1 || o.Rev != 2 || o.Item != "T2" || o.Comment == state.BodyID {
		t.Errorf("T2's child's origin is %+v; want item T2 of #1's plan, revision 2, from the repost", o)
	}
	if updates := slices.DeleteFunc(f.Writes(), func(w githubtest.Write) bool { return w.Method != "PATCH" || w.Path != "/issues/3" }); len(updates) != 0 {
		t.Errorf("T2's child got the edits %+v; want none, as its rewrite stands", updates)
	}
}

// A sub-issue of the story in another repo, numbered as an issue owner
// filed here that isn't linked and whose clean App comment names item T7
// of the story's plan, isn't read as that issue: the planner's context
// lists no child of T7, so the issue here isn't taken for one, as an
// issue a person filed is a child only once it's a sub-issue (SPEC §5.2).
func TestAdoptedOtherRepoSubIssue(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	var here int
	r := adoptedStory(t, f, false, func(f *githubtest.Fake, r adoptedIDs) {
		here = f.AddIssue("owner", "A task filed here, never linked", "", "type:task")
		f.AddComment(here, app, adoptedComment("T7", ""))
		f.LinkSubIssue(r.n, "o/other", here)
	})
	d := ivDaemon(t, f, newOrigin(t))
	files := planContext(t, d, r.n)
	want := fmt.Sprintf("## Children of plan revision 1\n\n- T1: #%d, Record when each quote expires (type task, closed as completed)\n"+
		"- T2: #%d, Refuse an expired quote at booking (type task, `stage:ready`)\n"+
		"- T3: #%d, Set the quote lifetime in production (type task, `stage:needs-human`)\n\n", r.t1, r.t2, r.t3)
	if !strings.Contains(files["children.md"], want) {
		t.Errorf("children.md lacks\n%s\nin\n%s", want, files["children.md"])
	}
	if strings.Contains(files["children.md"], "- T7: ") {
		t.Errorf("children.md lists #%d, here, as T7's child, from the sub-issue of that number in another repo:\n%s", here, files["children.md"])
	}
}
