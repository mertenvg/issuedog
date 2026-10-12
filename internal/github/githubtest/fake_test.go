package githubtest

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

func TestRepeatedGETIsNotModified(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	get := func() string {
		t.Helper()
		is, _, err := c.Issues.Get(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return is.GetBody()
	}
	for range 2 {
		if got := get(); got != "body" {
			t.Fatalf("body = %q, want body", got)
		}
	}
	if got := f.NotModified(); got != 1 {
		t.Errorf("NotModified() = %d after a repeated GET, want 1", got)
	}
	f.AddComment(n, "owner", "a change")
	get()
	if got := f.NotModified(); got != 1 {
		t.Errorf("NotModified() = %d after a GET of a changed issue, want still 1", got)
	}
}

func TestFailWrite(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	f.FailWrite(2)
	var statuses []int
	for _, l := range []string{"a", "b", "c"} {
		_, resp, err := c.Issues.AddLabelsToIssue(context.Background(), "o", "r", n, []string{l})
		var er *gogithub.ErrorResponse
		if err != nil && !errors.As(err, &er) {
			t.Fatal(err)
		}
		statuses = append(statuses, resp.StatusCode)
	}
	if want := []int{http.StatusOK, http.StatusInternalServerError, http.StatusOK}; !slices.Equal(statuses, want) {
		t.Errorf("statuses = %v, want %v", statuses, want)
	}
	if got := f.State().Issues[0].Labels; !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("labels = %v, want the failed write's left out", got)
	}
	want := []Write{
		{"POST", "/issues/1/labels", `["a"]` + "\n", false},
		{"POST", "/issues/1/labels", `["b"]` + "\n", true},
		{"POST", "/issues/1/labels", `["c"]` + "\n", false},
	}
	if got := f.Writes(); !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %+v, want %+v", got, want)
	}
}

// moveStage posts a marker, moves the stage label from intake to clarifying
// and 👍s the command, in the order of plan #5 assumption 4. Unless check is
// set, it posts the marker without looking for one first.
func moveStage(t *testing.T, check bool) func(*Fake) error {
	const marker = "<!-- issuedog:transition to=clarifying cmd=1001 -->"
	return func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		s, err := c.Snapshot(ctx, "o", "r", 1)
		if err != nil {
			return err
		}
		if !check || !slices.ContainsFunc(s.Comments, func(c github.Comment) bool { return c.Body == marker }) {
			if _, _, err := c.Issues.CreateComment(ctx, "o", "r", 1, gogithub.IssueCommentRequest{Body: marker}); err != nil {
				return err
			}
		}
		if !slices.Contains(s.Labels, "stage:clarifying") {
			if _, _, err := c.Issues.AddLabelsToIssue(ctx, "o", "r", 1, []string{"stage:clarifying"}); err != nil {
				return err
			}
		}
		if slices.Contains(s.Labels, "stage:intake") {
			if _, err := c.Issues.RemoveLabelForIssue(ctx, "o", "r", 1, "stage:intake"); err != nil {
				return err
			}
		}
		_, _, err = c.Client.Reactions.CreateIssueCommentReaction(ctx, "o", "r", s.Comments[0].ID, "+1")
		return err
	}
}

func TestReplay(t *testing.T) {
	setup := func(t testing.TB) *Fake {
		f := New(t, "o/r", "issuedog")
		f.AddComment(f.AddIssue("owner", "Title", "body", "stage:intake"), "owner", "/start")
		return f
	}
	Replay(t, setup, moveStage(t, true))

	problems := replay(t, setup, moveStage(t, false))
	if len(problems) == 0 {
		t.Fatal("Replay passed a pass that posts its marker twice")
	}
	if !strings.HasPrefix(problems[0], "crash after write 1 applied: the state") {
		t.Errorf("first problem = %q, want a duplicate marker after write 1 applied", problems[0])
	}

	// A pass that retries a failed write duplicates it when GitHub had
	// already applied it. Only a crash after the write applied shows that.
	retry := func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		s, err := c.Snapshot(ctx, "o", "r", 1)
		if err != nil || len(s.Comments) > 1 {
			return err
		}
		req := gogithub.IssueCommentRequest{Body: "<!-- issuedog:transition to=clarifying -->"}
		if _, _, err := c.Issues.CreateComment(ctx, "o", "r", 1, req); err != nil {
			_, _, err = c.Issues.CreateComment(ctx, "o", "r", 1, req)
			return err
		}
		return nil
	}
	problems = replay(t, setup, retry)
	if len(problems) != 1 || !strings.HasPrefix(problems[0], "crash after write 1 applied") {
		t.Errorf("problems = %q, want only the crash after write 1 applied", problems)
	}
	// Reactions are a set: a pass that adds 👀 then 👍 and carries on after an
	// error may redo them in another order, and Replay must accept that.
	react := func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		s, err := c.Snapshot(ctx, "o", "r", 1)
		if err != nil {
			return err
		}
		for _, content := range []string{"eyes", "+1"} {
			if _, _, err := c.Client.Reactions.CreateIssueCommentReaction(ctx, "o", "r", s.Comments[0].ID, content); err != nil {
				t.Logf("react %s: %v", content, err)
			}
		}
		return nil
	}
	if problems := replay(t, setup, react); len(problems) != 0 {
		t.Errorf("problems = %q, want none for reactions redone in another order", problems)
	}
}
