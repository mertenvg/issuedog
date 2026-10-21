package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
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
	g.d.Step = daemon.NewStep(g.d.Step.Applier, g.d.Step.Trust, g.d.CurrentModels)
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

// childWrites are the writes that file the children of issue parent's
// approved plan, issues ns, in plan order (#276): for each, its create,
// its link under parent and its first move.
func childWrites(parent int, ns ...int) []string {
	var w []string
	for _, n := range ns {
		w = append(w, "POST /issues", fmt.Sprintf("POST /issues/%d/sub_issues", parent), fmt.Sprintf("POST /issues/%d/comments", n))
	}
	return w
}

// blockWrites are the writes that add blocked-by links to issues ns, one
// each, in order (#277): the links follow plan order, one for each item a
// child's item depends on.
func blockWrites(ns ...int) []string {
	var w []string
	for _, n := range ns {
		w = append(w, fmt.Sprintf("POST /issues/%d/dependencies/blocked_by", n))
	}
	return w
}

// recordWrite is the write that posts the record of the children on
// issue parent (#277), the creating pass's last for creation.
func recordWrite(parent int) string { return fmt.Sprintf("POST /issues/%d/comments", parent) }

// runRecord checks, at step, that the children of issue parent's plan
// revision rev, want, filed as issues first on, are each blocked by the
// issues blockedBy names for it, and by nothing else, and that parent has
// the one record of them (#277).
func runRecord(t *testing.T, f *githubtest.Fake, step string, parent, rev, first int, want []wantChild, blockedBy map[int][]int) {
	t.Helper()
	st := f.State()
	for i := range want {
		n := first + i
		if got := st.Issues[n-1].BlockedBy; !slices.Equal(got, blockedBy[n]) {
			t.Errorf("%s: #%d is blocked by %v; want %v", step, n, got, blockedBy[n])
		}
	}
	if got, want := records(st.Issues[parent-1]), []string{recordOf(rev, first, want)}; !slices.Equal(got, want) {
		t.Errorf("%s: #%d's records are\n%q\nwant\n%q", step, parent, got, want)
	}
}

// childrenLooked reports whether a request f had from from on is the
// issue list a creating visit reads its children from (childrenFind),
// since at, when its parent moved into planned: no other request lists
// issues since then, as the poll's cursor is past it by then.
func childrenLooked(f *githubtest.Fake, from int, at time.Time) bool {
	since := "since=" + url.QueryEscape(at.UTC().Format(time.RFC3339))
	return slices.ContainsFunc(f.Requests()[from:], func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && r.Path == "/issues" && strings.Contains(r.Query, since)
	})
}

// recordStands checks, at step, that the requests f had from from on made
// none for the creation of issue parent's children, which it moved into
// planned at at (#277): once its record stands, a visit to parent doesn't
// look for its children (childrenLooked), nor reads what blocks the
// children dependents, which depend on others, nor files, links or blocks
// any. Other work may still list sub-issues, as the planner's context
// does, or what blocks a human task, as its digest item does. With reread,
// the requests must also visit parent again, as the record, its last
// write, changed it: a visit reads its timeline, for its labels. A GET of
// parent itself tells nothing: it may be a 304, and the planner's context
// reads a story's parent too.
func recordStands(t *testing.T, f *githubtest.Fake, step string, from, parent int, at time.Time, dependents []int, reread bool) {
	t.Helper()
	if childrenLooked(f, from, at) {
		t.Errorf("%s: a pass with #%d's record standing looked for its children again", step, parent)
	}
	read := false
	for _, r := range f.Requests()[from:] {
		blocks := slices.ContainsFunc(dependents, func(n int) bool { return r.Path == fmt.Sprintf("/issues/%d/dependencies/blocked_by", n) })
		if r.Method == http.MethodPost && (r.Path == "/issues" || strings.HasSuffix(r.Path, "/sub_issues") || strings.Contains(r.Path, "/dependencies/")) || blocks {
			t.Errorf("%s: a pass with #%d's record standing sent %+v; want no request for creation", step, parent, r)
		}
		if r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/%d/timeline", parent) {
			read = true
		}
	}
	if reread && !read {
		t.Errorf("%s: #%d wasn't read again after its record", step, parent)
	}
}

