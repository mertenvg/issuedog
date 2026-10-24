package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"reflect"
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

// This file is #272: a whole run of plan review against
// the fakes, through the real Planner at planning, with the stages
// registered as registerStages registers them, the daemon built as
// startDaemon builds it, daemon.New, so step 1 is daemon.NewStep's and the
// digest and assignment are on, and Daemon.Iterate as the loop. Config and trust come from main on
// the origin: mertenvg owns CODEOWNERS and is notify, mallory is a write
// collaborator CODEOWNERS doesn't trust, and helper[bot] is another App.
// Then the cases #268's verify left to this run (on #272), each on a loop
// of its own.

// The run's people and text.
const (
	planReviewRunOwner = "mertenvg"
	planReviewRunBot   = "helper[bot]"
	planReviewRunBody  = "Quotes should expire."
	planReviewRunNotes = "Split S1: refuse expired quotes in the API first, then show it in the PWA."
)

// planReviewRunLoop is a whole run's loop on f: a rig on the state
// directory root, rebuilt by start as a restart builds the daemon. seen is
// each issue's comment count before the last step, for added.
type planReviewRunLoop struct {
	t    *testing.T
	f    *githubtest.Fake
	o    *origin
	root string
	g    *rig
	seen map[int]int
}

// planReviewRunOrigin is main with mertenvg in CODEOWNERS and as notify,
// typing by label, with the scopes the golden plans use.
func planReviewRunOrigin(t testing.TB) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+planReviewRunOwner+"\n")
	o.commit(config.Path, "types: labels\nnotify: ["+planReviewRunOwner+"]\nscopes: [booking, infra, docs]\n")
	return o
}

// newPlanReviewRunLoop starts a loop on f, on a new origin and state
// directory, whose fake claude prints outs, in order, one a run.
func newPlanReviewRunLoop(t *testing.T, f *githubtest.Fake, outs ...map[string]any) *planReviewRunLoop {
	t.Helper()
	l := &planReviewRunLoop{t: t, f: f, o: planReviewRunOrigin(t), root: t.TempDir()}
	l.start()
	l.outs(outs...)
	return l
}

// start starts a daemon on l's fake, as a restart does: a new client, with
// no ETags, a new daemon, with no snapshots, and the same state directory,
// which outlives a restart. The stages are registered as registerStages
// registers them: the Interviewer at clarifying, the Planner at planning,
// and Triage as the Unlabeled stage, daemon.RegisterStages'. The daemon is
// built as startDaemon builds it, daemon.New (rig.wireAsMain), so step 1
// is daemon.NewStep's: every command handler, and the reaction triggers in
// its order, the end of a discussion hold first. Iterate loads the trust
// from main.
func (l *planReviewRunLoop) start() {
	l.t.Helper()
	g := newRig(l.t, l.f, l.o, l.root)
	// wholeRunClaude keeps each run's context and prints out.<n>, and fails
	// a run it has no output for, so an unexpected run shows.
	if err := os.WriteFile(g.path("claude", "claude"), []byte(wholeRunClaude), 0o755); err != nil {
		l.t.Fatal(err)
	}
	g.wireAsMain(g.d.Config)
	l.g = g
}

// outs sets what the fake claude prints on its next runs, one each, from
// the run after the last it has an output for.
func (l *planReviewRunLoop) outs(outs ...map[string]any) {
	l.t.Helper()
	n := 1
	for {
		if _, err := os.Stat(l.g.path("claude", "out."+strconv.Itoa(n))); err != nil {
			break
		}
		n++
	}
	for i, out := range outs {
		if err := os.WriteFile(l.g.path("claude", "out."+strconv.Itoa(n+i)), []byte(ivResult(l.t, out)), 0o644); err != nil {
			l.t.Fatal(err)
		}
	}
}

// mark keeps each issue's comment count, for added.
func (l *planReviewRunLoop) mark() {
	l.seen = map[int]int{}
	for _, is := range l.f.State().Issues {
		l.seen[is.Number] = len(is.Comments)
	}
}

// added is the bodies of the comments on issue n since the last step began.
func (l *planReviewRunLoop) added(n int) []string {
	var bodies []string
	for _, c := range l.f.State().Issues[n-1].Comments[l.seen[n]:] {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// step runs one Iterate, which must succeed, and checks that it wrote
// want, as "METHOD /path" in order, and that the fake claude has run runs
// times in all.
func (l *planReviewRunLoop) step(name string, runs int, want ...string) {
	l.t.Helper()
	l.mark()
	from := len(l.f.Writes())
	if err := l.g.d.Iterate(context.Background()); err != nil {
		l.t.Fatalf("%s: Iterate: %v", name, err)
	}
	if got := digestWrites(l.f, from); !slices.Equal(got, want) {
		l.t.Errorf("%s: %d writes %q; want %d: %q", name, len(got), got, len(want), want)
	}
	if got := l.g.runs(); got != runs {
		l.t.Fatalf("%s: %d claude runs in all; want %d", name, got, runs)
	}
}

// quiet runs two Iterates that must write nothing and start no run: the
// first reads back the App's own writes of the step before, and the
// second, an unchanged pass, must ask only conditional GETs that GitHub
// answers 304, so it costs no rate limit.
func (l *planReviewRunLoop) quiet(name string) {
	l.t.Helper()
	runs := l.g.runs()
	l.step(name+", then an extra pass", runs)
	from := len(l.f.Requests())
	l.step(name+", unchanged", runs)
	for _, r := range l.f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			l.t.Errorf("%s: an unchanged pass sent %+v; want only conditional GETs answered 304", name, r)
		}
	}
	if l.f.NotModified() == 0 {
		l.t.Errorf("%s: an unchanged pass got no 304", name)
	}
}

// restart starts a new daemon on the same fake and state directory, whose
// first pass, which reads every issue, and its second write nothing and
// start no run.
func (l *planReviewRunLoop) restart(name string) {
	l.t.Helper()
	l.start()
	runs := l.g.runs()
	l.step(name+": a restart", runs)
	l.step(name+": the restart's second pass", runs)
}

