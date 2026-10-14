package state

import (
	"errors"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
)

// longModel is longer than a label suffix, so its labels use its last 35
// characters.
const longModel = "claude-opus-5-5-20261001-extended-preview"

// runConfig has a default model and effort for every stage, and fallbacks
// for the implementer and verifier.
func runConfig() *config.Config {
	c := config.Defaults()
	c.Models.Allowed = []config.Model{{Name: "opus"}, {Name: "sonnet"}, {Name: "haiku"}, {Name: "claude-fable-5", Label: "fable"}, {Name: longModel}}
	c.Stages.Model = config.PerStage[string]{Interviewer: "sonnet", Planner: "opus", Implementer: "sonnet", Verifier: "opus"}
	c.Stages.Effort = config.PerStage[string]{Interviewer: "low", Planner: "high", Implementer: "medium", Verifier: "high"}
	c.Stages.FallbackModel = config.PerStage[[]string]{Implementer: []string{"opus", "haiku"}, Verifier: []string{"sonnet"}}
	return c
}

// unset clears the stage defaults, so every run uses the CLI's.
func unset(c *config.Config) {
	c.Stages.Model = config.PerStage[string]{}
	c.Stages.Effort = config.PerStage[string]{}
	c.Stages.FallbackModel = config.PerStage[[]string]{}
}

