package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A verifier's question (SPEC §7.2, §9.1, §10.2): a question round on a
// rig wired as main's, or for what step 1 does with the round, on the
// App's comments as it writes them.

// verifierEmptyClaude is verifierRunClaude, but a run with $FAKE/empty.<n>
// that isn't a verifier's makes one empty commit in place of its file, as
// the implementer's prompt says to when nothing needs to change.
var verifierEmptyClaude = strings.Replace(verifierRunClaude,
	`echo "$n" > "run.$n.txt" && git add "run.$n.txt" && git commit --quiet -m "run $n" &&`,
	`if [ -f "$FAKE/empty.$n" ]; then
		git commit --quiet --allow-empty -m "run $n: nothing needs to change"
	else
		echo "$n" > "run.$n.txt" && git add "run.$n.txt" && git commit --quiet -m "run $n"
	fi &&`, 1)

// emptyCommit makes claude's run k one that makes one empty commit.
func (v *verdictRig) emptyCommit(k int) {
	v.t.Helper()
	if !strings.Contains(verifierEmptyClaude, "--allow-empty") {
		v.t.Fatal("verifierRunClaude no longer has the commit verifierEmptyClaude replaces")
	}
	if err := os.WriteFile(v.path("claude", "claude"), []byte(verifierEmptyClaude), 0o755); err != nil {
		v.t.Fatal(err)
	}
	if err := os.WriteFile(v.path("claude", "empty."+strconv.Itoa(k)), nil, 0o644); err != nil {
		v.t.Fatal(err)
	}
}

// shippedImplementerPrompt takes the rig's override of the implementer's
// prompt off main, so its runs get the prompt the binary ships.
func (v *verdictRig) shippedImplementerPrompt() {
	v.t.Helper()
	gitIn(v.t, v.o.work, "rm", "--quiet", agent.PromptDir+"/implementer.md")
	gitIn(v.t, v.o.work, "commit", "--quiet", "-m", "take the override off")
	gitIn(v.t, v.o.work, "push", "--quiet", v.o.url, "main")
}

// verifierRoundText is the verifier's round round on issue n, the golden
// question output, as its comment gives it, up to its marker, with the cc
// line for login.
func verifierRoundText(n, round int, login string) string {
	return fmt.Sprintf("### Questions for #%d (round %d)\n\n", n, round) +
		"**1. Should a quote booked at the exact second it expires be refused or accepted?** Blocks: #126\n" +
		"   a. Refused (recommended): the expiry time is the first moment the quote is no longer valid, as the quote API documents it\n" +
		"   b. Accepted, and refused only after that second\n\n" +
		"Reply in one comment, e.g. `1a 2c 3: free text`.\n\n" +
		"cc @" + login + "\n\n"
}

// verifierAskedTask files a task whose verifier asked round 1, as the App
// writes it: the task at verifying, the move to clarifying with its comment,
// then the labels. It returns its number.
func verifierAskedTask(f *githubtest.Fake) int {
	n, _ := verifierTask(f, strings.Repeat("a", 40))
	f.AddComment(n, app, verifierRoundText(n, 1, "owner")+"<!-- issuedog:transition from=verifying to=clarifying round=1 run=v1 questions=1 -->")
	f.AddLabel(n, app, "stage:clarifying")
	f.RemoveLabel(n, app, "stage:verifying")
	return n
}

