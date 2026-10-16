package github_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// closingDescribe is each closing as "<closed|reopened> by <login> <state
// reason> <commit>", for comparison, with no IDs or times.
func closingDescribe(closings []github.Closing) []string {
	var got []string
	for _, cl := range closings {
		what := "closed"
		if cl.Reopen {
			what = "reopened"
		}
		got = append(got, what+" by "+cl.By.Login+" "+cl.StateReason+" "+cl.CommitID)
	}
	return got
}

// ClosingTimeline reads the whole timeline once, and returns its closed and
// reopened events, oldest first, each with its actor, its time, a closed
// event's state_reason and the commit it names; CheckLabels returns the
// same from the timeline it reads, acting on none of them.
func TestClosingTimeline(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	f.AddComment(n, "owner", "a comment")
	ids := []int64{
		f.CloseAs(n, "owner", "not_planned"),
		f.ReopenBy(n, "mallory"),
		f.CloseByCommit(n, "mallory", "abc123", true),
		f.ReopenBy(n, "owner"),
		f.CloseAs(n, "eve", "duplicate"),
	}
	ctx, c := context.Background(), f.Client(t)
	from := len(f.Requests())
	timeline, closings, err := c.ClosingTimeline(ctx, "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	reqs := slices.DeleteFunc(f.Requests()[from:], func(r githubtest.Request) bool { return strings.HasPrefix(r.Path, "/app/") })
	if len(reqs) != 1 || reqs[0].Path != "/issues/1/timeline" {
		t.Errorf("requests %+v; want one timeline read", reqs)
	}
	want := []string{"closed by owner not_planned ", "reopened by mallory  ", "closed by mallory completed abc123", "reopened by owner  ",
		"closed by eve duplicate "}
	if got := closingDescribe(closings); !reflect.DeepEqual(got, want) {
		t.Errorf("closings %q; want %q", got, want)
	}
	for i, cl := range closings {
		if cl.ID != ids[i] || cl.At.IsZero() || i > 0 && cl.At.Before(closings[i-1].At) {
			t.Errorf("closing %d: ID %d at %v; want ID %d, in order", i, cl.ID, cl.At, ids[i])
		}
	}
	if len(timeline) != 6 || timeline[0].GetEvent() != "commented" {
		t.Errorf("timeline %d events, first %q; want the comment and the 5 closes and reopens", len(timeline), timeline[0].GetEvent())
	}
	found, err := c.CheckLabels(ctx, "o", "r", n, nil, "issuedog", discNobody{}, func(int64) bool { return false }, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := closingDescribe(found.Closings); !reflect.DeepEqual(got, want) {
		t.Errorf("CheckLabels' closings %q; want %q", got, want)
	}
	if len(f.Writes()) != 0 {
		t.Errorf("writes %+v; want none", f.Writes())
	}
}

// A commit is a merged pull request's only when GitHub lists a merged pull
// request for it.
func TestClosingMerged(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.CloseByCommit(f.AddIssue("owner", "A", ""), "mallory", "merged1", true)
	f.CloseByCommit(f.AddIssue("owner", "B", ""), "mallory", "pushed1", false)
	c := f.Client(t)
	for sha, want := range map[string]bool{"merged1": true, "pushed1": false, "unknown": false} {
		got, err := c.ClosingMerged(context.Background(), "o", "r", sha)
		if err != nil || got != want {
			t.Errorf("ClosingMerged(%s) = %t, %v; want %t", sha, got, err, want)
		}
	}
}

// A failed read of a commit's pull requests is an error, not a no.
func TestClosingMergedFails(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/commits/abc/pulls" }, 500)
	if got, err := f.Client(t).ClosingMerged(context.Background(), "o", "r", "abc"); err == nil || got {
		t.Errorf("ClosingMerged = %t, %v; want the read's error", got, err)
	}
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/issues/9/timeline" }, 500)
	if _, _, err := f.Client(t).ClosingTimeline(context.Background(), "o", "r", 9); err == nil {
		t.Error("ClosingTimeline = nil; want the read's error")
	}
}
