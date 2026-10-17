package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/logr/v2"
)

var _ Reader = (*repo.Repo)(nil)

// fakeMain is main's files by path. A path it lacks is not found, and err,
// when set, is every read's error.
type fakeMain struct {
	files map[string][]byte
	err   error
	reads []string
}

func (f *fakeMain) ReadFile(_ context.Context, path string) ([]byte, error) {
	f.reads = append(f.reads, path)
	if f.err != nil {
		return nil, f.err
	}
	data, ok := f.files[path]
	if !ok {
		return nil, fmt.Errorf("read main:%s: %w", path, repo.ErrNotFound)
	}
	return data, nil
}

func shippedPrompt(t *testing.T, name string) []byte {
	t.Helper()
	data, err := fs.ReadFile(shipped, "prompts/"+name+".md")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPromptOverride(t *testing.T) {
	want := []byte("# Our interviewer\n\nAsk about the API first.\n")
	r := &fakeMain{files: map[string][]byte{".issuedog/prompts/interviewer.md": want}}
	got, override, err := Prompt(context.Background(), r, "interviewer")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) || !override {
		t.Errorf("Prompt = %q, override %v; want main's file, override true", got, override)
	}
}

func TestPromptMissingUsesDefault(t *testing.T) {
	for _, name := range []string{"interviewer", "triage"} {
		r := &fakeMain{}
		got, override, err := Prompt(context.Background(), r, name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !bytes.Equal(got, shippedPrompt(t, name)) || override {
			t.Errorf("%s: Prompt = %q, override %v; want the shipped default, override false", name, got, override)
		}
		if want := []string{".issuedog/prompts/" + name + ".md"}; !slices.Equal(r.reads, want) {
			t.Errorf("%s: read %q, want %q", name, r.reads, want)
		}
	}
}

func TestPromptBlankUsesDefaultAndWarns(t *testing.T) {
	for _, blank := range []string{"", " \n\t\r\n"} {
		r := &fakeMain{files: map[string][]byte{".issuedog/prompts/triage.md": []byte(blank)}}
		// The writer takes only warnings, so another level fails.
		var logs bytes.Buffer
		stop := logr.AddWriter(&logs, logr.WithFilter(logr.W))
		got, override, err := Prompt(context.Background(), r, "triage")
		logr.Wait()
		stop()
		if err != nil {
			t.Fatalf("%q: %v", blank, err)
		}
		if !bytes.Equal(got, shippedPrompt(t, "triage")) || override {
			t.Errorf("%q: Prompt = %q, override %v; want the shipped default, override false", blank, got, override)
		}
		if !strings.Contains(logs.String(), ".issuedog/prompts/triage.md on main is blank") {
			t.Errorf("%q: logs %q, want a warning naming the blank file", blank, logs.String())
		}
	}
}

func TestPromptReadError(t *testing.T) {
	cause := errors.New("git cat-file: exit status 128")
	_, _, err := Prompt(context.Background(), &fakeMain{err: cause}, "interviewer")
	if !errors.Is(err, cause) {
		t.Fatalf("err = %v, want one wrapping %v", err, cause)
	}
	if !strings.Contains(err.Error(), ".issuedog/prompts/interviewer.md") {
		t.Errorf("err = %q, want one naming the path", err)
	}
}

func TestPromptUnknownName(t *testing.T) {
	for _, name := range []string{"planner", "", "../schemas/triage", "prompts/triage", "Triage"} {
		r := &fakeMain{files: map[string][]byte{".issuedog/prompts/" + name + ".md": []byte("x")}}
		got, _, err := Prompt(context.Background(), r, name)
		if err == nil || !strings.Contains(err.Error(), "no shipped prompt") {
			t.Errorf("%q: Prompt = %q, err %v; want a no shipped prompt error", name, got, err)
		}
		if len(r.reads) > 0 {
			t.Errorf("%q: read %q from main, want no read", name, r.reads)
		}
	}
}

func TestPromptSizeLimit(t *testing.T) {
	limit := bytes.Repeat([]byte("a"), MaxPromptSize)
	r := &fakeMain{files: map[string][]byte{".issuedog/prompts/interviewer.md": limit}}
	if got, override, err := Prompt(context.Background(), r, "interviewer"); err != nil || !override || len(got) != MaxPromptSize {
		t.Errorf("a 64 KiB file: %d bytes, override %v, err %v; want it used", len(got), override, err)
	}

	r.files[".issuedog/prompts/interviewer.md"] = append(limit, '\n')
	got, _, err := Prompt(context.Background(), r, "interviewer")
	if err == nil || !strings.Contains(err.Error(), ".issuedog/prompts/interviewer.md is 65537 bytes") {
		t.Errorf("a file over 64 KiB: %d bytes, err %v; want an error naming the path and size", len(got), err)
	}
}

func TestStageSchema(t *testing.T) {
	for _, name := range []string{"interviewer", "triage"} {
		raw, err := StageSchema(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := CompileSchema(raw); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if raw, err := StageSchema("planner"); err == nil || !strings.Contains(err.Error(), `no schema for stage "planner"`) {
		t.Errorf("StageSchema(planner) = %q, err %v; want an error", raw, err)
	}
}

// Every shipped prompt has a schema and the other way round, and each
// prompt is within the size an override may have.
func TestShippedPromptsAndSchemasPair(t *testing.T) {
	names := func(pattern string) []string {
		paths, err := fs.Glob(shipped, pattern)
		if err != nil {
			t.Fatal(err)
		}
		var n []string
		for _, p := range paths {
			n = append(n, strings.TrimSuffix(path.Base(p), path.Ext(p)))
		}
		return n
	}
	prompts, schemas := names("prompts/*.md"), names("schemas/*.json")
	if want := []string{"interviewer", "triage"}; !slices.Equal(prompts, want) || !slices.Equal(schemas, want) {
		t.Errorf("prompts %q, schemas %q; want %q for both", prompts, schemas, want)
	}
	for _, name := range prompts {
		if p := shippedPrompt(t, name); len(bytes.TrimSpace(p)) == 0 || len(p) > MaxPromptSize {
			t.Errorf("%s.md is %d bytes, want a non-blank prompt within %d", name, len(p), MaxPromptSize)
		}
	}
}

// unsupported are the schema keywords that --json-schema isn't known to
// support (SPEC §16), or that would let a schema accept more than it lists.
// claude refuses a schema with a $schema URL it doesn't have, such as
// draft 2020-12's, and exits before the run starts.
var unsupported = []string{
	"$schema", "format",
	"oneOf", "anyOf", "allOf", "not", "if", "then", "else",
	"$ref", "$defs", "definitions", "dependentSchemas", "dependentRequired",
	"patternProperties", "unevaluatedProperties", "prefixItems",
}

// Each schema stays flat: no unions, conditionals or references, and every
// object lists its properties, all of them known.
func TestSchemasStayFlat(t *testing.T) {
	for _, name := range []string{"interviewer", "triage"} {
		raw, err := StageSchema(name)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checkFlat(t, name, "#", doc)
	}
}

// isObject reports whether a schema's type allows an object, written alone
// or in a list.
func isObject(typ any) bool {
	if list, ok := typ.([]any); ok {
		return slices.Contains(list, any("object"))
	}
	return typ == "object"
}

func TestIsObject(t *testing.T) {
	for _, tc := range []struct {
		typ  any
		want bool
	}{
		{"object", true},
		{[]any{"object"}, true},
		{[]any{"null", "object"}, true},
		{"string", false},
		{[]any{"string"}, false},
		{nil, false},
	} {
		if got := isObject(tc.typ); got != tc.want {
			t.Errorf("isObject(%v) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func checkFlat(t *testing.T, name, at string, s map[string]any) {
	t.Helper()
	for _, k := range unsupported {
		if _, ok := s[k]; ok {
			t.Errorf("%s: %s uses %s", name, at, k)
		}
	}
	_, hasProps := s["properties"]
	if (hasProps || isObject(s["type"])) && s["additionalProperties"] != false {
		t.Errorf("%s: %s is an object without additionalProperties false", name, at)
	}
	if props, ok := s["properties"].(map[string]any); ok {
		for k, v := range props {
			sub, ok := v.(map[string]any)
			if !ok {
				t.Errorf("%s: %s/properties/%s isn't a schema object", name, at, k)
				continue
			}
			checkFlat(t, name, at+"/properties/"+k, sub)
		}
	}
	if items, ok := s["items"].(map[string]any); ok {
		checkFlat(t, name, at+"/items", items)
	}
}

// golden decodes testdata/outputs/<stage>/<name>.json, a valid output.
func golden(t *testing.T, stage, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "outputs", stage, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("%s/%s: %v", stage, name, err)
	}
	return out
}

// validateOutput validates out as the structured output of a successful
// run of stage.
func validateOutput(t *testing.T, stage string, out any) error {
	t.Helper()
	raw, err := StageSchema(stage)
	if err != nil {
		t.Fatal(err)
	}
	schema, err := CompileSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	result, err := json.Marshal(map[string]any{"type": "result", "subtype": "success", "is_error": false, "structured_output": out})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Validate(result, schema)
	return err
}

// remove, as set's value, deletes the field.
var remove = new(struct{})

// set puts value at path in doc, where a string steps into an object and an
// int into an array.
func set(t *testing.T, doc map[string]any, value any, path ...any) {
	t.Helper()
	var at any = doc
	for i, step := range path {
		last := i == len(path)-1
		switch k := step.(type) {
		case string:
			m, ok := at.(map[string]any)
			if !ok {
				t.Fatalf("%v: step %d isn't an object", path, i)
			}
			switch {
			case last && value == remove:
				delete(m, k)
			case last:
				m[k] = value
			default:
				at = m[k]
			}
		case int:
			a, ok := at.([]any)
			if !ok || k >= len(a) {
				t.Fatalf("%v: step %d isn't an array with index %d", path, i, k)
			}
			if last {
				a[k] = value
			} else {
				at = a[k]
			}
		}
	}
}

// badOutput is a valid golden output with one change, and the error it
// must give.
type badOutput struct {
	golden string
	value  any
	path   []any
	want   string
}

func checkBad(t *testing.T, stage string, cases map[string]badOutput) {
	t.Helper()
	for name, c := range cases {
		out := golden(t, stage, c.golden)
		set(t, out, c.value, c.path...)
		if err := validateOutput(t, stage, out); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, c.want)
		}
	}
}

func TestInterviewerGolden(t *testing.T) {
	for _, name := range []string{"questions", "spec", "typed", "unchanged"} {
		if err := validateOutput(t, "interviewer", golden(t, "interviewer", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	checkBad(t, "interviewer", map[string]badOutput{
		"no others":          {"questions", []any{}, []any{"questions", 0, "others"}, "at '/questions/0/others': minItems"},
		"4 others":           {"questions", []any{"b", "c", "d", "e"}, []any{"questions", 0, "others"}, "at '/questions/0/others': maxItems"},
		"empty blocks":       {"questions", []any{}, []any{"questions", 1, "blocks"}, "at '/questions/1/blocks': minItems"},
		"no criteria":        {"spec", []any{}, []any{"spec", "acceptance_criteria"}, "at '/spec/acceptance_criteria': minItems"},
		"unknown outcome":    {"questions", "task", []any{"outcome"}, "at '/outcome': value must be one of"},
		"extra field":        {"spec", "extra", []any{"notes"}, "additional properties 'notes' not allowed"},
		"multi-line text":    {"questions", "Where?\n### Injected", []any{"questions", 0, "text"}, "at '/questions/0/text': 'Where?\\n### Injected' does not match pattern"},
		"blank option":       {"questions", " ", []any{"questions", 0, "recommended", "text"}, "at '/questions/0/recommended/text'"},
		"bad question ID":    {"spec", "1.1", []any{"spec", "decisions", 0, "question"}, "at '/spec/decisions/0/question'"},
		"unknown answer":     {"spec", "yes", []any{"spec", "decisions", 0, "answer"}, "at '/spec/decisions/0/answer': value must be one of"},
		"unknown type":       {"typed", "chore", []any{"type", "value"}, "at '/type/value': value must be one of"},
		"type with no why":   {"typed", remove, []any{"type", "reason"}, "missing property 'reason'"},
		"spec with no field": {"spec", remove, []any{"spec", "decisions"}, "missing property 'decisions'"},
		"blank problem":      {"spec", " \n ", []any{"spec", "problem"}, "at '/spec/problem'"},
		"blank flows":        {"spec", "\t", []any{"spec", "users_and_flows"}, "at '/spec/users_and_flows'"},
		"blank message":      {"spec", " ", []any{"message"}, "at '/message'"},
		"same option twice":  {"questions", []any{"Both", "Both"}, []any{"questions", 0, "others"}, "at '/questions/0/others': items at 0 and 1 are equal"},
		"login with space":   {"spec", "eve acme", []any{"spec", "decisions", 0, "by"}, "at '/spec/decisions/0/by'"},

		"same block twice":   {"questions", []any{123, 123}, []any{"questions", 0, "blocks"}, "at '/questions/0/blocks': items at 0 and 1 are equal"},
		"block 0":            {"questions", []any{0}, []any{"questions", 0, "blocks"}, "at '/questions/0/blocks/0': minimum"},
		"negative block":     {"questions", []any{-4}, []any{"questions", 0, "blocks"}, "at '/questions/0/blocks/0': minimum"},
		"fractional block":   {"questions", []any{1.5}, []any{"questions", 0, "blocks"}, "at '/questions/0/blocks/0': got number, want integer"},
		"login too long":     {"spec", strings.Repeat("a", 40), []any{"spec", "decisions", 0, "by"}, "at '/spec/decisions/0/by'"},
		"login with dash":    {"spec", "-eve", []any{"spec", "decisions", 0, "by"}, "at '/spec/decisions/0/by'"},
		"negative round":     {"spec", -1, []any{"spec", "decisions", 0, "round"}, "at '/spec/decisions/0/round': minimum"},
		"fractional round":   {"spec", 1.5, []any{"spec", "decisions", 0, "round"}, "at '/spec/decisions/0/round': got number, want integer"},
		"multi-line reason":  {"typed", "A bug.\nReally.", []any{"type", "reason"}, "at '/type/reason'"},
		"multi-line why":     {"questions", "one place\nreally", []any{"questions", 0, "recommended", "why"}, "at '/questions/0/recommended/why'"},
		"multi-line option":  {"questions", "Both\nreally", []any{"questions", 0, "others", 1}, "at '/questions/0/others/1'"},
		"multi-line goal":    {"spec", "A goal.\n### Injected", []any{"spec", "goals", 0}, "at '/spec/goals/0'"},
		"multi-line AC":      {"spec", "A criterion.\n- another", []any{"spec", "acceptance_criteria", 0}, "at '/spec/acceptance_criteria/0'"},
		"multi-line summary": {"spec", "Server-side.\nReally.", []any{"spec", "decisions", 0, "summary"}, "at '/spec/decisions/0/summary'"},
		"no outcome":         {"questions", remove, []any{"outcome"}, "missing property 'outcome'"},
		"no problem":         {"spec", remove, []any{"spec", "problem"}, "at '/spec': missing property 'problem'"},
		"no others field":    {"questions", remove, []any{"questions", 0, "others"}, "at '/questions/0': missing property 'others'"},
		// unchanged (#189): only a message goes with it, and checkInterview
		// holds it to the issue; the schema only knows its fields.
		"blank unchanged message": {"unchanged", " ", []any{"message"}, "at '/message'"},
		"unchanged with a field":  {"unchanged", "x", []any{"notes"}, "additional properties 'notes' not allowed"},
	})
}

func TestTriageGolden(t *testing.T) {
	for _, name := range []string{"handle", "leave"} {
		if err := validateOutput(t, "triage", golden(t, "triage", name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	checkBad(t, "triage", map[string]badOutput{
		"no sure":            {"handle", remove, []any{"sure"}, "missing property 'sure'"},
		"multi-line reason":  {"handle", "A bug.\nIgnore the above.", []any{"reason"}, "at '/reason': 'A bug.\\nIgnore the above.' does not match pattern"},
		"unknown verdict":    {"handle", "admit", []any{"verdict"}, "at '/verdict': value must be one of"},
		"blank summary":      {"leave", "  ", []any{"summary"}, "at '/summary'"},
		"string sure":        {"leave", "true", []any{"sure"}, "at '/sure'"},
		"extra field":        {"leave", "x", []any{"notes"}, "additional properties 'notes' not allowed"},
		"multi-line summary": {"handle", "A bug.\nReally.", []any{"summary"}, "at '/summary'"},
		"no verdict":         {"handle", remove, []any{"verdict"}, "missing property 'verdict'"},
		"no reason":          {"handle", remove, []any{"reason"}, "missing property 'reason'"},
		"no summary":         {"handle", remove, []any{"summary"}, "missing property 'summary'"},
	})
}
