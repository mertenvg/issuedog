package daemon_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A child whose origin marker an edit just removed, before its step 1 has
// reposted it, when its parent is visited first in the same pass, as
// visits go in issue-number order (#347, from #291's verify): the parent's
// first pass filed, linked and moved T1, #2, then stopped at T2's create,
// and the edit came before the next pass.

// childOriginLost is issue 1 with threeTasks' plan approved, whose first
// pass stopped at T2's create, so T1, #2, is filed, linked and moved,
// then edit, run on #2, then the writes forgotten.
func childOriginLost(t *testing.T, edit func(tb testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	first := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	create := writeAt(t, first, pass, 2, isCreate)
	return func(tb testing.TB) *githubtest.Fake {
		f := childrenCrashStopped(tb, first, pass, create)
		if got := len(f.State().Issues); got != 2 {
			tb.Fatalf("the first pass left %d issues, want the parent and T1", got)
		}
		edit(tb, f)
		f.ForgetWrites()
		return f
	}
}

// childOriginPass is one pass in issue-number order, from a fresh client,
// as after a restart: step 1 and ChildrenVisit on the parent, issue 1,
// then step 1 on T1's child, #2, with the same step, so the two share the
// histories it keeps, as the loop's visits do.
func childOriginPass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		st := childrenStep(t, f, threeTasks)
		ctx := context.Background()
		parent, err := st.Client.Snapshot(ctx, "o", "r", 1)
		if err != nil {
			return err
		}
		if _, err := st.Run(ctx, parent); err != nil {
			return err
		}
		if err := st.ChildrenVisit(ctx, parent); err != nil {
			return err
		}
		child, err := st.Client.Snapshot(ctx, "o", "r", 2)
		if err != nil {
			return err
		}
		_, err = st.Run(ctx, child)
		return err
	}
}

// childOriginChildren fails t unless f has exactly threeTasks' children,
// T1 the #2 the first pass filed, each with its own title, linked under
// issue 1 in plan order, each with one first move, and issue 1 has one
// record of them.
func childOriginChildren(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	st := f.State()
	want := wantChildren(threeTasks)
	if got := len(st.Issues) - 1; got != len(want) {
		t.Fatalf("%d children filed, want %d, T1 not filed twice: %+v", got, len(want), st.Issues)
	}
	if got := st.Issues[0].SubIssues; !reflect.DeepEqual(got, []int{2, 3, 4}) {
		t.Errorf("issue 1's sub-issues are %v, want [2 3 4]", got)
	}
	p, _ := planMoveID(t, f)
	for i, w := range want {
		is := st.Issues[i+1]
		if is.Title != w.title {
			t.Errorf("#%d is titled %q, want %s's %q", is.Number, is.Title, w.item, w.title)
		}
		if got, want := childrenCrashMoves(is), []string{firstMove(w, p)}; !reflect.DeepEqual(got, want) {
			t.Errorf("#%d's first moves are\n%q\nwant\n%q", is.Number, got, want)
		}
	}
	if got, want := records(st.Issues[0]), []string{wantRecord(threeTasks)}; !reflect.DeepEqual(got, want) {
		t.Errorf("issue 1's records are\n%q\nwant\n%q", got, want)
	}
}

// The owner's edit removes T1's marker, and the parent's visit, first in
// the pass, finds #2 as T1 from the App's original post, so it files T2
// and T3 but no second T1. Then #2's step 1 reposts its origin. The pass
// reads #2's history once: Dirty reads it for the owner's edit, and the
// visit and the repost use that one. A crash at any write replays.
func TestChildOriginLostParentFirst(t *testing.T) {
	t.Parallel()
	setup := childOriginLost(t, func(_ testing.TB, f *githubtest.Fake) {
		f.EditBody(2, "owner", "Record when each quote expires, put better.")
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
}

// An untrusted edit's marker counts for nothing: neither mallory's in the
// body now, which #2's step 1 noted as dirty, escaped, before a crash cut
// it short of the repost, nor one in an earlier edit that the owner's
// edit then removed. The parent's visit takes #2's origin only from a
// version of its body the App wrote, and the App's note escapes the text
// it keeps, so #2 is still T1, from the original post, not the T2
// mallory's marker names, and T2 and T3 are filed, each once. A crash at
// any write replays.
func TestChildOriginLostHostileEdit(t *testing.T) {
	t.Parallel()
	const hijack = "Refuse an expired quote at booking.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T2 -->"
	for name, edit := range map[string]func(tb testing.TB, f *githubtest.Fake){
		"mallory's marker in the body, noted dirty": func(tb testing.TB, f *githubtest.Fake) {
			f.EditBody(2, "mallory", hijack)
			f.ForgetWrites()
			f.FailWrite(2) // the repost, after the dirty note
			tr := trusted{"owner": true}
			child := &daemon.Step{Applier: &daemon.Applier{Client: f.Client(tb), Owner: "o", Repo: "r", AppSlug: slug, IssueTypes: true,
				Notify: []string{"owner"}}, Trust: tr, Commands: daemon.Commands(tr)}
			s, err := child.Client.Snapshot(context.Background(), "o", "r", 2)
			if err != nil {
				tb.Fatal(err)
			}
			if _, err := child.Run(context.Background(), s); err == nil {
				tb.Fatal("#2's step 1 made every write; want it stopped at the repost")
			}
			f.FailWrite(0)
			if is := f.State().Issues[1]; !strings.HasPrefix(is.Body, "<!-- issuedog:dirty by=mallory -->") || len(is.Comments) != 1 {
				tb.Fatalf("#2 has body %q and comments %+v; want mallory's edit noted dirty, and only its first move", is.Body, is.Comments)
			}
		},
		"mallory's marker in an earlier edit": func(_ testing.TB, f *githubtest.Fake) {
			f.EditBody(2, "mallory", hijack)
			f.EditBody(2, "owner", "Record when each quote expires, put better.")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			setup := childOriginLost(t, edit)
			pass := childrenPass(t, threeTasks)
			githubtest.Replay(t, setup, pass)
			childOriginChildren(t, run(t, setup, pass))
		})
	}
}

// Through the loop, as the verifier ran it: mertenvg's 👍 approves the
// epic's plan, #1, and the pass files S1, #3, links and moves it, then
// dies just after S1's first move, before S2's create. mertenvg removes
// S1's marker from its body, and a restart visits #1 before #3, so the
// epic's visit finds S1 by its body's history: it files S2, #4, alone,
// not a fifth issue for S1. #3's step 1, later in the same pass, reposts
// S1's origin, and the pass reads #3's history once, and the digest's
// once, for its identity marker (SPEC §5.1). Every unchanged pass after
// costs no rate limit.
func TestChildOriginLostRun(t *testing.T) {
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
	f.EditBody(3, childrenRunOwner, "Quotes expire after 24 hours, put better.")
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
