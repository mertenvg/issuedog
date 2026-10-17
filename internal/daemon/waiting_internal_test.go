package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

const waitSlug = "issuedog"

var (
	waitApp   = github.Author{Login: waitSlug + "[bot]", Type: "Bot"}
	waitOwner = github.Author{Login: "owner", Type: "User"}
	waitT0    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
)

// waitSnap is open issue n, an epic opened n hours after waitT0, with
// labels, and the App's comments, whose IDs are n*100+1, n*100+2, ...
func waitSnap(n int, labels []string, comments ...string) *github.Snapshot {
	s := &github.Snapshot{Number: n, Title: fmt.Sprintf("Issue %d @eve <b>", n), Type: "epic", Author: waitOwner,
		Body: "The spec.", Labels: labels, CreatedAt: waitT0.Add(time.Duration(n) * time.Hour)}
	for i, c := range comments {
		s.Comments = append(s.Comments, github.Comment{ID: int64(n*100 + i + 1), Author: waitApp, Body: c})
	}
	return s
}

// waitIn is waitFind's input for s in repo o/r.
func waitIn(s *github.Snapshot, d Dirt, inputs ...github.Comment) waitInput {
	return waitInput{Owner: "o", Repo: "r", AppSlug: waitSlug, S: s, Dirt: d, Inputs: inputs}
}

// waitFindOne returns the one item waitFind finds on in.
func waitFindOne(t *testing.T, in waitInput) waitItem {
	t.Helper()
	items, err := waitFind(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(items), items)
	}
	return items[0]
}

// waitRoundMarker is the interviewer's round move, round round with n
// questions.
func waitRoundMarker(round, n int) string {
	return fmt.Sprintf("<!-- issuedog:transition from=clarifying to=clarifying run=r%d round=%d questions=%d -->", round, round, n)
}

// waitRevision is a transition to spec-review that posts revision rev of
// body, with its revision marker.
func waitRevision(rev int, body string) string {
	h := state.BodyHash(body)
	return fmt.Sprintf("**Revision %d**\n\n<!-- issuedog:transition from=clarifying to=spec-review run=s%d rev=%d body=%s -->\n"+
		"<!-- issuedog:revision rev=%d body=%s -->", rev, rev, rev, h, rev, h)
}

// waitFlagMarker is triage's flag marker, recommending recommend.
func waitFlagMarker(recommend string) string {
	return "<!-- issuedog:triage verdict=ask recommend=" + recommend + " run=t1 -->"
}

// waitTexts is the Text of each line.
func waitTexts(lines []waitLine) []string {
	var texts []string
	for _, l := range lines {
		texts = append(texts, l.Text)
	}
	return texts
}