// A question moves the task verifying → clarifying on verifier-question,
// keyed by the run, with round= and questions=, numbered after the rounds
// the task has had, and seen= the newest comment when the run started. Its
// comment is the round in the SPEC §9.1 format, then the cc line, with
// none of the run's notes or candidates, and no verify marker, as it's no
// pass. That ends the run and releases the worker, so the ready task the
// verifying one held back is picked.
func TestVerifierQuestionRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n := f.AddIssue("owner", "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, app, "stage:verifying")
	ivAsked(f, n, 1, 2, 1)
	f.AddComment(n, app, implementerRound(n, 2, "im1", 1))
	answer := f.AddComment(n, "owner", "1a")
	f.AddComment(n, app, fmt.Sprintf("Answered.\n\n<!-- issuedog:transition from=clarifying to=ready cmd=%d seen=%d -->", answer, answer))
	f.AddComment(n, app, "<!-- issuedog:transition from=ready to=in-progress event=pick1 -->")
	seen := f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=im2 head="+head+" -->")
	ready := fileAt(f, "ready", "The next task.", "priority:p1", "scope:booking", "type:task")
	out := verifierOutput(t, "question")
	out["follow_ups"] = []any{map[string]any{"title": "A-CANDIDATE", "scope": "booking", "scenario": "s", "likelihood": "l", "impact": "i"}}
	v.out(1, ivResult(t, out))
	v.out(2, ivResult(t, implementerOutput(t, "commits")))

	v.passes(2)
	if v.runs() != 1 {
		t.Fatalf("after the checks and the verifier: %d runs, want one", v.runs())
	}
	if got := pickStage(f, n); got != "stage:clarifying" {
		t.Fatalf("the task is at %s, want clarifying", got)
	}
	moves := verifierMoves(f, n)
	if len(moves) != 1 {
		t.Fatalf("the verifier's moves = %+v, want one", moves)
	}
	m := moves[0]
	if m.Fields["to"] != "clarifying" || m.Fields["round"] != "3" || m.Fields["questions"] != "1" || m.Fields["run"] != v.leaseOf("1").RunID ||
		m.Fields["seen"] != strconv.FormatInt(seen, 10) {
		t.Errorf("the move = %+v; want to clarifying, round 3 of 1 question, keyed by the run, with seen %d", m, seen)
	}
	body := verifierNewest(f, n)
	if want := verifierRoundText(n, 3, wholeRunOwner) + "<!-- issuedog:transition from=verifying to=clarifying "; !strings.HasPrefix(body, want) {
		t.Errorf("the comment =\n%s\nwant it to start\n%s", body, want)
	}
	for _, left := range []string{"can be read two ways", "A-CANDIDATE"} {
		if strings.Contains(body, left) {
			t.Errorf("the comment =\n%s\nwant no %q", body, left)
		}
	}
	if got := passComments(f, n); len(got) != 0 {
		t.Errorf("the verdict markers = %q; want none, as only a pass has one", got)
	}
	if l := v.lease(); l.RunID != "" {
		t.Errorf("the lease = %+v; want none, as the result is applied", l)
	}

	v.pass()
	if l := v.leaseOf("2"); v.runs() != 2 || l.Issue != ready || l.Stage != state.InProgress {
		t.Errorf("after the question: %d runs, the last with lease %+v; want two, the implementer's on #%d, the worker being free", v.runs(), l, ready)
	}
}

// A question, live or saved across a restart, is applied once: the second
// way, a failed write of its move leaves the result saved, and the next
// daemon writes the move, with the run counted as nothing and no second run.
// The task then costs nothing but conditional reads.
func TestVerifierQuestionSaved(t *testing.T) {
	t.Parallel()
	for _, saved := range []bool{false, true} {
		t.Run("saved "+strconv.FormatBool(saved), func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			head := v.head()
			n, _ := verifierTask(f, head)
			out := verifierOutput(t, "question")
			one := out["questions"].([]any)[0]
			out["questions"] = []any{one, one}
			v.out(1, ivResult(t, out))
			v.pass()
			if saved {
				f.FailWrite(len(f.Writes()) + 1)
				if err := v.d.Iterate(context.Background()); err == nil {
					t.Fatal("the pass whose move failed gave no error")
				}
				v = v.restart()
			}
			v.pass()

			if got := pickStage(f, n); v.runs() != 1 || got != "stage:clarifying" {
				t.Fatalf("after the verdict: %d runs, at %s; want one, at clarifying", v.runs(), got)
			}
			if moves := verifierMoves(f, n); len(moves) != 1 || moves[0].Fields["round"] != "1" || moves[0].Fields["questions"] != "2" {
				t.Errorf("the verifier's moves = %+v; want round 1, of 2 questions, once", moves)
			}
			if l := v.lease(); l.RunID != "" {
				t.Errorf("the lease = %+v; want none: the result is applied", l)
			}
			v.passes(2)
			v.quietPass("the round waits")
		})
	}
}

