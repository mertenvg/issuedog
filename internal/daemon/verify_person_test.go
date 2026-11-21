package daemon_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A person's head at verifying (SPEC §3.4, §7.2, §8.3, §10.1, §10.2): a
// head someone pushed to an open task PR's branch, which step 2 moved the
// task to verifying on (pr_head_test.go), is checked and verified, on a
// verdictRig's task #1 whose PR #2 the PR step opened, against the fakes
// and a real origin. It goes back to pr-open on person-pushed, with the
// PR's body naming it and issuedog/verify on it, or to the owner when it
// fails rule 2, and never to in-progress, with no attempt counted and
// nothing pushed.

// personBrokenCommand is the command a personRig adds to verify.commands:
// it fails on a head with broken.txt.
const personBrokenCommand = "test ! -f broken.txt"

// personRig is a verdictRig whose verify.commands also fail on a head with
// broken.txt, with task #1 at pr-open on its verified head (prHeadOpen),
// which it returns.
func personRig(t *testing.T) (*githubtest.Fake, *verdictRig, string) {
	t.Helper()
	f := newFake(t)
	v := newVerdictRigWith(t, f, 3, "", `, "`+personBrokenCommand+`"`)
	return f, v, v.prHeadOpen()
}

// personPush commits file, a person's change, on top of head in v's
// origin and pushes it to the task's branch (prHeadPush), then runs the
// pass whose step 2 moves task 1 to verifying on it. It returns the new
// head and the ID of the comment holding that move.
func (v *verdictRig) personPush(head, file string) (string, int64) {
	v.t.Helper()
	if err := os.MkdirAll(filepath.Join(v.o.work, filepath.Dir(file)), 0o755); err != nil {
		v.t.Fatal(err)
	}
	tip := prHeadPush(v.t, v.o, head, file)
	v.pass()
	moves := personComments(v.t, v.f, "from=pr-open to=verifying event=n"+tip+" ")
	if len(moves) != 1 || pickStage(v.f, 1) != "stage:verifying" {
		v.t.Fatalf("after the push, the task at %s, its moves to verifying on %s %d; want verifying, by one", pickStage(v.f, 1), tip, len(moves))
	}
	return tip, moves[0].ID
}

// personComments are task 1's App comments whose body holds text, oldest
// first, as the App's client reads them, with their IDs.
func personComments(t *testing.T, f *githubtest.Fake, text string) []github.Comment {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	var found []github.Comment
	for _, c := range s.Comments {
		if c.Author.Login == app && strings.Contains(c.Body, text) {
			found = append(found, c)
		}
	}
	return found
}

// personBack is the one move of task 1 back to pr-open on its person's
// head, tip, and its marker's fields: the App's comment holding a move
// from verifying to pr-open with head=tip. It fails unless there's one,
// and the task is at pr-open.
func personBack(t *testing.T, f *githubtest.Fake, tip string) (github.Comment, map[string]string) {
	t.Helper()
	var moves []github.Comment
	for _, c := range personComments(t, f, "<!-- issuedog:transition from=verifying to=pr-open ") {
		if prMoveFields(t, c.Body)["head"] == tip {
			moves = append(moves, c)
		}
	}
	if len(moves) != 1 || pickStage(f, 1) != "stage:pr-open" {
		t.Fatalf("the task at %s, its moves back to pr-open on %s %+v; want pr-open, by one", pickStage(f, 1), tip, moves)
	}
	return moves[0], prMoveFields(t, moves[0].Body)
}

// personNever fails if task 1 was ever sent to in-progress, or any move
// out of verifying or pr-open carries attempt=: a person's head counts no
// attempt, and never goes to in-progress (SPEC §8.3). Only the
// implementer's move to verifying counts one here.
func personNever(t *testing.T, f *githubtest.Fake) {
	t.Helper()
	for _, c := range personComments(t, f, "<!-- issuedog:transition ") {
		fields := prMoveFields(t, c.Body)
		if fields["to"] == "in-progress" {
			t.Errorf("the task went to in-progress, in\n%s", c.Body)
		}
		if _, ok := fields["attempt"]; ok && (fields["from"] == "verifying" || fields["from"] == "pr-open") {
			t.Errorf("a move counts an attempt, in\n%s", c.Body)
		}
	}
}

// personCheck is a check run the App posted, as its request had it.
type personCheck struct {
	HeadSHA    string `json:"head_sha"`
	Conclusion string `json:"conclusion"`
	DetailsURL string `json:"details_url"`
	Output     struct {
		Title   string `json:"title"`
		Summary string `json:"summary"`
	} `json:"output"`
}

