package daemon_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Question rounds at planning (#253): the planner's rounds share one
// numbering with the interviewer's. The planner isn't built yet (#258,
// #259), so its rounds here are hand-built markers on the fake.

// planAsked posts the planner's round r, of q questions, on issue n as
// its run would, having seen comment seen, and returns its ID.
func planAsked(f *githubtest.Fake, n, r, q int, seen int64) int64 {
	return f.AddComment(n, app, fmt.Sprintf("### Questions for #%d (round %d)\n\nPLANNED-%d\n\n"+
		"<!-- issuedog:transition from=planning to=planning run=pl%d seen=%d round=%d questions=%d -->", n, r, r, r, seen, r, q))
}

// planRoundsIssue files the owner's epic with interviewer rounds 1 (2
// questions) and 2 (1 question), each answered by owner, a spec approved
// to planning, the planner's round 3 (2 questions), answered too, and an
// override back to clarifying, as a stage set by hand. It returns the
// issue's number, round 3's comment and its answer.
func planRoundsIssue(f *githubtest.Fake) (n int, round3, answer3 int64) {
	n, entry := ivFiled(f, "epic", false, "from=intake to=clarifying event=5")
	ivAsked(f, n, 1, 2, entry)
	a1 := f.AddComment(n, "owner", "1a 2b")
	ivAsked(f, n, 2, 1, a1)
	a2 := f.AddComment(n, "owner", "1a")
	f.AddComment(n, app, fmt.Sprintf("<!-- issuedog:transition from=clarifying to=spec-review run=sp seen=%d rev=1 body=abcd1234 -->", a2))
	f.AddComment(n, app, "<!-- issuedog:transition from=spec-review to=planning cmd=r7 rev=1 body=abcd1234 -->")
	round3 = planAsked(f, n, 3, 2, a2)
	answer3 = f.AddComment(n, "owner", "1a 2a")
	f.AddComment(n, app, "<!-- issuedog:transition from=planning to=clarifying by=owner event=77 -->")
	return n, round3, answer3
}

// TestInterviewerAfterPlannerRound: on an issue with interviewer rounds 1
// and 2 and planner round 3, the interviewer's next round is 4, its
// preamble says so and that round 3's questions are the planner's, its
// conversation labels round 3 and the answer to it, and its spec decides
// only its own questions: one naming Q3.1 fails, and one leaving out the
// planner's questions passes.
func TestInterviewerAfterPlannerRound(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n, round3, answer3 := planRoundsIssue(f)
	d := ivDaemon(t, f, newOrigin(t))

	files := ivContext(t, d, n)
	for _, want := range []string{
		"A question round now is round 4, and every question asked so far needs a decision in a spec.",
		"The planner asked round 3 (Q3.1–3.2): those questions aren't yours, so no decision in a spec names them.",
		"Questions so far: round 1 (Q1.1–1.2), round 2 (Q2.1).",
	} {
		if !strings.Contains(files["system.md"], want) {
			t.Errorf("system.md has no %q:\n%s", want, files["system.md"])
		}
	}
	for _, want := range []string{
		fmt.Sprintf("## Comment %d by issuedog: Round 3, Q3.1–3.2", round3),
		fmt.Sprintf("## Comment %d by owner, in round 3", answer3),
	} {
		if !strings.Contains(files["conversation.md"], want) {
			t.Errorf("conversation.md has no %q:\n%s", want, files["conversation.md"])
		}
	}

	in := ivInput(t, d, n)
	check := func(out map[string]any) error {
		return (&daemon.Interviewer{Daemon: d}).Check(context.Background(), daemon.Result{RunID: "run1", S: in.S, Dirt: in.Dirt, Output: ivJSON(t, out)})
	}
	decision := func(q string, round int) map[string]any {
		return map[string]any{"question": q, "answer": "a", "summary": "Yes.", "by": "owner", "round": round}
	}
	own := []map[string]any{decision("Q1.1", 1), decision("Q1.2", 1), decision("Q2.1", 2)}
	if err := check(ivSpec(own...)); err != nil {
		t.Errorf("Check of a spec deciding the interviewer's questions alone = %v, want nil", err)
	}
	if err := check(ivSpec(append(own, decision("Q3.1", 3))...)); err == nil || !strings.Contains(err.Error(), "rule decision-question") {
		t.Errorf("Check of a spec deciding the planner's Q3.1 = %v, want rule decision-question", err)
	}
	if err := check(ivSpec(own[:2]...)); err == nil || !strings.Contains(err.Error(), "rule decision-coverage") || !strings.Contains(err.Error(), "Q2.1 asked") {
		t.Errorf("Check of a spec missing Q2.1 = %v, want rule decision-coverage naming Q2.1 alone", err)
	}
	if err := check(ivRound(1, "")); err != nil {
		t.Errorf("Check of a round = %v, want nil", err)
	}

	if err := (&daemon.Interviewer{Daemon: d}).Apply(context.Background(), d.Step.Applier,
		daemon.Result{RunID: "run1", S: in.S, Dirt: in.Dirt, Output: ivJSON(t, ivRound(1, ""))}); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(strings.TrimSuffix(ivRoundText(4, 1), "\n\ncc @owner"), "#1 ", fmt.Sprintf("#%d ", n), 1) +
		"\n\n<!-- issuedog:transition from=clarifying to=clarifying round=4 run=run1 questions=1 -->"
	if got := newestComment(f, n); got != want {
		t.Errorf("the interviewer's round after planner round 3 =\n%s\nwant\n%s", got, want)
	}
}
