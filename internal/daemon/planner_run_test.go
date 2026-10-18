package daemon_test

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is #247's T9 (#260): whole runs of the planner against the
// fakes, on the rig of the interviewer's whole run (interviewer_run_test.go):
// the real Interviewer at clarifying and the real Planner at planning, as
// registerStages registers them, triage as the Unlabeled stage, step 1
// wired as main's newStep wires it, the digest and assignment on, as
// startDaemon sets them, and Daemon.Iterate as the loop. Each run ends with
// a restart: a new daemon on the same fake and state.
//
// Config and trust come from main on the origin: mertenvg and merten_acme
// own CODEOWNERS, notify is mertenvg, and eve has no access at all. The
// scopes and models.allowed are those the golden plans use, plan.review is
// false, so a story's plan skips review, and the planner's model is
// sonnet, while the interviewer has none.
//
// The planner's outputs are internal/agent's golden planner outputs (T4),
// and its plan comments are checked against T5's golden files in
// testdata/plan.

// planRunConfig is .issuedog/config.yml on main for the planner's whole run.
const planRunConfig = "types: labels\nnotify: [" + wholeRunOwner + "]\nscopes: [booking, infra, docs]\n" +
	"models: {allowed: [opus, sonnet, haiku]}\nplan: {review: false}\nstages: {model: {planner: sonnet}}\n"

// planRun is a whole run of the planner on issue n: a wholeRun, whose rig
// a restart replaces, keeping the fake, the origin and the state under root.
type planRun struct {
	*wholeRun
	o    *origin
	root string
}

// newPlanRun sets the fake, the origin and the daemon up, with no issue yet.
func newPlanRun(t *testing.T) *planRun {
	t.Helper()
	f := newFake(t)
	f.SetPermission(wholeRunOwner, "admin")
	f.SetPermission(wholeRunOther, "write")
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+wholeRunOwner+" @"+wholeRunOther+"\n")
	o.commit(config.Path, planRunConfig)
	p := &planRun{wholeRun: &wholeRun{}, o: o, root: t.TempDir()}
	p.start(t, f)
	return p
}

// start puts a new daemon on f, with p's origin and state, as a start of
// the issuedog binary does: the config from main and the labels it needs,
// the whole run's fake claude, the Stages as registerStages gives them,
// step 1 as newStep wires it, and the digest and assignment on, as
// startDaemon sets them.
func (p *planRun) start(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	g := newRig(t, f, p.o, p.root)
	// As startDaemon loads the config and creates the labels, before the
	// loop: with the defaults, the first pass would create them instead.
	cfg, err := config.Parse([]byte(planRunConfig))
	if err != nil {
		t.Fatal(err)
	}
	g.d.Config = cfg
	if _, err := github.EnsureLabels(context.Background(), f.Client(t), "o", "r", daemon.LabelOptions(cfg)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(g.path("claude", "claude"), []byte(wholeRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	// Run IDs go on across a restart, as the daemon's random ones never
	// repeat: run<k> is the fake claude's run k.
	g.r.NewID = func() string { return "run" + strconv.Itoa(g.runs()+1) }
	// As registerStages registers them (#247's T8).
	g.r.Stages = map[state.Stage]daemon.Stage{
		state.Clarifying: &daemon.Interviewer{Daemon: g.d},
		state.Planning:   &daemon.Planner{Daemon: g.d},
	}
	g.r.Unlabeled = &daemon.Triage{Daemon: g.d}
	// As newStep wires step 1.
	st := g.d.Step
	st.Commands = daemon.Commands(st.CurrentTrust())
	st.Commands["approve"] = daemon.ApproveCommand()
	st.Commands["revise"] = daemon.ReviseCommand()
	maps.Copy(st.Commands, daemon.SettingCommands(st.CurrentTrust(), g.d.CurrentModels))
	maps.Copy(st.Commands, daemon.FlagCommands())
	approve, answer := daemon.ApproveByReaction(st.CurrentTrust()), daemon.FlagByReaction(st.CurrentTrust())
	errAck, resolved := daemon.ErrorByReaction(st.CurrentTrust()), daemon.ResolveByReaction(st.CurrentTrust())
	st.Reactions = func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, d daemon.Dirt) error {
		if err := resolved(ctx, a, s, d); err != nil {
			return fmt.Errorf("discussion hold resolved: %w", err)
		}
		if err := approve(ctx, a, s, d); err != nil {
			return fmt.Errorf("approval by 👍: %w", err)
		}
		if err := answer(ctx, a, s, d); err != nil {
			return fmt.Errorf("flag answer by 👍: %w", err)
		}
		if err := errAck(ctx, a, s, d); err != nil {
			return fmt.Errorf("state error ack by 👍: %w", err)
		}
		return nil
	}
	// As startDaemon sets them.
	g.d.Digest, g.d.Assign = true, true
	p.rig = g
}

// restart replaces p's daemon with a new one, as a restart of the binary
// does, and checks that its first pass, which reads every open issue
// afresh, writes nothing and starts no run, nor does the pass after it.
func (p *planRun) restart(step string) {
	p.t.Helper()
	from := len(p.f.Writes())
	p.start(p.t, p.f)
	if w := p.f.Writes()[from:]; len(w) != 0 {
		p.t.Errorf("%s: the restart's start wrote %+v; want nothing", step, w)
	}
	p.iterate(step+", then a restart", p.runs())
	p.unchanged(step + ", after the restart")
}

// outputs writes what the fake claude prints on its next runs, in order:
// each a planner or interviewer output, as a successful run's result.
func (p *planRun) outputs(outs ...map[string]any) {
	p.t.Helper()
	next := p.runs()
	for _, out := range outs {
		next++
		if err := os.WriteFile(p.path("claude", "out."+strconv.Itoa(next)), []byte(ivResult(p.t, out)), 0o644); err != nil {
			p.t.Fatal(err)
		}
	}
}

// unchanged runs one more Iterate, which must write nothing, start no run,
// and send only conditional GETs, which GitHub answers 304: a pass where
// nothing changed makes no request beyond the poll.
func (p *planRun) unchanged(step string) {
	p.t.Helper()
	from := len(p.f.Requests())
	p.quiet(step)
	for _, r := range p.f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			p.t.Errorf("%s: an unchanged pass sent %+v; want only conditional GETs answered 304", step, r)
		}
	}
}

