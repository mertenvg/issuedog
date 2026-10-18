package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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

// This file is plan #250's T8 (#288): a whole run of human tasks against
// the fakes, from the plan that files them to their close. The stages are
// registered as registerStages registers them, step 1 is wired as main's
// newStep wires it, the Applier reads main from the daemon's clone, as
// startDaemon sets it, the digest and assignment are on, and
// Daemon.Iterate is the loop. Config and trust come from main on the
// origin: mertenvg owns CODEOWNERS and is notify, and eve can only read
// the repo, so CODEOWNERS doesn't trust her and her comments hold nothing.

// The run's people and text.
const (
	humanRunOwner = "mertenvg"
	humanRunEve   = "eve"
	humanRunBody  = "Quotes should expire after a lifetime production sets."
	// humanRunFile and humanRunLine are the file the first human task's
	// check reads on main, and the text it must hold.
	humanRunFile = "deploy/production.env"
	humanRunLine = "QUOTE_TTL=24h"
	// humanRunLabel is the label the second human task's check looks for.
	humanRunLabel = "quote-expired"
)

// humanRunPlan is the story's plan, which the fake planner returns: three
// human tasks and a task.
//   - T1, a human task with a file check: production's settings on main
//     hold the quote lifetime;
//   - T2, a human task with a label check: the repo has the label
//     quote-expired;
//   - T3, a human task whose "Done when" is plain text, which depends on
//     T4;
//   - T4, a task.
//
// So the children are filed as T4, then T1, T2 and T3, the human tasks
// last, in plan order, as none depends on another: #3 to #6.
const humanRunPlan = `{
  "outcome": "plan",
  "plan": {
    "kind": "tasks",
    "why": "One PR in booking, and three things only the owner can do.",
    "summary": "**Today** quotes never expire.\n\nThis plan adds the lifetime in production (T1), the label for expired quotes (T2), the FAQ entry (T3) and the expiry at booking (T4).",
    "assumptions": ["The lifetime is 24 hours in production."],
    "order": "T1, T2 and T4 in parallel, then T3",
    "items": [
      {"id": "T1", "title": "Set the quote lifetime in production", "outcome": "Production runs with a quote lifetime of 24 hours.",
       "criteria": ["Production's settings on main set the quote lifetime to 24 hours."], "depends_on": [], "scope": "infra", "human": true,
       "human_task": {"goal": "Set the quote lifetime to 24 hours in production's settings.",
         "why_blocks": "production has a quote lifetime, since the booking service won't start without one.",
         "steps": ["Add the line QUOTE_TTL=24h to production's settings file on main."],
         "links": ["https://github.com/o/r/blob/main/deploy/production.env"], "estimated_time": "5 min",
         "done_when": {"text": "Production's settings on main set the quote lifetime.", "check": {"kind": "file", "path": "deploy/production.env", "contains": "QUOTE_TTL=24h"}}}},
      {"id": "T2", "title": "Add the label for expired quotes", "outcome": "Support can label reports of expired quotes.",
       "criteria": ["The repo has the label quote-expired."], "depends_on": [], "scope": "infra", "human": true,
       "human_task": {"goal": "Add the label quote-expired to the repo.",
         "why_blocks": "support can label the reports of expired quotes.",
         "steps": ["Open the repo's labels.", "Add the label quote-expired."],
         "links": ["https://github.com/o/r/labels"], "estimated_time": "1 min",
         "done_when": {"text": "The repo has the label quote-expired.", "check": {"kind": "label", "label": "quote-expired"}}}},
      {"id": "T3", "title": "Explain quote expiry in the FAQ", "outcome": "Customers read why a quote expired.",
       "criteria": ["The FAQ explains quote expiry."], "depends_on": ["T4"], "scope": "docs", "human": true,
       "human_task": {"goal": "Add a FAQ entry on why quotes expire.",
         "why_blocks": "customers can read why a quote expired.",
         "steps": ["Open the FAQ page in the CMS.", "Add an entry on quote expiry, and publish it."],
         "links": ["https://example.com/cms/faq"], "estimated_time": "15 min",
         "done_when": {"text": "The FAQ page explains why quotes expire."}}},
      {"id": "T4", "title": "Refuse an expired quote at booking", "outcome": "Booking an expired quote returns an expired error.",
       "criteria": ["Booking a quote after its expiry time returns 410."], "depends_on": [], "scope": "booking", "human": false,
       "task": {"context": "- The booking service books any quote.", "change": "- Refuse a quote past its lifetime.",
         "definition_of_done": ["Tests added or updated for every changed behavior"], "test_plan": "- A quote past its lifetime is refused."}}
    ]
  }
}`