// planRunStories are the children of the golden epic plan (T4's
// epic-plan.json) under an epic at p1, on a repo that types by label: its
// three stories, each at planning, S2 at the epic's priority, as it names
// none, and each with the model:* and effort:* labels of its item's model
// and effort (#344): S1's opus and high, and S3's medium.
var planRunStories = []wantChild{
	{"S1", "S1. Quotes expire after 24 hours", "", []string{"effort:high", "model:opus", "priority:p1", "stage:planning", "type:story"}, state.Planning},
	{"S2", "S2. Customers see when a quote expires", "", []string{"priority:p1", "stage:planning", "type:story"}, state.Planning},
	{"S3", "S3. Expired quotes in the monthly report", "", []string{"effort:medium", "priority:p2", "stage:planning", "type:story"}, state.Planning},
}

// planRunTasks are the children of the golden story plan (T4's
// story-plan.json) under a story with no priority, on a repo that types by
// label: T1 and T2 at ready, T1 with its item's sonnet and medium as its
// model:* and effort:* labels (#344), T2 at its own p1, and T3, a human
// task, at needs-human.
var planRunTasks = []wantChild{
	{"T1", "Record when each quote expires", "", []string{"effort:medium", "model:sonnet", "priority:p2", "scope:booking", "stage:ready", "type:task"}, state.Ready},
	{"T2", "Refuse an expired quote at booking", "", []string{"priority:p1", "scope:booking", "stage:ready", "type:task"}, state.Ready},
	{"T3", "Set the quote lifetime in production", "", []string{"priority:p2", "scope:infra", "stage:needs-human", "type:task"}, state.NeedsHuman},
}

// planRunHumanOrigin is what the origin marker of the golden story plan's
// human task, T3, holds after its item: its check, that production's
// settings file holds the lifetime, and that it's human.
const planRunHumanOrigin = " check=file contains=QUOTE_TTL=24h human=1 value=deploy/production.env"

// runChildren checks, at step, that the approved plan revision rev of issue
// parent filed want's children on f, as issues first on, in plan order,
// and nothing after them (#276): each the App's, with its title, type,
// labels and origin, linked under parent in that order, and with its first
// move, keyed by p, the parent's move into planned, as its first comment
// and its only move out of intake.
func runChildren(t *testing.T, f *githubtest.Fake, step string, parent, rev int, p int64, first int, want []wantChild) {
	t.Helper()
	st := f.State()
	if got := len(st.Issues) - (first - 1); got != len(want) {
		t.Fatalf("%s: %d issues from #%d on; want the plan's %d children", step, got, first, len(want))
	}
	var subs []int
	for i, w := range want {
		is := st.Issues[first-1+i]
		subs = append(subs, is.Number)
		human := "" // a human task's origin holds its check (#327) and says it's human
		if w.stage == state.NeedsHuman {
			human = planRunHumanOrigin
		}
		origin := fmt.Sprintf("<!-- issuedog:origin plan=#%d rev=%d item=%s%s -->", parent, rev, w.item, human)
		if labels := ivLabels(f, is.Number); is.Author != app || is.Title != w.title || is.Type != w.typ || !slices.Equal(labels, w.labels) ||
			is.Parent != parent || !strings.HasSuffix(is.Body, "\n\n"+origin) {
			t.Errorf("%s: child #%d is by %s, %q, of type %q, labelled %v, under #%d, with body\n%s\nwant the App's %q, of type %q, labelled %v, under #%d, ending in %s",
				step, is.Number, is.Author, is.Title, is.Type, labels, is.Parent, is.Body, w.title, w.typ, w.labels, parent, origin)
		}
		moves := 0
		for _, c := range is.Comments {
			if strings.Contains(c.Body, "<!-- issuedog:transition from=intake ") {
				moves++
			}
		}
		if move := firstMoveUnder(parent, rev, w, p); len(is.Comments) == 0 || is.Comments[0].Author != app || is.Comments[0].Body != move || moves != 1 {
			t.Errorf("%s: child #%d's comments are %+v; want the App's first move first, and no other move out of intake:\n%s", step, is.Number, is.Comments, move)
		}
	}
	if got := st.Issues[parent-1].SubIssues; !slices.Equal(got, subs) {
		t.Errorf("%s: #%d's sub-issues are %v; want %v, in plan order", step, parent, got, subs)
	}
}

