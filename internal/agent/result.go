package agent

import (
	"bytes"
	"encoding/json"
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

// PublicCauseError is Validate's failure for a result that parsed but
// didn't succeed, in the daemon's own words: it quotes at most claude's
// type or subtype, and nothing from the host, such as a path, so a public
// comment may quote it (SPEC §5.3). A failure that wraps a parser's or the
// schema's error isn't one, as that text isn't the daemon's: the schema's
// names the daemon's working directory.
type PublicCauseError struct{ Text string }

func (e *PublicCauseError) Error() string { return e.Text }

// Validate parses a run's JSON result and returns its structured output. It
// fails closed (SPEC §3.3): anything other than a successful, error-free run
// whose structured output matches the schema is an error, including a
// subtype it doesn't know. One of the result's own fields failing is a
// *PublicCauseError.
func Validate(raw []byte, schema *Schema) (json.RawMessage, error) {
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("malformed result: %w", err)
	}
	if r.Type != "result" {
		return nil, &PublicCauseError{fmt.Sprintf("unexpected result type %q", r.Type)}
	}
	if r.Subtype != "success" {
		return nil, &PublicCauseError{fmt.Sprintf("run failed with subtype %q", r.Subtype)}
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