// Each kind is found with its key, its link, its issue's title and
// priority, and the lines that say what's decided and how to answer.
func TestWaitFindKinds(t *testing.T) {
	t.Parallel()
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Where?", Blocks: []int{3}, Recommended: interviewOption{Text: "Server", Why: "one place"}, Others: []string{"Client"}},
	}}
	flag := triageOutput{Verdict: triageHandle, Reason: "it's a bug", Summary: "Crash on save"}
	cases := []struct {
		name  string
		s     *github.Snapshot
		kind  waitKind
		key   string
		link  string
		prio  int
		lines []waitLine
	}{
		{"state error", waitSnap(1, []string{"Priority:P0"}, "I can't move this issue.\nIt stays.\n\n<!-- issuedog:error kind=transition key=50 -->"),
			waitError, "err-1-transition-50", "https://github.com/o/r/issues/1#issuecomment-101", 0, []waitLine{
				{Text: "State error `transition`, keyed `50`."},
				{Text: "I can't move this issue.", Copied: true}, {Text: "It stays.", Copied: true},
				{Text: "React 👍 to the error comment to acknowledge it."}}},
		{"approval", waitSnap(2, nil, waitRevision(1, "The spec.")),
			waitApproval, "rev-2-1", "https://github.com/o/r/issues/2#issuecomment-201", 2, []waitLine{
				{Text: "Revision 1 of the spec, `" + state.BodyHash("The spec.") + "`, waits for approval."},
				{Text: "React 👍 to the revision comment, or post `/approve`, to approve it."},
				{Text: "Post `/revise <notes>`, or any other comment, to send it back."}}},
		{"question", waitSnap(3, []string{"priority:p1"}, join(renderQuestions(round, 3, 1, []string{"owner"}), waitRoundMarker(1, 1))),
			waitQuestion, "q-3-1", "https://github.com/o/r/issues/3#issuecomment-301", 1, []waitLine{
				{Text: "Round 1 asks 1 question, and waits for an answer."},
				{Text: "Where?", Copied: true, Question: 1},
				{Text: "Server (recommended): one place", Copied: true, Option: "a", Recommended: true},
				{Text: "Client", Copied: true, Option: "b"},
				{Text: "Reply in one comment, e.g. `1a 2c 3: free text`."}}},
		{"flag", waitSnap(4, []string{"triage:ask"}, "Triage ran.", join(triageFlagText(flag, 4, []string{"owner"}), waitFlagMarker("handle"))),
			waitFlag, "flag-4-402", "https://github.com/o/r/issues/4#issuecomment-402", 2, []waitLine{
				{Text: "Triage flagged this issue: should issuedog handle it?"},
				{Text: "Crash on save", Copied: true, Question: 1},
				{Text: "Handle it (recommended): it's a bug", Copied: true, Option: "a", Recommended: true},
				{Text: "Leave it", Copied: true, Option: "b"},
				{Text: "React 👍 to the flag to take the recommendation, or post `/track` or `/ignore`."}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := waitFindOne(t, waitIn(c.s, Dirt{}))
			if got.Issue != c.s.Number || got.Kind != c.kind || got.Key != c.key || got.Link != c.link || got.Priority != c.prio {
				t.Errorf("got #%d %s %s %s p%d, want #%d %s %s %s p%d", got.Issue, got.Kind, got.Key, got.Link, got.Priority,
					c.s.Number, c.kind, c.key, c.link, c.prio)
			}
			if got.Title != c.s.Title || !got.Created.Equal(c.s.CreatedAt) || got.Unblocks != 0 {
				t.Errorf("got title %q, created %v, unblocks %d; want the raw title %q, %v and 0", got.Title, got.Created, got.Unblocks, c.s.Title, c.s.CreatedAt)
			}
			if !slices.Equal(got.Lines, c.lines) {
				t.Errorf("lines:\n got %+v\nwant %+v", got.Lines, c.lines)
			}
		})
	}
}

// An open state error and a question on one issue are both found, and an
// error that has ended is not.
func TestWaitFindErrorsAndQuestion(t *testing.T) {
	t.Parallel()
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Q", Blocks: []int{5}, Recommended: interviewOption{Text: "A", Why: "w"}, Others: []string{"B"}},
	}}
	s := waitSnap(5, nil,
		"Gone.\n\n<!-- issuedog:error kind=transition key=40 -->",
		"<!-- issuedog:transition from=intake to=clarifying cmd=9 -->",
		"Open.\n\n<!-- issuedog:error kind=labels key=0 -->",
		"Acked.\n\n<!-- issuedog:error kind=disagreement key=8 -->",
		"<!-- issuedog:error kind=disagreement key=8 ack=r1 -->",
		join(renderQuestions(round, 5, 1, nil), waitRoundMarker(1, 1)))
	items, err := waitFind(waitIn(s, Dirt{}))
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	if want := []string{"err-5-labels-0", "q-5-1"}; !slices.Equal(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}
}

