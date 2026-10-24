package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Result is the JSON result that `claude -p --output-format json` prints.
// Only the fields the daemon checks are decoded.
type Result struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	// ModelUsage is keyed by the full name of each model that ran, for
	// example claude-haiku-5-5 for a --model haiku run.
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

// ModelsRan parses a run's JSON result and returns the names of the models
// that ran, sorted. A result without modelUsage reports none.
func ModelsRan(raw []byte) ([]string, error) {
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("malformed result: %w", err)
	}
	return slices.Sorted(maps.Keys(r.ModelUsage)), nil
}

// Schema is a compiled JSON schema for one stage's structured output.
type Schema struct {
	schema *jsonschema.Schema
}

// CompileSchema compiles a stage's JSON schema.
func CompileSchema(raw []byte) (*Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("stage schema: %w", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("stage.json", doc); err != nil {
		return nil, fmt.Errorf("stage schema: %w", err)
	}
	s, err := c.Compile("stage.json")
	if err != nil {
		return nil, fmt.Errorf("stage schema: %w", err)
	}
	return &Schema{schema: s}, nil
}

// PublicCauseError is a run's failure in the daemon's own words, from
// Validate, FailedSubtype or Run's timeout: it names at most one of the
// subtypes claude ends a failed run with (failedSubtypes), or the stage's
// timeout from config, and nothing from the host, such as a path, nor any
// other text of claude's, so a public comment may quote it (SPEC §5.3). A
// failure that wraps a parser's or the schema's error isn't one, as that
// text isn't the daemon's: the schema's names the daemon's working
// directory. Nor is one that quotes a type or a subtype claude isn't known
// to write, as that's claude's text, not a name the daemon knows.
type PublicCauseError struct{ Text string }

func (e *PublicCauseError) Error() string { return e.Text }

// failedSubtypes are the subtypes claude ends a failed run's result with,
// the only ones a public comment may name.
var failedSubtypes = map[string]bool{
	"error_max_turns":                     true,
	"error_max_budget_usd":                true,
	"error_during_execution":              true,
	"error_max_structured_output_retries": true,
}

// subtypeError is the failure of a result whose subtype isn't success: a
// *PublicCauseError for one of failedSubtypes, and for any other, a plain
// error, which quotes it for the daemon's log alone.
func subtypeError(subtype string) error {
	text := fmt.Sprintf("run failed with subtype %q", subtype)
	if failedSubtypes[subtype] {
		return &PublicCauseError{text}
	}
	return errors.New(text)
}

// FailedSubtype reads the subtype of raw, the output of a run that failed
// before Validate could read it, as when claude exits non-zero: it's a
// *PublicCauseError naming it, as Validate's, if raw is a result with one
// of failedSubtypes, or nil for any other output, whose run's own error
// says why it failed. Only the subtype is read, as nothing else of a
// failed run's result is the daemon's to quote.
func FailedSubtype(raw []byte) error {
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil || r.Type != "result" || !failedSubtypes[r.Subtype] {
		return nil // output that doesn't parse, such as a killed run's, names no subtype
	}
	return subtypeError(r.Subtype)
}

// Validate parses a run's JSON result and returns its structured output. It
// fails closed (SPEC §3.3): anything other than a successful, error-free run
// whose structured output matches the schema is an error, including a
// subtype it doesn't know. One of the result's own fields failing is a
// *PublicCauseError, but for a type other than result, or a subtype not
// in failedSubtypes, as those errors quote claude's text.
func Validate(raw []byte, schema *Schema) (json.RawMessage, error) {
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("malformed result: %w", err)
	}
	if r.Type != "result" {
		return nil, fmt.Errorf("unexpected result type %q", r.Type)
	}
	if r.Subtype != "success" {
		return nil, subtypeError(r.Subtype)
	}
	if r.IsError {
		return nil, &PublicCauseError{"run reported an error"}
	}
	if len(r.StructuredOutput) == 0 || string(r.StructuredOutput) == "null" {
		return nil, &PublicCauseError{"result has no structured_output"}
	}
	out, err := jsonschema.UnmarshalJSON(bytes.NewReader(r.StructuredOutput))
	if err != nil {
		return nil, fmt.Errorf("malformed structured_output: %w", err)
	}
	if err := schema.schema.Validate(out); err != nil {
		return nil, fmt.Errorf("structured_output doesn't match the stage schema: %w", err)
	}
	return r.StructuredOutput, nil
}