// personChecks are the check runs the App posted on sha, oldest first, as
// its requests had them.
func personChecks(t *testing.T, f *githubtest.Fake, sha string) []personCheck {
	t.Helper()
	var checks []personCheck
	for _, w := range f.Writes() {
		if w.Method != http.MethodPost || w.Path != "/check-runs" || w.Failed {
			continue
		}
		var c personCheck
		if err := json.Unmarshal([]byte(w.Body), &c); err != nil {
			t.Fatal(err)
		}
		if c.HeadSHA == sha {
			checks = append(checks, c)
		}
	}
	return checks
}

// personFailedBody is the body of task 1's PR when its person's head, tip,
// fails verify, with the fallback summary, as prTask's move keeps no
// pr-text (SPEC §11.5).
func personFailedBody(tip string) string {
	return "Closes #1\n\n## What changed\n\n" +
		"The implementer's summary isn't on #1: the task's move to `verifying` keeps no `pr-text` data, as a move made by hand doesn't.\n\n" +
		"## Results\n\n`" + tip + "`, a head the daemon didn't push, fails verify, so it isn't verified: its `issuedog/verify` check has the findings.\n\n" +
		"## Verifier notes\n\nNone, as the head didn't pass."
}

// personFailedIntro opens the comment of a move back to pr-open when the
// person's head, tip, fails verify, with PR pr.
func personFailedIntro(tip string, pr int) string {
	return "`" + tip + "`, a head the daemon didn't push to this task's branch, fails verify, so the task is back at `pr-open`, with nothing pushed and no attempt counted. " +
		"PR #" + strconv.Itoa(pr) + "'s `issuedog/verify` check there is a failure, which blocks its merge. Push a fix to the branch, or review the PR with comments to send the task back to the implementer.\n\n"
}

// personPassedBody is the body of task 1's PR when its person's head, tip,
// passed against main, in pass, with the notes "Fine.", on v's commands and
// personBrokenCommand, all passed, with the fallback summary.
func (v *verdictRig) personPassedBody(tip, main string, pass int64) string {
	return strings.Replace(v.prFallbackBody(tip, main, pass), "| Rules 2 to 7 |", "| `"+personBrokenCommand+"` | pass |\n| Rules 2 to 7 |", 1)
}

// A clean person's head: the pass after the move runs the checks on it,
// and the next the verifier, whose pass names no attempt. Then the next
// pass, the PR step's, pushes nothing: the PR's body names the head and
// the pass's notes, its title stays, issuedog/verify is a success on the
// head, as the PR step posts one, and the task is back at pr-open on
// person-pushed, keyed by the pass's run, with head=, pr= and seen=, the
// pass comment, and no attempt=. Then its branch is at its verified head,
// so nothing moves, and the passes after write nothing about it.
func TestPersonHeadClean(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	pr := f.State().PRs[0]
	tip, _ := v.personPush(head, "person.txt")
	v.pass()
	if v.checksRan() != 2 || v.runs() != 0 || pickStage(f, 1) != "stage:verifying" {
		t.Fatalf("the checks' pass ran the commands %d times in all, claude %d, with the task at %s; want twice, never, and verifying",
			v.checksRan(), v.runs(), pickStage(f, 1))
	}
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	passes := personComments(t, f, "<!-- issuedog:verify verdict=pass ")
	if v.runs() != 1 || v.leaseOf("1").Start != tip || len(passes) != 2 {
		t.Fatalf("after %d runs, the verifier started at %s, with the passes %+v; want one, at %s, and its pass", v.runs(), v.leaseOf("1").Start, passes, tip)
	}
	pass := passes[1]
	verify := github.ParseMarkers(pass.Body)[0]
	if _, ok := verify.Fields["attempt"]; ok || verify.Fields["head"] != tip {
		t.Errorf("the pass's marker has %v; want head=%s, and no attempt", verify.Fields, tip)
	}

	v.pass()
	move, fields := personBack(t, f, tip)
	want := map[string]string{"from": "verifying", "to": "pr-open", "run": verify.Fields["run"], "head": tip, "pr": strconv.Itoa(pr.Number),
		"seen": strconv.FormatInt(pass.ID, 10)}
	if !maps.Equal(fields, want) {
		t.Errorf("the move's marker has %v; want %v", fields, want)
	}
	text := "The verifier passed `" + tip + "`, a head the daemon didn't push to this task's branch, so the task is back at `pr-open`, with nothing pushed and no attempt counted. " +
		"PR #" + strconv.Itoa(pr.Number) + "'s body names the head, and its `issuedog/verify` check there is a success. It waits for your review and merge, and merging it closes this task.\n\n"
	if !strings.HasPrefix(move.Body, text) {
		t.Errorf("the move's comment =\n%s\nwant it to start\n%s", move.Body, text)
	}
	now := f.State().PRs[0]
	if want := v.personPassedBody(tip, v.mainSHA(), pass.ID); now.Body != want || now.Title != pr.Title || now.HeadSHA != tip {
		t.Errorf("the PR is %+v; want its title %q kept, at %s, and its body\n%s", now, pr.Title, tip, want)
	}
	checks := personChecks(t, f, tip)
	link := "https://github.com/o/r/issues/1#issuecomment-" + strconv.FormatInt(pass.ID, 10)
	if len(checks) != 1 || checks[0].Conclusion != "success" || checks[0].Output.Title != "issuedog verified this head" || checks[0].DetailsURL != link ||
		!strings.HasPrefix(checks[0].Output.Summary, "issuedog verified `"+tip+"` for #1: the checks against `main` at `"+v.mainSHA()+"` passed") {
		t.Errorf("the checks on %s = %+v; want one success, as the PR step posts it, linking %s", tip, checks, link)
	}
	if got := v.originTip(prBranch); got != tip {
		t.Errorf("the branch is at %s; want the person's head %s, with nothing pushed", got, tip)
	}
	personNever(t, f)

	v.passes(2)
	writes := len(f.Writes())
	v.pass()
	for _, w := range f.Writes()[writes:] {
		if prHeadAbout(githubtest.Request{Method: w.Method, Path: w.Path}, pr.Number) {
			t.Errorf("a pass after wrote %s %s; want nothing about the task or its PR", w.Method, w.Path)
		}
	}
	if pickStage(f, 1) != "stage:pr-open" || len(prHeadMoves(f)) != 1 {
		t.Errorf("the task at %s, its moves to verifying %q; want still pr-open, by the one", pickStage(f, 1), prHeadMoves(f))
	}
}

