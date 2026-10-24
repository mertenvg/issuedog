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
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// An approved revision's changed children (SPEC §6.4, #296): each is
// rewritten in place from the revision, its labels set to the item's, and
// sent back to its first stage, or kept held with that stage as its
// hold's at and origin. Kept children get no write.

// changedPlanData is the plan-data marker holding out's plan, as the
// planner's run writes it (planDataMarker in internal/daemon).
func changedPlanData(t testing.TB, out map[string]any) github.Marker {
	t.Helper()
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

// changedStoryRevision is the golden story revision with T3, the human
// task, changed instead of dropped: its title and check now ask for 12
// hours, and the changed T2 depends on it, so its "Why it blocks" names
// T2's issue.
func changedStoryRevision(t testing.TB) map[string]any {
	t.Helper()
	out := plGolden(t, "story-revision")
	t3 := plItem(t, plGolden(t, "story-plan"), 2)
	t3["status"], t3["title"] = "changed", "Set the quote lifetime in production to 12 hours"
	human, ok := t3["human_task"].(map[string]any)
	if !ok {
		t.Fatalf("T3 %v has no human_task", t3)
	}
	done, ok := human["done_when"].(map[string]any)
	if !ok {
		t.Fatalf("T3 %v has no done_when", t3)
	}
	check, ok := done["check"].(map[string]any)
	if !ok {
		t.Fatalf("T3's done_when %v has no check", done)
	}
	done["text"], check["contains"] = "Production's settings on `main` set `QUOTE_TTL=12h`.", "QUOTE_TTL=12h"
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	items, ok := plan["items"].([]any)
	if !ok || len(items) < 3 {
		t.Fatalf("plan %v has no item 3", plan)
	}
	items[2] = t3
	plItem(t, out, 1)["depends_on"] = []any{"T1", "T3", "T4"}
	return out
}

// changedIDs are the issues and comments changedStory files.
type changedIDs struct {
	revIDs
	hold int64 // owner's /hold on T2's child, if held
	plan int64 // plan revision 2's comment
}

// changedStory is revStory brought back to planning by the owner, with its
// children as creating them left them (SPEC §5.2), each with its first
// move, keyed by revision 1's approval: T2's child, picked up and at
// in-progress, with a priority set by hand and the model revision 1 gave
// it, and T3's, the human task, at needs-human, with human=1, linked now.
// If held, owner's /hold holds T2's child from in-progress, and T3's from
// needs-human; r.hold is the latter /hold. Then edit, if set, changes the
// children, and changedStoryRevision is posted as plan revision 2 at
// plan-review, with the owner's 👍 on it, and the writes forgotten.
func changedStory(t testing.TB, f *githubtest.Fake, held bool, edit func(*githubtest.Fake, changedIDs)) changedIDs {
	t.Helper()
	return changedStoryWith(t, f, held, edit, changedPlanData(t, changedStoryRevision(t)))
}

// changedStoryWith is changedStory with data as plan revision 2's
// plan-data marker, such as a revision that drops T3 (#297).
func changedStoryWith(t testing.TB, f *githubtest.Fake, held bool, edit func(*githubtest.Fake, changedIDs), data github.Marker) changedIDs {
	t.Helper()
	r := changedIDs{revIDs: revStory(t, f, revByOwner)}
	first := func(n int, stage string) {
		f.AddComment(n, app, fmt.Sprintf("Created.\n\n<!-- issuedog:transition from=intake to=%s event=p%d -->", stage, r.approve))
	}
	first(r.t2, "ready")
	f.AddComment(r.t2, app, "Picked up.\n\n<!-- issuedog:transition from=ready to=in-progress run=i1 -->")
	f.RemoveLabel(r.t2, app, "stage:ready")
	f.AddLabel(r.t2, app, "stage:in-progress")
	f.AddLabel(r.t2, "owner", "priority:p3")
	f.AddLabel(r.t2, app, "model:opus")
	hold := func(n int, from string) {
		r.hold = f.AddComment(n, "owner", "/hold")
		f.AddComment(n, app, fmt.Sprintf("On hold.\n\n<!-- issuedog:transition from=%s to=held at=%s reason=owner cmd=%d -->", from, from, r.hold))
		f.AddReaction(r.hold, app, "+1")
		f.RemoveLabel(n, app, "stage:"+from)
		f.AddLabel(n, app, "stage:held")
	}
	if held {
		hold(r.t2, "in-progress")
	}
	f.EditBody(r.t3, app, "The human task.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T3 human=1 -->")
	f.SetParent(r.t3, r.n) // creation links each child in the pass that files it
	first(r.t3, "needs-human")
	if held {
		hold(r.t3, "needs-human")
	}
	if edit != nil {
		edit(f, r)
	}
	r.plan = revMove(t, f, r.n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p2"},
		PlanRev: 2, Text: planText(r.n, 2), Markers: []github.Marker{data}})
	f.AddReaction(r.plan, "owner", "+1")
	f.ForgetWrites()
	return r
}