// storyRoundMarker is the transition marker of the round 1 that claude
// run k asked on a story, with the run's effort and model, "" for none,
// having seen comment seen, the story's first move.
func storyRoundMarker(k int, effort, model string, seen int64) string {
	flags := ""
	if effort != "" {
		flags += " effort=" + effort
	}
	if model != "" {
		flags += " model=" + model
	}
	return fmt.Sprintf("<!-- issuedog:transition from=planning to=planning round=1 run=run%d%s questions=2 seen=%d -->", k, flags, seen)
}

// storyRound checks that claude run k was the planner's on story n, a
// child of issue p.n, at planning, with the planner's 60 turns, and model
// and effort as its --model and --effort, "" for no flag: the story's
// model:* and effort:* labels, else the planner's sonnet and no effort
// (SPEC §3.3). The story's newest comment must be the round 1 it asked,
// having seen the story's first move.
func (p *planRun) storyRound(step string, n, k int, model, effort string) {
	p.t.Helper()
	files := (&wholeRun{rig: p.rig, n: n}).context(k, state.Planning)
	if !strings.HasPrefix(files["parent.md"], fmt.Sprintf("# Parent #%d\n", p.n)) {
		p.t.Errorf("%s: planner run %d: parent.md =\n%s\nwant the parent #%d", step, k, files["parent.md"], p.n)
	}
	cs := p.snapshot(n).Comments
	if len(cs) != 2 {
		p.t.Fatalf("%s: #%d has %d comments; want its first move, then the planner's round", step, n, len(cs))
	}
	round := cs[1]
	want := storyRoundMarker(k, effort, model, cs[0].ID)
	if round.Author.Login != app || !strings.HasPrefix(round.Body, fmt.Sprintf("### Questions for #%d (round 1)\n\n**1. ", n)) ||
		!strings.HasSuffix(round.Body, "\n\ncc @"+wholeRunOwner+"\n\n"+want) {
		p.t.Errorf("%s: #%d's newest comment, by %s, =\n%s\nwant the App's round 1 of 2 questions, then the cc line and %s", step, n, round.Author.Login, round.Body, want)
	}
	if turns := p.flags(k)["--max-turns"]; turns != "60" {
		p.t.Errorf("%s: planner run %d: --max-turns %q; want the planner's 60", step, k, turns)
	}
	p.checkFlags(k, model, effort, "")
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
// go, and the same pass files the plan's three stories, each linked under
// the epic and moved to planning (#276). The planner then runs on each
// story in turn, one a pass, and asks its round 1, which waits in the
// digest with notify assigned on the story. The spec's revision and
// approval are as they were, each story was filed once, and a restart
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
	// command. In the same pass, the plan's three stories are filed as #3
	// to #5, in plan order, each created, linked under the epic and moved
	// to planning, keyed by the approval (#276). Then S2 and S3, which
	// depend on S1, are blocked by it, and the record of the stories is
	// posted on the epic, last (#277). Then the plan leaves the digest and
	// you're unassigned.
	thumbFrom := len(f.Requests())
	thumb := f.AddReaction(planned.ID, wholeRunOwner, "+1")
	p.iterate("a 👍 on the plan", 5, slices.Concat(
		[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review"},
		childWrites(p.n, 3, 4, 5), blockWrites(4, 5), []string{recordWrite(p.n)},
		[]string{digestEdit, unassign})...)
	p.labels("a 👍 on the plan", labels("planned")...)
	p.waiting("a 👍 on the plan", nil)
	cs = p.comments()
	planHash, ok := state.PlanHash(planned.Body)
	if !ok {
		t.Fatalf("a 👍 on the plan: the plan comment has no plan hash:\n%s", planned.Body)
	}
	// The newest comment is the record, and the approval is just before it.
	approvedBy := cs[len(cs)-2]
	if want := fmt.Sprintf("<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=r%d ", planHash, thumb); approvedBy.Author.Login != app || !strings.Contains(approvedBy.Body, want) {
		t.Errorf("a 👍 on the plan: the comment before the record, by %s, =\n%s\nwant the App's approval, with %q", approvedBy.Author.Login, approvedBy.Body, want)
	}
	if record := cs[len(cs)-1]; record.Author.Login != app || record.Body != recordOf(1, 3, planRunStories) {
		t.Errorf("a 👍 on the plan: the newest comment, by %s, =\n%s\nwant the App's record\n%s", record.Author.Login, record.Body, recordOf(1, 3, planRunStories))
	}
	runChildren(t, f, "a 👍 on the plan", p.n, 1, approvedBy.ID, 3, planRunStories)
	runRecord(t, f, "a 👍 on the plan", p.n, 1, 3, planRunStories, map[int][]int{4: {3}, 5: {3}})
	if !childrenLooked(f, thumbFrom, approvedBy.CreatedAt) {
		t.Errorf("a 👍 on the plan: the pass didn't look for the epic's children in the issues listed since its approval")
	}

	// 5. The planner runs on each new story in turn, one a pass, in plan
	// order: the stories skipped the rest of the pass that filed them. A
	// story's model:* and effort:* labels set its run's --model and
	// --effort (SPEC §3.3): S1's opus and high, S2's none, so the planner's
	// sonnet and no effort, and S3's medium, with sonnet. Each run asks the
	// story's round 1, which the pass after lists in the
	// digest, assigning you on the story. The epic, read again as the
	// approval and its record changed it, makes no request for creation:
	// its record stands (#277).
	ask := plGolden(t, "questions")
	p.outputs(ask, ask, ask)
	from := len(f.Requests())
	p.iterate("S1's round", 6, "POST /issues/3/comments")
	recordStands(t, f, "S1's round", from, p.n, approvedBy.CreatedAt, []int{4, 5}, true)
	p.storyRound("S1's round", 3, 6, "opus", "high")
	p.iterate("S2's round", 7, digestEdit, "POST /issues/3/assignees", "POST /issues/4/comments")
	p.storyRound("S2's round", 4, 7, "sonnet", "")
	p.iterate("S3's round", 8, digestEdit, "POST /issues/4/assignees", "POST /issues/5/comments")
	p.storyRound("S3's round", 5, 8, "sonnet", "medium")
	// The pass after S3's round lists it, and the pass after that reads
	// back its own writes; then a pass where nothing changed makes no
	// request beyond the poll.
	p.settle("the stories' rounds", digestEdit, "POST /issues/5/assignees")
	rounds := []string{"#3 Question round p1 1", "#4 Question round p1 1", "#5 Question round p2 1"}
	p.waiting("the stories' rounds", rounds)
	owner := []string{wholeRunOwner}
	assignWant(t, f, "the stories' rounds", map[int][]string{3: owner, 4: owner, 5: owner})
	recordStands(t, f, "the stories' rounds", from, p.n, approvedBy.CreatedAt, []int{4, 5}, true)
	from = len(f.Requests())
	p.restart("the stories' rounds")
	recordStands(t, f, "the restart", from, p.n, approvedBy.CreatedAt, []int{4, 5}, true)
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
	runChildren(t, f, "the end", p.n, 1, approvedBy.ID, 3, planRunStories)
	runRecord(t, f, "the end", p.n, 1, 3, planRunStories, map[int][]int{4: {3}, 5: {3}})
	p.waiting("the end", rounds)
	f.Advance(time.Hour)
	p.unchanged("the end, an hour on")
}

// TestPlannerWholeRunStory: a code owner sets stage:planning on a story
// with a parent. The planner runs at once, and with plan.review false, the
// story goes straight to planned, with one plan comment that ends with the
// skip line, not the review lines, and the story waits in the digest for
// no one. The pass after files the plan's three tasks, once each, linked
// under the story and at their first stages (#276), and the pass after
// that lists the human task, which nothing blocks, in the digest, with
// notify assigned on it (#336). A story back at planning from a discussion
// hold, whose move there carries origin, goes to plan-review instead,
// whatever plan.review says: its plan asks for review, and waits in the
// digest with notify assigned.
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
			planned := p.planComment("the plan", tc.golden, 124,
				fmt.Sprintf("<!-- issuedog:transition from=planning to=%s rev=1 run=run1 model=sonnet seen=%d -->", tc.to, entry.ID), plan)
			if p.plans() != 1 {
				t.Errorf("%d plan comments; want 1", p.plans())
			}
			if body := cs[len(cs)-1].Body; strings.Contains(body, "To approve") != tc.hold || strings.Contains(body, "skips review") == tc.hold {
				t.Errorf("the plan =\n%s\nwant it to ask for review only back from a discussion hold, and to say it skips review otherwise", body)
			}
			p.labels("the plan", "stage:"+tc.to, "type:story")
			// A plan at plan-review is a digest item, with notify assigned,
			// from the pass after; a planned story waits on no one. Its plan
			// files its three tasks, #4 to #6, in plan order, each created,
			// linked under the story and moved to its first stage, keyed by
			// the plan comment's move into planned (#276). That's in the pass
			// after, as the planner's move came after the pass's visits. Then
			// T2, which depends on T1 and T3, the human task, is blocked by
			// both, and the record of the tasks is posted on the story, last
			// (#277). The pass after that reads the story again, as the
			// record changed it, and makes no request for creation, as the
			// record stands. It lists the human task, #6, which nothing
			// blocks, in the digest, and assigns notify on it (#336), and the
			// pass after its writes reads them back. No task has a stage
			// that runs.
			from := len(f.Requests())
			if tc.waits != nil {
				p.settle("the plan", "PATCH /issues/3", "POST /issues/2/assignees")
				p.waiting("the plan", tc.waits, wholeRunOwner)
			} else {
				p.iterate("the plan, the pass after", 1, slices.Concat(childWrites(p.n, 4, 5, 6), blockWrites(5, 5), []string{recordWrite(p.n)})...)
				runChildren(t, f, "the plan", p.n, 1, planned.ID, 4, planRunTasks)
				runRecord(t, f, "the plan", p.n, 1, 4, planRunTasks, map[int][]int{5: {4, 6}})
				if !childrenLooked(f, from, planned.CreatedAt) {
					t.Errorf("the plan: the pass didn't look for the story's children in the issues listed since its move into planned")
				}
				from = len(f.Requests())
				p.settle("the tasks", "PATCH /issues/3", "POST /issues/6/assignees")
				p.waiting("the tasks", []string{"#6 Human task p2 1"})
				assignWant(t, f, "the tasks", map[int][]string{6: {wholeRunOwner}})
				recordStands(t, f, "the tasks", from, p.n, planned.CreatedAt, []int{5}, true)
				from = len(f.Requests())
			}
			p.restart("the plan")
			if !tc.hold {
				recordStands(t, f, "the restart", from, p.n, planned.CreatedAt, []int{5}, true)
				runChildren(t, f, "the restart", p.n, 1, planned.ID, 4, planRunTasks)
				runRecord(t, f, "the restart", p.n, 1, 4, planRunTasks, map[int][]int{5: {4, 6}})
				p.waiting("the restart", []string{"#6 Human task p2 1"})
			} else if n := len(f.State().Issues); n != 3 {
				t.Errorf("the restart: %d issues; want the epic, the story and the digest, as a plan at review files nothing", n)
			}
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