// digest is the digest issue's number: the one issue the App opened.
func (p *planRun) digest() int {
	p.t.Helper()
	for _, is := range p.f.State().Issues {
		if is.Author == app {
			return is.Number
		}
	}
	p.t.Fatal("no digest issue")
	return 0
}

// waiting checks the digest's items, as digestRunItems gives them, and
// issue n's assignees.
func (p *planRun) waiting(step string, items []string, assignees ...string) {
	p.t.Helper()
	is := p.f.State().Issues
	if got := digestRunItems(is[p.digest()-1].Body); !slices.Equal(got, items) {
		p.t.Errorf("%s: the digest's items %q; want %q", step, got, items)
	}
	if got := is[p.n-1].Assignees; !slices.Equal(got, assignees) {
		p.t.Errorf("%s: #%d is assigned %q; want %q", step, p.n, got, assignees)
	}
}

// plannerFlags checks run k's --max-turns and --model: the planner's 60,
// the default, and sonnet, from stages.model.planner.
func (p *planRun) plannerFlags(k int) {
	p.t.Helper()
	flags := p.flags(k)
	if flags["--max-turns"] != "60" || flags["--model"] != "sonnet" {
		p.t.Errorf("planner run %d: --max-turns %q and --model %q; want the planner's 60 and sonnet", k, flags["--max-turns"], flags["--model"])
	}
}

