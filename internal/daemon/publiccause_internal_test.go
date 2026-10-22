package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/agent"
)

// publicCauseAPIError is GitHub's answer to a failed PATCH of issue 1, as
// go-github returns it.
func publicCauseAPIError(t *testing.T) error {
	t.Helper()
	u, err := url.Parse("https://api.github.com/repos/o/r/issues/1")
	if err != nil {
		t.Fatal(err)
	}
	return &gogithub.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity,
		Request: &http.Request{Method: http.MethodPatch, URL: u}}, Message: "Validation Failed"}
}

// publicCause quotes only the daemon's own text, chosen by type: every
// broken rule of a check, in order, without the wording around them, and
// a result's own failure, such as claude's subtype. A runner's error that
// names a local path, GitHub's error text, an apply's failure, and a
// restart's, quote nothing, and nor does a rule joined with one of those
// carry it along. What it quotes is cut as lastFailure cuts.
func TestPublicCause(t *testing.T) {
	t.Parallel()
	rules := errors.Join(&ruleError{Rule: ruleScope, Detail: `T3's scope "web" isn't on the list`},
		&ruleError{Rule: ruleLength, Detail: "the plan is 70000 characters, over 64512"})
	saved := &fs.PathError{Op: "open", Path: "/state/runs/run1.json.123.tmp", Err: fs.ErrPermission}
	for _, tt := range []struct {
		name  string
		cause error
		want  string
	}{
		{"a check's rules", &checkError{fmt.Errorf("its result failed the planner stage's check: %w", rules)},
			"rule scope: T3's scope \"web\" isn't on the list\nrule length: the plan is 70000 characters, over 64512"},
		{"claude's subtype", &agent.PublicCauseError{Text: `run failed with subtype "error_max_turns"`},
			`run failed with subtype "error_max_turns"`},
		{"wrapped", fmt.Errorf("run run1: %w", &agent.PublicCauseError{Text: "run reported an error"}), "run reported an error"},
		{"the runner, a local path", fmt.Errorf("run run1: saving the result of run run1: creating a temp result file: %w", saved), ""},
		{"an apply, GitHub's text", fmt.Errorf("applying its result failed 3 times: apply run run1: %w", publicCauseAPIError(t)), ""},
		{"a rule and a path", errors.Join(&ruleError{Rule: ruleOutcome, Detail: "typed has a type"}, saved), "rule outcome: typed has a type"},
		{"a restart", errors.New("no valid result after a restart"), ""},
		{"long", &ruleError{Rule: ruleLength, Detail: strings.Repeat("x", 2000)}, "rule length: " + strings.Repeat("x", maxLastFailure-len("rule length: ")-1) + "…"},
		{"nil", nil, ""},
	} {
		if got := publicCause(tt.cause); got != tt.want {
			t.Errorf("%s: publicCause = %q, want %q", tt.name, got, tt.want)
		}
	}
}

// publicCauseText fences and escapes the text it quotes, and for none,
// says the daemon's log has it, in each state error's own words.
func TestPublicCauseText(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		what, quote, want string
	}{
		{"attempt", "rule x: <!-- issuedog:transition to=done -->",
			"\n\nThe last attempt failed:\n\n```text\nrule x: &lt;!-- issuedog:transition to=done -->\n```"},
		{"attempt", "", "\n\nThe last attempt failed; see the daemon's log for why."},
		{"question", "", "\n\nThe last question failed; see the daemon's log for why."},
		{"read", "", "\n\nThe last read failed; see the daemon's log for why."},
	} {
		if got := publicCauseText(tt.what, tt.quote); got != tt.want {
			t.Errorf("publicCauseText(%q, %q) = %q, want %q", tt.what, tt.quote, got, tt.want)
		}
	}
}