// changedSetup is changedStory on a new fake, for Replay.
func changedSetup(held bool, edit func(*githubtest.Fake, changedIDs)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		changedStory(t, f, held, edit)
		return f
	}
}

// changedApproval returns, of issue 1's comments, the move into planned
// that approved plan revision 2, which keys the changed children's moves,
// and that revision's plan comment, which their comments link.
func changedApproval(t *testing.T, f *githubtest.Fake) (approve, plan int64) {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		switch {
		case c.Author.Login != app:
		case strings.Contains(c.Body, "from=plan-review to=planned rev=2 "):
			approve = c.ID
		case strings.Contains(c.Body, "<!-- issuedog:revision kind=plan rev=2 "):
			plan = c.ID
		}
	}
	if approve == 0 || plan == 0 {
		t.Fatalf("issue 1's approval of plan revision 2 is comment %d, and its plan comment %d; want both", approve, plan)
	}
	return approve, plan
}

// changedLastMarker is the transition marker in issue n's newest comment.
func changedLastMarker(t *testing.T, f *githubtest.Fake, n int) github.Marker {
	t.Helper()
	cs := f.State().Issues[n-1].Comments
	for _, m := range github.ParseMarkers(cs[len(cs)-1].Body) {
		if m.Kind == "transition" {
			return m
		}
	}
	t.Fatalf("issue %d's newest comment has no transition marker: %q", n, cs[len(cs)-1].Body)
	return github.Marker{}
}

// changedMoveText is the text of the move of a child the revision changed,
// at stage, linking plan revision 2's comment, plan.
func changedMoveText(item string, plan int64, tail string) string {
	return fmt.Sprintf("Rewritten from #1's plan, rev 2, as %s, since [the plan](https://github.com/o/r/issues/1#issuecomment-%d) changed it. %s",
		item, plan, tail)
}