func TestRunSettings(t *testing.T) {
	longSuffix := config.Model{Name: longModel}.Suffix()
	for _, c := range []struct {
		name    string
		agent   Agent
		labels  []string
		attempt int
		edit    func(*config.Config)
		want    Settings
	}{
		{name: "interviewer defaults", agent: Interviewer, attempt: 1,
			want: Settings{Model: "sonnet", Effort: "low"}},
		{name: "triage has the interviewer's defaults", agent: Triage, attempt: 1,
			want: Settings{Model: "sonnet", Effort: "low"}},
		{name: "triage takes model and effort labels", agent: Triage, attempt: 1, labels: []string{"model:haiku", "effort:high", "verifier-effort:max"},
			want: Settings{Model: "haiku", Effort: "high"}},
		{name: "planner defaults", agent: Planner, attempt: 1,
			want: Settings{Model: "opus", Effort: "high"}},
		{name: "implementer defaults", agent: Implementer, attempt: 1,
			want: Settings{Model: "sonnet", Effort: "medium", FallbackModels: []string{"opus", "haiku"}}},
		{name: "verifier defaults", agent: Verifier, attempt: 1,
			want: Settings{Model: "opus", Effort: "high", FallbackModels: []string{"sonnet"}}},
		{name: "no defaults", agent: Implementer, attempt: 1, edit: unset,
			want: Settings{}},
		{name: "other labels ignored", agent: Implementer, attempt: 1, labels: []string{"stage:ready", "priority:p1", "scope:daemon", "modelling", "effort"},
			want: Settings{Model: "sonnet", Effort: "medium", FallbackModels: []string{"opus", "haiku"}}},

		{name: "model label", agent: Interviewer, attempt: 1, labels: []string{"model:haiku"},
			want: Settings{Model: "haiku", Effort: "low"}},
		{name: "effort label", agent: Planner, attempt: 1, labels: []string{"effort:xhigh"},
			want: Settings{Model: "opus", Effort: "xhigh"}},
		{name: "model and effort labels", agent: Implementer, attempt: 1, labels: []string{"model:haiku", "effort:low"},
			want: Settings{Model: "haiku", Effort: "low", FallbackModels: []string{"opus"}}},
		{name: "labels without defaults", agent: Planner, attempt: 1, labels: []string{"model:sonnet", "effort:max"}, edit: unset,
			want: Settings{Model: "sonnet", Effort: "max"}},
		{name: "verifier-model label", agent: Verifier, attempt: 1, labels: []string{"verifier-model:haiku"},
			want: Settings{Model: "haiku", Effort: "high", FallbackModels: []string{"sonnet"}}},
		{name: "verifier-effort label", agent: Verifier, attempt: 1, labels: []string{"verifier-effort:low"},
			want: Settings{Model: "opus", Effort: "low", FallbackModels: []string{"sonnet"}}},
		{name: "model label by its label suffix", agent: Implementer, attempt: 1, labels: []string{"model:fable"},
			want: Settings{Model: "claude-fable-5", Effort: "medium", FallbackModels: []string{"opus", "haiku"}}},
		{name: "model label by a long name's last 35 characters", agent: Implementer, attempt: 1, labels: []string{"model:" + longSuffix},
			want: Settings{Model: longModel, Effort: "medium", FallbackModels: []string{"opus", "haiku"}}},
		{name: "fallbacks leave out the run's model", agent: Implementer, attempt: 1, labels: []string{"model:opus"},
			want: Settings{Model: "opus", Effort: "medium", FallbackModels: []string{"haiku"}}},

		{name: "verifier ignores model and effort labels", agent: Verifier, attempt: 1, labels: []string{"model:haiku", "effort:low"},
			want: Settings{Model: "opus", Effort: "high", FallbackModels: []string{"sonnet"}}},
		{name: "implementer ignores verifier labels", agent: Implementer, attempt: 1, labels: []string{"verifier-model:haiku", "verifier-effort:max"},
			want: Settings{Model: "sonnet", Effort: "medium", FallbackModels: []string{"opus", "haiku"}}},
		{name: "planner ignores verifier labels", agent: Planner, attempt: 1, labels: []string{"verifier-model:haiku", "verifier-effort:low"},
			want: Settings{Model: "opus", Effort: "high"}},

		{name: "attempt 2 raises effort", agent: Implementer, attempt: 2,
			want: Settings{Model: "sonnet", Effort: "high", FallbackModels: []string{"opus", "haiku"}}},
		{name: "attempt 3 raises effort twice", agent: Implementer, attempt: 3,
			want: Settings{Model: "sonnet", Effort: "xhigh", FallbackModels: []string{"opus", "haiku"}}},
		{name: "attempt 3 raises an effort label", agent: Implementer, attempt: 3, labels: []string{"effort:low"},
			want: Settings{Model: "sonnet", Effort: "high", FallbackModels: []string{"opus", "haiku"}}},
		{name: "attempt 3 stops at max", agent: Implementer, attempt: 3, labels: []string{"effort:xhigh"},
			want: Settings{Model: "sonnet", Effort: "max", FallbackModels: []string{"opus", "haiku"}}},
		{name: "attempt 1 under a cap", agent: Implementer, attempt: 1, edit: func(c *config.Config) { c.Stages.MaxEffort = "high" },
			want: Settings{Model: "sonnet", Effort: "medium", FallbackModels: []string{"opus", "haiku"}}},
		{name: "attempt 2 reaches the cap", agent: Implementer, attempt: 2, edit: func(c *config.Config) { c.Stages.MaxEffort = "high" },
			want: Settings{Model: "sonnet", Effort: "high", FallbackModels: []string{"opus", "haiku"}}},
		{name: "attempt 3 stays at the cap", agent: Implementer, attempt: 3, edit: func(c *config.Config) { c.Stages.MaxEffort = "high" },
			want: Settings{Model: "sonnet", Effort: "high", FallbackModels: []string{"opus", "haiku"}}},
		{name: "a label above the cap isn't lowered", agent: Implementer, attempt: 3, labels: []string{"effort:max"}, edit: func(c *config.Config) { c.Stages.MaxEffort = "high" },
			want: Settings{Model: "sonnet", Effort: "max", FallbackModels: []string{"opus", "haiku"}}},
		{name: "unset effort stays unset on attempt 2", agent: Implementer, attempt: 2, edit: unset,
			want: Settings{}},
		{name: "unset effort stays unset on attempt 3", agent: Implementer, attempt: 3, labels: []string{"model:opus"}, edit: unset,
			want: Settings{Model: "opus"}},
		{name: "attempts don't raise the verifier's effort", agent: Verifier, attempt: 3,
			want: Settings{Model: "opus", Effort: "high", FallbackModels: []string{"sonnet"}}},
		{name: "attempts don't raise the planner's effort", agent: Planner, attempt: 3,
			want: Settings{Model: "opus", Effort: "high"}},
		{name: "attempt 2 doesn't raise the interviewer's effort", agent: Interviewer, attempt: 2,
			want: Settings{Model: "sonnet", Effort: "low"}},
		{name: "attempt 3 doesn't raise the interviewer's effort", agent: Interviewer, attempt: 3,
			want: Settings{Model: "sonnet", Effort: "low"}},

		{name: "label case", agent: Implementer, attempt: 1, labels: []string{"Model:HAIKU", "EFFORT:High"},
			want: Settings{Model: "haiku", Effort: "high", FallbackModels: []string{"opus"}}},
		{name: "verifier label case", agent: Verifier, attempt: 1, labels: []string{"Verifier-Model:Fable", "VERIFIER-EFFORT:max"},
			want: Settings{Model: "claude-fable-5", Effort: "max", FallbackModels: []string{"sonnet"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := runConfig()
			if c.edit != nil {
				c.edit(cfg)
			}
			got, err := RunSettings(c.agent, c.labels, cfg, c.attempt)
			if err != nil {
				t.Fatalf("RunSettings: %v", err)
			}
			if got.Model != c.want.Model || got.Effort != c.want.Effort || !slices.Equal(got.FallbackModels, c.want.FallbackModels) {
				t.Errorf("RunSettings = %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestRunSettingsKeepsConfig(t *testing.T) {
	cfg := runConfig()
	got, err := RunSettings(Implementer, []string{"model:opus"}, cfg, 1)
	if err != nil {
		t.Fatalf("RunSettings: %v", err)
	}
	if want := []string{"haiku"}; !slices.Equal(got.FallbackModels, want) {
		t.Fatalf("FallbackModels = %q, want %q", got.FallbackModels, want)
	}
	if want := []string{"opus", "haiku"}; !slices.Equal(cfg.Stages.FallbackModel.Implementer, want) {
		t.Errorf("stages.fallback_model.implementer after RunSettings = %q, want %q", cfg.Stages.FallbackModel.Implementer, want)
	}
}

func TestRunSettingsErrors(t *testing.T) {
	for _, c := range []struct {
		name   string
		agent  Agent
		labels []string
	}{
		{"duplicate model labels", Implementer, []string{"model:opus", "model:sonnet"}},
		{"duplicate effort labels, in any case", Planner, []string{"effort:low", "EFFORT:high"}},
		{"duplicate verifier-model labels", Verifier, []string{"verifier-model:opus", "verifier-model:haiku"}},
		{"duplicate verifier-effort labels", Verifier, []string{"verifier-effort:low", "verifier-effort:max"}},
		{"unknown effort level", Implementer, []string{"effort:extreme"}},
		{"unknown verifier effort level", Verifier, []string{"verifier-effort:extreme"}},
		{"empty effort level", Interviewer, []string{"effort:"}},
		{"model not allowed", Implementer, []string{"model:gpt"}},
		{"verifier model not allowed", Verifier, []string{"verifier-model:gpt"}},
		{"empty model", Planner, []string{"model:"}},
		{"full name of a model with a label suffix", Implementer, []string{"model:claude-fable-5"}},
		{"full name of a long model", Implementer, []string{"model:" + longModel}},
		// Every run checks all four kinds, including those it doesn't use.
		{"verifier with an unknown effort level", Verifier, []string{"effort:extreme"}},
		{"verifier with a model not allowed", Verifier, []string{"model:gpt"}},
		{"verifier with duplicate model labels", Verifier, []string{"model:gpt", "model:haiku"}},
		{"verifier with duplicate effort labels", Verifier, []string{"effort:extreme", "effort:low"}},
		{"implementer with an unknown verifier effort level", Implementer, []string{"verifier-effort:extreme"}},
		{"implementer with duplicate verifier-model labels", Implementer, []string{"verifier-model:opus", "verifier-model:haiku"}},
		{"planner with a verifier model not allowed", Planner, []string{"verifier-model:gpt"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := RunSettings(c.agent, c.labels, runConfig(), 1)
			if !errors.Is(err, ErrLabels) {
				t.Errorf("RunSettings = %+v, %v; want an error matching ErrLabels", got, err)
			}
		})
	}

	got, err := RunSettings("reviewer", nil, runConfig(), 1)
	if err == nil || errors.Is(err, ErrLabels) {
		t.Errorf("RunSettings for an unknown agent = %+v, %v; want an error not matching ErrLabels", got, err)
	}
}
