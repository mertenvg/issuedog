package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
// trusted, no bot, that a trusted user, no bot, 👍'd, naming the first
// trusted user to 👍 it, and counters only a trusted comment after an
// untrusted one, not a /hold or a /resume. It reads the reactions of the
// comments it may approve, and no others.
func TestIvHoldInputOf(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	before := f.AddComment(n, "mallory", "before the discussion")
	f.AddReaction(before, "owner", "+1")
	from := f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=spec-review to=held at=spec-review reason=discussion event=c1 -->")
	priority := f.AddComment(n, "owner", "/priority p1")
	approved := f.AddComment(n, "mallory", "approve me")
	f.AddReaction(approved, "eve", "+1")
	f.AddReaction(approved, "owner", "+1")
	f.AddReaction(approved, "alice", "+1") // trusted too, but after owner
	eve := f.AddComment(n, "eve", "me too")
	f.AddReaction(eve, "eve", "+1")
	f.AddReaction(eve, "owner", "-1")
	hearted := f.AddComment(n, "mallory", "heart me")
	f.AddReaction(hearted, "owner", "heart")
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
	tc := trustCache{st: &Step{Applier: a, Trust: ivHoldTestTrust{"owner": true, "alice": true}}, verdicts: map[string]bool{}}
	reqs := len(f.Requests())
	in, err := ivHoldInputOf(t.Context(), a, tc, s, Dirt{dirty: "eve"}, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Approved) != 1 || in.Approved[approved] != "owner" {
		t.Errorf("approved %v, want only %d, by owner, its first trusted 👍, not alice", in.Approved, approved)
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
	want := []string{fmt.Sprintf("/issues/comments/%d/reactions", approved), fmt.Sprintf("/issues/comments/%d/reactions", eve),
		fmt.Sprintf("/issues/comments/%d/reactions", hearted)}
	if !slices.Equal(read, want) {
		t.Errorf("read %q, want %q", read, want)
	}
	// The bound itself is out: a comment at from isn't the discussion's.
	at, err := ivHoldInputOf(t.Context(), a, tc, s, Dirt{dirty: "eve"}, before)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := at.Approved[before]; ok || at.Approved[approved] != "owner" {
		t.Errorf("from comment %d: approved %v, want only %d", before, at.Approved, approved)
	}
}

// A 👍 approves only the text it post-dates: one in the second of the
// comment's last edit came before it, and one between two edits came
// before the last, so neither approves; nor does one before an edit the
// App recorded as a write collaborator's trigger and a /resume accepted,
// as accepting a trigger lets the issue leave its hold but doesn't make
// the edit owner input (#189's verify, attempt 3). One after the last
// edit approves the comment as edited.
func TestIvHoldInputOfEdits(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	from := f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=spec-review to=held at=spec-review reason=discussion event=c1 -->")
	same := f.AddComment(n, "mallory", "same second")
	f.AddReaction(same, "owner", "+1")
	f.EditComment(same, "mallory", "same second, edited")
	between := f.AddComment(n, "mallory", "edited once")
	f.EditComment(between, "mallory", "edited twice")
	f.AddReaction(between, "owner", "+1")
	f.EditComment(between, "mallory", "edited three times")
	recorded := f.AddComment(n, "mallory", "recorded")
	f.AddReaction(recorded, "owner", "+1")
	f.EditComment(recorded, "mallory", "recorded, edited")
	after := f.AddComment(n, "mallory", "approved after")
	f.EditComment(after, "mallory", "approved after, edited")
	f.AddReaction(after, "owner", "+1")
	c := f.Client(t)
	s, err := c.Snapshot(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	var edited time.Time
	for _, cm := range s.Comments {
		if cm.ID == recorded {
			edited = cm.Edit.At
		}
	}
	if edited.IsZero() {
		t.Fatalf("comment %d has no edit", recorded)
	}
	// The record of recorded's last edit, then a /resume out of held: what
	// the dropped exception took as an accepted trigger.
	f.AddComment(n, "issuedog[bot]", "A notice.\n\n<!-- issuedog:notice key="+discEditKey(recorded, edited)+" -->")
	f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=held to=clarifying at=spec-review origin=spec-review cmd=99 seen=1 -->")
	a := &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}
	if s, err = c.Snapshot(t.Context(), "o", "r", n); err != nil {
		t.Fatal(err)
	}
	if records := discRecords(s, a.AppSlug, func(int64) bool { return false }); records[discEditKey(recorded, edited)] == 0 {
		t.Fatalf("records %v, want one of comment %d's last edit", records, recorded)
	}
	rs, err := c.Reactions(t.Context(), "o", "r", same)
	if err != nil || len(rs) != 1 {
		t.Fatalf("reactions on %d: %v, %v", same, rs, err)
	}
	for i := range s.Comments {
		if s.Comments[i].ID == same {
			s.Comments[i].Edit.At = rs[0].CreatedAt // in the 👍's second
		}
	}
	tc := trustCache{st: &Step{Applier: a, Trust: ivHoldTestTrust{"owner": true}}, verdicts: map[string]bool{}}
	in, err := ivHoldInputOf(t.Context(), a, tc, s, Dirt{}, from)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Approved) != 1 || in.Approved[after] != "owner" {
		t.Errorf("approved %v, want only %d, by owner, not %d, %d or %d", in.Approved, after, same, between, recorded)
	}
}

