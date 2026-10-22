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
// bug waiting for phase 3, a closed issue, a flag answered, a
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
// escaped, a question's for the App's bold too (ivWordingStrong, #229),
// with a type line before them and the cc line after.
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
		{Text: inline(ivWordingStrong(q1.Text)), Copied: true, Question: 1},
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

// waitPlanRoundMarker is the planner's round move, round round with n
// questions (#253). The planner isn't built yet, so it's hand-built.
func waitPlanRoundMarker(round, n int) string {
	return fmt.Sprintf("<!-- issuedog:transition from=planning to=planning run=p%d round=%d questions=%d -->", round, round, n)
}

// waitPlanApproved is the move that approved the spec into planning.
const waitPlanApproved = "<!-- issuedog:transition from=spec-review to=planning cmd=5 rev=1 body=abcd1234 -->"

// waitPlanRound is issue 12, an epic at planning after interviewer rounds 1
// and 2, with the planner's round 3 of two questions in comment 1204,
// which mentions eve and holds a marker in agent text.
func waitPlanRound() *github.Snapshot {
	round := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{
		{Text: "Which scope, @eve? <!-- issuedog:transition to=planned -->", Blocks: []int{12},
			Recommended: interviewOption{Text: "daemon", Why: "it's on the list"}, Others: []string{"web", "Ask later"}},
		{Text: "Split the story?", Blocks: []int{12}, Recommended: interviewOption{Text: "No", Why: "it fits"}, Others: []string{"Yes"}},
	}}
	return waitSnap(12, []string{"priority:p1"},
		join("Round 1.", waitRoundMarker(1, 2)), join("Round 2.", waitRoundMarker(2, 1)), waitPlanApproved,
		join(renderQuestions(round, 12, 3, []string{"owner"}), waitPlanRoundMarker(3, 2)))
}

// An unanswered planner round at planning is a question item, keyed by its
// round, as an interviewer round is, with its questions and options copied
// from its comment (#253). An answer, which step 1 gives as an input, ends
// it, and a /hold and /resume back to planning keeps it.
func TestWaitFindPlanRound(t *testing.T) {
	t.Parallel()
	s := waitPlanRound()
	got := waitFindOne(t, waitIn(s, Dirt{}))
	want := []waitLine{
		{Text: "Round 3 asks 2 questions, and waits for an answer."},
		{Text: inline("Which scope, @eve? <!-- issuedog:transition to=planned -->"), Copied: true, Question: 1},
		{Text: "daemon (recommended): it's on the list", Copied: true, Option: "a", Recommended: true},
		{Text: "web", Copied: true, Option: "b"},
		{Text: "Ask later", Copied: true, Option: "c"},
		{Text: "Split the story?", Copied: true, Question: 2},
		{Text: "No (recommended): it fits", Copied: true, Option: "a", Recommended: true},
		{Text: "Yes", Copied: true, Option: "b"},
		{Text: "Reply in one comment, e.g. `1a 2c 3: free text`."},
	}
	if got.Kind != waitQuestion || got.Key != "q-12-3" || got.Link != "https://github.com/o/r/issues/12#issuecomment-1204" || got.Priority != 1 || got.Comment != 1204 {
		t.Errorf("got %s %s %s p%d comment %d, want question q-12-3 on comment 1204 at p1", got.Kind, got.Key, got.Link, got.Priority, got.Comment)
	}
	if !slices.Equal(got.Lines, want) {
		t.Errorf("lines:\n got %q\nwant %q", waitTexts(got.Lines), waitTexts(want))
	}
	answer := github.Comment{ID: 1299, Author: waitOwner, Body: "1a 2a"}
	if items, err := waitFind(waitIn(s, Dirt{}, answer)); err != nil || len(items) != 0 {
		t.Errorf("waitFind with an answer = %+v, %v; want no item", items, err)
	}
	held := waitSnap(12, nil, waitPlanApproved, join("Round.", waitPlanRoundMarker(1, 1)),
		"<!-- issuedog:transition from=planning to=held at=planning reason=owner cmd=1210 -->",
		"<!-- issuedog:transition from=held to=planning cmd=1211 seen=1202 -->")
	if got := waitFindOne(t, waitIn(held, Dirt{})); got.Key != "q-12-1" || got.Comment != 1202 {
		t.Errorf("after a /hold and /resume, got %s on comment %d, want q-12-1 on comment 1202", got.Key, got.Comment)
	}
}

