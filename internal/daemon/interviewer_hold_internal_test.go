package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// ivHoldTestTrust trusts the logins it maps to true.
type ivHoldTestTrust map[string]bool

func (tr ivHoldTestTrust) Trusted(_ context.Context, login string) (bool, error) {
	return tr[login], nil
}

// ivHoldInputOf approves only a clean comment by someone who isn't
// trusted, no bot, that a trusted user, no bot, 👍'd, and counters only a
// trusted comment after an untrusted one, not a /hold or a /resume. It
// reads the reactions of the comments it may approve, and no others.
func TestIvHoldInputOf(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	before := f.AddComment(n, "mallory", "before the discussion")
	f.AddReaction(before, "owner", "+1")
	from := f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=spec-review to=held at=spec-review reason=discussion event=c1 -->")
	priority := f.AddComment(n, "owner", "/priority p1")
	approved := f.AddComment(n, "mallory", "approve me")
	f.AddReaction(approved, "eve", "+1")
	f.AddReaction(approved, "owner", "+1")
	eve := f.AddComment(n, "eve", "me too")
	f.AddReaction(eve, "eve", "+1")
	f.AddReaction(eve, "owner", "-1")
	bot := f.AddComment(n, "renovate[bot]", "bump")
	f.AddReaction(bot, "owner", "+1")
	dirty := f.AddComment(n, "mallory", "dirty")
	f.AddReaction(dirty, "owner", "+1")
	hold := f.AddComment(n, "owner", "/hold")
	counter := f.AddComment(n, "owner", "No.")
	c := f.Client(t)
	a := &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}
	s, err := c.Snapshot(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	tc := trustCache{st: &Step{Applier: a, Trust: ivHoldTestTrust{"owner": true}}, verdicts: map[string]bool{}}
	reqs := len(f.Requests())
	in, err := ivHoldInputOf(t.Context(), a, tc, s, Dirt{dirty: "eve"}, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Approved) != 1 || in.Approved[approved] != "owner" {
		t.Errorf("approved %v, want only %d, by owner", in.Approved, approved)
	}
	if !slices.Equal(in.Countered, []int64{counter}) {
		t.Errorf("countered %v, want only %d, not %d or %d", in.Countered, counter, priority, hold)
	}
	var read []string
	for _, r := range f.Requests()[reqs:] {
		if strings.Contains(r.Path, "/reactions") {
			read = append(read, r.Path)
		}
	}
	want := []string{fmt.Sprintf("/issues/comments/%d/reactions", approved), fmt.Sprintf("/issues/comments/%d/reactions", eve)}
	if !slices.Equal(read, want) {
		t.Errorf("read %q, want %q", read, want)
	}
}

// ivHoldRevise takes the oldest clean, trusted /revise above from and not
// above seen, that passes its check at spec-review, no marker names and
// the App hasn't reacted to.
func TestIvHoldReviseFind(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	f.AddComment(n, "owner", "/revise too old")
	from := f.AddComment(n, "issuedog[bot]", "held")
	f.AddComment(n, "mallory", "/revise untrusted")
	f.AddComment(n, "owner", "/revise")
	named := f.AddComment(n, "owner", "/revise named")
	reacted := f.AddComment(n, "owner", "/revise reacted")
	f.AddReaction(reacted, "issuedog[bot]", "-1")
	dirty := f.AddComment(n, "owner", "/revise dirty")
	want := f.AddComment(n, "owner", "/revise this one")
	later := f.AddComment(n, "owner", "/revise later")
	c := f.Client(t)
	a := &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}
	s, err := c.Snapshot(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	tc := trustCache{st: &Step{Applier: a, Trust: ivHoldTestTrust{"owner": true}}, verdicts: map[string]bool{}}
	is := state.Issue{Keys: map[state.Key]bool{state.CmdKey(named): true}}
	for _, tt := range []struct{ seen, want int64 }{{0, want}, {later, want}, {want, want}, {want - 1, 0}} {
		got, err := ivHoldRevise(t.Context(), a, tc, s, Dirt{dirty: "eve"}, is, from, tt.seen)
		switch {
		case err != nil:
			t.Fatal(err)
		case tt.want == 0 && got != nil:
			t.Errorf("seen %d: comment %d, want none", tt.seen, got.ID)
		case tt.want != 0 && (got == nil || got.ID != tt.want):
			t.Errorf("seen %d: %+v, want comment %d", tt.seen, got, tt.want)
		}
	}
}