// digest is the digest's items, as digestRunItems gives them, the digest
// being issue n.
func (l *planReviewRunLoop) digest(n int) []string {
	return digestRunItems(l.f.State().Issues[n-1].Body)
}

// comments are issue n's comments, with their IDs.
func (l *planReviewRunLoop) comments(n int) []github.Comment {
	l.t.Helper()
	s, err := l.f.Client(l.t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		l.t.Fatal(err)
	}
	return s.Comments
}

// newest is issue n's newest comment, which must be the App's.
func (l *planReviewRunLoop) newest(step string, n int) github.Comment {
	l.t.Helper()
	cs := l.comments(n)
	c := cs[len(cs)-1]
	if c.Author.Login != app {
		l.t.Fatalf("%s: #%d's newest comment is %s's, not the App's: %q", step, n, c.Author.Login, c.Body)
	}
	return c
}

// context is claude run k's context files, as the run had them, after
// checking that the run was leased on issue n at stage.
func (l *planReviewRunLoop) context(k, n int, stage state.Stage) map[string]string {
	l.t.Helper()
	w := &wholeRun{rig: l.g, n: n}
	return w.context(k, stage)
}

// planReviewRunOutput is internal/agent's golden epic plan, which the fake
// planner returns, with no model on any item, so main allows none, and
// with change, if set, made to it.
func planReviewRunOutput(t testing.TB, change func(plan map[string]any)) map[string]any {
	t.Helper()
	out := plGolden(t, "epic-plan")
	plan, ok := out["plan"].(map[string]any)
	if !ok {
		t.Fatalf("epic-plan.json has no plan: %v", out)
	}
	items, ok := plan["items"].([]any)
	if !ok {
		t.Fatalf("epic-plan.json's plan has no items: %v", plan)
	}
	for i := range items {
		delete(plItem(t, out, i), "model")
	}
	if change != nil {
		change(plan)
	}
	return out
}

// planReviewRunEpic files mertenvg's epic "Quote expiry", typed by his
// label, at planning, as the App moved it there: revision 1 of its spec,
// then its approval. It returns the epic's number and the approval's ID.
func planReviewRunEpic(f *githubtest.Fake) (int, int64) {
	n := f.AddIssue(planReviewRunOwner, "Quote expiry", planReviewRunBody, "type:epic")
	f.AddLabel(n, app, "stage:planning")
	rev := f.AddComment(n, app, revision(1, planReviewRunBody))
	return n, f.AddComment(n, app, fmt.Sprintf("Revision 1 of the spec is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=spec-review to=planning rev=1 body=%s cmd=r9 seen=%d -->", planReviewRunOwner, state.BodyHash(planReviewRunBody), rev))
}

// planReviewRunParent files mertenvg's epic "Bookings" at planned, as the
// App moved it there, approving its plan, which filed the stories. It
// returns its number.
func planReviewRunParent(f *githubtest.Fake) int {
	n := f.AddIssue(planReviewRunOwner, "Bookings", "Bookings should be simpler.", "type:epic")
	f.AddLabel(n, app, "stage:planned")
	f.AddComment(n, app, "Revision 1 of the plan is approved.\n\n<!-- issuedog:transition from=plan-review to=planned rev=1 body=abcd1234 cmd=r8 -->")
	return n
}

// planReviewRunStory files the App's story "Story <item>" under parent,
// which item of parent's plan revision 1 is, as its origin marker says,
// with priority, if set, at planning, then posts its plan revision 1
// (planText) with the planner's move to plan-review, as the planner
// posts it. It returns the story's number and its plan comment's ID.
func planReviewRunStory(t testing.TB, f *githubtest.Fake, parent int, item, priority string) (int, int64) {
	t.Helper()
	n := f.AddIssue(app, "Story "+item, fmt.Sprintf("The story.\n\n<!-- issuedog:origin plan=#%d rev=1 item=%s -->", parent, item))
	f.SetParent(n, parent)
	f.AddLabel(n, app, "type:story")
	if priority != "" {
		f.AddLabel(n, app, "priority:"+priority)
	}
	f.AddLabel(n, app, "stage:planning")
	f.AddComment(n, app, fmt.Sprintf("<!-- issuedog:transition from=intake to=planning run=c%d -->", n))
	return n, planReviewRunPlan(t, f, n)
}

// planReviewRunPlan posts plan revision 1 (planText) on issue n, with the
// planner's move from planning to plan-review, keyed run=p1, and returns
// its comment's ID.
func planReviewRunPlan(t testing.TB, f *githubtest.Fake, n int) int64 {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	a := &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}
	m := daemon.Move{To: state.PlanReview, Trigger: state.TriggerPlanValid, Key: state.Key{Field: "run", ID: "p1"}, PlanRev: 1, Text: planText(n, 1)}
	if err := a.Transition(context.Background(), s, clean, m); err != nil {
		t.Fatal(err)
	}
	return s.Comments[len(s.Comments)-1].ID
}

