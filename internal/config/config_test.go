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

// Every verify key that holds path patterns takes SPEC §13's globs.
func TestParseVerifyGlobs(t *testing.T) {
	t.Parallel()
	c, err := Parse([]byte("verify:\n  test_globs: ['**/*_test.go', 'e2e/**']\n  source_globs: ['**/*.go']\n" +
		"  config_paths: ['.golangci.yml']\n  binary_allow: ['assets/*.png']\n  protected_paths: ['deploy/**/prod-?.yaml', '[ab]/x']\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Verify.TestGlobs) != 2 || len(c.Verify.SourceGlobs) != 1 || len(c.Verify.ConfigPaths) != 1 ||
		len(c.Verify.BinaryAllow) != 1 || len(c.Verify.ProtectedPaths) != 2 {
		t.Errorf("unexpected verify config: %+v", c.Verify)
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

func TestModelLabelSuffixes(t *testing.T) {
	const bedrock = "us.anthropic.claude-sonnet-4-5-20250929-v1:0" // 44 characters
	long35 := strings.Repeat("é", 35)                              // 70 bytes
	label35 := strings.Repeat("ü", 35)
	short20 := strings.Repeat("ö", 20) // 40 bytes
	c, err := Parse([]byte(`models:
  allowed:
    - opus
    - {name: ` + bedrock + `, label: sonnet-4-5-bedrock}
    - {name: x.` + bedrock + `}
    - {name: ` + long35 + `}
    - {name: a` + long35 + `, label: Long}
    - {name: claude-b, label: ` + label35 + `}
    - ` + short20 + `
stages: {model: {verifier: ` + bedrock + `}}
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Models.Allowed) != 7 {
		t.Fatalf("models.allowed = %+v, want 7 entries", c.Models.Allowed)
	}
	for _, tc := range []struct{ name, suffix string }{
		{"opus", "opus"},
		{bedrock, "sonnet-4-5-bedrock"},
		{"x." + bedrock, "pic.claude-sonnet-4-5-20250929-v1:0"}, // the last 35
		{long35, long35},
		{"a" + long35, "Long"},
		{"claude-b", label35},
		{short20, short20},
	} {
		if got, ok := c.Models.LabelSuffix(tc.name); !ok || got != tc.suffix {
			t.Errorf("LabelSuffix(%q) = %q, %v; want %q", tc.name, got, ok, tc.suffix)
		}
		if got, ok := c.Models.ModelName(strings.ToUpper(tc.suffix)); !ok || got != tc.name {
			t.Errorf("ModelName(%q) = %q, %v; want %q", strings.ToUpper(tc.suffix), got, ok, tc.name)
		}
	}
	if _, ok := c.Models.LabelSuffix("Opus"); ok {
		t.Error("LabelSuffix matched Opus, which isn't an allowed name")
	}
	if got, ok := c.Models.ModelName("sonnet"); ok {
		t.Errorf("ModelName(sonnet) = %q, want no match", got)
	}
	for _, tc := range []struct{ arg, want string }{
		{bedrock, bedrock}, {"SONNET-4-5-bedrock", bedrock}, {"Opus", "opus"}, {"long", "a" + long35},
	} {
		if got, ok := c.Models.Resolve(tc.arg); !ok || got != tc.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tc.arg, got, ok, tc.want)
		}
	}
	if got, ok := c.Models.Resolve("sonnet"); ok {
		t.Errorf("Resolve(sonnet) = %q, want no match", got)
	}
}

func TestAllowsIsExact(t *testing.T) {
	m := Models{Allowed: []Model{{Name: "opus"}, {Name: "claude-x", Label: "x"}}}
	if !m.Allows("opus") || m.Allows("Opus") || m.Allows("x") || m.Allows("") {
		t.Error("Allows isn't an exact, case-sensitive match on full names")
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
		{"models: {allowed: [\"claude\\topus\"]}", "models.allowed[0]"},
		{"models: {allowed: [\"claude\\u00a0opus\"]}", "models.allowed[0]"},
		{"models: {allowed: [{name: claude-x, label: \"x\\u3000y\"}]}", "models.allowed[0].label"},
		{"models: {allowed: [opus, \"so\\0net\"]}", "models.allowed[1]"},
		{"models: {allowed: [\"opus\\u200b\"]}", "models.allowed[0]"},
		{"models: {allowed: [{label: opus}]}", "models.allowed[0]"},
		{"models: {allowed: [{name: opus, lable: o}]}", "field lable not found"},
		{"models: {allowed: [[opus]]}", "!!seq is not a model name"},
		{"models: {allowed: [{name: opus, label: " + strings.Repeat("o", 36) + "}]}", "models.allowed[0].label"},
		{"models: {allowed: [opus, {name: claude-x, label: 'x y'}]}", "models.allowed[1].label"},
		{"models: {allowed: [{name: claude-x, label: 'x,y'}]}", "models.allowed[0].label"},
		{"models: {allowed: [{name: claude-x, label: \"x\\ty\"}]}", "models.allowed[0].label"},
		{"models: {allowed: [{name: claude-x, label: \"x\\x7f\"}]}", "models.allowed[0].label"},
		{"models: {allowed: [opus, Opus]}", `"opus" and "Opus" have the same label suffix "Opus"`},
		{"models: {allowed: [opus, {name: claude-opus, label: OPUS}]}", `"opus" and "claude-opus" have the same label suffix`},
		{"models: {allowed: [us.anthropic.claude-sonnet-4-5-20250929-v1:0, eu.anthropic.claude-sonnet-4-5-20250929-v1:0]}",
			`"us.anthropic.claude-sonnet-4-5-20250929-v1:0" and "eu.anthropic.claude-sonnet-4-5-20250929-v1:0" have the same label suffix`},
		{"models: {allowed: [{name: claude-a, label: b}, {name: B, label: c}]}", `"B" is also the label suffix of "claude-a"`},
		{"stages: {fallback_model: {planner: opus}}", "!!str"},
		{"verify: {test_globs: ['**/*_test.go', '[']}", `verify.test_globs[1]: "["`},
		{"verify: {source_globs: ['src/**.go']}", "verify.source_globs[0]: \"src/**.go\" has ** inside a segment"},
		{"verify: {config_paths: ['/.golangci.yml']}", "verify.config_paths[0]: \"/.golangci.yml\" has an empty segment"},
		{"verify: {binary_allow: ['']}", "verify.binary_allow[0]: empty pattern"},
		{"verify: {protected_paths: ['secrets/']}", "verify.protected_paths[0]: \"secrets/\" has an empty segment"},
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
