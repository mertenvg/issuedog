package daemon_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Revised plans (#195, SPEC §11.3), through the harness with the real Planner:
// an issue planned before gets a planner run again once it's back at
// planning, its context has the children of its approved plan, and its
// revision is checked against them and waits at plan-review.

// revIDs are the issues and comments revStory files.
type revIDs struct {
	n             int   // the story
	plan, approve int64 // plan revision 1's comment, and its approval
	t1, t2, t3    int   // T1's to T3's children
	other         int   // a sub-issue filed by hand
	back          int64 // the move back to planning
}

// revByOwner brings revStory back to planning: the owner setting
// stage:planning by hand. TestPlannerRevisionAfterPlanOutcome brings it
// back by the interviewer's plan outcome (#294) instead.
const (
	revByOwner    = "from=planned to=planning event=31 by=owner"
	revPlanReview = false // plan.review, which a revision doesn't follow
)

// revMove applies m to issue n from a fresh client, as a run's move would,
// and returns its comment's ID.
func revMove(t testing.TB, f *githubtest.Fake, n int, m daemon.Move) int64 {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	a := &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}
	if err := a.Transition(context.Background(), s, clean, m); err != nil {
		t.Fatal(err)
	}
	return s.Comments[len(s.Comments)-1].ID
}

// revChild files the App's child of item id of issue n's plan revision 1,
// titled title, with stage label stage and its first move there, linked
// under n if linked is set.
func revChild(f *githubtest.Fake, n int, id, title, stage string, linked bool) int {
	c := f.AddIssue(app, title, fmt.Sprintf("The task.\n\n<!-- issuedog:origin plan=#%d rev=1 item=%s -->", n, id), "type:task", stage)
	f.AddComment(c, app, "Created.\n\n<!-- issuedog:transition from=intake to="+strings.TrimPrefix(stage, "stage:")+" event=p1 -->")
	if linked {
		f.SetParent(c, n)
	}
	return c
}

// revStory files owner's story "Quotes" as issue 1, planned before: the
// golden story plan posted as plan revision 1, at plan-review, then
// approved into planned. Its children are T1, linked and closed as
// completed; T2, linked, at ready; and T3, the human task, at
// needs-human, filed after the approval but never linked, as a pass that
// stopped part way leaves one. A bug filed by hand is linked under it too.
// Then back, each a move's marker fields, bring it back to planning.
func revStory(t testing.TB, f *githubtest.Fake, back ...string) revIDs {
	t.Helper()
	var r revIDs
	r.n, _ = plFiled(f, "story", false, plEntry)
	r.plan = revMove(t, f, r.n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"},
		PlanRev: 1, Text: planText(r.n, 1), Markers: []github.Marker{planData(t, threeTasks)}})
	ms := github.ParseMarkers(f.State().Issues[r.n-1].Comments[len(f.State().Issues[r.n-1].Comments)-1].Body)
	r.approve = f.AddComment(r.n, app, fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r9 rev=1 body=%s -->",
		ms[len(ms)-1].Fields["body"]))
	r.t1 = revChild(f, r.n, "T1", "Record when each quote expires", "stage:done", true)
	f.CloseAs(r.t1, app, "completed")
	r.t2 = revChild(f, r.n, "T2", "Refuse an expired quote at booking", "stage:ready", true)
	r.t3 = revChild(f, r.n, "T3", "Set the quote lifetime in production", "stage:needs-human", false)
	r.other = f.AddIssue("owner", "A bug found on the way", "", "type:bug")
	f.SetParent(r.other, r.n)
	for _, m := range back {
		r.back = f.AddComment(r.n, app, "Moved.\n\n<!-- issuedog:transition "+m+" -->")
	}
	f.RemoveLabel(r.n, app, "stage:plan-review")
	f.AddLabel(r.n, app, "stage:planning")
	return r
}

