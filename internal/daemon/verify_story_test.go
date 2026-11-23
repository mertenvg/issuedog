package daemon_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A task in a story at verifying (SPEC §7.5, §10.1, §10.2): the checks and
// the verifier judge its head against its story branch's tip, with the
// story's baseline as rule 2's, on a verdictRig wired as main's, with real
// temporary repos. Task #1 is in story #2, whose branch the test cuts and
// pushes to in the origin itself, and the task is put at verifying by its
// markers, with its head in the daemon's clone, as nothing in the daemon
// runs a task in a story yet. The rig's verify.commands also fail on a head
// with broken.txt (personBrokenCommand).

// storyVerifyBranch is story #2's branch, from its title, tipStoryTitle.
const storyVerifyBranch = "issuedog/2-quote-expiry"

// newStoryVerifyRig is a verdictRig on a new fake, whose verify.commands
// also fail on a head with broken.txt.
func newStoryVerifyRig(t *testing.T) (*githubtest.Fake, *verdictRig) {
	t.Helper()
	f := newFake(t)
	return f, newVerdictRigWith(t, f, 3, "", `, "`+personBrokenCommand+`"`)
}

// storyCut cuts story #2's branch from main in v's origin, then pushes a
// commit of the story's to it, story.txt, and returns the cut, main's
// commit then, and the branch's tip.
func (v *verdictRig) storyCut() (string, string) {
	v.t.Helper()
	cut := v.tipCut(storyVerifyBranch)
	return cut, v.tipPush(storyVerifyBranch, map[string]string{"story.txt": "the story's change\n"})
}

// storyHead commits files on base in v's origin, each path to its content,
// as the task's implementer would, and makes that commit task 1's head in
// v's clone, as its run's handoff left it (SPEC §3.2), and returns it.
func (v *verdictRig) storyHead(base string, files map[string]string) string {
	v.t.Helper()
	w := v.o.work
	gitIn(v.t, w, "checkout", "--quiet", "-B", "storytask", base)
	for _, path := range slices.Sorted(maps.Keys(files)) {
		full := filepath.Join(w, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			v.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(files[path]), 0o644); err != nil {
			v.t.Fatal(err)
		}
		gitIn(v.t, w, "add", path)
	}
	gitIn(v.t, w, "commit", "--quiet", "-m", "the task's run")
	head := gitIn(v.t, w, "rev-parse", "HEAD")
	gitIn(v.t, w, "checkout", "--quiet", "main")
	v.storyFetch(head)
	return head
}