// These wait on nobody at planning: the stage entered with no round, a
// planner round move with no round, no questions or no run, a dirty round
// comment, a planner round that a later move left, and an override back
// into planning after a round, which is an entry (#253). The move to
// plan-review leaves no question, only the plan's approval, which
// TestWaitFindPlanApproval covers (#270).
func TestWaitFindPlanRoundNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		s    *github.Snapshot
		dirt Dirt
	}{
		{"entered", waitSnap(12, nil, waitPlanApproved), Dirt{}},
		{"round 0", waitSnap(12, nil, waitPlanApproved, join("Round.", waitPlanRoundMarker(0, 1))), Dirt{}},
		{"no questions", waitSnap(12, nil, waitPlanApproved, join("Round.", waitPlanRoundMarker(1, 0))), Dirt{}},
		{"no run", waitSnap(12, nil, waitPlanApproved, "<!-- issuedog:transition from=planning to=planning cmd=5 round=1 questions=1 -->"), Dirt{}},
		{"dirty round", waitSnap(12, nil, waitPlanApproved, join("Round.", waitPlanRoundMarker(1, 1))), Dirt{1202: "eve"}},
		{"plan-review after the round", waitSnap(12, nil, waitPlanApproved, join("Round.", waitPlanRoundMarker(1, 1)),
			"<!-- issuedog:transition from=planning to=plan-review run=p2 -->"), Dirt{}},
		{"an override back into planning after a round", waitSnap(12, nil, waitPlanApproved,
			join("Round.", waitPlanRoundMarker(1, 1)),
			"<!-- issuedog:transition from=planning to=clarifying by=owner event=7 -->",
			"<!-- issuedog:transition from=clarifying to=planning by=owner event=8 -->"), Dirt{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, err := waitFind(waitIn(c.s, c.dirt))
			if err != nil {
				t.Fatal(err)
			}
			items = slices.DeleteFunc(items, func(it waitItem) bool { return it.Kind == waitPlanApproval })
			if len(items) != 0 {
				t.Errorf("got %+v, want no item", items)
			}
		})
	}
}

// At clarifying, a planner round's marker isn't the interviewer's round:
// the stage's own round is the one that waits.
func TestWaitFindPlanRoundAtClarifying(t *testing.T) {
	t.Parallel()
	s := waitSnap(12, nil, waitPlanApproved, join("Round.", waitPlanRoundMarker(1, 1)),
		"<!-- issuedog:transition from=planning to=clarifying by=owner event=7 -->")
	if items, err := waitFind(waitIn(s, Dirt{})); err != nil || len(items) != 0 {
		t.Errorf("waitFind = %+v, %v; want no item", items, err)
	}
}

