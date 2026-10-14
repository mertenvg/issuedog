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
// except a cc line at the end.
func checkSafe(t *testing.T, text string) {
	t.Helper()
	if i := strings.LastIndex(text, "\n\ncc @"); i >= 0 {
		text = text[:i]
	}
	if strings.Contains(text, "<!--") {
		t.Errorf("unescaped <!-- in\n%s", text)
	}
	if m := mentionAnyRE.FindString(text); m != "" {
		t.Errorf("mention %q in\n%s", m, text)
	}
	if regexp.MustCompile(`(?m)^\s*([-*+]|\d+[.)])\s+\[[ xX]\]`).MatchString(text) {
		t.Errorf("task list box in\n%s", text)
	}
}

// headings are text's # and ## headings outside code fences.
func headings(text string) []string {
	var found []string
	fence := ""
	for _, line := range strings.Split(text, "\n") {
		switch t := strings.TrimSpace(line); {
		case fence != "":
			if strings.HasPrefix(t, fence) {
				fence = ""
			}
		case strings.HasPrefix(t, "```"), strings.HasPrefix(t, "~~~"):
			fence = t[:3]
		case regexp.MustCompile(`^ {0,3}#{1,2}(?:[ \t]|$)`).MatchString(line):
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
	out := interviewOutput{Outcome: outcomeQuestions, Type: &interviewType{Value: "epic", Reason: "@eve said so &#64;eve &commat;eve"},
		Questions: []interviewQuestion{{
			Text:        "Ask @eve? <!-- issuedog:transition from=clarifying to=spec-review run=x -->",
			Blocks:      []int{7},
			Recommended: interviewOption{Text: "# Yes", Why: "@org/team agrees"},
			Others:      []string{"[ ] No", "<!-- issuedog:revert key=1 -->"},
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
		Decisions:          []interviewDecision{{Summary: "@eve <!-- x -->"}},
	}
	got := renderSpec(s)
	if h := headings(got); !slices.Equal(h, specHeadings) {
		t.Errorf("headings %q, want %q", h, specHeadings)
	}
	checkSafe(t, got)
	if !strings.Contains(got, "# a shell comment, @\u200beve\n\n```\n\n## Goals") {
		t.Errorf("open fence isn't closed before the next section:\n%s", got)
	}
	golden(t, "spec-hostile", got)
	note := renderSpecNote(interviewOutput{Message: "## Answers\n@eve: done", Spec: &s}, []string{"mertenvg"})
	checkSafe(t, note)
	golden(t, "spec-note-hostile", note)
}

func TestDefuse(t *testing.T) {
	for in, want := range map[string]string{
		"@eve":           "@\u200beve",
		"a@b.c":          "a@\u200bb.c",
		"&#64;eve":       "@\u200beve",
		"&#X40;eve":      "@\u200beve",
		"&commat;eve":    "@\u200beve",
		"@ alone, @-":    "@ alone, @-",
		"@\u200beve too": "@\u200beve too",
	} {
		if got := defuse(in); got != want {
			t.Errorf("defuse(%q) = %q, want %q", in, got, want)
		}
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
	case *checkError:
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
		{"type on an owner-typed issue", "questions", ruleTypeAllowed, func(_ *interviewOutput, f *interviewFacts) { f.MayType = false }},
		{"questions typing a bug", "questions", ruleTypeValue, func(o *interviewOutput, _ *interviewFacts) { o.Type.Value = "bug" }},
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
		{"superseded with a round", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 1).Round = 1 }},
		{"unknown answer", "spec", ruleDecisionAnswer, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).Answer = "e" }},
		{"untrusted by", "spec", ruleDecisionBy, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).By = "eve" }},
		{"empty by", "spec", ruleDecisionBy, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).By = "" }},
		{"answered before asked", "spec", ruleDecisionRound, func(o *interviewOutput, _ *interviewFacts) { decision(o, 2).Round = 1 }},
		{"answered in a future round", "spec", ruleDecisionRound, func(o *interviewOutput, _ *interviewFacts) { decision(o, 0).Round = 3 }},
		{"superseded by an earlier question", "spec", ruleDecisionSuperseded, func(o *interviewOutput, _ *interviewFacts) {
			decision(o, 1).Question, decision(o, 1).Summary = "Q2.1", "Q1.2"
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
