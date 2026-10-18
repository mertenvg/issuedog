package agent

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// valueAt returns the value at path in v, where a string steps into an object
// and an int into an array. A step that isn't there fails the test.
func valueAt(t *testing.T, v any, path ...any) any {
	t.Helper()
	for i, step := range path {
		switch k := step.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: step %d isn't an object", path, i)
			}
			if v, ok = m[k]; !ok {
				t.Fatalf("%v: step %d has no %q", path, i, k)
			}
		case int:
			a, ok := v.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("%v: step %d isn't an array with index %d", path, i, k)
			}
			v = a[k]
		}
	}
	return v
}

// planItems returns a golden plan's items, each an object.
func planItems(t *testing.T, name string) []map[string]any {
	t.Helper()
	list, ok := valueAt(t, golden(t, "planner", name), "plan", "items").([]any)
	if !ok {
		t.Fatalf("%s: plan.items isn't an array", name)
	}
	var items []map[string]any
	for i, it := range list {
		item, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("%s: item %d isn't an object", name, i)
		}
		items = append(items, item)
	}
	return items
}

func TestPlannerGolden(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"epic-plan", "story-plan", "questions"} {
		if err := validateOutput(t, "planner", golden(t, "planner", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The golden plans are what later tasks build on, so each has what it
// stands for: the epic plan stories with no task fields, and the story plan
// tasks with a human task whose check is a file.
func TestPlannerGoldenShape(t *testing.T) {
	t.Parallel()
	if kind := valueAt(t, golden(t, "planner", "epic-plan"), "plan", "kind"); kind != "stories" {
		t.Errorf("epic-plan: kind %v, want stories", kind)
	}
	for _, item := range planItems(t, "epic-plan") {
		for _, k := range []string{"scope", "human", "task", "human_task"} {
			if _, ok := item[k]; ok {
				t.Errorf("epic-plan: %v has %s", item["id"], k)
			}
		}
	}

	if kind := valueAt(t, golden(t, "planner", "story-plan"), "plan", "kind"); kind != "tasks" {
		t.Errorf("story-plan: kind %v, want tasks", kind)
	}
	var humans, fileChecks int
	for _, item := range planItems(t, "story-plan") {
		if item["human"] != true {
			continue
		}
		humans++
		check := valueAt(t, item, "human_task", "done_when", "check")
		if valueAt(t, check, "kind") == "file" && valueAt(t, check, "path") != "" && valueAt(t, check, "contains") != "" {
			fileChecks++
		}
	}
	if humans != 1 || fileChecks != 1 {
		t.Errorf("story-plan: %d human tasks, %d with a file check and contains; want 1 and 1", humans, fileChecks)
	}
}

// filePath is a word with a / and a file extension, as the plan comment's
// file-path rule reads one.
var filePath = regexp.MustCompile(`[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]*\.[A-Za-z0-9]+`)

// No golden plan names a file in a field the plan comment shows, a human
// task's "Done when" text among them, and the story plan's task bodies do
// name the files they touch.
func TestPlannerGoldenVisibleFieldsNameNoFiles(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"epic-plan", "story-plan"} {
		plan := valueAt(t, golden(t, "planner", name), "plan")
		visible := []any{valueAt(t, plan, "why"), valueAt(t, plan, "summary"), valueAt(t, plan, "assumptions"), valueAt(t, plan, "order")}
		if name == "story-plan" {
			visible = append(visible, valueAt(t, plan, "message"))
		}
		for _, item := range planItems(t, name) {
			visible = append(visible, item["title"], item["outcome"], item["criteria"], item["scope"])
			if item["human"] == true {
				// done_when.text is shown; the other fields here aren't, but
				// only steps, links and the check may name a file.
				for _, k := range []string{"goal", "why_blocks", "estimated_time"} {
					visible = append(visible, valueAt(t, item, "human_task", k))
				}
				visible = append(visible, valueAt(t, item, "human_task", "done_when", "text"))
			}
		}
		raw, err := json.Marshal(visible)
		if err != nil {
			t.Fatal(err)
		}
		if m := filePath.FindAllString(string(raw), -1); len(m) > 0 {
			t.Errorf("%s: visible fields name %q", name, m)
		}
	}
	for _, item := range planItems(t, "story-plan") {
		if item["human"] == true {
			continue
		}
		for _, k := range []string{"context", "change"} {
			if text, ok := valueAt(t, item, "task", k).(string); !ok || !filePath.MatchString(text) {
				t.Errorf("story-plan: %v's task.%s names no file", item["id"], k)
			}
		}
	}
}

func TestPlannerPrompt(t *testing.T) {
	t.Parallel()
	p := string(shippedPrompt(t, "planner"))
	if len(p) >= MaxPromptSize {
		t.Errorf("planner.md is %d bytes, want under %d", len(p), MaxPromptSize)
	}
	for _, want := range []string{
		// The five check kinds and their fields, by the names the schema uses.
		"`label`", "`issue-type`", "`issue_type`", "`file`", "`path`", "`contains`", "`ruleset`", "`check`", "`installation`", "`owner`",
		// The levels of detail.
		"An epic's plan** is stories", "no file-level detail", "name no file paths", "`task.context`", "`task.change`",
		// A human task's "Done when" text is shown, so it names no file either.
		"and a human task's `done_when.text`. These visible fields name no file paths", "The plan comment shows `text`, so it names no file",
		// The item fields' limits come from the preamble.
		"`plan.max_tasks`", "`verify.max_diff_lines`", "preamble's list of allowed models", "`low`, `medium`, `high`, `xhigh` and `max`", "`p0` to `p3`",
		"`recommended` first",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("planner.md doesn't contain %q", want)
		}
	}
	for _, old := range []string{"ruleset-check", "app-installed"} {
		if strings.Contains(p, old) {
			t.Errorf("planner.md names the old check kind %q", old)
		}
	}
}

// plannerUnsupported are the keywords the planner's schema still never
// uses: references, which --json-schema isn't known to resolve, and
// keywords that would let it accept more than it lists.
var plannerUnsupported = []string{
	"$schema", "format", "anyOf", "allOf", "not",
	"$ref", "$defs", "definitions", "dependentSchemas", "dependentRequired",
	"patternProperties", "unevaluatedProperties", "prefixItems",
}

// The planner's schema uses oneOf and if/then/else, but only below the top
// level, where the Messages API refuses unions in a tool's input schema.
// Every object it declares lists its properties, all of them known.
func TestPlannerSchemaShape(t *testing.T) {
	t.Parallel()
	raw, err := StageSchema("planner")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"oneOf", "if", "then", "else"} {
		if _, ok := doc[k]; ok {
			t.Errorf("the top level uses %s", k)
		}
	}
	checkPlannerSchema(t, "#", doc)
}

