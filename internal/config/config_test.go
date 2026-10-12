package config

import (
	"context"
	"errors"
	"os"
	"reflect"
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
		c.Loop.IdleSleep != time.Minute || c.Escalate.Paths[0] != "go.mod" {
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
			SkipMarkers:    []string{"t.Skip(", ".skip(", "xit(", "#[ignore]"},
		},
		PR: PR{TitleTypes: []string{"feat", "fix", "chore"}},
		Stages: Stages{
			Timeouts: PerStage[time.Duration]{Interviewer: 10 * time.Minute, Planner: 20 * time.Minute, Implementer: 60 * time.Minute, Verifier: 30 * time.Minute},
			MaxTurns: PerStage[int]{Interviewer: 30, Planner: 60, Implementer: 200, Verifier: 80},
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

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"typez: labels", "typez"},
		{"verify: {command_timout: 5m}", "command_timout"},
		{"stages: {timeouts: {reviewer: 5m}}", "reviewer"},
		{"plan: {max_tasks: many}", "`many` into int"},
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