// storyFetch makes head, a commit in v's origin's work tree on its
// storytask branch, task 1's head in v's clone.
func (v *verdictRig) storyFetch(head string) {
	v.t.Helper()
	gitIn(v.t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", v.o.work, "+refs/heads/storytask:"+repo.TasksRef+"1")
	if got := gitIn(v.t, v.d.Clone.Dir(), "rev-parse", repo.TasksRef+"1"); got != head {
		v.t.Fatalf("task 1's head in the daemon's clone is %s, want %s", got, head)
	}
}

// storyVerifyTask files task #1 at verifying, as its implementer's move left it,
// on attempt 1, naming head, then story #2, at stage, and makes the task
// its sub-issue.
func storyVerifyTask(t testing.TB, f *githubtest.Fake, head, stage string) {
	t.Helper()
	if n, _ := verifierTask(f, head); n != 1 {
		t.Fatalf("the task is #%d, want #1", n)
	}
	if n := tipStoryAt(f, tipStoryTitle, stage, "type:story"); n != 2 {
		t.Fatalf("the story is #%d, want #2", n)
	}
	f.SetParent(1, 2)
}

// storyTipJudged runs the pass that checks tip, story #2's branch's tip, as
// success, one run of the commands, and must leave only that check, with
// the task still at verifying.
func (v *verdictRig) storyTipJudged(tip string) {
	v.t.Helper()
	ran := v.checksRan()
	v.pass()
	want := []githubtest.CheckRunState{tipCheck(tip, storyVerifyBranch, "success")}
	if got := v.f.State().CheckRuns; !slices.Equal(got, want) || v.checksRan() != ran+1 || pickStage(v.f, 1) != "stage:verifying" {
		v.t.Fatalf("the tip's pass left the check runs %+v, ran the commands %d times, with the task at %s; want %+v, once, and verifying",
			got, v.checksRan()-ran, pickStage(v.f, 1), want)
	}
}

// storyFindings is the body of task 1's one move back to in-progress on
// findings, failing the test unless it's there, and the task with it.
func storyFindings(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	moves := personComments(t, f, "<!-- issuedog:transition from=verifying to=in-progress ")
	if len(moves) != 1 || pickStage(f, 1) != "stage:in-progress" {
		t.Fatalf("the task at %s, its findings moves %+v; want in-progress, by one", pickStage(f, 1), moves)
	}
	return moves[0].Body
}

// storyAgainst is how the findings name what task 1's head was checked
// against: story #2's branch at tip.
func storyAgainst(tip string) string {
	return "the story branch `" + storyVerifyBranch + "` at `" + tip + "`"
}

// A clean head on the story branch's tip, once that tip is checked as
// success: the next pass runs the checks on it, against the tip, and moves
// nothing, and the one after runs the verifier on it alone. Its checks.md
// names the story branch, the tip and the story's baseline, the cut, and
// its diff.patch starts at the head's merge base with the tip, so it holds
// the task's change and not the story's. Its pass moves nothing: the pass
// after, the PR step's, opens its PR into the story branch and moves it to
// pr-open (pr_story_test.go), with no command run, and once that's read,
// which merges the PR, and the next pass carries the tip's check over to
// the merge and closes the task, with no command run (storyMergeSettles),
// the passes after make no request but 304s, run no command and start no
// run.
func TestVerifyStoryPass(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	cut, tip := v.storyCut()
	head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	storyVerifyTask(t, f, head, "planned")
	comments := len(f.State().Issues[0].Comments)
	v.storyTipJudged(tip)

	v.pass()
	if v.checksRan() != 2 || v.runs() != 0 || pickStage(f, 1) != "stage:verifying" || len(f.State().Issues[0].Comments) != comments {
		t.Fatalf("the checks' pass ran the commands %d times in all, claude %d, with the task at %s and %d comments; want twice, never, verifying, and %d",
			v.checksRan(), v.runs(), pickStage(f, 1), len(f.State().Issues[0].Comments), comments)
	}
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	if v.runs() != 1 || v.leaseOf("1").Start != head || len(passComments(f, 1)) != 1 || v.checksRan() != 2 {
		t.Fatalf("after %d runs, the verifier started at %s, with %d passes, the commands run %d times; want one, at %s, one pass, and twice",
			v.runs(), v.leaseOf("1").Start, len(passComments(f, 1)), v.checksRan(), head)
	}
	checks := v.file("ctx.1/checks.md")
	want := "Rules 2 to 7 passed, against " + storyAgainst(tip) + ", the tip this task's head merges into: rule 7's target is that tip, " +
		"and rule 2's baseline is the story's, `main` at `" + cut + "`. diff.patch starts at the head's merge base with that tip."
	if !strings.Contains(checks, want) {
		t.Errorf("checks.md =\n%s\nwant it to hold\n%s", checks, want)
	}
	if patch := v.file("ctx.1/diff.patch"); !strings.Contains(patch, "quote.txt") || strings.Contains(patch, "story.txt") {
		t.Errorf("diff.patch =\n%s\nwant the task's quote.txt, and not the story's story.txt", patch)
	}

	v.pass()
	if prs := f.State().PRs; len(prs) != 1 || prs[0].Base != storyVerifyBranch || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the PR step's pass left the PRs %+v, the task at %s; want one into %s, and pr-open", prs, pickStage(f, 1), storyVerifyBranch)
	}
	v.storyMergeSettles(f.State().PRs[0].Number)
	v.quietPass("the pass after a story task's PR step")
	v.quietPass("again")
	if pickStage(f, 1) != "stage:completed" || v.runs() != 1 || v.checksRan() != 2 {
		t.Errorf("the task is at %s, after %d runs and the commands run %d times; want completed, one, and twice, as the merge's new tip has the check carried over",
			pickStage(f, 1), v.runs(), v.checksRan())
	}
}

