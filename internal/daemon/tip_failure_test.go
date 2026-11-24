package daemon_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
)

// A failing story tip files its failure under its story before its check
// is posted (SPEC §7.5, A failure on the story branch), on a verdictRig
// wired as main's, with real temporary repos: story 1, at planned, whose
// branch the test cuts and pushes to in the origin itself, as an admin
// may. The digest is issue 2, so the first issue filed is 3.

// tipFailureFirst is the number of the first issue a failing tip files:
// the story is 1, and the digest 2.
const tipFailureFirst = 3

// tipFailureCut files story 1 on v's fake, cuts its branch and runs the
// pass that checks the cut as success, and returns the cut. v picks
// nothing (pickOff), so a bug filed at ready stays there: its pick is
// story_failure_test.go's.
func (v *verdictRig) tipFailureCut() string {
	v.t.Helper()
	v.pickOff()
	tipStory(v.t, v.f)
	cut := v.tipCut(tipBranch)
	v.tipJudged(cut, 1, []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")})
	return cut
}

// tipFailureOrigin is the origin marker of the issue filed for story 1's
// branch's failing tip tip, with human=1 on a human task (SPEC §5).
func tipFailureOrigin(tip string, human bool) string {
	if human {
		return "<!-- issuedog:origin human=1 story=#1 tip=" + tip + " -->"
	}
	return "<!-- issuedog:origin story=#1 tip=" + tip + " -->"
}

// tipFailureMove is the marker of the first move, to stage, of the issue
// filed for the failing tip tip, keyed event=t<SHA> (SPEC §5.2, §7.2).
func tipFailureMove(stage, tip string) string {
	return "<!-- issuedog:transition from=intake to=" + stage + " event=t" + tip + " -->"
}

// tipFailureFiled checks that issue n on v's fake is the one filed for story
// 1's branch's failing tip tip: the App's, open, a sub-issue of the story
// alone, with labels, its body ending in its origin and holding each of
// texts, and one comment, its first move to stage, which names the story,
// the branch and the tip. It returns the issue's body.
func (v *verdictRig) tipFailureFiled(n int, tip, stage string, human bool, labels []string, texts ...string) string {
	v.t.Helper()
	st := v.f.State()
	if len(st.Issues) < n {
		v.t.Fatalf("the fake has %d issues; want #%d filed", len(st.Issues), n)
	}
	is := st.Issues[n-1]
	if is.Author != app || is.Closed || is.Parent != 1 || !slices.Equal(st.Issues[0].SubIssues, []int{n}) || !slices.Equal(is.Labels, labels) {
		v.t.Errorf("#%d is by %s, closed %t, below #%d, the story's sub-issues %v, labelled %v; want the App's, open, the story's one sub-issue, labelled %v",
			n, is.Author, is.Closed, is.Parent, st.Issues[0].SubIssues, is.Labels, labels)
	}
	if !strings.HasSuffix(is.Body, "\n\n"+tipFailureOrigin(tip, human)) {
		v.t.Errorf("#%d's body =\n%s\nwant it to end in its origin %s", n, is.Body, tipFailureOrigin(tip, human))
	}
	for _, text := range texts {
		if !strings.Contains(is.Body, text) {
			v.t.Errorf("#%d's body =\n%s\nwant it to hold\n%s", n, is.Body, text)
		}
	}
	label := "`stage:" + stage + "`"
	move := "Filed for story #1, whose branch `" + tipBranch + "` fails its checks at `" + tip + "`. It starts at " + label + ".\n\n" + tipFailureMove(stage, tip)
	if len(is.Comments) != 1 || is.Comments[0].Author != app || is.Comments[0].Body != move {
		v.t.Errorf("#%d's comments = %+v; want the App's first move alone,\n%s", n, is.Comments, move)
	}
	return is.Body
}

// tipFailureWrites are the writes of a clean pass on a failing tip with
// nothing filed for it yet: the issue n, its link under story 1, its first
// move, then the tip's check, last.
func tipFailureWrites(n int) []string {
	return []string{"POST /issues", "POST /issues/1/sub_issues", "POST /issues/" + strconv.Itoa(n) + "/comments", "POST /check-runs"}
}

// tipFailureFailed runs a pass of v on tip, story 1's branch's tip, which
// must file its failure and post issuedog/tip as failure there, the
// commands having run commands times, with the writes want after any the
// digest or assignment make first, and none after.
func (v *verdictRig) tipFailureFailed(cut, tip string, commands int, want []string) {
	v.t.Helper()
	from := len(v.f.Writes())
	checks := []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success"), tipCheck(tip, tipBranch, "failure")}
	slices.SortStableFunc(checks, func(a, b githubtest.CheckRunState) int { return strings.Compare(a.SHA, b.SHA) })
	v.tipJudged(tip, commands, checks)
	if got := writesOf(v.f)[from:]; len(got) < len(want) || !slices.Equal(got[len(got)-len(want):], want) {
		v.t.Errorf("the pass on the failing tip wrote %q; want it to end in %q", got, want)
	}
}

// A tip that fails rule 2 files one human task under its story, before its
// check: the App's, a sub-issue of the story, with type:task, the story's
// priority and stage:needs-human, its body the hand-off template, naming
// the tip and the protected path rule 2 found, saying to revert the change
// on the story branch with a new commit and to land it on main in a PR of
// its own if it's wanted, its "Done when" its tip check, then the cc line,
// and its origin with human=1, and its first move intake → needs-human,
// keyed event=t<SHA>. The passes after file nothing more, and once they've
// read what it wrote, make no request for the tip.
func TestTipFailureRule2(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	cut := v.tipFailureCut()
	tip := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n"})
	v.tipFailureFailed(cut, tip, 0, tipFailureWrites(tipFailureFirst))
	body := v.tipFailureFiled(tipFailureFirst, tip, "needs-human", true, []string{"priority:p1", "stage:needs-human", "type:task"},
		"**Parent:** #1   **Scope:** none   **Priority:** p1",
		"Story #1's branch `"+tipBranch+"` fails rule 2 at `"+tip+"`",
		"### Rule 2: protected paths", "secrets/key.txt",
		"**Goal:** Take the change to protected paths at `"+tip+"` off the story branch `"+tipBranch+"`",
		"**Why it blocks:** #1 can't proceed until its branch's tip passes its checks",
		"2. Revert those changes on `"+tipBranch+"` with a new commit, and push it to the branch.",
		"3. If the change is wanted, land it on `main` in a PR of its own.",
		"- https://github.com/o/r/commit/"+tip,
		"**Done when:** The story branch's tip is no longer `"+tip+"`. I check this when you reply `/done` or react 👍 to this issue.",
		"cc @"+wholeRunOwner)
	if f.State().Issues[tipFailureFirst-1].Title != "Revert the protected path change at "+tip[:7]+" on story #1's branch" {
		t.Errorf("the human task's title = %q", f.State().Issues[tipFailureFirst-1].Title)
	}
	if strings.Contains(body, "- [ ]") {
		t.Errorf("the human task's body =\n%s\nwant no checkbox", body)
	}
	v.tipSettle()
	v.tipQuiet("after the filing")
	if got := len(f.State().Issues); got != tipFailureFirst {
		t.Errorf("the fake has %d issues; want the human task alone filed", got)
	}
}

// A tip that passes rule 2 and fails a command files one bug under its
// story, before its check: the App's, a sub-issue of the story, with
// type:bug, the story's priority and stage:ready, its body a task naming
// the tip and its baseline and the failed command with its output, fenced,
// then its origin, and its first move intake → ready, keyed event=t<SHA>.
// The passes after file nothing more, and make no request for the tip.
func TestTipFailureCommand(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	cut := v.tipFailureCut()
	tip := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
	v.tipFailureFailed(cut, tip, 1, tipFailureWrites(tipFailureFirst))
	v.tipFailureFiled(tipFailureFirst, tip, "ready", false, []string{"priority:p1", "stage:ready", "type:bug"},
		"**Parent:** #1   **Scope:** none   **Priority:** p1",
		"## Context\n\nStory #1's branch `"+tipBranch+"` fails its checks at `"+tip+"`, against the story's baseline, `main` at `"+cut+"`.",
		"### Rule 1: verify.commands",
		"This command failed, exit status 1:\n\n```text\ntest ! -f breaks.txt\n```",
		"## Change", "## Acceptance criteria\n\n- Rule 1: each of `verify.commands` passes on the story branch.",
		"## Definition of done\n\n- verify.commands pass", "## Test plan")
	if got := f.State().Issues[tipFailureFirst-1].Title; got != "Fix story #1's branch, which fails its checks at "+tip[:7] {
		t.Errorf("the bug's title = %q", got)
	}
	v.tipSettle()
	v.tipQuiet("after the filing")
	if got := len(f.State().Issues); got != tipFailureFirst {
		t.Errorf("the fake has %d issues; want the bug alone filed", got)
	}
}

// A tip that fails rule 2 and a command too, as the same push changes a
// protected path and breaks a command, files the human task alone, never
// a bug, whatever else it fails: rule 2 stops the checks.
func TestTipFailureRules1And2(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	cut := v.tipFailureCut()
	tip := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n", "breaks.txt": "a person's change\n"})
	v.tipFailureFailed(cut, tip, 0, tipFailureWrites(tipFailureFirst))
	v.tipFailureFiled(tipFailureFirst, tip, "needs-human", true, []string{"priority:p1", "stage:needs-human", "type:task"}, "secrets/key.txt")
	v.tipSettle()
	v.tipQuiet("after the filing")
	if got := len(f.State().Issues); got != tipFailureFirst {
		t.Errorf("the fake has %d issues; want the human task alone filed", got)
	}
}

// A tip that deletes a test fails rule 3, and files one bug naming rule 3
// and the file.
func TestTipFailureDeletedTest(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	v.o.commit("quote_test.go", "package quote\n")
	cut := v.tipFailureCut()
	tip := v.tipPush(tipBranch, map[string]string{"quote_test.go": ""})
	v.tipFailureFailed(cut, tip, 1, tipFailureWrites(tipFailureFirst))
	v.tipFailureFiled(tipFailureFirst, tip, "ready", false, []string{"priority:p1", "stage:ready", "type:bug"},
		"### Rule 3: test deletions and skip markers", "quote_test.go",
		"- Rule 3: test deletions and skip markers passes on the story branch.")
	v.tipSettle()
	v.tipQuiet("after the filing")
}

// tipFailureChecks are the check runs story 1's branch's tips leave: the
// cut's success, then each of failed's failure, in the order the fake
// keeps them, by SHA.
func tipFailureChecks(cut string, failed ...string) []githubtest.CheckRunState {
	checks := []githubtest.CheckRunState{tipCheck(cut, tipBranch, "success")}
	for _, tip := range failed {
		checks = append(checks, tipCheck(tip, tipBranch, "failure"))
	}
	slices.SortStableFunc(checks, func(a, b githubtest.CheckRunState) int { return strings.Compare(a.SHA, b.SHA) })
	return checks
}

// While a failure's bug of the story is open, a later tip that passes rule
// 2 and fails another rule files no second bug: it gets one comment on the
// open bug, naming the branch, the tip and the failed command with its
// output, fenced, marked failure tip=<SHA>, before its check. A fresh
// daemon reads that tip's check and posts nothing. A later tip that fails
// rule 2 still files its human task, whatever bug is open.
func TestTipFailureBugOpen(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	cut := v.tipFailureCut()
	first := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
	v.tipFailureFailed(cut, first, 1, tipFailureWrites(tipFailureFirst))
	v.tipSettle()

	second := v.tipPush(tipBranch, map[string]string{"breaks.txt": "another change\n"})
	from := len(f.Writes())
	v.tipJudged(second, 1, tipFailureChecks(cut, first, second))
	if got, want := writesOf(f)[from:], []string{"POST /issues/3/comments", "POST /check-runs"}; !slices.Equal(got, want) {
		t.Errorf("the pass on the second failing tip wrote %q; want %q", got, want)
	}
	bug := f.State().Issues[tipFailureFirst-1]
	comment := "The story branch `" + tipBranch + "` moved to `" + second + "`, which passes rule 2 but fails its checks against the story's baseline, `main` at `" +
		cut + "`. This bug is open, so it's this bug's to fix too, and no second bug is filed.\n\n### Rule 1: verify.commands\n\n" +
		"This command failed, exit status 1:\n\n```text\ntest ! -f breaks.txt\n```\n\nIt printed nothing.\n\n<!-- issuedog:failure tip=" + second + " -->"
	if len(bug.Comments) != 2 || bug.Comments[1].Author != app || bug.Comments[1].Body != comment {
		t.Errorf("the bug's comments = %+v; want its first move, then the App's\n%s", bug.Comments, comment)
	}
	if got := len(f.State().Issues); got != tipFailureFirst {
		t.Errorf("the fake has %d issues; want no second bug", got)
	}
	v.tipSettle()
	v.tipQuiet("after the comment")

	v = v.restart()
	from, ran := len(f.Writes()), v.checksRan()
	v.passes(2)
	if got := writesOf(f)[from:]; len(got) != 0 || v.checksRan() != ran {
		t.Errorf("after the restart, the passes wrote %q and ran the commands %d times; want nothing", got, v.checksRan()-ran)
	}
	v.tipQuiet("after the restart")

	third := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n"})
	from = len(f.Writes())
	v.tipJudged(third, 0, tipFailureChecks(cut, first, second, third))
	if got, want := writesOf(f)[from:], tipFailureWrites(tipFailureFirst+1); !slices.Equal(got, want) {
		t.Errorf("the pass on the tip that fails rule 2 wrote %q; want %q", got, want)
	}
	if is := f.State().Issues[tipFailureFirst]; is.Parent != 1 || !strings.HasSuffix(is.Body, tipFailureOrigin(third, true)) {
		t.Errorf("#%d is below #%d, with the body\n%s\nwant the human task for %s below the story", tipFailureFirst+1, is.Parent, is.Body, third)
	}
}

// A closed issue for the failing tip, linked under the story, is found and
// reused: the pass on the tip files nothing, writes nothing to it, and
// posts the tip's check alone.
func TestTipFailureClosedIssue(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	cut := v.tipFailureCut()
	tip := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
	filed := f.AddIssue(app, "Fix story #1's branch", "Rule 1 failed.\n\n"+tipFailureOrigin(tip, false), "type:bug", "priority:p1")
	f.SetParent(filed, 1)
	f.CloseBy(filed, "owner")
	from := len(f.Writes())
	v.tipJudged(tip, 1, tipFailureChecks(cut, tip))
	if got, want := writesOf(f)[from:], []string{"POST /check-runs"}; !slices.Equal(got, want) {
		t.Errorf("the pass on the failing tip wrote %q; want %q", got, want)
	}
	if got := len(f.State().Issues); got != filed {
		t.Errorf("the fake has %d issues; want none filed after #%d", got, filed)
	}
	if is := f.State().Issues[filed-1]; len(is.Comments) != 0 || !is.Closed {
		t.Errorf("#%d is closed %t, with the comments %+v; want it closed, with none", filed, is.Closed, is.Comments)
	}
	v.tipSettle()
	v.tipQuiet("after the check")
}

// Once the failure's bug closed, as its PR's merge into the story branch
// closes it, a tip after it that fails again, such as that merge's, files
// a new bug, not a comment on the closed one.
func TestTipFailureBugClosed(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	cut := v.tipFailureCut()
	first := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
	v.tipFailureFailed(cut, first, 1, tipFailureWrites(tipFailureFirst))
	v.tipSettle()
	f.CloseBy(tipFailureFirst, "owner")
	v.tipSettle()

	merge := v.tipPush(tipBranch, map[string]string{"breaks.txt": "the bug's merge\n"})
	from := len(f.Writes())
	v.tipJudged(merge, 1, tipFailureChecks(cut, first, merge))
	if got, want := writesOf(f)[from:], tipFailureWrites(tipFailureFirst+1); !slices.Equal(got, want) {
		t.Errorf("the pass on the merge's failing tip wrote %q; want %q", got, want)
	}
	if n := len(f.State().Issues[tipFailureFirst-1].Comments); n != 1 {
		t.Errorf("the closed bug has %d comments; want its first move alone", n)
	}
	if is := f.State().Issues[tipFailureFirst]; !strings.HasSuffix(is.Body, tipFailureOrigin(merge, false)) || is.Parent != 1 {
		t.Errorf("#%d is below #%d, with the body\n%s\nwant the bug for %s below the story", tipFailureFirst+1, is.Parent, is.Body, merge)
	}
}

// A 422 on the filing, the issue's create or its link under the story, as
// for a story with 100 sub-issues, is warned of once, and the tip's check
// is posted anyway, so the tip isn't checked again: the passes after write
// nothing more, and log no second warning. A refused create leaves no
// issue, and a refused link leaves the issue, unlinked, at intake. It
// reads logr, so it isn't parallel.
func TestTipFailureRefused(t *testing.T) {
	for _, tt := range []struct {
		name   string
		path   string
		issues int
	}{
		{"the create", "/issues", tipFailureFirst - 1},
		{"the link", "/issues/1/sub_issues", tipFailureFirst},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
			cut := v.tipFailureCut()
			tip := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
			f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == tt.path }, http.StatusUnprocessableEntity)
			stop := logtest.Capture(t, logr.W)
			v.tipJudged(tip, 1, tipFailureChecks(cut, tip))
			v.tipSettle()
			v.tipQuiet("after the refusal")
			logs := stop()
			warning := "o/r#1: its branch " + tipBranch + "'s failing tip " + tip + " isn't filed, so only its check is posted"
			if got := strings.Count(logs, warning); got != 1 {
				t.Errorf("the refusal was warned of %d times; want once, in\n%s", got, logs)
			}
			st := f.State()
			if len(st.Issues) != tt.issues || len(st.Issues[0].SubIssues) != 0 {
				t.Errorf("the fake has %d issues, and the story the sub-issues %v; want %d, and none", len(st.Issues), st.Issues[0].SubIssues, tt.issues)
			}
		})
	}
}