// The digest's planner round item (#253): the golden is the digest's body
// with the planner round alone, and it ranks as an interviewer round does.
func TestWaitPlanRoundDigestGolden(t *testing.T) {
	t.Parallel()
	items := []waitItem{waitFindOne(t, waitIn(waitPlanRound(), Dirt{}))}
	waitRank(items, map[int]*github.Snapshot{12: waitPlanRound()})
	body := digestRenderBody("", items)
	waitGolden(t, "plan-round", body)
	digestRenderCheck(t, body)
	// Beside an interviewer round of the same priority on an older issue,
	// it ranks by SPEC §9.4's tie-breaks alone: the older issue first.
	iv := waitFindOne(t, waitIn(waitSnap(3, []string{"priority:p1"}, join("Round.", waitRoundMarker(1, 1))), Dirt{}))
	ranked := []waitItem{items[0], iv}
	waitRank(ranked, map[int]*github.Snapshot{12: waitPlanRound()})
	if ranked[0].Key != "q-3-1" || ranked[1].Key != "q-12-3" || ranked[0].Kind != ranked[1].Kind {
		t.Errorf("ranked %s then %s, want the interviewer round on the older issue first, both questions", ranked[0].Key, ranked[1].Key)
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

// waitPlanRevision is a plan comment, plan revision rev after text, as a
// move with PlanRev writes it, but with no transition marker, so the
// issue stays at its stage.
func waitPlanRevision(t *testing.T, rev int, text string) string {
	t.Helper()
	body, err := planRevision(text, rev)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// The approval item is the latest spec revision's, never a plan revision's
// after it (SPEC §7.4, Two kinds): its key, its link and its hash.
func TestWaitFindApprovalIgnoresPlanRevision(t *testing.T) {
	t.Parallel()
	plan := waitPlanRevision(t, 2, "### Plan for #14 (rev 2)")
	s := waitSnap(14, nil, waitRevision(1, "The spec."), plan)
	got := waitFindOne(t, waitIn(s, Dirt{}))
	want := "Revision 1 of the spec, `" + state.BodyHash("The spec.") + "`, waits for approval."
	if got.Key != "rev-14-1" || got.Link != "https://github.com/o/r/issues/14#issuecomment-1401" || len(got.Lines) != 3 || got.Lines[0].Text != want {
		t.Errorf("got %s %s %q, want rev-14-1 on comment 1401, approvable, starting %q", got.Key, got.Link, waitTexts(got.Lines), want)
	}
	// With only a plan revision, there's no spec revision to approve.
	none := waitSnap(15, nil, "<!-- issuedog:transition from=clarifying to=spec-review run=s1 -->", plan)
	got = waitFindOne(t, waitIn(none, Dirt{}))
	if got.Key != "rev-15-0" || got.Link != "https://github.com/o/r/issues/15#issuecomment-1501" {
		t.Errorf("got %s %s %q, want rev-15-0 on comment 1501", got.Key, got.Link, waitTexts(got.Lines))
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
	for k, want := range map[waitKind]string{waitError: "error", waitApproval: "approval", waitQuestion: "question", waitFlag: "flag", waitPlanApproval: "plan-approval", waitHuman: "human", 9: "kind9"} {
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

// Ranking (SPEC §9.4, The order) counts open descendants from the parents
// held and breaks each tie in turn, and any order of the same items ranks
// the same.
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

// waitPlanText is the text of the plan comment for plan revision rev, as
// a planner run posts it (#247): the plan, a cc line, and the move from
// planning into plan-review, all above the revision marker.
func waitPlanText(rev int) string {
	return fmt.Sprintf("### Plan for the issue (rev %d)\n\nThe plan.\n\ncc @owner\n\n<!-- issuedog:transition from=planning to=plan-review run=p%d -->", rev, rev)
}

// waitPlanComment is the plan comment for plan revision rev, with its
// revision marker on the last line (planRevision).
func waitPlanComment(t *testing.T, rev int) string {
	t.Helper()
	return waitPlanRevision(t, rev, waitPlanText(rev))
}

// waitPlanHash is the hash plan revision rev's comment records.
func waitPlanHash(t *testing.T, rev int) string {
	t.Helper()
	h, ok := state.PlanHash(waitPlanComment(t, rev))
	if !ok {
		t.Fatalf("plan revision %d has no hash", rev)
	}
	return h
}

// waitStory is story n, item S<item> of the plan on #40, filed by the App
// with its origin marker in the body, after comments: as waitSnap, with
// type story, parent 40 and labels.
func waitStory(n, item int, labels []string, comments ...string) *github.Snapshot {
	s := waitSnap(n, labels, comments...)
	s.Type, s.Parent, s.Author = "story", 40, waitApp
	s.Body = fmt.Sprintf("The story.\n\n<!-- issuedog:origin plan=#40 rev=1 item=S%d -->", item)
	return s
}

// The plan approval lines every approvable plan ends with (#270).
const (
	waitPlanApprove = "React 👍 to the plan comment, or post `/approve`, to approve it."
	waitPlanBack    = "Post `/revise <notes>`, or any other comment, to send it back."
	waitPlanRenew   = "Post `/revise <notes>`, or any other comment, to send it back and get a new plan."
)

// A story at plan-review with a clean plan revision is a plan's approval
// (#270): keyed plan-<n>-<rev>, linked to the plan comment, at its
// priority, with the revision and its hash, how to approve it and how to
// send it back. It's placed in its plan, from its origin marker: #40,
// item 2.
func TestWaitFindPlanApproval(t *testing.T) {
	t.Parallel()
	s := waitStory(41, 2, []string{"priority:p1"}, waitPlanComment(t, 1))
	got := waitFindOne(t, waitIn(s, Dirt{}))
	want := []string{"Revision 1 of the plan, `" + waitPlanHash(t, 1) + "`, waits for approval.", waitPlanApprove, waitPlanBack}
	if got.Kind != waitPlanApproval || got.Key != "plan-41-1" || got.Link != "https://github.com/o/r/issues/41#issuecomment-4101" ||
		got.Comment != 4101 || got.Priority != 1 || got.Plan != 40 || got.Index != 2 {
		t.Errorf("got %s %s %s comment %d p%d plan %d index %d; want a plan approval plan-41-1 on comment 4101 at p1, plan 40, index 2",
			got.Kind, got.Key, got.Link, got.Comment, got.Priority, got.Plan, got.Index)
	}
	if !slices.Equal(waitTexts(got.Lines), want) || slices.ContainsFunc(got.Lines, func(l waitLine) bool { return l.Copied }) {
		t.Errorf("lines = %+v, want %q, none copied", got.Lines, want)
	}
	// A later plan revision, posted with the move back from planning after
	// a revision request, is the one to approve.
	revised := waitStory(41, 2, nil, waitPlanComment(t, 1), "<!-- issuedog:transition from=plan-review to=planning cmd=9 seen=9 -->",
		waitPlanComment(t, 2))
	got = waitFindOne(t, waitIn(revised, Dirt{}))
	if got.Key != "plan-41-2" || got.Comment != 4103 || got.Lines[0].Text != "Revision 2 of the plan, `"+waitPlanHash(t, 2)+"`, waits for approval." {
		t.Errorf("after a revision, got %s on comment %d, %q; want plan-41-2 on comment 4103", got.Key, got.Comment, waitTexts(got.Lines))
	}
	// Back from a hold, it still links the plan comment, not the move back.
	resumed := waitStory(41, 2, nil, waitPlanComment(t, 1), "<!-- issuedog:transition from=plan-review to=held at=plan-review reason=owner cmd=5 -->",
		"<!-- issuedog:transition from=held to=plan-review cmd=6 seen=5 -->")
	got = waitFindOne(t, waitIn(resumed, Dirt{}))
	if got.Key != "plan-41-1" || got.Comment != 4101 || got.Link != "https://github.com/o/r/issues/41#issuecomment-4101" || !slices.Equal(waitTexts(got.Lines), want) {
		t.Errorf("back from a hold, got %s on comment %d, %s, %q; want plan-41-1 on comment 4101, approvable", got.Key, got.Comment, got.Link, waitTexts(got.Lines))
	}
	// A story whose body the owner edited is placed by the origin marker
	// in an App comment instead (#290).
	edited := waitStory(41, 2, nil, waitPlanComment(t, 1), "<!-- issuedog:origin plan=#40 rev=1 item=S2 -->")
	edited.Body, edited.BodyEdit = "The story, edited.", github.Edit{By: waitOwner, At: waitT0}
	got = waitFindOne(t, waitIn(edited, Dirt{}))
	if got.Kind != waitPlanApproval || got.Key != "plan-41-1" || got.Plan != 40 || got.Index != 2 {
		t.Errorf("with its origin in a comment, got %s %s plan %d index %d; want plan-41-1 in plan 40, index 2", got.Kind, got.Key, got.Plan, got.Index)
	}
	// An epic, with no origin marker, isn't placed in any plan.
	epic := waitSnap(42, nil, waitPlanComment(t, 1))
	got = waitFindOne(t, waitIn(epic, Dirt{}))
	if got.Kind != waitPlanApproval || got.Key != "plan-42-1" || got.Plan != 0 || got.Index != 0 || !slices.Equal(waitTexts(got.Lines), want) {
		t.Errorf("the epic's item = %+v; want plan-42-1 in no plan, approvable", got)
	}
}

// A plan that can't be approved yet says why, a reason a line, in
// state.PlanApproval's order and then the type's, without the login of
// whoever made something dirty, and doesn't say how to approve it. One
// with no clean revision, a hidden one or one whose comment changed says
// that sending it back gets a new plan (SPEC §7.4), and one with no
// clean revision links the transition, as it says "no clean revision",
// not "no plan yet" (#267's verify).
func TestWaitFindPlanApprovalBlocked(t *testing.T) {
	t.Parallel()
	plan := waitPlanComment(t, 1)
	hash := waitPlanHash(t, 1)
	// A plan comment whose revision marker records another hash.
	changed := waitPlanText(1) + "\n<!-- issuedog:revision kind=plan rev=1 body=0badbeef -->"
	// Held from plan-review, then back: the plan comment's later dirt
	// leaves no clean revision, and the stage from the later moves.
	heldBack := waitStory(43, 1, nil, plan, "<!-- issuedog:transition from=plan-review to=held at=plan-review reason=owner cmd=5 -->",
		"<!-- issuedog:transition from=held to=plan-review cmd=6 seen=5 -->")
	set := waitStory(43, 1, nil, "<!-- issuedog:transition from=planning to=plan-review by=owner event=7 -->")
	untyped := waitStory(43, 1, nil, plan)
	untyped.Type = ""
	unknown := waitStory(43, 1, nil, plan)
	unknown.Type = "feature"
	retyped := waitStory(43, 1, nil, plan)
	retyped.Type, retyped.TypeEvent = "task", 70
	const (
		none   = "there's no clean plan revision to approve: none was posted, or it was edited without approval or deleted"
		hidden = "a comment of mine after plan revision 1 was edited without approval or deleted, so it may hide a newer revision"
		dirty  = "the issue description has an edit that wasn't approved (see the note on it)"
	)
	cases := []struct {
		name    string
		s       *github.Snapshot
		dirt    Dirt
		key     string
		comment int64
		first   string
		why     []string
		last    string
	}{
		{"set by hand, with no plan", set, Dirt{}, "plan-43-0", 4301, "The plan waits for review, with no clean revision to approve.", []string{none}, waitPlanRenew},
		{"the plan comment made dirty", heldBack, Dirt{4301: "eve"}, "plan-43-0", 4303, "The plan waits for review, with no clean revision to approve.",
			[]string{none}, waitPlanRenew},
		{"hidden", waitStory(43, 1, nil, plan, "Edited by someone."), Dirt{4302: "eve"}, "plan-43-1", 4301,
			"Revision 1 of the plan, `" + hash + "`, waits for approval.", []string{hidden}, waitPlanRenew},
		{"changed", waitStory(43, 1, nil, changed), Dirt{}, "plan-43-1", 4301, "Revision 1 of the plan, `0badbeef`, waits for approval.",
			[]string{"the comment of plan revision 1 changed after it was posted, so it no longer matches `0badbeef`"}, waitPlanRenew},
		{"dirty body", waitStory(43, 1, nil, plan), Dirt{state.BodyID: "eve"}, "plan-43-1", 4301,
			"Revision 1 of the plan, `" + hash + "`, waits for approval.", []string{dirty}, waitPlanBack},
		{"hidden and a dirty body", waitStory(43, 1, nil, plan, "Edited by someone."), Dirt{state.BodyID: "eve", 4302: "eve"}, "plan-43-1", 4301,
			"Revision 1 of the plan, `" + hash + "`, waits for approval.", []string{hidden, dirty}, waitPlanRenew},
		{"untyped", untyped, Dirt{}, "plan-43-1", 4301, "Revision 1 of the plan, `" + hash + "`, waits for approval.",
			[]string{"an issue of its type doesn't go on to `planned` on approval"}, waitPlanBack},
		{"unknown type", unknown, Dirt{}, "plan-43-1", 4301, "Revision 1 of the plan, `" + hash + "`, waits for approval.",
			[]string{"I can't tell what kind of issue this is from its type"}, waitPlanBack},
		{"retyped", retyped, Dirt{}, "plan-43-1", 4301, "Revision 1 of the plan, `" + hash + "`, waits for approval.",
			[]string{"its type doesn't fit `plan-review`, so a 👍 is held back until the type changes (see the type error)"}, waitPlanBack},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			items, err := waitFind(waitIn(c.s, c.dirt))
			if err != nil {
				t.Fatal(err)
			}
			// A retyped issue has its type error too, which isn't this test's.
			items = slices.DeleteFunc(items, func(it waitItem) bool { return it.Kind == waitError })
			if len(items) != 1 {
				t.Fatalf("got %+v, want one plan approval", items)
			}
			got := items[0]
			if got.Kind != waitPlanApproval || got.Key != c.key || got.Comment != c.comment || got.Link != waitLink("o", "r", 43, c.comment) {
				t.Errorf("got %s %s on comment %d, %s; want a plan approval %s on comment %d", got.Kind, got.Key, got.Comment, got.Link, c.key, c.comment)
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
			if texts := waitTexts(got.Lines); len(texts) != len(c.why)+2 || texts[0] != c.first || texts[len(texts)-1] != c.last {
				t.Errorf("lines = %q, want %q, the reasons, then %q", texts, c.first, c.last)
			}
		})
	}
}

// The plan's approval goes when the issue moves to planned, by an
// approval, or to planning, by a revision request, and the move to
// planning leaves no question either (#270).
func TestWaitFindPlanApprovalGoes(t *testing.T) {
	t.Parallel()
	plan := waitPlanComment(t, 1)
	approved := fmt.Sprintf("<!-- issuedog:transition from=plan-review to=planned rev=1 body=%s cmd=r9 seen=4401 -->", waitPlanHash(t, 1))
	for name, s := range map[string]*github.Snapshot{
		"approved":  waitStory(44, 1, nil, plan, approved),
		"sent back": waitStory(44, 1, nil, plan, "<!-- issuedog:transition from=plan-review to=planning cmd=4403 seen=4403 -->", "Not like this."),
	} {
		if items, err := waitFind(waitIn(s, Dirt{})); err != nil || len(items) != 0 {
			t.Errorf("%s: waitFind = %+v, %v; want no item", name, items, err)
		}
	}
}

// At plan-review, the item is the plan's, never the spec's (SPEC §7.4,
// Two kinds): a spec revision 2 before plan revision 1 gives plan-<n>-1
// on the plan comment, whose key isn't the spec approval's rev-<n>-<rev>,
// even for the same revision number. At spec-review, the same comments
// give the spec's.
func TestWaitFindPlanApprovalKey(t *testing.T) {
	t.Parallel()
	spec := "**Revision 2**\n\n<!-- issuedog:revision rev=2 body=" + state.BodyHash("The spec.") + " seen=5 -->"
	s := waitSnap(45, nil, waitRevision(1, "The spec."), spec, waitPlanComment(t, 1))
	got := waitFindOne(t, waitIn(s, Dirt{}))
	if got.Kind != waitPlanApproval || got.Key != "plan-45-1" || got.Comment != 4503 || got.Lines[0].Text != "Revision 1 of the plan, `"+waitPlanHash(t, 1)+"`, waits for approval." {
		t.Errorf("got %s %s on comment %d, %q; want the plan's plan-45-1 on comment 4503", got.Kind, got.Key, got.Comment, waitTexts(got.Lines))
	}
	atSpec := waitSnap(45, nil, waitRevision(1, "The spec."), waitPlanRevision(t, 1, "### Plan"))
	spe := waitFindOne(t, waitIn(atSpec, Dirt{}))
	if spe.Kind != waitApproval || spe.Key != "rev-45-1" || spe.Key == "plan-45-1" {
		t.Errorf("at spec-review got %s %s; want the spec's rev-45-1", spe.Kind, spe.Key)
	}
}

// waitPlanOrder places an item by its origin: its plan and its number,
// for a story's or a task's ID, and nothing for no origin or another ID.
func TestWaitPlanOrder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		o           state.Origin
		plan, index int
	}{
		{state.Origin{}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "S2"}, 40, 2},
		{state.Origin{Plan: 40, Rev: 3, Item: "T10"}, 40, 10},
		{state.Origin{Plan: 40, Rev: 1, Item: "T999999999"}, 40, 999999999},
		{state.Origin{Plan: 0, Rev: 1, Item: "S2"}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "X2"}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "s2"}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "S0"}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "S02"}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "S2a"}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "S"}, 0, 0},
		{state.Origin{Plan: 40, Rev: 1, Item: "S1234567890"}, 0, 0},
	} {
		if plan, index := waitPlanOrder(c.o); plan != c.plan || index != c.index {
			t.Errorf("waitPlanOrder(%+v) = %d, %d; want %d, %d", c.o, plan, index, c.plan, c.index)
		}
	}
}