// The planner's context for a revision: children.md lists the children of
// the approved plan, T1 closed with its reason and the unlinked T3 found
// among the issues since the approval, then revision 1's items, then the
// bug as another sub-issue. system.md says it's a revision of revision 1.
func TestPlannerContextRevision(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	r := revStory(t, f, revByOwner)
	d := ivDaemon(t, f, newOrigin(t))
	files := planContext(t, d, r.n)
	for _, want := range []string{
		"# Children of #1's plan\n\n",
		fmt.Sprintf("## Children of plan revision 1\n\n- T1: #%d, Record when each quote expires (type task, closed as completed)\n"+
			"- T2: #%d, Refuse an expired quote at booking (type task, `stage:ready`)\n"+
			"- T3: #%d, Set the quote lifetime in production (type task, `stage:needs-human`)\n\n", r.t1, r.t2, r.t3),
		"## Items of plan revision 1, as approved, with their task bodies",
		`"id": "T3"`, `"human": true`, `"test_plan": `,
		fmt.Sprintf("## Other sub-issues\n\nLinked under this issue, but not created from its plan: context only. Never list them as items.\n\n"+
			"- #%d: A bug found on the way (open, type bug, not in the pipeline)\n", r.other),
	} {
		if !strings.Contains(files["children.md"], want) {
			t.Errorf("children.md lacks\n%s\nin\n%s", want, files["children.md"])
		}
	}
	for _, want := range []string{
		"- This issue has 3 sub-issues, of the 100 GitHub allows, so a revision has at most 97 `new` items.",
		"- This issue was planned before, and revision 1 of its plan is approved, so this plan is a revision of it.",
	} {
		if !strings.Contains(files["system.md"], want) {
			t.Errorf("system.md lacks\n%s", want)
		}
	}
}

// TestPlannerRevision: a story planned before, set back to planning by the
// owner's hand, gets a planner run again. Its revision posts one comment, plan
// revision 2: each item's status in its item line, kept and dropped items
// on one line, the validation line from the revision's counts, and the
// plan-data marker keeping the statuses. It moves to plan-review, though
// plan.review is false (SPEC §7.2). The children are read once for the
// context, and once for the check, which Apply takes.
func TestPlannerRevision(t *testing.T) {
	t.Parallel()
	for name, back := range map[string][]string{
		"set back by the owner": {revByOwner},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			r := revStory(t, f, back...)
			g := plRig(t, f, revPlanReview)
			g.claude(ivResult(t, plGolden(t, "story-revision")), 0)
			comments := len(f.State().Issues[r.n-1].Comments)
			if err := g.work(r.n); err != nil {
				t.Fatal(err)
			}
			if got := len(f.State().Issues[r.n-1].Comments); got != comments+1 || g.runs() != 1 {
				t.Fatalf("%d comments posted after %d runs, want 1 after 1", got-comments, g.runs())
			}
			subs, since := 0, 0
			for _, q := range f.Requests() {
				switch {
				case q.Method == http.MethodGet && q.Path == fmt.Sprintf("/issues/%d/sub_issues", r.n):
					subs++
				case q.Method == http.MethodGet && q.Path == "/issues":
					since++
				}
			}
			if subs != 2 || since != 2 {
				t.Errorf("the sub-issues were read %d times and the issues since %d, want 2 each: the context's and the check's", subs, since)
			}
			text, markers := plComment(t, f, r.n)
			for _, want := range []string{
				"### Plan for #1 (rev 2)\n\n",
				"\n\n**Changes since rev 1.**\n\n",
				"\n\n**T1. Record when each quote expires** · kept\n\n",
				"\n\n**T2. Refuse an expired quote at booking, saying when it expired** · changed · scope:booking · p1\nOutcome: ",
				"\n\n**T3. Set the quote lifetime in production** · dropped\n\n",
				"\n\n**T4. Give the quote lifetime a default** · new · scope:booking · p2\nOutcome: ",
				"\n\nValidation (11.3): 3 tasks, within `plan.max_tasks` of 10, each with one scope from the list; " +
					"every child listed once, kept, changed or dropped, and each closed one kept; no dependency cycles; " +
					"4 sub-issues in all, within 100; at most 2 dependency links of one type per issue, within 50.\n\n",
				"\n\ncc @owner\n\n**To approve this plan, react 👍 to this comment.**",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("the revision's text lacks %q:\n%s", want, text)
				}
			}
			if len(markers) != 3 || !strings.HasPrefix(markers[0], "<!-- issuedog:transition from=planning to=plan-review rev=2 run=run1 ") ||
				!strings.HasPrefix(markers[2], "<!-- issuedog:revision kind=plan rev=2 body=") {
				t.Fatalf("markers %q, want the move to plan-review with rev 2, the plan data, and plan revision 2", markers)
			}
			plan, ok := plData(t, markers[1]).(map[string]any)
			if !ok {
				t.Fatalf("the plan data %v isn't an object", plData(t, markers[1]))
			}
			items, ok := plan["items"].([]any)
			if !ok {
				t.Fatalf("the plan data's items %v aren't a list", plan["items"])
			}
			var statuses []string
			for _, it := range items {
				m, ok := it.(map[string]any)
				if !ok {
					t.Fatalf("item %v isn't an object", it)
				}
				statuses = append(statuses, fmt.Sprint(m["id"], " ", m["status"]))
			}
			if want := []string{"T1 kept", "T2 changed", "T3 dropped", "T4 new"}; !slices.Equal(statuses, want) {
				t.Errorf("the plan data keeps %q, want %q", statuses, want)
			}
			plRevision(t, g, r.n, 2)
			if labels := ivLabels(f, r.n); !slices.Equal(labels, []string{"stage:plan-review", "type:story"}) {
				t.Errorf("labels %v, want stage:plan-review though plan.review is false", labels)
			}
			writes := len(f.Writes())
			if err := g.work(r.n); err != nil || len(f.Writes()) != writes || g.runs() != 1 {
				t.Errorf("the pass after = %v, with writes %v and %d runs; want no run and no write", err, f.Writes()[writes:], g.runs())
			}
		})
	}
}

