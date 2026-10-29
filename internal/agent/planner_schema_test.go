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
	for _, name := range []string{"epic-plan", "story-plan", "story-revision", "questions"} {
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

// The golden revision has what it stands for: an item of each status, a
// kept and a dropped one with their ID, title and status only, and a
// changed and a new one written in full.
func TestPlannerGoldenRevisionShape(t *testing.T) {
	t.Parallel()
	statuses := map[any]bool{}
	for _, item := range planItems(t, "story-revision") {
		statuses[item["status"]] = true
		switch item["status"] {
		case "kept", "dropped":
			if len(item) != 3 {
				t.Errorf("story-revision: %s item %v has %d fields, want id, title and status", item["status"], item["id"], len(item))
			}
		case "changed", "new":
			for _, k := range []string{"outcome", "criteria", "depends_on", "scope", "human", "task"} {
				if _, ok := item[k]; !ok {
					t.Errorf("story-revision: %s item %v has no %s", item["status"], item["id"], k)
				}
			}
		default:
			t.Errorf("story-revision: item %v has status %v", item["id"], item["status"])
		}
	}
	if len(statuses) != 4 {
		t.Errorf("story-revision: statuses %v, want one item of each", statuses)
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
		// A revision's statuses, and their rules.
		"`children.md`", "## Revised plans", "- `kept`:", "- `changed`:", "- `new`:", "- `dropped`:",
		"List every child exactly once, never as `new`", "A closed child can only be `kept`", "On a first plan, leave `status` out of every item.",
		// The run's clone has no main branch.
		runClone,
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
	if strings.Contains(p, "checkout of `main`.") {
		t.Errorf("planner.md says the run is in a checkout of `main`, a branch its clone doesn't have")
	}

	// A human task's "Done when" text is shown in the plan comment, so only
	// its steps, links and check may name files (#266). Each place the
	// prompt says where a human task's files go says so too, in the exact
	// words, so that a rewording which keeps the words but flips the meaning
	// fails here.
	detail := plannerPromptSection(t, p, "Levels of detail")
	var filesGo string
	for _, para := range strings.Split(detail, "\n\n") {
		for _, s := range strings.Split(para, ". ") {
			if strings.Contains(s, "`task.context`") && filesGo == "" {
				filesGo = s
			}
		}
	}
	if want := "and a human task in its `steps`, `links` and `check`"; !strings.Contains(filesGo, want) {
		t.Errorf("planner.md's sentence on where files go doesn't say %q: %q", want, filesGo)
	}
	var taskBody string
	for _, line := range strings.Split(detail, "\n") {
		if strings.HasPrefix(line, "- **A task**") {
			taskBody = line
			break
		}
	}
	if want := "apart from a human task's `done_when.text`, isn't shown in the plan comment"; !strings.Contains(taskBody, want) {
		t.Errorf("planner.md's task bullet doesn't say %q: %q", want, taskBody)
	}
	// The paragraph on a human task's files is the one that names its steps,
	// links, check and done_when.text, wherever it is in the section.
	var humanFiles string
	for _, para := range strings.Split(plannerPromptSection(t, p, "Human tasks"), "\n\n") {
		names := true
		for _, field := range []string{"`steps`", "`links`", "`check`", "`done_when.text`"} {
			names = names && strings.Contains(para, field)
		}
		if names {
			humanFiles = para
			break
		}
	}
	if humanFiles == "" {
		t.Errorf("planner.md has no paragraph on human tasks that names `steps`, `links`, `check` and `done_when.text`")
	}
	for _, want := range []string{
		"A human task's `steps`, `links` and `check` go only into its own issue, so they may name files.",
		"Its other fields name none,",
		"`done_when.text` is shown in the plan comment.",
	} {
		if !strings.Contains(humanFiles, want) {
			t.Errorf("planner.md's paragraph on a human task's files doesn't say %q: %q", want, humanFiles)
		}
	}
}

// plannerPromptSection returns the text of planner.md's "## heading"
// section, up to the next heading. A missing section fails the test.
func plannerPromptSection(t *testing.T, p, heading string) string {
	t.Helper()
	_, section, ok := strings.Cut(p, "\n## "+heading+"\n")
	if !ok {
		t.Fatalf("planner.md has no %q section", heading)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return section
}

// The schema, which every planner run is sent, says as the prompt does that
// a human task's "Done when" text is shown in the plan comment and names no
// file (#266): on the text itself, on the human task, and on the plan,
// whose fields are all shown but the task bodies. Each says it in the exact
// words, so that a rewording which keeps the words but flips the meaning
// fails here.
func TestPlannerSchemaDoneWhenShown(t *testing.T) {
	t.Parallel()
	raw, err := StageSchema("planner")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	plan := []any{"properties", "plan"}
	humanTask := append(append([]any{}, plan...), "properties", "items", "items", "properties", "human_task")
	text := append(append([]any{}, humanTask...), "properties", "done_when", "properties", "text")
	for _, d := range []struct {
		name string
		path []any
		want []string // the phrases its description says it in
	}{
		{"done_when.text", text, []string{
			"The plan comment shows it, so it names no file",
			"a path goes only in check",
		}},
		{"human_task", humanTask, []string{
			"Its steps, links and check may name files.",
			"Its done_when text is also shown in the plan comment, so it names none.",
		}},
		// The plan says none of its shown fields names a file, and that a
		// human task's done_when text is shown, unlike the rest of its body.
		{"plan", plan, []string{
			"Every field here is shown in the plan comment, so none of them names a file",
			"except the task bodies: task, and human_task but for its done_when text.",
		}},
	} {
		desc, ok := valueAt(t, doc, append(append([]any{}, d.path...), "description")...).(string)
		if !ok {
			t.Errorf("%s has no description", d.name)
			continue
		}
		for _, want := range d.want {
			if !strings.Contains(desc, want) {
				t.Errorf("%s's description doesn't say %q: %q", d.name, want, desc)
			}
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

// A human task's why_blocks is the reason alone: the daemon writes "#12
// and #13 can't proceed until <why_blocks>" (#284). The schema, which every
// planner run is sent, and planner.md both say so in the same words, and
// neither asks for the items by ID, so a planner following either writes
// no "T2 can't …" after the daemon's "can't proceed until".
func TestPlannerWhyBlocks(t *testing.T) {
	t.Parallel()
	raw, err := StageSchema("planner")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	schema, ok := valueAt(t, doc, "properties", "plan", "properties", "items", "items", "properties",
		"human_task", "properties", "why_blocks", "description").(string)
	if !ok {
		t.Fatal("why_blocks has no description")
	}
	_, prompt, ok := strings.Cut(string(shippedPrompt(t, "planner")), "\n- `why_blocks`: ")
	if !ok {
		t.Fatal("planner.md has no why_blocks bullet")
	}
	prompt, _, _ = strings.Cut(prompt, "\n")
	const phrase = `in one line that reads on after "can't proceed until"`
	byID := regexp.MustCompile(`(?i)\bby ID\b|\bwhich items\b`)
	for _, d := range []struct{ name, text string }{{"the schema", schema}, {"planner.md", prompt}} {
		if !strings.Contains(d.text, phrase) {
			t.Errorf("%s's why_blocks doesn't say %q: %q", d.name, phrase, d.text)
		}
		if m := byID.FindString(d.text); m != "" {
			t.Errorf("%s's why_blocks asks for the items (%q): %q", d.name, m, d.text)
		}
	}
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
		"item extra field":     {"epic-plan", "x", []any{"plan", "items", 0, "notes"}, "additional properties 'notes' not allowed"},

		// A revision's statuses: a kept or dropped item has its ID, title and
		// status only, and a changed or new one is written in full.
		"unknown status":            {"story-revision", "renamed", []any{"plan", "items", 0, "status"}, "at '/plan/items/0/status': value must be one of"},
		"kept item with an outcome": {"story-revision", "Still done.", []any{"plan", "items", 0, "outcome"}, "invalid propertyName 'outcome'"},
		"dropped item with a scope": {"story-revision", "infra", []any{"plan", "items", 2, "scope"}, "invalid propertyName 'scope'"},
		"kept story with criteria":  {"epic-plan", "kept", []any{"plan", "items", 0, "status"}, "invalid propertyName 'criteria'"},
		"changed item, no outcome":  {"story-revision", remove, []any{"plan", "items", 1, "outcome"}, "missing property 'outcome'"},
		"new task with no scope":    {"story-revision", remove, []any{"plan", "items", 3, "scope"}, "missing property 'scope'"},
		"new task with no body":     {"story-revision", remove, []any{"plan", "items", 3, "task"}, "missing property 'task'"},

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
