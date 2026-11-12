package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// interviewerOutcomes are the interviewer's outcomes, each with its golden
// sample of the same name.
var interviewerOutcomes = []string{"questions", "spec", "typed", "unchanged", "plan", "task", "human"}

// interviewerTaskFields are the task template's sections (SPEC §11.2) as
// the task outcome's body has them, and interviewerHumanFields the human
// task template's fields (SPEC §9.3), in the order the schema lists them.
var (
	interviewerTaskFields  = []string{"scope", "context", "change", "acceptance_criteria", "definition_of_done", "test_plan"}
	interviewerHumanFields = []string{"goal", "why_blocks", "steps", "links", "done_when", "estimated_time"}
)

// stageSchemaDoc is the shipped schema of stage, decoded.
func stageSchemaDoc(t *testing.T, stage string) map[string]any {
	t.Helper()
	raw, err := StageSchema(stage)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", stage, err)
	}
	return doc
}

// stringList is the list of strings at path in doc.
func stringList(t *testing.T, doc map[string]any, path ...any) []string {
	t.Helper()
	list, ok := valueAt(t, doc, path...).([]any)
	if !ok {
		t.Fatalf("%v isn't a list", path)
	}
	var got []string
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("%v has %v", path, v)
		}
		got = append(got, s)
	}
	return got
}

// withoutDescription is a copy of the schema s with no description, so
// two fields whose wording differs compare by what they accept.
func withoutDescription(t *testing.T, s any) map[string]any {
	t.Helper()
	m, ok := s.(map[string]any)
	if !ok {
		t.Fatalf("%v isn't a schema object", s)
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if k != "description" {
			out[k] = v
		}
	}
	return out
}

// The schema lists the seven outcomes and each one's field, and nothing
// else, as SPEC §12 says it holds.
func TestInterviewerSchemaOutcomes(t *testing.T) {
	t.Parallel()
	doc := stageSchemaDoc(t, "interviewer")
	if got := stringList(t, doc, "properties", "outcome", "enum"); !slices.Equal(got, interviewerOutcomes) {
		t.Errorf("outcome is one of %q, want %q", got, interviewerOutcomes)
	}
	if got, want := stringList(t, doc, "required"), []string{"outcome"}; !slices.Equal(got, want) {
		t.Errorf("required is %q, want %q", got, want)
	}
	props, ok := valueAt(t, doc, "properties").(map[string]any)
	want := []string{"outcome", "type", "message", "questions", "spec", "task", "human"}
	if !ok || len(props) != len(want) {
		t.Errorf("the top level has properties %v, want exactly %q", props, want)
	}
	for _, k := range want {
		if _, ok := props[k]; !ok {
			t.Errorf("the top level has no %s", k)
		}
	}
}

// The top level has no conditional or union, as the planner's and the
// verifier's have none, since the Messages API is reported to refuse
// unions there (SPEC §16): the daemon's check pairs each outcome with its
// fields. TestSchemasStayFlat checks the levels below.
func TestInterviewerSchemaTopLevelHasNoConditional(t *testing.T) {
	t.Parallel()
	doc := stageSchemaDoc(t, "interviewer")
	for _, k := range []string{"if", "then", "else", "oneOf", "anyOf", "allOf", "not"} {
		if _, ok := doc[k]; ok {
			t.Errorf("the top level uses %s", k)
		}
	}
}

// Each new golden sample is the outcome it's named for, with that outcome's
// field and nothing else, and the task is a bug against earlier work, as
// the prompt says one is written (SPEC §4).
func TestInterviewerGoldenTaskBodies(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"task", "human"} {
		out := golden(t, "interviewer", name)
		if out["outcome"] != name {
			t.Errorf("%s: outcome %v, want %s", name, out["outcome"], name)
		}
		if _, ok := out[name].(map[string]any); !ok || len(out) != 2 {
			t.Errorf("%s: %v, want the outcome and %s alone", name, out, name)
		}
	}
	task := golden(t, "interviewer", "task")
	if context, ok := valueAt(t, task, "task", "context").(string); !ok || !strings.HasPrefix(context, "Follow-up to #98.") {
		t.Errorf("task: context %q, want it to start with Follow-up to #98", context)
	}
	if criteria := stringList(t, task, "task", "acceptance_criteria"); !slices.Contains(criteria, "The original behavior of #98 is unaffected.") {
		t.Errorf("task: criteria %q, want the original behavior of #98 is unaffected", criteria)
	}
}