// A head whose command fails moves the task back to in-progress on its
// findings, as any task's does, with a comment naming the head and the
// story branch and tip it was checked against, in place of main.
func TestVerifyStoryFailingCommand(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	_, tip := v.storyCut()
	head := v.storyHead(tip, map[string]string{"broken.txt": "broken\n"})
	storyVerifyTask(t, f, head, "planned")
	v.storyTipJudged(tip)
	v.pass()
	body := storyFindings(t, f)
	want := "Verify failed on attempt 1 of 3, at `" + head + "`, checked against " + storyAgainst(tip) + ".\n\n### Rule 1: verify.commands\n\n" +
		"This command failed, exit status 1:\n\n```text\n" + personBrokenCommand + "\n```"
	if !strings.HasPrefix(body, want) || strings.Contains(body, "`main` at") {
		t.Errorf("the findings comment =\n%s\nwant it to start\n%s\nand never name main", body, want)
	}
	if v.checksRan() != 2 || v.runs() != 0 {
		t.Errorf("the commands ran %d times, claude %d; want twice, the tip's and the head's, and never", v.checksRan(), v.runs())
	}
}

// Rule 7's target is the story branch's tip: a head cut from the story's
// baseline that changes f.txt, as the story's tip did since, merges
// cleanly with main but not with the tip, so it fails rule 7, naming the
// tip and the file, and every command still runs.
func TestVerifyStoryConflict(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	cut := v.tipCut(storyVerifyBranch)
	tip := v.tipPush(storyVerifyBranch, map[string]string{"f.txt": "the story's text\n"})
	head := v.storyHead(cut, map[string]string{"f.txt": "the task's text\n"})
	storyVerifyTask(t, f, head, "planned")
	v.storyTipJudged(tip)
	v.pass()
	body := storyFindings(t, f)
	for _, want := range []string{
		"checked against " + storyAgainst(tip) + ".",
		"### Rule 7: merge with the target",
		"doesn't merge cleanly with its target " + tip,
		"- f.txt",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the findings comment =\n%s\nwant it to hold\n%s", body, want)
		}
	}
	if v.checksRan() != 2 {
		t.Errorf("the commands ran %d times, want twice: the tip's and the head's", v.checksRan())
	}
}

// Rule 2's baseline is the story's, the merge base of its tip and main
// (SPEC §10.1): a head on the tip passes it once main changed a protected
// path since the cut, which the tip doesn't have, so the verifier runs on
// it next. A head that changes a protected path itself fails it, and no
// command runs on it.
func TestVerifyStoryProtected(t *testing.T) {
	t.Parallel()
	t.Run("main's change since the cut", func(t *testing.T) {
		t.Parallel()
		f, v := newStoryVerifyRig(t)
		_, tip := v.storyCut()
		v.prMainCommit("secrets/key.txt", "main's own key\n")
		head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
		storyVerifyTask(t, f, head, "planned")
		v.storyTipJudged(tip)
		v.pass()
		if v.checksRan() != 2 || pickStage(f, 1) != "stage:verifying" {
			t.Fatalf("the commands ran %d times, the task at %s; want twice, and verifying", v.checksRan(), pickStage(f, 1))
		}
		v.out(1, verifierPass(t, "Fine."))
		v.pass()
		if v.runs() != 1 || len(passComments(f, 1)) != 1 {
			t.Errorf("after the checks passed, claude ran %d times, with %d passes; want the verifier once, and its pass", v.runs(), len(passComments(f, 1)))
		}
	})
	t.Run("the head's own change", func(t *testing.T) {
		t.Parallel()
		f, v := newStoryVerifyRig(t)
		_, tip := v.storyCut()
		head := v.storyHead(tip, map[string]string{"secrets/new.txt": "a key\n"})
		storyVerifyTask(t, f, head, "planned")
		v.storyTipJudged(tip)
		v.pass()
		body := storyFindings(t, f)
		for _, want := range []string{"checked against " + storyAgainst(tip) + ".", "### Rule 2: protected paths", "secrets/new.txt"} {
			if !strings.Contains(body, want) {
				t.Errorf("the findings comment =\n%s\nwant it to hold\n%s", body, want)
			}
		}
		if v.checksRan() != 1 {
			t.Errorf("the commands ran %d times, want once, the tip's alone", v.checksRan())
		}
	})
}

