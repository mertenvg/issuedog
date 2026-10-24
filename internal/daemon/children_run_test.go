package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is #249's whole run (#279): a run against the fakes, from
// an epic's plan to its stories, a story's plan to its tasks, and the
// story finished once its tasks close. The daemon is built as startDaemon
// builds it, daemon.New, so step 1 is daemon.NewStep's, the Applier reads
// main from the daemon's clone, and the digest and assignment are on. The
// stages are registered as registerStages registers them,
// daemon.RegisterStages, the config's labels exist, as startDaemon creates
// them, and Daemon.Iterate is the loop. Config and trust come from main on
// the origin: mertenvg owns CODEOWNERS and is notify, the scopes are the
// golden plans', and models.allowed is opus and sonnet. plan.review is
// the default, true, so a story's plan waits for review.

// The run's owner and the epic's body.
const (
	childrenRunOwner = "mertenvg"
	childrenRunBody  = "Quotes should expire."
)

// childrenRunConfig is .issuedog/config.yml on main, typing by types:
// labels, or issue-types.
func childrenRunConfig(types string) string {
	return "types: " + types + "\nnotify: [" + childrenRunOwner + "]\nscopes: [booking, infra, docs]\nmodels: {allowed: [opus, sonnet]}\n"
}

// childrenRunLoop is a whole run's loop on f: a rig on the state
// directory root, rebuilt by start as a restart builds the daemon, with
// main's config cfg. seen is each issue's comment count before the last
// pass, for added.
type childrenRunLoop struct {
	t    *testing.T
	f    *githubtest.Fake
	o    *origin
	root string
	cfg  string
	g    *rig
	seen map[int]int
}

// newChildrenRunLoop starts a loop on f, on a new origin, with main's
// config typing by types, and a new state directory.
func newChildrenRunLoop(t *testing.T, f *githubtest.Fake, types string) *childrenRunLoop {
	t.Helper()
	l := &childrenRunLoop{t: t, f: f, o: newOrigin(t), root: t.TempDir(), cfg: childrenRunConfig(types)}
	l.o.commit(".github/CODEOWNERS", "* @"+childrenRunOwner+"\n")
	l.o.commit(config.Path, l.cfg)
	l.start()
	return l
}

// start starts a daemon on l's fake, as a start of the issuedog binary
// does: a new client, with no ETags, a new daemon, with no snapshots, and
// the same state directory, which outlives a restart. As startDaemon does,
// it loads the config and creates its labels before the loop, and builds
// the daemon with daemon.New (rig.wireAsMain). The Stages are
// registerStages', daemon.RegisterStages. Iterate loads the trust from
// main.
func (l *childrenRunLoop) start() {
	l.t.Helper()
	g := newRig(l.t, l.f, l.o, l.root)
	cfg, err := config.Parse([]byte(l.cfg))
	if err != nil {
		l.t.Fatal(err)
	}
	if _, err := github.EnsureLabels(context.Background(), l.f.Client(l.t), "o", "r", daemon.LabelOptions(cfg)); err != nil {
		l.t.Fatal(err)
	}
	// wholeRunClaude keeps each run's context and prints out.<n>, and fails
	// a run it has no output for, so an unexpected run shows.
	if err := os.WriteFile(g.path("claude", "claude"), []byte(wholeRunClaude), 0o755); err != nil {
		l.t.Fatal(err)
	}
	// Run IDs go on across a restart, as the daemon's random ones never
	// repeat: run<k> is the fake claude's run k.
	g.r.NewID = func() string { return "run" + strconv.Itoa(g.runs()+1) }
	g.wireAsMain(cfg)
	l.g = g
}