// A crash just after each write of the filing, a write GitHub made whose
// answer was lost, then a fresh daemon: its first pass reads the tip's
// checks, finds none, checks the tip again, finds what was filed, open,
// and makes only the writes left, so the failure ends filed once, linked
// once, moved once, with the tip's check. After a crash just after the
// check, the fresh daemon reads it, and runs and files nothing.
func TestTipFailureCrash(t *testing.T) {
	t.Parallel()
	for k, write := range tipFailureWrites(tipFailureFirst) {
		t.Run(write, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
			cut := v.tipFailureCut()
			tip := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
			method, path, _ := strings.Cut(write, " ")
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == method && r.Path == path },
				func() { f.FailWriteAfter(len(f.Writes()) + 1) })
			if err := v.d.Iterate(context.Background()); err == nil {
				t.Fatalf("the pass whose answer to %s was lost gave no error", write)
			}
			f.FailWriteAfter(0)
			if got := writesOf(f); !slices.Equal(got[len(got)-k-1:], tipFailureWrites(tipFailureFirst)[:k+1]) {
				t.Fatalf("the pass wrote %q; want it to end in %q", got, tipFailureWrites(tipFailureFirst)[:k+1])
			}
			v = v.restart()
			ran, again := v.checksRan(), 1
			if write == "POST /check-runs" {
				again = 0
			}
			v.passes(3)
			if v.checksRan() != ran+again {
				t.Errorf("after the restart, the commands ran %d times; want %d", v.checksRan()-ran, again)
			}
			v.tipFailureFiled(tipFailureFirst, tip, "ready", false, []string{"priority:p1", "stage:ready", "type:bug"}, "### Rule 1: verify.commands")
			if got, want := f.State().CheckRuns, tipFailureChecks(cut, tip); !slices.Equal(got, want) {
				t.Errorf("after the restart, the check runs = %+v; want %+v", got, want)
			}
			if got := len(f.State().Issues); got != tipFailureFirst {
				t.Errorf("the fake has %d issues; want one filed", got)
			}
			v.tipQuiet("after the restart")
		})
	}
}