// ivHoldRevise takes the oldest clean, trusted /revise above from and not
// above seen, that passes its check at the stage the issue came from, no
// marker names and the App hasn't reacted to: at the clarifying after a
// hold from spec-review or, since #294, plan-review. After a hold from
// planning, where no /revise applies, or with no hold, it takes none.
func TestIvHoldReviseFind(t *testing.T) {
	t.Parallel()
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
	for _, origin := range []state.Stage{state.SpecReview, state.PlanReview, state.Planning, ""} {
		is := state.Issue{Stage: state.Clarifying, Keys: map[state.Key]bool{state.CmdKey(named): true}}
		if origin != "" {
			is.Markers = []github.Marker{{Kind: "transition", Fields: map[string]string{"from": "held", "to": "clarifying", "origin": string(origin)}}}
		}
		for _, tt := range []struct{ seen, want int64 }{{0, want}, {later, want}, {want, want}, {want - 1, 0}} {
			if origin != state.SpecReview && origin != state.PlanReview {
				tt.want = 0
			}
			got, err := ivHoldRevise(t.Context(), a, tc, s, Dirt{dirty: "eve"}, is, from, tt.seen)
			switch {
			case err != nil:
				t.Fatal(err)
			case tt.want == 0 && got != nil:
				t.Errorf("origin %q, seen %d: comment %d, want none", origin, tt.seen, got.ID)
			case tt.want != 0 && (got == nil || got.ID != tt.want):
				t.Errorf("origin %q, seen %d: %+v, want comment %d", origin, tt.seen, got, tt.want)
			}
		}
	}
}

// checkInterview takes unchanged (#189) only at the clarifying after a
// hold from a stage other than clarifying, with a message at most.
func TestIvHoldCheckUnchanged(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	got := ivHoldSection(waitSnap(7, nil, "A note."), state.Planning, ivHoldSpan{}, ivHoldInput{Approved: map[int64]string{}})
	if !strings.HasSuffix(got, "\n\nNone: nobody approved or countered a comment.\n") || strings.Contains(got, "latest hold") ||
		!strings.Contains(got, "Before its first discussion hold it was at `planning`.") {
		t.Errorf("ivHoldSection =\n%s", got)
	}
}

// ivHoldStanding names the latest spec revision only at spec-review, while
// the body isn't dirty, still has its hash, and nothing hides the revision.
func TestIvHoldStanding(t *testing.T) {
	t.Parallel()
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
	// A plan revision is never the spec revision that stands (plan #247,
	// assumption 2), after it or alone.
	plan := waitPlanRevision(t, 2, "### Plan for #7 (rev 2)")
	if got := ivHoldStanding(waitSnap(7, nil, waitRevision(1, "The spec."), plan), waitSlug, none, state.SpecReview); got != want {
		t.Errorf("with a plan revision after it = %q, want %q", got, want)
	}
	if got := ivHoldStanding(waitSnap(7, nil, plan), waitSlug, none, state.SpecReview); got != "" {
		t.Errorf("with only a plan revision = %q, want none", got)
	}
}