// planComment checks that issue n's newest comment is a plan comment by
// the App: golden, T5's testdata/plan/<golden>.md with #<from> as #n, then
// the transition marker want, the plan-data marker and the plan revision
// marker rev 1, last, whose hash is PlanHash's of the comment and which
// LatestRevision reads as the latest plan revision, matching. Its data
// reads back (readPlanData) as the plan in out. It returns the comment.
func (p *planRun) planComment(step, golden string, from int, want string, out map[string]any) github.Comment {
	p.t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "plan", golden+".md"))
	if err != nil {
		p.t.Fatal(err)
	}
	text := strings.ReplaceAll(strings.TrimSuffix(string(data), "\n"), "#"+strconv.Itoa(from), "#"+strconv.Itoa(p.n))
	cs := p.comments()
	c := cs[len(cs)-1]
	body, markers, ok := strings.Cut(c.Body, "\n\n<!-- issuedog:")
	lines := strings.Split("<!-- issuedog:"+markers, "\n")
	if c.Author.Login != app || !ok || body != text || len(lines) != 3 || lines[0] != want {
		p.t.Fatalf("%s: the newest comment, by %s, =\n%s\nwant the App's\n%s\n\n%s\nthen the plan data and the revision", step, c.Author.Login, c.Body, text, want)
	}
	hash, ok := state.PlanHash(c.Body)
	if !ok || lines[2] != fmt.Sprintf("<!-- issuedog:revision kind=plan rev=1 body=%s -->", hash) {
		p.t.Errorf("%s: the last line is %s; want plan revision 1's marker, with the hash %s (found %t)", step, lines[2], hash, ok)
	}
	s := p.snapshot(p.n)
	rev, ok := state.LatestRevision(s, slug, state.PlanRevision, func(int64) bool { return false })
	if !ok || rev.Rev != 1 || rev.Comment != c.ID || rev.Hash != hash || rev.Hidden || !rev.Matches(s) {
		p.t.Errorf("%s: the latest plan revision = %+v (found %t); want rev 1 in comment %d, with the hash %s, matching", step, rev, ok, c.ID, hash)
	}
	got, wantPlan, err := daemon.PlanRunData(c, slug, ivJSON(p.t, out))
	if err != nil {
		p.t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wantPlan) {
		p.t.Errorf("%s: the plan data reads back as\n%+v\nwant the run's plan\n%+v", step, got, wantPlan)
	}
	return c
}

// plans counts issue n's comments with a plan's heading.
func (p *planRun) plans() int {
	n := 0
	for _, c := range p.f.State().Issues[p.n-1].Comments {
		if strings.HasPrefix(c.Body, fmt.Sprintf("### Plan for #%d ", p.n)) {
			n++
		}
	}
	return n
}

// settle runs the pass after a step, which must start no run and write
// want: the digest and assignment, which a pass settles before the stage
// work, catch up with what the step's run posted. Its writes change the
// issue's updated_at, as the run's did, so the pass after them reads the
// issue again, and writes nothing. Then a pass where nothing changed makes
// no request beyond the poll (unchanged).
func (p *planRun) settle(step string, want ...string) {
	p.t.Helper()
	p.iterate(step+", the pass after", p.runs(), want...)
	if len(want) > 0 {
		p.iterate(step+", the pass after its writes", p.runs())
	}
	p.unchanged(step)
}

