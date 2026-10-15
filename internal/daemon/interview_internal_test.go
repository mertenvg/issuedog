package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/interview")

// agentOutput decodes internal/agent's golden interviewer output name.
func agentOutput(t *testing.T, name string) interviewOutput {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "outputs", "interviewer", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeInterview(raw)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// golden compares got with testdata/interview/<name>.md, or writes it there
// with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "interview", name+".md")
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

// specFacts are the facts agentOutput("spec") was written for.
func specFacts() interviewFacts {
	return interviewFacts{Issue: 123, MaxQuestions: 7, Rounds: []int{2, 1}, Trusted: []string{"mertenvg", "merten_acme"}, Notify: []string{"mertenvg"}}
}

// questionFacts are the facts agentOutput("questions") was written for.
func questionFacts() interviewFacts {
	return interviewFacts{Issue: 123, MaxQuestions: 7, MayType: true, Untyped: true, Notify: []string{"mertenvg"}}
}

var mentionAnyRE = regexp.MustCompile(`@[A-Za-z0-9]`)

// checkSafe fails unless text has no marker, task list box or mention,
// except a cc line at the end and a mention in a code fence.
func checkSafe(t *testing.T, text string) {
	t.Helper()
	if i := strings.LastIndex(text, "\n\ncc @"); i >= 0 {
		text = text[:i]
	}
	if strings.Contains(text, "<!--") {
		t.Errorf("unescaped <!-- in\n%s", text)
	}
	if m := mentionAnyRE.FindString(strings.Join(unfenced(text), "\n")); m != "" {
		t.Errorf("mention %q in\n%s", m, text)
	}
	if regexp.MustCompile(`(?m)^\s*([-*+]|\d+[.)])\s+\[[ xX]\]`).MatchString(text) {
		t.Errorf("task list box in\n%s", text)
	}
}

// unfenced is text's lines outside code fences.
func unfenced(text string) []string {
	var lines []string
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		switch t := strings.TrimSpace(line); {
		case fence != "":
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
		case strings.HasPrefix(t, "```"), strings.HasPrefix(t, "~~~"):
			fence = t[:3]
		default:
			lines = append(lines, line)
		}
	}
	return lines
}

// headings are text's # and ## headings outside code fences.
func headings(text string) []string {
	var found []string
	for _, line := range unfenced(text) {
		if regexp.MustCompile(`^ {0,3}#{1,2}(?:[ \t]|$)`).MatchString(line) {
			found = append(found, line)
		}
	}
	return found
}

var specHeadings = []string{"## Problem", "## Goals", "## Out of scope", "## Users and flows", "## Constraints", "## Acceptance criteria", "## Decisions"}