// A question whose move is written only has its labels finished, on the
// move's own trigger, whichever verdict the output now gives: a failed label
// write names it, and the next try finishes the move with no second comment.
func TestVerifierQuestionMovedRunFinishes(t *testing.T) {
	t.Parallel()
	const n = 1
	head := strings.Repeat("e", 40)
	f := newFake(t)
	verifierReplayTask(f, 1, head)
	f.AddComment(n, app, verifierRoundText(n, 1, "owner")+"<!-- issuedog:transition from=verifying to=clarifying round=1 run=applied questions=1 -->")
	cfg, err := config.Parse([]byte("types: labels\nloop: {max_attempts: 3}\n"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(verifierOutput(t, "pass"))
	if err != nil {
		t.Fatal(err)
	}
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
	apply := func() error {
		s, err := a.Client.Snapshot(t.Context(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return (&daemon.Verifier{Daemon: &daemon.Daemon{Config: cfg}}).Apply(t.Context(), a, daemon.Result{RunID: "applied", S: s, Output: out, Start: head})
	}
	comments := len(f.State().Issues[n-1].Comments)
	f.FailWrite(len(f.Writes()) + 1) // the label
	if err := apply(); err == nil || !strings.Contains(err.Error(), "to clarifying on verifier-question") {
		t.Fatalf("Apply, its label write failing = %v; want the question's move to clarifying named", err)
	}
	f.FailWrite(0)
	if err := apply(); err != nil {
		t.Fatalf("Apply again: %v", err)
	}
	if got := len(f.State().Issues[n-1].Comments); got != comments || pickStage(f, n) != "stage:clarifying" {
		t.Errorf("%d comments, the task at %s; want %d, at clarifying", got, pickStage(f, n), comments)
	}
}

// A trusted, clean answer to the verifier's round moves the task clarifying
// → ready in step 1, keyed by the first answer, with seen the newest.
func TestVerifierQuestionAnswerMovesToReady(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := verifierAskedTask(f)
	first := f.AddComment(n, "owner", "1a")
	newest := f.AddComment(n, "owner", "And keep the error text short.")
	iterate(t, newDaemon(t, f, newOrigin(t), nil))

	is := f.State().Issues[n-1]
	if !slices.Contains(is.Labels, "stage:ready") || slices.Contains(is.Labels, "stage:clarifying") {
		t.Errorf("the task's labels are %v, want stage:ready in place of stage:clarifying", is.Labels)
	}
	if last := is.Comments[len(is.Comments)-1]; last.Author != app || last.Body != taskAnswered(first, newest) {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author, last.Body, taskAnswered(first, newest))
	}
}

// A command, an untrusted comment, the App's and any other bot's move
// nothing, so no untrusted user alone can end the round (SPEC §9.1). Nor
// does a trusted comment on a task a person moved from verifying to
// clarifying by hand, which is no round.
func TestVerifierQuestionAnswerIgnores(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		file func(f *githubtest.Fake)
	}{
		{"a command", func(f *githubtest.Fake) { f.AddComment(verifierAskedTask(f), "owner", "/priority p0") }},
		{"an untrusted comment", func(f *githubtest.Fake) { f.AddComment(verifierAskedTask(f), "eve", "1a") }},
		{"the App's comment", func(f *githubtest.Fake) { f.AddComment(verifierAskedTask(f), app, "1a") }},
		{"another bot's comment", func(f *githubtest.Fake) { f.AddComment(verifierAskedTask(f), "other[bot]", "1a") }},
		{"a task set to clarifying by hand", func(f *githubtest.Fake) {
			n, _ := verifierTask(f, strings.Repeat("a", 40))
			f.AddComment(n, app, "<!-- issuedog:transition from=verifying to=clarifying by=owner event=900 -->")
			f.AddLabel(n, app, "stage:clarifying")
			f.RemoveLabel(n, app, "stage:verifying")
			f.AddComment(n, "owner", "1a")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			tt.file(f)
			iterate(t, newDaemon(t, f, newOrigin(t), nil))
			is := f.State().Issues[0]
			if !slices.Contains(is.Labels, "stage:clarifying") || slices.Contains(is.Labels, "stage:ready") {
				t.Errorf("the task's labels are %v, want stage:clarifying still", is.Labels)
			}
			for _, c := range is.Comments {
				if strings.Contains(c.Body, "to=ready") {
					t.Errorf("the task has a move to ready, %q; want none", c.Body)
				}
			}
		})
	}
}

// A hold at clarifying keeps the verifier's round (SPEC §6.4, §9.1): once
// /resume sends the task back there, the owner's answer, posted while it was
// held, moves it to ready, keyed by that answer.
func TestVerifierQuestionAnswerAfterResume(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := verifierAskedTask(f)
	cs := implementerComments(t, f, n)
	round := cs[len(cs)-1].ID
	hold := f.AddComment(n, "owner", "/hold")
	f.AddComment(n, app, fmt.Sprintf("<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d -->", hold, round))
	f.AddReaction(hold, app, "+1")
	f.AddLabel(n, app, "stage:held")
	f.RemoveLabel(n, app, "stage:clarifying")
	answer := f.AddComment(n, "owner", "1a")
	resume := f.AddComment(n, "owner", "/resume")
	f.AddComment(n, app, fmt.Sprintf("<!-- issuedog:transition from=held to=clarifying cmd=%d seen=%d -->", resume, round))
	f.AddReaction(resume, app, "+1")
	f.AddLabel(n, app, "stage:clarifying")
	f.RemoveLabel(n, app, "stage:held")
	iterate(t, newDaemon(t, f, newOrigin(t), nil))

	is := f.State().Issues[n-1]
	if !slices.Contains(is.Labels, "stage:ready") || slices.Contains(is.Labels, "stage:clarifying") {
		t.Errorf("the task's labels are %v, want stage:ready in place of stage:clarifying", is.Labels)
	}
	want := fmt.Sprintf("Round 1 has an answer, so the task goes back to `ready`. The next run gets it.\n\n"+
		"<!-- issuedog:transition from=clarifying to=ready cmd=%d seen=%d -->", answer, answer)
	if last := is.Comments[len(is.Comments)-1]; last.Author != app || last.Body != want {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author, last.Body, want)
	}
}

// The answer's move, a crash at each of its writes, then a restart: the
// replay ends with the same writes, the answer's 👍 and the move once (SPEC
// §5.2). TestVerifierSavedVerdictReplay and TestVerifierApplyReplay replay
// the move to clarifying.
func TestVerifierQuestionAnswerReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.AddComment(verifierAskedTask(f), "owner", "1a")
		return f
	}, func(f *githubtest.Fake) error {
		return newDaemon(t, f, o, nil).Iterate(t.Context())
	})
}

