package daemon_test

import (
	"context"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// When main moves, a task at pr-open is checked on its merge with the new
// main, and its issuedog/verify check says the result (SPEC §7.2, §8.1 step
// 4, §10.4), on a verdictRig wired as main's, with real temporary repos:
// task 1's PR opened at its verified head by the PR step (prHeadOpen), or
// filed at pr-open by hand (prMainTask).

// prMainCommit commits path on main in v's origin and returns main's new
// tip, as a merge to main moves it.
func (v *verdictRig) prMainCommit(path, content string) string {
	v.t.Helper()
	v.o.commit(path, content)
	return v.mainSHA()
}

// prMainConflict commits quote.txt on main, with other text than the
// task's head gives it (verdictRig.head), so the head no longer merges
// cleanly with main, and returns main's new tip.
func (v *verdictRig) prMainConflict() string {
	v.t.Helper()
	return v.prMainCommit("quote.txt", "main's own quote\n")
}

// prMainCheck is the App's check run on head, with conclusion, naming main
// in its external_id, as State has it.
func prMainCheck(head, main, conclusion string) githubtest.CheckRunState {
	return githubtest.CheckRunState{SHA: head, Name: github.VerifyCheck, Conclusion: conclusion, AppID: githubtest.AppID, ExternalID: "main=" + main}
}

// prMainMoves are the App's comments on task 1 that hold a move pr-open →
// in-progress.
func prMainMoves(f *githubtest.Fake) []string {
	var bodies []string
	for _, c := range f.State().Issues[0].Comments {
		if c.Author == app && strings.Contains(c.Body, "<!-- issuedog:transition from=pr-open to=in-progress ") {
			bodies = append(bodies, c.Body)
		}
	}
	return bodies
}

// prMainSeen is the newest comment on task 1, as step 1 reads it, which
// the move back on main carries as seen= (SPEC §5).
func prMainSeen(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	var seen int64
	for _, c := range s.Comments {
		seen = max(seen, c.ID)
	}
	return strconv.FormatInt(seen, 10)
}

// prMainConflictText is the comment of the move back when head doesn't
// merge cleanly with main, quote.txt conflicting.
func prMainConflictText(head, main string) string {
	return "`main` moved to `" + main + "`, and this task's PR, verified at `" + head + "`, doesn't merge cleanly with it. These files conflict:\n\n" +
		"- `quote.txt`\n\nSo the task is back at `in-progress`, for the implementer to merge `main` first and resolve the conflicts.\n\n"
}

// prMainFindingsText is the start of the comment of the move back when
// head's merge with main fails the checks.
func prMainFindingsText(head, main string) string {
	return "`main` moved to `" + main + "`, and this task's PR, verified at `" + head + "`, fails verify once merged with it. " +
		"The findings below are the merge's. The next run merges `main` first.\n\n" +
		"Verify failed at `" + head + "`, checked against `main` at `" + main + "`."
}

// prMainMoved fails unless task 1 is at stage, after one move pr-open →
// in-progress on main's moving: its comment starts with text, and its
// marker is keyed event=m<main>, with seen= seen.
func prMainMoved(t *testing.T, f *githubtest.Fake, stage, text, main, seen string) {
	t.Helper()
	moves := prMainMoves(f)
	if pickStage(f, 1) != stage || len(moves) != 1 || !strings.HasPrefix(moves[0], text) {
		t.Fatalf("the task at %s, its moves to in-progress %q; want %s, after one move, starting\n%s", pickStage(f, 1), moves, stage, text)
	}
	want := map[string]string{"from": "pr-open", "to": "in-progress", "event": "m" + main, "seen": seen}
	if got := prMoveFields(t, moves[0]); !maps.Equal(got, want) {
		t.Errorf("the move's marker has %v; want %v", got, want)
	}
}

// prMainTarget fails unless claude's run k, the implementer's, started at
// head with main named as its target, which its clone holds as
// refs/heads/target, and its input.md gives the move back's comment,
// body, under a heading naming the merge with main (SPEC §3.4, §12).
func (v *verdictRig) prMainTarget(k int, head, main, body string) {
	v.t.Helper()
	l := v.leaseOf(strconv.Itoa(k))
	if l.Start != head || l.Target != main {
		v.t.Errorf("run %d started at %s with the target %q; want %s, with main's %s", k, l.Start, l.Target, head, main)
	}
	if refs := v.file("refs." + strconv.Itoa(k)); !strings.Contains(refs, main+" refs/heads/target") {
		v.t.Errorf("run %d's clone has the refs\n%s\nwant refs/heads/target at %s", k, refs, main)
	}
	text := body[:strings.Index(body, "\n\n<!-- issuedog:transition")]
	got := v.roundInput(k)
	if !strings.Contains(got, "\n## The merge with `main`\n\n```") || !strings.Contains(got, text) {
		v.t.Errorf("run %d's input.md =\n%s\nwant the move's comment under \"The merge with `main`\":\n%s", k, got, text)
	}
}

// prMainReads counts the requests since from that read head's check runs.
func prMainReads(f *githubtest.Fake, from int, head string) int {
	reads := 0
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == "/commits/"+head+"/check-runs" {
			reads++
		}
	}
	return reads
}