// The owner's 👍 on a revision that changes a task and a human task
// rewrites each in place, title and whole body, from revision 2, as a new
// child of its kind is rendered, with the rewrite marker first and the
// origin naming revision 2 last, the human task's with human=1 and its new
// check. Each one's type, scope, priority, model and effort labels become
// the item's, so the priority set by hand and the model revision 1 gave
// go. The task goes back to ready from in-progress, and the human task to
// needs-human, each with a comment naming the revision and linking the
// plan, keyed by the approval. The kept child, closed, and the bug filed
// by hand get no write, nor does the parent but for its approval. A crash
// at any write, before or after it applies, writes nothing twice.
func TestChildrenChanged(t *testing.T) {
	t.Parallel()
	setup := changedSetup(false, nil)
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	issues := f.State().Issues
	p, plan := changedApproval(t, f)
	t2, t3 := issues[2], issues[3]

	if t2.Title != "Refuse an expired quote at booking, saying when it expired" || t2.BodyEditedBy != app ||
		!strings.HasPrefix(t2.Body, "<!-- issuedog:rewrite run=p2 -->\n**Parent:** #1   **Scope:** scope:booking   **Priority:** p1\n\n## Context\n\n") ||
		!strings.HasSuffix(t2.Body, "\n\n<!-- issuedog:origin plan=#1 rev=2 item=T2 -->") ||
		!strings.Contains(t2.Body, "- The MCP server's book tool returns the same error.") {
		t.Errorf("T2's child is %q, last edited by %q, with body\n%s\nwant revision 2's T2, rewritten whole by the App", t2.Title, t2.BodyEditedBy, t2.Body)
	}
	if want := []string{"priority:p1", "scope:booking", "stage:ready", "type:task"}; !reflect.DeepEqual(t2.Labels, want) {
		t.Errorf("T2's child is labelled %v, want %v", t2.Labels, want)
	}
	if got, want := t2.Comments[len(t2.Comments)-1].Body, changedMoveText("T2", plan, "It goes back to `stage:ready`.")+
		fmt.Sprintf("\n\n<!-- issuedog:transition from=in-progress to=ready event=p%d -->", p); got != want {
		t.Errorf("T2's child's newest comment is\n%q\nwant\n%q", got, want)
	}

	if t3.Title != "Set the quote lifetime in production to 12 hours" || t3.BodyEditedBy != app ||
		!strings.HasPrefix(t3.Body, "<!-- issuedog:rewrite run=p2 -->\n") ||
		!strings.HasSuffix(t3.Body, "\n\n<!-- issuedog:origin plan=#1 rev=2 item=T3 check=file contains=QUOTE_TTL=12h human=1 value=deploy/production.env -->") ||
		!strings.Contains(t3.Body, "**Why it blocks:** #3 ") {
		t.Errorf("T3's child is %q, last edited by %q, with body\n%s\nwant revision 2's human task T3, blocking #3, rewritten whole", t3.Title, t3.BodyEditedBy, t3.Body)
	}
	if want := []string{"priority:p2", "scope:infra", "stage:needs-human", "type:task"}; !reflect.DeepEqual(t3.Labels, want) {
		t.Errorf("T3's child is labelled %v, want %v", t3.Labels, want)
	}
	if got, want := t3.Comments[len(t3.Comments)-1].Body, changedMoveText("T3", plan, "It goes back to `stage:needs-human`.")+
		fmt.Sprintf("\n\n<!-- issuedog:transition from=needs-human to=needs-human event=p%d -->", p); got != want {
		t.Errorf("T3's child's newest comment is\n%q\nwant\n%q", got, want)
	}

	for _, w := range f.Writes() {
		for _, n := range []int{2, 5} {
			if strings.HasPrefix(w.Path, fmt.Sprintf("/issues/%d/", n)) || w.Path == fmt.Sprintf("/issues/%d", n) {
				t.Errorf("#%d, the kept child or the bug, got the write %+v", n, w)
			}
		}
	}
	noSearch(t, f)
}

// changedKeptBlocksRevision is changedStoryRevision with T2 kept rather
// than changed: its data is revision 1's entry for it in full, depending
// on T1 and T3, as a revision's data carries a kept item's approved entry
// (planPrior.keptData, #351). So the changed human task T3 blocks the
// kept T2.
func changedKeptBlocksRevision(t testing.TB) map[string]any {
	t.Helper()
	out := changedStoryRevision(t)
	t2 := plItem(t, plGolden(t, "story-plan"), 1)
	t2["status"] = "kept"
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	items, ok := plan["items"].([]any)
	if !ok || len(items) < 2 {
		t.Fatalf("plan %v has no item 2", plan)
	}
	items[1] = t2
	return out
}

// changedKeptBlocksSetup is changedStory with changedKeptBlocksRevision as
// plan revision 2, on a new fake, for Replay, with each of moved, a
// child's number, then moved to another repo.
func changedKeptBlocksSetup(moved ...int) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		changedStoryWith(t, f, false, nil, changedPlanData(t, changedKeptBlocksRevision(t)))
		for _, n := range moved {
			f.Transfer(n, "o/elsewhere")
		}
		return f
	}
}

// A changed human task's "Why it blocks" names the issue of a kept item
// that depends on it, as a kept item's data carries its dependencies
// (SPEC §9.3): here the kept T2's #3, not the story, #1, as when no item
// depends on it. The kept child gets no write. A crash at any write,
// before or after it applies, writes nothing twice.
func TestChildrenChangedHumanBlocksKept(t *testing.T) {
	t.Parallel()
	setup := changedKeptBlocksSetup()
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	t3 := f.State().Issues[3]
	if t3.Title != "Set the quote lifetime in production to 12 hours" || t3.BodyEditedBy != app ||
		!strings.Contains(t3.Body, "**Why it blocks:** #3 can't proceed until ") {
		t.Errorf("T3's child is %q, last edited by %q, with body\n%s\nwant revision 2's human task T3, blocking the kept T2's #3", t3.Title, t3.BodyEditedBy, t3.Body)
	}
	droppedNoWrite(t, f, 3, "the kept T2's child")
	noSearch(t, f)
}