// outs sets what the fake claude prints on its next runs, one each, from
// the run after the last it ran.
func (l *childrenRunLoop) outs(outs ...map[string]any) {
	l.t.Helper()
	for i, out := range outs {
		if err := os.WriteFile(l.g.path("claude", "out."+strconv.Itoa(l.g.runs()+1+i)), []byte(ivResult(l.t, out)), 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
}

// step runs one Iterate, which must succeed, and checks that it wrote
// want, as "METHOD /path" in order, and that the fake claude has run runs
// times in all.
func (l *childrenRunLoop) step(name string, runs int, want ...string) {
	l.t.Helper()
	if err := l.iterate(name, runs, want...); err != nil {
		l.t.Fatalf("%s: Iterate: %v", name, err)
	}
}

// crash is step for a pass whose write number fail since the pass began,
// 1-based, takes effect but loses its answer, as when the daemon dies just
// after GitHub applied it: the pass must fail with that write's error.
func (l *childrenRunLoop) crash(name string, fail, runs int, want ...string) {
	l.t.Helper()
	l.f.FailWriteAfter(len(l.f.Writes()) + fail)
	err := l.iterate(name, runs, want...)
	l.f.FailWriteAfter(0)
	if err == nil || !strings.Contains(err.Error(), "FailWriteAfter") {
		l.t.Fatalf("%s: Iterate = %v; want write %d's lost answer", name, err, fail)
	}
}

// iterate runs one Iterate, checks its writes and runs as step does, and
// returns its error.
func (l *childrenRunLoop) iterate(name string, runs int, want ...string) error {
	l.t.Helper()
	l.seen = map[int]int{}
	for _, is := range l.f.State().Issues {
		l.seen[is.Number] = len(is.Comments)
	}
	from := len(l.f.Writes())
	err := l.g.d.Iterate(context.Background())
	if got := digestWrites(l.f, from); !slices.Equal(got, want) {
		l.t.Errorf("%s: %d writes %q; want %d: %q", name, len(got), got, len(want), want)
	}
	if got := l.g.runs(); got != runs {
		l.t.Fatalf("%s: %d claude runs in all; want %d", name, got, runs)
	}
	return err
}

// quiet runs two Iterates that must write nothing and start no run: the
// first reads back the App's own writes of the step before, and the
// second, an unchanged pass, must send only conditional GETs that GitHub
// answers 304, so it costs no rate limit.
func (l *childrenRunLoop) quiet(name string) {
	l.t.Helper()
	runs := l.g.runs()
	l.step(name+", then an extra pass", runs)
	from, notModified := len(l.f.Requests()), l.f.NotModified()
	l.step(name+", unchanged", runs)
	for _, r := range l.f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			l.t.Errorf("%s: an unchanged pass sent %+v; want only conditional GETs answered 304", name, r)
		}
	}
	if l.f.NotModified() == notModified {
		l.t.Errorf("%s: an unchanged pass got no 304", name)
	}
}

// restart starts a new daemon on the same fake and state directory, whose
// first pass, which reads every issue, and its second write nothing and
// start no run.
func (l *childrenRunLoop) restart(name string) {
	l.t.Helper()
	l.start()
	runs := l.g.runs()
	l.step(name+": a restart", runs)
	l.step(name+": the restart's second pass", runs)
}