func checkPlannerSchema(t *testing.T, at string, s map[string]any) {
	t.Helper()
	for _, k := range plannerUnsupported {
		if _, ok := s[k]; ok {
			t.Errorf("%s uses %s", at, k)
		}
	}
	if isObject(s["type"]) && s["additionalProperties"] != false {
		t.Errorf("%s is an object without additionalProperties false", at)
	}
	sub := func(at string, v any) {
		switch v := v.(type) {
		case map[string]any:
			checkPlannerSchema(t, at, v)
		case bool:
			if v {
				t.Errorf("%s is the schema true", at)
			}
		default:
			t.Errorf("%s isn't a schema", at)
		}
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for k, v := range props {
			sub(at+"/properties/"+k, v)
		}
	}
	for _, k := range []string{"items", "if", "then", "else"} {
		if v, ok := s[k]; ok {
			sub(at+"/"+k, v)
		}
	}
	if list, ok := s["oneOf"].([]any); ok {
		for i, v := range list {
			sub(at+"/oneOf/"+strconv.Itoa(i), v)
		}
	}
}

// storyTask is the story plan's T1 body, for cases that move it.
func storyTask(t *testing.T) any {
	t.Helper()
	return valueAt(t, golden(t, "planner", "story-plan"), "plan", "items", 0, "task")
}

// storyHumanTask is the story plan's T3 human task body.
func storyHumanTask(t *testing.T) any {
	t.Helper()
	return valueAt(t, golden(t, "planner", "story-plan"), "plan", "items", 2, "human_task")
}