// A changed child whose body someone made dirty is rewritten whole, so the
// body is clean again: its rewrite marker names who made it dirty, and
// its markers, the origin naming revision 2 among them, are read again.
// Its origin had been reposted by the App (#291), which is how it's still
// the plan's child.
func TestChildrenChangedDirty(t *testing.T) {
	t.Parallel()
	setup := changedSetup(false, func(f *githubtest.Fake, r changedIDs) {
		f.EditBody(r.t2, "eve", "The task, rewritten by eve.")
		f.AddComment(r.t2, app, originRepost("<!-- issuedog:origin plan=#1 rev=1 item=T2 -->"))
	})
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	dirt := func(f *githubtest.Fake) (*github.Snapshot, daemon.Dirt) {
		t.Helper()
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 3)
		if err != nil {
			t.Fatal(err)
		}
		a := &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}
		d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
		if err != nil {
			t.Fatal(err)
		}
		return s, d
	}
	if _, d := dirt(setup(t)); !d.Has(state.BodyID) {
		t.Fatal("before the approval, T2's child's body is clean; want it dirty, by eve's edit")
	}
	f := run(t, setup, childrenPass(t, revisionPlan))
	s, d := dirt(f)
	if !strings.HasPrefix(s.Body, "<!-- issuedog:rewrite run=p2 replaced=eve -->\n") {
		t.Errorf("T2's child's body is\n%s\nwant the App's rewrite, naming eve's edit it replaced", s.Body)
	}
	if o := state.Derive(s, slug, d.Has).Origin; d.Has(state.BodyID) || o.Rev != 2 || o.Comment != state.BodyID {
		t.Errorf("T2's child is dirty %t, with origin %+v; want a clean body, whose origin names revision 2", d.Has(state.BodyID), o)
	}
}

// A changed child that's held stays held: its held → held marker, keyed by
// the approval, makes ready its at and origin, with the hold's seen and no
// reason, so it's no hold of its own, and its labels and body are still
// rewritten. Owner's /resume then takes it to ready, not back to
// in-progress, where it was held from.
func TestChildrenChangedHeld(t *testing.T) {
	t.Parallel()
	setup := changedSetup(true, nil)
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	p, plan := changedApproval(t, f)
	t2 := f.State().Issues[2]
	if want := []string{"priority:p1", "scope:booking", "stage:held", "type:task"}; !reflect.DeepEqual(t2.Labels, want) ||
		!strings.Contains(t2.Body, "rev=2 item=T2") {
		t.Errorf("T2's child is labelled %v, with body\n%s\nwant it held, rewritten from revision 2, labelled %v", t2.Labels, t2.Body, want)
	}
	// The hold's seen is the bound before it, the move to in-progress's
	// comment, as the hold's marker has none.
	var picked int64
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if strings.Contains(c.Body, "to=in-progress run=i1") {
			picked = c.ID
		}
	}
	m := changedLastMarker(t, f, 3)
	want := map[string]string{"from": "held", "to": "held", "event": fmt.Sprintf("p%d", p), "at": "ready", "origin": "ready",
		"seen": fmt.Sprintf("%d", picked)}
	if !reflect.DeepEqual(m.Fields, want) {
		t.Errorf("T2's child's newest marker has %v, want %v", m.Fields, want)
	}
	if !strings.HasPrefix(t2.Comments[len(t2.Comments)-1].Body, changedMoveText("T2", plan, "It stays on hold, and `/resume` takes it to `stage:ready`.")) {
		t.Errorf("T2's child's newest comment is %q, want the change's, saying it stays on hold", t2.Comments[len(t2.Comments)-1].Body)
	}
	// T3's child, the human task, held from needs-human, keeps needs-human
	// as its at and origin, with its hold's seen, its first move's comment.
	t3, err := f.Client(t).Snapshot(context.Background(), "o", "r", 4)
	if err != nil {
		t.Fatal(err)
	}
	var created int64
	for _, c := range t3.Comments {
		if strings.Contains(c.Body, "from=intake to=needs-human ") {
			created = c.ID
		}
	}
	want = map[string]string{"from": "held", "to": "held", "event": fmt.Sprintf("p%d", p), "at": "needs-human", "origin": "needs-human",
		"seen": fmt.Sprintf("%d", created)}
	if m := changedLastMarker(t, f, 4); !reflect.DeepEqual(m.Fields, want) || !slices.Contains(f.State().Issues[3].Labels, "stage:held") {
		t.Errorf("T3's child, labelled %v, has the newest marker %v, want %v, still held", f.State().Issues[3].Labels, m.Fields, want)
	}

	resume := f.AddComment(3, "owner", "/resume")
	st := childrenStep(t, f, revisionPlan)
	s, err = st.Client.Snapshot(context.Background(), "o", "r", 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := f.State().Issues[2].Labels; !slices.Contains(got, "stage:ready") {
		t.Errorf("after /resume, T2's child is labelled %v, want stage:ready", got)
	}
	if m := changedLastMarker(t, f, 3); m.Fields["from"] != "held" || m.Fields["to"] != "ready" || m.Fields["cmd"] != fmt.Sprintf("%d", resume) {
		t.Errorf("after /resume, T2's child's newest marker has %v, want held → ready by the /resume", m.Fields)
	}
}