// humanRunOutput is humanRunPlan, decoded, for the fake planner.
func humanRunOutput(t testing.TB) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(humanRunPlan), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// humanRunLoop is the whole run's loop on f: a rig on the state directory
// root, rebuilt by start as a restart builds the daemon. seen is each
// issue's comment count before the last step, for added.
type humanRunLoop struct {
	t    *testing.T
	f    *githubtest.Fake
	o    *origin
	root string
	g    *rig
	seen map[int]int
}

// newHumanRunLoop starts a loop on f, on a new origin and state directory:
// main has mertenvg in CODEOWNERS and as notify, types by label, and the
// plan's scopes. Its fake claude prints the plan on its first run.
func newHumanRunLoop(t *testing.T, f *githubtest.Fake) *humanRunLoop {
	t.Helper()
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+humanRunOwner+"\n")
	o.commit(config.Path, "types: labels\nnotify: ["+humanRunOwner+"]\nscopes: [booking, infra, docs]\n")
	l := &humanRunLoop{t: t, f: f, o: o, root: t.TempDir()}
	l.start()
	if err := os.WriteFile(l.g.path("claude", "out.1"), []byte(ivResult(t, humanRunOutput(t))), 0o644); err != nil {
		t.Fatal(err)
	}
	return l
}

// start starts a daemon on l's fake, as a restart does: a new client, with
// no ETags, a new daemon, with no snapshots, and the same state directory,
// which outlives a restart. The stages are registered as registerStages
// registers them: the Interviewer at clarifying, the Planner at planning,
// and Triage as the Unlabeled stage. The Applier reads main from the
// daemon's clone, as startDaemon's does, for a file check. Step 1 is wired
// as newStep wires it (humanRunWire). Iterate loads the trust from main.
func (l *humanRunLoop) start() {
	l.t.Helper()
	g := newRig(l.t, l.f, l.o, l.root)
	// wholeRunClaude keeps each run's context and prints out.<n>, and fails
	// a run it has no output for, so an unexpected run shows.
	if err := os.WriteFile(g.path("claude", "claude"), []byte(wholeRunClaude), 0o755); err != nil {
		l.t.Fatal(err)
	}
	g.r.Stages = map[state.Stage]daemon.Stage{
		state.Clarifying: &daemon.Interviewer{Daemon: g.d},
		state.Planning:   &daemon.Planner{Daemon: g.d},
	}
	g.r.Unlabeled = &daemon.Triage{Daemon: g.d}
	g.d.Digest, g.d.Assign = true, true // as startDaemon sets them
	st := g.d.Step
	st.ReadMain = g.d.Clone.ReadFile // as startDaemon sets it
	st.Trust = nil                   // Iterate loads it from main
	humanRunWire(st, g.d.CurrentModels)
	l.g = g
}