// A crash between the filing of a bug and its first move leaves it at
// intake, the App's: before the next check of its tip writes that move,
// neither a trusted user's approved nor their /track admits it to
// clarifying, where the interviewer doesn't run on such a bug (SPEC §6.2):
// approved changes nothing, and /track is rejected, saying why. Then the
// tip's check files the move to ready, and the issue is never at
// clarifying.
func TestTipFailureIntakeAdmit(t *testing.T) {
	t.Parallel()
	for _, admit := range []string{"approved", "/track"} {
		t.Run(admit, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
			v.tipFailureCut()
			tip := v.tipPush(tipBranch, map[string]string{"breaks.txt": "a person's change\n"})
			link := func(r githubtest.Request) bool {
				return r.Method == http.MethodPost && r.Path == "/issues/1/sub_issues"
			}
			f.BeforeRequest(link, func() { f.FailWrite(len(f.Writes()) + 1) })
			if err := v.d.Iterate(context.Background()); err == nil {
				t.Fatal("the pass whose link failed gave no error")
			}
			f.FailWrite(0)
			if admit == "approved" {
				f.AddLabel(tipFailureFirst, "owner", "approved")
			} else {
				f.AddComment(tipFailureFirst, "owner", "/track")
			}
			v = v.restart()
			v.passes(3)
			is := f.State().Issues[tipFailureFirst-1]
			var moves []string
			for _, c := range is.Comments {
				if strings.Contains(c.Body, "issuedog:transition") {
					moves = append(moves, c.Body)
				}
			}
			if len(moves) != 1 || !strings.HasSuffix(moves[0], tipFailureMove("ready", tip)) || pickStage(f, tipFailureFirst) != "stage:ready" {
				t.Errorf("#%d's moves = %q, at %s; want its first move to ready alone", tipFailureFirst, moves, pickStage(f, tipFailureFirst))
			}
			if admit == "/track" && !slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool {
				return c.Author == app && strings.Contains(c.Body, "which `/track` doesn't admit")
			}) {
				t.Errorf("#%d's comments = %+v; want /track rejected, saying why", tipFailureFirst, is.Comments)
			}
		})
	}
}