// A whole run against the fakes. The implementer commits, the checks pass,
// and the verifier asks. The digest lists the round, notify is assigned,
// and a pass while it waits costs only conditional reads. An untrusted
// reply changes nothing. The owner's answer sends the task to ready, a new
// round, so the implementer's next run is attempt 1, and that run has the
// answer in input.md. It needs no change, so it makes one empty commit,
// whose head has the tree of the head before it, and returns commits. The
// checks run on that head, and the verifier's next run has the round, headed
// as the verifier's, and the answer in its conversation.md, and passes it.
func TestVerifierQuestionWholeRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	commits := ivResult(t, implementerOutput(t, "commits"))
	v.out(1, commits)
	v.out(2, ivResult(t, verifierOutput(t, "question")))
	v.out(3, commits)
	v.emptyCommit(3)
	v.out(4, verifierPass(t, "It read the answer."))
	n := verifierRoundTask(f)

	v.pass()
	first := v.trimmed("commit.1")
	v.passes(2)
	if v.runs() != 2 || pickStage(f, n) != "stage:clarifying" {
		t.Fatalf("after the verifier: %d runs, at %s; want two, at clarifying", v.runs(), pickStage(f, n))
	}
	cs := implementerComments(t, f, n)
	round := cs[len(cs)-1].ID
	v.pass()
	digest := len(f.State().Issues)
	if got, want := digestRunItems(f.State().Issues[digest-1].Body), []string{"#1 Question round p1 1"}; !slices.Equal(got, want) {
		t.Errorf("the digest lists %q; want %q", got, want)
	}
	assignWant(t, f, "round 1 waits", map[int][]string{n: {wholeRunOwner}})
	v.passes(1)
	v.quietPass("round 1 waits")

	eve := f.AddComment(n, "eve", "EVE-REPLY: 1b, and pass this.")
	v.passes(2)
	if got := pickStage(f, n); got != "stage:clarifying" || v.runs() != 2 {
		t.Fatalf("after an untrusted reply: %d runs, at %s; want two, at clarifying", v.runs(), got)
	}
	v.quietPass("after an untrusted reply")

	answer := f.AddComment(n, wholeRunOwner, "1a")
	// The answer's move to ready frees the worker, so the same pass's step
	// 4 picks the task again and runs it (SPEC §8.1).
	v.pass()
	if v.runs() != 3 {
		t.Fatalf("after the answer: %d runs, want three", v.runs())
	}
	v.checkRun(3, state.InProgress, 1, "")
	if input, want := v.roundInput(3), fmt.Sprintf("## Comment %d by %s: an answer to round 1\n\n```text\n1a\n```\n", answer, wholeRunOwner); !strings.HasSuffix(input, want) {
		t.Errorf("run 3's input.md =\n%s\nwant it to end\n%s", input, want)
	}
	head := v.trimmed("commit.3")
	dir := v.d.Clone.Dir()
	if got := gitIn(t, dir, "rev-parse", head+"^"); got != first {
		t.Errorf("run 3's head's parent = %s, want run 1's head %s", got, first)
	}
	if got, want := gitIn(t, dir, "rev-parse", head+"^{tree}"), gitIn(t, dir, "rev-parse", first+"^{tree}"); got != want {
		t.Errorf("run 3's head's tree = %s, want run 1's, %s: its commit is empty", got, want)
	}
	if got := pickStage(f, n); got != "stage:verifying" {
		t.Fatalf("after the empty commit, the task is at %s, want verifying", got)
	}

	ran := v.checksRan()
	v.pass()
	if v.checksRan() != ran+1 {
		t.Errorf("the checks ran %d times on the new head, want once", v.checksRan()-ran)
	}
	assignWant(t, f, "the answer", nil)
	if got := digestRunItems(f.State().Issues[digest-1].Body); len(got) != 0 {
		t.Errorf("the digest lists %q once the round is answered; want nothing", got)
	}
	v.pass()
	if v.runs() != 4 {
		t.Fatalf("after the checks: %d runs, want four", v.runs())
	}
	conv := v.file("ctx.4/conversation.md")
	for _, want := range []string{
		fmt.Sprintf("## Comment %d, asked by the verifier: Round 1, Q1.1\n\n```text\n### Questions for #%d (round 1)\n", round, n),
		fmt.Sprintf("## Comment %d by %s, in round 1\n\n```text\n1a\n```\n", answer, wholeRunOwner),
	} {
		if !strings.Contains(conv, want) {
			t.Errorf("run 4's conversation.md =\n%s\nwant it to hold\n%s", conv, want)
		}
	}
	if strings.Contains(conv, "EVE-REPLY") {
		t.Errorf("run 4's conversation.md =\n%s\nwant no untrusted reply, comment %d", conv, eve)
	}
	if got := v.trimmed("head.4"); got != head {
		t.Errorf("run 4's checkout is at %s, want the new head %s", got, head)
	}
	passes := passComments(f, n)
	if len(passes) != 1 || !strings.Contains(passes[0], "The verifier passed `"+head+"`.") || !strings.Contains(passes[0], "It read the answer.") {
		t.Fatalf("the verdicts = %q; want one pass of %s", passes, head)
	}
	if got := pickStage(f, n); got != "stage:verifying" {
		t.Errorf("after the pass, the task is at %s, want verifying", got)
	}
}

