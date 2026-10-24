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

// TestIvWordingStrongURL: a bare URL in a question's text, or a triage
// flag's summary, keeps its "_" and "~", so GitHub links it whole, as far
// as GFM's link would run. Its "*", "\", "[" and "]" stay escaped, and so
// does the text around it, a URL GFM doesn't link, one in a code span, and
// trailing punctuation GFM leaves out of the link (#399).
func TestIvWordingStrongURL(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ text, want string }{
		{"See https://example.com/a_b_c for it", "See https://example.com/a_b_c for it"},
		{"See https://example.com/~eve/x~y for it", "See https://example.com/~eve/x~y for it"},
		{"HTTP://example.com/a_b and snake_case", `HTTP://example.com/a_b and snake\_case`},
		{"www.example.com/a_b~c, then ~~gone~~", `www.example.com/a_b~c, then \~\~gone\~\~`},
		{"Is it *https://example.com/a_b*?", `Is it \*https://example.com/a_b\*?`},
		{"Is it https://example.com/a*b*c?", `Is it https://example.com/a\*b\*c?`},
		{"Is it https://example.com/a\\b?", `Is it https://example.com/a\\b?`},
		{"Is it https://example.com/a[1]_b?", `Is it https://example.com/a\[1\]_b?`},
		{"https://example.com/x_**gone**", `https://example.com/x_\*\*gone\*\*`},
		{"Is it https://example.com/x_?", `Is it https://example.com/x\_?`},
		{"Is it https://example.com/x~.", `Is it https://example.com/x\~.`},
		{"(https://example.com/a_(b)_c_)", `(https://example.com/a_(b)_c\_)`},
		{"See https://example.com/a_&hl; next", `See https://example.com/a\_&hl; next`},
		{"See https://example.com/a_b;", `See https://example.com/a_b;`},
		{"xhttps://example.com/a_b", `xhttps://example.com/a\_b`},
		{"see https:// a_b", `see https:// a\_b`},
		{"Keep `https://example.com/a_b` as code, and a_b", "Keep `https://example.com/a_b` as code, and a\\_b"},
		{"https://example.com/a_b<i>_x", `https://example.com/a_b&lt;i>\_x`},
	} {
		got := inline(ivWordingStrong(tt.text))
		if got != tt.want {
			t.Errorf("%q: got %q, want %q", tt.text, got, tt.want)
		}
		if text, bare := ivWordingUnescape(got); strings.ReplaceAll(text, "&lt;", "<") != tt.text || strings.ContainsAny(bare, "*[]`\\") {
			t.Errorf("%q: %q reads as %q, and leaves a delimiter other than a URL's in %q", tt.text, got, text, bare)
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

// TestIvWordingURL: a bare URL runs as GFM's link does, from its start to
// a space, a control character or "<", less trailing punctuation, and
// starts only where GFM's can. Nothing after a start is no URL (#399).
func TestIvWordingURL(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		s    string
		at   int
		want string // the URL, or "" for none
	}{
		{"https://e.com/a_b c", 0, "https://e.com/a_b"},
		{"see www.e.com/a~b.", 4, "www.e.com/a~b"},
		{"see HTTPS://e.com/a\tb", 4, "HTTPS://e.com/a"},
		{"see http://e.com/a\x01b", 4, "http://e.com/a"},
		{"(https://e.com/a_(b))", 1, "https://e.com/a_(b)"},
		{"https://e.com/a&amp;", 0, "https://e.com/a"},
		{"https://e.com/a;", 0, "https://e.com/a"},
		{"https://e.com/a<b", 0, "https://e.com/a"},
		{"https://", 0, ""},
		{"https://_.", 0, ""},
		{"ftp://e.com/a", 0, ""},
		{"xhttps://e.com/a", 1, ""},
		{"~https://e.com/a", 1, "https://e.com/a"},
		{"a\thttps://e.com/b", 2, "https://e.com/b"},
	} {
		got := ""
		if end := ivWordingURL(tt.s, tt.at); end > 0 {
			got = tt.s[tt.at:end]
		}
		if got != tt.want {
			t.Errorf("ivWordingURL(%q, %d) is %q, want %q", tt.s, tt.at, got, tt.want)
		}
	}
}

// TestIvWordingStrongURLSafe: a bare URL is escaped as the rest of the
// text is but for its "_" and "~", so no marker, mention or HTML tag gets
// through in it or around it (#399).
func TestIvWordingStrongURLSafe(t *testing.T) {
	t.Parallel()
	text := "https://example.com/@eve_x/<!--_issuedog:transition_to=planning_--> https://e.com/a_<b>x</b>_ " +
		"www.example.com/a_&#64;eve_<!-- issuedog:revert key=1 -->"
	out := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{{Text: text,
		Blocks: []int{7}, Recommended: interviewOption{Text: "Yes", Why: "simplest"}, Others: []string{"No"}}}}
	for _, got := range []string{renderQuestions(out, 7, 1, nil), triageFlagText(triageOutput{Verdict: triageHandle, Summary: text, Reason: "r"}, 7, nil)} {
		checkSafe(t, got)
		if strings.Contains(got, "<b>") || strings.Contains(got, "<!--_") {
			t.Errorf("an HTML tag or comment in\n%s", got)
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