// A pass is kept by the head and the tip: a tip that moves between the
// checks and the verifier is checked first, then the head's checks run
// again against it, and only then the verifier, whose checks.md names the
// new tip. No verifier run starts on the old tip's pass.
func TestVerifyStoryTipMoves(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	_, tip := v.storyCut()
	head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	storyVerifyTask(t, f, head, "planned")
	v.storyTipJudged(tip)
	v.pass()
	if v.checksRan() != 2 {
		t.Fatalf("the commands ran %d times, want twice", v.checksRan())
	}
	moved := v.tipPush(storyVerifyBranch, map[string]string{"more.txt": "more of the story\n"})
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	if v.checksRan() != 3 || v.runs() != 0 {
		t.Fatalf("the pass after the tip moved ran the commands %d times in all, claude %d; want 3, the new tip's, and never", v.checksRan(), v.runs())
	}
	v.pass()
	if v.checksRan() != 4 || v.runs() != 0 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the next pass ran the commands %d times in all, claude %d, the task at %s; want 4, the head's on the new tip, never, and verifying",
			v.checksRan(), v.runs(), pickStage(f, 1))
	}
	v.pass()
	if v.runs() != 1 || v.leaseOf("1").Start != head {
		t.Fatalf("after %d runs, the verifier started at %s; want one, at %s", v.runs(), v.leaseOf("1").Start, head)
	}
	if checks := v.file("ctx.1/checks.md"); !strings.Contains(checks, "against "+storyAgainst(moved)+",") {
		t.Errorf("checks.md =\n%s\nwant it to name the new tip %s", checks, moved)
	}
}

// A task waits at verifying, uncounted, while its story branch's tip has
// no issuedog/tip naming the branch as success: a held story's tip, which
// is never checked, and a tip whose check is a failure. No command runs on
// the task, nothing is written on it, and its passes make no request but
// 304s.
func TestVerifyStoryTipUnchecked(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, stage string
		files       map[string]string
		ran         int
	}{
		{"a held story's tip", "held", map[string]string{"story.txt": "the story's change\n"}, 0},
		{"a failing tip", "planned", map[string]string{"broken.txt": "the story broke it\n"}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, v := newStoryVerifyRig(t)
			v.tipCut(storyVerifyBranch)
			tip := v.tipPush(storyVerifyBranch, tt.files)
			head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
			storyVerifyTask(t, f, head, tt.stage)
			comments := len(f.State().Issues[0].Comments)
			v.passes(2)
			if v.checksRan() != tt.ran || pickStage(f, 1) != "stage:verifying" || len(f.State().Issues[0].Comments) != comments || v.runs() != 0 {
				t.Fatalf("after two passes, the commands ran %d times, the task at %s with %d comments, claude %d times; want %d, verifying, %d, and never",
					v.checksRan(), pickStage(f, 1), len(f.State().Issues[0].Comments), v.runs(), tt.ran, comments)
			}
			v.quietPass("waiting for the tip")
			v.quietPass("again")
		})
	}
}

