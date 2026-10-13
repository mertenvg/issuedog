package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The fake's clock can't put two edits in one second, so this pins that a
// kept history is used only for the same last edit and the same body.
func TestHistoriesKeyedOnEditAndBody(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Issue", "body")
	f.EditComment(f.AddComment(1, "owner", "x"), "owner", "y")
	client := f.Client(t)
	s, err := client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	c := s.Comments[0]
	need := []content{{node: c.Node, edit: c.Edit, body: c.Body}}
	for _, tc := range []struct {
		name string
		kept kept
		hit  bool
	}{
		{"same edit and body", kept{at: c.Edit.At, body: c.Body}, true},
		{"same edit, another body", kept{at: c.Edit.At, body: "z"}, false},
		{"same body, another edit", kept{at: c.Edit.At.Add(-time.Second), body: c.Body}, false},
	} {
		before := f.HistoryQueries()
		if _, err := (histories{c.Node: tc.kept}).read(context.Background(), client, need); err != nil {
			t.Fatal(err)
		}
		if hit := f.HistoryQueries() == before; hit != tc.hit {
			t.Errorf("%s: used the kept history = %t, want %t", tc.name, hit, tc.hit)
		}
	}
}

// The fake can delete an issue, but not answer 410 or 301.
func TestGone(t *testing.T) {
	status := func(code int) error {
		return fmt.Errorf("snapshot: %w", &gogithub.ErrorResponse{Response: &http.Response{StatusCode: code}})
	}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{status(http.StatusNotFound), true},
		{status(http.StatusGone), true},
		{fmt.Errorf("snapshot: %w", &gogithub.RedirectionError{StatusCode: http.StatusMovedPermanently}), true},
		{fmt.Errorf("snapshot: %w", github.ErrTransferred), true},
		{status(http.StatusInternalServerError), false},
		{fmt.Errorf("snapshot: %w", &gogithub.RedirectionError{StatusCode: http.StatusFound}), false},
		{errors.New("boom"), false},
		{nil, false},
	} {
		if got := gone(tc.err); got != tc.want {
			t.Errorf("gone(%v) = %t, want %t", tc.err, got, tc.want)
		}
	}
}

// The fake's clock moves a second per change, so it can't change an issue
// or comment in the second the snapshot was read; this pins those rules.
func TestChangedInTheSameSecond(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	s := &github.Snapshot{UpdatedAt: at, Body: "body", Labels: []string{"stage:ready", "priority:p2"},
		Comments: []github.Comment{{ID: 7, Body: "hi", UpdatedAt: at}}}
	issue := func(sec int, body string, labels ...string) *gogithub.Issue {
		is := &gogithub.Issue{UpdatedAt: &gogithub.Timestamp{Time: at.Add(time.Duration(sec) * time.Second)}, Body: &body}
		for _, l := range labels {
			is.Labels = append(is.Labels, &gogithub.Label{Name: l})
		}
		return is
	}
	for _, tc := range []struct {
		name  string
		issue *gogithub.Issue
		want  bool
	}{
		{"same, labels in another order and case", issue(0, "body", "Priority:P2", "stage:ready"), false},
		{"older", issue(-1, "other"), false},
		{"newer", issue(1, "body", "stage:ready", "priority:p2"), true},
		{"same second, another body", issue(0, "other", "stage:ready", "priority:p2"), true},
		{"same second, another label", issue(0, "body", "stage:ready"), true},
		{"same second, another title", func() *gogithub.Issue {
			is := issue(0, "body", "stage:ready", "priority:p2")
			is.Title = new("Renamed")
			return is
		}(), true},
		{"same second, a type", func() *gogithub.Issue {
			is := issue(0, "body", "stage:ready", "priority:p2")
			is.Type = &gogithub.IssueType{Name: new("Bug")}
			return is
		}(), true},
		{"same second, a parent", func() *gogithub.Issue {
			is := issue(0, "body", "stage:ready", "priority:p2")
			is.ParentIssueURL = new("https://api.github.com/repos/o/r/issues/3")
			return is
		}(), true},
		{"same second, an empty parent", func() *gogithub.Issue {
			is := issue(0, "body", "stage:ready", "priority:p2")
			is.ParentIssueURL = new("")
			return is
		}(), false},
	} {
		if got := changed(s, tc.issue); got != tc.want {
			t.Errorf("%s: changed = %t, want %t", tc.name, got, tc.want)
		}
	}
	// A list without parent_issue_url says nothing of the parent, so a
	// sub-issue at the cursor isn't read again on every pass.
	sub := *s
	sub.Parent = 3
	if changed(&sub, issue(0, "body", "stage:ready", "priority:p2")) {
		t.Error("a sub-issue listed with no parent URL is changed, want unchanged")
	}
	if !changed(nil, issue(0, "body")) {
		t.Error("an issue with no snapshot isn't changed, want changed")
	}
	comment := func(id int64, sec int, body string) *gogithub.IssueComment {
		return &gogithub.IssueComment{ID: &id, Body: &body, UpdatedAt: &gogithub.Timestamp{Time: at.Add(time.Duration(sec) * time.Second)}}
	}
	for _, tc := range []struct {
		name    string
		comment *gogithub.IssueComment
		want    bool
	}{
		{"same", comment(7, 0, "hi"), false},
		{"new", comment(8, 0, "hi"), true},
		{"newer", comment(7, 1, "hi"), true},
		{"same second, another body", comment(7, 0, "edited"), true},
	} {
		if got := commentChanged(s, tc.comment); got != tc.want {
			t.Errorf("comment %s: changed = %t, want %t", tc.name, got, tc.want)
		}
	}
}