// TestPlannerRevisionAfterPlanOutcome: a story planned before, held for
// discussion from planned by mallory's comment, which the owner's 👍
// approves, goes to clarifying when the discussion ends. The interviewer's
// plan outcome (#294) sends it to planning, and the planner's run that
// follows is a revision: its context has the interviewer's notes and the
// children of the approved plan, and revision 2 waits at plan-review,
// though plan.review is false.
func TestPlannerRevisionAfterPlanOutcome(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	r := revStory(t, f)
	f.RemoveLabel(r.n, app, "stage:planning")
	f.AddLabel(r.n, app, "stage:planned")
	m := f.AddComment(r.n, "mallory", planHoldMallory)
	if err := resolvePass(t, r.n)(f); err != nil {
		t.Fatal(err)
	}
	if c := newestComment(f, r.n); !strings.Contains(c, "to=held at=planned") {
		t.Fatalf("newest comment %q; want the hold at planned", c)
	}
	f.AddReaction(m, "owner", "+1")
	f.AddComment(r.n, "owner", ivPlanCounter)
	f.ForgetWrites()
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.planner(revPlanReview)
	ivHoldRig(g)
	if err := os.WriteFile(g.path("claude", "claude"), []byte(wholeRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	for i, out := range []map[string]any{ivPlanOut, plGolden(t, "story-revision")} {
		if err := os.WriteFile(g.path("claude", "out."+strconv.Itoa(i+1)), []byte(ivResult(t, out)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for range 3 {
		if err := g.work(r.n); err != nil {
			t.Fatal(err)
		}
	}
	if l := ivLabels(f, r.n); !slices.Equal(l, []string{"stage:plan-review", "type:story"}) || g.runs() != 2 {
		t.Fatalf("labels %q after %d runs; want plan-review after the interviewer's run and the planner's", l, g.runs())
	}
	if move := newestMarker(t, f, r.n, "from=clarifying to=planning"); !strings.Contains(move, " origin=planned") || !strings.Contains(move, " run=run1") {
		t.Errorf("the interviewer's move is %q, want run1's, with origin=planned", move)
	}
	text, markers := plComment(t, f, r.n)
	if len(markers) != 3 || !strings.Contains(markers[0], "from=planning to=plan-review rev=2 run=run2 ") ||
		!strings.Contains(text, "\n\n**T1. Record when each quote expires** · kept\n\n") || !strings.Contains(text, "To approve this plan") {
		t.Errorf("the revision's markers %q, want rev 2 at plan-review, and text\n%s", markers, text)
	}
	if in := g.file("ctx.2/input.md"); !strings.Contains(in, "the interviewer's move to planning after a discussion hold") {
		t.Errorf("the planner's input.md lacks the interviewer's notes:\n%s", in)
	}
	if children := g.file("ctx.2/children.md"); !strings.Contains(children, fmt.Sprintf("- T1: #%d, Record when each quote expires (type task, closed as completed)", r.t1)) {
		t.Errorf("the planner's children.md lacks T1:\n%s", children)
	}
	writes := len(f.Writes())
	if err := g.work(r.n); err != nil || len(f.Writes()) != writes || g.runs() != 2 {
		t.Errorf("a pass at plan-review = %v, writing %+v, after %d runs; want nothing", err, f.Writes()[writes:], g.runs())
	}
}

// newestMarker is the marker line with text in issue n's newest comment
// that has one, or fails t.
func newestMarker(t *testing.T, f *githubtest.Fake, n int, text string) string {
	t.Helper()
	cs := f.State().Issues[n-1].Comments
	for i := len(cs) - 1; i >= 0; i-- {
		for _, line := range strings.Split(cs[i].Body, "\n") {
			if strings.HasPrefix(line, "<!-- issuedog:") && strings.Contains(line, text) {
				return line
			}
		}
	}
	t.Fatalf("issue %d has no marker with %q", n, text)
	return ""
}

// revReposted is revStory with T2's child's body rewritten by the owner,
// losing its origin marker, which the App reposted in a comment (#291).
func revReposted(t testing.TB, f *githubtest.Fake) revIDs {
	t.Helper()
	r := revStory(t, f, revByOwner)
	f.EditBody(r.t2, "owner", "The task, rewritten by hand.")
	f.AddComment(r.t2, app, originRepost("<!-- issuedog:origin plan=#1 rev=1 item=T2 -->"))
	f.ForgetWrites()
	return r
}

// TestPlannerRevisionReposted: a child whose origin lives only in the App's
// repost, its body's marker lost to an edit, is still the approved plan's
// child: children.md lists it, and a revision that changes it passes, as
// it must list it, and posts revision 2. A repost someone else wrote
// counts for nothing: the child isn't the plan's, so a revision marking it
// fails the children rule.
func TestPlannerRevisionReposted(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	r := revReposted(t, f)
	d := ivDaemon(t, f, newOrigin(t))
	children := planContext(t, d, r.n)["children.md"]
	if want := fmt.Sprintf("- T2: #%d, Refuse an expired quote at booking (type task, `stage:ready`)\n", r.t2); !strings.Contains(children, want) {
		t.Errorf("children.md lacks\n%s\nin\n%s", want, children)
	}
	g := plRig(t, f, revPlanReview)
	g.claude(ivResult(t, plGolden(t, "story-revision")), 0)
	if err := g.work(r.n); err != nil {
		t.Fatal(err)
	}
	if _, markers := plComment(t, f, r.n); len(markers) != 3 || !strings.Contains(markers[0], "from=planning to=plan-review rev=2 ") {
		t.Fatalf("markers %q, want revision 2 at plan-review", markers)
	}

	f = newFake(t)
	r = revStory(t, f, revByOwner)
	f.EditBody(r.t2, "owner", "The task, rewritten by hand.")
	// With no repost, the child would be matched by the marker in the
	// App's original post (originLost, #347), so that version is deleted
	// from the history, and only the owner's copy could give one.
	f.DeleteRevision(r.t2, 0)
	f.AddComment(r.t2, "owner", "<!-- issuedog:origin plan=#1 rev=1 item=T2 -->")
	g = plRig(t, f, revPlanReview)
	g.d.Config.Loop.MaxAttempts = 1
	g.claude(ivResult(t, plGolden(t, "story-revision")), 0)
	if err := g.work(r.n); err != nil {
		t.Fatal(err)
	}
	want := "rule children: T2 is changed, but no child is item T2"
	if l := g.lease(); !l.Failed || !strings.Contains(l.PublicFailure, want) {
		t.Errorf("with the owner's copy of the marker, lease %+v; want a failed attempt naming %q", l, want)
	}
}

// TestPlannerRevisionCheckFails: a fake planner's revision that omits a
// child, lists one twice, changes a closed child or reuses an ID fails its
// attempt, writing nothing but the attempts error, which names the check.
// So does a first plan with a status, on an issue never planned.
func TestPlannerRevisionCheckFails(t *testing.T) {
	t.Parallel()
	drop := func(i int) func(testing.TB, map[string]any) {
		return func(t testing.TB, out map[string]any) {
			plan, ok := out["plan"].(map[string]any)
			if !ok {
				t.Fatalf("output %v has no plan", out)
			}
			items, ok := plan["items"].([]any)
			if !ok {
				t.Fatalf("plan %v has no items", plan)
			}
			plan["items"] = slices.Delete(items, i, i+1)
		}
	}
	for _, tt := range []struct {
		name   string
		change func(testing.TB, map[string]any)
		rule   string // the attempts error names it, with #T<n> for a child's number
		first  bool   // the golden story plan, on an issue never planned
	}{
		{"a child omitted", drop(2), "rule children: child #T3, item T3, isn't listed: every child is kept, changed or dropped", false},
		{"a child listed twice", func(t testing.TB, out map[string]any) {
			plan, ok := out["plan"].(map[string]any)
			if !ok {
				t.Fatalf("output %v has no plan", out)
			}
			items, ok := plan["items"].([]any)
			if !ok {
				t.Fatalf("plan %v has no items", plan)
			}
			plan["items"] = append(items, items[2])
		}, "rule children: child #T3, item T3, is listed 2 times, not once", false},
		{"a closed child changed", func(t testing.TB, out map[string]any) {
			changed := map[string]any{}
			for k, v := range plItem(t, out, 3) {
				changed[k] = v
			}
			changed["id"], changed["status"], changed["depends_on"] = "T1", "changed", []any{}
			plan, ok := out["plan"].(map[string]any)
			if !ok {
				t.Fatalf("output %v has no plan", out)
			}
			items, ok := plan["items"].([]any)
			if !ok {
				t.Fatalf("plan %v has no items", plan)
			}
			items[0] = changed
			plItem(t, out, 3)["depends_on"] = []any{"T1"}
		}, "rule closed-child: child #T1, item T1, is closed, so it can only be kept, not changed", false},
		{"a new item reusing an ID", func(t testing.TB, out map[string]any) {
			plItem(t, out, 3)["id"] = "T2"
			plItem(t, out, 1)["depends_on"] = []any{"T1"}
		}, "rule new-id: T2 is new, but it's the item of child #T2", false},
		{"a first plan with a status", func(t testing.TB, out map[string]any) { plItem(t, out, 1)["status"] = "new" },
			`rule status: T2 has status "new", but this is the issue's first plan, whose items have none`, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			var r revIDs
			out := plGolden(t, "story-revision")
			if tt.first {
				r.n, _ = plFiled(f, "story", false, plEntry)
				out = plGolden(t, "story-plan")
			} else {
				r = revStory(t, f, revByOwner)
			}
			g := plRig(t, f, true)
			g.d.Config.Loop.MaxAttempts = 1
			tt.change(t, out)
			g.claude(ivResult(t, out), 0)
			before := commentBodies(f, r.n)
			if err := g.work(r.n); err != nil {
				t.Fatal(err)
			}
			rule := strings.NewReplacer("#T1", fmt.Sprintf("#%d", r.t1), "#T2", fmt.Sprintf("#%d", r.t2), "#T3", fmt.Sprintf("#%d", r.t3)).Replace(tt.rule)
			after := commentBodies(f, r.n)
			if l := g.lease(); g.runs() != 1 || !l.Failed || !strings.Contains(l.PublicFailure, rule) {
				t.Errorf("after %d runs, lease %+v; want one failed attempt naming %q", g.runs(), l, rule)
			}
			if len(after) != len(before)+1 || !strings.Contains(after[len(after)-1], rule) || strings.Contains(after[len(after)-1], "### Plan") {
				t.Errorf("comments %q, want the attempts error alone, naming %q", after[len(before):], rule)
			}
			if labels := ivLabels(f, r.n); !slices.Equal(labels, []string{"stage:planning", "type:story"}) {
				t.Errorf("labels %v, want the issue left at planning", labels)
			}
		})
	}
}

// TestPlannerRevisionReplay: the revision's writes, the comment, the stage
// label and the old label's removal, are safe to replay after a crash at
// any of them, and it reaches plan-review with plan.review false.
func TestPlannerRevisionReplay(t *testing.T) {
	t.Parallel()
	replayRuns(t, func(tb testing.TB, f *githubtest.Fake, _ string) {
		revStory(tb, f, revByOwner)
		f.ForgetWrites()
	}, func(g *rig) {
		g.planner(revPlanReview)
		g.claude(ivResult(t, plGolden(t, "story-revision")), 0)
	})
}

// revKeptOut is the golden revision with every child of revision 1 kept,
// and no other item: T1, T2 and T3, each its ID, title and status alone.
func revKeptOut(t testing.TB) map[string]any {
	t.Helper()
	out := plGolden(t, "story-revision")
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	first := plGolden(t, "story-plan")
	var items []any
	for i := range 3 {
		it := plItem(t, first, i)
		items = append(items, map[string]any{"id": it["id"], "title": it["title"], "status": "kept"})
	}
	plan["items"], plan["order"] = items, "As before"
	return out
}

// revKeptTwice is revStory with T3's child linked, set back by the owner,
// and revised by the real Planner as plan revision 2, which keeps all
// three children (revKeptOut), approved into planned, then set back to
// planning by the owner again. So its next plan is its third, and T2,
// which depends on T1 and T3 in revision 1, is kept twice. Revision 2's
// run is second1, so another rig's first run, run1, isn't it.
func revKeptTwice(t *testing.T, tb testing.TB, f *githubtest.Fake) revIDs {
	tb.Helper()
	r := revStory(tb, f, revByOwner)
	f.SetParent(r.t3, r.n)
	g := plRig(t, f, revPlanReview)
	ids := 0
	g.r.NewID = func() string { ids++; return "second" + strconv.Itoa(ids) }
	g.claude(ivResult(tb, revKeptOut(tb)), 0)
	if err := g.work(r.n); err != nil {
		tb.Fatal(err)
	}
	cs := f.State().Issues[r.n-1].Comments
	ms := github.ParseMarkers(cs[len(cs)-1].Body)
	if len(ms) != 3 || ms[2].Kind != "revision" || ms[2].Fields["rev"] != "2" {
		tb.Fatalf("the newest comment's markers are %+v, want plan revision 2's", ms)
	}
	f.AddComment(r.n, app, fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r19 rev=2 body=%s -->", ms[2].Fields["body"]))
	f.AddComment(r.n, app, "Moved.\n\n<!-- issuedog:transition from=planned to=planning event=32 by=owner -->")
	f.RemoveLabel(r.n, app, "stage:plan-review")
	f.AddLabel(r.n, app, "stage:planning")
	return r
}

// revKeptTwiceT2 is what revision 2's data keeps for T2, kept twice by the
// third plan: revision 1's T2 in full, its outcome, first criterion and
// dependencies, as children.md shows them.
func revKeptTwiceT2(t testing.TB) []string {
	t.Helper()
	t2 := plItem(t, plGolden(t, "story-plan"), 1)
	criteria, ok := t2["criteria"].([]any)
	if !ok || len(criteria) == 0 {
		t.Fatalf("T2 %v has no criteria", t2)
	}
	return []string{
		fmt.Sprintf(`"outcome": "%s"`, t2["outcome"]),
		fmt.Sprintf(`"%s"`, criteria[0]),
		"\"depends_on\": [\n      \"T1\",\n      \"T3\"\n    ]",
	}
}

// TestPlannerRevisionKeptTwice: on a story's third plan, T2, kept by
// revision 2 and kept again, keeps its dependencies on T1 and T3 from
// revision 1, which revision 2's data carries in full. children.md shows
// its approved text, and a revision that changes T3 to depend on T2 fails
// the cycle check, writing nothing but the attempts error.
func TestPlannerRevisionKeptTwice(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	r := revKeptTwice(t, t, f)
	children := planContext(t, ivDaemon(t, f, newOrigin(t)), r.n)["children.md"]
	for _, want := range append([]string{"## Items of plan revision 2, as approved"}, revKeptTwiceT2(t)...) {
		if !strings.Contains(children, want) {
			t.Errorf("the third plan's children.md lacks\n%s\nin\n%s", want, children)
		}
	}

	out := revKeptOut(t)
	t3 := plItem(t, plGolden(t, "story-plan"), 2)
	t3["status"], t3["depends_on"] = "changed", []any{"T2"}
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	items, ok := plan["items"].([]any)
	if !ok {
		t.Fatalf("plan %v has no items", plan)
	}
	items[2] = t3
	g := plRig(t, f, revPlanReview)
	g.d.Config.Loop.MaxAttempts = 1
	g.claude(ivResult(t, out), 0)
	before := commentBodies(f, r.n)
	if err := g.work(r.n); err != nil {
		t.Fatal(err)
	}
	const want = "rule cycle: T2 → T3 → T2"
	after := commentBodies(f, r.n)
	if l := g.lease(); g.runs() != 1 || !l.Failed || !strings.Contains(l.PublicFailure, want) {
		t.Errorf("after %d runs, lease %+v; want one failed attempt naming %q", g.runs(), l, want)
	}
	if len(after) != len(before)+1 || !strings.Contains(after[len(after)-1], want) || strings.Contains(after[len(after)-1], "### Plan") {
		t.Errorf("comments %q, want the attempts error alone, naming %q", after[len(before):], want)
	}
}

// TestPlannerRevisionKeptTwiceReplay: a third plan that keeps all three
// children again posts plan revision 3, safe to replay after a crash at
// any write, and its data keeps the twice-kept T2 in full, on T1 and T3,
// for a fourth plan to read.
func TestPlannerRevisionKeptTwiceReplay(t *testing.T) {
	t.Parallel()
	replayRuns(t, func(tb testing.TB, f *githubtest.Fake, _ string) {
		revKeptTwice(t, tb, f)
		f.ForgetWrites()
	}, func(g *rig) {
		g.planner(revPlanReview)
		g.claude(ivResult(t, revKeptOut(t)), 0)
	})

	f := newFake(t)
	r := revKeptTwice(t, t, f)
	g := plRig(t, f, revPlanReview)
	g.claude(ivResult(t, revKeptOut(t)), 0)
	if err := g.work(r.n); err != nil {
		t.Fatal(err)
	}
	_, markers := plComment(t, f, r.n)
	if len(markers) != 3 || !strings.HasPrefix(markers[0], "<!-- issuedog:transition from=planning to=plan-review rev=3 run=run1 ") {
		t.Fatalf("markers %q, want the move to plan-review with rev 3, the plan data, and plan revision 3", markers)
	}
	plan, ok := plData(t, markers[1]).(map[string]any)
	if !ok {
		t.Fatalf("the plan data %v isn't an object", plData(t, markers[1]))
	}
	items, ok := plan["items"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("the plan data's items %v aren't three", plan["items"])
	}
	raw, err := json.MarshalIndent(items[1], "  ", "  ") // as children.md indents an item
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range append([]string{`"status": "kept"`, `"test_plan": `}, revKeptTwiceT2(t)...) {
		if !strings.Contains(string(raw), want) {
			t.Errorf("revision 3's data for T2 lacks\n%s\nin\n%s", want, raw)
		}
	}
	plRevision(t, g, r.n, 3)
}

// revApproved is revStory with the planner's revision posted as plan
// revision 2, the golden revision (T1 kept, T2 changed, T3 dropped, T4
// new), at plan-review, and the owner's 👍 on it, with the writes
// forgotten.
func revApproved(t testing.TB) *githubtest.Fake {
	return revApprovedWith(t, planData(t, revisionPlan))
}

// revApprovedWith is revApproved with data as revision 2's plan-data
// marker.
func revApprovedWith(t testing.TB, data github.Marker) *githubtest.Fake {
	f := newFake(t)
	r := revStory(t, f, revByOwner)
	c := revMove(t, f, r.n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p2"},
		PlanRev: 2, Text: planText(r.n, 2), Markers: []github.Marker{data}})
	f.AddReaction(c, "owner", "+1")
	f.ForgetWrites()
	return f
}

// revHumanData is the plan-data marker of the golden revision with T4 a
// new human task, the golden story plan's T3 body, which the changed T2
// depends on.
func revHumanData(t testing.TB) github.Marker {
	t.Helper()
	out := plGolden(t, "story-revision")
	human := plItem(t, plGolden(t, "story-plan"), 2)
	t4 := plItem(t, out, 3)
	delete(t4, "task")
	t4["human"], t4["human_task"], t4["scope"], t4["depends_on"] = true, human["human_task"], "infra", []any{}
	plItem(t, out, 1)["depends_on"] = []any{"T1", "T4"}
	raw, err := json.Marshal(out["plan"])
	if err != nil {
		t.Fatal(err)
	}
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": base64.RawURLEncoding.EncodeToString(z.Bytes())}}
}

// A new human task that the changed T2 depends on is filed, after it, its
// "Why it blocks" naming T2's issue, which is found, not filed again.
func TestChildrenOfRevisionHumanBlocksChanged(t *testing.T) {
	t.Parallel()
	setup := func(tb testing.TB) *githubtest.Fake { return revApprovedWith(tb, revHumanData(tb)) }
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	issues := f.State().Issues
	if len(issues) != 6 {
		t.Fatalf("%d issues, want one filed, for T4", len(issues))
	}
	if t4 := issues[5]; !slices.Contains(t4.Labels, "stage:needs-human") || !strings.Contains(t4.Body, "**Why it blocks:** #3 ") {
		t.Errorf("the new human task is labelled %v, with body\n%s\nwant it at needs-human, blocking #3, T2's child", t4.Labels, t4.Body)
	}
}

// revisionPlan is the golden revision, all four items, on a labels repo.
var revisionPlan = childPlan{"Story", "story-revision", 4, false}

// TestChildrenOfRevision: creating the children of an approved revision
// files only its new items' children, and rewrites its changed ones (SPEC
// §5.2, #296). The owner's 👍 on a revision with one item of each status files
// exactly one child, for the new T4, linked and at ready, with its origin
// naming revision 2, and blocked by the kept T1's child, as its item
// depends on T1 (#339's links). The changed T2's child is rewritten from
// revision 2 (TestChildrenChanged has the details), and its links follow
// the revision: blocked by T1's and T4's children (#297). The kept child
// gets no write, and nothing errors on the kept and dropped items, which
// carry no body to describe.
//
// The dropped T3's child was never linked under the story, and hasn't
// been updated since revision 2's approval, as for a child someone
// unlinked by hand, but it's still found, as the planner found it, since
// the children are listed since the plan's first approval, revision 1's
// (#362): it's closed as not planned, and the record of revision 2, the
// fixture's only record, lists T1's, T2's and T4's children. A pass that
// crashes at any write leaves the same, and a later pass writes nothing.
func TestChildrenOfRevision(t *testing.T) {
	t.Parallel()
	githubtest.Replay(t, revApproved, childrenPass(t, revisionPlan))
	f := run(t, revApproved, childrenPass(t, revisionPlan))
	issues := f.State().Issues
	if got := issues[0].Labels; !slices.Contains(got, "stage:planned") {
		t.Fatalf("issue 1 is labelled %v, want stage:planned", got)
	}
	creates := 0
	for _, w := range f.Writes() {
		if isCreate(w) {
			creates++
		}
	}
	if creates != 1 || len(issues) != 6 {
		t.Fatalf("%d issues filed, %d in all; want one filed, for T4, after the story, its three children and the bug", creates, len(issues))
	}
	t4 := issues[5]
	if t4.Title != "Give the quote lifetime a default" || !strings.Contains(t4.Body, "<!-- issuedog:origin plan=#1 rev=2 item=T4 -->") ||
		!slices.Contains(t4.Labels, "stage:ready") || !slices.Contains(t4.Labels, "scope:booking") {
		t.Errorf("the new child is %q, labelled %v, with body\n%s\nwant T4's, at ready, from revision 2", t4.Title, t4.Labels, t4.Body)
	}
	if subs := issues[0].SubIssues; !slices.Contains(subs, 6) {
		t.Errorf("issue 1's sub-issues are %v, want #6 linked", subs)
	}
	if got := t4.BlockedBy; !slices.Equal(got, []int{2}) {
		t.Errorf("the new child is blocked by %v, want T1's #2 alone", got)
	}
	var records []string
	for _, c := range issues[0].Comments {
		if strings.Contains(c.Body, "<!-- issuedog:created ") {
			records = append(records, c.Body)
		}
	}
	want := "I've applied plan rev 2 to the children, which are now:\n- T1 #2: Record when each quote expires\n" +
		"- T2 #3: Refuse an expired quote at booking, saying when it expired\n- T4 #6: Give the quote lifetime a default\n\n" +
		"<!-- issuedog:created rev=2 children=2,3,6 -->"
	if len(records) != 1 || records[0] != want {
		t.Errorf("issue 1's records of its children are %q, want only\n%q", records, want)
	}
	if reason := droppedReason(t, f, 4); !issues[3].Closed || reason != "not_planned" {
		t.Errorf("T3's child, never linked, is closed %v as %q; want it found, and closed as not planned", issues[3].Closed, reason)
	}
	for _, w := range f.Writes() {
		if strings.HasPrefix(w.Path, "/issues/2/") || w.Path == "/issues/2" {
			t.Errorf("child #2, kept, got the write %+v", w)
		}
	}
	if t2 := issues[2]; t2.Title != "Refuse an expired quote at booking, saying when it expired" ||
		!strings.Contains(t2.Body, "<!-- issuedog:origin plan=#1 rev=2 item=T2 -->") || !slices.Equal(t2.BlockedBy, []int{2, 6}) {
		t.Errorf("the changed child is %q, blocked by %v, with body\n%s\nwant T2's from revision 2, blocked by T1's #2 and T4's #6", t2.Title, t2.BlockedBy, t2.Body)
	}
	noSearch(t, f)
}