// planComment checks that c, on issue n, is plan revision rev, posted by
// claude run k, run<k>, having seen comment seen: the plan's text, with
// its cc line and how to approve it, then the move's transition marker,
// the plan-data marker, which reads back as out's plan, and the plan
// revision marker, whose hash is of everything above it. It returns the
// hash.
func (l *planReviewRunLoop) planComment(step string, c github.Comment, n, rev, k int, seen int64, out map[string]any) string {
	l.t.Helper()
	text, rest, ok := strings.Cut(c.Body, "\n\n<!-- issuedog:")
	if !ok || c.Author.Login != app {
		l.t.Fatalf("%s: comment %d, by %s, has no markers, or isn't the App's: %q", step, c.ID, c.Author.Login, c.Body)
	}
	markers := strings.Split("<!-- issuedog:"+rest, "\n")
	if !strings.HasPrefix(text, fmt.Sprintf("### Plan for #%d (rev %d)\n\n", n, rev)) ||
		!strings.HasSuffix(text, "\n\ncc @"+planReviewRunOwner+"\n\n**To approve this plan, react 👍 to this comment.** Any other comment sends it back for revision.") {
		l.t.Errorf("%s: plan revision %d's text =\n%s\nwant its heading, then the plan, ending in the cc line and how to approve it", step, rev, text)
	}
	if len(markers) != 3 {
		l.t.Fatalf("%s: plan revision %d's markers %q; want the transition, the plan data and the revision", step, rev, markers)
	}
	if want := fmt.Sprintf("<!-- issuedog:transition from=planning to=plan-review rev=%d run=run%d seen=%d -->", rev, k, seen); markers[0] != want {
		l.t.Errorf("%s: plan revision %d's move = %s; want %s", step, rev, markers[0], want)
	}
	if got := plData(l.t, markers[1]); !reflect.DeepEqual(got, out["plan"]) {
		l.t.Errorf("%s: plan revision %d's data reads back as\n%v\nwant the planner's\n%v", step, rev, got, out["plan"])
	}
	hash := state.BodyHash(c.Body[:strings.LastIndex(c.Body, "\n")])
	if want := fmt.Sprintf("<!-- issuedog:revision kind=plan rev=%d body=%s -->", rev, hash); markers[2] != want {
		l.t.Errorf("%s: plan revision %d's last line = %s; want %s", step, rev, markers[2], want)
	}
	return hash
}

// planItem checks that the digest, issue 5, lists the epic's plan
// approval third, after the stories', for revision rev with hash, in
// comment id, approvable as it stands, with no mention.
func (l *planReviewRunLoop) planItem(step string, rev int, hash string, id int64) {
	l.t.Helper()
	body := l.f.State().Issues[4].Body
	want := fmt.Sprintf("### 3. #1: Quote expiry\n\n**Plan approval**, priority p2, unblocks 1 open issue (itself included)\n\n"+
		"- Revision %d of the plan, `%s`, waits for approval.\n"+
		"- React 👍 to the plan comment, or post `/approve`, to approve it.\n"+
		"- Post `/revise <notes>`, or any other comment, to send it back.\n"+
		"- Answer here: https://github.com/o/r/issues/1#issuecomment-%d\n", rev, hash, id)
	if !strings.Contains(body, want) || strings.Contains(body, "@"+planReviewRunOwner) {
		l.t.Errorf("%s: the digest =\n%s\nwant the epic's item\n%s\nand no mention", step, body, want)
	}
}

// labels checks issue n's labels, sorted.
func (l *planReviewRunLoop) labels(step string, n int, want ...string) {
	l.t.Helper()
	if got := ivLabels(l.f, n); !slices.Equal(got, want) {
		l.t.Errorf("%s: #%d's labels %v; want %v", step, n, got, want)
	}
}

// reactions checks the reactions on comment id of issue n, as
// "<login> <content>".
func (l *planReviewRunLoop) reactions(step string, n int, id int64, want ...string) {
	l.t.Helper()
	cs := l.f.State().Issues[n-1].Comments
	i := slices.IndexFunc(l.comments(n), func(c github.Comment) bool { return c.ID == id })
	if i < 0 || i >= len(cs) {
		l.t.Fatalf("%s: #%d has no comment %d", step, n, id)
	}
	if !slices.Equal(cs[i].Reactions, want) {
		l.t.Errorf("%s: comment %d's reactions %q; want %q", step, id, cs[i].Reactions, want)
	}
}

// storyRound checks that claude run k was the planner's on story n, at
// planning, with effort as its --effort, "" for none, from the story's
// effort:* label (SPEC §3.3), and no model, as main allows none, and that
// the story's comments are its first move, then the round 1 that run
// asked, having seen it.
func (l *planReviewRunLoop) storyRound(step string, n, k int, effort string) {
	l.t.Helper()
	l.context(k, n, state.Planning)
	cs := l.comments(n)
	if len(cs) != 2 {
		l.t.Fatalf("%s: #%d has %d comments; want its first move, then the planner's round", step, n, len(cs))
	}
	round := cs[1]
	want := storyRoundMarker(k, effort, "", cs[0].ID)
	if round.Author.Login != app || !strings.HasPrefix(round.Body, fmt.Sprintf("### Questions for #%d (round 1)\n\n**1. ", n)) ||
		!strings.HasSuffix(round.Body, "\n\ncc @"+planReviewRunOwner+"\n\n"+want) {
		l.t.Errorf("%s: #%d's newest comment, by %s, =\n%s\nwant the App's round 1 of 2 questions, then the cc line and %s", step, n, round.Author.Login, round.Body, want)
	}
	l.g.checkFlags(k, "", effort, "")
}

// planReviewRunData is the plan the plan-data marker in comment c holds.
func planReviewRunData(t *testing.T, c github.Comment) any {
	t.Helper()
	for _, line := range strings.Split(c.Body, "\n") {
		if strings.HasPrefix(line, "<!-- issuedog:plan-data ") {
			return plData(t, line)
		}
	}
	t.Fatalf("comment %d has no plan-data marker: %q", c.ID, c.Body)
	return nil
}