func TestPlannerBadOutputs(t *testing.T) {
	t.Parallel()
	check := []any{"plan", "items", 2, "human_task", "done_when", "check"}
	checkAt := func(path ...any) []any { return append(append([]any{}, check...), path...) }
	checkBad(t, "planner", map[string]badOutput{
		// Levels of detail: a story has no task fields.
		"story with a scope":      {"epic-plan", "booking", []any{"plan", "items", 0, "scope"}, "at '/plan/items/0/scope': false schema"},
		"story with human":        {"epic-plan", false, []any{"plan", "items", 1, "human"}, "at '/plan/items/1/human': false schema"},
		"story with a task":       {"epic-plan", storyTask(t), []any{"plan", "items", 0, "task"}, "at '/plan/items/0/task': false schema"},
		"story with a human task": {"epic-plan", storyHumanTask(t), []any{"plan", "items", 2, "human_task"}, "at '/plan/items/2/human_task': false schema"},

		// A task has a scope, human, and exactly the body that goes with it.
		"task with no scope":             {"story-plan", remove, []any{"plan", "items", 0, "scope"}, "at '/plan/items/0': missing property 'scope'"},
		"task with no human":             {"story-plan", remove, []any{"plan", "items", 1, "human"}, "at '/plan/items/1': missing property 'human'"},
		"non-human task with no task":    {"story-plan", remove, []any{"plan", "items", 0, "task"}, "at '/plan/items/0': missing property 'task'"},
		"human task with a task body":    {"story-plan", storyTask(t), []any{"plan", "items", 2, "task"}, "at '/plan/items/2/task': false schema"},
		"human task with no human_task":  {"story-plan", remove, []any{"plan", "items", 2, "human_task"}, "at '/plan/items/2': missing property 'human_task'"},
		"non-human task with human_task": {"story-plan", storyHumanTask(t), []any{"plan", "items", 1, "human_task"}, "at '/plan/items/1/human_task': false schema"},
		"string human":                   {"story-plan", "true", []any{"plan", "items", 2, "human"}, "at '/plan/items/2/human'"},
		"blank scope":                    {"story-plan", " ", []any{"plan", "items", 0, "scope"}, "at '/plan/items/0/scope'"},

		// The check: one of five kinds, with exactly that kind's fields.
		"check kind ruleset-check":     {"story-plan", "ruleset-check", checkAt("kind"), "at '/plan/items/2/human_task/done_when/check/kind': value must be one of"},
		"check kind app-installed":     {"story-plan", "app-installed", checkAt("kind"), "at '/plan/items/2/human_task/done_when/check/kind': value must be one of"},
		"check kind command":           {"story-plan", "command", checkAt("kind"), "at '/plan/items/2/human_task/done_when/check/kind': value must be one of"},
		"file check with an owner":     {"story-plan", "mertenvg", checkAt("owner"), "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"label check with a path":      {"story-plan", map[string]any{"kind": "label", "label": "scope:infra", "path": "x"}, check, "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"label check with no label":    {"story-plan", map[string]any{"kind": "label"}, check, "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"file check with no path":      {"story-plan", remove, checkAt("path"), "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"ruleset check with a label":   {"story-plan", map[string]any{"kind": "ruleset", "check": "issuedog/verify", "label": "x"}, check, "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"issue-type check with owner":  {"story-plan", map[string]any{"kind": "issue-type", "issue_type": "Epic", "owner": "acme"}, check, "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"installation with a contains": {"story-plan", map[string]any{"kind": "installation", "owner": "acme", "contains": "x"}, check, "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"installation with a label":    {"story-plan", map[string]any{"kind": "installation", "owner": "acme", "label": "x"}, check, "at '/plan/items/2/human_task/done_when/check': 'oneOf' failed, none matched"},
		"check with no kind":           {"story-plan", remove, checkAt("kind"), "at '/plan/items/2/human_task/done_when/check': missing property 'kind'"},
		"unknown check field":          {"story-plan", "x", checkAt("value"), "additional properties 'value' not allowed"},
		"absolute path":                {"story-plan", "/etc/passwd", checkAt("path"), "at '/plan/items/2/human_task/done_when/check/path'"},
		"backslash path":               {"story-plan", `deploy\production.env`, checkAt("path"), "at '/plan/items/2/human_task/done_when/check/path'"},
		"multi-line contains":          {"story-plan", "A=1\nB=2", checkAt("contains"), "at '/plan/items/2/human_task/done_when/check/contains'"},
		"contains over 256":            {"story-plan", strings.Repeat("a", 257), checkAt("contains"), "at '/plan/items/2/human_task/done_when/check/contains': maxLength"},
		"label over 50":                {"story-plan", map[string]any{"kind": "label", "label": strings.Repeat("a", 51)}, check, "at '/plan/items/2/human_task/done_when/check/label': maxLength"},
		"owner with a space":           {"story-plan", map[string]any{"kind": "installation", "owner": "acme corp"}, check, "at '/plan/items/2/human_task/done_when/check/owner'"},
		"blank ruleset check":          {"story-plan", map[string]any{"kind": "ruleset", "check": " "}, check, "at '/plan/items/2/human_task/done_when/check/check'"},
		"blank issue type":             {"story-plan", map[string]any{"kind": "issue-type", "issue_type": ""}, check, "at '/plan/items/2/human_task/done_when/check/issue_type'"},
		"no done_when text":            {"story-plan", remove, []any{"plan", "items", 2, "human_task", "done_when", "text"}, "missing property 'text'"},

		// Every 9.3 field is filled.
		"no goal":           {"story-plan", remove, []any{"plan", "items", 2, "human_task", "goal"}, "missing property 'goal'"},
		"no why_blocks":     {"story-plan", remove, []any{"plan", "items", 2, "human_task", "why_blocks"}, "missing property 'why_blocks'"},
		"no steps":          {"story-plan", []any{}, []any{"plan", "items", 2, "human_task", "steps"}, "at '/plan/items/2/human_task/steps': minItems"},
		"no links":          {"story-plan", []any{}, []any{"plan", "items", 2, "human_task", "links"}, "at '/plan/items/2/human_task/links': minItems"},
		"no estimated_time": {"story-plan", remove, []any{"plan", "items", 2, "human_task", "estimated_time"}, "missing property 'estimated_time'"},
		"no done_when":      {"story-plan", remove, []any{"plan", "items", 2, "human_task", "done_when"}, "missing property 'done_when'"},
		"multi-line step":   {"story-plan", "Open it.\nEdit it.", []any{"plan", "items", 2, "human_task", "steps", 0}, "at '/plan/items/2/human_task/steps/0'"},

		// The 11.2 body.
		"no context":           {"story-plan", remove, []any{"plan", "items", 0, "task", "context"}, "missing property 'context'"},
		"no change":            {"story-plan", remove, []any{"plan", "items", 0, "task", "change"}, "missing property 'change'"},
		"no test_plan":         {"story-plan", remove, []any{"plan", "items", 0, "task", "test_plan"}, "missing property 'test_plan'"},
		"empty DoD":            {"story-plan", []any{}, []any{"plan", "items", 0, "task", "definition_of_done"}, "at '/plan/items/0/task/definition_of_done': minItems"},
		"blank change":         {"story-plan", " \n", []any{"plan", "items", 0, "task", "change"}, "at '/plan/items/0/task/change'"},
		"multi-line DoD":       {"story-plan", "Tests\npass", []any{"plan", "items", 0, "task", "definition_of_done", 0}, "at '/plan/items/0/task/definition_of_done/0'"},
		"task with acceptance": {"story-plan", "x", []any{"plan", "items", 0, "task", "acceptance"}, "additional properties 'acceptance' not allowed"},

		// Item IDs and dependencies.
		"ID T0":                {"story-plan", "T0", []any{"plan", "items", 0, "id"}, "at '/plan/items/0/id'"},
		"ID T01":               {"story-plan", "T01", []any{"plan", "items", 0, "id"}, "at '/plan/items/0/id'"},
		"ID X1":                {"story-plan", "X1", []any{"plan", "items", 0, "id"}, "at '/plan/items/0/id'"},
		"ID T1a":               {"story-plan", "T1a", []any{"plan", "items", 0, "id"}, "at '/plan/items/0/id'"},
		"ID 1":                 {"story-plan", "1", []any{"plan", "items", 0, "id"}, "at '/plan/items/0/id'"},
		"story ID in tasks":    {"story-plan", "S1", []any{"plan", "items", 0, "id"}, "at '/plan/items/0/id': 'S1' does not match pattern '^T'"},
		"task ID in stories":   {"epic-plan", "T1", []any{"plan", "items", 0, "id"}, "at '/plan/items/0/id': 'T1' does not match pattern '^S'"},
		"issue dependency":     {"story-plan", []any{"#12"}, []any{"plan", "items", 1, "depends_on"}, "at '/plan/items/1/depends_on/0'"},
		"story dependency":     {"story-plan", []any{"S1"}, []any{"plan", "items", 1, "depends_on"}, "at '/plan/items/1/depends_on/0': 'S1' does not match pattern '^T'"},
		"task dependency":      {"epic-plan", []any{"T1"}, []any{"plan", "items", 1, "depends_on"}, "at '/plan/items/1/depends_on/0': 'T1' does not match pattern '^S'"},
		"dependency twice":     {"story-plan", []any{"T1", "T1"}, []any{"plan", "items", 1, "depends_on"}, "at '/plan/items/1/depends_on': items at 0 and 1 are equal"},
		"no depends_on":        {"epic-plan", remove, []any{"plan", "items", 0, "depends_on"}, "missing property 'depends_on'"},
		"priority p4":          {"epic-plan", "p4", []any{"plan", "items", 0, "priority"}, "at '/plan/items/0/priority': value must be one of"},
		"priority P1":          {"story-plan", "P1", []any{"plan", "items", 1, "priority"}, "at '/plan/items/1/priority': value must be one of"},
		"effort extreme":       {"epic-plan", "extreme", []any{"plan", "items", 0, "effort"}, "at '/plan/items/0/effort': value must be one of"},
		"effort minimal":       {"story-plan", "minimal", []any{"plan", "items", 0, "effort"}, "at '/plan/items/0/effort': value must be one of"},
		"model with a space":   {"epic-plan", "claude opus", []any{"plan", "items", 0, "model"}, "at '/plan/items/0/model'"},
		"model with a comma":   {"epic-plan", "opus,sonnet", []any{"plan", "items", 0, "model"}, "at '/plan/items/0/model'"},
		"empty model":          {"epic-plan", "", []any{"plan", "items", 0, "model"}, "at '/plan/items/0/model'"},
		"no criteria":          {"epic-plan", []any{}, []any{"plan", "items", 0, "criteria"}, "at '/plan/items/0/criteria': minItems"},
		"no title":             {"epic-plan", remove, []any{"plan", "items", 0, "title"}, "missing property 'title'"},
		"no item outcome":      {"epic-plan", remove, []any{"plan", "items", 0, "outcome"}, "missing property 'outcome'"},
		"no item ID":           {"epic-plan", remove, []any{"plan", "items", 0, "id"}, "missing property 'id'"},
		"multi-line title":     {"epic-plan", "Quotes expire\n### Injected", []any{"plan", "items", 0, "title"}, "at '/plan/items/0/title'"},
		"multi-line outcome":   {"epic-plan", "Refused.\nReally.", []any{"plan", "items", 0, "outcome"}, "at '/plan/items/0/outcome'"},
		"multi-line criterion": {"epic-plan", "Works.\n- another", []any{"plan", "items", 0, "criteria", 0}, "at '/plan/items/0/criteria/0'"},
		"item extra field":     {"epic-plan", "kept", []any{"plan", "items", 0, "status"}, "additional properties 'status' not allowed"},

		// The plan.
		"kind epics":            {"epic-plan", "epics", []any{"plan", "kind"}, "at '/plan/kind': value must be one of"},
		"no kind":               {"epic-plan", remove, []any{"plan", "kind"}, "at '/plan': missing property 'kind'"},
		"no why":                {"epic-plan", remove, []any{"plan", "why"}, "at '/plan': missing property 'why'"},
		"no summary":            {"epic-plan", remove, []any{"plan", "summary"}, "at '/plan': missing property 'summary'"},
		"no order":              {"epic-plan", remove, []any{"plan", "order"}, "at '/plan': missing property 'order'"},
		"no items":              {"epic-plan", remove, []any{"plan", "items"}, "at '/plan': missing property 'items'"},
		"empty items":           {"epic-plan", []any{}, []any{"plan", "items"}, "at '/plan/items': minItems"},
		"multi-line why":        {"epic-plan", "Stories.\nReally.", []any{"plan", "why"}, "at '/plan/why'"},
		"multi-line order":      {"epic-plan", "S1\nS2", []any{"plan", "order"}, "at '/plan/order'"},
		"multi-line assumption": {"epic-plan", "One.\nTwo.", []any{"plan", "assumptions", 0}, "at '/plan/assumptions/0'"},
		"blank summary":         {"epic-plan", " \n ", []any{"plan", "summary"}, "at '/plan/summary'"},
		"blank message":         {"story-plan", " ", []any{"plan", "message"}, "at '/plan/message'"},
		"plan extra field":      {"epic-plan", "x", []any{"plan", "notes"}, "additional properties 'notes' not allowed"},

		// The output.
		"unknown outcome": {"epic-plan", "spec", []any{"outcome"}, "at '/outcome': value must be one of"},
		"no outcome":      {"epic-plan", remove, []any{"outcome"}, "missing property 'outcome'"},
		"extra field":     {"questions", "x", []any{"message"}, "additional properties 'message' not allowed"},

		// The questions are the interviewer's.
		"4 others":           {"questions", []any{"b", "c", "d", "e"}, []any{"questions", 0, "others"}, "at '/questions/0/others': maxItems"},
		"no others":          {"questions", []any{}, []any{"questions", 0, "others"}, "at '/questions/0/others': minItems"},
		"empty blocks":       {"questions", []any{}, []any{"questions", 1, "blocks"}, "at '/questions/1/blocks': minItems"},
		"block 0":            {"questions", []any{0}, []any{"questions", 0, "blocks"}, "at '/questions/0/blocks/0': minimum"},
		"multi-line text":    {"questions", "Which?\n### Injected", []any{"questions", 0, "text"}, "at '/questions/0/text'"},
		"no recommended why": {"questions", remove, []any{"questions", 0, "recommended", "why"}, "missing property 'why'"},
	})
}