// The §9.1 example renders exactly, with its elided second question left
// out and the cc line added.
func TestRenderQuestionsSpecExample(t *testing.T) {
	spec, err := os.ReadFile(filepath.Join("..", "..", "SPEC.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, after, _ := strings.Cut(string(spec), "### 9.1 Questions")
	_, after, _ = strings.Cut(after, "```\n")
	example, _, _ := strings.Cut(after, "\n```")
	want := strings.Replace(example, "**2. ...**\n\n", "", 1) + "\n\ncc @mertenvg"
	out := interviewOutput{Outcome: outcomeQuestions, Questions: []interviewQuestion{{
		Text:        "Where should the quote expiry be enforced?",
		Blocks:      []int{130, 131},
		Recommended: interviewOption{Text: "Server-side on booking", Why: "one place, covers PWA and MCP"},
		Others:      []string{"Client-side countdown only", "Both"},
	}}}
	got := renderQuestions(out, 123, 2, []string{"mertenvg"})
	if !strings.Contains(example, "Server-side on booking (recommended)") || got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	golden(t, "questions-spec-example", got)
}

// A round that sets a type starts with the type line.
func TestRenderQuestionsTyped(t *testing.T) {
	out := agentOutput(t, "questions")
	got := renderQuestions(out, 123, 1, []string{"mertenvg", "merten_acme"})
	if !strings.HasPrefix(got, "Type: epic, because a large feature") {
		t.Errorf("no type line first:\n%s", got)
	}
	if !strings.Contains(got, "\n   a. Kept, marked expired (recommended): reports need them\n   b. Deleted after 30 days") {
		t.Errorf("recommended option isn't first:\n%s", got)
	}
	golden(t, "questions-typed", got)
	golden(t, "typed", renderQuestions(agentOutput(t, "typed"), 123, 1, []string{"mertenvg"}))
}

// Agent text in a round can't post a marker or mention anyone.
func TestRenderQuestionsHostile(t *testing.T) {
	out := interviewOutput{Outcome: outcomeQuestions, Type: &interviewType{Value: "epic @eve <b>", Reason: "@eve said so &#64;eve &commat;eve"},
		Questions: []interviewQuestion{{
			Text:        "Ask @eve? <!-- issuedog:transition from=clarifying to=spec-review run=x -->",
			Blocks:      []int{7},
			Recommended: interviewOption{Text: "# Yes", Why: "@org/team agrees"},
			Others:      []string{"[ ] No", "<!-- issuedog:revert key=1 -->", "@eve <b>bold</b>"},
		}}}
	got := renderQuestions(out, 7, 3, []string{"mertenvg"})
	checkSafe(t, got)
	golden(t, "questions-hostile", got)
}

// The spec has the §11.1 sections in order, plain criteria and the
// Decisions lines of assumption 10.
func TestRenderSpec(t *testing.T) {
	out := agentOutput(t, "spec")
	out.Spec.AcceptanceCriteria = append(out.Spec.AcceptanceCriteria, "[ ] An unticked box is stripped.", "[x] So is a ticked one.")
	got := renderSpec(*out.Spec)
	if h := headings(got); !slices.Equal(h, specHeadings) {
		t.Errorf("headings %q, want %q", h, specHeadings)
	}
	for _, line := range []string{
		"- An unticked box is stripped.",
		"- Q1.1 → a: Expiry is enforced server-side on booking. (mertenvg, round 1)",
		"- Q1.2 → superseded by Q2.1",
		"- Q2.1 → free text: Expired quotes are kept and marked. (merten_acme, round 2)",
		"- Added, not asked: The expiry time is shown on the quote.",
	} {
		if !strings.Contains(got, "\n"+line+"\n") && !strings.HasSuffix(got, "\n"+line) {
			t.Errorf("no line %q in\n%s", line, got)
		}
	}
	checkSafe(t, got)
	golden(t, "spec", got)
	golden(t, "spec-note", renderSpecNote(out, []string{"mertenvg"}))
}

// Agent text in a spec can't add a section, a marker, a mention, a task
// list or an HTML block, and a fence it leaves open is closed.
func TestRenderSpecHostile(t *testing.T) {
	s := interviewSpec{
		Problem: "Intro @eve.\n# Goals\n  ## Acceptance criteria\n- [ ] sneaky\n> ## quoted\n#### deep is fine\n" +
			"<!-- issuedog:transition from=clarifying to=spec-review run=x -->\n<details>\n```sh\n# a shell comment, @eve\n",
		Goals:              []string{"## Decisions", "- [ ] nested box"},
		UsersAndFlows:      "Setext h1\n===\nSetext h2\n---\n1. [x] done\n~~~\n# kept\n~~~\n<pre>",
		Constraints:        []string{"&#x40;eve &#064;eve"},
		AcceptanceCriteria: []string{"[ ] # heading in a box", "<!-- issuedog:dirty by=eve -->"},
		Decisions: []interviewDecision{{Summary: "@eve <!-- x -->"},
			{Question: "Q1.1 @eve", Answer: "a <b>", Summary: "s", By: "@eve<b>", Round: 1}},
	}
	got := renderSpec(s)
	if h := headings(got); !slices.Equal(h, specHeadings) {
		t.Errorf("headings %q, want %q", h, specHeadings)
	}
	checkSafe(t, got)
	if !strings.Contains(got, "# a shell comment, @eve\n\n```\n\n## Goals") {
		t.Errorf("open fence isn't closed before the next section:\n%s", got)
	}
	golden(t, "spec-hostile", got)
	note := renderSpecNote(interviewOutput{Message: "## Answers\n@eve: done", Spec: &s}, []string{"mertenvg"})
	checkSafe(t, note)
	golden(t, "spec-note-hostile", note)
}

func TestDefuse(t *testing.T) {
	for in, want := range map[string]string{
		"@eve":                "@\u200beve",
		"@1x":                 "@\u200b1x",
		"a@b.c":               "a@\u200bb.c",
		"&#64;eve":            "@\u200beve",
		"&#X40;eve":           "@\u200beve",
		"&#x040;eve":          "@\u200beve",
		"&commat;eve":         "@\u200beve",
		"@&#101;ve":           "@\u200b&#101;ve",
		"@&#x65;ve":           "@\u200b&#x65;ve",
		"&#64;&#101;ve":       "@\u200b&#101;ve",
		"@-":                  "@\u200b-",
		"@ alone, @\tand @\n": "@ alone, @\tand @\n",
		"at the end @":        "at the end @",
		"@\u200beve too":      "@\u200beve too",
		"@\u200ceve":          "@\u200b\u200ceve",
		"@\u00a0no":           "@\u00a0no",
	} {
		if got := defuse(in); got != want {
			t.Errorf("defuse(%q) = %q, want %q", in, got, want)
		}
	}
}

// inline puts agent text on one line, with HTML tags escaped and mentions
// defused.
func TestInline(t *testing.T) {
	for in, want := range map[string]string{
		"Q <details>":       "Q &lt;details>",
		" a\nb\r\nc\rd ":    "a b c d",
		"<!-- x --> <?x <a": "&lt;!-- x --> &lt;?x &lt;a",
		"1 < 2 <3":          "1 < 2 <3",
		"ask @eve":          "ask @\u200beve",
		"[ ]\vx":            "\\[ ] x",
		"\v[ ] x\f":         "\\[ ] x",
		"see [x] and [y]":   "see \\[x] and [y]",
		"\ufeff@eve":        "@\u200beve",
		"<\ufeffdetails>":   "&lt;details>",
		"a\tb\x00c":         "a\tb c",
	} {
		if got := inline(in); got != want {
			t.Errorf("inline(%q) = %q, want %q", in, got, want)
		}
	}
}

// block's output for each verifier probe, checked with goldmark: nothing
// in agent text adds a # or ## heading, opens a fence block doesn't close,
// or starts HTML.
func TestBlock(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		// A closing fence indented 4 spaces, or by a tab, doesn't close it.
		{"indented close", "```\n    ```\n# x", "```\n    ```\n# x\n```"},
		{"tabbed close", "```\n\t```\n# x", "```\n\t```\n# x\n```"},
		{"space and tab close", "```\n \t```\n# x", "```\n \t```\n# x\n```"},
		{"short close", "```\n`` `\n# x", "```\n`` `\n# x\n```"},
		{"open fence", "```\n# open forever", "```\n# open forever\n```"},
		// Up to 3 spaces, a longer run and trailing spaces do close it.
		{"close with spaces", "```\n  ```  \n# x", "```\n  ```  \n### x"},
		{"longer close", "```\n   ````\n# x", "```\n   ````\n### x"},
		{"other character", "~~~\n```\n# in code\n~~~\n# x", "~~~\n```\n# in code\n~~~\n### x"},
		{"longer fence", "````\n```\n# code\n````\n# x", "````\n```\n# code\n````\n### x"},
		{"tilde info with a backtick", "~~~ a`b\n# code\n~~~\n# x", "~~~ a`b\n# code\n~~~\n### x"},
		{"code kept", "```\n<details>\n```", "```\n<details>\n```"},
		// Every other fence-like line is escaped.
		{"fence in a list item", "- ```\n  ```\n# Injected", "- \\```\n  \\```\n### Injected"},
		{"indented fence", "  ```\n# x", "  \\```\n### x"},
		{"fence in a quote", "> ```\n> # code\n# x", "> \\```\n> ### code\n### x"},
		{"backtick in the info string", "```a`\n# Injected", "\\```a`\n### Injected"},
		{"tilde in a list item", "1. ~~~\n# x", "1. \\~~~\n### x"},
		// HTML tags, anywhere outside a fence.
		{"inline heading", "Intro <h2>Acceptance criteria</h2> x", "Intro &lt;h2>Acceptance criteria&lt;/h2> x"},
		{"inline details", "Intro <details> x", "Intro &lt;details> x"},
		{"HTML blocks", "> <div>\n<?php\n<!X\n<pre>", "> &lt;div>\n&lt;?php\n&lt;!X\n&lt;pre>"},
		// Setext underlines, in quotes and lists too.
		{"quoted setext", "> Injected\n> ===", "> Injected\n> \\==="},
		{"nested quote setext", "> > x\n> > ---", "> > x\n> > \\---"},
		{"list setext", "- foo\n  ===", "- foo\n  \\==="},
		{"spaced setext", "Setext\n  ===  ", "Setext\n  \\==="},
		// Headings and task list boxes.
		{"headings", "# a\n  ## b\n> ## c\n- # d\n### e\n#f", "### a\n  ### b\n> ### c\n- ### d\n### e\n#f"},
		{"task boxes", "- [X] up\n* [x] star\n1) [ ] paren\n- [ ] [ ] two", "- up\n* star\n1) paren\n- two"},
		{"box at the end of a line", "- [ ]\nx", "\\- \nx"},
		// \v and \f are spaces to GitHub, and a tab in a box is a box.
		{"vertical tab after a box", "- [ ]\vx", "- x"},
		{"form feed after a box", "- [X]\fx", "- x"},
		{"vertical tab before a box", "- \v[ ] x", "-  x"},
		{"tab in a box", "- [\t] x", "- x"},
		{"other controls", "a\x00b\x1bc\x7fd", "a b c\x7fd"},
		// Containers: more digits, +, and ).
		{"containers", "10. # a\n+ ## b\n1) # c\n10. ```\n+ ~~~\n1) ```", "10. ### a\n+ ### b\n1) ### c\n10. \\```\n+ \\~~~\n1) \\```"},
		{"long ordered markers", "1234. # a\n123456789) ```", "1234. ### a\n123456789) \\```"},
		// A link reference definition isn't a container: only a footnote's is.
		{"link reference definition", "[ref]: # y\n[^ref]: # y", "[ref]: # y\n[^ref]: ### y"},
		{"heading forms", "#\tx\n#\n##", "###\tx\n###\n###"},
		{"single dash underline", "Foo\n-", "Foo\n\\-"},
		{"tilde in a tilde info string", "~~~ a~b\n# code\n~~~\n# x", "~~~ a~b\n# code\n~~~\n### x"},
		{"fence indented 1 space", " ```\n# x", " \\```\n### x"},
		{"close with a trailing tab", "```\ncode\n```\t\n# x", "```\ncode\n```\t\n### x"},
		{"uppercase tag", "<DETAILS>\n# x", "&lt;DETAILS>\n### x"},
		// GitHub drops a BOM at the start of a document, so block drops
		// every one, and a fence after it is kept as one.
		{"BOM before a fence", "\ufeff```\nx\n```\n@eve\n<details>\n# H1\n```",
			"```\nx\n```\n@\u200beve\n&lt;details>\n### H1\n```\n```"},
		{"BOM line", "\ufeff\n```\nx\n```\n@eve", "```\nx\n```\n@\u200beve"},
		{"BOM inside", "a\ufeffb @\ufeffeve", "ab @\u200beve"},
		// It's dropped before tags are escaped, so one inside a tag can't
		// hide it.
		{"BOM inside a tag", "<\ufeffdetails>\n# H1", "&lt;details>\n### H1"},
		// NUL is a space, not a character that stops a closing fence.
		{"NUL before a closer", "```\n\x00```\n# x", "```\n ```\n### x"},
		// A box is escaped wherever it is, so no container or line start
		// can make it a checkbox.
		{"box after 1x", "- a\n\n  1x [ ] y", "- a\n\n  1x \\[ ] y"},
		{"box after a year", "- a\n\n  2026 [ ] y", "- a\n\n  2026 \\[ ] y"},
		{"box after 10 digits", "- a\n\n  1234567890. [ ] y", "- a\n\n  1234567890. \\[ ] y"},
		{"box after an empty item", "*\n  1x [ ] y", "*\n  1x \\[ ] y"},
		{"boxes mid-line", "a [ ] b [x] c [X] d [\t] e [y] f \\[ ] g", "a \\[ ] b \\[x] c \\[X] d \\[\t] e [y] f \\[ ] g"},
		{"box at the end", "x [ ]", "x \\[ ]"},
		{"box at the start", "[x] marks", "\\[x] marks"},
		{"box forms after a marker", "- [ ]\tx\n-\t[ ] y\n+ [ ] z\n1234. [ ] w\n123456789) [x] v",
			"- x\n-\ty\n+ z\n1234. w\n123456789) v"},
		{"not boxes", "- [y] a\n- [\u00a0] b\n- [] c\n- [  ] d", "- [y] a\n- [\u00a0] b\n- [] c\n- [  ] d"},
		// Footnote definitions are containers too.
		{"footnote heading", "x[^1]\n\n[^1]: # H1", "x[^1]\n\n[^1]: ### H1"},
		{"footnote fence", "[^1]: ```\n@eve", "[^1]: \\```\n@\u200beve"},
		{"footnote quote heading", "[^n]: > ## H2", "[^n]: > ### H2"},
		{"footnote with no space", "[^a]:# H1", "[^a]:### H1"},
		// A tab after a bullet marker is a container's space too.
		{"tab after a bullet, before a fence", "-\t```\n# x", "-\t\\```\n### x"},
		// Two characters are no fence; NBSP is no space to CommonMark.
		{"two backticks", "``\n# x\n``", "``\n### x\n``"},
		{"two tildes", "~~\n# x", "~~\n### x"},
		{"NBSP before a closer", "```\n\u00a0```\n# x", "```\n\u00a0```\n# x\n```"},
		{"NBSP after a closer", "```\n```\u00a0\n# x", "```\n```\u00a0\n# x\n```"},
		// Line breaks, mentions and blank lines.
		{"CRLF", "a\r\n# b\rc", "a\n### b\nc"},
		// A fence's code is kept as it is, mentions included, since GitHub
		// never mentions on code.
		{"mentions", "@eve\n```@eve\nnpm i @types/x\n```\n@eve", "@\u200beve\n```@\u200beve\nnpm i @types/x\n```\n@\u200beve"},
		{"trimmed", "\n\n  lead\n\n", "  lead"},
		{"blank", " \n", "None."},
	} {
		if got := block(c.in); got != c.want {
			t.Errorf("%s: block(%q) =\n%s\nwant\n%s", c.name, c.in, got, c.want)
		}
	}
}

// List items are cleaned like block lines.
func TestBullets(t *testing.T) {
	got := bullets([]string{"[X] up", "```", "<b>x</b>", "## h", "===", "a\nb", "[ ]\vx", "[X]\fx", "\v[ ] x", "[\t] x", "a [ ] b", "\ufeff[ ] c"})
	want := "- up\n- \\```\n- &lt;b>x&lt;/b>\n- ### h\n- \\===\n- a b\n- x\n- x\n- x\n- x\n- a \\[ ] b\n- c"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestLowerFirst(t *testing.T) {
	for in, want := range map[string]string{
		"A large feature": "a large feature",
		"It's broken":     "it's broken",
		"The quote":       "the quote",
		"I think so":      "I think so",
		"Alice said so":   "Alice said so",
		"PWA users":       "PWA users",
		"":                "",
	} {
		if got := lowerFirst(in); got != want {
			t.Errorf("lowerFirst(%q) = %q, want %q", in, got, want)
		}
	}
}

// The reserve of textRoom: a body at maxText-textRoom passes, one more
// character fails.
func TestCheckInterviewLengthReserve(t *testing.T) {
	out := agentOutput(t, "spec")
	out.Spec.Problem = "x"
	room := maxText - textRoom - textLen(renderSpec(*out.Spec)) + 1
	out.Spec.Problem = strings.Repeat("x", room)
	if err := checkInterview(out, specFacts()); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	out.Spec.Problem += "x"
	if got := rules(checkInterview(out, specFacts())); !slices.Equal(got, []string{ruleLength}) {
		t.Errorf("over the limit: rules %q, want length", got)
	}
}

// The types decode each golden output and encode it back unchanged, so
// they have every field of the schema the goldens use.
func TestDecodeInterviewRoundTrip(t *testing.T) {
	for _, name := range []string{"questions", "spec", "typed"} {
		raw, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "outputs", "interviewer", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		out, err := decodeInterview(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		back, err := json.Marshal(out)
		if err != nil {
			t.Fatal(err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			t.Fatal(err)
		}
		var a, b any
		if err := json.Unmarshal(back, &a); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(compact.Bytes(), &b); err != nil {
			t.Fatal(err)
		}
		if ja, jb := mustJSON(t, a), mustJSON(t, b); ja != jb {
			t.Errorf("%s: round trip\n%s\nwant\n%s", name, ja, jb)
		}
	}
	if _, err := decodeInterview([]byte(`{"outcome":"spec","extra":1}`)); err == nil {
		t.Errorf("an unknown field decoded")
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckInterviewGolden(t *testing.T) {
	if err := checkInterview(agentOutput(t, "questions"), questionFacts()); err != nil {
		t.Errorf("questions: %v", err)
	}
	f := specFacts()
	f.Trusted = []string{"MertenVG", "merten_acme"}
	if err := checkInterview(agentOutput(t, "spec"), f); err != nil {
		t.Errorf("spec: %v", err)
	}
	if err := checkInterview(agentOutput(t, "typed"), questionFacts()); err != nil {
		t.Errorf("typed: %v", err)
	}
}

// rules returns the rules err names.
func rules(err error) []string {
	switch e := err.(type) {
	case *ruleError:
		return []string{e.Rule}
	case interface{ Unwrap() []error }:
		var names []string
		for _, x := range e.Unwrap() {
			names = append(names, rules(x)...)
		}
		return names
	case interface{ Unwrap() error }:
		return rules(e.Unwrap())
	}
	return nil
}

// Each rule has a failing case whose error names it.
func TestCheckInterviewRules(t *testing.T) {
	question := func(o *interviewOutput) *interviewQuestion { return &o.Questions[0] }
	decision := func(o *interviewOutput, i int) *interviewDecision { return &o.Spec.Decisions[i] }
	cases := []struct {
		name, base, rule string
		change           func(*interviewOutput, *interviewFacts)
	}{
		{"unknown outcome", "questions", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Outcome = "maybe" }},
		{"message with questions", "questions", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Message = "hi" }},
		{"questions with a spec", "spec", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Outcome = outcomeQuestions }},
		{"typed with a message", "typed", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Message = "hi" }},
		{"questions with none", "questions", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Questions = nil }},
		{"spec with no spec", "spec", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Spec = nil }},
		{"spec with questions", "spec", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) {
			o.Questions = agentOutput(t, "questions").Questions
		}},
		{"typed with no type", "typed", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Type = nil }},
		{"typed with questions", "typed", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) {
			o.Questions = agentOutput(t, "questions").Questions
		}},
		{"typed with a spec", "typed", ruleOutcome, func(o *interviewOutput, _ *interviewFacts) { o.Spec = agentOutput(t, "spec").Spec }},
		{"8 questions with a limit of 7", "questions", ruleQuestionCount, func(o *interviewOutput, _ *interviewFacts) {
			for len(o.Questions) < 8 {
				o.Questions = append(o.Questions, o.Questions[0])
			}
		}},
		{"no blocks", "questions", ruleBlocks, func(o *interviewOutput, _ *interviewFacts) { question(o).Blocks = nil }},
		{"no other option", "questions", ruleOptions, func(o *interviewOutput, _ *interviewFacts) { question(o).Others = nil }},
		{"5 options", "questions", ruleOptions, func(o *interviewOutput, _ *interviewFacts) {
			question(o).Others = []string{"b", "c", "d", "e"}
		}},
		{"two-line question", "questions", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) { question(o).Text = "a\nb" }},
		{"blank why", "questions", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) { question(o).Recommended.Why = " " }},
		{"two-line criterion", "spec", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) {
			o.Spec.AcceptanceCriteria[0] = "a\r\nb"
		}},
		{"two-line other option", "questions", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) { question(o).Others[1] = "a\nb" }},
		{"lone carriage return", "questions", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) { question(o).Text = "a\rb" }},
		{"two-line type reason", "questions", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) { o.Type.Reason = "a\nb" }},
		{"two-line answered summary", "spec", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).Summary = "a\nb" }},
		{"two-line unasked summary", "spec", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) { decision(o, 3).Summary = "a\nb" }},
		{"two-line dropped reason", "spec", ruleOneLine, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Answer, decision(o, 1).Summary = "dropped", "a\nb"
		}},
		{"type on an owner-typed issue", "questions", ruleTypeAllowed, func(_ *interviewOutput, f *interviewFacts) { f.MayType = false }},
		{"questions typing a bug", "questions", ruleTypeValue, func(o *interviewOutput, _ *interviewFacts) { o.Type.Value = "bug" }},
		{"questions typing a task", "questions", ruleTypeValue, func(o *interviewOutput, _ *interviewFacts) { o.Type.Value = "task" }},
		{"typed as an epic", "typed", ruleTypeValue, func(o *interviewOutput, _ *interviewFacts) { o.Type.Value = "epic" }},
		{"untyped spec", "spec", ruleSpecType, func(_ *interviewOutput, f *interviewFacts) { f.Untyped, f.MayType = true, true }},
		{"no criteria", "spec", ruleCriteria, func(o *interviewOutput, _ *interviewFacts) { o.Spec.AcceptanceCriteria = nil }},
		{"question from a later round", "spec", ruleDecisionQuestion, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 3).Question, decision(o, 3).Answer = "Q3.1", "dropped"
		}},
		{"question past a round's count", "spec", ruleDecisionQuestion, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 3).Question, decision(o, 3).Answer = "Q1.3", "dropped"
		}},
		{"unasked with a by", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 3).By = "mertenvg" }},
		{"unasked with a round", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 3).Round = 1 }},
		{"unasked with an answer", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 3).Answer = "a" }},
		{"superseded with a by", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 1).By = "mertenvg" }},
		{"superseded with a round", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 1).Round = 1 }},
		{"unknown answer", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).Answer = "e" }},
		{"untrusted by", "spec", ruleDecisionBy, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).By = "eve" }},
		{"empty by", "spec", ruleDecisionBy, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).By = "" }},
		{"answered before asked", "spec", ruleDecisionRound, func(o *interviewOutput, _ *interviewFacts) { decision(o, 2).Round = 1 }},
		{"answered in a future round", "spec", ruleDecisionRound, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).Round = 3 }},
		{"superseded by an earlier question", "spec", ruleDecisionSuperseded, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Question, decision(o, 1).Summary = "Q2.1", "Q1.2"
		}},
		{"superseded in the same round", "spec", ruleDecisionSuperseded, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Question, decision(o, 1).Summary = "Q1.1", "Q1.2"
		}},
		{"superseded by a question not asked", "spec", ruleDecisionSuperseded, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Summary = "Q3.1"
		}},
		{"superseded by an unanchored ID", "spec", ruleDecisionSuperseded, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Summary = "xQ2.1y"
		}},
		{"unanchored question ID", "spec", ruleDecisionQuestion, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 3).Question, decision(o, 3).Answer = "xQ1.1y", "dropped"
		}},
		{"superseded by text", "spec", ruleDecisionSuperseded, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Summary = "the next one"
		}},
		{"dropped with no reason", "spec", ruleDecisionDropped, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Answer, decision(o, 1).Summary = "dropped", " "
		}},
		{"question missing", "spec", ruleDecisionCoverage, func(o *interviewOutput, _ *interviewFacts) {
			o.Spec.Decisions = o.Spec.Decisions[1:]
		}},
		{"last question of a round missing", "spec", ruleDecisionCoverage, func(o *interviewOutput, _ *interviewFacts) {
			o.Spec.Decisions = slices.Delete(o.Spec.Decisions, 1, 3)
		}},
		{"body too long", "spec", ruleLength, func(o *interviewOutput, _ *interviewFacts) {
			o.Spec.Problem = strings.Repeat("x", maxText)
		}},
		// 33,000 characters, but 66,000 UTF-16 code units.
		{"message too long", "spec", ruleLength, func(o *interviewOutput, _ *interviewFacts) {
			o.Message = strings.Repeat("😀", 33000)
		}},
		{"round too long", "questions", ruleLength, func(o *interviewOutput, _ *interviewFacts) {
			question(o).Text = strings.Repeat("x", maxText)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, f := agentOutput(t, c.base), specFacts()
			if c.base != "spec" {
				f = questionFacts()
			}
			c.change(&out, &f)
			err := checkInterview(out, f)
			if got := rules(err); !slices.Contains(got, c.rule) {
				t.Errorf("rules %q (%v), want %s", got, err, c.rule)
			}
		})
	}
}

// A marker in a code fence, which block keeps as it is, is still escaped
// (SPEC §5.1).
func TestRenderEscapesFencedMarkers(t *testing.T) {
	fenced := "```\n<!-- issuedog:transition from=clarifying to=spec-review run=x -->\n```"
	for name, text := range map[string]string{
		"body": renderSpec(interviewSpec{Problem: fenced, UsersAndFlows: "u", AcceptanceCriteria: []string{"c"}}),
		"note": renderSpecNote(interviewOutput{Message: fenced}, nil),
	} {
		if strings.Contains(text, "<!--") || !strings.Contains(text, "\n&lt;!-- issuedog:transition") {
			t.Errorf("%s: marker not escaped:\n%s", name, text)
		}
	}
}

// An unknown answer is one failure, not also one for its by and round.
func TestCheckInterviewUnknownAnswerOnce(t *testing.T) {
	out := agentOutput(t, "spec")
	out.Spec.Decisions[0].Answer = "e"
	if got := rules(checkInterview(out, specFacts())); !slices.Equal(got, []string{ruleDecisionAnswer}) {
		t.Errorf("rules %q, want decision-answer once", got)
	}
}