// These wait on nobody: an answered round, an issue at planning, a typed
// bug waiting for phase 3, a closed issue, plan-review, a flag answered, a
// flag on an issue with a stage label or past intake, a round whose
// comment is dirty, a round move with no round or questions, and another
// move that carries them.
func TestWaitFindNothing(t *testing.T) {
	t.Parallel()
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Q", Blocks: []int{6}, Recommended: interviewOption{Text: "A", Why: "w"}, Others: []string{"B"}},
	}}
	roundComment := join(renderQuestions(round, 6, 1, nil), waitRoundMarker(1, 1))
	answer := github.Comment{ID: 999, Author: waitOwner, Body: "1a"}
	typed := waitSnap(6, nil, "Type: bug, because it is.\n\n<!-- issuedog:transition from=clarifying to=clarifying run=r1 -->")
	typed.Type = "bug"
	closed := waitSnap(6, nil, "Err.\n\n<!-- issuedog:error kind=transition key=50 -->")
	closed.Closed = true
	closedReview := waitSnap(6, nil, waitRevision(1, "The spec."))
	closedReview.Closed = true
	// A flag, then an override to planning whose stage label isn't on yet.
	flagged := waitSnap(6, nil, join(triageFlagText(triageOutput{Verdict: triageHandle}, 6, nil), waitFlagMarker("handle")),
		"<!-- issuedog:transition from=intake to=planning by=owner event=5 -->")
	cases := []struct {
		name   string
		s      *github.Snapshot
		dirt   Dirt
		inputs []github.Comment
	}{
		{"answered round", waitSnap(6, nil, roundComment), Dirt{}, []github.Comment{answer}},
		{"planning", waitSnap(6, nil, "<!-- issuedog:transition from=spec-review to=planning cmd=5 rev=1 body=abcd1234 -->"), Dirt{}, nil},
		{"typed bug", typed, Dirt{}, nil},
		{"closed", closed, Dirt{}, nil},
		{"closed at spec-review", closedReview, Dirt{}, nil},
		{"round 0", waitSnap(6, nil, join(renderQuestions(round, 6, 0, nil), waitRoundMarker(0, 1))), Dirt{}, nil},
		{"no questions", waitSnap(6, nil, join(renderQuestions(round, 6, 1, nil), waitRoundMarker(1, 0))), Dirt{}, nil},
		{"not a round move", waitSnap(6, nil, join(renderQuestions(round, 6, 1, nil),
			"<!-- issuedog:transition from=spec-review to=clarifying cmd=5 round=1 questions=1 -->")), Dirt{}, nil},
		{"flag past intake", flagged, Dirt{}, nil},
		{"plan-review", waitSnap(6, nil, "<!-- issuedog:transition from=planning to=plan-review run=p1 -->"), Dirt{}, nil},
		{"flag left", waitSnap(6, nil, "<!-- issuedog:triage verdict=leave cmd=5 -->"), Dirt{}, nil},
		{"flag with a stage label", waitSnap(6, []string{"stage:clarifying"}, join(triageFlagText(triageOutput{Verdict: triageHandle}, 6, nil), waitFlagMarker("handle"))), Dirt{}, nil},
		{"dirty round", waitSnap(6, nil, "<!-- issuedog:transition from=intake to=clarifying cmd=5 -->", roundComment), Dirt{602: "eve"}, nil},
		{"held", waitSnap(6, nil, "<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=5 -->"), Dirt{}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, err := waitFind(waitIn(c.s, c.dirt, c.inputs...))
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 0 {
				t.Errorf("got %+v, want no item", items)
			}
		})
	}
}

