package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #298: a whole re-plan against the fakes, from an epic
// planned through the path that creates the children (#249), with its
// stories and their tasks, to a discussion hold, the interviewer's plan
// outcome, a revision applied to the stories, and a changed story's own
// revision applied to its tasks. It runs on childrenRunLoop (#279), so the
// stages are registered as registerStages registers them, the daemon is
// built as startDaemon builds it, daemon.New, so step 1 is
// daemon.NewStep's, the Applier reads main from the daemon's clone, and
// the digest and assignment are on, the config's labels exist, as
// startDaemon creates them, and Daemon.Iterate is the loop. Config and
// trust come from main on the origin: mertenvg owns CODEOWNERS and is
// notify, and wendy is a write collaborator CODEOWNERS doesn't trust.
// plan.review is false, so a story's first plan skips review, while a
// revision, and the epic's plan, wait for it.

// The run's untrusted collaborator and her comment, the interviewer's
// notes, and mertenvg's edit of a human task's body, which drops its
// origin marker.
const (
	replanRunWendy   = "wendy"
	replanRunComment = "Should support see why a quote expired?"
	replanRunNotes   = "Support needs to see why a quote expired, so the plan changes."
	replanRunEdited  = "Approve the default lifetime, 24 hours, in the defaults file."
)

// replanRunConfig is .issuedog/config.yml on main: childrenRunConfig's,
// typing by labels, with plan.review false.
const replanRunConfig = "types: labels\nnotify: [" + childrenRunOwner + "]\nscopes: [booking, infra, docs]\n" +
	"models: {allowed: [opus, sonnet]}\nplan: {review: false}\n"

// newReplanRunLoop starts a loop on f, on a new origin with
// replanRunConfig on main, and a new state directory.
func newReplanRunLoop(t *testing.T, f *githubtest.Fake) *childrenRunLoop {
	t.Helper()
	l := &childrenRunLoop{t: t, f: f, o: newOrigin(t), root: t.TempDir(), cfg: replanRunConfig}
	l.o.commit(".github/CODEOWNERS", "* @"+childrenRunOwner+"\n")
	l.o.commit(config.Path, l.cfg)
	l.start()
	return l
}

// replanRunOneTask is internal/agent's golden story plan cut to T1, for
// the stories S1, S3 and S4, which get one task each.
func replanRunOneTask(t *testing.T) map[string]any {
	t.Helper()
	out := plGolden(t, "story-plan")
	plan := childrenRunPlanOf(t, out)
	plan["items"], plan["order"] = []any{plItem(t, out, 0)}, "T1"
	plan["summary"] = "**Today** nothing reads a quote's age.\n\nThis plan adds each quote's expiry time (T1)."
	return out
}

// replanRunEpicRevision is the epic's plan revision 2 against the golden
// epic plan's S1 to S3: S1 kept, S2 changed, now depending on S4 alone, S3
// dropped, and S4 new, depending on the kept S1.
func replanRunEpicRevision(t *testing.T) map[string]any {
	t.Helper()
	out := plGolden(t, "epic-plan")
	plan := childrenRunPlanOf(t, out)
	s2, s4 := plItem(t, out, 1), plItem(t, out, 2)
	s2["status"], s2["title"], s2["depends_on"] = "changed", "Customers see when a quote expires, and why", []any{"S4"}
	s2["outcome"] = "Every quote a customer sees shows when it expires, and an expired one says why."
	s4["id"], s4["status"], s4["title"], s4["depends_on"] = "S4", "new", "Support sees why a quote expired", []any{"S1"}
	s4["outcome"] = "Support sees, for any quote, when and why it expired."
	s4["criteria"] = []any{"Support's quote page shows an expired quote's expiry time and the rule that expired it."}
	plan["items"] = []any{
		map[string]any{"id": "S1", "title": "Quotes expire after 24 hours", "status": "kept"},
		s2,
		map[string]any{"id": "S3", "title": "Expired quotes in the monthly report", "status": "dropped"},
		s4,
	}
	plan["order"], plan["message"] = "S4, then S2", "Support needs the reason a quote expired, and the report waits."
	plan["summary"] = "**Today** quotes expire at booking (S1).\n\nThis revision keeps S1, changes S2, drops S3 and adds S4."
	return out
}