// The wait for a tip with no success is warned of once, not every pass. It
// reads logr, so it isn't parallel.
func TestVerifyStoryTipWaitWarnedOnce(t *testing.T) {
	f, v := newStoryVerifyRig(t)
	v.tipCut(storyVerifyBranch)
	tip := v.tipPush(storyVerifyBranch, map[string]string{"broken.txt": "the story broke it\n"})
	head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	storyVerifyTask(t, f, head, "planned")
	stop := logtest.Capture(t, logr.W)
	v.passes(4)
	logs := stop()
	want := "o/r#1 waits at verifying, as its story branch " + storyVerifyBranch + "'s tip " + tip + " has no issuedog/tip naming it as success"
	if got := strings.Count(logs, want); got != 1 {
		t.Errorf("the wait was warned of %d times; want once, in\n%s", got, logs)
	}
}

// A person's head on a task in a story is judged against the story
// branch's tip too: one whose command fails takes the task back to
// pr-open on person-pushed, with issuedog/verify failure on the head,
// naming the story's baseline, the newest main commit its commands ran
// with, and the findings naming the story branch and tip, and it's never
// sent to in-progress or counted.
func TestVerifyStoryPersonHead(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	cut, tip := v.storyCut()
	verified := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	person := v.storyHead(verified, map[string]string{"broken.txt": "a person's change\n"})
	// The person pushed it to the task's branch.
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "storytask:refs/heads/issuedog/1-refuse-an-expired-quote")
	storyVerifyTask(t, f, verified, "planned")
	f.AddComment(1, app, "The PR is open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rpass head="+verified+" pr=9 -->")
	moved := f.AddComment(1, app, "A new head.\n\n<!-- issuedog:transition from=pr-open to=verifying event=n"+person+" head="+person+" -->")
	v.storyTipJudged(tip)
	v.pass()
	move, fields := personBack(t, f, person)
	if fields["event"] != "v"+strconv.FormatInt(moved, 10) || fields["pr"] != "9" {
		t.Errorf("the move's fields = %v; want event=v%d, and pr=9", fields, moved)
	}
	if want := "checked against " + storyAgainst(tip) + "."; !strings.Contains(move.Body, want) || strings.Contains(move.Body, "`main` at") {
		t.Errorf("the move's comment =\n%s\nwant it to hold\n%s\nand never name main", move.Body, want)
	}
	want := githubtest.CheckRunState{SHA: person, Name: github.VerifyCheck, Conclusion: "failure", AppID: githubtest.AppID, ExternalID: "main=" + cut}
	if got := f.State().CheckRuns; !slices.Contains(got, want) {
		t.Errorf("the check runs = %+v; want one with %+v", got, want)
	}
	personNever(t, f)
	if v.checksRan() != 2 || v.runs() != 0 {
		t.Errorf("the commands ran %d times, claude %d; want twice, the tip's and the head's, and never", v.checksRan(), v.runs())
	}
}