// At plan-review, ivHoldStanding names the latest plan revision while
// state.PlanApproval gives no reason it can't be approved (#271): never
// the spec revision, and nothing once the plan is hidden, its comment no
// longer matches its hash, or the body is dirty.
func TestIvHoldStandingPlan(t *testing.T) {
	t.Parallel()
	none := Dirt{}
	plan := waitPlanRevision(t, 2, "### Plan for #7 (rev 2)")
	h, ok := state.PlanHash(plan)
	if !ok {
		t.Fatalf("no plan hash in %q", plan)
	}
	s := waitSnap(7, nil, waitRevision(1, "The spec."), plan)
	want := "Revision 2 of the plan, `" + h + "`, still stands: to approve it, react 👍 to it, or post `/approve`."
	if got := ivHoldStanding(s, waitSlug, none, state.PlanReview); got != want {
		t.Errorf("at plan-review = %q, want %q", got, want)
	}
	// A trusted edit to the body leaves the plan standing (#248's Q2a).
	s.Body = "The spec, edited."
	if got := ivHoldStanding(s, waitSlug, none, state.PlanReview); got != want {
		t.Errorf("with another body = %q, want %q", got, want)
	}
	if got := ivHoldStanding(s, waitSlug, Dirt{state.BodyID: "eve"}, state.PlanReview); got != "" {
		t.Errorf("with a dirty body = %q, want none", got)
	}
	hidden := waitSnap(7, nil, waitRevision(1, "The spec."), plan, "A note.")
	if got := ivHoldStanding(hidden, waitSlug, Dirt{703: "eve"}, state.PlanReview); got != "" {
		t.Errorf("with a dirty App comment after it = %q, want none", got)
	}
	changed := waitSnap(7, nil, waitRevision(1, "The spec."), strings.Replace(plan, "(rev 2)", "(rev 2, edited)", 1))
	if got := ivHoldStanding(changed, waitSlug, none, state.PlanReview); got != "" {
		t.Errorf("with the plan comment edited = %q, want none", got)
	}
	if got := ivHoldStanding(waitSnap(7, nil, waitRevision(1, "The spec.")), waitSlug, none, state.PlanReview); got != "" {
		t.Errorf("with only a spec revision = %q, want none", got)
	}
}