// replanRunStoryRevision is the golden story revision (T1 kept, T2
// changed, T3 dropped, T4 new), with T4 a human task whose check reads a
// file on main, and the changed T2 depending on T1 and T4.
func replanRunStoryRevision(t *testing.T) map[string]any {
	t.Helper()
	out := plGolden(t, "story-revision")
	human := plItem(t, plGolden(t, "story-plan"), 2)
	task, ok := human["human_task"].(map[string]any)
	if !ok {
		t.Fatalf("T3 %v has no human_task", human)
	}
	task["goal"] = "Approve the default quote lifetime of 24 hours."
	task["done_when"] = map[string]any{"text": "The defaults file on `main` sets `QUOTE_TTL_DEFAULT=24h`.",
		"check": map[string]any{"kind": "file", "path": "deploy/defaults.env", "contains": "QUOTE_TTL_DEFAULT=24h"}}
	t4 := plItem(t, out, 3)
	delete(t4, "task")
	t4["title"], t4["human"], t4["human_task"], t4["scope"], t4["depends_on"] = "Approve the default quote lifetime", true, task, "infra", []any{}
	plItem(t, out, 1)["depends_on"] = []any{"T1", "T4"}
	return out
}

// replanRunUntouched fails t if a write f had from from on is to one of
// issues ns: a kept child, or one outside the revision, gets none.
func replanRunUntouched(t *testing.T, f *githubtest.Fake, step string, from int, ns ...int) {
	t.Helper()
	for _, w := range f.Writes()[from:] {
		for _, n := range ns {
			if w.Path == fmt.Sprintf("/issues/%d", n) || strings.HasPrefix(w.Path, fmt.Sprintf("/issues/%d/", n)) {
				t.Errorf("%s: #%d got the write %s %s; want none", step, n, w.Method, w.Path)
			}
		}
	}
}

// replanRunDropped checks that each of issues ns is closed as not
// planned, with no dependency link left to or from it.
func replanRunDropped(t *testing.T, f *githubtest.Fake, step string, ns ...int) {
	t.Helper()
	for _, n := range ns {
		if is := f.State().Issues[n-1]; !is.Closed || droppedReason(t, f, n) != "not_planned" || len(is.BlockedBy) != 0 {
			t.Errorf("%s: #%d is closed %t, as %q, blocked by %v; want it closed as not planned, unlinked", step, n, is.Closed, droppedReason(t, f, n), is.BlockedBy)
		}
		for _, is := range f.State().Issues {
			if slices.Contains(is.BlockedBy, n) {
				t.Errorf("%s: #%d is still blocked by the dropped #%d", step, is.Number, n)
			}
		}
	}
}

// replanRunRevision checks that issue n's newest comment is the App's plan
// revision 2, posted by claude run k, with each of lines, and that it asks
// for review, though plan.review is false, at plan-review, with labels.
// It returns the comment's ID.
func replanRunRevision(l *childrenRunLoop, step string, n, k int, labels, lines []string) int64 {
	l.t.Helper()
	cs := l.comments(n)
	c := cs[len(cs)-1]
	move := fmt.Sprintf("\n\n<!-- issuedog:transition from=planning to=plan-review rev=2 run=run%d ", k)
	if c.Author.Login != app || !strings.HasPrefix(c.Body, fmt.Sprintf("### Plan for #%d (rev 2)\n\n", n)) || !strings.Contains(c.Body, move) ||
		!strings.Contains(c.Body, "\n\n**Changes since rev 1.**\n\n") || !strings.Contains(c.Body, "\n\n**To approve this plan, react 👍 to this comment.**") ||
		strings.Contains(c.Body, "skips review") || !strings.HasSuffix(c.Body, fmt.Sprintf("<!-- issuedog:revision kind=plan rev=2 body=%s -->", childrenRunPlanHash(c.Body))) {
		l.t.Errorf("%s: #%d's newest comment, by %s, =\n%s\nwant the App's plan revision 2 from run %d, asking for review", step, n, c.Author.Login, c.Body, k)
	}
	for _, line := range lines {
		if !strings.Contains(c.Body, "\n\n"+line+"\n") {
			l.t.Errorf("%s: #%d's plan revision 2 lacks the line\n%s", step, n, line)
		}
	}
	l.labels(step, n, false, labels...)
	return c.ID
}