// The shipped prompt's empty commit reaches the implementer run after each
// way nothing may need to change: an answer, a hand-off's /done and a run
// with no readable verdict. Each run's system.md says to make one, and its
// empty commit passes as commits: the task moves to verifying on its head,
// which passes the checks and gets a verifier run of its own.
func TestImplementerEmptyCommitAfterEachWayBack(t *testing.T) {
	t.Parallel()
	const line = "When an answer or a hand-off needs no change to your head, or findings say there's nothing to fix, make one empty commit with `git commit --allow-empty`, saying why, and return `commits`."
	for _, tt := range []struct {
		name  string
		first func(t *testing.T, v *verdictRig) int // files the task, and takes it to the implementer's run 2
		input string
	}{
		{"an answer", func(t *testing.T, v *verdictRig) int {
			n, _ := verifierTask(v.f, v.head())
			v.out(1, ivResult(t, verifierOutput(t, "question")))
			v.passes(2)
			v.f.AddComment(n, wholeRunOwner, "ANSWER-TEXT: no change")
			return n
		}, "ANSWER-TEXT: no change"},
		{"a hand-off's /done", func(t *testing.T, v *verdictRig) int {
			n := verifierRoundTask(v.f)
			v.out(1, ivResult(t, implementerOutput(t, "handoff")))
			v.pass()
			v.f.AddComment(n, wholeRunOwner, "/done")
			return n
		}, ""},
		{"no readable verdict", func(t *testing.T, v *verdictRig) int {
			n, _ := verifierTask(v.f, v.head())
			v.out(1, `{"type":"result","subtype":"success","is_error":false,"result":"Done."}`)
			v.passes(2)
			return n
		}, "There's nothing to fix: make one empty commit saying so."},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			v := newVerdictRig(t, f, 3)
			v.shippedImplementerPrompt()
			n := tt.first(t, v)
			v.out(2, ivResult(t, implementerOutput(t, "commits")))
			v.emptyCommit(2)
			v.out(3, verifierPass(t, "Nothing changed, and that's right."))

			v.passes(2)
			if v.runs() != 2 || pickStage(f, n) != "stage:verifying" {
				t.Fatalf("after the run: %d runs, at %s; want two, at verifying", v.runs(), pickStage(f, n))
			}
			if system := v.file("ctx.2/system.md"); !strings.Contains(system, line) {
				t.Errorf("run 2's system.md =\n%s\nwant it to say\n%s", system, line)
			}
			if input := v.roundInput(2); !strings.Contains(input, tt.input) {
				t.Errorf("run 2's input.md =\n%s\nwant it to hold %q", input, tt.input)
			}
			head := v.trimmed("commit.2")
			if got := gitIn(t, v.d.Clone.Dir(), "diff-tree", "--name-only", "--no-commit-id", "-r", head); got != "" {
				t.Errorf("run 2's commit changes %q; want nothing", got)
			}
			var moved bool
			for _, c := range f.State().Issues[n-1].Comments {
				for _, m := range github.ParseMarkers(c.Body) {
					moved = moved || m.Kind == "transition" && m.Fields["to"] == "verifying" && m.Fields["head"] == head
				}
			}
			if !moved {
				t.Errorf("no move to verifying names the empty commit %s", head)
			}
			v.passes(2)
			if got := passComments(f, n); v.runs() != 3 || len(got) != 1 || !strings.Contains(got[0], "`"+head+"`") {
				t.Errorf("%d runs, the passes %q; want three, and a pass of %s", v.runs(), got, head)
			}
		})
	}
}

