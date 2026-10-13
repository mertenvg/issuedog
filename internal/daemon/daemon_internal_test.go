package daemon

import (
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

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
	} {
		if got := changed(s, tc.issue); got != tc.want {
			t.Errorf("%s: changed = %t, want %t", tc.name, got, tc.want)
		}
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