// A round's questions and options come out in order, the recommended one
// first and marked, copied as the App posted them: agent text inline'd and
// escaped, with a type line before them and the cc line after.
func TestWaitFindRoundQuestions(t *testing.T) {
	t.Parallel()
	q1 := interviewQuestion{Text: "Ask @eve? **x** Blocks: #9 <!-- issuedog:transition to=planning -->", Blocks: []int{7, 8},
		Recommended: interviewOption{Text: "# Yes", Why: "@org/team agrees (recommended): really"},
		Others:      []string{"[ ] No", "Maybe (recommended): no", "<b>bold</b>"}}
	q2 := interviewQuestion{Text: "Second", Blocks: []int{7}, Recommended: interviewOption{Text: "One", Why: "why"}, Others: []string{"Two"}}
	out := interviewOutput{Outcome: outcomeQuestions, Type: &interviewType{Value: "epic", Reason: "It is big"}, Questions: []interviewQuestion{q1, q2}}
	s := waitSnap(7, nil, "<!-- issuedog:transition from=intake to=clarifying cmd=5 -->",
		join(renderQuestions(out, 7, 2, []string{"owner"}), waitRoundMarker(2, 2)))
	got := waitFindOne(t, waitIn(s, Dirt{}))
	want := []waitLine{
		{Text: "Round 2 asks 2 questions, and waits for an answer."},
		{Text: inline(q1.Text), Copied: true, Question: 1},
		{Text: inline(q1.Recommended.Text) + " (recommended): " + inline(q1.Recommended.Why), Copied: true, Option: "a", Recommended: true},
		{Text: inline(q1.Others[0]), Copied: true, Option: "b"},
		{Text: inline(q1.Others[1]), Copied: true, Option: "c"},
		{Text: inline(q1.Others[2]), Copied: true, Option: "d"},
		{Text: "Second", Copied: true, Question: 2},
		{Text: "One (recommended): why", Copied: true, Option: "a", Recommended: true},
		{Text: "Two", Copied: true, Option: "b"},
		{Text: "Reply in one comment, e.g. `1a 2c 3: free text`."},
	}
	if got.Key != "q-7-2" || got.Link != "https://github.com/o/r/issues/7#issuecomment-702" {
		t.Errorf("got %s %s, want q-7-2 on comment 702", got.Key, got.Link)
	}
	if !slices.Equal(got.Lines, want) {
		t.Errorf("lines:\n got %q\nwant %q", waitTexts(got.Lines), waitTexts(want))
		t.Errorf("got %+v", got.Lines)
	}
	for _, l := range got.Lines {
		if strings.Contains(l.Text, "<!--") || strings.Contains(l.Text, "@eve") {
			t.Errorf("line %q has a marker or a mention", l.Text)
		}
	}
}

// A flag recommending leave lists "Leave it" first.
func TestWaitFindFlagLeave(t *testing.T) {
	t.Parallel()
	out := triageOutput{Verdict: triageLeave, Reason: "@eve asked <b>", Summary: "A question"}
	s := waitSnap(8, nil, join(triageFlagText(out, 8, nil), waitFlagMarker("leave")))
	got := waitFindOne(t, waitIn(s, Dirt{}))
	want := []string{"Triage flagged this issue: should issuedog handle it?", "A question",
		"Leave it (recommended): " + inline(out.Reason), "Handle it",
		"React 👍 to the flag to take the recommendation, or post `/track` or `/ignore`."}
	if !slices.Equal(waitTexts(got.Lines), want) {
		t.Errorf("lines = %q, want %q", waitTexts(got.Lines), want)
	}
}

// A round or flag whose comment can't be read back still gives its item,
// with one line saying so in place of the questions.
func TestWaitFindUnreadable(t *testing.T) {
	t.Parallel()
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Q", Blocks: []int{9}, Recommended: interviewOption{Text: "A", Why: "w"}, Others: []string{"B"}},
	}}
	const roundGone = "I couldn't read them back from the round comment, so see it there."
	cases := []struct {
		name, comment, want string
	}{
		{"count", join(renderQuestions(round, 9, 1, nil), waitRoundMarker(1, 2)), roundGone},
		{"round number", join(renderQuestions(round, 9, 3, nil), waitRoundMarker(1, 1)), roundGone},
		{"flag", "Something else.\n\n" + waitFlagMarker("handle"), "I couldn't read the question back from the flag, so see it there."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := waitFindOne(t, waitIn(waitSnap(9, nil, c.comment), Dirt{}))
			if len(got.Lines) != 3 || got.Lines[1] != (waitLine{Text: c.want}) {
				t.Errorf("lines = %+v, want the second to be %q", got.Lines, c.want)
			}
		})
	}
}