// A person's head that fails a command: the checks' pass moves it back to
// pr-open on person-pushed, keyed event=v by the move to verifying, as the
// checks' moves are, with head=, pr= and seen=, and no attempt=. Before
// it, the PR's body names the head as failing, and issuedog/verify is a
// failure there, its summary naming the command, escaped, and linking the
// issue. No verifier runs. Then a trusted review asks for changes, and the
// implementer's run is attempt 1, as it'd be with no person's head between
// (SPEC §8.3).
func TestPersonHeadFailsACommand(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	pr := f.State().PRs[0]
	tip, move := v.personPush(head, "broken.txt")
	v.pass()
	back, fields := personBack(t, f, tip)
	want := map[string]string{"from": "verifying", "to": "pr-open", "event": "v" + strconv.FormatInt(move, 10), "head": tip,
		"pr": strconv.Itoa(pr.Number), "seen": strconv.FormatInt(move, 10)}
	if !maps.Equal(fields, want) {
		t.Errorf("the move's marker has %v; want %v", fields, want)
	}
	if !strings.HasPrefix(back.Body, personFailedIntro(tip, pr.Number)+"Verify failed at `"+tip+"`, checked against `main` at `"+v.mainSHA()+"`.") ||
		!strings.Contains(back.Body, "### Rule 1: verify.commands") || !strings.Contains(back.Body, personBrokenCommand) {
		t.Errorf("the move's comment =\n%s\nwant it to name the head, and the failed command", back.Body)
	}
	if now := f.State().PRs[0]; now.Body != personFailedBody(tip) || now.Title != pr.Title {
		t.Errorf("the PR is %+v; want its title %q kept, and its body\n%s", now, pr.Title, personFailedBody(tip))
	}
	checks := personChecks(t, f, tip)
	if len(checks) != 1 || checks[0].Conclusion != "failure" || checks[0].Output.Title != "issuedog found this head fails verify" ||
		checks[0].DetailsURL != "https://github.com/o/r/issues/1" ||
		!strings.HasPrefix(checks[0].Output.Summary, "issuedog checked `"+tip+"` for #1, a head the daemon didn't push, and it fails verify.\n\nVerify failed at `"+tip+"`") ||
		!strings.Contains(checks[0].Output.Summary, personBrokenCommand) {
		t.Errorf("the checks on %s = %+v; want one failure, naming the command, linking the issue", tip, checks)
	}
	if v.runs() != 0 {
		t.Errorf("claude ran %d times; want never, as the checks failed", v.runs())
	}
	personNever(t, f)

	v.passes(2)
	f.ReviewPR(pr.Number, wholeRunOwner, github.ReviewChangesRequested, "Fix the broken file.", nil)
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.passes(2)
	if pickStage(f, 1) != "stage:verifying" || v.runs() != 1 {
		t.Fatalf("after the review, the task at %s, after %d runs; want verifying, by the implementer's one", pickStage(f, 1), v.runs())
	}
	if got := v.leaseOf("1").Attempt; got != 1 {
		t.Errorf("the implementer's run is attempt %d; want 1, as with no person's head between", got)
	}
	moves := personComments(t, f, "<!-- issuedog:transition from=in-progress to=verifying ")
	if got := prMoveFields(t, moves[len(moves)-1].Body)["attempt"]; got != "1" {
		t.Errorf("the implementer's move names attempt %q; want 1", got)
	}
}