// added is the bodies of the comments on issue n since the last pass began.
func (l *childrenRunLoop) added(n int) []string {
	var bodies []string
	for _, c := range l.f.State().Issues[n-1].Comments[l.seen[n]:] {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// comments are issue n's comments, with their IDs.
func (l *childrenRunLoop) comments(n int) []github.Comment {
	l.t.Helper()
	s, err := l.f.Client(l.t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		l.t.Fatal(err)
	}
	return s.Comments
}

// labels checks issue n's labels, sorted, and whether it's closed.
func (l *childrenRunLoop) labels(step string, n int, closed bool, want ...string) {
	l.t.Helper()
	if got, is := ivLabels(l.f, n), l.f.State().Issues[n-1]; !slices.Equal(got, want) || is.Closed != closed {
		l.t.Errorf("%s: #%d is labelled %v, closed %t; want %v, closed %t", step, n, got, is.Closed, want, closed)
	}
}

// digest checks the digest's items, issue d, as digestRunItems gives them,
// and who is assigned, by issue: an issue assigned leaves out has no one.
func (l *childrenRunLoop) digest(step string, d int, items []string, assigned ...int) {
	l.t.Helper()
	if got := digestRunItems(l.f.State().Issues[d-1].Body); !slices.Equal(got, items) {
		l.t.Errorf("%s: the digest's items %q; want %q", step, got, items)
	}
	want := map[int][]string{}
	for _, n := range assigned {
		want[n] = []string{childrenRunOwner}
	}
	assignWant(l.t, l.f, step, want)
}

// planned checks that issue n's newest comment is the App's plan
// revision 1, posted at plan-review by claude run k, which asks for review,
// and returns it.
func (l *childrenRunLoop) planned(step string, n, k int) github.Comment {
	l.t.Helper()
	cs := l.comments(n)
	c := cs[len(cs)-1]
	move := fmt.Sprintf("\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=run%d ", k)
	if c.Author.Login != app || !strings.HasPrefix(c.Body, fmt.Sprintf("### Plan for #%d (rev 1)\n\n", n)) || !strings.Contains(c.Body, move) ||
		!strings.Contains(c.Body, "\n\n**To approve this plan, react 👍 to this comment.**") || !strings.Contains(c.Body, "\n<!-- issuedog:plan-data ") ||
		!strings.HasSuffix(c.Body, fmt.Sprintf("\n<!-- issuedog:revision kind=plan rev=1 body=%s -->", childrenRunPlanHash(c.Body))) {
		l.t.Errorf("%s: #%d's newest comment, by %s, =\n%s\nwant the App's plan revision 1 from run %d, asking for review", step, n, c.Author.Login, c.Body, k)
	}
	return c
}

// childrenRunPlanHash is the hash of a plan comment's text above its last line.
func childrenRunPlanHash(body string) string {
	return state.BodyHash(body[:max(strings.LastIndex(body, "\n"), 0)])
}

// childrenRunEpicPlan is internal/agent's golden epic plan, cut to its
// first two stories: S1, at p1, with model opus and effort high, and S2,
// which depends on S1.
func childrenRunEpicPlan(t *testing.T) map[string]any {
	t.Helper()
	out := plGolden(t, "epic-plan")
	plan := childrenRunPlanOf(t, out)
	plan["items"] = []any{plItem(t, out, 0), plItem(t, out, 1)}
	plan["order"] = "S1, then S2"
	plan["summary"] = "**Today** a quote never expires.\n\nThis plan adds expiry at booking (S1), then the expiry time on every quote a customer sees (S2)."
	return out
}

// childrenRunStoryPlan is internal/agent's golden story plan, cut to T1, a
// task with model sonnet and effort medium, and its human task, T3,
// renumbered T2, which depends on T1.
func childrenRunStoryPlan(t *testing.T) map[string]any {
	t.Helper()
	out := plGolden(t, "story-plan")
	plan := childrenRunPlanOf(t, out)
	human := plItem(t, out, 2)
	human["id"], human["depends_on"] = "T2", []any{"T1"}
	plan["items"] = []any{plItem(t, out, 0), human}
	plan["order"] = "T1, then T2"
	plan["summary"] = "**Today** nothing reads a quote's age.\n\nThis plan adds each quote's expiry time (T1), then the lifetime set in production (T2)."
	return out
}

// childrenRunPlanOf is out's plan.
func childrenRunPlanOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("output %v has no plan", out)
	}
	return plan
}

// childrenRunAtPlanning files mertenvg's issue "Quote expiry" with
// labels, at planning, as the App moved it there: revision 1 of its spec,
// then its approval. It returns its number.
func childrenRunAtPlanning(f *githubtest.Fake, labels ...string) int {
	n := f.AddIssue(childrenRunOwner, "Quote expiry", childrenRunBody, labels...)
	f.AddLabel(n, app, "stage:planning")
	rev := f.AddComment(n, app, revision(1, childrenRunBody))
	f.AddComment(n, app, fmt.Sprintf("Revision 1 of the spec is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=spec-review to=planning rev=1 body=%s cmd=r9 seen=%d -->", childrenRunOwner, state.BodyHash(childrenRunBody), rev))
	return n
}

