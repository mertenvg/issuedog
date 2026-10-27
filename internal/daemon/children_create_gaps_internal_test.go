package daemon

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A child GitHub refuses to file, or to make blocked by a sibling, with a
// 422, is a failure no later pass gets past (childrenStuck), whose why is
// the daemon's, for the state error, and whose err, which is only logged
// (childrenStuckReport), is the write's own error, saying what failed and
// wrapping GitHub's answer. A 502 on the link isn't one: it's returned as
// it is, so a later pass tries again.
func TestChildrenRefusedCause(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	key := state.Key{Field: "event", ID: "p10"}
	children := func(f *githubtest.Fake) map[string]childFound {
		t1 := f.AddIssue("issuedog[bot]", "T1's child", "")
		t2 := f.AddIssue("issuedog[bot]", "T2's child", "")
		return map[string]childFound{"T1": {Number: t1, ID: 1002, Linked: true}, "T2": {Number: t2, ID: 1003, Linked: true}}
	}
	for _, tt := range []struct {
		name   string
		match  func(githubtest.Request) bool
		status int
		write  func(*Step, *github.Snapshot, map[string]childFound) error
		stuck  bool
		prefix string
	}{
		{"a create", func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues" }, http.StatusUnprocessableEntity,
			func(st *Step, s *github.Snapshot, _ map[string]childFound) error {
				_, err := st.childWrite(ctx, s, 1, key, childSpec{Item: "T3", Title: "T3", Body: "The task.", Stage: state.Ready}, childFound{}, map[string]int{})
				return err
			}, true, "create its child: "},
		{"a blocked-by link", isBlockRequest, http.StatusUnprocessableEntity,
			func(st *Step, _ *github.Snapshot, children map[string]childFound) error {
				return st.childrenLink(ctx, []planItem{{ID: "T2", DependsOn: []string{"T1"}}}, children)
			}, true, "item T2: block #3 by #2: "},
		{"a revised blocked-by link", isBlockRequest, http.StatusUnprocessableEntity,
			func(st *Step, _ *github.Snapshot, children map[string]childFound) error {
				return st.revisedLink(ctx, []planItem{{ID: "T2", Status: planChanged, DependsOn: []string{"T1"}}}, children)
			}, true, "item T2: block #3 by #2: "},
		{"a blocked-by link's 502", isBlockRequest, http.StatusBadGateway,
			func(st *Step, _ *github.Snapshot, children map[string]childFound) error {
				return st.childrenLink(ctx, []planItem{{ID: "T2", DependsOn: []string{"T1"}}}, children)
			}, false, "blocked-by links: item T2: block #3 by #2: "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "The story", "")
			kids := children(f)
			st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
			s, err := st.Client.Snapshot(ctx, "o", "r", 1)
			if err != nil {
				t.Fatal(err)
			}
			f.AnswerOnce(tt.match, tt.status)
			err = tt.write(st, s, kids)
			var stuck *childrenStuck
			if got := errors.As(err, &stuck); got != tt.stuck {
				t.Fatalf("error %v is a childrenStuck: %t, want %t", err, got, tt.stuck)
			}
			cause := err
			if stuck != nil {
				cause = stuck.err
			}
			var e *gogithub.ErrorResponse
			if !errors.As(cause, &e) || e.Response == nil || e.Response.StatusCode != tt.status || !strings.HasPrefix(cause.Error(), tt.prefix) {
				t.Errorf("the cause is %v; want one starting %q, wrapping GitHub's %d", cause, tt.prefix, tt.status)
			}
		})
	}
}

// isBlockRequest reports whether r adds a blocked-by link.
func isBlockRequest(r githubtest.Request) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/dependencies/blocked_by")
}