// humanRunWire wires st as main's newStep does: every command handler,
// /done among them, and the reaction triggers in its order, the end of a
// discussion hold first, and a 👍 on a human task before a 👍 on a state
// error.
func humanRunWire(st *daemon.Step, models func() config.Models) {
	st.Models = models
	st.Commands = daemon.Commands(st.CurrentTrust())
	st.Commands["approve"] = daemon.ApproveCommand()
	st.Commands["revise"] = daemon.ReviseCommand()
	st.Commands["done"] = daemon.DoneCommand()
	maps.Copy(st.Commands, daemon.SettingCommands(st.CurrentTrust(), models))
	maps.Copy(st.Commands, daemon.FlagCommands())
	approve, answer := daemon.ApproveByReaction(st.CurrentTrust()), daemon.FlagByReaction(st.CurrentTrust())
	errAck, resolved := daemon.ErrorByReaction(st.CurrentTrust()), daemon.ResolveByReaction(st.CurrentTrust())
	done := daemon.DoneByReaction(st.CurrentTrust())
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
		if err := done(ctx, a, s, d); err != nil {
			return fmt.Errorf("human task done by 👍: %w", err)
		}
		if err := errAck(ctx, a, s, d); err != nil {
			return fmt.Errorf("state error ack by 👍: %w", err)
		}
		return nil
	}
}

