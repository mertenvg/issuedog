package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// reviewsPR opens a pull request from issuedog/7-task to main on a fake
// whose git remote has that branch, and returns the fake and its number.
func reviewsPR(t *testing.T) (*githubtest.Fake, int) {
	t.Helper()
	f, _ := pullsFake(t, "issuedog/7-task")
	pr, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r",
		github.NewPullRequest{Title: "Task", Head: "issuedog/7-task", Base: "main", Body: "Closes #7"})
	if err != nil {
		t.Fatal(err)
	}
	return f, pr.Number
}

// ReviewComments lists one review's line comments, every page of 100,
// oldest first, each with its ID, author, file, line and text, and none of
// another review's. A review with none lists none.
func TestReviewComments(t *testing.T) {
	t.Parallel()
	f, n := reviewsPR(t)
	var lines []githubtest.ReviewLine
	for i := range 101 {
		lines = append(lines, githubtest.ReviewLine{Path: fmt.Sprintf("dir/f%d.go", i), Line: i + 1, Body: fmt.Sprintf("Fix `line` %d.", i+1)})
	}
	f.ReviewPR(n, "owner", github.ReviewCommented, "Earlier.", []githubtest.ReviewLine{{Path: "old.go", Line: 3, Body: "another review's"}})
	id := f.ReviewPR(n, "owner", github.ReviewChangesRequested, "See the lines.", lines)
	approved := f.ReviewPR(n, "owner", github.ReviewApproved, "", nil)
	c, ctx := f.Client(t), context.Background()
	from := len(f.Requests())
	got, err := c.ReviewComments(ctx, "o", "r", n, id)
	if err != nil {
		t.Fatal(err)
	}
	var queries []string
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == fmt.Sprintf("/pulls/%d/reviews/%d/comments", n, id) {
			queries = append(queries, r.Query)
		}
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "per_page=100") || !strings.Contains(queries[1], "page=2") {
		t.Errorf("queries %q; want two pages of 100", queries)
	}
	if len(got) != 101 {
		t.Fatalf("listed %d; want the review's 101", len(got))
	}
	for i, cm := range got {
		want := github.ReviewComment{ID: cm.ID, Author: "owner", Path: fmt.Sprintf("dir/f%d.go", i), Line: i + 1, Body: fmt.Sprintf("Fix `line` %d.", i+1)}
		if cm != want || cm.ID == 0 || i > 0 && cm.ID <= got[i-1].ID {
			t.Errorf("comment %d %+v; want %+v, oldest first, with an ID", i, cm, want)
		}
	}
	none, err := c.ReviewComments(ctx, "o", "r", n, approved)
	if err != nil || len(none) != 0 {
		t.Errorf("an approval's comments %+v, %v; want none", none, err)
	}
}

// A review the pull request hasn't got is GitHub's 404, an error that
// names the review and the pull request.
func TestReviewCommentsNotFound(t *testing.T) {
	t.Parallel()
	f, n := reviewsPR(t)
	_, err := f.Client(t).ReviewComments(context.Background(), "o", "r", n, 77)
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusNotFound || !strings.Contains(err.Error(), fmt.Sprintf("review 77 on o/r#%d", n)) {
		t.Errorf("err %v; want GitHub's 404, naming the review and the pull request", err)
	}
}

// A review is in TimelineSince by its submitted_at, as a reviewed event
// has no created_at: one submitted before since is left out, and one at or
// after it is kept, with its ID, which ReviewComments takes, its reviewer,
// its state lower case and its text.
func TestReviewInTimelineSince(t *testing.T) {
	t.Parallel()
	f, n := reviewsPR(t)
	f.ReviewPR(n, "owner", github.ReviewCommented, "Before.", nil)
	since := githubtest.Start.Add(24 * time.Hour)
	f.Advance(24 * time.Hour)
	id := f.ReviewPR(n, "owner", github.ReviewChangesRequested, "After.", []githubtest.ReviewLine{{Path: "a.go", Line: 2, Body: "Here."}})
	c, ctx := f.Client(t), context.Background()
	events, err := c.TimelineSince(ctx, "o", "r", n, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("%d events since %s; want the later review alone", len(events), since)
	}
	e := events[0]
	if e.GetEvent() != "reviewed" || e.GetID() != id || e.GetUser().GetLogin() != "owner" || e.GetState() != github.ReviewChangesRequested ||
		e.GetBody() != "After." || e.SubmittedAt == nil || e.SubmittedAt.Before(since) || e.CreatedAt != nil || e.Actor != nil {
		t.Errorf("event %+v; want review %d by owner, changes_requested, After., by its submitted_at", e, id)
	}
	comments, err := c.ReviewComments(ctx, "o", "r", n, e.GetID())
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0] != (github.ReviewComment{ID: comments[0].ID, Author: "owner", Path: "a.go", Line: 2, Body: "Here."}) {
		t.Errorf("its comments %+v; want owner's on a.go:2, Here.", comments)
	}
}
