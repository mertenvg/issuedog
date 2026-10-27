package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A child whose body someone edited to hold a whole origin marker naming
// another item, before its step 1 marks it dirty, when its parent is
// visited first in the same pass (#501, from #347's verify): the parent's
// first pass filed, linked and moved T1, #2, then stopped at T2's create,
// and the edit came before the next pass. Matched by the listed body, #2
// would be T2's child, so T1 would be filed a second time and T2 never.
// The parent's visit takes #2's origin from the versions of its body the
// App wrote, so #2 is still T1's child, and T2 and T3 are filed, each
// once. #2's step 1 then reposts T1's origin, as its body's markers aren't
// read. The pass reads #2's history once: the parent's visit reads it, and
// #2's dirty check and its repost use that one, past the App's dirty note
// for mallory's edit. A second pass writes nothing. A crash at any write
// replays.
func TestChildForgedOrigin(t *testing.T) {
	t.Parallel()
	const forged = "Record when each quote expires, put better.\n\n"
	for name, edit := range map[string]struct{ login, marker string }{
		"mallory's marker for T2":           {"mallory", "<!-- issuedog:origin plan=#1 rev=1 item=T2 -->"},
		"the owner's marker for T2":         {"owner", "<!-- issuedog:origin plan=#1 rev=1 item=T2 -->"},
		"mallory's marker for another plan": {"mallory", "<!-- issuedog:origin plan=#9 rev=1 item=T1 -->"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			setup := childOriginLost(t, func(_ testing.TB, f *githubtest.Fake) {
				f.EditBody(2, edit.login, forged+edit.marker)
			})
			pass := childOriginPass(t)
			githubtest.Replay(t, setup, pass)

			f := setup(t)
			before := f.HistoryQueries()
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			if q := f.HistoryQueries() - before; q != 1 {
				t.Errorf("the pass sent %d history queries; want 1, for #2's edit", q)
			}
			childOriginChildren(t, f)
			if !slices.ContainsFunc(originAdded(f, 2, 0), func(c string) bool {
				return strings.HasPrefix(c, originRepost("<!-- issuedog:origin plan=#1 rev=1 item=T1 "))
			}) {
				t.Errorf("#2's comments %q; want the repost of its origin, item T1", originAdded(f, 2, 0))
			}
			if o := originDerived(t, f, 2).Origin; o.Plan != 1 || o.Item != "T1" || o.Comment == state.BodyID {
				t.Errorf("#2's origin = %+v; want item T1 of #1's plan, from the repost", o)
			}
			writes := len(f.Writes())
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			if w := f.Writes(); len(w) != writes {
				t.Errorf("a second pass wrote %+v, want nothing", w[writes:])
			}
			noSearch(t, f)
		})
	}
}

// Mallory's edit removes T1's marker from #2's body, so Dirty reads no
// history for it, and the parent's visit, first in the pass, reads #2's
// history to find it as T1 (childOrigins). #2's step 1 then notes the
// edit dirty and reposts T1's origin, from that same history, kept past
// the App's note: the pass reads it once, not once for the visit and once
// for the repost (#501). A crash at any write replays.
func TestChildOriginLostUntrustedOneRead(t *testing.T) {
	t.Parallel()
	setup := childOriginLost(t, func(_ testing.TB, f *githubtest.Fake) {
		f.EditBody(2, "mallory", "Record when each quote expires, put better.")
	})
	pass := childOriginPass(t)
	githubtest.Replay(t, setup, pass)

	f := setup(t)
	before := f.HistoryQueries()
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if q := f.HistoryQueries() - before; q != 1 {
		t.Errorf("the pass sent %d history queries; want 1, for #2's edit", q)
	}
	childOriginChildren(t, f)
	if is := f.State().Issues[1]; !strings.HasPrefix(is.Body, "<!-- issuedog:dirty by=mallory -->") {
		t.Errorf("#2's body is %q; want mallory's edit noted dirty", is.Body)
	}
	if !slices.ContainsFunc(originAdded(f, 2, 0), func(c string) bool {
		return strings.HasPrefix(c, originRepost("<!-- issuedog:origin plan=#1 rev=1 item=T1 "))
	}) {
		t.Errorf("#2's comments %q; want the repost of its origin, item T1", originAdded(f, 2, 0))
	}
}