// waitPlanRankSnaps are the issues the plan ranking test holds: 73 has 74
// below it, open, so it unblocks 2. Every other issue has nothing below.
func waitPlanRankSnaps() map[int]*github.Snapshot {
	return map[int]*github.Snapshot{73: {Number: 73, Parent: 70}, 74: {Number: 74, Parent: 73}}
}

// waitPlanRankItems are the items the plan ranking test ranks:
//   - plan #40's S2 at p1 and S1 at p2: the group ranks as S2 would, at
//     p1, and lists S1 first;
//   - plan #70's S1 at p3, S2 at p2 and S3 at p3, which unblocks 2: the
//     group ranks as S3, first of all, and lists S1, S2, S3;
//   - an epic's plan approval with no plan, at p2 on the oldest issue: it
//     ranks alone, after the p1 approvals and before the question;
//   - a state error, spec approvals at p0 and p1 around plan #40's place,
//     and a question at p0, each where it ranked before.
func waitPlanRankItems() []waitItem {
	at := func(h int) time.Time { return waitT0.Add(time.Duration(h) * time.Hour) }
	return []waitItem{
		{Issue: 41, Kind: waitPlanApproval, Key: "plan-41-1", Priority: 1, Created: at(41), Comment: 1, Plan: 40, Index: 2},
		{Issue: 42, Kind: waitPlanApproval, Key: "plan-42-1", Priority: 2, Created: at(42), Comment: 1, Plan: 40, Index: 1},
		{Issue: 71, Kind: waitPlanApproval, Key: "plan-71-1", Priority: 3, Created: at(71), Comment: 1, Plan: 70, Index: 1},
		{Issue: 72, Kind: waitPlanApproval, Key: "plan-72-1", Priority: 2, Created: at(72), Comment: 1, Plan: 70, Index: 2},
		{Issue: 73, Kind: waitPlanApproval, Key: "plan-73-1", Priority: 3, Created: at(5), Comment: 1, Plan: 70, Index: 3},
		{Issue: 60, Kind: waitPlanApproval, Key: "plan-60-1", Priority: 2, Created: at(1), Comment: 1},
		{Issue: 30, Kind: waitError, Key: "err-30-labels-0", Priority: 2, Created: at(30), Comment: 1},
		{Issue: 31, Kind: waitApproval, Key: "rev-31-1", Priority: 0, Created: at(31), Comment: 1},
		{Issue: 32, Kind: waitApproval, Key: "rev-32-1", Priority: 1, Created: at(32), Comment: 1},
		{Issue: 33, Kind: waitApproval, Key: "rev-33-1", Priority: 1, Created: at(50), Comment: 1},
		{Issue: 34, Kind: waitQuestion, Key: "q-34-1", Priority: 0, Created: at(34), Comment: 1},
	}
}