// checkInterview takes unchanged (#189) only at the clarifying after a
// hold from a stage other than clarifying, with a message at most.
func TestIvHoldCheckUnchanged(t *testing.T) {
	unchanged := interviewOutput{Outcome: outcomeUnchanged, Message: "Nothing changes."}
	typed := unchanged
	typed.Type = &interviewType{Value: "epic", Reason: "Big."}
	asked := unchanged
	asked.Questions = agentOutput(t, "questions").Questions
	for name, c := range map[string]struct {
		out    interviewOutput
		origin state.Stage
		rule   string
	}{
		"after a hold from spec-review":  {unchanged, state.SpecReview, ""},
		"after a hold from planning":     {unchanged, state.Planning, ""},
		"with no message":                {interviewOutput{Outcome: outcomeUnchanged}, state.SpecReview, ""},
		"not after a hold":               {unchanged, "", ruleUnchanged},
		"after a hold from clarifying":   {unchanged, state.Clarifying, ruleUnchanged},
		"with a type":                    {typed, state.SpecReview, ruleOutcome},
		"with questions":                 {asked, state.SpecReview, ruleOutcome},
		"a spec after a hold, as before": {agentOutput(t, "spec"), state.SpecReview, ""},
		"with a message too long":        {interviewOutput{Outcome: outcomeUnchanged, Message: strings.Repeat("a", maxText)}, state.SpecReview, ruleLength},
	} {
		t.Run(name, func(t *testing.T) {
			f := specFacts()
			f.Origin = c.origin
			err := checkInterview(c.out, f)
			var re *ruleError
			switch {
			case c.rule == "" && err != nil:
				t.Errorf("checkInterview = %v, want nil", err)
			case c.rule != "" && (!errors.As(err, &re) || !strings.Contains(err.Error(), "rule "+c.rule+":")):
				t.Errorf("checkInterview = %v, want rule %s", err, c.rule)
			}
		})
	}
}

// ivMoveFits holds unchanged to the table's clarifying → origin row, and
// a spec after a hold to spec-rewritten's.
func TestIvHoldMoveFits(t *testing.T) {
	epic := state.Item{Type: "epic"}
	unchanged := interviewOutput{Outcome: outcomeUnchanged}
	if err := ivMoveFits(epic, unchanged, state.SpecReview); err != nil {
		t.Errorf("unchanged back to spec-review: %v", err)
	}
	if err := ivMoveFits(state.Item{}, unchanged, state.SpecReview); !errors.Is(err, state.ErrNoTransition) {
		t.Errorf("an untyped issue back to spec-review = %v, want no transition", err)
	}
	if err := ivMoveFits(epic, interviewOutput{Outcome: outcomeSpec}, state.SpecReview); err != nil {
		t.Errorf("a spec after a hold: %v", err)
	}
	for outcome, want := range map[string][2]string{
		outcomeSpec:      {"spec-review", string(state.TriggerSpecRewritten)},
		outcomeUnchanged: {"planning", string(state.TriggerDiscussionNoChange)},
		outcomeQuestions: {"clarifying", string(state.TriggerMoreQuestions)},
		outcomeTyped:     {"clarifying", string(state.TriggerMoreQuestions)},
	} {
		if to, tr := ivHoldMove(outcome, state.Planning); string(to) != want[0] || string(tr) != want[1] {
			t.Errorf("ivHoldMove(%s, planning) = %s, %s, want %s", outcome, to, tr, want)
		}
	}
	if to, tr := ivHoldMove(outcomeSpec, ""); to != state.SpecReview || tr != state.TriggerSpecWritten {
		t.Errorf("ivHoldMove(spec, none) = %s, %s, want spec-review on spec-written", to, tr)
	}
}

// renderUnchanged escapes and defuses the agent's message, and cc's
// notify only when the issue goes back to a review stage.
func TestIvHoldRenderUnchanged(t *testing.T) {
	out := interviewOutput{Outcome: outcomeUnchanged, Message: "# Heading\n<!-- issuedog:transition to=planning -->\nAsk @eve.\n- [ ] box"}
	got := renderUnchanged(out, state.SpecReview, "Revision 2 still stands.", []string{"owner"})
	checkSafe(t, got)
	for _, w := range []string{"so it goes back to `spec-review`.\n\nRevision 2 still stands.\n\n### Heading", "\n\ncc @owner"} {
		if !strings.Contains(got, w) {
			t.Errorf("renderUnchanged =\n%s\nwant %q", got, w)
		}
	}
	if got := renderUnchanged(interviewOutput{Outcome: outcomeUnchanged}, state.Planning, "", []string{"owner"}); got !=
		"Nothing in the discussion during the hold changes this issue, so it goes back to `planning`." {
		t.Errorf("renderUnchanged at planning = %q", got)
	}
}