// The verifier's request_changes on a person's head moves the task back
// to pr-open on person-pushed, keyed by its run, with head=, pr= and its
// run's seen=, and no attempt=. Before it, the
// PR's body names the head as failing, and issuedog/verify is a failure
// there, with the verifier's findings.
func TestPersonHeadVerifierFails(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	pr := f.State().PRs[0]
	tip, move := v.personPush(head, "person.txt")
	v.pass()
	v.out(1, ivResult(t, verifierOutput(t, "request_changes")))
	v.pass()
	back, fields := personBack(t, f, tip)
	want := map[string]string{"from": "verifying", "to": "pr-open", "run": v.leaseOf("1").RunID, "head": tip, "pr": strconv.Itoa(pr.Number),
		"seen": strconv.FormatInt(move, 10)}
	if !maps.Equal(fields, want) {
		t.Errorf("the move's marker has %v; want %v", fields, want)
	}
	criterion := "A quote past its expiry time can't be booked"
	if !strings.HasPrefix(back.Body, personFailedIntro(tip, pr.Number)+"The verifier asked for changes at `"+tip+"`.") || !strings.Contains(back.Body, criterion) {
		t.Errorf("the move's comment =\n%s\nwant it to name the head, and the findings", back.Body)
	}
	if now := f.State().PRs[0]; now.Body != personFailedBody(tip) || now.Title != pr.Title {
		t.Errorf("the PR is %+v; want its title %q kept, and its body\n%s", now, pr.Title, personFailedBody(tip))
	}
	checks := personChecks(t, f, tip)
	if len(checks) != 1 || checks[0].Conclusion != "failure" || !strings.Contains(checks[0].Output.Summary, criterion) {
		t.Errorf("the checks on %s = %+v; want one failure, with the findings", tip, checks)
	}
	if len(passComments(f, 1)) != 1 {
		t.Errorf("the passes = %q; want the old head's alone", passComments(f, 1))
	}
	personNever(t, f)
}

// A verifier's question on a person's head goes to clarifying, as any
// question does (SPEC §9.1), with no check and no PR write.
func TestPersonHeadQuestion(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	body := f.State().PRs[0].Body
	tip, _ := v.personPush(head, "person.txt")
	v.pass()
	v.out(1, ivResult(t, verifierOutput(t, "question")))
	v.pass()
	if pickStage(f, 1) != "stage:clarifying" || len(personChecks(t, f, tip)) != 0 || f.State().PRs[0].Body != body {
		t.Errorf("the task at %s, the checks on %s %+v, the PR's body changed %t; want clarifying, none, and unchanged",
			pickStage(f, 1), tip, personChecks(t, f, tip), f.State().PRs[0].Body != body)
	}
	personNever(t, f)
}

// A person's head that changes a protected path fails rule 2: no command
// runs, and the task is handed off at needs-human, keyed event=v by its
// move to verifying, with head= and seen=, its comment naming the head
// and the path, and ending in the cc line. Nothing is posted on the head,
// and the PR's body stays. The digest's hand-off item says why. Then the
// hand-off is ended by /done, or by a 👍 on it, which sends the task to
// ready, and its next run starts from main, as the head fails rule 2, on
// attempt 1.
func TestPersonHeadFailsRule2(t *testing.T) {
	t.Parallel()
	for _, end := range []string{"/done", "👍"} {
		t.Run(end, func(t *testing.T) {
			t.Parallel()
			f, v, head := personRig(t)
			body := f.State().PRs[0].Body
			ran := v.checksRan()
			tip, move := v.personPush(head, "secrets/key.txt")
			v.pass()
			handoffs := personComments(t, f, "<!-- issuedog:transition from=verifying to=needs-human ")
			if len(handoffs) != 1 || pickStage(f, 1) != "stage:needs-human" {
				t.Fatalf("the task at %s, its hand-offs %+v; want needs-human, by one", pickStage(f, 1), handoffs)
			}
			want := map[string]string{"from": "verifying", "to": "needs-human", "event": "v" + strconv.FormatInt(move, 10), "head": tip,
				"seen": strconv.FormatInt(move, 10)}
			if got := prMoveFields(t, handoffs[0].Body); !maps.Equal(got, want) {
				t.Errorf("the hand-off's marker has %v; want %v", got, want)
			}
			intro := "`" + tip + "`, a head the daemon didn't push to this task's branch, fails rule 2: it changes a protected path, which only a change on `main` may. " +
				"So no command or verifier ran on it, and the task doesn't go back to `pr-open`.\n\n### Rule 2: protected paths"
			if c := handoffs[0].Body; !strings.HasPrefix(c, intro) || !strings.Contains(c, "secrets/key.txt") || !strings.Contains(c, "cc @"+wholeRunOwner) {
				t.Errorf("the hand-off =\n%s\nwant it to start\n%s\nand name the path, and end in the cc line", c, intro)
			}
			if v.checksRan() != ran || v.runs() != 0 || len(personChecks(t, f, tip)) != 0 || f.State().PRs[0].Body != body {
				t.Errorf("the commands ran %d more times, claude %d, the checks on %s %+v, the PR's body changed %t; want none, none, none and unchanged",
					v.checksRan()-ran, v.runs(), tip, personChecks(t, f, tip), f.State().PRs[0].Body != body)
			}
			personNever(t, f)

			v.pass()
			why := "A hand-off waits for you: a head someone else pushed to the task's branch changes a protected path, so it fails rule 2, and the hand-off names the paths."
			if d := prMoveDigest(f); !strings.Contains(d, why) || !strings.Contains(d, "Goal: Decide what to do about the head someone pushed") {
				t.Errorf("the digest =\n%s\nwant the hand-off's item saying\n%s", d, why)
			}

			if end == "/done" {
				f.AddComment(1, wholeRunOwner, "/done")
			} else {
				f.AddReaction(handoffs[0].ID, wholeRunOwner, "+1")
			}
			v.out(1, ivResult(t, implementerOutput(t, "commits")))
			v.pass()
			if done := personComments(t, f, "<!-- issuedog:transition from=needs-human to=ready "); len(done) != 1 || pickStage(f, 1) == "stage:needs-human" {
				t.Fatalf("after %s, the task is at %s, its moves to ready %+v; want it moved on, by one", end, pickStage(f, 1), done)
			}
			v.pass()
			if l := v.leaseOf("1"); v.runs() != 1 || l.Start != v.mainSHA() || l.Attempt != 1 {
				t.Errorf("after %d runs, the implementer started at %s, attempt %d; want one, from main's %s, as the head fails rule 2, attempt 1",
					v.runs(), l.Start, l.Attempt, v.mainSHA())
			}
		})
	}
}