// checkInterview takes plan (#294) only at the clarifying after a hold
// from planning, plan-review or planned, on an epic or a story, with a
// message and nothing else. On a story with a parent (#195's Q1a), it
// refuses a spec and a typed outcome.
func TestIvHoldCheckPlan(t *testing.T) {
	t.Parallel()
	plan := agentOutput(t, "plan")
	typed := plan
	typed.Type = &interviewType{Value: "epic", Reason: "Big."}
	asked := plan
	asked.Questions = agentOutput(t, "questions").Questions
	for name, c := range map[string]struct {
		out    interviewOutput
		origin state.Stage
		class  state.Class
		rule   string
	}{
		"an epic after a hold from plan-review":            {plan, state.PlanReview, state.ClassEpic, ""},
		"an epic after a hold from planning":               {plan, state.Planning, state.ClassEpic, ""},
		"an epic after a hold from planned":                {plan, state.Planned, state.ClassEpic, ""},
		"a story with no parent":                           {plan, state.PlanReview, state.ClassOwnerStory, ""},
		"a story with a parent":                            {plan, state.Planned, state.ClassStory, ""},
		"after a hold from spec-review":                    {plan, state.SpecReview, state.ClassEpic, rulePlan},
		"after a hold from clarifying":                     {plan, state.Clarifying, state.ClassEpic, rulePlan},
		"not after a hold":                                 {plan, "", state.ClassEpic, rulePlan},
		"an untyped issue":                                 {plan, state.PlanReview, state.ClassUntyped, rulePlan},
		"a task":                                           {plan, state.Planning, state.ClassTask, rulePlan},
		"with no message":                                  {interviewOutput{Outcome: outcomePlan}, state.PlanReview, state.ClassEpic, ruleOutcome},
		"with a blank message":                             {interviewOutput{Outcome: outcomePlan, Message: " \n"}, state.PlanReview, state.ClassEpic, ruleOutcome},
		"with a type":                                      {typed, state.PlanReview, state.ClassEpic, ruleOutcome},
		"with questions":                                   {asked, state.PlanReview, state.ClassEpic, ruleOutcome},
		"with a message too long":                          {interviewOutput{Outcome: outcomePlan, Message: strings.Repeat("a", maxText)}, state.PlanReview, state.ClassEpic, ruleLength},
		"a spec on a story with a parent":                  {agentOutput(t, "spec"), state.Planned, state.ClassStory, ruleParentStory},
		"typed on a story with a parent":                   {agentOutput(t, "typed"), state.Planned, state.ClassStory, ruleParentStory},
		"unchanged on a story with a parent":               {interviewOutput{Outcome: outcomeUnchanged}, state.Planned, state.ClassStory, ""},
		"a spec on a story with no parent, as before":      {agentOutput(t, "spec"), state.SpecReview, state.ClassOwnerStory, ""},
		"unchanged on an epic from plan-review, as before": {interviewOutput{Outcome: outcomeUnchanged}, state.PlanReview, state.ClassEpic, ""},
	} {
		t.Run(name, func(t *testing.T) {
			f := specFacts()
			if c.out.Outcome == outcomeTyped {
				f.MayType = true
			}
			f.Origin, f.Class = c.origin, c.class
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

// ivMoveFits holds plan to the table's clarifying → planning row on
// plan-changed, which only an epic or a story has (#294), and a round on a
// story with a parent to the clarifying after a hold from a stage of its
// plan (#195's Q1a).
func TestIvHoldMoveFitsPlan(t *testing.T) {
	t.Parallel()
	if to, tr := ivHoldMove(outcomePlan, state.PlanReview); to != state.Planning || tr != state.TriggerPlanChanged {
		t.Errorf("ivHoldMove(plan, plan-review) = %s, %s, want planning on plan-changed", to, tr)
	}
	plan := interviewOutput{Outcome: outcomePlan, Message: "x"}
	story := state.Item{Type: "story", HasParent: true}
	for _, it := range []state.Item{{Type: "epic"}, {Type: "story"}, story} {
		if err := ivMoveFits(it, plan, state.PlanReview); err != nil {
			t.Errorf("plan on %+v: %v", it, err)
		}
	}
	for _, it := range []state.Item{{}, {Type: "task"}, {Type: "bug"}} {
		if err := ivMoveFits(it, plan, state.PlanReview); !errors.Is(err, state.ErrNoTransition) {
			t.Errorf("plan on %+v = %v, want no transition", it, err)
		}
	}
	round := interviewOutput{Outcome: outcomeQuestions}
	for origin, ok := range map[state.Stage]bool{state.Planning: true, state.PlanReview: true, state.Planned: true, state.Clarifying: false, "": false} {
		if err := ivMoveFits(story, round, origin); (err == nil) != ok {
			t.Errorf("a round on a story with a parent after a hold from %q = %v, want ok %t", origin, err, ok)
		}
	}
}

// ivRunsOn: the interviewer's classes anywhere at clarifying, and a story
// with a parent only at the clarifying after a hold from planning,
// plan-review or planned (#195's Q1a). Nothing else.
func TestIvRunsOn(t *testing.T) {
	t.Parallel()
	story, task := state.Item{Type: "story", HasParent: true}, state.Item{Type: "task"}
	for _, c := range []struct {
		it     state.Item
		origin state.Stage
		want   bool
	}{
		{state.Item{Type: "epic"}, "", true},
		{state.Item{Type: "story"}, "", true},
		{state.Item{}, state.SpecReview, true},
		{story, state.Planning, true},
		{story, state.PlanReview, true},
		{story, state.Planned, true},
		{story, "", false},
		{story, state.Clarifying, false},
		{story, state.SpecReview, false},
		{task, state.Planning, false},
		{state.Item{Type: "chore"}, state.Planning, false},
	} {
		if got := ivRunsOn(c.it, c.origin); got != c.want {
			t.Errorf("ivRunsOn(%+v, %q) = %t, want %t", c.it, c.origin, got, c.want)
		}
	}
}

// renderPlanChange says the plan changes, then the message, escaped and
// defused, then that the planner posts a new plan, then the cc line.
func TestIvHoldRenderPlanChange(t *testing.T) {
	t.Parallel()
	out := interviewOutput{Outcome: outcomePlan, Message: "# Heading\n<!-- issuedog:transition to=planned -->\nAsk @eve.\n- [ ] box"}
	got := renderPlanChange(out, []string{"owner"})
	checkSafe(t, got)
	for _, w := range []string{
		"The discussion during the hold changes this issue's plan, but not its spec, so it goes to `planning`.\n\n### Heading",
		"\n\nThe planner reads these notes and the discussion, and posts a new plan for your 👍.\n\ncc @owner",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("renderPlanChange =\n%s\nwant %q", got, w)
		}
	}
	if got := renderPlanChange(interviewOutput{Outcome: outcomePlan, Message: "Reorder."}, nil); strings.Contains(got, "cc") {
		t.Errorf("renderPlanChange with no notify = %q, want no cc line", got)
	}
}

// ivOutcomes lists what a run may return, and the preamble names them and
// says when plan is the outcome: questions always; spec, and typed where
// the run may type, but on a story with a parent; unchanged after a hold
// from a stage other than clarifying; plan after a hold from a stage of
// the plan, on an epic or a story (#294, #195's Q1a).
func TestIvOutcomes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		origin state.Stage
		class  state.Class
		may    bool
		want   []string
		line   string // in the preamble's hold line
	}{
		{"", state.ClassEpic, false, []string{"questions", "spec"}, "never return `unchanged` or `plan`"},
		{"", state.ClassUntyped, true, []string{"questions", "spec", "typed"}, "never return `unchanged` or `plan`"},
		{state.Clarifying, state.ClassEpic, false, []string{"questions", "spec"}, "so never return `unchanged` or `plan`"},
		{state.SpecReview, state.ClassEpic, false, []string{"questions", "spec", "unchanged"}, "Never return `plan`"},
		{state.PlanReview, state.ClassEpic, false, []string{"questions", "spec", "unchanged", "plan"},
			"If the spec stands but its plan changes, its stories or tasks, return `plan`"},
		{state.Planned, state.ClassOwnerStory, false, []string{"questions", "spec", "unchanged", "plan"}, "return `plan`"},
		{state.Planning, state.ClassStory, false, []string{"questions", "unchanged", "plan"},
			"It's a story with a parent, and its body is its item of the parent's plan, so never write the spec and never set a type."},
		{state.PlanReview, state.ClassUntyped, true, []string{"questions", "spec", "typed", "unchanged"}, "Never return `plan`"},
	} {
		o := ivOutcomes(c.origin, c.class, c.may)
		if !slices.Equal(o.outcomes, c.want) {
			t.Errorf("ivOutcomes(%q, %s, %t) = %q, want %q", c.origin, c.class, c.may, o.outcomes, c.want)
		}
		quoted := make([]string, len(c.want))
		for i, w := range c.want {
			quoted[i] = "`" + w + "`"
		}
		p := interviewerPreamble(4, nil, nil, nil, "typing", o)
		for _, w := range []string{"- The outcomes you may return here: " + list(quoted) + ". Any other outcome fails the run.", c.line} {
			if !strings.Contains(p, w) {
				t.Errorf("origin %q, class %s: the preamble lacks %q:\n%s", c.origin, c.class, w, p)
			}
		}
	}
}