// TestPlannerWholeRunEpic: you file an epic with stage:intake, eve
// comments, and the interviewer asks two rounds and writes the spec, which
// you approve with /approve. The planner runs at planning at once, and asks
// round 3, numbered after the interviewer's two, which waits in the digest
// with notify assigned. Your answer gets a 👍, and the planner's second run
// gets it as its input and posts the epic's plan, as revision 1 of the
// plan, at plan-review, where it waits in the digest with notify assigned.
// Your 👍 on the plan comment approves the plan, not the spec: the epic
// moves to planned with plan revision 1, the plan's item and the assignment
// go, the spec's revision and approval are as they were, and a restart
// writes nothing.
func TestPlannerWholeRunEpic(t *testing.T) {
	t.Parallel()
	p := newPlanRun(t)
	f := p.f
	// The interviewer's outputs, as the interviewer's whole run gives them
	// for an epic its owner typed, then the planner's round and its plan.
	round1 := wholeRunOutput(t, "questions")
	delete(round1, "type")
	round2 := wholeRunOutput(t, "questions")
	delete(round2, "type")
	qs, ok := round2["questions"].([]any)
	if !ok || len(qs) == 0 {
		t.Fatalf("questions.json has no questions: %v", round2)
	}
	round2["questions"] = qs[:1]
	asked, plan := plGolden(t, "questions"), plGolden(t, "epic-plan")
	p.outputs(round1, round2, wholeRunOutput(t, "spec"), asked, plan)
	// At p1, the priority the golden plan is rendered at.
	p.n = f.AddIssue(wholeRunOwner, "Quote expiry", wholeRunBody, "type:epic", "priority:p1", "stage:intake")
	if p.n != 1 {
		t.Fatalf("the epic is #%d; want #1, which the writes' paths name", p.n)
	}
	p.eve = f.AddComment(p.n, "eve", wholeRunEve)
	labels := func(stage string) []string { return []string{"priority:p1", "stage:" + stage, "type:epic"} }
	react := func(id int64) string { return fmt.Sprintf("POST /issues/comments/%d/reactions", id) }
	// The digest is #2, which the first pass creates and pins, and each
	// change to what waits on you edits. #1 is assigned to notify while a
	// round, the spec or the plan waits on you.
	const (
		digestEdit = "PATCH /issues/2"
		assign     = "POST /issues/1/assignees"
		unassign   = "DELETE /issues/1/assignees"
	)

	// 1. The interviewer's two rounds and the spec, at spec-review.
	p.iterate("round 1", 1, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:intake",
		"POST /issues", "POST /graphql", digestEdit, "POST /issues/1/comments")
	p.settle("round 1", digestEdit, assign)
	a1 := f.AddComment(p.n, wholeRunOwner, wholeRunAnswer)
	p.iterate("round 2", 2, react(a1), digestEdit, unassign, "POST /issues/1/comments")
	p.settle("round 2", digestEdit, assign)
	a2 := f.AddComment(p.n, wholeRunOther, "1b")
	p.iterate("the spec", 3, react(a2), digestEdit, unassign,
		"PATCH /issues/1", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying")
	// At spec-review, step 1 reads the revision's reactions each pass.
	p.iterate("the spec, the pass after", 3, digestEdit, assign)
	p.quiet("the spec")
	p.labels("the spec", labels("spec-review")...)
	spec := p.comments()[len(p.comments())-1]
	specRev, ok := state.LatestRevision(p.snapshot(p.n), slug, state.SpecRevision, func(int64) bool { return false })
	if !ok || specRev.Rev != 1 || specRev.Comment != spec.ID {
		t.Fatalf("the spec: its latest spec revision = %+v (found %t); want rev 1 in comment %d", specRev, ok, spec.ID)
	}

	// 2. Your /approve moves it to planning, and gets a 👍. The planner runs
	// at once, in the same pass: it asks round 3, after the interviewer's
	// 1 and 2, which waits in the digest, with you assigned, from the pass
	// after.
	approveCmd := f.AddComment(p.n, wholeRunOwner, "/approve")
	p.iterate("the approval and round 3", 4, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:spec-review",
		react(approveCmd), digestEdit, unassign, "POST /issues/1/comments")
	p.labels("round 3", labels("planning")...)
	cs := p.comments()
	approval := cs[len(cs)-2]
	if want := fmt.Sprintf("<!-- issuedog:transition from=spec-review to=planning rev=1 body=%s cmd=%d", specRev.Hash, approveCmd); approval.Author.Login != app || !strings.Contains(approval.Body, want) {
		t.Errorf("the approval, by %s, =\n%s\nwant the App's, with %q", approval.Author.Login, approval.Body, want)
	}
	round3 := cs[len(cs)-1]
	if want := fmt.Sprintf("### Questions for #%d (round 3)\n\n**1. ", p.n); round3.Author.Login != app || !strings.HasPrefix(round3.Body, want) ||
		!strings.HasSuffix(round3.Body, fmt.Sprintf("\n\ncc @%s\n\n<!-- issuedog:transition from=planning to=planning round=3 run=run4 model=sonnet questions=2 seen=%d -->", wholeRunOwner, approval.ID)) {
		t.Errorf("round 3, by %s, =\n%s\nwant the App's round 3 of 2 questions, then the cc line and the planner's move", round3.Author.Login, round3.Body)
	}
	files := p.context(4, state.Planning)
	if system := files["system.md"]; !strings.Contains(system, "A question round now is round 3.") || !strings.Contains(system, "The interviewer asked them all.") {
		t.Errorf("planner run 1: system.md =\n%s\nwant round 3 next, after the interviewer's", system)
	}
	if input := files["input.md"]; !strings.Contains(input, "\nnone: first run\n") {
		t.Errorf("planner run 1: input.md =\n%s\nwant none: the first run", input)
	}
	ivOnlyIn(t, files, wholeRunEve, "untrusted.md")
	for k := 1; k <= 3; k++ {
		if flags := p.flags(k); flags["--max-turns"] != "30" || flags["--model"] != "" {
			t.Errorf("interviewer run %d: --max-turns %q and --model %q; want the interviewer's 30 and no model", k, flags["--max-turns"], flags["--model"])
		}
	}
	p.plannerFlags(4)
	p.settle("round 3", digestEdit, assign)
	p.waiting("round 3", []string{"#1 Question round p1 1"}, wholeRunOwner)
	f.Advance(time.Hour)
	p.unchanged("round 3, an hour on")

	// 3. Your answer gets a 👍, which takes round 3 out of the digest and
	// unassigns you, and the planner's second run, with the answer as its
	// input, posts the plan at plan-review. The plan waits in the digest,
	// with you assigned, from the pass after.
	a3 := f.AddComment(p.n, wholeRunOwner, "1a 2a")
	p.iterate("the plan", 5, react(a3), digestEdit, unassign,
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	p.acked("the plan", a1, a2, approveCmd, a3)
	files = p.context(5, state.Planning)
	if want := fmt.Sprintf("## Comment %d by %s: an answer to round 3\n\n```text\n1a 2a\n```", a3, wholeRunOwner); !strings.Contains(files["input.md"], want) {
		t.Errorf("planner run 2: input.md =\n%s\nwant\n%s", files["input.md"], want)
	}
	if want := fmt.Sprintf("## Comment %d by issuedog: Round 3, Q3.1–3.2\n", round3.ID); !strings.Contains(files["conversation.md"], want) {
		t.Errorf("planner run 2: conversation.md =\n%s\nwant round 3 under %q", files["conversation.md"], want)
	}
	p.plannerFlags(5)
	planned := p.planComment("the plan", "epic", 123,
		fmt.Sprintf("<!-- issuedog:transition from=planning to=plan-review rev=1 run=run5 model=sonnet seen=%d -->", a3), plan)
	p.labels("the plan", labels("plan-review")...)
	p.settle("the plan", digestEdit, assign)
	p.waiting("the plan", []string{"#1 Plan approval p1 1"}, wholeRunOwner)

	// 4. Your 👍 on the plan comment approves the plan, and no spec: step 1
	// moves the epic to planned, with plan revision 1 and the 👍 as its
	// command, which takes the plan out of the digest and unassigns you.
	// The restart reads it, and writes nothing.
	thumb := f.AddReaction(planned.ID, wholeRunOwner, "+1")
	p.iterate("a 👍 on the plan", 5, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		digestEdit, unassign)
	p.labels("a 👍 on the plan", labels("planned")...)
	p.waiting("a 👍 on the plan", nil)
	cs = p.comments()
	planHash, ok := state.PlanHash(planned.Body)
	if !ok {
		t.Fatalf("a 👍 on the plan: the plan comment has no plan hash:\n%s", planned.Body)
	}
	if want := fmt.Sprintf("<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=r%d ", planHash, thumb); cs[len(cs)-1].Author.Login != app || !strings.Contains(cs[len(cs)-1].Body, want) {
		t.Errorf("a 👍 on the plan: the newest comment, by %s, =\n%s\nwant the App's approval, with %q", cs[len(cs)-1].Author.Login, cs[len(cs)-1].Body, want)
	}
	// The approval's own writes settle the digest and assignment, so the
	// pass after it only reads the epic again, and writes nothing.
	p.settle("a 👍 on the plan")
	p.restart("a 👍 on the plan")
	p.labels("the end", labels("planned")...)
	s := p.snapshot(p.n)
	got, ok := state.LatestRevision(s, slug, state.SpecRevision, func(int64) bool { return false })
	if !ok || got != specRev || !got.Matches(s) {
		t.Errorf("the end: the latest spec revision = %+v (found %t, matches %t); want it as it was, %+v", got, ok, got.Matches(s), specRev)
	}
	if approved, c, ok := state.ApprovedPlan(s, slug, func(int64) bool { return false }); !ok || approved.Rev != 1 || c.ID != planned.ID {
		t.Errorf("the end: the approved plan = %+v in comment %d (found %t); want plan revision 1, in comment %d", approved, c.ID, ok, planned.ID)
	}
	approvals, planApprovals := 0, 0
	for _, c := range p.comments() {
		if strings.Contains(c.Body, " to=planning rev=1 ") {
			approvals++
		}
		if strings.Contains(c.Body, " to=planned rev=1 ") {
			planApprovals++
		}
	}
	if approvals != 1 || planApprovals != 1 || p.plans() != 1 {
		t.Errorf("the end: %d spec approvals, %d plan approvals and %d plans; want the one spec approval, by /approve, the one plan approval, by your 👍, and one plan",
			approvals, planApprovals, p.plans())
	}
	f.Advance(time.Hour)
	p.unchanged("the end, an hour on")
}

// TestPlannerWholeRunStory: a code owner sets stage:planning on a story
// with a parent. The planner runs at once, and with plan.review false, the
// story goes straight to planned, with one plan comment that ends with the
// skip line, not the review lines, and nothing waits in the digest. A
// story back at planning from a discussion hold, whose move there carries
// origin, goes to plan-review instead, whatever plan.review says: its plan
// asks for review, and waits in the digest with notify assigned.
func TestPlannerWholeRunStory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// hold puts the story at planning as the interviewer leaves it after
		// a discussion hold; else a code owner sets stage:planning on it.
		hold   bool
		golden string // in testdata/plan
		to     string
		// waits is the digest's items once the plan settles: the plan's
		// approval, at plan-review, which assigns notify.
		waits []string
	}{
		{"set by a code owner", false, "story-no-review", "planned", nil},
		{"back from a discussion hold", true, "story", "plan-review", []string{"#2 Plan approval p2 1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := newPlanRun(t)
			f := p.f
			parent := f.AddIssue(wholeRunOwner, "Quote expiry", wholeRunBody, "type:epic")
			f.AddLabel(parent, app, "stage:planned")
			f.AddComment(parent, app, "Planned.\n\n<!-- issuedog:transition from=plan-review to=planned cmd=r1 rev=1 -->")
			plan := plGolden(t, "story-plan")
			p.outputs(plan)
			p.n = f.AddIssue(wholeRunOwner, "Refuse expired quotes", "Booking refuses an expired quote.", "type:story")
			if p.n != 2 {
				t.Fatalf("the story is #%d; want #2, which the writes' paths name", p.n)
			}
			f.SetParent(p.n, parent)
			// The owner's stage is recorded first, then the digest is created
			// as #3, and the planner's plan moves the story on.
			want := []string{"POST /issues/2/comments", "POST /issues", "POST /graphql", "PATCH /issues/3",
				"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:planning"}
			if tc.hold {
				f.AddLabel(p.n, app, "stage:planning")
				entry := f.AddComment(p.n, app, "Approved.\n\n<!-- issuedog:transition "+plEntry+" -->")
				f.AddComment(p.n, app, fmt.Sprintf("Back to planning.\n\n<!-- issuedog:transition from=clarifying to=planning run=iv9 origin=planning seen=%d -->", entry))
				want = want[1:]
			} else {
				f.AddLabel(p.n, wholeRunOwner, "stage:planning")
			}

			p.iterate("the plan", 1, want...)
			files := p.context(1, state.Planning)
			if !strings.HasPrefix(files["parent.md"], fmt.Sprintf("# Parent #%d\n", parent)) {
				t.Errorf("the planner's run: parent.md =\n%s\nwant the parent #%d", files["parent.md"], parent)
			}
			p.plannerFlags(1)
			cs := p.comments()
			entry := cs[len(cs)-2]
			if want := fmt.Sprintf(" to=planning by=%s ", wholeRunOwner); !tc.hold && (entry.Author.Login != app || !strings.Contains(entry.Body, want)) {
				t.Errorf("the owner's stage, recorded by %s, =\n%s\nwant the App's, with %q", entry.Author.Login, entry.Body, want)
			}
			p.planComment("the plan", tc.golden, 124,
				fmt.Sprintf("<!-- issuedog:transition from=planning to=%s rev=1 run=run1 model=sonnet seen=%d -->", tc.to, entry.ID), plan)
			if p.plans() != 1 {
				t.Errorf("%d plan comments; want 1", p.plans())
			}
			if body := cs[len(cs)-1].Body; strings.Contains(body, "To approve") != tc.hold || strings.Contains(body, "skips review") == tc.hold {
				t.Errorf("the plan =\n%s\nwant it to ask for review only back from a discussion hold, and to say it skips review otherwise", body)
			}
			p.labels("the plan", "stage:"+tc.to, "type:story")
			// A plan at plan-review is a digest item, with notify assigned,
			// from the pass after; a planned story waits on no one.
			if tc.waits != nil {
				p.settle("the plan", "PATCH /issues/3", "POST /issues/2/assignees")
				p.waiting("the plan", tc.waits, wholeRunOwner)
			} else {
				p.settle("the plan")
				p.waiting("the plan", nil)
			}
			p.restart("the plan")
		})
	}
}

