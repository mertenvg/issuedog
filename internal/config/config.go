// Package config parses a target repo's .issuedog/config.yaml (SPEC §13),
// read from main (SPEC §3.4).
package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"time"

	"gopkg.in/yaml.v3"
)

// Path is the config file's path in the target repo.
const Path = ".issuedog/config.yaml"

// Reader reads a file from main. *repo.Repo satisfies it.
type Reader interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// Config is .issuedog/config.yaml. Field meanings are in SPEC §13 and the
// sections it references.
type Config struct {
	Types      string     `yaml:"types"`
	Scopes     []string   `yaml:"scopes"`
	Notify     []string   `yaml:"notify"`
	Plan       Plan       `yaml:"plan"`
	Verify     Verify     `yaml:"verify"`
	Escalate   Escalate   `yaml:"escalate"`
	PR         PR         `yaml:"pr"`
	Stages     Stages     `yaml:"stages"`
	Loop       Loop       `yaml:"loop"`
	Network    Network    `yaml:"network"`
	UsageLimit UsageLimit `yaml:"usage_limit"`
	CrossRepo  CrossRepo  `yaml:"cross_repo"`
}

type Plan struct {
	MaxTasks Int  `yaml:"max_tasks"`
	Review   bool `yaml:"review"`
}

type Verify struct {
	Commands       []string      `yaml:"commands"`
	StoryCommands  []string      `yaml:"story_commands"`
	CommandTimeout time.Duration `yaml:"command_timeout"`
	MaxDiffLines   Int           `yaml:"max_diff_lines"`
	SkipMarkers    []string      `yaml:"skip_markers"`
	TestGlobs      []string      `yaml:"test_globs"`
	SourceGlobs    []string      `yaml:"source_globs"`
	ConfigPaths    []string      `yaml:"config_paths"`
	ToolingScope   string        `yaml:"tooling_scope"`
	BinaryAllow    []string      `yaml:"binary_allow"`
	ProtectedPaths []string      `yaml:"protected_paths"`
}

type Escalate struct {
	Paths []string `yaml:"paths"`
}

type PR struct {
	TitleTypes []string `yaml:"title_types"`
}

// PerStage holds one value for each agent stage.
type PerStage[T any] struct {
	Interviewer T `yaml:"interviewer"`
	Planner     T `yaml:"planner"`
	Implementer T `yaml:"implementer"`
	Verifier    T `yaml:"verifier"`
}

type Stages struct {
	Timeouts PerStage[time.Duration] `yaml:"timeouts"`
	MaxTurns PerStage[Int]           `yaml:"max_turns"`
	// MaxBudgetUSD is optional per stage; 0 means no --max-budget-usd.
	MaxBudgetUSD PerStage[float64] `yaml:"max_budget_usd"`
}

type Loop struct {
	IdleSleep            time.Duration `yaml:"idle_sleep"`
	MaxAttempts          Int           `yaml:"max_attempts"`
	MaxQuestionsPerRound Int           `yaml:"max_questions_per_round"`
}

type Network struct {
	Allow []string `yaml:"allow"`
}

type UsageLimit struct {
	Pattern string `yaml:"pattern"`
}

type CrossRepo struct {
	Allowed []string `yaml:"allowed"`
}

// Int is an int that must be written as one, since yaml.v3 truncates 1.5 to 1.
type Int int

// UnmarshalYAML refuses any node not tagged !!int.
func (i *Int) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag != "!!int" {
		return fmt.Errorf("line %d: %q is not an integer", n.Line, n.Value)
	}
	var v int
	if err := n.Decode(&v); err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*i = Int(v)
	return nil
}