// waitQuestions takes only the exact format, and anything else gives the
// fallback line.
func TestWaitQuestions(t *testing.T) {
	t.Parallel()
	const h = "### H"
	ok := "**1. Q** Blocks: #1\n   a. A (recommended): w\n   b. B"
	cases := []struct {
		name, body string
		n          int
		want       int // lines, or 0 for the fallback
	}{
		{"ok", h + "\n\n" + ok + "\n\nReply.", 1, 3},
		{"CRLF", strings.ReplaceAll(h+"\n\n"+ok+"\n", "\n", "\r\n"), 1, 3},
		{"two", h + "\n" + ok + "\n\n**2. R** Blocks: #1, #2\n   a. C (recommended): w\n   b. D\n   c. E\n   d. F", 2, 8},
		{"no heading", ok, 1, 0},
		{"another heading", "### I\n" + ok, 1, 0},
		{"too few", h + "\n" + ok, 2, 0},
		{"too many", h + "\n" + ok + "\n**2. R** Blocks: #1\n   a. C (recommended): w\n   b. D", 1, 0},
		{"numbering", h + "\n**2. Q** Blocks: #1\n   a. A (recommended): w\n   b. B", 1, 0},
		{"one option", h + "\n**1. Q** Blocks: #1\n   a. A (recommended): w", 1, 0},
		{"one option then a question", h + "\n**1. Q** Blocks: #1\n   a. A (recommended): w\n**2. R** Blocks: #1\n   a. C (recommended): w\n   b. D", 2, 0},
		{"lettering", h + "\n**1. Q** Blocks: #1\n   a. A (recommended): w\n   c. B", 1, 0},
		{"not recommended", h + "\n**1. Q** Blocks: #1\n   a. A\n   b. B (recommended): w", 1, 0},
		{"option first", h + "\n   a. A (recommended): w\n" + ok, 1, 0},
		{"no blocks", h + "\n**1. Q**\n   a. A (recommended): w\n   b. B", 1, 0},
		{"text ends it", h + "\n" + ok + "\nMore.\n**2. R** Blocks: #1\n   a. C (recommended): w\n   b. D", 1, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := waitQuestions(c.body, h, c.n, "fallback")
			switch {
			case c.want == 0 && !slices.Equal(got, []waitLine{{Text: "fallback"}}):
				t.Errorf("got %+v, want the fallback", got)
			case c.want > 0 && (len(got) != c.want || got[0].Question != 1 || !got[0].Copied):
				t.Errorf("got %+v, want %d copied lines from question 1", got, c.want)
			}
		})
	}
}

