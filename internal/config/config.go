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
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/mertenvg/issuedog/internal/verify/glob"
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
	Models     Models     `yaml:"models"`
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
	// Model, Effort and FallbackModel are optional per stage; empty means
	// no --model, --effort or --fallback-model, so the CLI's default applies.
	Model         PerStage[string]   `yaml:"model"`
	Effort        PerStage[string]   `yaml:"effort"`
	FallbackModel PerStage[[]string] `yaml:"fallback_model"`
	// MaxEffort caps the effort that retries raise to (RaiseEffort).
	MaxEffort string `yaml:"max_effort"`
}

// stageValue is one stage's key and value in a PerStage.
type stageValue[T any] struct {
	stage string
	v     T
}

func (p PerStage[T]) list() []stageValue[T] {
	return []stageValue[T]{{"interviewer", p.Interviewer}, {"planner", p.Planner}, {"implementer", p.Implementer}, {"verifier", p.Verifier}}
}

// Models lists the model names and aliases that stage defaults, commands and
// labels may use. An empty list allows no model overrides.
type Models struct {
	Allowed []Model `yaml:"allowed"`
}

// Model is one entry of models.allowed: a plain name, such as "opus", or a
// map {name: ..., label: ...} whose optional label is a short label suffix.
type Model struct {
	Name  string `yaml:"name"`
	Label string `yaml:"label"`
}

// labelSuffixMax is the longest label suffix: GitHub caps a label name at 50
// characters, and "verifier-model:" takes 15 of them.
const labelSuffixMax = 35

// UnmarshalYAML takes a plain name, or a map of name and label. yaml.v3
// doesn't pass KnownFields on to a node's Decode, so other keys are refused
// here.
func (m *Model) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if err := n.Decode(&m.Name); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		return nil
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			if k := n.Content[i]; k.Value != "name" && k.Value != "label" {
				return fmt.Errorf("line %d: field %s not found in a models.allowed entry", k.Line, k.Value)
			}
		}
		type plain Model
		if err := n.Decode((*plain)(m)); err != nil {
			return fmt.Errorf("line %d: %w", n.Line, err)
		}
		return nil
	}
	return fmt.Errorf("line %d: %s is not a model name or {name, label}", n.Line, n.ShortTag())
}

// Suffix returns the entry's label suffix, as in model:<suffix>: its label if
// it has one, else its name if that fits in 35 characters, else the name's
// last 35 characters.
func (m Model) Suffix() string {
	if m.Label != "" {
		return m.Label
	}
	r := []rune(m.Name)
	if len(r) <= labelSuffixMax {
		return m.Name
	}
	return string(r[len(r)-labelSuffixMax:])
}

// Allows reports whether name is an entry's full name. The match is exact,
// so it is case-sensitive, and a label suffix doesn't count.
func (m Models) Allows(name string) bool {
	return slices.ContainsFunc(m.Allowed, func(e Model) bool { return e.Name == name })
}

// LabelSuffix returns the label suffix of the allowed model name, matched
// exactly, for its model:* and verifier-model:* labels.
func (m Models) LabelSuffix(name string) (string, bool) {
	for _, e := range m.Allowed {
		if e.Name == name {
			return e.Suffix(), true
		}
	}
	return "", false
}

// ModelName returns the full name of the allowed model whose label suffix is
// suffix, ignoring case, as GitHub matches label names.
func (m Models) ModelName(suffix string) (string, bool) {
	for _, e := range m.Allowed {
		if strings.EqualFold(e.Suffix(), suffix) {
			return e.Name, true
		}
	}
	return "", false
}

// Resolve returns the full name that arg names: an allowed full name,
// matched exactly, or else a label suffix, matched as ModelName does.
// validateModels makes the two unambiguous.
func (m Models) Resolve(arg string) (string, bool) {
	if m.Allows(arg) {
		return arg, true
	}
	return m.ModelName(arg)
}

// effortLevels are the claude CLI's --effort levels (Claude Code 2.1.296),
// lowest first. Config can't add to them.
var effortLevels = []string{"low", "medium", "high", "xhigh", "max"}

// EffortLevels returns the effort levels, lowest first.
func EffortLevels() []string {
	return slices.Clone(effortLevels)
}

// ValidEffort reports whether level is one of EffortLevels.
func ValidEffort(level string) bool {
	return slices.Contains(effortLevels, level)
}