// TestPlannerWholeRunFails: a code owner sets stage:planning on an epic,
// and the planner's plan names a scope that isn't on the list, on every
// run. After loop.max_attempts runs, 3, there's one kind=attempts error,
// naming the scope rule, and no run follows: not on the next pass, not
// after a restart, and not for eve's comment. Your comment starts a new
// run, whose plan is posted, and waits in the digest with you assigned.
func TestPlannerWholeRunFails(t *testing.T) {
	t.Parallel()
	p := newPlanRun(t)
	f := p.f
	bad := plGolden(t, "story-plan")
	plItem(t, bad, 2)["scope"] = "web"
	plan := plGolden(t, "epic-plan")
	p.outputs(bad, bad, bad, plan)
	p.n = f.AddIssue(wholeRunOwner, "Quote expiry", wholeRunBody, "type:epic")
	f.AddLabel(p.n, wholeRunOwner, "stage:planning")

	// Each pass makes one attempt, which writes nothing; the third writes
	// the one kind=attempts error.
	p.iterate("attempt 1", 1, "POST /issues/1/comments", "POST /issues", "POST /graphql", "PATCH /issues/2")
	p.iterate("attempt 2", 2)
	p.iterate("attempt 3", 3, "POST /issues/1/comments")
	cs := p.comments()
	errs := 0
	for _, c := range cs {
		if strings.Contains(c.Body, "<!-- issuedog:error kind=attempts ") {
			errs++
		}
	}
	stopped := cs[len(cs)-1].Body
	if errs != 1 || !strings.HasPrefix(stopped, "The planner run failed 3 times in a row, so I've stopped running it here. ") ||
		!strings.Contains(stopped, "\n\nThe last attempt failed:\n\n```text\nits result failed the planner stage's check: ") ||
		!strings.Contains(stopped, `rule scope: T3's scope "web" isn't on the list`) ||
		!strings.HasSuffix(stopped, fmt.Sprintf("\n```\n\n<!-- issuedog:error kind=attempts key=planning-%d -->", cs[0].ID)) {
		t.Errorf("%d attempts errors, the newest comment =\n%s\nwant one, naming the scope rule, fenced", errs, stopped)
	}
	p.labels("attempt 3", "stage:planning", "type:epic")
	// The error waits in the digest, announced, with you assigned.
	p.settle("attempt 3", "POST /issues/2/comments", "PATCH /issues/2", "POST /issues/1/assignees")
	p.restart("attempt 3")
	f.AddComment(p.n, "eve", wholeRunEve)
	p.iterate("eve's comment", 3)
	p.unchanged("eve's comment")

	// Your comment gets a 👍, takes the error out of the digest, and starts
	// a new count: its run's plan is posted, and the pass after lists it in
	// the digest and assigns you.
	again := f.AddComment(p.n, wholeRunOwner, "Please try again: T3's scope is infra.")
	p.iterate("your comment", 4, fmt.Sprintf("POST /issues/comments/%d/reactions", again), "PATCH /issues/2", "DELETE /issues/1/assignees",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	p.labels("your comment", "stage:plan-review", "type:epic")
	cs = p.comments()
	if want := fmt.Sprintf("\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=run4 model=sonnet seen=%d -->\n", again); !strings.Contains(cs[len(cs)-1].Body, want) {
		t.Errorf("your comment: the newest comment =\n%s\nwant run 4's plan, with %q", cs[len(cs)-1].Body, want)
	}
	p.settle("your comment", "PATCH /issues/2", "POST /issues/1/assignees")
	p.waiting("your comment", []string{"#1 Plan approval p2 1"}, wholeRunOwner)
}