// The task outcome's body has the task template's sections (SPEC §11.2),
// each required, and nothing else. Each is shaped as a plan's task has it:
// the body's sections as the planner's task body, the scope as a plan
// item's, and the criteria as a plan item's, so the daemon checks and
// renders them as it does a plan's task.
func TestInterviewerTaskIsAPlansTask(t *testing.T) {
	t.Parallel()
	doc := stageSchemaDoc(t, "interviewer")
	task := valueAt(t, doc, "properties", "task")
	if got := stringList(t, doc, "properties", "task", "required"); !slices.Equal(got, interviewerTaskFields) {
		t.Errorf("task requires %q, want %q", got, interviewerTaskFields)
	}
	props, ok := valueAt(t, task, "properties").(map[string]any)
	if !ok || len(props) != len(interviewerTaskFields) {
		t.Errorf("task has properties %v, want exactly %q", props, interviewerTaskFields)
	}
	if valueAt(t, task, "additionalProperties") != false {
		t.Errorf("task allows other properties")
	}

	planner := stageSchemaDoc(t, "planner")
	item := []any{"properties", "plan", "properties", "items", "items", "properties"}
	at := func(path ...any) any { return valueAt(t, planner, append(append([]any{}, item...), path...)...) }
	for _, k := range []string{"context", "change", "definition_of_done", "test_plan"} {
		if got, want := valueAt(t, task, "properties", k), at("task", "properties", k); !reflect.DeepEqual(got, want) {
			t.Errorf("task.%s is %v, want the planner's task.%s %v", k, got, want, k)
		}
	}
	for k, want := range map[string]any{"scope": at("scope"), "acceptance_criteria": at("criteria")} {
		if got := withoutDescription(t, valueAt(t, task, "properties", k)); !reflect.DeepEqual(got, withoutDescription(t, want)) {
			t.Errorf("task.%s is %v, want a plan item's %v", k, got, want)
		}
	}
}

// The human outcome's body has every field of SPEC §9.3's template, each
// required, in the implementer's hand-off's order, and nothing else. Each
// accepts what the hand-off's field accepts, so the daemon checks and
// renders it as it does a hand-off.
func TestInterviewerHumanIsTheHandoff(t *testing.T) {
	t.Parallel()
	doc := stageSchemaDoc(t, "interviewer")
	human := valueAt(t, doc, "properties", "human")
	handoff := valueAt(t, stageSchemaDoc(t, "implementer"), "properties", "handoff")
	if got := stringList(t, doc, "properties", "human", "required"); !slices.Equal(got, interviewerHumanFields) {
		t.Errorf("human requires %q, want %q", got, interviewerHumanFields)
	}
	if want, ok := valueAt(t, handoff, "required").([]any); !ok || len(want) != len(interviewerHumanFields) {
		t.Errorf("the hand-off requires %v, want %q", want, interviewerHumanFields)
	}
	props, ok := valueAt(t, human, "properties").(map[string]any)
	if !ok || len(props) != len(interviewerHumanFields) {
		t.Errorf("human has properties %v, want exactly %q", props, interviewerHumanFields)
	}
	if valueAt(t, human, "additionalProperties") != false {
		t.Errorf("human allows other properties")
	}
	for _, k := range interviewerHumanFields {
		got := withoutDescription(t, valueAt(t, human, "properties", k))
		if want := withoutDescription(t, valueAt(t, handoff, "properties", k)); !reflect.DeepEqual(got, want) {
			t.Errorf("human.%s is %v, want the hand-off's %v", k, got, want)
		}
	}
}