// TestChildrenRun is the whole run (#279's acceptance criteria). The fake
// holds mertenvg's epic, #1, at planning, its spec approved. The first
// pass creates the digest, #2, and the planner posts the epic's plan of
// two stories, S2 depending on S1, at plan-review. Then:
//   - mertenvg's 👍 approves it, and the stories are filed as #3 and #4,
//     through two crashes: just after S1's create, and just after its
//     link, each followed by a restart that writes only what was missing;
//   - the planner runs on S1, whose plan of two tasks waits at
//     plan-review, then on S2, without waiting for S1's review;
//   - mertenvg's /approve on S1's plan files T1 at ready, with its model
//     and effort, and the human task T2 at needs-human, blocked by T1;
//   - mertenvg closes both tasks, and S1 is listed as a finished story,
//     and assigned, till he closes it.
//
// The writes are checked after every pass, every unchanged pass costs no
// rate limit, and a restart at the end writes nothing.
func TestChildrenRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(childrenRunOwner, "admin")
	epic := childrenRunAtPlanning(f, "type:epic")
	l := newChildrenRunLoop(t, f, "labels")
	l.outs(childrenRunEpicPlan(t))

	// 1. The first pass creates and pins the digest, and the planner posts
	// the epic's plan revision 1 at plan-review, which the pass after lists
	// and assigns.
	l.step("the epic's plan", 1, "POST /issues", "POST /graphql", "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	plan := l.planned("the epic's plan", epic, 1)
	l.step("the epic's plan listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")
	l.digest("the epic's plan listed", 2, []string{"#1 Plan approval p2 1"}, epic)
	l.quiet("the epic's plan")

	// 2. mertenvg's 👍 approves the plan. The same pass files S1, #3, but
	// dies just after its create: the issue is there, unlinked, with no
	// first move. The epic's visit failed, so the digest and assignment
	// stay as they were.
	up := f.AddReaction(plan.ID, childrenRunOwner, "+1")
	upFrom := len(f.Requests())
	l.crash("the approval, crashing after S1's create", 4, 1,
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review", "POST /issues")
	approval := fmt.Sprintf("Revision 1 of the plan is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=r%d seen=%d -->", childrenRunOwner, childrenRunPlanHash(plan.Body), up, plan.ID)
	if added := l.added(epic); !slices.Equal(added, []string{approval}) {
		t.Errorf("the approval: added %q; want\n%s", added, approval)
	}
	if st := f.State(); len(st.Issues) != 3 || st.Issues[2].Parent != 0 || len(st.Issues[2].Comments) != 0 || len(st.Issues[0].SubIssues) != 0 {
		t.Fatalf("the approval: %d issues, #3 under #%d with comments %+v; want S1 alone, unlinked, with none", len(st.Issues), st.Issues[2].Parent, st.Issues[2].Comments)
	}
	l.digest("the approval", 2, []string{"#1 Plan approval p2 1"}, epic)
	approvedBy := l.comments(epic)[len(l.comments(epic))-1]

	// 3. A restart finds S1 in the issues listed since the approval, and
	// links it, but dies just after the link: S1 is under the epic, with no
	// first move, and no second issue was filed. The new daemon's digest,
	// built afresh, drops the epic's item, as the epic waits on no one at
	// planned, but its assignment stays, as its visit failed (SPEC §9.6).
	l.start()
	l.crash("the first restart, crashing after S1's link", 1, 1, "POST /issues/1/sub_issues", "PATCH /issues/2")
	l.digest("the first restart", 2, nil, epic)
	if st := f.State(); len(st.Issues) != 3 || st.Issues[2].Parent != epic || len(st.Issues[2].Comments) != 0 {
		t.Fatalf("the first restart: %d issues, #3 under #%d with comments %+v; want S1 alone, linked, with none", len(st.Issues), st.Issues[2].Parent, st.Issues[2].Comments)
	}

	// 4. A second restart finds S1 among the epic's sub-issues, and writes
	// only what's missing: S1's first move, then S2, #4, filed, linked and
	// moved, S2's blocked-by link to S1 and the record, last. Then
	// mertenvg is unassigned from the epic.
	l.start()
	l.step("the second restart", 1, slices.Concat([]string{"POST /issues/3/comments"}, childWrites(epic, 4), blockWrites(4),
		[]string{recordWrite(epic), "DELETE /issues/1/assignees"})...)
	stories := []wantChild{
		{"S1", "S1. Quotes expire after 24 hours", "", []string{"effort:high", "model:opus", "priority:p1", "stage:planning", "type:story"}, state.Planning},
		{"S2", "S2. Customers see when a quote expires", "", []string{"priority:p2", "stage:planning", "type:story"}, state.Planning},
	}
	runChildren(t, f, "the second restart", epic, 1, approvedBy.ID, 3, stories)
	runRecord(t, f, "the second restart", epic, 1, 3, stories, map[int][]int{4: {3}})
	if !childrenLooked(f, upFrom, approvedBy.CreatedAt) {
		t.Errorf("the restarts didn't look for the epic's children in the issues listed since its approval")
	}
	for _, r := range f.Requests() {
		if strings.HasPrefix(r.Path, "/search") {
			t.Errorf("a pass searched: %+v", r)
		}
	}
	s2Body := "**Parent:** #1   **Priority:** p2\n\n## Outcome\n\nEvery quote a customer sees shows the time it expires.\n\n" +
		"## Acceptance criteria\n\n- A new quote in the PWA shows its expiry time in the customer's time zone.\n" +
		"- A quote from the MCP server includes its expiry time.\n- A quote shown after it expired says it has expired and offers a new quote.\n\n" +
		"Depends on: S1.\n\n<!-- issuedog:origin plan=#1 rev=1 item=S2 -->"
	if got := f.State().Issues[3].Body; got != s2Body {
		t.Errorf("the second restart: S2's body =\n%s\nwant\n%s", got, s2Body)
	}
	l.labels("the second restart", epic, false, "stage:planned", "type:epic")
	l.digest("the second restart", 2, nil)

	// 5. The planner runs on each story in turn, one a pass, as the stories
	// skipped the rest of the pass that filed them. S1's model:* and
	// effort:* labels set its run's --model and --effort (SPEC §3.3). Its
	// plan of two tasks waits at plan-review, and the pass after lists it
	// and assigns S1, while the planner runs on S2, which has no override
	// labels, without waiting for S1's review. The pass after that lists
	// S2's plan too. The epic makes no request for creation: its record
	// stands (#277).
	storyPlan := childrenRunStoryPlan(t)
	l.outs(storyPlan, storyPlan)
	from := len(f.Requests())
	l.step("S1's plan", 2, "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:planning")
	s1Plan := l.planned("S1's plan", 3, 2)
	l.g.checkFlags(2, "opus", "high", "")
	l.step("S2's plan", 3, "PATCH /issues/2", "POST /issues/3/assignees",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:planning")
	l.planned("S2's plan", 4, 3)
	l.g.checkFlags(3, "", "", "")
	l.step("S2's plan listed", 3, "PATCH /issues/2", "POST /issues/4/assignees")
	// S1 blocks S2, so it scores 2 (SPEC §9.4, The order).
	plans := []string{"#3 Plan approval p1 2", "#4 Plan approval p2 1"}
	l.digest("S2's plan listed", 2, plans, 3, 4)
	l.labels("S2's plan listed", 3, false, "effort:high", "model:opus", "priority:p1", "stage:plan-review", "type:story")
	l.labels("S2's plan listed", 4, false, "priority:p2", "stage:plan-review", "type:story")
	recordStands(t, f, "the stories' plans", from, epic, approvedBy.CreatedAt, []int{4}, false)
	l.quiet("the stories' plans")

	// 6. mertenvg's /approve on S1 approves its plan, with a 👍, and the
	// same pass files its tasks as #5 and #6, in plan order, each created,
	// linked under S1 and moved, keyed by the approval: T1 at ready, with
	// its item's model:* and effort:* labels, and the human task T2 at
	// needs-human, with its §9.3 body and cc line. Then T2 is blocked by
	// T1, and the record goes on S1, last. Then S1's plan leaves the
	// digest, and S1 is unassigned. T2, blocked, isn't listed.
	approve := f.AddComment(3, childrenRunOwner, "/approve")
	from = len(f.Requests())
	l.step("S1's /approve", 3, slices.Concat(
		[]string{"POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:plan-review", discRunReact(approve)},
		childWrites(3, 5, 6), blockWrites(6), []string{recordWrite(3), "PATCH /issues/2", "DELETE /issues/3/assignees"})...)
	tasks := []wantChild{
		{"T1", "Record when each quote expires", "", []string{"effort:medium", "model:sonnet", "priority:p1", "scope:booking", "stage:ready", "type:task"}, state.Ready},
		{"T2", "Set the quote lifetime in production", "", []string{"priority:p1", "scope:infra", "stage:needs-human", "type:task"}, state.NeedsHuman},
	}
	s1Approval := fmt.Sprintf("Revision 1 of the plan is approved, by %s's `/approve`.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=%d seen=%d -->", childrenRunOwner, childrenRunPlanHash(s1Plan.Body), approve, approve)
	if added, want := l.added(3), []string{s1Approval, recordOf(1, 5, tasks)}; !slices.Equal(added, want) {
		t.Errorf("S1's /approve: added %q; want the approval, then the record:\n%q", added, want)
	}
	cs := l.comments(3)
	runChildren(t, f, "S1's /approve", 3, 1, cs[len(cs)-2].ID, 5, tasks)
	runRecord(t, f, "S1's /approve", 3, 1, 5, tasks, map[int][]int{6: {5}})
	if !childrenLooked(f, from, cs[len(cs)-2].CreatedAt) {
		t.Errorf("S1's /approve: the pass didn't look for S1's children in the issues listed since its approval")
	}
	if got := f.State().Issues[4].Body; !strings.HasPrefix(got, "**Parent:** #3   **Scope:** scope:booking   **Priority:** p1\n\n## Context\n\n- `internal/booking/quote.go`: ") {
		t.Errorf("S1's /approve: T1's body =\n%s\nwant the §11.2 task, naming its files in its Context", got)
	}
	t2Body := "**Parent:** #3   **Scope:** scope:infra   **Priority:** p1\n\n" +
		"**Goal:** Set the quote lifetime to 24 hours in production's settings.\n" +
		"**Why it blocks:** #3 can't proceed until production has a quote lifetime, because the booking service won't start without one, and the deploy settings are a path only the owner can change.\n" +
		"**Steps:**\n1. On a new branch, open `deploy/production.env`.\n2. Add the line `QUOTE_TTL=24h`.\n3. Open a PR, check its diff is only that line, and merge it into `main`.\n\n" +
		"**Links:**\n- https://github.com/acme/booking/blob/main/deploy/production.env\n\n" +
		"**Done when:** The file `deploy/production.env` on `main` contains `QUOTE_TTL=24h`. I check this when you reply `/done` or react 👍 to this issue.\n" +
		"**Estimated time:** 5 min\n\ncc @" + childrenRunOwner + "\n\n<!-- issuedog:origin plan=#3 rev=1 item=T2" + planRunHumanOrigin + " -->"
	if got := f.State().Issues[5].Body; got != t2Body {
		t.Errorf("S1's /approve: T2's body =\n%s\nwant\n%s", got, t2Body)
	}
	l.labels("S1's /approve", 3, false, "effort:high", "model:opus", "priority:p1", "stage:planned", "type:story")
	l.digest("S1's /approve", 2, plans[1:], 4)
	l.quiet("the tasks")

	// 7. mertenvg closes T1 and T2. The pass that judges the closes, the
	// newest first, sets stage:completed on each, and, as every child S1's
	// record names has closed, lists S1 as a finished story, keyed done-3,
	// ranked after every other kind of item of the same score, but first
	// here, as it blocks S2, announces it once and assigns mertenvg on it
	// (#278).
	f.CloseBy(5, childrenRunOwner)
	f.CloseBy(6, childrenRunOwner)
	l.step("the tasks closed", 3, "POST /issues/6/labels", "DELETE /issues/6/labels/stage:needs-human",
		"POST /issues/5/labels", "DELETE /issues/5/labels/stage:ready", "POST /issues/2/comments", "PATCH /issues/2", "POST /issues/3/assignees")
	l.labels("the tasks closed", 5, true, "effort:medium", "model:sonnet", "priority:p1", "scope:booking", "stage:completed", "type:task")
	l.labels("the tasks closed", 6, true, "priority:p1", "scope:infra", "stage:completed", "type:task")
	news := "New on the digest, waiting on you:\n\n" +
		"- **Finished story** on #3: S1. Quotes expire after 24 hours. Answer here: https://github.com/o/r/issues/3\n\n" +
		"cc @" + childrenRunOwner + "\n\n<!-- issuedog:digest-new items=done-3 -->"
	if added := l.added(2); !slices.Equal(added, []string{news}) {
		t.Errorf("the tasks closed: the digest's new comments %q; want the announcement\n%s", added, news)
	}
	finished := "**Finished story**, priority p1, unblocks 2 open issues (itself included)\n\n" +
		"- Every child of this story is closed, so it waits for story verification, which runs by hand for now. Close it once that passes.\n" +
		"- Its children: #5, #6.\n"
	if body := f.State().Issues[1].Body; !strings.Contains(body, finished) {
		t.Errorf("the tasks closed: the digest =\n%s\nwant S1's item\n%s", body, finished)
	}
	done := append([]string{"#3 Finished story p1 2"}, plans[1:]...)
	l.digest("the tasks closed", 2, done, 3, 4)
	l.quiet("S1 finished")
	l.restart("S1 finished")
	l.digest("S1 finished: the restart", 2, done, 3, 4)

	// 8. mertenvg closes S1, as its item asks once story verification
	// passes. The close judge sets stage:completed, and S1's item ends. Its
	// assignee stays, as on any closed issue (SPEC §9.6).
	f.CloseBy(3, childrenRunOwner)
	l.step("S1 closed", 3, "POST /issues/3/labels", "DELETE /issues/3/labels/stage:planned", "PATCH /issues/2")
	l.labels("S1 closed", 3, true, "effort:high", "model:opus", "priority:p1", "stage:completed", "type:story")
	l.digest("S1 closed", 2, plans[1:], 3, 4)
	l.quiet("S1 closed")

	// 9. A restart writes nothing and starts no run. Each child was filed
	// once, and the digest announced the finished story once.
	l.restart("the end")
	if n, news := len(f.State().Issues), digestComments(f, 2); n != 6 || len(news) != 1 {
		t.Errorf("the end: %d issues, the digest's comments %q; want the epic, the digest, 2 stories and 2 tasks, and one announcement", n, news)
	}
	for i := range tasks { // closed, as T1 and T2 are now
		tasks[i].labels = slices.Concat(slices.DeleteFunc(slices.Clone(tasks[i].labels), func(l string) bool { return strings.HasPrefix(l, "stage:") }), []string{"stage:completed"})
		slices.Sort(tasks[i].labels)
	}
	runChildren(t, f, "the end", 3, 1, cs[len(cs)-2].ID, 5, tasks)
	runRecord(t, f, "the end", 3, 1, 5, tasks, map[int][]int{6: {5}})
	runRecord(t, f, "the end", epic, 1, 3, stories, map[int][]int{4: {3}})
}

// TestChildrenRunIssueTypes is the shorter run on an origin whose main
// types by issue type (types: issue-types): mertenvg's story, #1, typed
// Story by him, at planning, its spec approved, gets its plan from the
// planner, at plan-review, and his /approve files its tasks, #3 and #4,
// with the issue type Task and no type label, linked under it and moved,
// T2 blocked by T1, and the record. A restart writes nothing.
func TestChildrenRunIssueTypes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(childrenRunOwner, "admin")
	story := childrenRunAtPlanning(f)
	f.SetTypeBy(story, childrenRunOwner, "Story")
	f.IssueTypeID("Task")
	l := newChildrenRunLoop(t, f, "issue-types")
	l.outs(childrenRunStoryPlan(t))

	// 1. The first pass creates the digest, #2, and the planner posts the
	// story's plan at plan-review, which the pass after lists and assigns.
	l.step("the story's plan", 1, "POST /issues", "POST /graphql", "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	plan := l.planned("the story's plan", story, 1)
	l.step("the story's plan listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")
	l.digest("the story's plan listed", 2, []string{"#1 Plan approval p2 1"}, story)
	l.quiet("the story's plan")

	// 2. mertenvg's /approve files T1 and T2, each of type Task, with no
	// type label, at the story's p2, then T2's blocked-by link to T1 and
	// the record, and ends the story's item and assignment.
	approve := f.AddComment(story, childrenRunOwner, "/approve")
	l.step("the /approve", 1, slices.Concat(
		[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review", discRunReact(approve)},
		childWrites(story, 3, 4), blockWrites(4), []string{recordWrite(story), "PATCH /issues/2", "DELETE /issues/1/assignees"})...)
	tasks := []wantChild{
		{"T1", "Record when each quote expires", "Task", []string{"effort:medium", "model:sonnet", "priority:p2", "scope:booking", "stage:ready"}, state.Ready},
		{"T2", "Set the quote lifetime in production", "Task", []string{"priority:p2", "scope:infra", "stage:needs-human"}, state.NeedsHuman},
	}
	approval := fmt.Sprintf("Revision 1 of the plan is approved, by %s's `/approve`.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=%d seen=%d -->", childrenRunOwner, childrenRunPlanHash(plan.Body), approve, approve)
	if added, want := l.added(story), []string{approval, recordOf(1, 3, tasks)}; !slices.Equal(added, want) {
		t.Errorf("the /approve: added %q; want the approval, then the record:\n%q", added, want)
	}
	cs := l.comments(story)
	runChildren(t, f, "the /approve", story, 1, cs[len(cs)-2].ID, 3, tasks)
	runRecord(t, f, "the /approve", story, 1, 3, tasks, map[int][]int{4: {3}})
	l.labels("the /approve", story, false, "stage:planned")
	l.digest("the /approve", 2, nil)
	l.quiet("the tasks")
	l.restart("the end")
	runChildren(t, f, "the end", story, 1, cs[len(cs)-2].ID, 3, tasks)
}