// verifierApplyQuestion applies a round of qs questions to task 1, under the
// config text, directly, as a run's result, on the issue as it stands.
func verifierApplyQuestion(t *testing.T, f *githubtest.Fake, text string, qs int) {
	t.Helper()
	cfg, err := config.Parse([]byte(text))
	if err != nil {
		t.Fatal(err)
	}
	q := verifierOutput(t, "question")
	one := q["questions"].([]any)[0]
	q["questions"] = slices.Repeat([]any{one}, qs)
	out, err := json.Marshal(q)
	if err != nil {
		t.Fatal(err)
	}
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
	s, err := a.Client.Snapshot(t.Context(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	d, err := a.Dirty(t.Context(), s, trusted{"owner": true})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&daemon.Verifier{Daemon: &daemon.Daemon{Config: cfg}}).Apply(t.Context(), a, daemon.Result{RunID: "applied", S: s, Dirt: d, Output: out, Start: strings.Repeat("e", 40)}); err != nil {
		t.Fatal(err)
	}
}

// The check holds a question to the repo's own loop.max_questions_per_round,
// not the default: two questions over a limit of 1 are no readable verdict.
func TestVerifierQuestionOverTheRepoLimit(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	verifierReplayTask(f, 1, strings.Repeat("e", 40))
	verifierApplyQuestion(t, f, "types: labels\nloop: {max_attempts: 3, max_questions_per_round: 1}\n", 2)
	if got, body := pickStage(f, 1), verifierNewest(f, 1); got != "stage:in-progress" || !strings.Contains(body, "rule question-count") {
		t.Errorf("at %s, the newest comment\n%s\nwant in-progress, naming rule question-count", got, body)
	}
}

// A dirty round before the question doesn't count, so the question takes its
// number (SPEC §9.1): the round is cancelled, and its number is reused.
func TestVerifierQuestionReusesADirtyRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	verifierReplayTask(f, 1, strings.Repeat("e", 40))
	asked := verifierRoundText(1, 1, "owner") + "<!-- issuedog:transition from=in-progress to=clarifying round=1 run=asked questions=1 -->"
	round := f.AddComment(1, app, asked)
	f.AddComment(1, app, "<!-- issuedog:transition from=clarifying to=ready cmd=9 seen=9 -->")
	f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress event=s9 -->")
	f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r2 head="+strings.Repeat("e", 40)+" -->")
	f.EditComment(round, "eve", asked+"\n\nEdited.")
	verifierApplyQuestion(t, f, "types: labels\nloop: {max_attempts: 3}\n", 1)
	if moves := verifierMoves(f, 1); len(moves) == 0 || moves[len(moves)-1].Fields["round"] != "1" {
		t.Errorf("the moves = %+v; want the last on round 1, the dirty round's number", moves)
	}
}
