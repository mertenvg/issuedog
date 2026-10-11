package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
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