// waitPlanRankText is waitRankText with each item's plan and index.
func waitPlanRankText(items []waitItem) string {
	var lines []string
	for _, it := range items {
		lines = append(lines, fmt.Sprintf("%-15s unblocks=%d kind=%-13s p%d created=%s plan=%d index=%d",
			it.Key, it.Unblocks, it.Kind, it.Priority, it.Created.Format(time.RFC3339), it.Plan, it.Index))
	}
	return strings.Join(lines, "\n")
}

// A plan's approvals rank in groups by plan (#270): each group where its
// best member would rank alone, its members in item order, whatever their
// priorities; an approval with no plan, and every other item, by SPEC
// §9.4's order. Any order of the same items ranks the same.
func TestWaitRankPlanGolden(t *testing.T) {
	t.Parallel()
	items := waitPlanRankItems()
	waitRank(items, waitPlanRankSnaps())
	waitGolden(t, "plan-rank", waitPlanRankText(items))
	reversed := waitPlanRankItems()
	slices.Reverse(reversed)
	waitRank(reversed, waitPlanRankSnaps())
	if got, want := waitPlanRankText(reversed), waitPlanRankText(items); got != want {
		t.Errorf("reversed input ranks\n%s\nwant\n%s", got, want)
	}
	// Plan #40's S1 and S2 are next to each other, where S2 alone ranks.
	alone := slices.DeleteFunc(waitPlanRankItems(), func(it waitItem) bool { return it.Key == "plan-42-1" })
	waitRank(alone, waitPlanRankSnaps())
	where := slices.IndexFunc(alone, func(it waitItem) bool { return it.Key == "plan-41-1" })
	s1 := slices.IndexFunc(items, func(it waitItem) bool { return it.Key == "plan-42-1" })
	if where < 0 || s1 != where || items[s1+1].Key != "plan-41-1" {
		t.Errorf("S1 at %d, S2 alone at %d, in\n%s\nwant S1 then S2 where S2 alone ranks", s1, where, waitPlanRankText(items))
	}
	// With no plans, the same items rank by SPEC §9.4's order alone, as
	// before #270.
	plain := waitPlanRankItems()
	for i := range plain {
		plain[i].Plan, plain[i].Index = 0, 0
	}
	waitRank(plain, waitPlanRankSnaps())
	var keys []string
	for _, it := range plain {
		keys = append(keys, it.Key)
	}
	if want := []string{"plan-73-1", "err-30-labels-0", "rev-31-1", "rev-32-1", "plan-41-1", "rev-33-1", "plan-60-1", "plan-42-1", "plan-72-1", "plan-71-1", "q-34-1"}; !slices.Equal(keys, want) {
		t.Errorf("with no plans, ranked %q; want %q", keys, want)
	}
}

