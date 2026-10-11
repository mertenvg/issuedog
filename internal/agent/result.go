package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

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

// Validate parses a run's JSON result and returns its structured output. It
// fails closed (SPEC §3.3): anything other than a successful, error-free run
// whose structured output matches the schema is an error, including a
// subtype it doesn't know.
func Validate(raw []byte, schema *Schema) (json.RawMessage, error) {
	var r Result
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("malformed result: %w", err)
	}
	if r.Type != "result" {
		return nil, fmt.Errorf("unexpected result type %q", r.Type)
	}
	if r.Subtype != "success" {
		return nil, fmt.Errorf("run failed with subtype %q", r.Subtype)
	}
	if r.IsError {
		return nil, errors.New("run reported an error")
	}
	if len(r.StructuredOutput) == 0 || string(r.StructuredOutput) == "null" {
		return nil, errors.New("result has no structured_output")
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