// A push during the checks of a person's head, after the pass's sync: the
// checks and the verifier judge the head the move named, the task goes
// back to pr-open on it without waiting for the branch, and nothing is
// pushed over the new tip. The next pass's step 2 finds the new tip, and
// moves the task to verifying on it.
func TestPersonHeadPushDuringTheChecks(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	tip, _ := v.personPush(head, "person.txt")
	var next string
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues" },
		func() { next = prHeadPush(t, v.o, tip, "later.txt") })
	v.pass()
	if next == "" || v.checksRan() != 2 {
		t.Fatalf("the checks' pass ran the commands %d times in all, with the push %q; want twice, and a push", v.checksRan(), next)
	}
	v.out(1, verifierPass(t, "Fine."))
	v.pass()
	if v.leaseOf("1").Start != tip {
		t.Fatalf("the verifier started at %s; want the head the move named, %s", v.leaseOf("1").Start, tip)
	}
	v.pass()
	personBack(t, f, tip)
	if got := personChecks(t, f, tip); len(got) != 1 || got[0].Conclusion != "success" {
		t.Errorf("the checks on %s = %+v; want one success", tip, got)
	}
	if got := v.originTip(prBranch); got != next {
		t.Errorf("the branch is at %s; want the newer push %s, with nothing pushed over it", got, next)
	}
	v.pass()
	if len(prHeadMoves(f)) != 2 || len(personComments(t, f, "event=n"+next+" ")) != 1 || pickStage(f, 1) != "stage:verifying" {
		t.Errorf("the task at %s, its moves to verifying %q; want verifying, by a second one, on %s", pickStage(f, 1), prHeadMoves(f), next)
	}
}

// A crash just after the failure's check, whose answer is lost, then a
// fresh daemon: its pass runs the checks again, finds the check on the
// head written, posts none, and makes the move, once.
func TestPersonHeadCrashAfterTheCheck(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	tip, _ := v.personPush(head, "broken.txt")
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/check-runs" },
		func() { f.FailWriteAfter(len(f.Writes()) + 1) })
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose check's answer was lost gave no error")
	}
	on := func() []githubtest.CheckRunState {
		return slices.DeleteFunc(f.State().CheckRuns, func(c githubtest.CheckRunState) bool { return c.SHA != tip })
	}
	failure := []githubtest.CheckRunState{{SHA: tip, Name: github.VerifyCheck, Conclusion: "failure", AppID: githubtest.AppID}}
	if got := on(); pickStage(f, 1) != "stage:verifying" || !slices.Equal(got, failure) {
		t.Fatalf("after the lost answer, the task at %s, the checks on %s %+v; want verifying, and the failure", pickStage(f, 1), tip, got)
	}
	v = v.restart()
	ran, writes := v.checksRan(), len(f.Writes())
	v.pass()
	personBack(t, f, tip)
	if got := on(); !slices.Equal(got, failure) || v.checksRan() != ran+1 {
		t.Errorf("after the restart, the checks on %s %+v, the commands ran %d more times; want the one failure, and once", tip, got, v.checksRan()-ran)
	}
	for _, w := range f.Writes()[writes:] {
		if strings.HasPrefix(w.Path, "/pulls") || w.Path == "/check-runs" {
			t.Errorf("after the restart, the pass wrote %s %s; want no PR write, as its body and its check stand", w.Method, w.Path)
		}
	}
	if got := f.State().PRs[0].Body; got != personFailedBody(tip) {
		t.Errorf("the PR's body =\n%s\nwant\n%s", got, personFailedBody(tip))
	}
}