// An approval that can't be approved yet says why, without the login of
// whoever made the body dirty, and doesn't say how to approve it.
func TestWaitFindApprovalBlocked(t *testing.T) {
	t.Parallel()
	hidden := waitSnap(10, nil, waitRevision(1, "The spec."), "Edited by someone.")
	untyped := waitSnap(10, nil, waitRevision(1, "The spec."))
	untyped.Type = ""
	unknown := waitSnap(10, nil, waitRevision(1, "The spec."))
	unknown.Type = "feature"
	// A type change that stands, to a type that can't be at spec-review or
	// one the pipeline doesn't know, holds a 👍 back (#156's verify).
	retyped := waitSnap(10, nil, waitRevision(1, "The spec."))
	retyped.Type, retyped.TypeEvent = "task", 70
	retypedUnknown := waitSnap(10, nil, waitRevision(1, "The spec."))
	retypedUnknown.Type, retypedUnknown.TypeEvent = "feature", 71
	held := "its type doesn't fit `spec-review`, so a 👍 is held back until the type changes (see the type error)"
	cases := []struct {
		name string
		s    *github.Snapshot
		dirt Dirt
		key  string
		why  []string
	}{
		{"dirty body", waitSnap(10, nil, waitRevision(1, "The spec.")), Dirt{state.BodyID: "eve"}, "rev-10-1",
			[]string{"the issue description has an edit that wasn't approved (see the note on it)"}},
		{"hidden", hidden, Dirt{1002: "eve"}, "rev-10-1",
			[]string{"a comment of mine after revision 1 was edited without approval or deleted, so it may hide a newer revision"}},
		{"hash", waitSnap(10, nil, waitRevision(1, "An older spec.")), Dirt{}, "rev-10-1",
			[]string{"the issue description changed after revision 1, so it no longer matches `" + state.BodyHash("An older spec.") + "`"}},
		{"untyped", untyped, Dirt{}, "rev-10-1", []string{"an issue of its type doesn't go on to `planning` on approval"}},
		{"unknown type", unknown, Dirt{}, "rev-10-1", []string{"I can't tell what kind of issue this is from its type"}},
		{"retyped", retyped, Dirt{}, "rev-10-1", []string{held}},
		{"retyped to an unknown type", retypedUnknown, Dirt{}, "rev-10-1", []string{held}},
		{"no revision, dirty", waitSnap(10, nil, "No revision.\n\n<!-- issuedog:transition from=clarifying to=spec-review run=s1 -->"),
			Dirt{state.BodyID: "eve"}, "rev-10-0", []string{"the issue description has an edit that wasn't approved (see the note on it)"}},
		{"dirty and hash", waitSnap(10, nil, waitRevision(1, "An older spec.")), Dirt{state.BodyID: "eve"}, "rev-10-1",
			[]string{"the issue description has an edit that wasn't approved (see the note on it)",
				"the issue description changed after revision 1, so it no longer matches `" + state.BodyHash("An older spec.") + "`"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := waitFindOne(t, waitIn(c.s, c.dirt))
			if got.Kind != waitApproval || got.Key != c.key || got.Link != "https://github.com/o/r/issues/10#issuecomment-1001" {
				t.Errorf("got %s %s %s, want an approval %s on comment 1001", got.Kind, got.Key, got.Link, c.key)
			}
			var why []string
			for _, l := range got.Lines {
				if w, ok := strings.CutPrefix(l.Text, "It can't be approved yet: "); ok {
					why = append(why, strings.TrimSuffix(w, "."))
				}
				if strings.Contains(l.Text, "eve") || strings.Contains(l.Text, "/approve") || l.Copied {
					t.Errorf("line %+v names the login, says how to approve, or is copied", l)
				}
			}
			if !slices.Equal(why, c.why) {
				t.Errorf("why = %q, want %q", why, c.why)
			}
			if last := got.Lines[len(got.Lines)-1].Text; last != "Post `/revise <notes>`, or any other comment, to send it back." {
				t.Errorf("last line = %q, want how to send it back", last)
			}
		})
	}
}

// A clean spec-review issue with no revision yet gives the approval of no
// revision, linked to the transition, which waits for the next pass's.
func TestWaitFindApprovalNoRevision(t *testing.T) {
	t.Parallel()
	s := waitSnap(11, nil, "<!-- issuedog:transition from=clarifying to=spec-review run=s1 -->")
	got := waitFindOne(t, waitIn(s, Dirt{}))
	want := []string{"The spec waits for review, with no revision to approve yet.", "Post `/revise <notes>`, or any other comment, to send it back."}
	if got.Key != "rev-11-0" || got.Link != "https://github.com/o/r/issues/11#issuecomment-1101" || !slices.Equal(waitTexts(got.Lines), want) {
		t.Errorf("got %s %s %q, want rev-11-0 on comment 1101, %q", got.Key, got.Link, waitTexts(got.Lines), want)
	}
}

// A revision posted on its own, after the move to spec-review, is the one
// to approve, and its comment is the link.
func TestWaitFindApprovalLatest(t *testing.T) {
	t.Parallel()
	s := waitSnap(13, nil, waitRevision(1, "An older spec."),
		"**Revision 2**\n\n<!-- issuedog:revision rev=2 body="+state.BodyHash("The spec.")+" seen=5 -->")
	got := waitFindOne(t, waitIn(s, Dirt{}))
	if got.Key != "rev-13-2" || got.Link != "https://github.com/o/r/issues/13#issuecomment-1302" || len(got.Lines) != 3 {
		t.Errorf("got %s %s %q, want rev-13-2 on comment 1302, approvable", got.Key, got.Link, waitTexts(got.Lines))
	}
}