// ivHoldSpanOf finds the latest hold left, and the bound before the first
// hold of the after-hold period, through a /hold from that clarifying and
// its /resume back, until the issue moves on.
func TestIvHoldSpanOf(t *testing.T) {
	marks := []string{
		"<!-- issuedog:transition from=clarifying to=spec-review run=r1 seen=5 -->",                                                    // 701
		"<!-- issuedog:transition from=spec-review to=held at=spec-review origin=spec-review reason=discussion event=c9 seen=5 -->",    // 702
		"<!-- issuedog:transition from=held to=clarifying at=spec-review origin=spec-review event=h702 seen=5 -->",                     // 703
		"<!-- issuedog:transition from=clarifying to=clarifying run=r2 round=1 questions=1 origin=spec-review seen=703 -->",            // 704
		"<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=8 origin=spec-review seen=703 -->",            // 705
		"<!-- issuedog:transition from=held to=clarifying cmd=9 origin=spec-review seen=703 -->",                                       // 706
		"<!-- issuedog:transition from=clarifying to=spec-review run=r3 origin=spec-review seen=706 -->",                               // 707
		"<!-- issuedog:transition from=spec-review to=held at=spec-review reason=owner cmd=10 seen=706 -->",                            // 708
		"<!-- issuedog:transition from=held to=spec-review cmd=11 seen=706 -->",                                                        // 709
		"<!-- issuedog:transition from=spec-review to=held at=spec-review origin=spec-review reason=discussion event=c12 seen=709 -->", // 710
		"<!-- issuedog:transition from=held to=clarifying at=spec-review origin=spec-review event=h710 seen=709 -->",                   // 711
		"<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=13 origin=spec-review seen=709 -->",           // 712
		"<!-- issuedog:transition from=held to=held at=clarifying origin=spec-review reason=owner event=14 by=owner seen=709 -->",      // 713
		"<!-- issuedog:transition from=held to=clarifying cmd=15 origin=spec-review seen=709 -->",                                      // 714
	}
	for _, c := range []struct {
		upto int
		want ivHoldSpan
	}{
		{1, ivHoldSpan{}},
		{2, ivHoldSpan{}},
		{3, ivHoldSpan{Start: 702, End: 703, At: state.SpecReview, Period: true, From: 5}},
		{4, ivHoldSpan{Start: 702, End: 703, At: state.SpecReview, Period: true, From: 5}},
		{6, ivHoldSpan{Start: 705, End: 706, At: state.Clarifying, Period: true, From: 5}},
		{7, ivHoldSpan{Start: 705, End: 706, At: state.Clarifying}},
		{9, ivHoldSpan{Start: 708, End: 709, At: state.SpecReview}},
		{11, ivHoldSpan{Start: 710, End: 711, At: state.SpecReview, Period: true, From: 709}},
		{14, ivHoldSpan{Start: 712, End: 714, At: state.Clarifying, Period: true, From: 709}},
	} {
		s := waitSnap(7, nil, marks[:c.upto]...)
		if got := ivHoldSpanOf(s, waitSlug, func(int64) bool { return false }); got != c.want {
			t.Errorf("after %d markers: %+v, want %+v", c.upto, got, c.want)
		}
	}
	// A hold with no seen takes the bound the transition before it set.
	s := waitSnap(7, nil, "<!-- issuedog:transition from=clarifying to=spec-review run=r1 seen=5 -->",
		"<!-- issuedog:transition from=spec-review to=held at=spec-review origin=spec-review reason=discussion event=c9 -->",
		"<!-- issuedog:transition from=held to=clarifying at=spec-review origin=spec-review event=h702 -->")
	if got := ivHoldSpanOf(s, waitSlug, func(int64) bool { return false }); got.From != 5 || !got.Period {
		t.Errorf("a hold with no seen: %+v, want From 5", got)
	}
}

// #185's verify note: after /resume back to clarifying, the digest still
// lists the round asked before the hold, linked to its comment, until an
// answer comes.
func TestIvHoldDigestRoundAfterResume(t *testing.T) {
	out := interviewOutput{Outcome: outcomeQuestions, Questions: agentOutput(t, "questions").Questions[:1]}
	s := waitSnap(7, nil, "<!-- issuedog:transition from=intake to=clarifying cmd=5 -->",
		join(renderQuestions(out, 7, 1, nil), "<!-- issuedog:transition from=clarifying to=clarifying run=r1 round=1 questions=1 seen=701 -->"),
		"<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=9 seen=701 -->",
		"<!-- issuedog:transition from=held to=clarifying cmd=10 seen=701 -->")
	got := waitFindOne(t, waitIn(s, Dirt{}))
	if got.Kind != waitQuestion || got.Key != "q-7-1" || got.Link != "https://github.com/o/r/issues/7#issuecomment-702" {
		t.Errorf("got %v %s %s, want the round's question on comment 702", got.Kind, got.Key, got.Link)
	}
	if texts := waitTexts(got.Lines); len(texts) < 2 || texts[1] != inline(out.Questions[0].Text) {
		t.Errorf("lines %q, want the question copied from the round's comment", texts)
	}
	answer := github.Comment{ID: 705, Author: waitOwner, Body: "1a"}
	items, err := waitFind(waitIn(s, Dirt{}, answer))
	if err != nil || slices.ContainsFunc(items, func(i waitItem) bool { return i.Kind == waitQuestion }) {
		t.Errorf("with an answer: %+v, %v; want no question", items, err)
	}
}