// A crash between the new head's move and the task's head, then a hold
// and /resume before any pass at verifying: the move back from held
// names no head, but the hold is looked through, so the task's head
// follows the person's head, and the checks and the verifier judge it,
// and the task goes back to pr-open on it (SPEC §3.2, §6.4).
func TestPersonHeadCrashThenHold(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	tip := prHeadPush(t, v.o, head, "person.txt")
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/comments" },
		func() { f.FailWriteAfter(len(f.Writes()) + 1) })
	if err := v.d.Iterate(context.Background()); err == nil {
		t.Fatal("the pass whose answer was lost gave no error")
	}
	v = v.restart()
	f.AddComment(1, wholeRunOwner, "/hold")
	v.pass()
	if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.TasksRef+"1"); pickStage(f, 1) != "stage:held" || got != head {
		t.Fatalf("after /hold, the task at %s, its head %s; want held, and still %s", pickStage(f, 1), got, head)
	}
	f.AddComment(1, wholeRunOwner, "/resume")
	v.out(1, verifierPass(t, "Fine."))
	v.passes(4)
	if got := gitIn(t, v.d.Clone.Dir(), "rev-parse", repo.TasksRef+"1"); got != tip || v.leaseOf("1").Start != tip {
		t.Errorf("the task's head is %s, the verifier started at %s; want both the person's head %s", got, v.leaseOf("1").Start, tip)
	}
	personBack(t, f, tip)
	personNever(t, f)
}

// A person's head's pass whose follow-ups can't be filed, as its data
// doesn't read back, waits at verifying, as the PR step's would (SPEC
// §10.2): no check, no PR write and no move, and once its state error is
// posted, a pass makes no request but conditional GETs.
func TestPersonHeadPassWaitsForItsFollowUps(t *testing.T) {
	t.Parallel()
	f, v, head := personRig(t)
	body := f.State().PRs[0].Body
	tip, _ := v.personPush(head, "person.txt")
	f.AddComment(1, app, "The verifier passed `"+tip+"`.\n\nFine.\n\n<!-- issuedog:verify verdict=pass run=rp head="+tip+" -->\n"+
		verifyNotesLine(t, "Fine.")+"\n<!-- issuedog:verify-data v=9 data=x -->")
	v.passes(3)
	if pickStage(f, 1) != "stage:verifying" || len(personChecks(t, f, tip)) != 0 || f.State().PRs[0].Body != body || v.runs() != 0 {
		t.Fatalf("the task at %s, the checks on %s %+v, the PR's body changed %t, claude ran %d times; want verifying, none, unchanged, and never",
			pickStage(f, 1), tip, personChecks(t, f, tip), f.State().PRs[0].Body != body, v.runs())
	}
	v.quietPass("waiting for its follow-ups")
}

// A task closed, or set to another stage by hand, while the checks of its
// person's head run gets no write when they fail: the issue is read again
// first (SPEC §10.1).
func TestPersonHeadMovedDuringTheChecks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		move  func(f *githubtest.Fake)
		stage string
	}{
		{"closed", func(f *githubtest.Fake) { f.Close(1) }, "stage:verifying"},
		{"set by hand", func(f *githubtest.Fake) {
			f.RemoveLabel(1, wholeRunOwner, "stage:verifying")
			f.AddLabel(1, wholeRunOwner, "stage:ready")
		}, "stage:ready"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gate := t.TempDir()
			wait := "if [ -f '" + gate + "/hold' ]; then touch '" + gate + "/started'; while [ ! -f '" + gate + "/go' ]; do sleep 0.05; done; fi"
			f := newFake(t)
			v := newVerdictRigWith(t, f, 3, "", `, "`+personBrokenCommand+`", "`+strings.ReplaceAll(wait, `"`, `\"`)+`"`)
			head := v.prHeadOpen()
			body := f.State().PRs[0].Body
			tip, _ := v.personPush(head, "broken.txt")
			if err := os.WriteFile(filepath.Join(gate, "hold"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- v.d.Iterate(context.Background()) }()
			for deadline := time.Now().Add(time.Minute); ; time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(filepath.Join(gate, "started")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the checks never started")
				}
			}
			tt.move(f)
			if err := os.WriteFile(filepath.Join(gate, "go"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatalf("the checks' pass: %v", err)
			}
			if back := personComments(t, f, "to=pr-open head="+tip); len(back) != 0 || len(personChecks(t, f, tip)) != 0 || f.State().PRs[0].Body != body {
				t.Errorf("the moves back on %s %+v, its checks %+v, the PR's body changed %t; want none, none and unchanged",
					tip, back, personChecks(t, f, tip), f.State().PRs[0].Body != body)
			}
			if got := pickStage(f, 1); got != tt.stage {
				t.Errorf("the task is at %s; want %s", got, tt.stage)
			}
		})
	}
}

