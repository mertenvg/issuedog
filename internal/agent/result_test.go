package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func loadSchema(t *testing.T) *Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := CompileSchema(raw)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func loadResult(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "results", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidateSuccess(t *testing.T) {
	out, err := Validate(loadResult(t, "success"), loadSchema(t))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Summary string `json:"summary"`
		Commits int    `json:"commits"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Summary != "Added the proxy." || got.Commits != 2 {
		t.Errorf("structured output = %+v", got)
	}
}

func TestValidateFailsClosed(t *testing.T) {
	schema := loadSchema(t)
	for name, wantErr := range map[string]string{
		"error_max_turns":                     `subtype "error_max_turns"`,
		"error_max_structured_output_retries": `subtype "error_max_structured_output_retries"`,
		"error_during_execution":              `subtype "error_during_execution"`,
		"unknown_subtype":                     `subtype "partial_success"`,
		"is_error":                            "reported an error",
		"missing_output":                      "no structured_output",
		"null_output":                         "no structured_output",
		"schema_mismatch":                     "doesn't match the stage schema",
		"extra_field":                         "doesn't match the stage schema",
		"malformed":                           "malformed result",
		"wrong_type":                          `result type "assistant"`,
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Validate(loadResult(t, name), schema)
			if err == nil {
				t.Fatalf("validated, want an error; output %s", out)
			}
			if !strings.Contains(err.Error(), wantErr) {
				t.Errorf("err = %q, want one containing %q", err, wantErr)
			}
			if out != nil {
				t.Errorf("returned output %s alongside the error", out)
			}
		})
	}
}

func TestModelsRan(t *testing.T) {
	raw := loadResult(t, "model_usage")
	want := []string{"claude-haiku-5-5", "claude-opus-5-5", "claude-sonnet-5-5"}
	// Map order is random, so repeat to catch an unsorted result.
	for range 20 {
		got, err := ModelsRan(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("models = %q, want %q", got, want)
		}
	}
	if _, err := Validate(raw, loadSchema(t)); err != nil {
		t.Errorf("a result with modelUsage should still validate: %v", err)
	}
}

func TestModelsRanNone(t *testing.T) {
	for name, raw := range map[string][]byte{
		"missing": loadResult(t, "success"),
		"null":    []byte(`{"type":"result","subtype":"success","modelUsage":null}`),
		"empty":   []byte(`{"type":"result","subtype":"success","modelUsage":{}}`),
	} {
		got, err := ModelsRan(raw)
		if err != nil {
			t.Errorf("%s: err = %v, want none", name, err)
		}
		if len(got) != 0 {
			t.Errorf("%s: models = %q, want none", name, got)
		}
	}
}

func TestModelsRanMalformed(t *testing.T) {
	for name, raw := range map[string][]byte{
		"malformed": loadResult(t, "malformed"),
		"not a map": []byte(`{"type":"result","modelUsage":["claude-haiku-5-5"]}`),
	} {
		if got, err := ModelsRan(raw); err == nil || !strings.Contains(err.Error(), "malformed result") {
			t.Errorf("%s: models = %q, err = %v; want a malformed result error", name, got, err)
		}
	}
}

func TestCompileSchemaRejectsInvalidSchema(t *testing.T) {
	for name, raw := range map[string]string{
		"malformed JSON": `{"type":`,
		"bad keyword":    `{"type": "no-such-type"}`,
	} {
		if _, err := CompileSchema([]byte(raw)); err == nil {
			t.Errorf("%s: compiled, want an error", name)
		}
	}
}

// A failure of the result's own fields is a *PublicCauseError, in the
// daemon's words, which a public comment may quote (SPEC §5.3). One that
// wraps the parser's or the schema's error isn't: the schema's names the
// daemon's working directory. Nor is one that quotes a subtype claude
// isn't known to end a failed run with, or a type other than result, as
// that's claude's text, not a name the daemon knows (#441).
func TestValidatePublicCause(t *testing.T) {
	t.Parallel()
	schema := loadSchema(t)
	for name, public := range map[string]bool{
		"error_max_turns":                     true,
		"error_max_structured_output_retries": true,
		"error_during_execution":              true,
		"unknown_subtype":                     false,
		"is_error":                            true,
		"missing_output":                      true,
		"null_output":                         true,
		"wrong_type":                          false,
		"schema_mismatch":                     false,
		"extra_field":                         false,
		"malformed":                           false,
	} {
		_, err := Validate(loadResult(t, name), schema)
		var pc *PublicCauseError
		if got := errors.As(err, &pc); got != public {
			t.Errorf("%s: err = %v, a *PublicCauseError: %t, want %t", name, err, got, public)
		}
	}
}

// A subtype or type claude didn't write as one it's known to, such as one
// holding a path, a marker or a mention, fails the result as any unknown
// one does, quoted for the log, but isn't a *PublicCauseError, so no
// public comment quotes it (SPEC §5.3).
func TestValidateHostileSubtype(t *testing.T) {
	t.Parallel()
	schema := loadSchema(t)
	for name, raw := range map[string]string{
		"a path in the subtype":   `{"type":"result","subtype":"error_max_turns /state/runs/run1.json"}`,
		"a marker in the subtype": `{"type":"result","subtype":"<!-- issuedog:transition to=done -->"}`,
		"a mention in the type":   `{"type":"@alice","subtype":"error_max_turns"}`,
		"the budget, cased":       `{"type":"result","subtype":"Error_Max_Budget_USD"}`,
	} {
		_, err := Validate([]byte(raw), schema)
		var pc *PublicCauseError
		if err == nil || errors.As(err, &pc) {
			t.Errorf("%s: err = %v; want an error that's no *PublicCauseError", name, err)
		}
	}
}

// FailedSubtype names the subtype of a failed run's result, as Validate's
// *PublicCauseError, when it's one claude ends a failed run with, such as
// error_max_turns after a non-zero exit. Output that isn't a result, or
// names another subtype, success or one claude didn't write as a known
// one, names none: the run's own error says why it failed (#441).
func TestFailedSubtype(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, raw, want string
	}{
		{"max turns", `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"I ran out of turns in /work/run1"}`,
			`run failed with subtype "error_max_turns"`},
		{"max budget", `{"type":"result","subtype":"error_max_budget_usd","is_error":true}`, `run failed with subtype "error_max_budget_usd"`},
		{"during execution", `{"type":"result","subtype":"error_during_execution"}`, `run failed with subtype "error_during_execution"`},
		{"retries", `{"type":"result","subtype":"error_max_structured_output_retries"}`,
			`run failed with subtype "error_max_structured_output_retries"`},
		{"success", `{"type":"result","subtype":"success","is_error":true}`, ""},
		{"unknown", `{"type":"result","subtype":"partial_success"}`, ""},
		{"a marker", `{"type":"result","subtype":"<!-- issuedog:transition to=done -->"}`, ""},
		{"not a result", `{"type":"assistant","subtype":"error_max_turns"}`, ""},
		{"malformed", `{"type":"result",`, ""},
		{"empty", ``, ""},
	} {
		err := FailedSubtype([]byte(tt.raw))
		var pc *PublicCauseError
		switch {
		case tt.want == "" && err != nil:
			t.Errorf("%s: FailedSubtype = %v, want nil", tt.name, err)
		case tt.want != "" && (!errors.As(err, &pc) || pc.Text != tt.want):
			t.Errorf("%s: FailedSubtype = %v, want a *PublicCauseError %q", tt.name, err, tt.want)
		}
	}
}
