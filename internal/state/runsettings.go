package state

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
)

// Agent is the agent a stage run starts (SPEC §3.3), named as in the
// stages.* config keys.
type Agent string

// The agents, in pipeline order.
const (
	Interviewer Agent = "interviewer"
	Planner     Agent = "planner"
	Implementer Agent = "implementer"
	Verifier    Agent = "verifier"
)

// Settings are a run's model, effort and fallback models, as agent.Stage
// takes them. Each left empty leaves its flag out, so claude uses its
// default.
type Settings struct {
	Model          string
	Effort         string
	FallbackModels []string
}

// ErrLabels is the cause of a RunSettings error over an issue's override
// labels: two of one kind, or one whose value can't be used.
var ErrLabels = errors.New("override labels")

// RunSettings resolves the settings of a run of agent a on an issue with
// labels, on implementer attempt attempt, counted from 1 (#93's plan, T3).
// It starts from the stage's defaults in cfg, then applies the issue's
// model:* and effort:* labels, or for the verifier its verifier-model:* and
// verifier-effort:* labels instead. Labels match ignoring case, as GitHub
// matches them. Each implementer attempt after the first raises effort one
// level, capped at stages.max_effort; an unset effort stays unset. The
// fallback models leave out the run's own model, since claude refuses its
// model as a fallback.
//
// Two labels of a kind the run uses, or one that names no allowed model's
// label suffix or no effort level, is an error matching ErrLabels: the run
// doesn't start rather than guess (assumption 3).
func RunSettings(a Agent, labels []string, cfg *config.Config, attempt int) (Settings, error) {
	model, mok := forAgent(cfg.Stages.Model, a)
	effort, eok := forAgent(cfg.Stages.Effort, a)
	fallback, fok := forAgent(cfg.Stages.FallbackModel, a)
	if !mok || !eok || !fok {
		return Settings{}, fmt.Errorf("run settings: unknown agent %q", a)
	}
	modelPrefix, effortPrefix := github.ModelPrefix, github.EffortPrefix
	if a == Verifier {
		modelPrefix, effortPrefix = github.VerifierModelPrefix, github.VerifierEffortPrefix
	}

	label, suffix, err := overrideLabel(labels, modelPrefix)
	if err != nil {
		return Settings{}, fmt.Errorf("%s run settings: %w", a, err)
	}
	if label != "" {
		name, ok := cfg.Models.ModelName(suffix)
		if !ok {
			return Settings{}, fmt.Errorf("%s run settings: %w: %q names no model in models.allowed", a, ErrLabels, label)
		}
		model = name
	}

	label, suffix, err = overrideLabel(labels, effortPrefix)
	if err != nil {
		return Settings{}, fmt.Errorf("%s run settings: %w", a, err)
	}
	if label != "" {
		level := strings.ToLower(suffix)
		if !config.ValidEffort(level) {
			return Settings{}, fmt.Errorf("%s run settings: %w: %q is not one of %s", a, ErrLabels, label, strings.Join(config.EffortLevels(), ", "))
		}
		effort = level
	}

	if a == Implementer {
		for range attempt - 1 {
			effort = config.RaiseEffort(effort, cfg.Stages.MaxEffort)
		}
	}
	fallback = slices.DeleteFunc(slices.Clone(fallback), func(m string) bool { return m == model })
	return Settings{Model: model, Effort: effort, FallbackModels: fallback}, nil
}

// forAgent returns a's value in p, or false for an unknown agent.
func forAgent[T any](p config.PerStage[T], a Agent) (T, bool) {
	switch a {
	case Interviewer:
		return p.Interviewer, true
	case Planner:
		return p.Planner, true
	case Implementer:
		return p.Implementer, true
	case Verifier:
		return p.Verifier, true
	}
	var zero T
	return zero, false
}

// overrideLabel returns the one label in labels with prefix, ignoring case,
// and the rest of it after the prefix, or an empty label if there is none.
// Two such labels are an error.
func overrideLabel(labels []string, prefix string) (label, suffix string, err error) {
	for _, l := range labels {
		if len(l) < len(prefix) || !strings.EqualFold(l[:len(prefix)], prefix) {
			continue
		}
		if label != "" {
			return "", "", fmt.Errorf("%w: %q and %q are both %s* labels", ErrLabels, label, l, prefix)
		}
		label, suffix = l, l[len(prefix):]
	}
	return label, suffix, nil
}