// A person's head the bare repo lacks, after a hold and /resume: the head
// judged is still the one the new head's move names, not the task's head,
// so the checks find it missing, and the task goes back to pr-open on it,
// with no check, as there's nothing to post one on, and no PR found from
// a branch that isn't there (SPEC §10.1).
func TestPersonHeadMissingThroughAHold(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	missing := strings.Repeat("ab", 20)
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, app, "stage:verifying")
	move := f.AddComment(n, app, "A new head.\n\n<!-- issuedog:transition from=pr-open to=verifying event=n"+missing+" head="+missing+" -->")
	hold := f.AddComment(n, wholeRunOwner, "/hold")
	f.AddComment(n, app, "Held.\n\n<!-- issuedog:transition from=verifying to=held cmd="+strconv.FormatInt(hold, 10)+" at=verifying -->")
	resume := f.AddComment(n, wholeRunOwner, "/resume")
	f.AddComment(n, app, "Resumed.\n\n<!-- issuedog:transition from=held to=verifying cmd="+strconv.FormatInt(resume, 10)+
		" seen="+strconv.FormatInt(move, 10)+" -->")
	v.pass()
	_, fields := personBack(t, f, missing)
	if _, ok := fields["pr"]; ok || len(f.State().CheckRuns) != 0 {
		t.Errorf("the move's marker has %v, the checks %+v; want no pr=, and none", fields, f.State().CheckRuns)
	}
	if back := personComments(t, f, "to=pr-open "); len(back) != 1 || !strings.Contains(back[0].Body, "The head "+missing+" isn't in the bare repo.") {
		t.Errorf("the moves back to pr-open = %+v; want one, saying the head isn't in the bare repo", back)
	}
}

// personReplayFake is a fake whose task #1 is at verifying on a person's
// head, tip, on o's task branch, after its PR #2 was opened at old: the
// implementer's move, the PR, the PR step's move and step 2's move on the
// new head, then more, more comments of the App's.
func personReplayFake(tb testing.TB, o *origin, old, tip string, more ...string) *githubtest.Fake {
	tb.Helper()
	f := newFake(tb)
	f.SetPermission(wholeRunOwner, "admin")
	f.SetGitRemote(o.url)
	n, _ := verifierTask(f, old)
	pr, err := f.Client(tb).CreatePullRequest(context.Background(), "o", "r",
		github.NewPullRequest{Title: prItemTitle, Head: prBranch, Base: "main", Body: "Closes #1\n\nWhat changed."})
	if err != nil {
		tb.Fatal(err)
	}
	if pr.Number != 2 {
		tb.Fatalf("the PR is #%d, want #2", pr.Number)
	}
	f.AddComment(n, app, prMoveText(pr.Number, old)+"<!-- issuedog:transition from=verifying to=pr-open run=rv head="+old+" pr=2 -->")
	f.AddComment(n, app, prHeadText(old, tip)+"<!-- issuedog:transition from=pr-open to=verifying event=n"+tip+" head="+tip+" -->")
	for _, c := range more {
		f.AddComment(n, app, c)
	}
	f.ForgetWrites()
	return f
}

// Replay passes for a person's head's writes, whatever write a crash
// comes at (SPEC §5.2): the PR's body, the check and the move back to
// pr-open, on a pass and on a failed command, and the hand-off on rule 2.
// Each pass is a fresh daemon on the fake's own bare clone, so it runs the
// checks again first, and its third iteration reads what it wrote.
func TestPersonHeadReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, file, stage, conclusion string
		pass                          bool
	}{
		{"pass", "person.txt", "stage:pr-open", "success", true},
		{"a failed command", "broken.txt", "stage:pr-open", "failure", false},
		{"rule 2", "secrets/key.txt", "stage:needs-human", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := newVerdictRigWith(t, newFake(t), 3, "", `, "`+personBrokenCommand+`"`)
			old := base.head()
			gitIn(t, base.o.work, "push", "--quiet", base.o.url, old+":refs/heads/"+prBranch)
			if err := os.MkdirAll(filepath.Join(base.o.work, filepath.Dir(tt.file)), 0o755); err != nil {
				t.Fatal(err)
			}
			tip := prHeadPush(t, base.o, old, tt.file)
			var more []string
			if tt.pass {
				more = append(more, prPassComment(t, "rp", tip, "Fine."))
			}
			roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
			githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
				f := personReplayFake(tb, base.o, old, tip, more...)
				roots[f] = t.TempDir()
				return f
			}, func(f *githubtest.Fake) error {
				g := newRig(t, f, base.o, roots[f])
				if clones[f] == nil {
					clones[f] = g.d.Clone
				}
				g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
				v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
				v.wire()
				for range 3 {
					if err := v.d.Iterate(context.Background()); err != nil {
						return err
					}
				}
				return nil
			})
			for f := range roots {
				var want []githubtest.CheckRunState
				if tt.conclusion != "" {
					want = []githubtest.CheckRunState{{SHA: tip, Name: github.VerifyCheck, Conclusion: tt.conclusion, AppID: githubtest.AppID}}
				}
				if got := f.State().CheckRuns; !slices.Equal(got, want) {
					t.Errorf("after the replay, the check runs = %+v; want %+v", got, want)
				}
				to := strings.TrimPrefix(tt.stage, "stage:")
				moves := slices.DeleteFunc(prMoveComments(f, to), func(c string) bool { return !strings.Contains(c, " head="+tip+" ") })
				if len(moves) != 1 || pickStage(f, 1) != tt.stage {
					t.Errorf("after the replay, the moves to %s on %s = %q, at %s; want one, at %s", to, tip, moves, pickStage(f, 1), tt.stage)
				}
			}
		})
	}
}