// RaiseEffort returns the level one above level, capped at limit. It never
// lowers a level already at or above limit, and returns an unset or unknown
// level, or any level under an unknown limit, unchanged.
func RaiseEffort(level, limit string) string {
	i, top := slices.Index(effortLevels, level), slices.Index(effortLevels, limit)
	if i < 0 || i >= top {
		return level
	}
	return effortLevels[i+1]
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
		return fmt.Errorf("line %d: %s %q is not an integer", n.Line, n.ShortTag(), n.Value)
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
			Timeouts:  PerStage[time.Duration]{Interviewer: 10 * time.Minute, Planner: 20 * time.Minute, Implementer: 60 * time.Minute, Verifier: 30 * time.Minute},
			MaxTurns:  PerStage[Int]{Interviewer: 30, Planner: 60, Implementer: 200, Verifier: 80},
			MaxEffort: "max",
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
	switch err := dec.Decode(&yaml.Node{}); {
	case err == nil:
		return nil, fmt.Errorf("parse %s: more than one YAML document", Path)
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("parse %s: %w", Path, err)
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
	if err := c.Verify.validateGlobs(); err != nil {
		return fmt.Errorf("validate globs: %w", err)
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
	return c.validateModels()
}

// validateGlobs checks that every path pattern in the verify keys that hold
// them is one glob.Match can match (SPEC §13), naming the key and the
// entry of the first that isn't.
func (v *Verify) validateGlobs() error {
	for _, k := range []struct {
		key      string
		patterns []string
	}{
		{"verify.test_globs", v.TestGlobs},
		{"verify.source_globs", v.SourceGlobs},
		{"verify.config_paths", v.ConfigPaths},
		{"verify.binary_allow", v.BinaryAllow},
		{"verify.protected_paths", v.ProtectedPaths},
	} {
		for i, p := range k.patterns {
			if err := glob.Valid(p); err != nil {
				return fmt.Errorf("%s[%d]: %w", k.key, i, err)
			}
		}
	}
	return nil
}

// validateModels checks models.allowed and the stages' model and effort keys.
func (c *Config) validateModels() error {
	allowed := c.Models.Allowed
	for i, m := range allowed {
		// A model name is a command argument and a comma-joined
		// --fallback-model entry, and it or its label is a label suffix.
		if !token(m.Name) {
			return fmt.Errorf("models.allowed[%d]: %q is empty, has a space or comma, or isn't printable", i, m.Name)
		}
		if m.Label != "" && (!token(m.Label) || utf8.RuneCountInString(m.Label) > labelSuffixMax) {
			return fmt.Errorf("models.allowed[%d].label: %q has a space or comma, isn't printable, or is longer than %d characters", i, m.Label, labelSuffixMax)
		}
	}
	// Labels match ignoring case, and /model takes a name or a suffix.
	for i, a := range allowed {
		for _, b := range allowed[i+1:] {
			if strings.EqualFold(a.Suffix(), b.Suffix()) {
				return fmt.Errorf("models.allowed: %q and %q have the same label suffix %q, ignoring case", a.Name, b.Name, b.Suffix())
			}
		}
		for j, b := range allowed {
			if i != j && strings.EqualFold(a.Name, b.Suffix()) {
				return fmt.Errorf("models.allowed: %q is also the label suffix of %q, ignoring case", a.Name, b.Name)
			}
		}
	}
	for _, s := range c.Stages.Model.list() {
		if s.v != "" && !c.Models.Allows(s.v) {
			return fmt.Errorf("stages.model.%s: %q is not in models.allowed", s.stage, s.v)
		}
	}
	for _, s := range c.Stages.FallbackModel.list() {
		for _, m := range s.v {
			if !c.Models.Allows(m) {
				return fmt.Errorf("stages.fallback_model.%s: %q is not in models.allowed", s.stage, m)
			}
		}
	}
	levels := strings.Join(effortLevels, ", ")
	for _, s := range c.Stages.Effort.list() {
		if s.v != "" && !ValidEffort(s.v) {
			return fmt.Errorf("stages.effort.%s: %q is not one of %s", s.stage, s.v, levels)
		}
	}
	if !ValidEffort(c.Stages.MaxEffort) {
		return fmt.Errorf("stages.max_effort: %q is not one of %s", c.Stages.MaxEffort, levels)
	}
	return nil
}

// token reports whether s is a nonempty run of printable characters without
// whitespace or a comma.
func token(s string) bool {
	return s != "" && !strings.ContainsRune(s, ',') && strings.IndexFunc(s, unicode.IsSpace) < 0 &&
		strings.IndexFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) < 0
}