// An error in the body links to the issue itself, has none of the body's
// text, and says a 👍 can't acknowledge it (#155's verify).
func TestWaitFindErrorInBody(t *testing.T) {
	t.Parallel()
	s := waitSnap(12, nil)
	s.Author, s.Body = waitApp, "Body text.\n\n<!-- issuedog:error kind=type key=70 -->"
	// Its cause stands: a type the pipeline doesn't know, set by event 70.
	s.Type, s.TypeEvent = "feature", 70
	got := waitFindOne(t, waitIn(s, Dirt{}))
	want := []string{"State error `type`, keyed `70`.", "It's in the issue description, where a 👍 can't acknowledge it, so it ends when its cause is gone."}
	if got.Key != "err-12-type-70" || got.Link != "https://github.com/o/r/issues/12" || got.Comment != state.BodyID || !slices.Equal(waitTexts(got.Lines), want) {
		t.Errorf("got %s %s %d %q, want err-12-type-70 linked to the issue, %q", got.Key, got.Link, got.Comment, waitTexts(got.Lines), want)
	}
}

// waitLink falls back to the issue for the body, and waitBody finds no
// comment for it.
func TestWaitLinkAndBody(t *testing.T) {
	t.Parallel()
	if got := waitLink("o", "r", 3, 0); got != "https://github.com/o/r/issues/3" {
		t.Errorf("waitLink(body) = %q", got)
	}
	if got := waitLink("o", "r", 3, 42); got != "https://github.com/o/r/issues/3#issuecomment-42" {
		t.Errorf("waitLink(42) = %q", got)
	}
	s := &github.Snapshot{Comments: []github.Comment{{ID: 0, Body: "zero"}, {ID: 5, Body: "five"}}}
	if got := waitBody(s, 0); got != "" {
		t.Errorf("waitBody(body) = %q, want none", got)
	}
	if got := waitBody(s, 5); got != "five" {
		t.Errorf("waitBody(5) = %q, want five", got)
	}
	for k, want := range map[waitKind]string{waitError: "error", waitApproval: "approval", waitQuestion: "question", waitFlag: "flag", 9: "kind9"} {
		if k.String() != want {
			t.Errorf("waitKind(%d) = %s, want %s", k, k, want)
		}
	}
	if waitCount(1, "question") != "1 question" || waitCount(0, "question") != "0 questions" {
		t.Errorf("waitCount: %q, %q", waitCount(1, "question"), waitCount(0, "question"))
	}
}

// waitRankSnaps are the issues the ranking golden test holds:
//   - 10 has 11 and 12 below it, 13 below 12, and 14 below 11. 12 is
//     closed, so 10 unblocks 4: itself, 11, 13 and 14.
//   - 20 and 21 are each other's parent, a loop: 20 unblocks 2.
//   - 40 has 41 below it, closed, so it unblocks 1.
//   - 30 to 36 have nothing below them.
func waitRankSnaps() map[int]*github.Snapshot {
	snaps := map[int]*github.Snapshot{}
	for n, parent := range map[int]int{10: 0, 11: 10, 12: 10, 13: 12, 14: 11, 20: 21, 21: 20, 30: 0, 31: 0, 32: 0, 33: 0, 34: 0, 35: 0, 36: 0, 40: 0, 41: 40, 50: 99} {
		snaps[n] = &github.Snapshot{Number: n, Parent: parent, Closed: n == 12 || n == 41}
	}
	return snaps
}