// prMainRestart restarts v, and runs the fresh daemon's first pass, which
// must read head's check runs once, run no command and post no check, and
// then a pass that makes no request for the task, its PR or its head.
func (v *verdictRig) prMainRestart(head string) *verdictRig {
	v.t.Helper()
	runs := v.f.State().CheckRuns
	v = v.restart()
	from, ran := len(v.f.Requests()), v.checksRan()
	v.pass()
	if got := prMainReads(v.f, from, head); got != 1 || v.checksRan() != ran || !slices.Equal(v.f.State().CheckRuns, runs) {
		v.t.Errorf("the restart's first pass read the head's checks %d times, ran the commands %d times, and left the checks %+v; want once, none, and %+v",
			got, v.checksRan()-ran, v.f.State().CheckRuns, runs)
	}
	v.prMoveQuiet("after the restart", true)
	return v
}

// A merge into main that doesn't touch the task's files: the PR step's
// checks against main judge the head, so its checks are read only by the
// PR step, and with main unchanged, a pass at pr-open makes no request and
// runs no command. Once
// main moves, the next pass builds the head's merge with it under
// refs/issuedog/merges/1, with main and the head as its parents, runs the
// commands there once, and posts issuedog/verify as success on the head,
// never the merge, naming the new main in its external_id, with the
// commands' results and a link to the task. The task stays at pr-open, the
// PR step's check stays, and the next pass is quiet again. A fresh daemon
// reads the head's checks once, finds the one naming main, and runs
// nothing.
func TestPRMainCleanMove(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	old := v.mainSHA()
	if got := prMainReads(f, 0, head); got != 1 {
		t.Errorf("the head's checks were read %d times; want once, by the PR step, whose checks against main judge the head", got)
	}
	v.prMoveQuiet("main unchanged", true)

	main := v.prMainCommit("other.txt", "main's own change\n")
	ran := v.checksRan()
	v.pass()
	if v.checksRan() != ran+1 {
		t.Errorf("the pass after main moved ran the commands %d times; want once", v.checksRan()-ran)
	}
	want := []githubtest.CheckRunState{prMoveVerified(head, old), prMainCheck(head, main, "success")}
	if got := f.State().CheckRuns; !slices.Equal(got, want) {
		t.Fatalf("the check runs = %+v; want %+v", got, want)
	}
	checks := prMoveChecks(t, f)
	last := checks[len(checks)-1]
	summary := "issuedog checked `" + head + "` for #1 on its merge with `main` at `" + main + "`: the checks passed.\n\n" +
		strings.TrimPrefix(v.prResults(), "## Results\n\n")
	if last.Status != "completed" || last.Output.Title != "issuedog verified this head's merge with main" || last.Output.Summary != summary ||
		last.DetailsURL != "https://github.com/o/r/issues/1" {
		t.Errorf("the check = %+v; want completed, titled for the merge, with the summary\n%s\nand a link to the task", last, summary)
	}
	if got, want := gitIn(t, v.d.Clone.Dir(), "rev-list", "--parents", "-n", "1", repo.MergesRef+"1"), " "+main+" "+head; !strings.HasSuffix(got, want) {
		t.Errorf("the merge result is %q; want a commit whose parents are%s", got, want)
	}
	if pickStage(f, 1) != "stage:pr-open" || len(prMainMoves(f)) != 0 {
		t.Errorf("the task is at %s, its moves to in-progress %q; want pr-open, and none", pickStage(f, 1), prMainMoves(f))
	}
	v.prMoveQuiet("after the check", true)
	v.prMainRestart(head)
}

