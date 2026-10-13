package config

import (
	"context"
	"errors"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/repo"
)

func TestParseRepoConfig(t *testing.T) {
	data, err := os.ReadFile("../../" + Path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Types != "labels" || c.Verify.ToolingScope != "tooling" || len(c.Verify.Commands) != 3 ||
		c.Verify.CommandTimeout != 20*time.Minute || c.Stages.Timeouts.Implementer != time.Hour ||
		c.Loop.IdleSleep != time.Minute || c.Escalate.Paths[0] != "go.mod" ||
		!slices.Equal(c.Verify.SkipMarkers, Defaults().Verify.SkipMarkers) {
		t.Errorf("unexpected config: %+v", c)
	}
}

func TestParseEmptyGivesDefaults(t *testing.T) {
	want := &Config{
		Types: "issue-types",
		Plan:  Plan{MaxTasks: 10, Review: true},
		Verify: Verify{
			CommandTimeout: 20 * time.Minute,
			MaxDiffLines:   400,
			SkipMarkers:    Defaults().Verify.SkipMarkers, // pinned by TestParseRepoConfig
		},
		PR: PR{TitleTypes: []string{"feat", "fix", "chore"}},
		Stages: Stages{
			Timeouts:  PerStage[time.Duration]{Interviewer: 10 * time.Minute, Planner: 20 * time.Minute, Implementer: 60 * time.Minute, Verifier: 30 * time.Minute},
			MaxTurns:  PerStage[Int]{Interviewer: 30, Planner: 60, Implementer: 200, Verifier: 80},
			MaxEffort: "max", // model, effort and fallback_model stay unset
		},
		Loop:       Loop{IdleSleep: time.Minute, MaxAttempts: 3, MaxQuestionsPerRound: 7},
		Network:    Network{Allow: []string{"api.anthropic.com", "proxy.golang.org", "sum.golang.org"}},
		UsageLimit: UsageLimit{Pattern: "(?i)((usage|session|weekly|opus|sonnet|fable) limit|you('|’)?ve (hit|reached) your [a-z ]*limit|you have (hit|reached) your [a-z ]*limit)"},
	}
	for _, in := range []string{"", "# only a comment\n", "{}"} {
		got, err := Parse([]byte(in))
		if err != nil {
			t.Fatalf("Parse(%q): %v", in, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Parse(%q) =\n%+v\nwant\n%+v", in, got, want)
		}
	}
}

func TestParseOverrides(t *testing.T) {
	c, err := Parse([]byte("plan: {review: false}\nstages: {max_budget_usd: {planner: 2.5}}\nverify: {skip_markers: []}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Plan.Review || c.Plan.MaxTasks != 10 || c.Stages.MaxBudgetUSD.Planner != 2.5 || len(c.Verify.SkipMarkers) != 0 {
		t.Errorf("unexpected config: %+v", c)
	}
}

func TestParseModelsAndEffort(t *testing.T) {
	c, err := Parse([]byte(`models: {allowed: [opus, sonnet, claude-haiku-5-5]}
stages:
  model: {interviewer: sonnet, planner: opus, implementer: opus, verifier: claude-haiku-5-5}
  effort: {interviewer: low, planner: high, implementer: medium, verifier: xhigh}
  fallback_model: {implementer: [sonnet, claude-haiku-5-5]}
  max_effort: xhigh
`))
	if err != nil {
		t.Fatal(err)
	}
	s := c.Stages
	if s.Model != (PerStage[string]{"sonnet", "opus", "opus", "claude-haiku-5-5"}) ||
		s.Effort != (PerStage[string]{"low", "high", "medium", "xhigh"}) ||
		!slices.Equal(s.FallbackModel.Implementer, []string{"sonnet", "claude-haiku-5-5"}) ||
		s.FallbackModel.Planner != nil || s.MaxEffort != "xhigh" || len(c.Models.Allowed) != 3 {
		t.Errorf("unexpected stages: %+v, models: %+v", s, c.Models)
	}
	if !c.Models.Allows("opus") || c.Models.Allows("fable") || c.Models.Allows("") {
		t.Errorf("Allows disagrees with models.allowed %v", c.Models.Allowed)
	}
}

func TestEffortLevels(t *testing.T) {
	levels := EffortLevels()
	if !slices.Equal(levels, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("EffortLevels = %v", levels)
	}
	levels[0] = "none"
	if !ValidEffort("low") || ValidEffort("none") || ValidEffort("") || ValidEffort("High") {
		t.Error("ValidEffort disagrees with EffortLevels, or EffortLevels shares its slice")
	}
	for _, tc := range []struct{ level, limit, want string }{
		{"low", "max", "medium"},
		{"high", "max", "xhigh"},
		{"xhigh", "max", "max"},
		{"max", "max", "max"},
		{"medium", "high", "high"},
		{"high", "high", "high"},
		{"max", "high", "max"}, // above the cap: not lowered
		{"", "max", ""},        // unset stays unset
		{"extreme", "max", "extreme"},
		{"low", "", "low"},
	} {
		if got := RaiseEffort(tc.level, tc.limit); got != tc.want {
			t.Errorf("RaiseEffort(%q, %q) = %q, want %q", tc.level, tc.limit, got, tc.want)
		}
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"typez: labels", "typez"},
		{"verify: {command_timout: 5m}", "command_timout"},
		{"stages: {timeouts: {reviewer: 5m}}", "reviewer"},
		{"plan: {max_tasks: many}", `"many" is not an integer`},
		{"plan: {max_tasks: [1]}", "!!seq"},
		{"types: labels\n---\n[", "parse"},
		{"plan: {max_tasks: 1.5}", `"1.5" is not an integer`},
		{"loop: {max_attempts: '3'}", `"3" is not an integer`},
		{"types: labels\n---\ntypez: x", "more than one YAML document"},
		{"usage_limit: {pattern: ''}", "usage_limit.pattern: empty"},
		{"pr: {title_types: [feat, '']}", "title_types"},
		{"scopes: tooling", "!!str"},
		{"verify: {command_timeout: soon}", "soon"},
		{"plan: [1]", "!!seq"},
		{"types: jira", "types"},
		{"plan: {max_tasks: 0}", "plan.max_tasks"},
		{"loop: {idle_sleep: -1s}", "loop.idle_sleep"},
		{"stages: {max_budget_usd: {verifier: -1}}", "max_budget_usd"},
		{"usage_limit: {pattern: '('}", "usage_limit.pattern"},
		{"scopes: [a]\nverify: {tooling_scope: tooling}", "tooling_scope"},
		{"pr: {title_types: []}", "title_types"},
		{"stages: {effort: {planner: extreme}}", "stages.effort.planner"},
		{"stages: {effort: {verifier: High}}", "stages.effort.verifier"},
		{"stages: {model: {implementer: opus}}", "stages.model.implementer"},
		{"models: {allowed: [sonnet]}\nstages: {model: {interviewer: opus}}", "stages.model.interviewer"},
		{"models: {allowed: [opus]}\nstages: {fallback_model: {verifier: [opus, sonnet]}}", "stages.fallback_model.verifier"},
		{"stages: {fallback_model: {planner: ['']}}", "stages.fallback_model.planner"},
		{"stages: {max_effort: extreme}", "stages.max_effort"},
		{"stages: {max_effort: ''}", "stages.max_effort"},
		{"models: {allowed: [opus, '']}", "models.allowed"},
		{"models: {allowed: ['opus,sonnet']}", "models.allowed"},
		{"models: {allowed: ['claude opus']}", "models.allowed"},
		{"models: {allowed: opus}", "!!str"},
		{"stages: {fallback_model: {planner: opus}}", "!!str"},
	} {
		_, err := Parse([]byte(tc.in))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("Parse(%q) error = %v, want one containing %q", tc.in, err, tc.want)
		}
	}
}

type fakeReader map[string]string

func (f fakeReader) ReadFile(_ context.Context, path string) ([]byte, error) {
	s, ok := f[path]
	if !ok {
		return nil, repo.ErrNotFound
	}
	return []byte(s), nil
}

func TestLoad(t *testing.T) {
	c, err := Load(context.Background(), fakeReader{Path: "types: labels\nscopes: [x]\n"})
	if err != nil || c.Types != "labels" || c.Scopes[0] != "x" {
		t.Errorf("Load = %+v, %v", c, err)
	}
	if _, err := Load(context.Background(), fakeReader{}); !errors.Is(err, repo.ErrNotFound) {
		t.Errorf("Load of missing file: err = %v, want ErrNotFound", err)
	}
	if _, err := Load(context.Background(), fakeReader{Path: "bogus: 1"}); err == nil {
		t.Error("Load of bad file: want error")
	}
}

var _ Reader = (*repo.Repo)(nil)
