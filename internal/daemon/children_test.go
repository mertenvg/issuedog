package daemon_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Creating each child of an approved plan once, linked under its parent
// and at its first stage (#276): ChildrenVisit, after step 1 on the
// parent, in the pass that approves the plan or skips its review.

// childPlan is a parent and the plan it gets: its type, internal/agent's
// golden planner output its plan comes from, with its first items items,
// and whether its repo uses issue types.
type childPlan struct {
	typ, golden string
	items       int
	issueTypes  bool
}

var (
	// twoStories is an epic planned into its first two stories, S1 and S2,
	// on a repo that uses type:* labels.
	twoStories = childPlan{"Epic", "epic-plan", 2, false}
	// threeTasks is a story planned into three tasks, T1 to T3, the last a
	// human task, on a repo that uses issue types.
	threeTasks = childPlan{"Story", "story-plan", 3, true}
)

// planData is the plan-data marker holding k's plan, as the planner's run
// writes it in the plan comment: the plan's JSON, deflated, in base64url
// (planDataMarker in internal/daemon).
func planData(t testing.TB, k childPlan) github.Marker {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "outputs", "planner", k.golden+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Plan map[string]json.RawMessage `json:"plan"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	var items []json.RawMessage
	if err := json.Unmarshal(out.Plan["items"], &items); err != nil {
		t.Fatal(err)
	}
	if out.Plan["items"], err = json.Marshal(items[:k.items]); err != nil {
		t.Fatal(err)
	}
	plan, err := json.Marshal(out.Plan)
	if err != nil {
		t.Fatal(err)
	}
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(plan); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": base64.RawURLEncoding.EncodeToString(z.Bytes())}}
}

// atPlanning files issue 1, the parent, of k's type, at planning, and
// comment 1001, its move there. On a repo that uses issue types, the
// owner set its type, and the org has Task too. Then it runs more, and
// forgets the writes.
func atPlanning(k childPlan, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		if k.issueTypes {
			f.SetTypeBy(1, "owner", k.typ)
			f.IssueTypeID("Task")
		} else {
			f.SetType(1, k.typ)
		}
		f.AddLabel(1, app, "stage:planning")
		post(t, f, 1001, app, "<!-- issuedog:transition from=intake to=planning run=a -->")
		more(t, f)
		f.ForgetWrites()
	})
}

// planOfK posts k's plan, revision 1, on issue 1 as comment 1002, with the
// planner's move to: plan-review, or planned when it skips review.
func planOfK(t testing.TB, f *githubtest.Fake, k childPlan, to state.Stage) {
	t.Helper()
	trigger := state.TriggerPlanValid
	if to == state.Planned {
		trigger = state.TriggerPlanNoReview
	}
	moveOn(t, f, 1, daemon.Move{To: to, Trigger: trigger, Key: state.Key{Field: "run", ID: "p1"}, PlanRev: 1,
		Text: planText(1, 1), Markers: []github.Marker{planData(t, k)}}, 1002)
}

// planAtReview is issue 1 at plan-review, with k's plan in comment 1002,
// then more.
func planAtReview(k childPlan, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return atPlanning(k, func(t testing.TB, f *githubtest.Fake) {
		planOfK(t, f, k, state.PlanReview)
		more(t, f)
	})
}

// thumbsUp is the owner's 👍 on plan comment 1002.
func thumbsUp(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") }

// childrenStep is step 1 with the approval handlers, /hold and /resume,
// and the owner trusted, from a fresh client of f, as the loop's is.
func childrenStep(t *testing.T, f *githubtest.Fake, k childPlan) *daemon.Step {
	tr := trusted{"owner": true}
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug, IssueTypes: k.issueTypes, Notify: []string{"owner"}}
	st := &daemon.Step{Applier: a, Trust: tr, Commands: daemon.Commands(tr), Reactions: daemon.ApproveByReaction(tr)}
	st.Commands["approve"] = daemon.ApproveCommand()
	return st
}

// childrenPass reads issue 1 and runs step 1, then ChildrenVisit, on it,
// as Iterate's visit does.
func childrenPass(t *testing.T, k childPlan) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		st := childrenStep(t, f, k)
		s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		if _, err := st.Run(context.Background(), s); err != nil {
			return err
		}
		return st.ChildrenVisit(context.Background(), s)
	}
}

// planMoveID is the ID of the App's comment on issue 1 that moved it into
// planned with plan revision 1, as a pass left it, read fresh, and when
// it was posted.
func planMoveID(t *testing.T, f *githubtest.Fake) (int64, time.Time) {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if c.Author.Login == app && strings.Contains(c.Body, "to=planned rev=1 ") {
			return c.ID, c.CreatedAt
		}
	}
	t.Fatal("issue 1 has no move into planned with plan revision 1")
	return 0, time.Time{}
}

// wantChild is a child as k's plan files it under issue 1.
type wantChild struct {
	item, title, typ string
	labels           []string
	stage            state.Stage
}

// wantChildren are the children of k's plan, in plan order.
func wantChildren(k childPlan) []wantChild {
	all := map[string][]wantChild{
		"epic-plan": {
			{"S1", "S1. Quotes expire after 24 hours", "", []string{"priority:p1", "stage:planning", "type:story"}, state.Planning},
			{"S2", "S2. Customers see when a quote expires", "", []string{"priority:p2", "stage:planning", "type:story"}, state.Planning},
		},
		"story-plan": {
			{"T1", "Record when each quote expires", "Task", []string{"priority:p2", "scope:booking", "stage:ready"}, state.Ready},
			{"T2", "Refuse an expired quote at booking", "Task", []string{"priority:p1", "scope:booking", "stage:ready"}, state.Ready},
			{"T3", "Set the quote lifetime in production", "Task", []string{"priority:p2", "scope:infra", "stage:needs-human"}, state.NeedsHuman},
		},
	}
	return all[k.golden][:k.items]
}

// firstMove is the comment carrying a child's first move, keyed by the
// parent's move into planned, comment p.
func firstMove(w wantChild, p int64) string {
	where := ""
	if w.stage == state.Planning {
		where = ", where the planner writes its task plan"
	}
	return fmt.Sprintf("Created from #1's plan, rev 1, as %s. It starts at `stage:%s`%s.\n\n"+
		"<!-- issuedog:transition from=intake to=%s event=p%d -->", w.item, w.stage, where, w.stage, p)
}

// checkChildren fails t unless f has exactly k's children, issues 2 on, in
// plan order: each the App's, with its title, type, labels and origin,
// linked under issue 1 in that order, and with its one first move, keyed
// by issue 1's move into planned. The children were looked for in the
// issues listed since that move.
func checkChildren(t *testing.T, f *githubtest.Fake, k childPlan) {
	t.Helper()
	p, at := planMoveID(t, f)
	since := "since=" + url.QueryEscape(at.UTC().Format(time.RFC3339))
	if !slices.ContainsFunc(f.Requests(), func(r githubtest.Request) bool {
		return r.Method == "GET" && r.Path == "/issues" && strings.Contains(r.Query, since)
	}) {
		t.Errorf("no issue list since the move into planned, %s, in %+v", at, f.Requests())
	}
	st := f.State()
	want := wantChildren(k)
	if got := len(st.Issues) - 1; got != len(want) {
		t.Fatalf("%d children filed, want %d: %+v", got, len(want), st.Issues)
	}
	var subs []int
	for i, w := range want {
		subs = append(subs, i+2)
		is := st.Issues[i+1]
		origin := fmt.Sprintf("<!-- issuedog:origin plan=#1 rev=1 item=%s", w.item)
		if is.Author != app || is.Title != w.title || is.Type != w.typ || !reflect.DeepEqual(is.Labels, w.labels) || is.Parent != 1 ||
			!strings.Contains(is.Body, origin) {
			t.Errorf("child #%d is by %s, %q, of type %q, labelled %v, under #%d, with body\n%s\nwant the App's %q, of type %q, labelled %v, under #1, with %s",
				is.Number, is.Author, is.Title, is.Type, is.Labels, is.Parent, is.Body, w.title, w.typ, w.labels, origin)
		}
		var comments []string
		for _, c := range is.Comments {
			comments = append(comments, c.Body)
		}
		if want := []string{firstMove(w, p)}; !reflect.DeepEqual(comments, want) {
			t.Errorf("child #%d's comments are\n%q\nwant\n%q", is.Number, comments, want)
		}
	}
	if got := st.Issues[0].SubIssues; !reflect.DeepEqual(got, subs) {
		t.Errorf("issue 1's sub-issues are %v, want %v, in plan order", got, subs)
	}
}

// noSearch fails t if f was asked anything of the search API.
func noSearch(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	for _, r := range f.Requests() {
		if strings.HasPrefix(r.Path, "/search") {
			t.Errorf("a request went to the search API: %+v", r)
		}
	}
}

// An approval by 👍 or /approve, or a planner's move that skips review,
// creates every item's child in the same pass: with its title, body,
// labels and type, linked under the parent in plan order, and with one
// first move to its first stage. A pass that crashes before or after any
// write, on either kind of repo, leaves the same children when run again,
// and no request goes to the search API.
func TestChildrenCreated(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		k     childPlan
		setup func(testing.TB) *githubtest.Fake
	}{
		{"a 👍 on an epic's plan, on a labels repo", twoStories, planAtReview(twoStories, thumbsUp)},
		{"an /approve of an epic's plan, on a labels repo", twoStories, planAtReview(twoStories, func(t testing.TB, f *githubtest.Fake) {
			post(t, f, 1003, "owner", "/approve")
		})},
		{"a 👍 on a story's plan with a human task, on an issue-types repo", threeTasks, planAtReview(threeTasks, thumbsUp)},
		{"a story's plan that skips review, on an issue-types repo", threeTasks, atPlanning(threeTasks, func(t testing.TB, f *githubtest.Fake) {
			planOfK(t, f, threeTasks, state.Planned)
		})},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			githubtest.Replay(t, tt.setup, childrenPass(t, tt.k))
			f := run(t, tt.setup, childrenPass(t, tt.k))
			checkChildren(t, f, tt.k)
			if got := f.State().Issues[0].Labels; !slices.Contains(got, "stage:planned") {
				t.Errorf("issue 1 is labelled %v, want stage:planned", got)
			}
			noSearch(t, f)
		})
	}
}

// writeAt is the number of the write, counted from 1, that a clean pass
// over a fresh fake from setup makes as the nth that matches, or fails t.
func writeAt(t *testing.T, setup func(testing.TB) *githubtest.Fake, pass func(*githubtest.Fake) error, nth int, match func(githubtest.Write) bool) int {
	t.Helper()
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	for i, w := range f.Writes() {
		if match(w) {
			if nth--; nth == 0 {
				return i + 1
			}
		}
	}
	t.Fatalf("no such write in %+v", f.Writes())
	return 0
}

// isCreate reports whether w files an issue.
func isCreate(w githubtest.Write) bool { return w.Method == "POST" && w.Path == "/issues" }

// isLink reports whether w links a sub-issue.
func isLink(w githubtest.Write) bool {
	return w.Method == "POST" && strings.HasSuffix(w.Path, "/sub_issues")
}

// A held parent gets no children, nor a closed one. Once the hold ends
// back at planned, creation carries on where it stopped: the child filed
// before the hold, which the hold held with its parent, gets no second
// issue or first move.
func TestChildrenHeld(t *testing.T) {
	t.Parallel()
	setup := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	f := setup(t)
	// The first pass approves the plan and files T1, but stops at T2's
	// create.
	f.FailWrite(writeAt(t, setup, pass, 2, isCreate))
	if err := pass(f); err == nil {
		t.Fatal("the first pass made every write; want it stopped at T2's create")
	}
	f.FailWrite(0)
	if got := len(f.State().Issues); got != 2 {
		t.Fatalf("the first pass left %d issues, want the parent and T1", got)
	}

	f.AddComment(1, "owner", "/hold")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if got := st.Issues[0].Labels; !slices.Contains(got, "stage:held") {
		t.Fatalf("issue 1 is labelled %v after /hold, want stage:held", got)
	}
	if got := len(st.Issues); got != 2 {
		t.Errorf("a pass over the held parent left %d issues, want the parent and T1 still", got)
	}
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if got := len(f.State().Issues); got != 2 {
		t.Errorf("a second pass over the held parent left %d issues, want the parent and T1 still", got)
	}

	f.AddComment(1, "owner", "/resume")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	st = f.State()
	if got := len(st.Issues); got != 4 {
		t.Fatalf("after /resume, %d issues, want the parent and its three children", got)
	}
	if got := st.Issues[0].SubIssues; !reflect.DeepEqual(got, []int{2, 3, 4}) {
		t.Errorf("issue 1's sub-issues are %v, want [2 3 4]", got)
	}
	p, _ := planMoveID(t, f)
	for i, w := range wantChildren(threeTasks) {
		is := st.Issues[i+1]
		moves := 0
		for _, c := range is.Comments {
			if strings.Contains(c.Body, "event=p") {
				moves++
				if c.Body != firstMove(w, p) {
					t.Errorf("child #%d's first move is\n%s\nwant\n%s", is.Number, c.Body, firstMove(w, p))
				}
			}
		}
		if moves != 1 {
			t.Errorf("child #%d has %d first moves, want 1: %+v", is.Number, moves, is.Comments)
		}
		if want := "stage:" + string(w.stage); !slices.Contains(is.Labels, want) {
			t.Errorf("child #%d is labelled %v, want %s", is.Number, is.Labels, want)
		}
	}
	n := len(f.Writes())
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != n {
		t.Errorf("a pass once every child is filed wrote %+v, want nothing", w[n:])
	}

	noSearch(t, f)

	// A parent closed once its plan is approved, at planned, gets nothing.
	closed := planAtReview(twoStories, thumbsUp)(t)
	approve := childrenStep(t, closed, twoStories)
	s, err := approve.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approve.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	closed.Close(1)
	closed.ForgetWrites()
	visit := childrenStep(t, closed, twoStories)
	if s, err = visit.Client.Snapshot(context.Background(), "o", "r", 1); err != nil {
		t.Fatal(err)
	}
	if got := state.Derive(s, slug, clean).Stage; got != state.Planned || !s.Closed {
		t.Fatalf("issue 1 is at %s, closed %v; want it closed at planned", got, s.Closed)
	}
	if err := visit.ChildrenVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if w := closed.Writes(); len(w) != 0 || len(closed.State().Issues) != 1 {
		t.Errorf("a visit to the closed parent wrote %+v; want nothing", w)
	}
}

// A child filed just before a crash, but never linked, is found again
// through the issue list on the next pass, with no search request: it's
// linked and moved, with no second issue. Likewise one filed whose answer
// was lost.
func TestChildrenUnlinked(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		for _, k := range []childPlan{twoStories, threeTasks} {
			setup := planAtReview(k, thumbsUp)
			pass := childrenPass(t, k)
			f := setup(t)
			if after {
				// The create takes effect, but its answer is lost.
				f.FailWriteAfter(writeAt(t, setup, pass, 1, isCreate))
			} else {
				f.FailWrite(writeAt(t, setup, pass, 1, isLink))
			}
			if err := pass(f); err == nil {
				t.Fatalf("%s, %v: the first pass made every write; want it stopped", k.golden, after)
			}
			f.FailWrite(0)
			st := f.State()
			if len(st.Issues) != 2 || st.Issues[1].Parent != 0 || len(st.Issues[1].Comments) != 0 {
				t.Fatalf("%s, %v: the first pass left %+v; want one child, unlinked and with no comment", k.golden, after, st.Issues)
			}
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			checkChildren(t, f, k)
			noSearch(t, f)
		}
	}
}

// A found child keeps its labels. A stage change the owner made to it
// meanwhile is recorded as an override, and it gets no first move.
func TestChildrenOverride(t *testing.T) {
	t.Parallel()
	setup := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	f := setup(t)
	// The first pass files and links T1, but stops at its first move.
	f.FailWrite(writeAt(t, setup, pass, 1, isLink) + 1)
	if err := pass(f); err == nil {
		t.Fatal("the first pass made every write; want it stopped at T1's first move")
	}
	f.FailWrite(0)
	f.RemoveLabel(2, "owner", "stage:ready")
	f.AddLabel(2, "owner", "stage:in-progress")
	f.AddLabel(2, "owner", "effort:high")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if len(st.Issues) != 4 {
		t.Fatalf("%d issues, want the parent and its three children", len(st.Issues))
	}
	child := st.Issues[1]
	var comments []string
	for _, c := range child.Comments {
		comments = append(comments, c.Body)
	}
	want := []string{"owner set `stage:in-progress` by hand, so I've recorded it as an owner override.\n\n" +
		"<!-- issuedog:transition from=intake to=in-progress by=owner event=" + overrideEvent(t, f) + " -->"}
	if !reflect.DeepEqual(comments, want) {
		t.Errorf("T1's comments are\n%q\nwant only the override\n%q", comments, want)
	}
	if want := []string{"effort:high", "priority:p2", "scope:booking", "stage:in-progress"}; !reflect.DeepEqual(child.Labels, want) {
		t.Errorf("T1 is labelled %v, want %v, the labels it had", child.Labels, want)
	}
	n := len(f.Writes())
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != n {
		t.Errorf("a pass once every child is filed wrote %+v, want nothing", w[n:])
	}
}

// overrideEvent is the ID of the owner's add of stage:in-progress to issue
// 2, as its timeline gives it.
func overrideEvent(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	events, err := f.Client(t).TimelineSince(context.Background(), "o", "r", 2, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.GetEvent() == "labeled" && e.GetLabel().GetName() == "stage:in-progress" {
			return strconv.FormatInt(e.GetID(), 10)
		}
	}
	t.Fatal("no add of stage:in-progress on issue 2")
	return ""
}

// Through the loop: the pass that approves a plan by 👍 creates its
// children too. Every child a visit writes to skips the rest of that pass,
// step 1 and Work, even one the poll listed and read this pass, and is
// read again on the next; one it only reads doesn't. Here the first pass
// stops at S2's first move, so the second finds S1 done, which it visits,
// and S2, which it moves and doesn't visit until the third. Then a pass
// writes nothing.
func TestIterateCreatesChildren(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetType(f.AddIssue("owner", "Epic", "body"), "Epic")
	f.AddLabel(1, app, "stage:planning")
	post(t, f, 1001, app, "<!-- issuedog:transition from=intake to=planning run=a -->")
	planOfK(t, f, twoStories, state.PlanReview)
	thumbsUp(t, f)
	f.ForgetWrites()

	var visits, worked []int
	d := newDaemon(t, f, newOrigin(t), nil)
	approve := daemon.ApproveByReaction(d.Step.CurrentTrust())
	d.Step.Reactions = func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, dirt daemon.Dirt) error {
		visits = append(visits, s.Number)
		return approve(ctx, a, s, dirt)
	}
	d.Work = func(_ context.Context, ready []daemon.Ready) error {
		for _, r := range ready {
			worked = append(worked, r.S.Number)
		}
		return nil
	}
	pass := func(wantErr bool) {
		t.Helper()
		visits, worked = nil, nil
		if err := d.Iterate(context.Background()); (err != nil) != wantErr {
			t.Fatalf("Iterate: %v, want an error: %v", err, wantErr)
		}
	}

	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "POST" && r.Path == "/issues/3/comments" }, 500)
	pass(true)
	st := f.State()
	if len(st.Issues) != 3 || !slices.Contains(st.Issues[0].Labels, "stage:planned") || len(st.Issues[1].Comments) != 1 ||
		st.Issues[2].Parent != 1 || len(st.Issues[2].Comments) != 0 {
		t.Fatalf("the first pass left %+v; want issue 1 approved, S1 filed under it and moved, and S2 filed under it", st.Issues)
	}
	if !reflect.DeepEqual(visits, []int{1}) || len(worked) != 0 {
		t.Errorf("the first pass visited %v and handed Work %v; want [1], and nothing for the parent whose visit failed", visits, worked)
	}

	pass(false)
	if !reflect.DeepEqual(visits, []int{1, 2}) || !reflect.DeepEqual(worked, []int{1, 2}) {
		t.Errorf("the second pass visited %v and handed Work %v; want the parent and S1, [1 2], not S2, which it moved", visits, worked)
	}
	checkChildren(t, f, twoStories)

	pass(false)
	if !reflect.DeepEqual(visits, []int{3}) || !reflect.DeepEqual(worked, []int{1, 2, 3}) {
		t.Errorf("the third pass visited %v and handed Work %v; want S2 read again and visited, [3], and Work given every issue, [1 2 3]", visits, worked)
	}
	n := len(f.Writes())
	pass(false)
	if w := f.Writes(); len(w) != n {
		t.Errorf("a pass once every child is filed wrote %+v, want nothing", w[n:])
	}
	noSearch(t, f)
}

// Every child the visit files, links or moves is reported to Touched, and
// one it only reads isn't: here T1 was filed, linked and moved before a
// crash, and T2 filed but not linked.
func TestChildrenTouched(t *testing.T) {
	t.Parallel()
	setup := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	f := setup(t)
	f.FailWrite(writeAt(t, setup, pass, 2, isLink))
	if err := pass(f); err == nil {
		t.Fatal("the first pass made every write; want it stopped at T2's link")
	}
	f.FailWrite(0)
	st := childrenStep(t, f, threeTasks)
	var touched []int
	st.Touched = func(n int) {
		if !slices.Contains(touched, n) {
			touched = append(touched, n)
		}
	}
	s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ChildrenVisit(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if want := []int{3, 4}; !reflect.DeepEqual(touched, want) {
		t.Errorf("touched %v, want %v: T2, linked and moved, and T3, filed", touched, want)
	}
	checkChildren(t, f, threeTasks)
}

// A parent the owner moved by hand from planned back to planning, once its
// plan was approved, gets no children of that plan.
func TestChildrenNotPlanned(t *testing.T) {
	t.Parallel()
	f := planAtReview(twoStories, thumbsUp)(t)
	approve := childrenStep(t, f, twoStories)
	s, err := approve.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approve.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	f.RemoveLabel(1, "owner", "stage:planned")
	f.AddLabel(1, "owner", "stage:planning")
	if err := childrenPass(t, twoStories)(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if len(st.Issues) != 1 || !slices.Contains(st.Issues[0].Labels, "stage:planning") {
		t.Errorf("%+v; want issue 1 alone, at planning by the owner's override", st.Issues)
	}
}

// A child found closed, as when someone closed it before its first move,
// gets no first move, and the children after it are still filed.
func TestChildrenClosed(t *testing.T) {
	t.Parallel()
	setup := planAtReview(threeTasks, thumbsUp)
	pass := childrenPass(t, threeTasks)
	f := setup(t)
	// The first pass files and links T1, but stops at its first move.
	f.FailWrite(writeAt(t, setup, pass, 1, isLink) + 1)
	if err := pass(f); err == nil {
		t.Fatal("the first pass made every write; want it stopped at T1's first move")
	}
	f.FailWrite(0)
	f.CloseBy(2, "owner")
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	st := f.State()
	if len(st.Issues) != 4 || !reflect.DeepEqual(st.Issues[0].SubIssues, []int{2, 3, 4}) {
		t.Fatalf("%+v; want the parent and its three children, linked", st.Issues)
	}
	if c := st.Issues[1].Comments; len(c) != 0 {
		t.Errorf("the closed T1 has comments %+v; want none", c)
	}
	p, _ := planMoveID(t, f)
	for i, w := range wantChildren(threeTasks)[1:] {
		is := st.Issues[i+2]
		if len(is.Comments) != 1 || is.Comments[0].Body != firstMove(w, p) {
			t.Errorf("child #%d's comments are %+v, want its first move", is.Number, is.Comments)
		}
	}
}