// The checks on the merge with main come before the pick, and end the
// pass's stage work, as one task a pass is checked: with a task at ready,
// the pass after main moves checks the head's merge and picks nothing, and
// the next pass picks the ready task and starts its run.
func TestPRMainBeforeThePick(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	next := fileAt(f, "ready", "The next task.", "priority:p1", "scope:booking", "type:task")
	main := v.prMainCommit("other.txt", "main's own change\n")
	v.pass()
	if !slices.Contains(f.State().CheckRuns, prMainCheck(head, main, "success")) || pickStage(f, next) != "stage:ready" || v.runs() != 0 {
		t.Fatalf("the check runs = %+v, the ready task at %s, after %d runs; want the merge's success, the task still at ready, and none",
			f.State().CheckRuns, pickStage(f, next), v.runs())
	}
	v.out(1, ivResult(t, implementerOutput(t, "question")))
	v.pass()
	if v.runs() != 1 || v.leaseOf("1").Issue != next {
		t.Errorf("after %d runs, the pass after picked #%d; want the implementer's run on #%d", v.runs(), v.leaseOf("1").Issue, next)
	}
}

// The checks on the merge with main never run on a pass that ran task
// work, so the commands never overlap a claude run: with another task at
// in-progress when main moves, its implementer's pass, its checks' pass
// and its verifier's pass check no merge, and once its verifier's question
// frees the worker, the next pass checks the head's merge.
func TestPRMainAfterTaskWork(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	other := f.AddIssue(wholeRunOwner, "Refuse a stale quote", "Booking a stale quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(other, wholeRunOwner, "stage:in-progress")
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.out(2, ivResult(t, verifierOutput(t, "question")))
	main := v.prMainCommit("other.txt", "main's own change\n")
	checks := len(f.State().CheckRuns)
	for _, step := range []string{"the implementer", "the other task's checks", "the verifier"} {
		v.pass()
		if got := f.State().CheckRuns; len(got) != checks {
			t.Fatalf("%s: the check runs = %+v; want no new one", step, got)
		}
	}
	if v.runs() != 2 || pickStage(f, other) != "stage:clarifying" {
		t.Fatalf("after %d runs, the other task is at %s; want the implementer's and the verifier's, and clarifying", v.runs(), pickStage(f, other))
	}
	v.pass()
	if !slices.Contains(f.State().CheckRuns, prMainCheck(head, main, "success")) {
		t.Errorf("once the worker is free, the check runs = %+v; want the merge's success", f.State().CheckRuns)
	}
}

// A merge into main that changes the task's file too: the pass after
// posts issuedog/verify as failure on the head, naming the new main and
// the conflicting file, and runs no command, then moves the task pr-open →
// in-progress, keyed event=m<main>, naming main, the head and the file.
// No merge result is kept. The implementer's next run starts from the
// head with main named as its target, its input.md giving the move's
// comment under a heading naming the merge with main.
func TestPRMainConflict(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	old := v.mainSHA()
	seen := prMainSeen(t, f)
	main := v.prMainConflict()
	ran := v.checksRan()
	v.pass()
	if v.checksRan() != ran {
		t.Errorf("the conflict's pass ran the commands %d times; want none", v.checksRan()-ran)
	}
	want := []githubtest.CheckRunState{prMoveVerified(head, old), prMainCheck(head, main, "failure")}
	if got := f.State().CheckRuns; !slices.Equal(got, want) {
		t.Fatalf("the check runs = %+v; want %+v", got, want)
	}
	checks := prMoveChecks(t, f)
	last := checks[len(checks)-1]
	summary := "issuedog couldn't merge `" + head + "` for #1 with `main` at `" + main + "`: these files conflict.\n\n- `quote.txt`"
	if last.Output.Title != "This head doesn't merge cleanly with main" || last.Output.Summary != summary {
		t.Errorf("the check = %+v; want it titled for the conflict, with the summary\n%s", last, summary)
	}
	prMainMoved(t, f, "stage:in-progress", prMainConflictText(head, main), main, seen)
	if got := gitIn(t, v.d.Clone.Dir(), "for-each-ref", repo.MergesRef); got != "" {
		t.Errorf("the merge results are %q; want none", got)
	}

	v.out(1, ivResult(t, implementerOutput(t, "question")))
	v.pass()
	if v.runs() != 1 {
		t.Fatalf("claude ran %d times; want the implementer once", v.runs())
	}
	v.prMainTarget(1, head, main, prMainMoves(f)[0])
	if len(f.State().CheckRuns) != 2 || len(prMainMoves(f)) != 1 {
		t.Errorf("after the run, the checks %+v, the moves %q; want two and one", f.State().CheckRuns, prMainMoves(f))
	}
}

// A merge into main that breaks a command only on the merge: the head
// alone passes it, so the PR step's check is a success, but once main
// moves, the commands run on the merge, one fails, and issuedog/verify
// is failure on the head, naming the new main, with the findings. Then
// the task moves back as a conflict does, its comment giving the merge's
// findings, and its next run names main as its target, though the head
// merges cleanly with it.
func TestPRMainFailingCommand(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "test ! -f breaks.txt"`)
	head := v.prHeadOpen()
	old := v.mainSHA()
	seen := prMainSeen(t, f)
	main := v.prMainCommit("breaks.txt", "main's own change\n")
	ran := v.checksRan()
	v.pass()
	if v.checksRan() != ran+1 {
		t.Errorf("the pass after main moved ran the commands %d times; want once", v.checksRan()-ran)
	}
	want := []githubtest.CheckRunState{prMoveVerified(head, old), prMainCheck(head, main, "failure")}
	if got := f.State().CheckRuns; !slices.Equal(got, want) {
		t.Fatalf("the check runs = %+v; want %+v", got, want)
	}
	checks := prMoveChecks(t, f)
	last := checks[len(checks)-1]
	if last.Output.Title != "This head fails verify on its merge with main" || !strings.HasPrefix(last.Output.Summary, prMainFindingsText(head, main)) ||
		!strings.Contains(last.Output.Summary, "This command failed, exit status 1:\n\n```text\ntest ! -f breaks.txt\n```") {
		t.Errorf("the check = %+v; want it titled for the failure, with the merge's findings", last)
	}
	prMainMoved(t, f, "stage:in-progress", prMainFindingsText(head, main), main, seen)
	if body := prMainMoves(f)[0]; !strings.Contains(body, "test ! -f breaks.txt") || !strings.Contains(body, "So the task is back at in-progress, for the implementer to fix.") {
		t.Errorf("the move's comment =\n%s\nwant the failed command, and that the task is back", body)
	}

	v.out(1, ivResult(t, implementerOutput(t, "question")))
	v.pass()
	if v.runs() != 1 {
		t.Fatalf("claude ran %d times; want the implementer once", v.runs())
	}
	v.prMainTarget(1, head, main, prMainMoves(f)[0])
}

// main moves again during the checks on its merge, here just before the
// check is posted: the check names the main the merge was built on, and
// the next pass, whose sync brings the newer main, checks the head again
// on it, and posts a check naming it. The pass after is quiet.
func TestPRMainMovesDuringTheChecks(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	old := v.mainSHA()
	first := v.prMainCommit("other.txt", "main's own change\n")
	var newer string
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/check-runs" },
		func() { newer = v.prMainCommit("later.txt", "main's later change\n") })
	ran := v.checksRan()
	v.pass()
	want := []githubtest.CheckRunState{prMoveVerified(head, old), prMainCheck(head, first, "success")}
	if got := f.State().CheckRuns; !slices.Equal(got, want) || newer == "" || newer == first {
		t.Fatalf("the check runs = %+v, main moved to %q meanwhile; want %+v, and a newer main", got, newer, want)
	}
	v.pass()
	want = append(want, prMainCheck(head, newer, "success"))
	if got := f.State().CheckRuns; !slices.Equal(got, want) || v.checksRan() != ran+2 {
		t.Errorf("the next pass left the check runs %+v, with the commands run %d times; want %+v, and twice", got, v.checksRan()-ran, want)
	}
	v.prMoveQuiet("after the newer main's check", true)
}

// A crash just after the conflict's check, and just after the move's
// comment, each a write GitHub made whose answer was lost, then a fresh
// daemon: the replay posts no second check and no second move. After the
// check, the restart reads the head's checks, finds its failure naming
// main, and makes the move. After the comment, its step 1 finishes the
// move's labels. Either way the implementer's run then names main as its
// target.
func TestPRMainCrash(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		path string // the write whose answer is lost
	}{
		{"after the check", "/check-runs"},
		{"after the move", "/issues/1/comments"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			head := v.prHeadOpen()
			old := v.mainSHA()
			seen := prMainSeen(t, f)
			main := v.prMainConflict()
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == tt.path },
				func() { f.FailWriteAfter(len(f.Writes()) + 1) })
			if err := v.d.Iterate(context.Background()); err == nil {
				t.Fatal("the pass whose answer was lost gave no error")
			}
			want := []githubtest.CheckRunState{prMoveVerified(head, old), prMainCheck(head, main, "failure")}
			if got := f.State().CheckRuns; !slices.Equal(got, want) || pickStage(f, 1) != "stage:pr-open" {
				t.Fatalf("after the lost answer, the check runs = %+v, the task at %s; want %+v, and still at pr-open", got, pickStage(f, 1), want)
			}

			v = v.restart()
			v.out(1, ivResult(t, implementerOutput(t, "question")))
			v.passes(2)
			if got := f.State().CheckRuns; !slices.Equal(got, want) {
				t.Errorf("after the restart, the check runs = %+v; want %+v", got, want)
			}
			prMainMoved(t, f, "stage:clarifying", prMainConflictText(head, main), main, seen)
			if v.runs() != 1 {
				t.Fatalf("after the restart, claude ran %d times; want the implementer once", v.runs())
			}
			v.prMainTarget(1, head, main, prMainMoves(f)[0])
		})
	}
}

// A task the owner sets back to pr-open by hand after main's moving sent
// it to in-progress, with main and its head as they were: that key moves
// it once (SPEC §5.2), so its head isn't checked on that main again, and
// once the override is read, a pass runs nothing, writes nothing, and makes
// no request about the task, its PR or its head.
func TestPRMainSetBackByHand(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	main := v.prMainConflict()
	v.pass()
	if pickStage(f, 1) != "stage:in-progress" || len(prMainMoves(f)) != 1 {
		t.Fatalf("after the conflict, the task at %s, its moves %q; want in-progress, by one", pickStage(f, 1), prMainMoves(f))
	}
	f.RemoveLabel(1, wholeRunOwner, "stage:in-progress")
	f.AddLabel(1, wholeRunOwner, "stage:pr-open")
	ran, checks := v.checksRan(), f.State().CheckRuns
	v.passes(3)
	v.prMoveQuiet("set back by hand", true)
	if pickStage(f, 1) != "stage:pr-open" || len(prMainMoves(f)) != 1 || v.checksRan() != ran || !slices.Equal(f.State().CheckRuns, checks) || v.runs() != 0 {
		t.Errorf("the task at %s, its moves %q, the commands run %d times more, the checks %+v, claude's runs %d; want pr-open, by hand, the one move, none, %+v and none",
			pickStage(f, 1), prMainMoves(f), v.checksRan()-ran, f.State().CheckRuns, v.runs(), checks)
	}
	if want := prMainCheck(head, main, "failure"); !slices.Contains(f.State().CheckRuns, want) {
		t.Errorf("the check runs = %+v; want %+v among them", f.State().CheckRuns, want)
	}
}

// The owner sets the task's stage by hand while its merge with main is
// checked, here just before the conflict's check is posted: the check is
// posted, but the move's read of the issue finds the task moved since, so
// there's no move back.
func TestPRMainTaskMovedMeanwhile(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	main := v.prMainConflict()
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/check-runs" },
		func() {
			f.RemoveLabel(1, wholeRunOwner, "stage:pr-open")
			f.AddLabel(1, wholeRunOwner, "stage:ready")
		})
	v.pass()
	if want := prMainCheck(head, main, "failure"); !slices.Contains(f.State().CheckRuns, want) {
		t.Errorf("the check runs = %+v; want %+v among them", f.State().CheckRuns, want)
	}
	if pickStage(f, 1) != "stage:ready" || len(prMainMoves(f)) != 0 {
		t.Errorf("the task at %s, its moves %q; want ready, by hand, and none", pickStage(f, 1), prMainMoves(f))
	}
}

// prMainTask files task 1 at pr-open, as the PR step's move left it at
// head, its PR #2, the App's, open from prBranch to main with priority:p1,
// and no write logged. The origin's prBranch must be at head.
func prMainTask(t testing.TB, f *githubtest.Fake, head string) {
	t.Helper()
	prMainTaskMove(t, f, head, "<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr=2 -->")
}

// prMainTaskMove is prMainTask, with marker on the move to pr-open, whose
// comment names head.
func prMainTaskMove(t testing.TB, f *githubtest.Fake, head, marker string) {
	t.Helper()
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	if n != 1 {
		t.Fatalf("the task is #%d, want #1", n)
	}
	f.AddLabel(n, app, "stage:pr-open")
	pr, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r",
		github.NewPullRequest{Title: prItemTitle, Head: prBranch, Base: "main", Body: "Closes #1\n\nWhat changed."})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 2 {
		t.Fatalf("the PR is #%d, want #2", pr.Number)
	}
	f.AddLabel(pr.Number, app, "priority:p1")
	f.AddComment(n, app, prMoveText(pr.Number, head)+marker)
	f.ForgetWrites()
}

// A task at pr-open whose move names no head, or a head the bare repo
// lacks, or a PR that isn't the App's, as a marker written by hand might,
// once main moves: its merge isn't built, so the pass gives no error,
// posts no check and runs no command. A second branch with the task's
// prefix keeps step 2 from judging the branch's tip as a new head.
func TestPRMainNotChecked(t *testing.T) {
	t.Parallel()
	missing := strings.Repeat("ab", 20)
	for _, tt := range []struct {
		name, marker string
	}{
		{"no head named", "<!-- issuedog:transition from=verifying to=pr-open run=rv pr=2 -->"},
		{"a head the bare repo lacks", "<!-- issuedog:transition from=verifying to=pr-open run=rv head=" + missing + " pr=2 -->"},
		{"no PR of the App's", "<!-- issuedog:transition from=verifying to=pr-open run=rv head=HEAD pr=99 -->"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			head := v.head()
			gitIn(t, v.o.work, "push", "--quiet", v.o.url, "task:refs/heads/"+prBranch)
			gitIn(t, v.o.work, "push", "--quiet", v.o.url, "task:refs/heads/issuedog/1-another-branch")
			prMainTaskMove(t, f, head, strings.ReplaceAll(tt.marker, "HEAD", head))
			v.prMainCommit("other.txt", "main's own change\n")
			ran := v.checksRan()
			v.passes(2)
			if got := f.State().CheckRuns; len(got) != 0 || v.checksRan() != ran || pickStage(f, 1) != "stage:pr-open" {
				t.Errorf("the check runs = %+v, the commands run %d times, the task at %s; want none, none, and pr-open", got, v.checksRan()-ran, pickStage(f, 1))
			}
		})
	}
}

// A task at pr-open whose head has a check posted before main was named
// in one, a success with no external_id, which names no main: a fresh
// daemon's first pass, with main as it was, reads the head's checks, runs
// the commands once on the merge, and posts a success naming main. The
// passes after run nothing, and once its writes are read, make no request
// for the task, its PR or its head. A restart then reads the head's checks
// once, and runs nothing.
func TestPRMainUnnamedCheck(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	gitIn(t, v.o.work, "push", "--quiet", v.o.url, "task:refs/heads/"+prBranch)
	prMainTask(t, f, head)
	f.SetCheckRun(head, github.VerifyCheck, "success", githubtest.AppID)
	main := v.mainSHA()

	from, ran := len(f.Requests()), v.checksRan()
	v.pass()
	unnamed := githubtest.CheckRunState{SHA: head, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID}
	want := []githubtest.CheckRunState{unnamed, prMainCheck(head, main, "success")}
	if got := f.State().CheckRuns; !slices.Equal(got, want) || v.checksRan() != ran+1 || prMainReads(f, from, head) != 1 {
		t.Fatalf("the first pass left the check runs %+v, ran the commands %d times and read the head's checks %d times; want %+v, once and once",
			got, v.checksRan()-ran, prMainReads(f, from, head), want)
	}
	v.passes(2)
	if v.checksRan() != ran+1 || len(f.State().CheckRuns) != 2 {
		t.Errorf("the passes after ran the commands %d times in all, with the checks %+v; want once, and two", v.checksRan()-ran, f.State().CheckRuns)
	}
	v.prMoveQuiet("after the check", true)
	v.prMainRestart(head)
}

// A conflict while another task holds the worker, at verifying with a
// parent, so it gets no checks: the check is posted, but the move waits,
// with no write, and the passes that wait run nothing and read nothing
// for the task's head. main moving again drops it, and the head is checked
// on the newer main, posting a check naming it, and waits again. Then the
// other task closes, freeing the worker, as main moves once more: the next
// pass drops the waiting move, as main isn't at the commit it names, checks
// the head on the newest main, and makes the move keyed by it, once.
func TestPRMainWaitsForTheWorker(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.prHeadOpen()
	old := v.mainSHA()
	seen := prMainSeen(t, f)
	story := f.AddIssue(wholeRunOwner, "A story", "Its plan.", "type:story")
	f.Close(story)
	other := f.AddIssue(wholeRunOwner, "Refuse a stale quote", "Booking a stale quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.SetParent(other, story)
	f.AddLabel(other, wholeRunOwner, "stage:verifying")
	main := v.prMainConflict()

	v.pass()
	want := []githubtest.CheckRunState{prMoveVerified(head, old), prMainCheck(head, main, "failure")}
	if got := f.State().CheckRuns; !slices.Equal(got, want) || pickStage(f, 1) != "stage:pr-open" || len(prMainMoves(f)) != 0 {
		t.Fatalf("the check runs = %+v, the task at %s, its moves %q; want %+v, still at pr-open, and none", got, pickStage(f, 1), prMainMoves(f), want)
	}
	from, ran := len(f.Requests()), v.checksRan()
	v.pass()
	if got := prMainReads(f, from, head); got != 0 || v.checksRan() != ran || len(prMainMoves(f)) != 0 || len(f.State().CheckRuns) != 2 {
		t.Errorf("the pass that waits read the head's checks %d times, ran the commands %d times, with the moves %q and the checks %+v; want none of each but the two checks",
			got, v.checksRan()-ran, prMainMoves(f), f.State().CheckRuns)
	}

	newer := v.prMainCommit("later.txt", "main's later change\n")
	v.pass()
	want = append(want, prMainCheck(head, newer, "failure"))
	if got := f.State().CheckRuns; !slices.Equal(got, want) || len(prMainMoves(f)) != 0 {
		t.Fatalf("after main moved again, the check runs = %+v, the moves %q; want %+v, and none", got, prMainMoves(f), want)
	}

	newest := v.prMainCommit("latest.txt", "main's latest change\n")
	f.CloseBy(other, wholeRunOwner)
	v.pass()
	want = append(want, prMainCheck(head, newest, "failure"))
	prMainMoved(t, f, "stage:in-progress", prMainConflictText(head, newest), newest, seen)
	if got := f.State().CheckRuns; !slices.Equal(got, want) {
		t.Errorf("after the move, the check runs = %+v; want %+v", got, want)
	}
}

// Replay passes for the check and the move, whatever write a crash comes
// at (SPEC §5.2), on a conflict, whose writes are the check and the move,
// and on a merge that passes, whose write is the check: each pass is a
// fresh daemon's, with its own bare clone, and the digest and assignment
// off, so its writes are the merge's alone.
func TestPRMainReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		path   string // the file main's commit writes
		writes []string
	}{
		{"conflict", "quote.txt", []string{"POST /check-runs", "POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:pr-open"}},
		{"pass", "other.txt", []string{"POST /check-runs"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := newVerdictRig(t, newFake(t), 3)
			head := base.head()
			gitIn(t, base.o.work, "push", "--quiet", base.o.url, "task:refs/heads/"+prBranch)
			main := base.prMainCommit(tt.path, "main's own change\n")
			roots := map[*githubtest.Fake]string{}
			setup := func(tb testing.TB) *githubtest.Fake {
				f := newFake(tb)
				f.SetPermission(wholeRunOwner, "admin")
				f.SetGitRemote(base.o.url)
				prMainTask(tb, f, head)
				roots[f] = t.TempDir()
				return f
			}
			pass := func(f *githubtest.Fake) error {
				v := &verdictRig{rig: newRig(t, f, base.o, roots[f]), o: base.o, cfg: base.cfg, ran: base.ran}
				v.wire()
				v.d.Digest, v.d.Assign = false, false
				return v.d.Iterate(context.Background())
			}
			clean := setup(t)
			if err := pass(clean); err != nil {
				t.Fatal(err)
			}
			if got := writesOf(clean); !slices.Equal(got, tt.writes) {
				t.Errorf("the clean pass wrote %q; want %q", got, tt.writes)
			}
			conclusion := "success"
			if len(tt.writes) > 1 {
				conclusion = "failure"
			}
			if got, want := clean.State().CheckRuns, []githubtest.CheckRunState{prMainCheck(head, main, conclusion)}; !slices.Equal(got, want) {
				t.Errorf("the clean pass left the check runs %+v; want %+v", got, want)
			}
			githubtest.Replay(t, setup, pass)
		})
	}
}

// A head a person pushed that fails a command goes back to pr-open on
// person-pushed with issuedog/verify failure on it (verify_person.go).
// That failure stands: neither the passes after it nor the ones after
// main moves, nor a fresh daemon's passes, build its merge, run a command,
// post a check or send the task to in-progress, whatever main its check
// names. Its next push or the owner's review moves it on.
func TestPRMainPersonFailed(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	tip, _ := v.personPush(head, "broken.txt")
	v.pass()
	personBack(t, f, tip)
	checks, ran := f.State().CheckRuns, v.checksRan()
	v.passes(2)
	v.prMainCommit("other.txt", "main's own change\n")
	v.passes(2)
	v = v.restart()
	v.passes(2)
	if got := f.State().CheckRuns; !slices.Equal(got, checks) || v.checksRan() != ran || pickStage(f, 1) != "stage:pr-open" {
		t.Errorf("the check runs = %+v, the commands run %d times more, the task at %s; want %+v, none, and pr-open",
			got, v.checksRan()-ran, pickStage(f, 1), checks)
	}
	personNever(t, f)
	v.prMoveQuiet("after the restart", true)
}

// A head a person pushed that passes goes back to pr-open on person-pushed
// with issuedog/verify success on it, and is judged on a new main as any
// head is: once main moves, its merge runs the commands once, and a
// success naming the new main is posted on it.
func TestPRMainPersonPassed(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	tip, _ := v.personPush(head, "person.txt")
	v.pass()
	v.out(1, verifierPass(t, "Fine."))
	v.passes(2)
	personBack(t, f, tip)
	main := v.prMainCommit("other.txt", "main's own change\n")
	ran := v.checksRan()
	v.pass()
	if !slices.Contains(f.State().CheckRuns, prMainCheck(tip, main, "success")) || v.checksRan() != ran+1 {
		t.Errorf("the check runs = %+v, the commands run %d times; want a success on %s naming %s, and once",
			f.State().CheckRuns, v.checksRan()-ran, tip, main)
	}
}