// Defaults returns the §13 defaults. Keys whose §13 values are examples for
// one repo (scopes, notify, verify commands, globs and config paths,
// tooling_scope, escalate paths, cross_repo) default to empty.
func Defaults() *Config {
	return &Config{
		Types: "issue-types",
		Plan:  Plan{MaxTasks: 10, Review: true},
		Verify: Verify{
			CommandTimeout: 20 * time.Minute,
			MaxDiffLines:   400,
			SkipMarkers:    []string{"t.Skip(", ".skip(", "xit(", "#[ignore]"},
		},
		PR: PR{TitleTypes: []string{"feat", "fix", "chore"}},
		Stages: Stages{
			Timeouts: PerStage[time.Duration]{Interviewer: 10 * time.Minute, Planner: 20 * time.Minute, Implementer: 60 * time.Minute, Verifier: 30 * time.Minute},
			MaxTurns: PerStage[Int]{Interviewer: 30, Planner: 60, Implementer: 200, Verifier: 80},
		},
		Loop:       Loop{IdleSleep: 60 * time.Second, MaxAttempts: 3, MaxQuestionsPerRound: 7},
		Network:    Network{Allow: []string{"api.anthropic.com", "proxy.golang.org", "sum.golang.org"}},
		UsageLimit: UsageLimit{Pattern: "(?i)((usage|session|weekly|opus|sonnet|fable) limit|you('|’)?ve (hit|reached) your [a-z ]*limit|you have (hit|reached) your [a-z ]*limit)"},
	}
}

// Load reads Path from main and parses it. A missing file is an error
// matching repo.ErrNotFound, not the defaults: a repo is onboarded by adding
// the file (SPEC §15), and SPEC gives a fallback only for prompts (§3.4).
func Load(ctx context.Context, r Reader) (*Config, error) {
	data, err := r.ReadFile(ctx, Path)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return c, nil
}

// Parse decodes data over the defaults and validates the result. An unknown
// key or a value of the wrong type is an error naming it. A key that is set
// replaces its default; a list is replaced, not appended to.
func Parse(data []byte) (*Config, error) {
	c := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: %w", Path, err)
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse %s: more than one YAML document", Path)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate %s: %w", Path, err)
	}
	return c, nil
}

func (c *Config) validate() error {
	if c.Types != "labels" && c.Types != "issue-types" {
		return fmt.Errorf("types: %q is not labels or issue-types", c.Types)
	}
	if c.Verify.ToolingScope != "" && !slices.Contains(c.Scopes, c.Verify.ToolingScope) {
		return fmt.Errorf("verify.tooling_scope: %q is not in scopes", c.Verify.ToolingScope)
	}
	if len(c.PR.TitleTypes) == 0 || slices.Contains(c.PR.TitleTypes, "") {
		return errors.New("pr.title_types: empty, or has an empty entry")
	}
	if c.UsageLimit.Pattern == "" {
		return errors.New("usage_limit.pattern: empty, which would match every failed run")
	}
	if _, err := regexp.Compile(c.UsageLimit.Pattern); err != nil {
		return fmt.Errorf("usage_limit.pattern: %w", err)
	}
	positive := []struct {
		key string
		v   int64
	}{
		{"plan.max_tasks", int64(c.Plan.MaxTasks)},
		{"verify.command_timeout", int64(c.Verify.CommandTimeout)},
		{"verify.max_diff_lines", int64(c.Verify.MaxDiffLines)},
		{"stages.timeouts.interviewer", int64(c.Stages.Timeouts.Interviewer)},
		{"stages.timeouts.planner", int64(c.Stages.Timeouts.Planner)},
		{"stages.timeouts.implementer", int64(c.Stages.Timeouts.Implementer)},
		{"stages.timeouts.verifier", int64(c.Stages.Timeouts.Verifier)},
		{"stages.max_turns.interviewer", int64(c.Stages.MaxTurns.Interviewer)},
		{"stages.max_turns.planner", int64(c.Stages.MaxTurns.Planner)},
		{"stages.max_turns.implementer", int64(c.Stages.MaxTurns.Implementer)},
		{"stages.max_turns.verifier", int64(c.Stages.MaxTurns.Verifier)},
		{"loop.idle_sleep", int64(c.Loop.IdleSleep)},
		{"loop.max_attempts", int64(c.Loop.MaxAttempts)},
		{"loop.max_questions_per_round", int64(c.Loop.MaxQuestionsPerRound)},
	}
	for _, p := range positive {
		if p.v <= 0 {
			return fmt.Errorf("%s: must be positive", p.key)
		}
	}
	b := c.Stages.MaxBudgetUSD
	if b.Interviewer < 0 || b.Planner < 0 || b.Implementer < 0 || b.Verifier < 0 {
		return errors.New("stages.max_budget_usd: must not be negative")
	}
	return nil
}
