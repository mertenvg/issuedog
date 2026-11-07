package daemon

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// verifierRoundMarker is a verifier's round marker, with n questions.
func verifierRoundMarker(round, n int) string {
	return fmt.Sprintf("<!-- issuedog:transition from=verifying to=clarifying run=v%d round=%d questions=%d -->", round, round, n)
}

// A verifier's round is a run's move from verifying to clarifying, and no
// other move: not a person's, keyed by event=, and not a move on from
// verifying. It's a task's round, as the implementer's is, and both are
// rounds, as the interviewer's and the planner's are.
func TestVerifierRoundMove(t *testing.T) {
	t.Parallel()
	move := func(fields ...string) github.Marker {
		m := github.Marker{Kind: "transition", Fields: map[string]string{}}
		for i := 0; i < len(fields); i += 2 {
			m.Fields[fields[i]] = fields[i+1]
		}
		return m
	}
	for _, tt := range []struct {
		name                     string
		m                        github.Marker
		verifier, task, anyRound bool
	}{
		{"the verifier's round", move("from", "verifying", "to", "clarifying", "run", "v1"), true, true, true},
		{"the implementer's round", move("from", "in-progress", "to", "clarifying", "run", "i1"), false, true, true},
		{"the interviewer's round", move("from", "clarifying", "to", "clarifying", "run", "c1"), false, false, true},
		{"the planner's round", move("from", "planning", "to", "planning", "run", "p1"), false, false, true},
		{"a person's move", move("from", "verifying", "to", "clarifying", "event", "900", "by", "owner"), false, false, false},
		{"the verifier's findings", move("from", "verifying", "to", "in-progress", "run", "v1"), false, false, false},
		{"a verify marker", github.Marker{Kind: verifyKind, Fields: map[string]string{"from": "verifying", "to": "clarifying", "run": "v1"}}, false, false, false},
	} {
		if got := verifierRoundMove(tt.m); got != tt.verifier {
			t.Errorf("%s: verifierRoundMove = %v, want %v", tt.name, got, tt.verifier)
		}
		if got := taskRoundMove(tt.m); got != tt.task {
			t.Errorf("%s: taskRoundMove = %v, want %v", tt.name, got, tt.task)
		}
		if got := roundMove(tt.m); got != tt.anyRound {
			t.Errorf("%s: roundMove = %v, want %v", tt.name, got, tt.anyRound)
		}
	}
}

// A verifier's round counts with every stage's (SPEC §9.1): the next round
// comes after it, and each names the stage that asked it.
func TestAllRoundsNamesTheAsker(t *testing.T) {
	t.Parallel()
	s := roundsSnap(
		"<!-- issuedog:transition from=clarifying to=clarifying run=a seen=9 round=1 questions=2 -->",
		"<!-- issuedog:transition from=planning to=planning run=b seen=10 round=2 questions=1 -->",
		"<!-- issuedog:transition from=in-progress to=clarifying run=c seen=11 round=3 questions=1 -->",
		"<!-- issuedog:transition from=verifying to=clarifying run=d seen=12 round=4 questions=3 -->",
	)
	got := allRounds(s, "issuedog", func(int64) bool { return false })
	want := []interviewerRound{
		{Number: 1, Questions: 2, Comment: 10, Bound: 9, Asker: interviewerAgent},
		{Number: 2, Questions: 1, Comment: 11, Bound: 10, Asker: plannerAgent},
		{Number: 3, Questions: 1, Comment: 12, Bound: 11, Asker: implementerAgent},
		{Number: 4, Questions: 3, Comment: 13, Bound: 12, Asker: verifierAgent},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("allRounds = %+v, want %+v", got, want)
	}
	if got := nextRound(got); got != 5 {
		t.Errorf("after the verifier's round 4, the next round is %d, want 5", got)
	}
}

// A round's heading in the verifier's conversation.md names the stage that
// asked it, not issuedog, as its wording is that agent's.
func TestVerifierAppLabel(t *testing.T) {
	t.Parallel()
	rounds := []interviewerRound{{Number: 2, Questions: 1, Comment: 5, Asker: implementerAgent}, {Number: 3, Questions: 2, Comment: 6, Asker: plannerAgent}}
	for _, tt := range []struct {
		id   int64
		want string
	}{
		{5, "comment 5, asked by the implementer: Round 2, Q2.1"},
		{6, "comment 6, asked by the planner: Round 3, Q3.1–3.2"},
		{7, "comment 7"},
	} {
		if got := verifierAppLabel(github.Comment{ID: tt.id}, rounds); got != tt.want {
			t.Errorf("verifierAppLabel(%d) = %q, want %q", tt.id, got, tt.want)
		}
	}
}

// The digest's question item finds a verifier's round as it finds the
// implementer's, with its key and rank (SPEC §9.4), through a hold the task
// went back from by /resume too, and no item once it has an answer.
func TestWaitFindVerifierRound(t *testing.T) {
	t.Parallel()
	asked := renderQuestions(interviewOutput{Questions: []interviewQuestion{{Text: "Where?", Blocks: []int{3},
		Recommended: interviewOption{Text: "Server", Why: "one place"}, Others: []string{"Client"}}}}, 3, 2, []string{"owner"})
	round := join(asked, verifierRoundMarker(2, 1))
	for _, tt := range []struct {
		name     string
		comments []string
	}{
		{"the round", []string{round}},
		{"after a hold and /resume", []string{round,
			"<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=900 seen=301 -->",
			"<!-- issuedog:transition from=held to=clarifying cmd=901 seen=301 -->"}},
	} {
		s := waitSnap(3, []string{"priority:p1"}, tt.comments...)
		got := waitFindOne(t, waitIn(s, Dirt{}))
		if got.Kind != waitQuestion || got.Key != "q-3-2" || got.Priority != 1 || got.Comment != 301 {
			t.Errorf("%s: the item = %+v; want round 2's question, on comment 301", tt.name, got)
		}
		if want := "Round 2 asks 1 question, and waits for an answer."; got.Lines[0].Text != want {
			t.Errorf("%s: its first line = %q, want %q", tt.name, got.Lines[0].Text, want)
		}
	}
	s := waitSnap(3, []string{"priority:p1"}, round)
	items, err := waitFind(waitIn(s, Dirt{}, github.Comment{ID: 900, Body: "1a"}))
	if err != nil || len(items) != 0 {
		t.Errorf("with an answer, waitFind = %+v, %v; want nothing", items, err)
	}
}

