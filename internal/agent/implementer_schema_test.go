package agent

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// implementerOutcomes are the implementer's three outcomes, each with its
// golden sample of the same name.
var implementerOutcomes = []string{"commits", "question", "handoff"}

// implementerSchemaDoc is the shipped implementer schema, decoded.
func implementerSchemaDoc(t *testing.T) map[string]any {
	t.Helper()
	raw, err := StageSchema("implementer")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

// Each golden sample passes the schema, and is the outcome it's named for.
func TestImplementerGolden(t *testing.T) {
	t.Parallel()
	for _, name := range implementerOutcomes {
		out := golden(t, "implementer", name)
		if err := validateOutput(t, "implementer", out); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if out["outcome"] != name {
			t.Errorf("%s: outcome %v, want %s", name, out["outcome"], name)
		}
	}
}

// The schema branches on outcome with if/then/else, the one conditional it
// has at the top level, where the Messages API refuses unions in a tool's
// input schema (SPEC §16). Below it, as the planner's, it uses no union or
// reference, and every object lists its properties, all of them known.
func TestImplementerSchemaShape(t *testing.T) {
	t.Parallel()
	doc := implementerSchemaDoc(t)
	for _, k := range []string{"oneOf", "anyOf", "allOf", "not"} {
		if _, ok := doc[k]; ok {
			t.Errorf("the top level uses %s", k)
		}
	}
	for _, k := range []string{"if", "then", "else"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("the top level has no %s, so it doesn't branch on outcome", k)
		}
	}
	checkPlannerSchema(t, "#", doc)
	raw, err := StageSchema("implementer")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"oneOf"`) {
		t.Errorf("the schema uses oneOf")
	}
}

// A question is the interviewer's own question shape, so the daemon checks
// it as it checks the interviewer's (SPEC §9.1), not a near variant of it.
func TestImplementerQuestionShapeIsTheInterviewers(t *testing.T) {
	t.Parallel()
	raw, err := StageSchema("interviewer")
	if err != nil {
		t.Fatal(err)
	}
	var interviewer map[string]any
	if err := json.Unmarshal(raw, &interviewer); err != nil {
		t.Fatal(err)
	}
	implementer := implementerSchemaDoc(t)
	for _, k := range []string{"type", "items"} {
		got := valueAt(t, implementer, "properties", "questions", k)
		want := valueAt(t, interviewer, "properties", "questions", k)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("questions.%s is %v, want the interviewer's %v", k, got, want)
		}
	}
}

// The hand-off has every field of SPEC §9.3's template, one each, and
// nothing else.
func TestImplementerHandoffFields(t *testing.T) {
	t.Parallel()
	handoff := valueAt(t, implementerSchemaDoc(t), "properties", "handoff")
	want := []string{"goal", "why_blocks", "steps", "links", "done_when", "estimated_time"}
	required, ok := valueAt(t, handoff, "required").([]any)
	if !ok {
		t.Fatal("handoff.required isn't a list")
	}
	var got []string
	for _, r := range required {
		s, ok := r.(string)
		if !ok {
			t.Fatalf("handoff.required has %v", r)
		}
		got = append(got, s)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("handoff requires %q, want %q", got, want)
	}
	props, ok := valueAt(t, handoff, "properties").(map[string]any)
	if !ok || len(props) != len(want) {
		t.Errorf("handoff has properties %v, want exactly %q", props, want)
	}
}

func TestImplementerBadOutputs(t *testing.T) {
	t.Parallel()
	question := golden(t, "implementer", "question")["questions"]
	handoff := golden(t, "implementer", "handoff")["handoff"]
	commits := golden(t, "implementer", "commits")
	title, summary := commits["title"], commits["summary"]
	checkBad(t, "implementer", map[string]badOutput{
		// commits: a PR title and a summary, and no other outcome's fields.
		"commits with no title":         {"commits", remove, []any{"title"}, "missing property 'title'"},
		"commits with no summary":       {"commits", remove, []any{"summary"}, "missing property 'summary'"},
		"commits with questions":        {"commits", question, []any{"questions"}, "at '/questions': false schema"},
		"commits with a handoff":        {"commits", handoff, []any{"handoff"}, "at '/handoff': false schema"},
		"title with no type":            {"commits", remove, []any{"title", "type"}, "at '/title': missing property 'type'"},
		"title with no sentence":        {"commits", remove, []any{"title", "sentence"}, "at '/title': missing property 'sentence'"},
		"blank type":                    {"commits", " ", []any{"title", "type"}, "at '/title/type'"},
		"blank sentence":                {"commits", "", []any{"title", "sentence"}, "at '/title/sentence'"},
		"multi-line sentence":           {"commits", "refuse expired quotes\n\nCloses #1", []any{"title", "sentence"}, "at '/title/sentence'"},
		"multi-line type":               {"commits", "feat\nfix", []any{"title", "type"}, "at '/title/type'"},
		"title with a scope":            {"commits", "booking", []any{"title", "scope"}, "additional properties 'scope' not allowed"},
		"title as a string":             {"commits", "feat: refuse expired quotes", []any{"title"}, "at '/title': got string, want object"},
		"summary with no what_changed":  {"commits", remove, []any{"summary", "what_changed"}, "at '/summary': missing property 'what_changed'"},
		"summary with no why":           {"commits", remove, []any{"summary", "why"}, "at '/summary': missing property 'why'"},
		"summary with no tests_added":   {"commits", remove, []any{"summary", "tests_added"}, "at '/summary': missing property 'tests_added'"},
		"summary with no tests_updated": {"commits", remove, []any{"summary", "tests_updated"}, "at '/summary': missing property 'tests_updated'"},
		"blank what_changed":            {"commits", " \n ", []any{"summary", "what_changed"}, "at '/summary/what_changed'"},
		"blank why":                     {"commits", "\t", []any{"summary", "why"}, "at '/summary/why'"},
		"multi-line test added":         {"commits", "TestA\nTestB", []any{"summary", "tests_added", 0}, "at '/summary/tests_added/0'"},
		"blank test updated":            {"commits", " ", []any{"summary", "tests_updated", 0}, "at '/summary/tests_updated/0'"},
		"summary with notes":            {"commits", "x", []any{"summary", "notes"}, "additional properties 'notes' not allowed"},

		// question: questions, in the interviewer's shape, and nothing else.
		"question with no questions": {"question", remove, []any{"questions"}, "missing property 'questions'"},
		"question with a title":      {"question", title, []any{"title"}, "at '/title': false schema"},
		"question with a summary":    {"question", summary, []any{"summary"}, "at '/summary': false schema"},
		"question with a handoff":    {"question", handoff, []any{"handoff"}, "at '/handoff': false schema"},
		"4 others":                   {"question", []any{"b", "c", "d", "e"}, []any{"questions", 0, "others"}, "at '/questions/0/others': maxItems"},
		"no others":                  {"question", []any{}, []any{"questions", 0, "others"}, "at '/questions/0/others': minItems"},
		"empty blocks":               {"question", []any{}, []any{"questions", 0, "blocks"}, "at '/questions/0/blocks': minItems"},
		"multi-line question":        {"question", "Which?\n### Injected", []any{"questions", 0, "text"}, "at '/questions/0/text'"},
		"no recommended why":         {"question", remove, []any{"questions", 0, "recommended", "why"}, "missing property 'why'"},
		"options in place of others": {"question", []any{"Accepted"}, []any{"questions", 0, "options"}, "additional properties 'options' not allowed"},

		// handoff: every 9.3 field, filled, and nothing else.
		"handoff with no handoff":     {"handoff", remove, []any{"handoff"}, "missing property 'handoff'"},
		"handoff with a title":        {"handoff", title, []any{"title"}, "at '/title': false schema"},
		"handoff with a summary":      {"handoff", summary, []any{"summary"}, "at '/summary': false schema"},
		"handoff with questions":      {"handoff", question, []any{"questions"}, "at '/questions': false schema"},
		"no goal":                     {"handoff", remove, []any{"handoff", "goal"}, "at '/handoff': missing property 'goal'"},
		"no why_blocks":               {"handoff", remove, []any{"handoff", "why_blocks"}, "at '/handoff': missing property 'why_blocks'"},
		"no steps":                    {"handoff", remove, []any{"handoff", "steps"}, "at '/handoff': missing property 'steps'"},
		"no links":                    {"handoff", remove, []any{"handoff", "links"}, "at '/handoff': missing property 'links'"},
		"no done_when":                {"handoff", remove, []any{"handoff", "done_when"}, "at '/handoff': missing property 'done_when'"},
		"no estimated_time":           {"handoff", remove, []any{"handoff", "estimated_time"}, "at '/handoff': missing property 'estimated_time'"},
		"empty goal":                  {"handoff", "", []any{"handoff", "goal"}, "at '/handoff/goal'"},
		"blank why_blocks":            {"handoff", " ", []any{"handoff", "why_blocks"}, "at '/handoff/why_blocks'"},
		"blank steps":                 {"handoff", " \n ", []any{"handoff", "steps"}, "at '/handoff/steps'"},
		"empty links":                 {"handoff", "", []any{"handoff", "links"}, "at '/handoff/links'"},
		"blank done_when":             {"handoff", "\t", []any{"handoff", "done_when"}, "at '/handoff/done_when'"},
		"empty estimated_time":        {"handoff", "", []any{"handoff", "estimated_time"}, "at '/handoff/estimated_time'"},
		"multi-line goal":             {"handoff", "Add a key.\n### Injected", []any{"handoff", "goal"}, "at '/handoff/goal'"},
		"multi-line why_blocks":       {"handoff", "a key\nreally", []any{"handoff", "why_blocks"}, "at '/handoff/why_blocks'"},
		"multi-line done_when":        {"handoff", "The key.\nReally.", []any{"handoff", "done_when"}, "at '/handoff/done_when'"},
		"multi-line estimated_time":   {"handoff", "5\nmin", []any{"handoff", "estimated_time"}, "at '/handoff/estimated_time'"},
		"steps as a list":             {"handoff", []any{"Open it."}, []any{"handoff", "steps"}, "at '/handoff/steps': got array, want string"},
		"handoff with a check":        {"handoff", map[string]any{"kind": "label"}, []any{"handoff", "check"}, "additional properties 'check' not allowed"},
		"handoff as a string":         {"handoff", "Add the key.", []any{"handoff"}, "at '/handoff': got string, want object"},
		"handoff with an extra field": {"handoff", "x", []any{"handoff", "notes"}, "additional properties 'notes' not allowed"},

		// The output.
		"outcome questions": {"question", "questions", []any{"outcome"}, "at '/outcome': value must be one of"},
		"outcome plan":      {"commits", "plan", []any{"outcome"}, "at '/outcome': value must be one of"},
		"no outcome":        {"commits", remove, []any{"outcome"}, "missing property 'outcome'"},
		"extra field":       {"commits", "x", []any{"message"}, "additional properties 'message' not allowed"},
	})
}

// implementerPlanLabel matches a plan's labels, such as T6, S2, Q2b or
// assumption 3, which neither the prompt nor the schema names.
var implementerPlanLabel = regexp.MustCompile(`\b[TS][0-9]+\b|\bQ[0-9]+[a-z]\b|(?i)\bassumption [0-9]+\b`)

// implementerConfigValue matches the config's defaults that the preamble
// gives in its place, as config can change them: the title types, the diff
// line limit, the protected paths and the question limit (SPEC §12, §13).
var implementerConfigValue = regexp.MustCompile("`feat`|`fix`|`chore`|\\bfeat:|\\b400\\b|\\b7\\b|\\.github/|\\.issuedog/|\\.claude/|CLAUDE\\.md|\\.mcp\\.json|CODEOWNERS|max_diff_lines|max_questions|title_types|protected_paths")

func TestImplementerPrompt(t *testing.T) {
	t.Parallel()
	p := string(shippedPrompt(t, "implementer"))
	for _, want := range []string{
		// The clone: commit there, push nothing, add no remote.
		"you run in a detached checkout of the commit the daemon named for this task, with its history but no branches and no remote",
		"Work in the current directory, the clone. Commit your work there",
		"Never push, and add no remote",
		"Only committed work counts",
		// Protected paths, from the preamble.
		"Touch no protected path: the preamble lists them.",
		// Tests with every change, and no skip markers.
		"Add or update tests with every source change",
		"Add no skip markers, and delete no test file.",
		// The limits the preamble gives.
		"within the line limit the preamble gives",
		"one of the preamble's title types",
		"At most the per-round limit the preamble gives",
		// A question for an ambiguous criterion, a hand-off only for an
		// owner action, every field filled.
		"Ask when an acceptance criterion is ambiguous",
		"Hand off only for an action only the owner can do",
		"Fill every field of `handoff`. If you can't fill one, such as an exact value you don't know, ask a question instead.",
		"Commit the work you've done before you ask",
		"Commit the work you've done before you hand off",
		// The question shape is the interviewer's.
		"`recommended` first, with a one-line `why`, then 1 to 3 `others`",
		// The three outcomes and their fields.
		"- `commits`: `title` and `summary`.", "- `question`: `questions`.", "- `handoff`: `handoff`.",
		"`tests_added`", "`tests_updated`", "`what_changed`",
		"`goal`", "`why_blocks`", "`steps`", "`links`", "`done_when`", "`estimated_time`",
		`in one line that reads on after "can't proceed until"`,
		// The run's files.
		"`issue.md`", "`conversation.md`", "`input.md`",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("implementer.md doesn't contain %q", want)
		}
	}
	// The run gets no untrusted.md (SPEC §6.2), and the clone has no main
	// branch.
	for _, not := range []string{"untrusted.md", "checkout of `main`"} {
		if strings.Contains(p, not) {
			t.Errorf("implementer.md names %q", not)
		}
	}
}

// Neither the prompt nor the schema states a value the preamble gives, as
// config can change it, nor names a plan's labels.
func TestImplementerStatesNoConfigValues(t *testing.T) {
	t.Parallel()
	raw, err := StageSchema("implementer")
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{"implementer.md": string(shippedPrompt(t, "implementer")), "implementer.json": string(raw)} {
		if m := implementerConfigValue.FindAllString(text, -1); len(m) > 0 {
			t.Errorf("%s states config values %q", name, m)
		}
		if m := implementerPlanLabel.FindAllString(text, -1); len(m) > 0 {
			t.Errorf("%s names plan labels %q", name, m)
		}
	}
}