// waitRankItems are the items the ranking golden test ranks, in the order
// it gives them. Each tie-break decides at least one pair.
func waitRankItems() []waitItem {
	at := func(h int) time.Time { return waitT0.Add(time.Duration(h) * time.Hour) }
	return []waitItem{
		{Issue: 30, Kind: waitFlag, Key: "flag-30-1", Priority: 0, Created: at(1), Comment: 1},            // kind: a flag is last, p0 or not
		{Issue: 31, Kind: waitQuestion, Key: "q-31-1", Priority: 2, Created: at(1), Comment: 1},           // kind: before flags
		{Issue: 32, Kind: waitApproval, Key: "rev-32-1", Priority: 3, Created: at(1), Comment: 1},         // kind: before questions
		{Issue: 33, Kind: waitError, Key: "err-33-transition-5", Priority: 2, Created: at(5), Comment: 9}, // key: after -4
		{Issue: 34, Kind: waitError, Key: "err-34-labels-0", Priority: 1, Created: at(9), Comment: 1},     // priority: p1 before p2
		{Issue: 35, Kind: waitError, Key: "err-35-labels-0", Priority: 2, Created: at(2), Comment: 1},     // created: older first
		{Issue: 36, Kind: waitError, Key: "err-36-labels-0", Priority: 2, Created: at(5), Comment: 1},     // number: 33 before 36
		{Issue: 33, Kind: waitError, Key: "err-33-labels-0", Priority: 2, Created: at(5), Comment: 3},     // comment: 3 before 9
		{Issue: 33, Kind: waitError, Key: "err-33-transition-4", Priority: 2, Created: at(5), Comment: 9}, // key: before -5
		{Issue: 10, Kind: waitQuestion, Key: "q-10-2", Priority: 3, Created: at(10), Comment: 1},          // unblocks 4: first, whatever else
		{Issue: 20, Kind: waitFlag, Key: "flag-20-1", Priority: 3, Created: at(20), Comment: 1},           // unblocks 2, through a loop
		{Issue: 13, Kind: waitError, Key: "err-13-type-1", Priority: 0, Created: at(0), Comment: 1},       // unblocks 1: below a closed issue
		{Issue: 40, Kind: waitApproval, Key: "rev-40-1", Priority: 2, Created: at(0), Comment: 1},         // unblocks 1: its child is closed
		{Issue: 50, Kind: waitQuestion, Key: "q-50-1", Priority: 2, Created: at(0), Comment: 1},           // unblocks 1: its parent isn't held
		{Issue: 60, Kind: waitApproval, Key: "rev-60-1", Priority: 2, Created: at(0), Comment: 1},         // unblocks 1: not held at all
	}
}

// waitRankText is one line per ranked item, as the golden file has them.
func waitRankText(items []waitItem) string {
	var lines []string
	for _, it := range items {
		lines = append(lines, fmt.Sprintf("%-20s unblocks=%d kind=%-8s p%d created=%s comment=%d",
			it.Key, it.Unblocks, it.Kind, it.Priority, it.Created.Format(time.RFC3339), it.Comment))
	}
	return strings.Join(lines, "\n")
}

// waitGolden compares got with testdata/waiting/<name>.txt, or writes it
// there with -update.
func waitGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "waiting", name+".txt")
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got+"\n" != string(want) {
		t.Errorf("%s: got\n%s\nwant\n%s", name, got, want)
	}
}

// Ranking (Q3a) counts open descendants from the parents held and breaks
// each tie in turn, and any order of the same items ranks the same.
func TestWaitRankGolden(t *testing.T) {
	t.Parallel()
	items := waitRankItems()
	waitRank(items, waitRankSnaps())
	waitGolden(t, "rank", waitRankText(items))
	reversed := waitRankItems()
	slices.Reverse(reversed)
	waitRank(reversed, waitRankSnaps())
	if got, want := waitRankText(reversed), waitRankText(items); got != want {
		t.Errorf("reversed input ranks\n%s\nwant\n%s", got, want)
	}
}

// Items found on real snapshots rank by their issues' tree: a parent's
// question beats its child's error, which beats nothing.
func TestWaitFindThenRank(t *testing.T) {
	t.Parallel()
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Q", Blocks: []int{70}, Recommended: interviewOption{Text: "A", Why: "w"}, Others: []string{"B"}},
	}}
	parent := waitSnap(70, nil, join(renderQuestions(round, 70, 1, nil), waitRoundMarker(1, 1)))
	child := waitSnap(71, []string{"priority:p0"}, "Err.\n\n<!-- issuedog:error kind=transition key=50 -->")
	child.Parent = 70
	var items []waitItem
	for _, s := range []*github.Snapshot{child, parent} {
		found, err := waitFind(waitIn(s, Dirt{}))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, found...)
	}
	waitRank(items, map[int]*github.Snapshot{70: parent, 71: child})
	if len(items) != 2 || items[0].Key != "q-70-1" || items[0].Unblocks != 2 || items[1].Key != "err-71-transition-50" || items[1].Unblocks != 1 {
		t.Errorf("got %s, want q-70-1 (2) then err-71-transition-50 (1)", waitRankText(items))
	}
}