// TestPlanReviewRun is the whole run (#272's acceptance criteria). The fake
// holds, by number:
//  1. mertenvg's epic at planning, its spec approved;
//  2. mertenvg's epic "Bookings" at planned, whose plan filed 3 and 4;
//  3. the App's story S2 of 2's plan, at priority:p1, at plan-review with
//     plan revision 1;
//  4. the App's story S1 of 2's plan, with no priority, at plan-review
//     with plan revision 1.
//
// The first pass creates the digest, 5, and the planner posts the epic's
// plan revision 1, which the next pass lists and assigns. Then mertenvg's
// comment sends it back, the planner gets it as its notes and posts
// revision 2, 👍s on revision 1, and by mallory, the App and helper[bot],
// approve nothing, and mertenvg's 👍 on revision 2 moves the epic to
// planned, which ends its item and its assignment. The same pass files
// revision 2's three stories, 6 to 8, each linked under the epic and moved
// to planning (#276), and the planner then asks each its round 1, one a
// pass, which waits in the digest with mertenvg assigned. A /hold, an
// /approve and a /resume on story S1, and a restart, end the run. The writes are
// checked at each step, and every unchanged pass costs no rate limit.
func TestPlanReviewRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(planReviewRunOwner, "admin")
	epic, entry := planReviewRunEpic(f)
	parent := planReviewRunParent(f)
	s2, _ := planReviewRunStory(t, f, parent, "S2", "p1")
	s1, plan1 := planReviewRunStory(t, f, parent, "S1", "")
	if epic != 1 || parent != 2 || s2 != 3 || s1 != 4 {
		t.Fatalf("issues %d, %d, %d and %d; want 1 to 4, which the writes' paths name", epic, parent, s2, s1)
	}
	rev1Out := planReviewRunOutput(t, nil)
	rev2Out := planReviewRunOutput(t, func(plan map[string]any) {
		items, ok := plan["items"].([]any)
		if !ok || len(items) == 0 {
			t.Fatalf("the plan has no items: %v", plan)
		}
		first, ok := items[0].(map[string]any)
		if !ok {
			t.Fatalf("item S1 is %T", items[0])
		}
		first["title"] = "The booking API refuses expired quotes"
	})
	l := newPlanReviewRunLoop(t, f, rev1Out, rev2Out)
	owner := []string{planReviewRunOwner}
	stories := []string{"#4 Plan approval p2 1", "#3 Plan approval p1 1"} // S1, then S2, whatever their priorities
	withEpic := append(slices.Clone(stories), "#1 Plan approval p2 1")

	// 1. The first pass creates and pins the digest, which lists the
	// stories in their plan's order and announces nothing, as each plan
	// comment ends in a cc line, and assigns them. Then the planner's
	// first run posts the epic's plan revision 1, at plan-review.
	l.step("the first pass", 1, "POST /issues", "POST /graphql", "PATCH /issues/5", "POST /issues/3/assignees", "POST /issues/4/assignees",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	if got := l.digest(5); !slices.Equal(got, stories) || len(digestComments(f, 5)) != 0 {
		t.Errorf("the first pass: items %q, announcements %q; want %q, and none", got, digestComments(f, 5), stories)
	}
	assignWant(t, f, "the first pass", map[int][]string{3: owner, 4: owner})
	files := l.context(1, epic, state.Planning)
	if !strings.Contains(files["input.md"], "\nnone: first run\n") {
		t.Errorf("planner run 1: input.md =\n%s\nwant none: the first run", files["input.md"])
	}
	rev1 := l.newest("plan revision 1", epic)
	hash1 := l.planComment("plan revision 1", rev1, epic, 1, 1, entry, rev1Out)
	l.labels("plan revision 1", epic, "stage:plan-review", "type:epic")

	// 2. The next pass lists the epic's plan, ranked on its own, and
	// assigns it. The stories' group sits where S2, at p1, ranks.
	l.step("the epic's plan listed", 1, "PATCH /issues/5", "POST /issues/1/assignees")
	if got := l.digest(5); !slices.Equal(got, withEpic) || len(digestComments(f, 5)) != 0 {
		t.Errorf("the epic's plan listed: items %q, announcements %q; want %q, and none", got, digestComments(f, 5), withEpic)
	}
	l.planItem("the epic's plan listed", 1, hash1, rev1.ID)
	assignWant(t, f, "the epic's plan listed", map[int][]string{1: owner, 3: owner, 4: owner})
	l.quiet("plan revision 1")

	// 3. 👍s on revision 1 by mallory, whom CODEOWNERS doesn't trust, by
	// the App and by helper[bot] approve nothing.
	f.AddReaction(rev1.ID, "mallory", "+1")
	f.AddReaction(rev1.ID, app, "+1")
	f.AddReaction(rev1.ID, planReviewRunBot, "+1")
	l.step("untrusted 👍s on revision 1", 1)
	l.labels("untrusted 👍s on revision 1", epic, "stage:plan-review", "type:epic")

	// 4. mertenvg's comment sends the plan back, with a 👍 on it, which
	// ends the epic's item for the pass. The planner's second run gets the
	// comment as its notes, labelled so, and posts plan revision 2 in the
	// same pass, which waits on mertenvg again, so he stays assigned with
	// no write (SPEC §9.6, #333).
	notes := f.AddComment(epic, planReviewRunOwner, planReviewRunNotes)
	l.step("the comment", 2, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		discRunReact(notes), "PATCH /issues/5",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	added := l.added(epic)
	sentBack := fmt.Sprintf("The plan is sent back for revision, with %s's comment as the notes.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planning cmd=%d seen=%d -->", planReviewRunOwner, notes, notes)
	if len(added) != 2 || added[0] != sentBack {
		t.Fatalf("the comment: added %q; want the move back to planning\n%s\nthen plan revision 2", added, sentBack)
	}
	l.reactions("the comment", epic, notes, app+" +1")
	cs := l.comments(epic)
	back, rev2 := cs[len(cs)-2], cs[len(cs)-1]
	hash2 := l.planComment("plan revision 2", rev2, epic, 2, 2, back.ID, rev2Out)
	l.labels("plan revision 2", epic, "stage:plan-review", "type:epic")
	if got := l.digest(5); !slices.Equal(got, stories) {
		t.Errorf("the comment: items %q; want %q, with the epic's gone", got, stories)
	}
	assignWant(t, f, "the comment", map[int][]string{1: owner, 3: owner, 4: owner})
	files = l.context(2, epic, state.Planning)
	input := files["input.md"]
	if heading := fmt.Sprintf("## Comment %d by %s: the revision notes that sent the plan back\n", notes, planReviewRunOwner); !strings.Contains(input, heading) ||
		!strings.Contains(input, "```text\n"+planReviewRunNotes+"\n```") || strings.Count(input, "## Comment ") != 1 {
		t.Errorf("planner run 2: input.md =\n%s\nwant the comment alone, fenced, under %q", input, heading)
	}
	if !strings.Contains(files["conversation.md"], fmt.Sprintf("## Comment %d by issuedog", rev1.ID)) {
		t.Errorf("planner run 2: conversation.md =\n%s\nwant plan revision 1 in it", files["conversation.md"])
	}

	// 5. The next pass lists revision 2, and the epic keeps its assignee.
	l.step("plan revision 2 listed", 2, "PATCH /issues/5")
	if got := l.digest(5); !slices.Equal(got, withEpic) {
		t.Errorf("plan revision 2 listed: items %q; want %q", got, withEpic)
	}
	l.planItem("plan revision 2 listed", 2, hash2, rev2.ID)
	assignWant(t, f, "plan revision 2 listed", map[int][]string{1: owner, 3: owner, 4: owner})
	l.quiet("plan revision 2")

	// 6. mertenvg's 👍 on revision 1, now an earlier revision, approves
	// nothing, and nor do helper[bot]'s, mallory's and the App's on
	// revision 2, though the bot's is the earliest.
	f.AddReaction(rev1.ID, planReviewRunOwner, "+1")
	f.AddReaction(rev2.ID, planReviewRunBot, "+1")
	f.AddReaction(rev2.ID, "mallory", "+1")
	f.AddReaction(rev2.ID, app, "+1")
	l.step("👍s that approve nothing", 2)
	l.labels("👍s that approve nothing", epic, "stage:plan-review", "type:epic")

	// 7. mertenvg's 👍 on revision 2 moves the epic to planned, with
	// rev=2, and the same pass files revision 2's three stories as #6 to
	// #8, in plan order, each created, linked under the epic and moved to
	// planning, keyed by the approval (#276). Then S2 and S3, which depend
	// on S1, are blocked by it, and the record of the stories is posted on
	// the epic, last (#277). Then the epic's item and its assignment end.
	//
	// The stories are revision 2's, S1 with its new title, at the
	// priority each item names, else the epic's p2, and with the effort:*
	// label of each item's effort (#344), S1's high and S3's medium. The
	// items name no model (planReviewRunOutput), so none has a model:*
	// label.
	epicStories := []wantChild{
		{"S1", "S1. The booking API refuses expired quotes", "", []string{"effort:high", "priority:p1", "stage:planning", "type:story"}, state.Planning},
		{"S2", "S2. Customers see when a quote expires", "", []string{"priority:p2", "stage:planning", "type:story"}, state.Planning},
		{"S3", "S3. Expired quotes in the monthly report", "", []string{"effort:medium", "priority:p2", "stage:planning", "type:story"}, state.Planning},
	}
	up := f.AddReaction(rev2.ID, planReviewRunOwner, "+1")
	upFrom := len(f.Requests())
	l.step("the approval", 2, slices.Concat(
		[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review"},
		childWrites(epic, 6, 7, 8), blockWrites(7, 8), []string{recordWrite(epic)},
		[]string{"PATCH /issues/5", "DELETE /issues/1/assignees"})...)
	approval := fmt.Sprintf("Revision 2 of the plan is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planned rev=2 body=%s cmd=r%d seen=%d -->", planReviewRunOwner, hash2, up, rev2.ID)
	if added, want := l.added(epic), []string{approval, recordOf(2, 6, epicStories)}; !slices.Equal(added, want) {
		t.Errorf("the approval: added %q; want the approval, then the record:\n%q", added, want)
	}
	l.labels("the approval", epic, "stage:planned", "type:epic")
	if got := l.digest(5); !slices.Equal(got, stories) {
		t.Errorf("the approval: items %q; want %q", got, stories)
	}
	assignWant(t, f, "the approval", map[int][]string{3: owner, 4: owner})
	// The approved revision reads back as revision 2, with the plan data
	// the planner's second run gave.
	got, c, ok := state.ApprovedPlan(l.g.snapshot(epic), slug, clean)
	if want := (state.Revision{Kind: state.PlanRevision, Rev: 2, Hash: hash2, Comment: rev2.ID, Seen: back.ID}); !ok || got != want || c.Body != rev2.Body {
		t.Fatalf("ApprovedPlan = %+v, %v; want %+v, in plan revision 2's comment", got, ok, want)
	}
	if plan := planReviewRunData(t, c); !reflect.DeepEqual(plan, rev2Out["plan"]) || reflect.DeepEqual(plan, rev1Out["plan"]) {
		t.Errorf("the approved plan's data =\n%v\nwant the planner's second\n%v", plan, rev2Out["plan"])
	}
	// The newest comment is the record, and the approval, which keys the
	// stories' first moves, is just before it.
	l.newest("the approval", epic)
	cs = l.comments(epic)
	approvedBy := cs[len(cs)-2]
	runChildren(t, f, "the approval", epic, 2, approvedBy.ID, 6, epicStories)
	runRecord(t, f, "the approval", epic, 2, 6, epicStories, map[int][]int{7: {6}, 8: {6}})
	if !childrenLooked(f, upFrom, approvedBy.CreatedAt) {
		t.Errorf("the approval: the pass didn't look for the epic's children in the issues listed since its approval")
	}

	// 8. The planner runs on each new story in turn, one a pass, in plan
	// order, as the stories skipped the rest of the pass that filed them.
	// A story's effort:* label sets its run's --effort (SPEC §3.3): S1's
	// high, S2's none and S3's medium. Each run asks the story's round 1, which the pass after lists in the
	// digest, S1's first, as it blocks S2 and S3, and S2's and S3's after
	// the stories of 2's plan, assigning mertenvg on the story. The epic, read again as the approval and its record changed
	// it, makes no request for creation: its record stands (#277).
	ask := plGolden(t, "questions")
	l.outs(ask, ask, ask)
	from := len(f.Requests())
	l.step("S1's round", 3, "POST /issues/6/comments")
	recordStands(t, f, "S1's round", from, epic, approvedBy.CreatedAt, []int{7, 8}, true)
	l.storyRound("S1's round", 6, 3, "high")
	l.step("S2's round", 4, "PATCH /issues/5", "POST /issues/6/assignees", "POST /issues/7/comments")
	l.storyRound("S2's round", 7, 4, "")
	l.step("S3's round", 5, "PATCH /issues/5", "POST /issues/7/assignees", "POST /issues/8/comments")
	l.storyRound("S3's round", 8, 5, "medium")
	l.step("S3's round listed", 5, "PATCH /issues/5", "POST /issues/8/assignees")
	// S1, #6, blocks S2 and S3, so it scores 3 and comes first, above the
	// plan approvals (SPEC §9.4, The order).
	first := []string{"#6 Question round p1 3"}
	rounds := []string{"#7 Question round p2 1", "#8 Question round p2 1"}
	if got, want := l.digest(5), slices.Concat(first, stories, rounds); !slices.Equal(got, want) {
		t.Errorf("the stories' rounds: items %q; want %q", got, want)
	}
	assignWant(t, f, "the stories' rounds", map[int][]string{3: owner, 4: owner, 6: owner, 7: owner, 8: owner})
	l.quiet("planned")
	recordStands(t, f, "the stories' rounds", from, epic, approvedBy.CreatedAt, []int{7, 8}, true)

	// 9. mertenvg's /hold on S1 holds it from plan-review, which ends its
	// item and its assignment.
	hold := f.AddComment(s1, planReviewRunOwner, "/hold")
	l.step("the /hold", 5, "POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:plan-review",
		discRunReact(hold), "PATCH /issues/5", "DELETE /issues/4/assignees")
	held := fmt.Sprintf("On hold by `/hold`, from `plan-review`.\n\n"+
		"<!-- issuedog:transition from=plan-review to=held at=plan-review reason=owner cmd=%d seen=%d -->", hold, plan1)
	if added := l.added(s1); !slices.Equal(added, []string{held}) {
		t.Errorf("the /hold: added %q; want\n%s", added, held)
	}
	l.labels("the /hold", s1, "stage:held", "type:story")
	if got, want := l.digest(5), slices.Concat(first, stories[1:], rounds); !slices.Equal(got, want) {
		t.Errorf("the /hold: items %q; want %q", got, want)
	}
	assignWant(t, f, "the /hold", map[int][]string{3: owner, 6: owner, 7: owner, 8: owner})
	l.quiet("held")

	// 10. An /approve posted during the hold waits: no write, no reaction.
	approve := f.AddComment(s1, planReviewRunOwner, "/approve")
	l.step("the /approve during the hold", 5)
	l.reactions("the /approve during the hold", s1, approve)
	l.labels("the /approve during the hold", s1, "stage:held", "type:story")
	l.quiet("the /approve waits")

	// 11. /resume takes S1 back to plan-review, and the /approve then
	// approves its plan revision 1, in the same pass. That revision
	// (planText) holds no plan data, so it files no children.
	resume := f.AddComment(s1, planReviewRunOwner, "/resume")
	l.step("the /resume", 5, "POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:held", discRunReact(resume),
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:plan-review", discRunReact(approve))
	want := []string{
		fmt.Sprintf("Resumed by `/resume`, back to `plan-review`.\n\n<!-- issuedog:transition from=held to=plan-review cmd=%d seen=%d -->", resume, plan1),
		fmt.Sprintf("Revision 1 of the plan is approved, by %s's `/approve`.\n\n"+
			"<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=%d seen=%d -->", planReviewRunOwner, planHash(s1, 1), approve, approve),
	}
	if added := l.added(s1); !slices.Equal(added, want) {
		t.Errorf("the /resume: added %q; want %q", added, want)
	}
	l.labels("the /resume", s1, "stage:planned", "type:story")
	l.reactions("the /resume", s1, approve, app+" +1")
	if got, want := l.digest(5), slices.Concat(first, stories[1:], rounds); !slices.Equal(got, want) {
		t.Errorf("the /resume: items %q; want %q", got, want)
	}
	assignWant(t, f, "the /resume", map[int][]string{3: owner, 6: owner, 7: owner, 8: owner})
	l.quiet("resumed")

	// 12. A restart writes nothing, starts no run, announces nothing,
	// creates no other digest and files no story again.
	from = len(f.Requests())
	l.restart("the end")
	if n, news := len(f.State().Issues), digestComments(f, 5); n != 8 || len(news) != 0 || l.g.runs() != 5 {
		t.Errorf("%d issues, announcements %q, %d runs; want 8, none, and the planner's 2 on the epic and 3 on its stories", n, news, l.g.runs())
	}
	recordStands(t, f, "the end", from, epic, approvedBy.CreatedAt, []int{7, 8}, true)
	runChildren(t, f, "the end", epic, 2, approvedBy.ID, 6, epicStories)
	runRecord(t, f, "the end", epic, 2, 6, epicStories, map[int][]int{7: {6}, 8: {6}})
}

// planReviewRunAlone is a loop on a fake holding only mertenvg's epic, 1,
// at planning (planReviewRunEpic), whose fake claude prints the golden
// epic plan (planReviewRunOutput), then more, one a run. Its first pass
// creates the digest, 2, and the planner posts plan revision 1, which the
// second pass lists and assigns. It returns the loop, plan revision 1's
// comment, and the ID of the spec's approval, which put the epic at
// planning.
func planReviewRunAlone(t *testing.T, more ...map[string]any) (*planReviewRunLoop, github.Comment, int64) {
	t.Helper()
	f := newFake(t)
	f.SetPermission(planReviewRunOwner, "admin")
	epic, entry := planReviewRunEpic(f)
	out := planReviewRunOutput(t, nil)
	l := newPlanReviewRunLoop(t, f, append([]map[string]any{out}, more...)...)
	l.step("the first pass", 1, "POST /issues", "POST /graphql", "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	rev1 := l.newest("plan revision 1", epic)
	l.planComment("plan revision 1", rev1, epic, 1, 1, entry, out)
	l.step("the epic's plan listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")
	if got, want := l.digest(2), []string{"#1 Plan approval p2 1"}; !slices.Equal(got, want) {
		t.Errorf("the epic's plan listed: items %q; want %q", got, want)
	}
	l.quiet("plan revision 1")
	return l, rev1, entry
}

// At spec-review, after a discussion hold from plan-review, a 👍 on the
// old plan comment approves nothing (#268's verify, on #272). mallory's
// comment holds the epic from plan-review, and mertenvg's 👍 on plan
// revision 1, given during the hold, waits. His 👍 on mallory's comment
// ends the hold for clarifying, where the interviewer writes spec
// revision 2, at spec-review, in the same pass. There the 👍 on the plan
// approves nothing, on any pass, and only his 👍 on spec revision 2 moves
// the epic on, to planning, where the planner posts plan revision 2.
// mertenvg stays assigned throughout, with no write, as each of those
// passes' stage work leaves the epic waiting on him again (SPEC §9.6). A
// restart writes nothing.
func TestPlanReviewRunOldPlanAtSpecReview(t *testing.T) {
	t.Parallel()
	l, rev1, _ := planReviewRunAlone(t, ivSpec(), planReviewRunOutput(t, nil))
	f := l.f
	m := f.AddComment(1, "mallory", "Should S3 wait for S2?")
	l.step("mallory's comment", 1, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review", "PATCH /issues/2")
	l.labels("mallory's comment", 1, "stage:held", "type:epic")
	hold := l.newest("mallory's comment", 1)
	if got, want := l.digest(2), []string{"#1 Discussion hold p2 1"}; !slices.Equal(got, want) {
		t.Errorf("mallory's comment: items %q; want %q", got, want)
	}
	l.quiet("held")

	f.AddReaction(rev1.ID, planReviewRunOwner, "+1")
	l.step("a 👍 on the plan during the hold", 1)
	l.labels("a 👍 on the plan during the hold", 1, "stage:held", "type:epic")

	f.AddReaction(m, planReviewRunOwner, "+1")
	l.step("the discussion resolved", 2, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:held",
		"PATCH /issues/2", "PATCH /issues/1", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying")
	added := l.added(1)
	if len(added) != 2 || !strings.HasPrefix(added[0], "The discussion is resolved, by `mertenvg`, so this issue leaves its hold from `plan-review` for `clarifying`.") {
		t.Fatalf("the discussion resolved: added %q; want the move to clarifying, then spec revision 2", added)
	}
	l.labels("the discussion resolved", 1, "stage:spec-review", "type:epic")
	cs := l.comments(1)
	leave, spec := cs[len(cs)-2], cs[len(cs)-1]
	hash := state.BodyHash(f.State().Issues[0].Body)
	if want := fmt.Sprintf("<!-- issuedog:transition from=clarifying to=spec-review origin=plan-review rev=2 body=%s run=run2 seen=%d -->\n"+
		"<!-- issuedog:revision rev=2 body=%s -->", hash, leave.ID, hash); !strings.HasSuffix(spec.Body, want) {
		t.Errorf("the discussion resolved: the newest comment =\n%s\nwant spec revision 2, ending\n%s", spec.Body, want)
	}
	if hold.ID >= leave.ID {
		t.Errorf("the hold, %d, isn't before the move out of it, %d", hold.ID, leave.ID)
	}

	l.step("spec revision 2 listed", 2, "PATCH /issues/2")
	if got, want := l.digest(2), []string{"#1 Approval p2 1"}; !slices.Equal(got, want) {
		t.Errorf("spec revision 2 listed: items %q; want %q", got, want)
	}
	l.quiet("spec revision 2")
	l.labels("spec revision 2", 1, "stage:spec-review", "type:epic")

	up := f.AddReaction(spec.ID, planReviewRunOwner, "+1")
	l.step("the spec approved", 3, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:spec-review",
		"PATCH /issues/2", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	cs = l.comments(1)
	approval, rev2 := cs[len(cs)-2], cs[len(cs)-1]
	if want := fmt.Sprintf("Revision 2 of the spec is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=spec-review to=planning rev=2 body=%s cmd=r%d seen=%d -->", planReviewRunOwner, hash, up, spec.ID); approval.Body != want {
		t.Errorf("the spec approved: the approval =\n%s\nwant\n%s", approval.Body, want)
	}
	hash2 := l.planComment("plan revision 2", rev2, 1, 2, 3, approval.ID, planReviewRunOutput(t, nil))
	l.step("plan revision 2 listed", 3, "PATCH /issues/2")
	if got, want := l.digest(2), []string{"#1 Plan approval p2 1"}; !slices.Equal(got, want) || !strings.Contains(f.State().Issues[1].Body, "`"+hash2+"`") {
		t.Errorf("plan revision 2 listed: items %q; want %q, for plan revision 2", got, want)
	}
	// The 👍 on plan revision 1 approves revision 2 no more than the spec.
	l.quiet("plan revision 2")
	l.labels("plan revision 2", 1, "stage:plan-review", "type:epic")
	l.restart("the end")
}

// An /approve rejected while several reasons hold names the first, in
// state.PlanApproval's order (#268's verify, on #272): a hidden revision,
// then a changed one, then a dirty body. mallory's edit of the body holds
// the epic for discussion from plan-review, and, in one case, her edit of
// the App's hold comment, after plan revision 1, hides that revision. The
// App's comment of plan revision 1 changes, so it no longer matches its
// hash, as no one else's edit can make it (a trusted user's edit of an
// App comment makes it dirty). Then mertenvg's /resume takes the epic back
// to plan-review, and his /approve, after it, is rejected there, naming
// the first reason only. The digest's item names each reason, in the same
// order. A restart writes nothing.
func TestPlanReviewRunApproveReasons(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		hide bool
	}{
		{"hidden, changed and a dirty body", true},
		{"changed and a dirty body", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			l, rev1, entry := planReviewRunAlone(t)
			f := l.f
			hash := state.BodyHash(rev1.Body[:strings.LastIndex(rev1.Body, "\n")])
			f.EditBody(1, "mallory", planReviewRunBody+" Or never.")
			l.step("mallory's edit of the body", 1, "PATCH /issues/1", "POST /issues/1/comments", "POST /issues/1/labels",
				"DELETE /issues/1/labels/stage:plan-review", "PATCH /issues/2")
			l.labels("mallory's edit of the body", 1, "stage:held", "type:epic")
			hold := l.newest("mallory's edit of the body", 1)
			if !strings.HasPrefix(hold.Body, "On hold for discussion, from `plan-review`: `mallory` edited the issue description") {
				t.Fatalf("mallory's edit of the body: the newest comment =\n%s\nwant the hold", hold.Body)
			}
			var why []string
			if tt.hide {
				f.EditComment(hold.ID, "mallory", hold.Body+" Or never.")
				l.step("mallory's edit of the hold", 1, "POST /issues/1/comments", fmt.Sprintf("PATCH /issues/comments/%d", hold.ID),
					"POST /issues/2/comments", "PATCH /issues/2")
				why = append(why, "a comment of mine after plan revision 1 was edited without approval or deleted, so it may hide a newer revision")
			}
			why = append(why, fmt.Sprintf("the comment of plan revision 1 changed after it was posted, so it no longer matches `%s`", hash),
				"the issue description has an edit that wasn't approved (see the note on it)")
			f.EditComment(rev1.ID, app, strings.Replace(rev1.Body, "**Order:**", "**Order, as agreed:**", 1))
			l.step("the App's edit of the plan", 1)

			resume := f.AddComment(1, planReviewRunOwner, "/resume")
			approve := f.AddComment(1, planReviewRunOwner, "/approve")
			l.step("the /resume and the /approve", 1, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:held",
				discRunReact(resume), discRunReact(approve), "POST /issues/1/comments", "PATCH /issues/2")
			reason := fmt.Sprintf("the comment of plan revision 1 was edited after I posted it, so it no longer matches that revision's hash, `%s`. %s", hash, replanHint)
			if tt.hide {
				reason = "a comment of mine after plan revision 1 was edited without approval or deleted, so it may hide a newer plan revision. " + replanHint
			}
			a := strconv.FormatInt(approve, 10)
			want := []string{
				fmt.Sprintf("Resumed by `/resume`, back to `plan-review`.\n\n<!-- issuedog:transition from=held to=plan-review cmd=%d seen=%d -->", resume, entry),
				final("/approve", "I didn't apply this command", reason, a),
			}
			if added := l.added(1); !slices.Equal(added, want) {
				t.Errorf("the /resume and the /approve: added %q; want %q", added, want)
			}
			l.labels("the /resume and the /approve", 1, "stage:plan-review", "type:epic")
			l.reactions("the /resume and the /approve", 1, approve, app+" -1")
			lines := make([]string, len(why))
			for i, w := range why {
				lines[i] = "- It can't be approved yet: " + w + ".\n"
			}
			if body := f.State().Issues[1].Body; !strings.Contains(body, strings.Join(lines, "")) {
				t.Errorf("the /resume and the /approve: the digest =\n%s\nwant the reasons, in order:\n%s", body, strings.Join(lines, ""))
			}
			l.quiet("rejected")
			l.restart("the end")
		})
	}
}

// Another bot's 👍 on the latest plan revision is ignored because it's a
// bot, not only because it isn't trusted (#268's verify, on #272): with
// the daemon built as startDaemon builds it, daemon.New, and a Trust that
// trusts helper[bot] by name, its 👍, the earliest, approves nothing, and
// mertenvg's after it approves. The loop's trust, loaded from main, never
// trusts a bot, so this Trust is set on step 1 alone.
func TestPlanReviewRunBotThumb(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	epic, _ := planReviewRunEpic(f)
	plan := planReviewRunPlan(t, f, epic)
	base := newDaemon(t, f, newOrigin(t), nil)
	d := daemon.New(base.Step.Applier, base.Clone, base.Config, trusted{planReviewRunOwner: true, planReviewRunBot: true})
	pass := func(step string) []string {
		t.Helper()
		s, err := f.Client(t).Snapshot(context.Background(), "o", "r", epic)
		if err != nil {
			t.Fatal(err)
		}
		from := len(f.Writes())
		if _, err := d.Step.Run(context.Background(), s); err != nil {
			t.Fatalf("%s: step 1: %v", step, err)
		}
		return digestWrites(f, from)
	}
	f.AddReaction(plan, planReviewRunBot, "+1")
	if w := pass("helper[bot]'s 👍"); len(w) != 0 {
		t.Errorf("helper[bot]'s 👍: writes %q; want none", w)
	}
	if l := ivLabels(f, epic); !slices.Equal(l, []string{"stage:plan-review", "type:epic"}) {
		t.Errorf("helper[bot]'s 👍: labels %q; want plan-review", l)
	}
	up := f.AddReaction(plan, planReviewRunOwner, "+1")
	if w, want := pass("mertenvg's 👍"), []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review"}; !slices.Equal(w, want) {
		t.Errorf("mertenvg's 👍: writes %q; want %q", w, want)
	}
	if got, want := newestComment(f, epic), fmt.Sprintf("Revision 1 of the plan is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=r%d seen=%d -->", planReviewRunOwner, planHash(epic, 1), up, plan); got != want {
		t.Errorf("mertenvg's 👍: the newest comment =\n%s\nwant\n%s", got, want)
	}
}