// step runs one Iterate, which must succeed, and checks that it wrote
// want, as "METHOD /path" in order, and that the fake claude has run runs
// times in all.
func (l *humanRunLoop) step(name string, runs int, want ...string) {
	l.t.Helper()
	l.seen = map[int]int{}
	for _, is := range l.f.State().Issues {
		l.seen[is.Number] = len(is.Comments)
	}
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
func (l *humanRunLoop) quiet(name string) {
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
func (l *humanRunLoop) restart(name string) {
	l.t.Helper()
	l.start()
	runs := l.g.runs()
	l.step(name+": a restart", runs)
	l.step(name+": the restart's second pass", runs)
}

// added is the bodies of the comments on issue n since the last step began.
func (l *humanRunLoop) added(n int) []string {
	var bodies []string
	for _, c := range l.f.State().Issues[n-1].Comments[l.seen[n]:] {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// newest is issue n's newest comment.
func (l *humanRunLoop) newest(n int) github.Comment {
	l.t.Helper()
	s, err := l.f.Client(l.t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		l.t.Fatal(err)
	}
	return s.Comments[len(s.Comments)-1]
}

// state checks issue n's labels, sorted, and whether it's closed.
func (l *humanRunLoop) state(step string, n int, closed bool, labels ...string) {
	l.t.Helper()
	if got, is := ivLabels(l.f, n), l.f.State().Issues[n-1]; !slices.Equal(got, labels) || is.Closed != closed {
		l.t.Errorf("%s: #%d is labelled %v, closed %t; want %v, closed %t", step, n, got, is.Closed, labels, closed)
	}
}

// digest checks the digest's items, issue 2, as digestRunItems gives them,
// and who is assigned, by issue: an issue want leaves out has no one.
func (l *humanRunLoop) digest(step string, items []string, assigned ...int) {
	l.t.Helper()
	if got := digestRunItems(l.f.State().Issues[1].Body); !slices.Equal(got, items) {
		l.t.Errorf("%s: the digest's items %q; want %q", step, got, items)
	}
	want := map[int][]string{}
	for _, n := range assigned {
		want[n] = []string{humanRunOwner}
	}
	assignWant(l.t, l.f, step, want)
}

// TestHumanTaskRun is the whole run (#288's acceptance criteria). The fake
// holds mertenvg's story, 1, at planning, its spec approved. The first
// pass creates the digest, 2, and the planner posts the story's plan
// (humanRunPlan), which mertenvg's 👍 approves. That files T4 at ready and
// the three human tasks at needs-human, #3 to #6, each with the body
// humanBody writes and its check on its origin marker. The same pass links
// T3, #6, as blocked by T4, #3, as its plan says, and records the children
// on the story (#277). Then:
//   - T1's check fails on mertenvg's /done, with one reply; the file lands
//     on main, and his 👍 on the issue closes it as completed;
//   - T2's check passes on his first /done;
//   - eve's /done and 👍s write nothing;
//   - T3 enters the digest and assignment once T4 closes, and his 👍
//     closes it.
//
// Each close ends the task's item in the same pass, and the pass after
// sets stage:completed. The writes are checked after every pass, every
// unchanged pass costs no rate limit, and a restart, after T1's failed
// check and at the end, writes nothing.
func TestHumanTaskRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(humanRunOwner, "admin")
	f.SetPermission(humanRunEve, "read")
	story := f.AddIssue(humanRunOwner, "Quote lifetime", humanRunBody, "type:story")
	f.AddLabel(story, app, "stage:planning")
	rev := f.AddComment(story, app, revision(1, humanRunBody))
	f.AddComment(story, app, fmt.Sprintf("Revision 1 of the spec is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=spec-review to=planning rev=1 body=%s cmd=r9 seen=%d -->", humanRunOwner, state.BodyHash(humanRunBody), rev))
	l := newHumanRunLoop(t, f)

	// 1. The first pass creates the digest, and the planner posts the
	// story's plan, revision 1, at plan-review, which the pass after lists
	// and assigns.
	l.step("the first pass", 1, "POST /issues", "POST /graphql", "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	plan := l.newest(story)
	l.state("the plan", story, false, "stage:plan-review", "type:story")
	l.step("the plan listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")
	l.digest("the plan listed", []string{"#1 Plan approval p2 1"}, story)
	l.quiet("the plan")

	// 2. mertenvg's 👍 on the plan approves it, and the same pass files
	// its children, T4 first, then the human tasks, each linked under the
	// story and given its first move, keyed by the approval. Then it links
	// T3 as blocked by T4, as T3's depends_on says, the plan's one
	// dependency, and records the children on the story, by item (#277).
	// Then the story's item and its assignment end.
	up := f.AddReaction(plan.ID, humanRunOwner, "+1")
	l.step("the approval", 1, slices.Concat(
		[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review"},
		humanRunChildWrites(3, 4, 5, 6),
		[]string{"POST /issues/6/dependencies/blocked_by", "POST /issues/1/comments", "PATCH /issues/2", "DELETE /issues/1/assignees"})...)
	approval := fmt.Sprintf("Revision 1 of the plan is approved, by %s's 👍.\n\n"+
		"<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=r%d seen=%d -->",
		humanRunOwner, state.BodyHash(plan.Body[:strings.LastIndex(plan.Body, "\n")]), up, plan.ID)
	record := "I've created the children of plan rev 1:\n" +
		"- T1 #4: Set the quote lifetime in production\n" +
		"- T2 #5: Add the label for expired quotes\n" +
		"- T3 #6: Explain quote expiry in the FAQ\n" +
		"- T4 #3: Refuse an expired quote at booking\n\n" +
		"<!-- issuedog:created rev=1 children=4,5,6,3 -->"
	if added := l.added(story); !slices.Equal(added, []string{approval, record}) {
		t.Errorf("the approval: added %q; want the approval\n%s\nthen the record\n%s", added, approval, record)
	}
	l.state("the approval", story, false, "stage:planned", "type:story")
	cs := l.comments(story)
	l.children("the approval", cs[len(cs)-2].ID)
	if got := f.State().Issues[5].BlockedBy; !slices.Equal(got, []int{3}) {
		t.Errorf("the approval: T3, #6, is blocked by %v; want T4, #3", got)
	}
	for _, n := range []int{3, 4, 5} {
		if got := f.State().Issues[n-1].BlockedBy; len(got) != 0 {
			t.Errorf("the approval: #%d is blocked by %v; want nothing", n, got)
		}
	}
	l.digest("the approval", nil)

	// 3. The pass after lists T1 and T2, which nothing blocks, and assigns
	// mertenvg on each. T3 waits for T4.
	human := func(ns ...int) []string {
		var items []string
		for _, n := range ns {
			items = append(items, fmt.Sprintf("#%d Human task p2 1", n))
		}
		return items
	}
	l.step("the human tasks listed", 1, "PATCH /issues/2", "POST /issues/4/assignees", "POST /issues/5/assignees")
	l.digest("the human tasks listed", human(4, 5), 4, 5)
	l.quiet("the human tasks")

	// 4. eve's /done and 👍s write nothing: on T1, and on T3, blocked. Nor
	// does mertenvg's ❤️ on T1, which isn't a 👍.
	eve := f.AddComment(4, humanRunEve, "/done")
	f.AddIssueReaction(4, humanRunEve, "+1")
	f.AddIssueReaction(6, humanRunEve, "+1")
	f.AddIssueReaction(4, humanRunOwner, "heart")
	l.step("eve's /done and 👍s", 1)
	l.state("eve's /done and 👍s", 4, false, "priority:p2", "scope:infra", "stage:needs-human", "type:task")
	l.reactions("eve's /done and 👍s", 4, eve)
	l.quiet("eve's /done and 👍s")

	// 5. mertenvg's /done on T1 fails its check, as main has no such file:
	// the App's 👎, and one reply saying what it found. T1 stays open,
	// listed and assigned.
	done1 := f.AddComment(4, humanRunOwner, "/done")
	l.step("T1's /done", 1, discRunReact(done1), "POST /issues/4/comments")
	reply := rejected("/done", "its \"Done when\" check didn't pass. There's no file `"+humanRunFile+"` on `main`", strconv.FormatInt(done1, 10))
	if added := l.added(4); !slices.Equal(added, []string{reply}) {
		t.Errorf("T1's /done: added %q; want\n%s", added, reply)
	}
	l.reactions("T1's /done", 4, done1, app+" -1")
	l.state("T1's /done", 4, false, "priority:p2", "scope:infra", "stage:needs-human", "type:task")
	l.digest("T1's /done", human(4, 5), 4, 5)
	l.quiet("T1's check failed")
	// A restart checks the /done no second time: its reply's marker names
	// it.
	l.restart("T1's check failed")
	l.digest("T1's check failed: the restart", human(4, 5), 4, 5)

	// 6. mertenvg's 👍 on T1 fails the check too, with one reply keyed by
	// the 👍, which no later pass checks again.
	thumb := f.AddIssueReaction(4, humanRunOwner, "+1")
	l.step("T1's 👍", 1, "POST /issues/4/comments")
	thumbReply := thumbRejected(humanRunOwner, "There's no file `"+humanRunFile+"` on `main`.", thumb)
	if added := l.added(4); !slices.Equal(added, []string{thumbReply}) {
		t.Errorf("T1's 👍: added %q; want\n%s", added, thumbReply)
	}
	l.state("T1's 👍", 4, false, "priority:p2", "scope:infra", "stage:needs-human", "type:task")
	l.quiet("T1's 👍 failed")

	// 7. The file lands on main, and mertenvg removes his 👍 and adds it
	// again, which closes T1 as completed, having seen his /done, and ends
	// its item in the same pass. The pass after sets stage:completed. Its
	// assignee stays, as on any closed issue (SPEC §9.6).
	l.o.commit(humanRunFile, "# Production settings\n"+humanRunLine+"\n")
	if !f.DeleteIssueReaction(4, thumb) {
		t.Fatal("mertenvg's 👍 on T1 wasn't there to remove")
	}
	up1 := f.AddIssueReaction(4, humanRunOwner, "+1")
	l.step("T1's 👍 again", 1, "POST /issues/4/comments", "PATCH /issues/4", "PATCH /issues/2")
	closed1 := fmt.Sprintf("`%s`'s 👍 says this task is done. The file `%s` on `main` contains `%s`. So I've closed it as completed.\n\n"+
		"<!-- issuedog:transition from=needs-human to=closed reason=completed cmd=r%d seen=%d -->", humanRunOwner, humanRunFile, humanRunLine, up1, done1)
	if added := l.added(4); !slices.Equal(added, []string{closed1}) {
		t.Errorf("T1's 👍 again: added %q; want\n%s", added, closed1)
	}
	l.digest("T1's 👍 again", human(5), 4, 5)
	l.step("T1 completed", 1, "POST /issues/4/labels", "DELETE /issues/4/labels/stage:needs-human")
	l.state("T1 completed", 4, true, "priority:p2", "scope:infra", "stage:completed", "type:task")
	l.quiet("T1 completed")

	// 8. The label exists, and mertenvg's first /done on T2 closes it: the
	// marker, the App's 👍, then the close, which ends its item. The pass
	// after sets stage:completed.
	f.AddRepoLabel(humanRunLabel)
	done2 := f.AddComment(5, humanRunOwner, "/done")
	l.step("T2's /done", 1, "POST /issues/5/comments", discRunReact(done2), "PATCH /issues/5", "PATCH /issues/2")
	closed2 := fmt.Sprintf("%s said this task is done. The label `%s` exists. So I've closed it as completed.\n\n"+
		"<!-- issuedog:transition from=needs-human to=closed reason=completed cmd=%d seen=%d -->", humanRunOwner, humanRunLabel, done2, done2)
	if added := l.added(5); !slices.Equal(added, []string{closed2}) {
		t.Errorf("T2's /done: added %q; want\n%s", added, closed2)
	}
	l.reactions("T2's /done", 5, done2, app+" +1")
	l.digest("T2's /done", nil, 4, 5)
	l.step("T2 completed", 1, "POST /issues/5/labels", "DELETE /issues/5/labels/stage:needs-human")
	l.state("T2 completed", 5, true, "priority:p2", "scope:infra", "stage:completed", "type:task")
	l.quiet("T2 completed")

	// 9. T4 closes, as its PR's merge would close it, and the pass that
	// judges its close, setting stage:completed, lists T3, which it
	// blocked, and assigns mertenvg on it. eve's 👍 on T3, given while it
	// was blocked, still does nothing.
	f.CloseAs(3, humanRunOwner, "completed")
	l.step("T4 closed", 1, "POST /issues/3/labels", "DELETE /issues/3/labels/stage:ready", "PATCH /issues/2", "POST /issues/6/assignees")
	l.state("T4 closed", 3, true, "priority:p2", "scope:booking", "stage:completed", "type:task")
	l.state("T4 closed", 6, false, "priority:p2", "scope:docs", "stage:needs-human", "type:task")
	l.digest("T4 closed", human(6), 4, 5, 6)
	l.quiet("T3 unblocked")

	// 10. mertenvg's 👍 on T3, whose "Done when" is plain text, closes it at
	// once, having seen its first move, its newest comment, and ends its
	// item. The pass after sets stage:completed.
	up3 := f.AddIssueReaction(6, humanRunOwner, "+1")
	l.step("T3's 👍", 1, "POST /issues/6/comments", "PATCH /issues/6", "PATCH /issues/2")
	closed3 := fmt.Sprintf("`%s`'s 👍 says this task is done. The \"Done when\" has no check for me to run. So I've closed it as completed.\n\n"+
		"<!-- issuedog:transition from=needs-human to=closed reason=completed cmd=r%d seen=%d -->", humanRunOwner, up3, l.comments(6)[0].ID)
	if added := l.added(6); !slices.Equal(added, []string{closed3}) {
		t.Errorf("T3's 👍: added %q; want\n%s", added, closed3)
	}
	l.digest("T3's 👍", nil, 4, 5, 6)
	l.step("T3 completed", 1, "POST /issues/6/labels", "DELETE /issues/6/labels/stage:needs-human")
	l.state("T3 completed", 6, true, "priority:p2", "scope:docs", "stage:completed", "type:task")
	l.quiet("T3 completed")

	// 11. A restart writes nothing, and starts no run. Each human task was
	// closed as completed, and the story waits at planned.
	l.restart("the end")
	l.digest("the end", nil, 4, 5, 6)
	for _, n := range []int{4, 5, 6} {
		if r := stateReason(t, f, n); r != "completed" {
			t.Errorf("the end: #%d's state_reason = %q; want completed", n, r)
		}
	}
	l.state("the end", story, false, "stage:planned", "type:story")
	if n := len(f.State().Issues); n != 6 {
		t.Errorf("the end: %d issues; want the story, the digest and the plan's 4 children", n)
	}
}

// humanRunChildWrites are the writes that file the children of the
// story's approved plan, issues ns, in the order they're filed: for each,
// its create, its link under the story and its first move.
func humanRunChildWrites(ns ...int) []string {
	var w []string
	for _, n := range ns {
		w = append(w, "POST /issues", "POST /issues/1/sub_issues", fmt.Sprintf("POST /issues/%d/comments", n))
	}
	return w
}

// reactions checks the reactions on comment id of issue n, as
// "<login> <content>".
func (l *humanRunLoop) reactions(step string, n int, id int64, want ...string) {
	l.t.Helper()
	s, err := l.f.Client(l.t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		l.t.Fatal(err)
	}
	cs := l.f.State().Issues[n-1].Comments
	i := slices.IndexFunc(s.Comments, func(c github.Comment) bool { return c.ID == id })
	if i < 0 || i >= len(cs) {
		l.t.Fatalf("%s: #%d has no comment %d", step, n, id)
	}
	if !slices.Equal(cs[i].Reactions, want) {
		l.t.Errorf("%s: comment %d's reactions %q; want %q", step, id, cs[i].Reactions, want)
	}
}

// comments are issue n's comments, with their IDs.
func (l *humanRunLoop) comments(n int) []github.Comment {
	l.t.Helper()
	s, err := l.f.Client(l.t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		l.t.Fatal(err)
	}
	return s.Comments
}

// humanRunTask is a human task's body as humanBody writes it under the
// story, at p2, with mertenvg in its cc line, and origin, its origin
// marker's fields after item=.
func humanRunTask(scope, goal, why string, steps []string, link, done, estimate, item, origin string) string {
	numbered := make([]string, len(steps))
	for i, s := range steps {
		numbered[i] = fmt.Sprintf("%d. %s", i+1, s)
	}
	return strings.Join([]string{
		"**Parent:** #1   **Scope:** scope:" + scope + "   **Priority:** p2",
		"",
		"**Goal:** " + goal,
		"**Why it blocks:** #1 can't proceed until " + why,
		"**Steps:**",
		strings.Join(numbered, "\n"),
		"",
		"**Links:**",
		"- " + link,
		"",
		"**Done when:** " + done,
		"**Estimated time:** " + estimate,
		"",
		"cc @" + humanRunOwner,
		"",
		"<!-- issuedog:origin plan=#1 rev=1 item=" + item + origin + " -->",
	}, "\n")
}

// humanRunTail is the sentence that follows the daemon's own "Done when"
// for a check.
const humanRunTail = " I check this when you reply `/done` or react 👍 to this issue."

// children checks the story's children, #3 to #6, as the approval in
// comment p filed them (#328): T4, then the human tasks in plan order,
// each the App's, with its title, labels and body, linked under the story
// in that order, and with its one comment, its first move, keyed by p. A
// human task's body is the §9.3 template humanBody writes (#284): "Why it
// blocks" names the story, as no item depends on it, and "Done when" is
// the daemon's sentence for its check, or the planner's text. Its origin
// marker reads back as human, with its check.
func (l *humanRunLoop) children(step string, p int64) {
	l.t.Helper()
	st := l.f.State()
	if len(st.Issues) != 6 {
		l.t.Fatalf("%s: %d issues; want the story, the digest and the plan's 4 children", step, len(st.Issues))
	}
	if got := st.Issues[0].SubIssues; !slices.Equal(got, []int{3, 4, 5, 6}) {
		l.t.Errorf("%s: #1's sub-issues are %v; want [3 4 5 6], in the order they're filed", step, got)
	}
	for i, w := range []struct {
		item, title string
		stage       state.Stage
		labels      []string
		check       state.Check
		body        string // a human task's whole body, or a task's last line
	}{
		{"T4", "Refuse an expired quote at booking", state.Ready, []string{"priority:p2", "scope:booking", "stage:ready", "type:task"}, state.Check{},
			"<!-- issuedog:origin plan=#1 rev=1 item=T4 -->"},
		{"T1", "Set the quote lifetime in production", state.NeedsHuman, []string{"priority:p2", "scope:infra", "stage:needs-human", "type:task"},
			state.Check{Kind: state.CheckFile, Value: humanRunFile, Contains: humanRunLine},
			humanRunTask("infra", "Set the quote lifetime to 24 hours in production's settings.",
				"production has a quote lifetime, since the booking service won't start without one.",
				[]string{"Add the line QUOTE_TTL=24h to production's settings file on main."}, "https://github.com/o/r/blob/main/deploy/production.env",
				"The file `"+humanRunFile+"` on `main` contains `"+humanRunLine+"`."+humanRunTail, "5 min",
				"T1", " check=file contains="+humanRunLine+" human=1 value="+humanRunFile)},
		{"T2", "Add the label for expired quotes", state.NeedsHuman, []string{"priority:p2", "scope:infra", "stage:needs-human", "type:task"},
			state.Check{Kind: state.CheckLabel, Value: humanRunLabel},
			humanRunTask("infra", "Add the label quote-expired to the repo.", "support can label the reports of expired quotes.",
				[]string{"Open the repo's labels.", "Add the label quote-expired."}, "https://github.com/o/r/labels",
				"The label `"+humanRunLabel+"` exists in this repo."+humanRunTail, "1 min",
				"T2", " check=label human=1 value="+humanRunLabel)},
		{"T3", "Explain quote expiry in the FAQ", state.NeedsHuman, []string{"priority:p2", "scope:docs", "stage:needs-human", "type:task"}, state.Check{},
			humanRunTask("docs", "Add a FAQ entry on why quotes expire.", "customers can read why a quote expired.",
				[]string{"Open the FAQ page in the CMS.", "Add an entry on quote expiry, and publish it."}, "https://example.com/cms/faq",
				"The FAQ page explains why quotes expire.", "15 min", "T3", " human=1")},
	} {
		is := st.Issues[i+2]
		if labels := ivLabels(l.f, is.Number); is.Author != app || is.Title != w.title || !slices.Equal(labels, w.labels) || is.Parent != 1 {
			l.t.Errorf("%s: #%d is by %s, %q, labelled %v, under #%d; want the App's %s, %q, labelled %v, under #1",
				step, is.Number, is.Author, is.Title, labels, is.Parent, w.item, w.title, w.labels)
		}
		if human := w.stage == state.NeedsHuman; human && is.Body != w.body || !human && !strings.HasSuffix(is.Body, "\n\n"+w.body) {
			l.t.Errorf("%s: #%d's body is\n%s\nwant\n%s", step, is.Number, is.Body, w.body)
		}
		if got, want := closingComments(l.f, is.Number, 0), []string{firstMoveUnder(1, 1, wantChild{item: w.item, stage: w.stage}, p)}; !slices.Equal(got, want) {
			l.t.Errorf("%s: #%d's comments are %q; want its first move %q", step, is.Number, got, want)
		}
		s := &github.Snapshot{Number: is.Number, Author: github.Author{Login: app, Type: "Bot"}, Body: is.Body, Type: "task", Parent: 1}
		want := state.Origin{Plan: 1, Rev: 1, Item: w.item, Comment: state.BodyID, Human: w.stage == state.NeedsHuman, Check: w.check}
		if o := state.Derive(s, slug, clean).Origin; o != want {
			l.t.Errorf("%s: #%d's origin reads back as %+v; want %+v", step, is.Number, o, want)
		}
	}
}