// Through the loop, as the verifier ran it, with the owner's edit: as in
// TestChildOriginLostRun, the epic's first pass dies just after S1's
// first move, before S2's create, and mertenvg then edits S1's body, #3,
// to hold a whole marker for S2. A restart visits #1 before #3, and the
// epic's visit finds #3 as S1 by the App's original post, so it files S2,
// #4, alone: no second issue for S1, and S2 isn't left unfiled. #3's step 1,
// later in the same pass, reposts S1's origin, and the pass reads #3's
// history once, and the digest's once, for its identity marker (SPEC
// §5.1). Every unchanged pass after costs no rate limit.
func TestChildForgedOriginRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(childrenRunOwner, "admin")
	epic := childrenRunAtPlanning(f, "type:epic")
	l := newChildrenRunLoop(t, f, "labels")
	l.outs(childrenRunEpicPlan(t))
	l.step("the epic's plan", 1, "POST /issues", "POST /graphql", "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	plan := l.planned("the epic's plan", epic, 1)
	l.step("the epic's plan listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")

	f.AddReaction(plan.ID, childrenRunOwner, "+1")
	l.crash("the approval, crashing after S1's first move", 6, 1,
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		"POST /issues", "POST /issues/1/sub_issues", "POST /issues/3/comments")
	approvedBy := l.comments(epic)[len(l.comments(epic))-1]
	if st := f.State(); len(st.Issues) != 3 || st.Issues[2].Parent != epic || len(st.Issues[2].Comments) != 1 {
		t.Fatalf("the approval: %d issues, #3 under #%d with comments %+v; want S1 alone, linked and moved", len(st.Issues), st.Issues[2].Parent, st.Issues[2].Comments)
	}

	// S1 isn't written to in the restart's pass, so the planner runs on it
	// there, and on S2 in the pass after.
	f.EditBody(3, childrenRunOwner, "Quotes expire after 24 hours, put better.\n\n<!-- issuedog:origin plan=#1 rev=1 item=S2 -->")
	storyPlan := childrenRunStoryPlan(t)
	l.outs(storyPlan, storyPlan)
	l.start()
	histories := f.HistoryQueries()
	l.step("the restart after the edit", 2, slices.Concat(childWrites(epic, 4), blockWrites(4),
		[]string{recordWrite(epic), "POST /issues/3/comments", "PATCH /issues/2", "DELETE /issues/1/assignees",
			"POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:planning"})...)
	if q := f.HistoryQueries() - histories; q != 2 {
		t.Errorf("the restart after the edit sent %d history queries; want 2, for #3's edit and the digest's identity marker", q)
	}
	if got := len(f.State().Issues); got != 4 {
		t.Fatalf("the restart after the edit left %d issues; want 4: the epic, the digest, S1 and S2", got)
	}
	stories := []wantChild{
		{"S1", "S1. Quotes expire after 24 hours", "", []string{"effort:high", "model:opus", "priority:p1", "stage:planning", "type:story"}, state.Planning},
		{"S2", "S2. Customers see when a quote expires", "", []string{"priority:p2", "stage:planning", "type:story"}, state.Planning},
	}
	runRecord(t, f, "the restart after the edit", epic, 1, 3, stories, map[int][]int{4: {3}})
	if got, want := childrenCrashMoves(f.State().Issues[2]), []string{firstMoveUnder(epic, 1, stories[0], approvedBy.ID)}; !slices.Equal(got, want) {
		t.Errorf("S1's first moves are\n%q\nwant\n%q", got, want)
	}
	if added := l.added(3); len(added) != 2 || !strings.HasPrefix(added[0], originRepost("<!-- issuedog:origin plan=#1 rev=1 item=S1 ")) {
		t.Errorf("#3's new comments %q; want the repost of its origin, item S1, then its plan", added)
	}
	l.planned("the restart after the edit", 3, 2)
	l.step("S2's plan", 3, "PATCH /issues/2", "POST /issues/3/assignees",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:planning")
	l.planned("S2's plan", 4, 3)
	l.step("S2's plan listed", 3, "PATCH /issues/2", "POST /issues/4/assignees")
	if q := f.HistoryQueries() - histories; q != 2 {
		t.Errorf("the passes after the edit sent %d history queries; want 2, for #3's edit and the digest's identity marker", q)
	}
	l.quiet("the stories' plans")
}

// A revision's planner finds the approved plan's children as creating them
// does (planPrior.read), so a marker mallory pasted into T2's child's
// body, naming T3, doesn't make it T3's child: children.md still lists it
// as T2, from the App's original post, and T3 as the unlinked issue filed
// for it (#501).
func TestPlannerContextRevisionForged(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	r := revStory(t, f, revByOwner)
	f.EditBody(r.t2, "mallory", "The task.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T3 -->")
	d := ivDaemon(t, f, newOrigin(t))
	children := planContext(t, d, r.n)["children.md"]
	for _, want := range []string{
		fmt.Sprintf("- T2: #%d, Refuse an expired quote at booking (type task, `stage:ready`)\n", r.t2),
		fmt.Sprintf("- T3: #%d, Set the quote lifetime in production (type task, `stage:needs-human`)\n", r.t3),
	} {
		if !strings.Contains(children, want) {
			t.Errorf("children.md lacks\n%s\nin\n%s", want, children)
		}
	}
}
