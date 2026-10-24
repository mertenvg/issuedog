package daemon_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// What #462 left in creating a plan's children (#544, from #462's
// verify): a revision's changed story whose child has no parent, and a
// blocked-by link GitHub refuses with a 422, are each a failure no later
// pass gets past, reported once on the parent (SPEC §5.2, §5.3).

// orphanEpicSetup is changedEpic with S1's child, issue 2, filed but
// never linked under the epic, and so never moved, as a pass that stopped
// between its create and its link leaves it: a story with no parent. S2's
// child, issue 3, is linked, at planning, with its first move. Revision 2
// changes S1 and keeps S2, and the owner's 👍 approves it.
func orphanEpicSetup(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	n, _ := plFiled(f, "epic", false, plEntry)
	revMove(t, f, n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"},
		PlanRev: 1, Text: planText(n, 1), Markers: []github.Marker{planData(t, twoStories)}})
	cs := f.State().Issues[n-1].Comments
	ms := github.ParseMarkers(cs[len(cs)-1].Body)
	approve := f.AddComment(n, app, fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r9 rev=1 body=%s -->",
		ms[len(ms)-1].Fields["body"]))
	f.AddIssue(app, "S1. Quotes expire after 24 hours", fmt.Sprintf("The story.\n\n<!-- issuedog:origin plan=#%d rev=1 item=S1 -->", n),
		"type:story", "priority:p2", "stage:planning")
	s2 := f.AddIssue(app, "S2. Customers see when a quote expires", fmt.Sprintf("The story.\n\n<!-- issuedog:origin plan=#%d rev=1 item=S2 -->", n),
		"type:story", "priority:p2", "stage:planning")
	f.SetParent(s2, n)
	f.AddComment(s2, app, fmt.Sprintf("Created.\n\n<!-- issuedog:transition from=intake to=planning event=p%d -->", approve))
	f.AddComment(n, app, "Moved.\n\n<!-- issuedog:transition "+revByOwner+" -->")
	f.RemoveLabel(n, app, "stage:plan-review")
	f.AddLabel(n, app, "stage:planning")
	c := revMove(t, f, n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p2"},
		PlanRev: 2, Text: planText(n, 2), Markers: []github.Marker{changedPlanData(t, changedEpicRevision(t))}})
	f.AddReaction(c, "owner", "+1")
	f.ForgetWrites()
	return f
}

// A revision that changes a story whose child has no parent, as one filed
// but never linked under the epic, can't send it back to planning: the
// transition table has no such row for a story with no parent (SPEC §7.2).
// It's a failure no later pass gets past, so a state error on the epic
// says so, once, and the child gets no write, not even a transition error
// of its own. The visit returns no error, so the epic isn't read again for
// it, and a visit after it makes no request, rather than failing and
// reading the children again on every pass. A crash at any write replays.
func TestChildrenChangedStoryOrphan(t *testing.T) {
	t.Parallel()
	pass := childrenPass(t, twoStories)
	githubtest.Replay(t, orphanEpicSetup, pass)

	f := run(t, orphanEpicSetup, pass)
	droppedNoWrite(t, f, 2, "S1's child, which has no parent")
	droppedNoWrite(t, f, 3, "S2's child, which the revision keeps")
	for _, c := range f.State().Issues[1].Comments {
		if strings.Contains(c.Body, "issuedog:") {
			t.Errorf("S1's child has the App's comment %q; want none", c.Body)
		}
	}
	creationRestStuck(t, f, twoStories, 2, goneMoveID(t, f),
		"#2, the child of item S1, is a story with no parent, which no transition sends back to `planning` until it's linked under this issue")
}

// orphanEpicHeldSetup is orphanEpicSetup with S1's child held by the
// owner's /hold on it.
func orphanEpicHeldSetup(t testing.TB) *githubtest.Fake {
	f := orphanEpicSetup(t)
	hold := f.AddComment(2, "owner", "/hold")
	f.AddComment(2, app, fmt.Sprintf("On hold.\n\n<!-- issuedog:transition from=intake to=held at=intake reason=owner cmd=%d -->", hold))
	f.AddReaction(hold, app, "+1")
	f.RemoveLabel(2, app, "stage:planning")
	f.AddLabel(2, app, "stage:held")
	f.ForgetWrites()
	return f
}