// Two groups whose best members tie on every one of SPEC §9.4's keys,
// which only items alike in issue, comment and key can, still don't mix:
// the lower plan's group goes first, whole, so the order stays total.
func TestWaitRankPlanTie(t *testing.T) {
	t.Parallel()
	tied := waitItem{Issue: 9, Kind: waitPlanApproval, Key: "plan-9-1", Priority: 1, Created: waitT0, Comment: 1}
	items := []waitItem{
		{Issue: 91, Kind: waitPlanApproval, Key: "plan-91-1", Priority: 2, Created: waitT0, Comment: 1, Plan: 50, Index: 1},
		{Issue: 92, Kind: waitPlanApproval, Key: "plan-92-1", Priority: 2, Created: waitT0, Comment: 1, Plan: 40, Index: 1},
		tied, tied,
	}
	items[2].Plan, items[2].Index = 50, 2
	items[3].Plan, items[3].Index = 40, 2
	waitRank(items, nil)
	var got []string
	for _, it := range items {
		got = append(got, fmt.Sprintf("%s/%d", it.Key, it.Plan))
	}
	if want := []string{"plan-92-1/40", "plan-9-1/40", "plan-91-1/50", "plan-9-1/50"}; !slices.Equal(got, want) {
		t.Errorf("ranked %q, want %q", got, want)
	}
}

