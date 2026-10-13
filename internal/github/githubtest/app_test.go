package githubtest

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/trust"
)

// noFiles is a main with no CODEOWNERS, so trust goes by role.
type noFiles struct{}

func (noFiles) ReadFile(context.Context, string) ([]byte, error) { return nil, repo.ErrNotFound }

func TestAppSlug(t *testing.T) {
	f := New(t, "o/r", "issuedog-dev")
	slug, err := f.Client(t).AppSlug(context.Background())
	if err != nil || slug != "issuedog-dev" {
		t.Errorf("AppSlug = %q, %v; want issuedog-dev", slug, err)
	}
	// The fake answers GET /app only with the App's JWT, not a token.
	if r := f.Requests(); len(r) != 1 || r[0].Path != "/app" || r[0].Status != http.StatusOK {
		t.Errorf("requests = %+v, want one GET /app answered 200", r)
	}
}

func TestSnapshotOfTransferredIssue(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Moved", "")
	f.AddIssue("owner", "Here", "")
	f.Transfer(1, "o/elsewhere")
	f.Transfer(2, "O/R") // its own repo, in another case
	c := f.Client(t)
	if _, err := c.Snapshot(context.Background(), "o", "r", 1); !errors.Is(err, github.ErrTransferred) {
		t.Errorf("Snapshot of a transferred issue: %v, want ErrTransferred", err)
	}
	if _, err := c.Snapshot(context.Background(), "o", "r", 2); err != nil {
		t.Errorf("Snapshot of an issue in its repo: %v", err)
	}
}

func TestIssuesSinceAndClose(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	f.AddIssue("owner", "One", "")
	f.AddIssue("owner", "Two", "")
	f.Close(1) // now the newest
	c := f.Client(t)
	all, err := c.IssuesSince(context.Background(), "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, is := range all {
		got = append(got, is.GetNumber())
	}
	if !reflect.DeepEqual(got, []int{2, 1}) || all[1].GetState() != "closed" {
		t.Errorf("issues = %v, want 2 then 1, closed, oldest update first", got)
	}
	since, err := c.IssuesSince(context.Background(), "o", "r", all[1].GetUpdatedAt().Time)
	if err != nil || len(since) != 1 || since[0].GetNumber() != 1 {
		t.Errorf("issues since #1's update = %v, %v; want #1 alone", since, err)
	}
	open, _, err := c.Issues.ListByRepo(context.Background(), "o", "r", nil)
	if err != nil || len(open) != 1 || open[0].GetNumber() != 2 {
		t.Errorf("open issues = %v, %v; want #2 alone", open, err)
	}
}

func TestRepoLabels(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	f.AddRepoLabel("Stage:Intake")
	created, err := github.EnsureLabels(context.Background(), f.Client(t), "o", "r", github.LabelOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(created, "stage:intake") || !slices.Contains(created, "stage:held") {
		t.Errorf("created %v, want every label but stage:intake", created)
	}
	if got := f.RepoLabels(); got[0] != "Stage:Intake" || len(got) != len(created)+1 {
		t.Errorf("repo labels = %v, want Stage:Intake and the %d created", got, len(created))
	}
	again, err := github.EnsureLabels(context.Background(), f.Client(t), "o", "r", github.LabelOptions{})
	if err != nil || len(again) != 0 {
		t.Errorf("EnsureLabels again created %v, %v; want none", again, err)
	}
	if _, _, err := f.Client(t).Issues.CreateLabel(context.Background(), "o", "r", gogithub.CreateIssueLabelRequest{Name: "STAGE:HELD"}); err == nil {
		t.Error("creating a label that exists in another case succeeded, want a 422")
	}
}

func TestPermission(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	f.SetPermission("Owner", "admin")
	f.SetPermission("reader", "read")
	set, err := trust.Load(context.Background(), noFiles{}, f.Client(t).Client, "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	for login, want := range map[string]bool{"owner": true, "reader": false, "stranger": false} {
		if got, err := set.Trusted(context.Background(), login); err != nil || got != want {
			t.Errorf("Trusted(%s) = %t, %v; want %t", login, got, err, want)
		}
	}
}

func TestRequestLog(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	for range 2 {
		if _, _, err := c.Issues.Get(context.Background(), "o", "r", n); err != nil {
			t.Fatal(err)
		}
	}
	f.FailWriteAfter(1)
	if _, _, err := c.Issues.AddLabelsToIssue(context.Background(), "o", "r", n, []string{"x"}); err == nil {
		t.Fatal("the failed write succeeded")
	}
	var got []Request
	for _, r := range f.Requests() {
		if r.Path != "/app/installations/1/access_tokens" {
			got = append(got, r)
		}
	}
	want := []Request{
		{Method: "GET", Path: "/issues/1", Status: 200},
		{Method: "GET", Path: "/issues/1", Status: 304, Conditional: true},
		{Method: "POST", Path: "/issues/1/labels", Status: 500},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("requests = %+v, want %+v", got, want)
	}
}