// personMoveID is the ID of the comment on f's task #1 that holds step 2's
// move to verifying on tip.
func personMoveID(tb testing.TB, f *githubtest.Fake, tip string) int64 {
	tb.Helper()
	s, err := f.Client(tb).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		tb.Fatal(err)
	}
	for _, c := range s.Comments {
		if c.Author.Login == app && strings.Contains(c.Body, "event=n"+tip+" ") {
			return c.ID
		}
	}
	tb.Fatalf("no move to verifying on %s", tip)
	return 0
}

// Replay passes for the verifier's request_changes, and for no readable
// verdict, on a person's head, whatever write a crash comes at (SPEC
// §5.2): a saved result, applied by a fresh daemon on each pass, writes
// the PR's body, the failure on the head and the move back to pr-open,
// keyed by the saved run, once each, through the verdict's own checks
// (verdictRun, runMoved), with no attempt counted.
func TestPersonHeadVerifierReplay(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		out  func(t *testing.T) string
	}{
		{"request_changes", func(t *testing.T) string { return ivResult(t, verifierOutput(t, "request_changes")) }},
		{"no readable verdict", func(*testing.T) string {
			return `{"type":"result","subtype":"success","is_error":false,"result":"Done."}`
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			base := newVerdictRigWith(t, newFake(t), 3, "", `, "`+personBrokenCommand+`"`)
			old := base.head()
			gitIn(t, base.o.work, "push", "--quiet", base.o.url, old+":refs/heads/"+prBranch)
			tip := prHeadPush(t, base.o, old, "person.txt")
			out := tt.out(t)
			roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
			githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
				f := personReplayFake(tb, base.o, old, tip)
				roots[f] = t.TempDir()
				verifierSaved(tb, roots[f], tip, out, personMoveID(tb, f, tip))
				return f
			}, func(f *githubtest.Fake) error {
				g := newRig(t, f, base.o, roots[f])
				if clones[f] == nil {
					clones[f] = g.d.Clone
				}
				g.d.Clone, g.r.Workspace.Bare = clones[f], clones[f].Dir()
				v := &verdictRig{rig: g, o: base.o, cfg: base.cfg, ran: base.ran}
				v.wire()
				for range 3 {
					if err := v.d.Iterate(context.Background()); err != nil {
						return err
					}
				}
				return nil
			})
			for f := range roots {
				want := []githubtest.CheckRunState{{SHA: tip, Name: github.VerifyCheck, Conclusion: "failure", AppID: githubtest.AppID}}
				if got := f.State().CheckRuns; !slices.Equal(got, want) {
					t.Errorf("after the replay, the check runs = %+v; want %+v", got, want)
				}
				moves := slices.DeleteFunc(prMoveComments(f, "pr-open"), func(c string) bool { return !strings.Contains(c, " head="+tip+" ") })
				if len(moves) != 1 || pickStage(f, 1) != "stage:pr-open" {
					t.Fatalf("after the replay, the moves to pr-open on %s = %q, at %s; want one, at pr-open", tip, moves, pickStage(f, 1))
				}
				fields := prMoveFields(t, moves[0])
				if _, ok := fields["attempt"]; ok || fields["run"] != "saved" || fields["head"] != tip {
					t.Errorf("after the replay, the move's marker has %v; want run=saved, head=%s, and no attempt", fields, tip)
				}
				if got := f.State().PRs[0].Body; got != personFailedBody(tip) {
					t.Errorf("after the replay, the PR's body =\n%s\nwant\n%s", got, personFailedBody(tip))
				}
			}
		})
	}
}