// A held story with no parent is no such failure: its change's move is
// held → held, which the table has for any class, so it's rewritten, and
// stays held with planning as its hold's at and origin. A crash at any
// write replays.
func TestChildrenChangedStoryOrphanHeld(t *testing.T) {
	t.Parallel()
	pass := childrenPass(t, twoStories)
	githubtest.Replay(t, orphanEpicHeldSetup, pass)

	f := run(t, orphanEpicHeldSetup, pass)
	if got := childrenCrashErrors(f.State().Issues[0]); len(got) != 0 {
		t.Errorf("issue 1's state errors are %q; want none", got)
	}
	p, _ := changedApproval(t, f)
	if m := changedLastMarker(t, f, 2); m.Fields["from"] != "held" || m.Fields["to"] != "held" || m.Fields["at"] != "planning" ||
		m.Fields["event"] != fmt.Sprintf("p%d", p) {
		t.Errorf("S1's child's newest marker has %v, want held → held at planning, keyed by the approval", m.Fields)
	}
	if got := records(f.State().Issues[0]); len(got) != 1 {
		t.Errorf("issue 1's records are %q, want one", got)
	}
}

// A plan comment that names no run is no failure for a plan with no
// changed item, whose children no rewrite names a run for: here a first
// plan's, whose move to plan-review is keyed by an event, gets its
// children as any other does. A crash at any write replays.
func TestChildrenNoRunUnchanged(t *testing.T) {
	t.Parallel()
	setup := atPlanning(threeTasks, func(t testing.TB, f *githubtest.Fake) {
		moveOn(t, f, 1, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "event", ID: "77"}, PlanRev: 1,
			Text: planText(1, 1), Markers: []github.Marker{planData(t, threeTasks)}}, 1002)
		thumbsUp(t, f)
	})
	pass := childrenPass(t, threeTasks)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	if got := childrenCrashErrors(f.State().Issues[0]); len(got) != 0 {
		t.Errorf("issue 1's state errors are %q; want none", got)
	}
	checkChildren(t, f, threeTasks)
}

// blockCapSetup is the setup of a pass that finds the story's children
// filed, linked and moved, as a crash at T2's first blocked-by link left
// them, with 50 issues filed by hand blocking T2's child, #3, GitHub's
// cap, so it refuses T2's links with a 422.
func blockCapSetup(t *testing.T) func(testing.TB) *githubtest.Fake {
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	block := writeAt(t, first, pass, 1, isBlock)
	return func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, block)
		for i := range 50 {
			f.SetBlockedBy(3, f.AddIssue("owner", fmt.Sprintf("Blocker %d", i+1), ""))
		}
		f.ForgetWrites()
		return f
	}
}

// A blocked-by link GitHub refuses with a 422, here as 50 issues block
// T2's child already, is a failure no later pass gets past, as a refused
// sub-issue link is: a state error on the story says so, in the daemon's
// words, quoting none of GitHub's, the visit returns no error, and a
// visit after it makes no request, rather than retrying the link on every
// pass. No record is posted. A crash at the error's post replays.
func TestChildrenBlockRefused(t *testing.T) {
	t.Parallel()
	setup := blockCapSetup(t)
	pass := childrenPass(t, threeTasks)
	githubtest.Replay(t, setup, pass)

	f := run(t, setup, pass)
	if got := f.State().Issues[2].BlockedBy; len(got) != 50 {
		t.Errorf("T2's child is blocked by %d issues, want the 50 filed by hand", len(got))
	}
	p, _ := planMoveID(t, f)
	creationRestStuck(t, f, threeTasks, 1, p, "GitHub refused to make #3, the child of item T2, blocked by #2, the child of item T1")
}

// blockCapChangedSetup is changedStory with 50 issues filed by hand
// blocking T2's child, GitHub's cap, so it refuses the links the changed
// T2's revision adds with a 422.
func blockCapChangedSetup(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	changedStory(t, f, false, func(f *githubtest.Fake, r changedIDs) {
		for i := range 50 {
			f.SetBlockedBy(r.t2, f.AddIssue("owner", fmt.Sprintf("Blocker %d", i+1), ""))
		}
	})
	return f
}

// So is a link a changed child's revision adds that GitHub refuses with a
// 422: here the changed T2's child, which 50 issues block already, gets
// none of its new links, and a state error on the story says so, once.
// No record is posted. A crash at any write replays.
func TestChildrenRevisedBlockRefused(t *testing.T) {
	t.Parallel()
	pass := childrenPass(t, revisionPlan)
	githubtest.Replay(t, blockCapChangedSetup, pass)

	f := run(t, blockCapChangedSetup, pass)
	t2 := f.State().Issues[2]
	if got := t2.BlockedBy; len(got) != 50 {
		t.Errorf("T2's child is blocked by %v, want the 50 filed by hand alone", got)
	}
	why := fmt.Sprintf("GitHub refused to make #%d, the child of item T2, blocked by #%d, the child of item T1", t2.Number, f.State().Issues[1].Number)
	creationRestStuck(t, f, revisionPlan, 2, goneMoveID(t, f), why)
}