// A fresh daemon holds no pass and no judged tip: its first pass reads
// the tip's checks once, finds its issuedog/tip naming the branch, and
// runs the head's checks again against it, and only the pass after runs
// the verifier. After the verifier's pass, a fresh daemon runs the checks
// once more, and the pass after is the PR step's, which opens the task's
// PR into its story branch and moves it to pr-open, with no command and no
// run, and once that's read, which merges the PR, and the next pass
// carries the tip's check over to the merge and closes the task
// (storyMergeSettles), the passes after make no request but 304s.
func TestVerifyStoryRestart(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	_, tip := v.storyCut()
	head := v.storyHead(tip, map[string]string{"quote.txt": "expired\n"})
	storyVerifyTask(t, f, head, "planned")
	v.storyTipJudged(tip)
	v.pass()
	if v.checksRan() != 2 {
		t.Fatalf("the commands ran %d times, want twice", v.checksRan())
	}

	v = v.restart()
	from := len(f.Requests())
	v.pass()
	if got := tipReads(f, from, tip); got != 1 || v.checksRan() != 3 || v.runs() != 0 {
		t.Fatalf("the restart's first pass read the tip's checks %d times, ran the commands %d times in all, claude %d; want once, 3, and never",
			got, v.checksRan(), v.runs())
	}
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	if v.runs() != 1 || v.leaseOf("1").Start != head || len(passComments(f, 1)) != 1 {
		t.Fatalf("after %d runs, the verifier started at %s, with %d passes; want one, at %s, and one", v.runs(), v.leaseOf("1").Start, len(passComments(f, 1)), head)
	}

	v = v.restart()
	v.pass()
	if v.checksRan() != 4 || v.runs() != 1 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the second restart's first pass ran the commands %d times in all, claude %d, the task at %s; want 4, once, and verifying",
			v.checksRan(), v.runs(), pickStage(f, 1))
	}
	v.pass()
	if prs := f.State().PRs; len(prs) != 1 || prs[0].Base != storyVerifyBranch || prs[0].HeadSHA != head || pickStage(f, 1) != "stage:pr-open" ||
		v.checksRan() != 4 || v.runs() != 1 {
		t.Fatalf("the PR step's pass left the PRs %+v, the task at %s, the commands run %d times, claude %d; want one into %s at %s, pr-open, 4, and once",
			prs, pickStage(f, 1), v.checksRan(), v.runs(), storyVerifyBranch, head)
	}
	v.storyMergeSettles(f.State().PRs[0].Number)
	v.quietPass("after the second restart")
	v.quietPass("again")
}

// The findings move of a task in a story, a crash at each of its writes,
// then a restart: the replay ends with the same writes (SPEC §5.2). Each
// pass is a fresh daemon, on the same origin, so it reads the tip's check
// once and runs the checks again on the head.
func TestVerifyStoryFindingsReplay(t *testing.T) {
	t.Parallel()
	_, base := newStoryVerifyRig(t)
	_, tip := base.storyCut()
	head := base.storyHead(tip, map[string]string{"broken.txt": "broken\n"})
	setup := func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.SetPermission(wholeRunOwner, "admin")
		f.SetGitRemote(base.o.url)
		storyVerifyTask(tb, f, head, "planned")
		if _, err := f.Client(tb).PostCheck(context.Background(), "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: "branch=" + storyVerifyBranch}); err != nil {
			tb.Fatal(err)
		}
		f.ForgetWrites()
		return f
	}
	pass := func(f *githubtest.Fake) error {
		v := &verdictRig{rig: newRig(t, f, base.o, t.TempDir()), o: base.o, cfg: base.cfg, ran: base.ran}
		v.wire()
		v.storyFetch(head)
		return v.d.Iterate(context.Background())
	}
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	if body := storyFindings(t, clean); !strings.Contains(body, "checked against "+storyAgainst(tip)+".") {
		t.Fatalf("the clean pass's findings comment =\n%s\nwant it to name the story branch and %s", body, tip)
	}
	githubtest.Replay(t, setup, pass)
}

// A head with no merge base with its story branch's tip, as an orphan,
// fails rule 2, as the task's own diff then has no start: the finding
// names the tip as the story branch's, and no command runs on the head.
func TestVerifyStoryNoMergeBase(t *testing.T) {
	t.Parallel()
	f, v := newStoryVerifyRig(t)
	_, tip := v.storyCut()
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "--orphan", "storytask")
	gitIn(t, w, "commit", "--quiet", "-m", "an orphan")
	head := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "checkout", "--quiet", "-f", "main")
	v.storyFetch(head)
	storyVerifyTask(t, f, head, "planned")
	v.storyTipJudged(tip)
	v.pass()
	body := storyFindings(t, f)
	for _, want := range []string{"checked against " + storyAgainst(tip) + ".", "### Rule 2: protected paths",
		"The head doesn't have exactly one merge base with its story branch's tip " + tip + ", so the task's own diff has no start"} {
		if !strings.Contains(body, want) {
			t.Errorf("the findings comment =\n%s\nwant it to hold\n%s", body, want)
		}
	}
	if v.checksRan() != 1 {
		t.Errorf("the commands ran %d times, want once, the tip's alone", v.checksRan())
	}
}