// specFollowUpRule returns SPEC §4's acceptance criterion for a bug filed
// against earlier work, without its quotes or closing period.
func specFollowUpRule(t *testing.T) string {
	t.Helper()
	spec, err := os.ReadFile(filepath.Join("..", "..", "SPEC.md"))
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(spec), "\n## 4. Work item types\n")
	if !ok {
		t.Fatal("SPEC.md has no section 4")
	}
	section, _, _ = strings.Cut(section, "\n## 5. ")
	_, rule, ok := strings.Cut(section, "starts its Context with `Follow-up to #N` and includes the acceptance criterion \"")
	if !ok {
		t.Fatal("SPEC §4 has no Follow-up to #N rule")
	}
	rule, _, ok = strings.Cut(rule, ".\"")
	if !ok {
		t.Fatal("SPEC §4's acceptance criterion for a bug against earlier work doesn't end")
	}
	return rule
}

// The shipped prompt says when each new outcome fits, gives each field of
// both bodies, and keeps SPEC §4's rule for a bug against earlier work,
// its criterion word for word, so the two can't drift.
func TestInterviewerPromptTaskBody(t *testing.T) {
	t.Parallel()
	p := string(shippedPrompt(t, "interviewer"))
	for _, want := range []string{
		// When task fits: a bug or a task with nothing open that blocks it,
		// and after a hold, a task whose body the discussion changes.
		"Return `task` only when the preamble says you may, in two cases:",
		"- A bug or a task with nothing open that blocks it: once no question that blocks the work is left, write its body.",
		"- After a discussion hold, a task whose body the discussion changes: write the whole body again",
		"- It changes a task's body: on a task or a bug, return `task`, with the whole new body; on a human task, return `human`. Only when the preamble says you may.",
		// When human fits: only after a hold, on a human task.
		"Return `human` only after a discussion hold, on a human task whose body the discussion changes, and only when the preamble says you may.",
		"A task that isn't human gets `task`, never `human`.",
		// Their fields, and nothing else with them.
		"- `task`: `task` only, where the preamble allows it.",
		"- `human`: `human` only, and only after a hold, on a human task, where the preamble allows it.",
		// The task template's sections, and the daemon writes the header.
		"in the task template's sections. The daemon writes the header, with the parent, the scope and the priority, and the headings",
		"A scope that isn't on the list is a question, never a guess.",
		`It always includes "Tests added or updated for every changed behavior", "verify.commands pass" and "No new skip markers".`,
		// The human task's fields are all filled.
		"Fill every field. If you can't fill one, such as an exact value you don't know, ask a question instead.",
		`in one line that reads on after "can't proceed until"`,
		// SPEC §4's rule for a bug against earlier work.
		"starts `context` with `Follow-up to #N` and includes the acceptance criterion \"" + specFollowUpRule(t) + "\"",
		"Keep both when you rewrite its body.",
		// Typing comes first, and the run after it writes the task (SPEC
		// §9.1).
		"An untyped bug or task always returns `typed` before `task`: you run again at once, and that run writes its task, or asks a round first",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("interviewer.md doesn't contain %q", want)
		}
	}
	// A typed bug or task no longer waits: the daemon writes its task.
	if bad := "It waits for a later stage."; strings.Contains(p, bad) {
		t.Errorf("interviewer.md still says %q", bad)
	}
	doc := stageSchemaDoc(t, "interviewer")
	for name, fields := range map[string][]string{"task": interviewerTaskFields, "human": interviewerHumanFields} {
		for _, k := range fields {
			if want := "- `" + k + "`: "; !strings.Contains(p, want) {
				t.Errorf("interviewer.md doesn't say what %s.%s holds, as %q", name, k, want)
			}
		}
		if props, ok := valueAt(t, doc, "properties", name, "properties").(map[string]any); !ok || len(props) != len(fields) {
			t.Errorf("%s has properties %v, want the %q the prompt gives", name, props, fields)
		}
	}
}