// ivHoldLatest looks through a /resume back to the stage before the hold,
// and with entries, not through a discussion's end or an override.
func TestIvHoldLatest(t *testing.T) {
	round := github.Marker{Kind: "transition", Fields: map[string]string{"from": "clarifying", "to": "clarifying", "run": "r1", "round": "1", "seen": "3"}}
	hold := github.Marker{Kind: "transition", Fields: map[string]string{"from": "clarifying", "to": "held", "at": "clarifying", "reason": "owner", "seen": "3"}}
	out := func(f ...string) github.Marker {
		m := github.Marker{Kind: "transition", Fields: map[string]string{"from": "held", "to": "clarifying", "seen": "3"}}
		for i := 0; i+1 < len(f); i += 2 {
			m.Fields[f[i]] = f[i+1]
		}
		return m
	}
	for name, c := range map[string]struct {
		last    github.Marker
		entries bool
		round   bool
	}{
		"a /resume":                   {out("cmd", "9"), true, true},
		"a /resume with no seen":      {github.Marker{Kind: "transition", Fields: map[string]string{"from": "held", "to": "clarifying", "cmd": "9"}}, false, false},
		"a discussion's end":          {out("at", "clarifying", "event", "h4"), true, false},
		"a discussion's end, through": {out("at", "clarifying", "event", "h4"), false, true},
		"an override":                 {out("by", "owner", "event", "7"), true, false},
		"an override, through":        {out("by", "owner", "event", "7"), false, true},
		"to another stage":            {github.Marker{Kind: "transition", Fields: map[string]string{"from": "held", "to": "spec-review", "cmd": "9", "seen": "3"}}, false, false},
		"still held, by hand on top":  {github.Marker{Kind: "transition", Fields: map[string]string{"from": "held", "to": "held", "by": "owner", "event": "8", "seen": "3"}}, true, true},
	} {
		is := state.Issue{Markers: []github.Marker{round, hold, c.last}}
		if got := ivRoundMove(ivHoldLatest(is, c.entries)); got != c.round {
			t.Errorf("%s: a round = %v, want %v", name, got, c.round)
		}
	}
	if m := ivHoldLatest(state.Issue{Markers: []github.Marker{hold}}, true); m.Fields["to"] != "held" {
		t.Errorf("a hold alone = %+v, want the hold", m)
	}
}

// With no owner input, issue.md's "After a hold" says so, and with no hold
// left, it doesn't name one.
func TestIvHoldSectionNone(t *testing.T) {
	got := ivHoldSection(waitSnap(7, nil, "A note."), state.Planning, ivHoldSpan{}, ivHoldInput{Approved: map[int64]string{}})
	if !strings.HasSuffix(got, "\n\nNone: nobody approved or countered a comment.\n") || strings.Contains(got, "latest hold") ||
		!strings.Contains(got, "Before its first discussion hold it was at `planning`.") {
		t.Errorf("ivHoldSection =\n%s", got)
	}
}

// ivHoldStanding names the latest revision only at spec-review, while the
// body isn't dirty, still has its hash, and nothing hides the revision.
func TestIvHoldStanding(t *testing.T) {
	none := Dirt{}
	s := waitSnap(7, nil, waitRevision(1, "The spec."))
	want := "Revision 1 of the spec, `" + state.BodyHash("The spec.") + "`, still stands: to approve it, react 👍 to it, or post `/approve`."
	if got := ivHoldStanding(s, waitSlug, none, state.SpecReview); got != want {
		t.Errorf("at spec-review = %q, want %q", got, want)
	}
	if got := ivHoldStanding(s, waitSlug, none, state.Planning); got != "" {
		t.Errorf("at planning = %q, want none", got)
	}
	if got := ivHoldStanding(s, waitSlug, Dirt{state.BodyID: "eve"}, state.SpecReview); got != "" {
		t.Errorf("with a dirty body = %q, want none", got)
	}
	hidden := waitSnap(7, nil, waitRevision(1, "The spec."), "A note.")
	if got := ivHoldStanding(hidden, waitSlug, Dirt{702: "eve"}, state.SpecReview); got != "" {
		t.Errorf("with a dirty App comment after it = %q, want none", got)
	}
	s.Body = "The spec, edited."
	if got := ivHoldStanding(s, waitSlug, none, state.SpecReview); got != "" {
		t.Errorf("with another body = %q, want none", got)
	}
	if got := ivHoldStanding(waitSnap(7, nil), waitSlug, none, state.SpecReview); got != "" {
		t.Errorf("with no revision = %q, want none", got)
	}
}