// changedEpicRevision is the epic's plan revision 2: S1 changed, to 12
// hours, with priority p1 and no model or effort, and S2 kept. Revision
// 1 had S1 and S2 (twoStories).
func changedEpicRevision(t testing.TB) map[string]any {
	t.Helper()
	out := plGolden(t, "epic-plan")
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	plan["items"] = []any{
		map[string]any{"id": "S1", "title": "Quotes expire after 12 hours", "status": "changed",
			"outcome":    "Booking a quote more than 12 hours old is refused, with a link to a new quote.",
			"criteria":   []any{"Booking a quote more than 12 hours old returns an expired error and books nothing."},
			"depends_on": []any{}, "priority": "p1"},
		map[string]any{"id": "S2", "title": "Customers see when a quote expires", "status": "kept"},
	}
	return out
}

// changedEpic files owner's epic "Quotes" as issue 1, planned before into
// twoStories, as plan revision 1, approved; its stories S1, issue 2, at
// planning, with revision 1's model and effort labels, and S2, issue 3, at
// planning too, each the App's, linked under it, with its first move,
// keyed by the approval. Then the owner brings the epic back to planning,
// and changedEpicRevision is posted as plan revision 2 at plan-review,
// with the owner's 👍 on it, and the writes are forgotten.
func changedEpic(t testing.TB, f *githubtest.Fake) {
	t.Helper()
	n, _ := plFiled(f, "epic", false, plEntry)
	revMove(t, f, n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"},
		PlanRev: 1, Text: planText(n, 1), Markers: []github.Marker{planData(t, twoStories)}})
	cs := f.State().Issues[n-1].Comments
	ms := github.ParseMarkers(cs[len(cs)-1].Body)
	approve := f.AddComment(n, app, fmt.Sprintf("Approved.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r9 rev=1 body=%s -->",
		ms[len(ms)-1].Fields["body"]))
	for _, s := range []struct{ id, title string }{{"S1", "S1. Quotes expire after 24 hours"}, {"S2", "S2. Customers see when a quote expires"}} {
		c := f.AddIssue(app, s.title, fmt.Sprintf("The story.\n\n<!-- issuedog:origin plan=#%d rev=1 item=%s -->", n, s.id), "type:story", "stage:planning")
		f.SetParent(c, n)
		f.AddComment(c, app, fmt.Sprintf("Created.\n\n<!-- issuedog:transition from=intake to=planning event=p%d -->", approve))
	}
	f.AddLabel(2, app, "model:opus")
	f.AddLabel(2, app, "effort:high")
	f.AddComment(n, app, "Moved.\n\n<!-- issuedog:transition "+revByOwner+" -->")
	f.RemoveLabel(n, app, "stage:plan-review")
	f.AddLabel(n, app, "stage:planning")
	c := revMove(t, f, n, daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p2"},
		PlanRev: 2, Text: planText(n, 2), Markers: []github.Marker{changedPlanData(t, changedEpicRevision(t))}})
	f.AddReaction(c, "owner", "+1")
	f.ForgetWrites()
}

// changedEpicSetup is changedEpic on a new fake, for Replay.
func changedEpicSetup(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	changedEpic(t, f)
	return f
}