// The next run's answers are the round's whichever stage asked it, so the
// verifier's round reaches implementerAnswers too, and an implementer run's
// move since ends them.
func TestImplementerAnswersVerifierRound(t *testing.T) {
	t.Parallel()
	app := contextTestApp
	owner := github.Author{Login: "owner", Type: "User"}
	comments := []github.Comment{
		{ID: 10, Author: app, Body: "Round 1.\n\n" + verifierRoundMarker(1, 1)},
		{ID: 11, Author: owner, Body: "1a"},
		{ID: 12, Author: app, Body: "Answered.\n\n<!-- issuedog:transition from=clarifying to=ready cmd=11 seen=11 -->"},
		{ID: 13, Author: app, Body: "<!-- issuedog:transition from=ready to=in-progress event=pick -->"},
	}
	read := func(cs []github.Comment) []github.Comment {
		s := &github.Snapshot{Number: 1, Comments: cs}
		got, err := implementerAnswers(t.Context(), s, Dirt{}, contextTestTC(), state.Derive(s, "issuedog", Dirt{}.Has))
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := read(comments); len(got) != 1 || got[0].ID != 11 {
		t.Errorf("the next run's answers = %+v; want comment 11", got)
	}
	used := append(comments[:len(comments):len(comments)], github.Comment{ID: 14, Author: app, Body: "<!-- issuedog:transition from=in-progress to=verifying run=i2 head=abc -->"})
	if got := read(used); len(got) != 0 {
		t.Errorf("after the next run's move, the answers = %+v; want none", got)
	}
}

// A no-verdict comment below the cap says what's true of it: it opens saying
// no verdict came, not that changes were asked for, ends with the task back
// at in-progress and nothing to fix, with no word of a fix for the
// implementer, and escapes the whole. At the cap, it's the owner's hand-off,
// which tells no one to make an empty commit.
func TestRenderVerdictNoVerdict(t *testing.T) {
	t.Parallel()
	out := noVerdict(fmt.Errorf("check: %w", &ruleError{Rule: ruleOutcome, Detail: "pass has no findings"}))
	got := renderVerdict("abc", 2, 3, out)
	want := "The verifier gave no verdict on attempt 2 of 3, at `abc`.\n\n" +
		"The verifier's run gave no readable verdict, so this counts as `request_changes`: rule outcome: pass has no findings. Nothing is known to be wrong with the code.\n\n" +
		"So the task is back at in-progress. There's nothing to fix: make one empty commit saying so."
	if got != want {
		t.Errorf("renderVerdict =\n%s\nwant\n%s", got, want)
	}
	if got := renderVerdict("abc", 0, 3, out); !strings.HasPrefix(got, "The verifier gave no verdict at `abc`.\n\n") {
		t.Errorf("with no attempt, renderVerdict =\n%s\nwant it to name none", got)
	}
	handoff := changesComment(out, verifierFacts{Head: "abc", Attempt: 3, Limit: 3, Issue: 7})
	if !strings.Contains(handoff, "The verifier gave no verdict on attempt 3 of 3, at `abc`.") {
		t.Errorf("the hand-off =\n%s\nwant it to say no verdict came", handoff)
	}
	if strings.Contains(handoff, "empty commit") {
		t.Errorf("the hand-off =\n%s\nwant no word of an empty commit: it's the owner's", handoff)
	}
}

// A question's comment is the round alone, in the format of SPEC §9.1, for
// the round the facts name, ending in the cc line.
func TestQuestionComment(t *testing.T) {
	t.Parallel()
	out := verifierOutput{Verdict: verdictQuestion, Notes: "MY-NOTES", Questions: []interviewQuestion{{Text: "Where?", Blocks: []int{7},
		Recommended: interviewOption{Text: "Server", Why: "one place"}, Others: []string{"Client"}}}}
	got := questionComment(out, verifierFacts{Issue: 7, Round: 3, Notify: []string{"owner"}})
	want := "### Questions for #7 (round 3)\n\n**1. Where?** Blocks: #7\n   a. Server (recommended): one place\n   b. Client\n\n" +
		"Reply in one comment, e.g. `1a 2c 3: free text`.\n\ncc @owner"
	if got != want {
		t.Errorf("questionComment =\n%s\nwant\n%s", got, want)
	}
}

// taskAnswerText says "the next run", whichever stage asked, as the round's
// answer reaches the implementer's run and the verifier's after it.
func TestTaskAnswerTextNamesNoStage(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		round string
		n     int
		want  string
	}{
		{"2", 1, "Round 2 has an answer, so the task goes back to `ready`. The next run gets it."},
		{"2", 3, "Round 2 has answers, so the task goes back to `ready`. The next run gets them."},
		{"", 1, "The question round has an answer, so the task goes back to `ready`. The next run gets it."},
	} {
		if got := taskAnswerText(tt.round, tt.n); got != tt.want {
			t.Errorf("taskAnswerText(%q, %s) = %q, want %q", tt.round, strconv.Itoa(tt.n), got, tt.want)
		}
	}
}