// waitPlanRankFound finds the plan approvals of plan #40's S1 at p2 and
// S2 at p1, from real snapshots, with a spec approval at p1 on an older
// issue and S2's hidden plan revision.
func waitPlanRankFound(t *testing.T) ([]waitItem, map[int]*github.Snapshot) {
	t.Helper()
	plan := waitPlanComment(t, 1)
	snaps := map[int]*github.Snapshot{
		46: waitStory(46, 1, nil, plan),
		47: waitStory(47, 2, []string{"priority:p1"}, plan, "Edited by someone."),
		8:  waitSnap(8, []string{"priority:p1"}, waitRevision(1, "The spec.")),
	}
	dirt := map[int]Dirt{47: {4702: "eve"}}
	var items []waitItem
	for _, n := range []int{8, 46, 47} {
		found, err := waitFind(waitIn(snaps[n], dirt[n]))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, found...)
	}
	waitRank(items, snaps)
	return items, snaps
}

// Found on real snapshots, plan #40's stories rank S1 then S2, after the
// older p1 spec approval, and the digest lists them so (#270): the golden
// is the digest's body, with an approvable plan and a hidden one, which
// mentions nobody, has no marker but its own, and names each "Plan
// approval".
func TestWaitPlanApprovalDigestGolden(t *testing.T) {
	t.Parallel()
	items, _ := waitPlanRankFound(t)
	var keys []string
	for _, it := range items {
		keys = append(keys, it.Key)
	}
	if want := []string{"rev-8-1", "plan-46-1", "plan-47-1"}; !slices.Equal(keys, want) {
		t.Fatalf("ranked %q, want %q", keys, want)
	}
	body := digestRenderBody("", items)
	digestRenderGolden(t, "plan-approval", body)
	digestRenderCheck(t, body)
	if n := strings.Count(body, "**Plan approval**, priority p"); n != 2 {
		t.Errorf("the body names %d plan approvals, want 2:\n%s", n, body)
	}
	// It gets no mention, so it alone posts no announcement.
	if news, err := digestRenderNews(items[1:], []string{"owner"}); news != "" || err != nil {
		t.Errorf("announcing the plan approvals gave %q, %v; want nothing to post", news, err)
	}
}