// The owner's 👍 on an epic's revision that changes a story rewrites it,
// "S1. " first in its title, its labels set to the item's, so the model
// and effort revision 1 gave go, and sends it back to planning from
// planning, with a comment naming the revision. The kept S2 gets no
// write. A crash at any write writes nothing twice.
func TestChildrenChangedStory(t *testing.T) {
	t.Parallel()
	githubtest.Replay(t, changedEpicSetup, childrenPass(t, twoStories))
	f := run(t, changedEpicSetup, childrenPass(t, twoStories))
	p, plan := changedApproval(t, f)
	s1 := f.State().Issues[1]
	if s1.Title != "S1. Quotes expire after 12 hours" ||
		!strings.HasPrefix(s1.Body, "<!-- issuedog:rewrite run=p2 -->\n**Parent:** #1   **Priority:** p1\n\n## Outcome\n\n") ||
		!strings.HasSuffix(s1.Body, "\n\nDepends on: nothing.\n\n<!-- issuedog:origin plan=#1 rev=2 item=S1 -->") {
		t.Errorf("S1's child is %q, with body\n%s\nwant revision 2's S1, rewritten whole", s1.Title, s1.Body)
	}
	if want := []string{"priority:p1", "stage:planning", "type:story"}; !reflect.DeepEqual(s1.Labels, want) {
		t.Errorf("S1's child is labelled %v, want %v", s1.Labels, want)
	}
	if got, want := s1.Comments[len(s1.Comments)-1].Body, changedMoveText("S1", plan, "It goes back to `stage:planning`, where the planner plans it again.")+
		fmt.Sprintf("\n\n<!-- issuedog:transition from=planning to=planning event=p%d -->", p); got != want {
		t.Errorf("S1's child's newest comment is\n%q\nwant\n%q", got, want)
	}
	for _, w := range f.Writes() {
		if strings.HasPrefix(w.Path, "/issues/3/") || w.Path == "/issues/3" {
			t.Errorf("#3, the kept S2's child, got the write %+v", w)
		}
	}
}

// A planner run on S1's child that was still waiting to be applied when
// the epic's revision changed S1 is dropped, uncounted, writing nothing:
// the change's move to planning sits above the run's seen, though the
// stage is the same.
func TestChildrenChangedPlannerDropped(t *testing.T) {
	t.Parallel()
	f := changedEpicSetup(t)
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 2)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	changedSaveLease(t, root, 2, s.Comments[len(s.Comments)-1].ID, ivResult(t, plGolden(t, "story-plan")))
	if err := childrenPass(t, twoStories)(f); err != nil {
		t.Fatal(err)
	}
	before, writes := len(f.State().Issues[1].Comments), len(f.Writes())

	g := newRig(t, f, newOrigin(t), root)
	g.planner(true)
	if err := g.r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != writes {
		t.Errorf("resuming the run wrote %+v, want nothing", w[writes:])
	}
	if got := len(f.State().Issues[1].Comments); got != before {
		t.Errorf("S1's child has %d comments, want %d: the run's result dropped", got, before)
	}
	if l := g.lease(); l.RunID != "" || l.Failed {
		t.Errorf("lease %+v, want the run dropped, uncounted", l)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v, want nothing of the dropped run", left)
	}
}

// changedSaveLease leaves the files of run "saved" on issue n at planning
// under root, as a daemon that stopped before applying it would: the
// lease, out, claude's output, and a clone. The run was given comments up
// to seen.
func changedSaveLease(t testing.TB, root string, n int, seen int64, out string) {
	t.Helper()
	l, err := json.Marshal(daemon.Lease{Issue: n, Stage: state.Planning, RunID: "saved", Clone: filepath.Join(root, "runs", "saved"), Seen: seen,
		AttemptKey: fmt.Sprintf("planning-%d", seen), Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"state/lease.json": string(l), "runs/saved/f.txt": "x", "state/runs/saved.json": out} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Through the loop, the pass that approves the revision rewrites the
// changed children, and posts the record of revision 2 (#297); the next
// pass reads them back and writes nothing, and the one after, unchanged,
// sends only conditional GETs that GitHub answers 304.
func TestChildrenChangedQuiet(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	changedStory(t, f, false, nil)
	o := newOrigin(t)
	d := newDaemon(t, f, o, nil)
	d.Step.Reactions = daemon.ApproveByReaction(d.Step.CurrentTrust())
	d.Work = func(context.Context, []daemon.Ready) error { return nil }
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if t2 := f.State().Issues[2]; !slices.Contains(t2.Labels, "stage:ready") || !strings.Contains(t2.Body, "rev=2 item=T2") {
		t.Fatalf("after the approving pass, T2's child is labelled %v, with body\n%s\nwant it rewritten, at ready", t2.Labels, t2.Body)
	}
	n := len(f.Writes())
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != n {
		t.Errorf("the pass after wrote %+v, want nothing", w[n:])
	}
	from, notModified := len(f.Requests()), f.NotModified()
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
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
