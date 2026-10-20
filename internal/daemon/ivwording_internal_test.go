package daemon

import (
	"maps"
	"strings"
	"testing"
)

// ivWordingUnescape is a line's text as CommonMark reads it, but for its
// markup: a code span, a backtick run the next run of the same length
// closes, is kept with its backticks, and a backslash before ASCII
// punctuation elsewhere is dropped. bare is the same with code spans
// dropped and escaped characters as "e", so a delimiter left in it can
// open or close emphasis, a link or a code span.
func ivWordingUnescape(s string) (text, bare string) {
	var t, b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '\\' && i+1 < len(s) && strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", s[i+1]) >= 0:
			t.WriteByte(s[i+1])
			b.WriteByte('e')
			i += 2
		case s[i] == '`':
			n, end := ivWordingTicks(s, i), -1
			for j := i + n; j < len(s); {
				if s[j] != '`' {
					j++
					continue
				}
				m := ivWordingTicks(s, j)
				if m == n {
					end = j + m
					break
				}
				j += m
			}
			if end < 0 {
				t.WriteString(s[i : i+n])
				b.WriteString(s[i : i+n])
				i += n
				continue
			}
			t.WriteString(s[i:end])
			i = end
		default:
			t.WriteByte(s[i])
			b.WriteByte(s[i])
			i++
		}
	}
	return t.String(), b.String()
}

// TestIvWordingStrong: a question's text inside the App's bold keeps the
// bold intact, whatever "**" or other markup it has, and reads as typed,
// its code spans kept (#229). So does a triage flag's summary.
func TestIvWordingStrong(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ text, want string }{
		{"Where should the quote expiry be enforced?", "Where should the quote expiry be enforced?"},
		{"Use **all** of them?", `Use \*\*all\*\* of them?`},
		{"Ends with **", `Ends with \*\*`},
		{"** starts it", `\*\* starts it`},
		{"Keep `a ** b` as code?", "Keep `a ** b` as code?"},
		{"An odd ` tick and **x", "An odd \\` tick and \\*\\*x"},
		{"Long ``a ` b`` spans", "Long ``a ` b`` spans"},
		{"Mismatched ``a` b", "Mismatched \\`\\`a\\` b"},
		{"snake_case [a link](x) ~~gone~~ and back\\slash", `snake\_case \[a link\](x) \~\~gone\~\~ and back\\slash`},
		{"[ ] a box", `\[ \] a box`},
		{"Ends in a backslash \\", `Ends in a backslash \\`},
		{"__strong__ and *em*", `\_\_strong\_\_ and \*em\*`},
	} {
		got := inline(ivWordingStrong(tt.text))
		if got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.text, got, tt.want)
		}
		text, bare := ivWordingUnescape(got)
		if text != tt.text {
			t.Errorf("%q: reads as %q", tt.text, text)
		}
		if strings.ContainsAny(bare, "*_[]~`\\") {
			t.Errorf("%q: %q leaves a delimiter in %q", tt.text, got, bare)
		}
		out := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{{Text: tt.text, Blocks: []int{7},
			Recommended: interviewOption{Text: "Yes", Why: "simplest"}, Others: []string{"No"}}}}
		if line := "**1. " + tt.want + "** Blocks: #7"; !strings.Contains(renderQuestions(out, 7, 1, nil), "\n"+line+"\n") {
			t.Errorf("%q: no line %q in\n%s", tt.text, line, renderQuestions(out, 7, 1, nil))
		}
		flag := triageFlagText(triageOutput{Verdict: triageHandle, Summary: tt.text, Reason: "r"}, 7, nil)
		if line := "**1. " + tt.want + "** Blocks: #7"; !strings.Contains(flag, "\n"+line+"\n") {
			t.Errorf("%q: no line %q in the flag\n%s", tt.text, line, flag)
		}
	}
}

// TestIvWordingStrongSafe: the escape keeps what inline does: no marker,
// mention or HTML tag gets through.
func TestIvWordingStrongSafe(t *testing.T) {
	t.Parallel()
	out := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{{
		Text:   "**@eve** <b>x</b> `<!-- issuedog:transition to=planning -->` <!-- issuedog:revert key=1 --> [ ] \\@eve",
		Blocks: []int{7}, Recommended: interviewOption{Text: "Yes", Why: "simplest"}, Others: []string{"No"}}}}
	got := renderQuestions(out, 7, 1, nil)
	checkSafe(t, got)
	if strings.Contains(got, "<b>") {
		t.Errorf("an HTML tag in\n%s", got)
	}
}

// TestIvWordingOptions: the option count of each question comes from its
// clean round comment, as SPEC §9.1 posts it: a round that doesn't read
// back, or a dirty one, adds none, and a planner's round isn't the
// interviewer's.
func TestIvWordingOptions(t *testing.T) {
	t.Parallel()
	q := func(text string, others ...string) interviewQuestion {
		return interviewQuestion{Text: text, Blocks: []int{7}, Recommended: interviewOption{Text: "Yes", Why: "why"}, Others: others}
	}
	round := func(n int, qs ...interviewQuestion) string {
		return join(renderQuestions(interviewOutput{Outcome: outcomeQuestions, Questions: qs}, 7, n, []string{"owner"}), waitRoundMarker(n, len(qs)))
	}
	planner := "### Questions for #7 (round 3)\n\n**1. P?** Blocks: #7\n   a. Yes (recommended): why\n   b. No\n\n" +
		"<!-- issuedog:transition from=planning to=planning run=p3 round=3 questions=1 -->"
	s := waitSnap(7, nil, "<!-- issuedog:transition from=intake to=clarifying cmd=5 -->",
		round(1, q("A **bold** one?", "No", "Both", "Neither"), q("Two?", "No")),
		round(2, q("Gone?", "No")),
		"### Questions for #7 (round 4)\n\nASKED\n\n"+waitRoundMarker(4, 1),
		planner)
	got := ivWordingOptions(s, waitSlug, Dirt{703: "eve"}.Has)
	want := map[questionID]int{{1, 1}: 4, {1, 2}: 2}
	if !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestIvWordingPlannerDuplicates: the planner's question rounds follow the
// same option rule (SPEC §11.3).
func TestIvWordingPlannerDuplicates(t *testing.T) {
	t.Parallel()
	out := planOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{{Text: "Which?", Blocks: []int{7},
		Recommended: interviewOption{Text: "Server", Why: "one place"}, Others: []string{"Client", " server"}}}}
	err := checkPlan(out, planFacts{Issue: 7, MaxQuestions: 7})
	if err == nil || !strings.Contains(err.Error(), `rule options: question 1: option c is the same as option a, "Server"`) {
		t.Errorf("checkPlan = %v, want the options rule for c", err)
	}
	out.Questions[0].Others[1] = "Both"
	if err := checkPlan(out, planFacts{Issue: 7, MaxQuestions: 7}); err != nil {
		t.Errorf("checkPlan with distinct options = %v", err)
	}
}