// tipFailureAtIntake is newFake with issue 1 a bug the App filed for story
// 9's branch's failing tip, with no first move, at stage:intake, with
// priority:p1. Then it runs more.
func tipFailureAtIntake(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue(app, "Fix story #9's branch", "Rule 1 failed.\n\n<!-- issuedog:origin story=#9 tip="+strings.Repeat("e", 40)+" -->")
		f.SetType(1, "bug")
		f.AddLabel(1, app, "priority:p1")
		f.AddLabel(1, app, "stage:intake")
		more(t, f)
		return f
	}
}

// approved, added by a trusted user, admits no bug filed for a failure on
// a story branch at intake, where only its first move takes it on (SPEC
// §6.2): it stays at intake, with nothing written, pass after pass.
func TestTipFailureApprovedLabel(t *testing.T) {
	t.Parallel()
	f := tipFailureAtIntake(func(t testing.TB, f *githubtest.Fake) { f.AddLabel(1, "owner", "approved") })(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	approvalState(t, f, []string{"approved", "priority:p1", "stage:intake"}, nil)
}

// /track on a bug filed for a failure on a story branch, at intake, is
// rejected, with the App's 👎 and one reply saying why, and it stays at
// intake.
func TestTipFailureTrack(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := tipFailureAtIntake(func(t testing.TB, f *githubtest.Fake) {
		if id := f.AddComment(1, "owner", "/track"); id != 1001 {
			t.Fatalf("/track's comment ID = %d, want 1001", id)
		}
	})
	githubtest.Replay(t, setup, approvalPass(t, o))

	f := run(t, setup, approvalPass(t, o))
	approvalState(t, f, []string{"priority:p1", "stage:intake"}, []string{"/track",
		"> /track\n\nI didn't apply this command: this issue was filed for a failure on its story's branch, which `/track` doesn't admit: " +
			"its own first move, written when that failing tip is checked again, puts it in the pipeline.\n\n<!-- issuedog:reply reason=rejected cmd=1001 -->"})
	if r := f.State().Issues[0].Comments[0].Reactions; !slices.Equal(r, []string{app + " -1"}) {
		t.Errorf("/track's reactions = %q, want the App's 👎", r)
	}
}

// Replay passes for the filing (SPEC §5.2): the issue, its link and its
// first move, then the check, for a bug and for a human task, and for a
// later tip while the failure's bug is open, the comment on it, then the
// check. Each pass is a fresh daemon's, with its own bare clone, and the
// digest and assignment off, so its only writes are the filing's and the
// check.
func TestTipFailureReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		files map[string]string
		open  bool // a failure's bug of the story is open, for an earlier tip
		want  []string
	}{
		{"a bug", map[string]string{"breaks.txt": "a person's change\n"}, false, tipFailureWrites(2)},
		{"a human task", map[string]string{"secrets/key.txt": "a key\n"}, false, tipFailureWrites(2)},
		{"a comment on the open bug", map[string]string{"breaks.txt": "a person's change\n"}, true, []string{"POST /issues/2/comments", "POST /check-runs"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := newVerdictRigWith(t, newFake(t), 3, "", `, "test ! -f breaks.txt"`)
			base.tipCut(tipBranch)
			tip := base.tipPush(tipBranch, tt.files)
			roots := map[*githubtest.Fake]string{}
			setup := func(tb testing.TB) *githubtest.Fake {
				f := newFake(tb)
				f.SetPermission(wholeRunOwner, "admin")
				f.SetGitRemote(base.o.url)
				tipStory(tb, f)
				if tt.open {
					earlier := strings.Repeat("e", 40)
					bug := f.AddIssue(app, "Fix story #1's branch", "Rule 1 failed.\n\n"+tipFailureOrigin(earlier, false), "type:bug", "priority:p1")
					f.AddLabel(bug, app, "stage:ready")
					f.AddComment(bug, app, "Filed.\n\n"+tipFailureMove("ready", earlier))
					f.SetParent(bug, 1)
				}
				f.ForgetWrites()
				roots[f] = t.TempDir()
				return f
			}
			pass := func(f *githubtest.Fake) error {
				v := &verdictRig{rig: newRig(t, f, base.o, roots[f]), o: base.o, cfg: base.cfg, ran: base.ran, noPick: true}
				v.wire()
				v.d.Digest, v.d.Assign = false, false
				return v.d.Iterate(context.Background())
			}
			clean := setup(t)
			if err := pass(clean); err != nil {
				t.Fatal(err)
			}
			if got := writesOf(clean); !slices.Equal(got, tt.want) {
				t.Errorf("the clean pass wrote %q; want %q", got, tt.want)
			}
			if got, want := clean.State().CheckRuns, []githubtest.CheckRunState{tipCheck(tip, tipBranch, "failure")}; !slices.Equal(got, want) {
				t.Errorf("the clean pass left the check runs %+v; want %+v", got, want)
			}
			githubtest.Replay(t, setup, pass)
		})
	}
}
