package github_test

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// readsSince is the reads the fake answered since request from, as
// "GET <path> <status> conditional|plain", without the token's.
func readsSince(f *githubtest.Fake, from int) []string {
	var got []string
	for _, r := range f.Requests()[from:] {
		if strings.HasPrefix(r.Path, "/app/") {
			continue // the first request's token
		}
		kind := map[bool]string{true: "conditional", false: "plain"}[r.Conditional]
		got = append(got, fmt.Sprintf("%s %s %d %s", r.Method, r.Path, r.Status, kind))
	}
	return got
}

// IssueReactions lists the reactions to the issue itself, oldest first, and
// none of its comments'. A second call with nothing changed is a
// conditional request answered 304. A deleted reaction is gone from the
// next read, which is a 200.
func TestIssueReactions(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	at := func(s int) time.Time { return githubtest.Start.Add(time.Duration(s) * time.Second) }
	n := f.AddIssue("owner", "Human task", "body")
	other := f.AddIssue("owner", "Other", "body")
	comment := f.AddComment(n, "owner", "/done")
	f.AddReaction(comment, "owner", "+1")
	f.AddIssueReaction(other, "owner", "+1")
	up := f.AddIssueReaction(n, "owner", "+1")
	eyes := f.AddIssueReaction(n, "issuedog[bot]", "eyes")
	ctx, c := context.Background(), f.Client(t)

	got, err := c.IssueReactions(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	want := []github.Reaction{
		{ID: up, User: github.Author{Login: "owner", Type: "User"}, Content: "+1", CreatedAt: at(6)},
		{ID: eyes, User: github.Author{Login: "issuedog[bot]", Type: "Bot"}, Content: "eyes", CreatedAt: at(7)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reactions = %+v, want only the issue's own %+v", got, want)
	}

	from, notModified := len(f.Requests()), f.NotModified()
	if again, err := c.IssueReactions(ctx, "o", "r", n); err != nil || !reflect.DeepEqual(again, want) {
		t.Errorf("reactions again = %+v, %v; want %+v", again, err, want)
	}
	if got, want := readsSince(f, from), []string{"GET /issues/1/reactions 304 conditional"}; !slices.Equal(got, want) {
		t.Errorf("requests of the second read = %q, want %q", got, want)
	}
	if got := f.NotModified(); got != notModified+1 {
		t.Errorf("NotModified() = %d after the second read, want %d", got, notModified+1)
	}

	if !f.DeleteIssueReaction(n, up) {
		t.Fatalf("DeleteIssueReaction = false, want true")
	}
	from = len(f.Requests())
	got, err = c.IssueReactions(ctx, "o", "r", n)
	if err != nil || !reflect.DeepEqual(got, want[1:]) {
		t.Errorf("reactions after a delete = %+v, %v; want %+v", got, err, want[1:])
	}
	if got, want := readsSince(f, from), []string{"GET /issues/1/reactions 200 conditional"}; !slices.Equal(got, want) {
		t.Errorf("requests after a delete = %q, want %q", got, want)
	}

	if _, err := c.IssueReactions(ctx, "o", "r", 999); err == nil || !strings.Contains(err.Error(), "list reactions to o/r#999: ") {
		t.Errorf("reactions to a missing issue: err = %v, want one naming o/r#999", err)
	}
}

// IssueReactions reads every page of 100, oldest first, and a repeat of an
// unchanged listing is a conditional 304 for each page. An issue with none
// lists none.
func TestIssueReactionsPages(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Busy", "body")
	none := f.AddIssue("owner", "Quiet", "body")
	const count = 103
	for i := range count {
		f.AddIssueReaction(n, fmt.Sprintf("user%d", i), "+1")
	}
	ctx, c := context.Background(), f.Client(t)
	got, err := c.IssueReactions(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != count || got[0].User.Login != "user0" || got[count-1].User.Login != fmt.Sprintf("user%d", count-1) {
		t.Errorf("%d reactions, want %d, from user0 to user%d", len(got), count, count-1)
	}
	from := len(f.Requests())
	if again, err := c.IssueReactions(ctx, "o", "r", n); err != nil || !reflect.DeepEqual(again, got) {
		t.Errorf("reactions again: %d, %v; want the same %d", len(again), err, count)
	}
	want := []string{"GET /issues/1/reactions 304 conditional", "GET /issues/1/reactions 304 conditional"}
	if got := readsSince(f, from); !slices.Equal(got, want) {
		t.Errorf("requests of the second read = %q, want %q", got, want)
	}
	if got, err := c.IssueReactions(ctx, "o", "r", none); err != nil || len(got) != 0 {
		t.Errorf("reactions to #%d = %+v, %v; want none", none, got, err)
	}
}