// replanRunApproval checks that issue n's newest comment but one, the
// record being the newest, is the move into planned that approved its
// plan revision 2, the comment plan, by mertenvg's 👍 up, and returns its
// ID, which keys the moves of the revision's children.
func replanRunApproval(l *childrenRunLoop, step string, n int, plan, up int64) int64 {
	l.t.Helper()
	cs := l.comments(n)
	c := cs[len(cs)-2]
	want := fmt.Sprintf("Revision 2 of the plan is approved, by %s's 👍.\n\n<!-- issuedog:transition from=plan-review to=planned rev=2 body=", childrenRunOwner)
	if !strings.HasPrefix(c.Body, want) || !strings.HasSuffix(c.Body, fmt.Sprintf(" cmd=r%d seen=%d -->", up, plan)) {
		l.t.Errorf("%s: #%d's newest comment but one =\n%s\nwant the approval of plan revision 2 by the 👍 r%d", step, n, c.Body, up)
	}
	return c.ID
}

// TestReplanRun is the whole re-plan (#298's acceptance criteria, in
// order). The fake holds mertenvg's epic, #1, at planning, its spec
// approved. Through the path that creates the children (#249), the epic's
// plan files S1 to S3 as #3 to #5, and each story's plan, which skips
// review, files its tasks: S1's T1 as #6, S2's T1 to T3 as #7 to #9, T3 a
// human task, and S3's T1 as #10. Then:
//   - wendy's comment holds the epic from planned, and mertenvg's 👍 on it
//     resolves the hold;
//   - the interviewer's plan outcome sends it to planning, and the
//     planner's revision waits at plan-review, though plan.review is
//     false;
//   - mertenvg's 👍 applies it: S1 is untouched, S2 is rewritten and back
//     at planning, S3 and its task are closed as not planned, unlinked,
//     and S4 is filed as #11, linked;
//   - S2's own revision, approved the same way, keeps #7, rewrites #8,
//     closes #9 and files the human task T4 as #13;
//   - mertenvg's edit of #13's body has its origin reposted, and it stays a
//     human task; renamed issuedog digest, it isn't taken for the digest.
//
// The writes are checked after every pass, every unchanged pass costs no
// rate limit, and a restart at the end writes nothing.
func TestReplanRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission(childrenRunOwner, "admin")
	f.SetPermission(replanRunWendy, "write")
	epic := childrenRunAtPlanning(f, "type:epic")
	l := newReplanRunLoop(t, f)
	l.outs(plGolden(t, "epic-plan"))

	// 1. The setup, through the path that creates the children (#249). The
	// first pass creates the digest, #2, and the planner posts the epic's
	// plan of three stories at plan-review, as an epic's plan always waits
	// for review.
	l.step("the epic's plan", 1, "POST /issues", "POST /graphql", "PATCH /issues/2",
		"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	plan := l.planned("the epic's plan", epic, 1)
	l.step("the epic's plan listed", 1, "PATCH /issues/2", "POST /issues/1/assignees")
	l.quiet("the epic's plan")

	// mertenvg's 👍 approves it: S1 to S3 are filed as #3 to #5, S2 and S3
	// blocked by S1, then the record. Then the planner runs on each story,
	// one a pass, and each plan skips review, so the pass after files its
	// tasks and records them: S1's #6, S2's #7 to #9, #8 blocked by #7 and
	// the human task #9, and S3's #10. #9 is listed and assigned.
	f.AddReaction(plan.ID, childrenRunOwner, "+1")
	l.step("the epic's approval", 1, slices.Concat(
		[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review"},
		childWrites(epic, 3, 4, 5), blockWrites(4, 5), []string{recordWrite(epic), "PATCH /issues/2", "DELETE /issues/1/assignees"})...)
	l.outs(replanRunOneTask(t), plGolden(t, "story-plan"), replanRunOneTask(t))
	l.step("S1's plan", 2, "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:planning")
	l.step("S2's plan", 3, slices.Concat(childWrites(3, 6), []string{recordWrite(3),
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:planning"})...)
	l.step("S3's plan", 4, slices.Concat(childWrites(4, 7, 8, 9), blockWrites(8, 8), []string{recordWrite(4),
		"POST /issues/5/comments", "POST /issues/5/labels", "DELETE /issues/5/labels/stage:planning"})...)
	l.step("S3's tasks", 4, slices.Concat(childWrites(5, 10), []string{recordWrite(5), "PATCH /issues/2", "POST /issues/9/assignees"})...)
	for n, subs := range map[int][]int{1: {3, 4, 5}, 3: {6}, 4: {7, 8, 9}, 5: {10}} {
		if is := f.State().Issues[n-1]; !slices.Contains(is.Labels, "stage:planned") || !slices.Equal(is.SubIssues, subs) || len(records(is)) != 1 {
			t.Fatalf("the setup: #%d is labelled %v, with sub-issues %v and records %q; want it planned, with %v and one record", n, is.Labels, is.SubIssues, records(is), subs)
		}
	}
	l.digest("the setup", 2, []string{"#9 Human task p2 1"}, 9)
	l.quiet("the setup")

	// 2. wendy's comment, as a write collaborator CODEOWNERS doesn't trust,
	// holds the epic from planned for discussion, listed and assigned.
	wendy := f.AddComment(epic, replanRunWendy, replanRunComment)
	l.step("wendy's comment", 4, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planned",
		"PATCH /issues/2", "POST /issues/1/assignees")
	if added := l.added(epic); len(added) != 1 ||
		!strings.Contains(added[0], fmt.Sprintf("\n\n<!-- issuedog:transition from=planned to=held at=planned origin=planned reason=discussion event=c%d ", wendy)) {
		t.Errorf("wendy's comment: added %q; want the hold for discussion from planned", added)
	}
	l.labels("wendy's comment", epic, false, "stage:held", "type:epic")
	hold := l.comments(epic)[len(l.comments(epic))-1].ID
	l.quiet("the hold")

	// 3. mertenvg's 👍 on her comment resolves the hold, to clarifying, in
	// a move keyed by the hold's comment, and the same pass the
	// interviewer's plan outcome sends the epic to planning, with its notes
	// and the cc line, after which mertenvg is unassigned, as an unassign
	// at clarifying waits for the pass's stage work (SPEC §9.6), which
	// leaves the epic at planning. The pass after, the planner posts revision 2, which
	// waits at plan-review though plan.review is false, and the pass after
	// that lists it: its approval unblocks the epic and the 8 open issues
	// below it.
	f.AddReaction(wendy, childrenRunOwner, "+1")
	l.outs(map[string]any{"outcome": "plan", "message": replanRunNotes}, replanRunEpicRevision(t))
	l.step("the hold resolved", 5, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:held",
		"PATCH /issues/2", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:clarifying", "DELETE /issues/1/assignees")
	added := l.added(epic)
	planMove := "The discussion during the hold changes this issue's plan, but not its spec, so it goes to `planning`.\n\n" + replanRunNotes +
		"\n\nThe planner reads these notes and the discussion, and posts a new plan for your 👍.\n\ncc @" + childrenRunOwner +
		"\n\n<!-- issuedog:transition from=clarifying to=planning origin=planned run=run5 seen="
	if len(added) != 2 || !strings.Contains(added[0], fmt.Sprintf("\n\n<!-- issuedog:transition from=held to=clarifying at=planned origin=planned event=h%d ", hold)) ||
		!strings.HasPrefix(added[1], planMove) {
		t.Errorf("the hold resolved: added %q; want the move to clarifying, then the interviewer's to planning", added)
	}
	l.labels("the hold resolved", epic, false, "stage:planning", "type:epic")
	l.step("the epic's revision", 6, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planning")
	rev := replanRunRevision(l, "the epic's revision", epic, 6, []string{"stage:plan-review", "type:epic"}, []string{
		"**S1. Quotes expire after 24 hours** · kept", "**S2. Customers see when a quote expires, and why** · changed · p2",
		"**S3. Expired quotes in the monthly report** · dropped", "**S4. Support sees why a quote expired** · new · p2 · effort:medium"})
	l.step("the epic's revision listed", 6, "PATCH /issues/2", "POST /issues/1/assignees")
	l.digest("the epic's revision listed", 2, []string{"#1 Plan approval p2 9", "#9 Human task p2 1"}, 1, 9)
	l.quiet("the epic's revision")

	// 4. mertenvg's 👍 applies it, in one pass: the approval; S4 filed as
	// #11, linked and moved; S2 rewritten and moved to planning, then S4
	// blocked by S1, and S2 by S4 and no longer by S1; S3's subtree closed
	// deepest first, its task #10, then S3, which loses its link to S1; and
	// the record, the pass's last write before the digest's. S1 and its
	// task get none.
	from := len(f.Writes())
	up := f.AddReaction(rev, childrenRunOwner, "+1")
	l.step("the epic's revision approved", 6, "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:plan-review",
		"POST /issues", "POST /issues/1/sub_issues", "POST /issues/11/comments",
		"PATCH /issues/4", "POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:planned",
		"POST /issues/11/dependencies/blocked_by", "POST /issues/4/dependencies/blocked_by", "DELETE /issues/4/dependencies/blocked_by/700003",
		"POST /issues/10/comments", "PATCH /issues/10", "POST /issues/5/comments", "PATCH /issues/5", "DELETE /issues/5/dependencies/blocked_by/700003",
		recordWrite(epic), "PATCH /issues/2", "DELETE /issues/1/assignees")
	approval := replanRunApproval(l, "the epic's revision approved", epic, rev, up)
	replanRunUntouched(t, f, "the epic's revision approved", from, 3, 6)
	l.labels("the epic's revision approved", 3, false, "effort:high", "model:opus", "priority:p1", "stage:planned", "type:story")
	is := f.State().Issues
	if s2 := is[3]; s2.Title != "S2. Customers see when a quote expires, and why" || s2.BodyEditedBy != app ||
		!strings.HasPrefix(s2.Body, "<!-- issuedog:rewrite run=run6 -->\n**Parent:** #1   **Priority:** p2\n\n## Outcome\n\n"+
			"Every quote a customer sees shows when it expires, and an expired one says why.\n\n") ||
		!strings.HasSuffix(s2.Body, "\n\nDepends on: S4.\n\n<!-- issuedog:origin plan=#1 rev=2 item=S2 -->") || !slices.Equal(s2.BlockedBy, []int{11}) {
		t.Errorf("the epic's revision approved: S2 is %q, blocked by %v, with body\n%s\nwant revision 2's S2, rewritten whole, blocked by S4's #11 alone", s2.Title, s2.BlockedBy, s2.Body)
	}
	l.labels("the epic's revision approved", 4, false, "priority:p2", "stage:planning", "type:story")
	moved := fmt.Sprintf("Rewritten from #1's plan, rev 2, as S2, since [the plan](https://github.com/o/r/issues/1#issuecomment-%d) changed it. "+
		"It goes back to `stage:planning`, where the planner plans it again.\n\n<!-- issuedog:transition from=planned to=planning event=p%d -->", rev, approval)
	if added := l.added(4); !slices.Equal(added, []string{moved}) {
		t.Errorf("the epic's revision approved: S2's new comments %q; want\n%s", added, moved)
	}
	replanRunDropped(t, f, "the epic's revision approved", 5, 10)
	link := fmt.Sprintf("([the plan](https://github.com/o/r/issues/1#issuecomment-%d)).\n\n", rev)
	for n, text := range map[int]string{
		5:  "Closed as not planned, since #1's plan, rev 2, drops S3 " + link + "<!-- issuedog:transition from=planned to=closed reason=not_planned event=p",
		10: "Closed as not planned, since #1's plan, rev 2, drops S3, #5, which this issue is below " + link + "<!-- issuedog:transition from=ready to=closed reason=not_planned event=p",
	} {
		if added := l.added(n); len(added) != 1 || added[0] != fmt.Sprintf("%s%d -->", text, approval) {
			t.Errorf("the epic's revision approved: #%d's new comments %q; want the close naming the revision", n, added)
		}
	}
	if s4 := is[10]; s4.Title != "S4. Support sees why a quote expired" || s4.Parent != epic || !slices.Equal(s4.BlockedBy, []int{3}) ||
		!strings.HasSuffix(s4.Body, "\n\n<!-- issuedog:origin plan=#1 rev=2 item=S4 -->") || !slices.Equal(is[0].SubIssues, []int{3, 4, 5, 11}) {
		t.Errorf("the epic's revision approved: #11 is %q, under #%d, blocked by %v, with body\n%s\nand the epic's sub-issues %v; want S4, linked, blocked by S1's #3",
			s4.Title, s4.Parent, s4.BlockedBy, s4.Body, is[0].SubIssues)
	}
	l.labels("the epic's revision approved", 11, false, "effort:medium", "priority:p2", "stage:planning", "type:story")
	record := "I've applied plan rev 2 to the children, which are now:\n- S1 #3: Quotes expire after 24 hours\n" +
		"- S2 #4: Customers see when a quote expires, and why\n- S4 #11: Support sees why a quote expired\n\n<!-- issuedog:created rev=2 children=3,4,11 -->"
	if got := records(is[0]); len(got) != 2 || got[1] != record {
		t.Errorf("the epic's revision approved: the epic's records %q; want revision 1's, then\n%s", got, record)
	}
	l.labels("the epic's revision approved", epic, false, "stage:planned", "type:epic")
	l.digest("the epic's revision approved", 2, []string{"#9 Human task p2 1"}, 9)

	// The pass after, the close judge sets stage:discarded on #5 and #10,
	// two label writes each, and the planner posts S2's own revision, of
	// its tasks, which waits at plan-review. The pass after lists it, and
	// the planner plans S4, whose first plan skips review, and the pass
	// after that files its task, #12. The epic and S1 get no write.
	from = len(f.Writes())
	l.outs(replanRunStoryRevision(t), replanRunOneTask(t))
	l.step("S2's revision", 7, "POST /issues/5/labels", "DELETE /issues/5/labels/stage:planned", "POST /issues/10/labels", "DELETE /issues/10/labels/stage:ready",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:planning")
	l.labels("S2's revision", 5, true, "effort:medium", "priority:p2", "stage:discarded", "type:story")
	l.labels("S2's revision", 10, true, "effort:medium", "model:sonnet", "priority:p2", "scope:booking", "stage:discarded", "type:task")
	srev := replanRunRevision(l, "S2's revision", 4, 7, []string{"priority:p2", "stage:plan-review", "type:story"}, []string{
		"**T1. Record when each quote expires** · kept", "**T2. Refuse an expired quote at booking, saying when it expired** · changed · scope:booking · p1",
		"**T3. Set the quote lifetime in production** · dropped", "**T4. Approve the default quote lifetime** · new · scope:infra · p2 · human"})
	l.step("S4's plan", 8, "PATCH /issues/2", "POST /issues/4/assignees", "POST /issues/11/comments", "POST /issues/11/labels", "DELETE /issues/11/labels/stage:planning")
	l.step("S4's task", 8, slices.Concat(childWrites(11, 12), []string{recordWrite(11)})...)
	replanRunUntouched(t, f, "the passes after the approval", from, epic, 3)
	l.digest("S4's task", 2, []string{"#4 Plan approval p2 4", "#9 Human task p2 1"}, 4, 9)
	l.quiet("S2's revision")

	// 5. mertenvg's 👍 applies S2's revision the same way: T4 filed as the
	// human task #13, linked and moved; T2's #8 rewritten, kept at ready,
	// then blocked by #13, and no longer by T3's #9; #9 closed as not
	// planned, which ends its item; and the record on S2. T1's #7 gets no
	// write. The pass after, the close judge sets #9's closed stage, and
	// #13 is listed and assigned.
	from = len(f.Writes())
	up = f.AddReaction(srev, childrenRunOwner, "+1")
	l.step("S2's revision approved", 8, "POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:plan-review",
		"POST /issues", "POST /issues/4/sub_issues", "POST /issues/13/comments", "PATCH /issues/8", "POST /issues/8/comments",
		"POST /issues/8/dependencies/blocked_by", "DELETE /issues/8/dependencies/blocked_by/700009", "POST /issues/9/comments", "PATCH /issues/9",
		recordWrite(4), "PATCH /issues/2", "DELETE /issues/4/assignees")
	sApproval := replanRunApproval(l, "S2's revision approved", 4, srev, up)
	replanRunUntouched(t, f, "S2's revision approved", from, 7)
	is = f.State().Issues
	if t2 := is[7]; t2.Title != "Refuse an expired quote at booking, saying when it expired" || !slices.Equal(t2.BlockedBy, []int{7, 13}) ||
		!strings.HasPrefix(t2.Body, "<!-- issuedog:rewrite run=run7 -->\n**Parent:** #4   **Scope:** scope:booking   **Priority:** p1\n\n## Context\n\n") ||
		!strings.HasSuffix(t2.Body, "\n\n<!-- issuedog:origin plan=#4 rev=2 item=T2 -->") {
		t.Errorf("S2's revision approved: #8 is %q, blocked by %v, with body\n%s\nwant revision 2's T2, rewritten whole, blocked by #7 and #13", t2.Title, t2.BlockedBy, t2.Body)
	}
	l.labels("S2's revision approved", 8, false, "priority:p1", "scope:booking", "stage:ready", "type:task")
	moved = fmt.Sprintf("Rewritten from #4's plan, rev 2, as T2, since [the plan](https://github.com/o/r/issues/4#issuecomment-%d) changed it. "+
		"It goes back to `stage:ready`.\n\n<!-- issuedog:transition from=ready to=ready event=p%d -->", srev, sApproval)
	closed := fmt.Sprintf("Closed as not planned, since #4's plan, rev 2, drops T3 ([the plan](https://github.com/o/r/issues/4#issuecomment-%d)).\n\n"+
		"<!-- issuedog:transition from=needs-human to=closed reason=not_planned event=p%d -->", srev, sApproval)
	if a8, a9 := l.added(8), l.added(9); !slices.Equal(a8, []string{moved}) || !slices.Equal(a9, []string{closed}) {
		t.Errorf("S2's revision approved: #8's new comments %q, and #9's %q; want\n%s\nand\n%s", a8, a9, moved, closed)
	}
	replanRunDropped(t, f, "S2's revision approved", 9)
	origin := "<!-- issuedog:origin plan=#4 rev=2 item=T4 check=file contains=QUOTE_TTL_DEFAULT=24h human=1 value=deploy/defaults.env -->"
	if t4 := is[12]; t4.Title != "Approve the default quote lifetime" || t4.Parent != 4 || !strings.HasSuffix(t4.Body, "\n\n"+origin) ||
		!strings.Contains(t4.Body, "\n**Why it blocks:** #8 ") || !slices.Equal(is[3].SubIssues, []int{7, 8, 9, 13}) {
		t.Errorf("S2's revision approved: #13 is %q, under #%d, with body\n%s\nand S2's sub-issues %v; want T4's human task, linked, blocking #8",
			t4.Title, t4.Parent, t4.Body, is[3].SubIssues)
	}
	l.labels("S2's revision approved", 13, false, "priority:p2", "scope:infra", "stage:needs-human", "type:task")
	record = "I've applied plan rev 2 to the children, which are now:\n- T1 #7: Record when each quote expires\n" +
		"- T2 #8: Refuse an expired quote at booking, saying when it expired\n- T4 #13: Approve the default quote lifetime\n\n<!-- issuedog:created rev=2 children=7,8,13 -->"
	if got := records(is[3]); len(got) != 2 || got[1] != record {
		t.Errorf("S2's revision approved: S2's records %q; want revision 1's, then\n%s", got, record)
	}
	l.labels("S2's revision approved", 4, false, "priority:p2", "stage:planned", "type:story")
	l.step("S2's revision applied", 8, "POST /issues/9/labels", "DELETE /issues/9/labels/stage:needs-human", "PATCH /issues/2", "POST /issues/13/assignees")
	l.labels("S2's revision applied", 9, true, "priority:p2", "scope:infra", "stage:discarded", "type:task")
	// #9 keeps its assignee, as any closed issue does (SPEC §9.6).
	l.digest("S2's revision applied", 2, []string{"#13 Human task p2 1"}, 9, 13)
	l.quiet("S2's tasks")

	// 6. mertenvg's edit of #13's body drops its origin marker: the App
	// reposts the whole marker, human=1 and the check's fields included, in
	// a comment with no cc line, and nothing holds #13, as mertenvg is a
	// code owner. It stays a human task, listed, though its digest item
	// now says to see the issue, as its Goal is no longer the App's text.
	f.EditBody(13, childrenRunOwner, replanRunEdited)
	l.step("the edit", 8, "POST /issues/13/comments", "PATCH /issues/2")
	if added := l.added(13); !slices.Equal(added, []string{originRepost(origin)}) {
		t.Errorf("the edit: #13's new comments %q; want the repost\n%s", added, originRepost(origin))
	}
	l.labels("the edit", 13, false, "priority:p2", "scope:infra", "stage:needs-human", "type:task")
	l.digest("the edit", 2, []string{"#13 Human task p2 1"}, 9, 13)
	l.quiet("the edit")

	// 7. Renamed issuedog digest, #13 isn't the digest: its item stays,
	// under its new title, and its body is never written.
	f.RenameIssue(13, childrenRunOwner, "issuedog digest")
	l.step("the rename", 8, "PATCH /issues/2")
	l.quiet("the rename")

	// 8. A restart, which lists every issue afresh and judges the title
	// rule again, writes nothing and starts no run. #13 is still a human
	// task, and #2 the digest.
	l.restart("the end")
	if is := f.State().Issues; len(is) != 13 || is[12].Body != replanRunEdited ||
		!strings.Contains(is[1].Body, "\n### 1. #13: issuedog digest\n\n**Human task**, priority p2, ") {
		t.Errorf("the end: %d issues, #13's body %q, and the digest\n%s\nwant 13, #13 as mertenvg left it, and #2 listing it", len(is), is[12].Body, is[1].Body)
	}
	l.digest("the end", 2, []string{"#13 Human task p2 1"}, 9, 13)
	l.labels("the end", 13, false, "priority:p2", "scope:infra", "stage:needs-human", "type:task")
}
