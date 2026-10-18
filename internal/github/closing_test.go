package github_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// closingDescribe is each closing as "<closed|reopened> by <login> <node
// id>", for comparison, with no times.
func closingDescribe(closings []github.Closing) []string {
	var got []string
	for _, cl := range closings {
		what := "closed"
		if cl.Reopen {
			what = "reopened"
		}
		got = append(got, what+" by "+cl.By.Login+" "+cl.NodeID)
	}
	return got
}

// ClosingTimeline reads the whole timeline once, and returns its closed and
// reopened events, oldest first, each with its actor, its time and its node
// ID; CheckLabels returns the same from the timeline it reads, acting on
// none of them.
func TestClosingTimeline(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	f.AddComment(n, "owner", "a comment")
	ids := []int64{
		f.CloseAs(n, "owner", "not_planned"),
		f.ReopenBy(n, "mallory"),
		f.CloseByPullRequest(n, "mallory", githubtest.ClosingPR{Merged: true}),
		f.ReopenBy(n, "owner"),
		f.CloseByCommit(n, "eve", "abc123", ""),
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
	want := []string{"closed by owner CE_9001", "reopened by mallory REE_9002", "closed by mallory CE_9003", "reopened by owner REE_9004",
		"closed by eve CE_9005"}
	if got := closingDescribe(closings); !reflect.DeepEqual(got, want) {
		t.Errorf("closings %q; want %q", got, want)
	}
	for i, cl := range closings {
		if cl.ID != ids[i] || cl.At.IsZero() || i > 0 && cl.At.Before(closings[i-1].At) {
			t.Errorf("closing %d: ID %d at %v; want ID %d, in order", i, cl.ID, cl.At, ids[i])
		}
	}
	if len(timeline) != 6 || timeline[0].GetEvent() != "commented" || timeline[5].GetCommitID() != "abc123" || timeline[3].GetCommitID() != "" {
		t.Errorf("timeline %d events; want the comment and the 5 closes and reopens, only the commit's naming it", len(timeline))
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

// A close is a merge only when its closer is a merged pull request into
// this repo's default branch, or a commit in this repo. Its state reason
// comes with it, in lower case. An event GitHub can't resolve is a close
// by nobody named, with no error.
func TestClosingCloser(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	for range 8 {
		f.AddIssue("owner", "Issue", "")
	}
	cases := []struct {
		event  int64
		want   github.ClosingCloser
		reason string
	}{
		{f.CloseByPullRequest(1, "mallory", githubtest.ClosingPR{Merged: true}), github.ClosingCloser{Merge: true, StateReason: "completed"}, "a merged pull request"},
		{f.CloseByPullRequest(2, "mallory", githubtest.ClosingPR{Merged: false}), github.ClosingCloser{StateReason: "completed"}, "an unmerged one"},
		{f.CloseByPullRequest(3, "mallory", githubtest.ClosingPR{Merged: true, Base: "dev"}), github.ClosingCloser{StateReason: "completed"}, "one merged into another branch"},
		{f.CloseByPullRequest(4, "mallory", githubtest.ClosingPR{Merged: true, Repo: "x/y"}), github.ClosingCloser{StateReason: "completed"}, "one in another repo"},
		{f.CloseByCommit(5, "mallory", "abc", ""), github.ClosingCloser{Merge: true, StateReason: "completed"}, "a commit in this repo"},
		{f.CloseByCommit(6, "mallory", "def", "x/y"), github.ClosingCloser{StateReason: "completed"}, "a commit in another repo"},
		{f.CloseAs(7, "owner", "duplicate"), github.ClosingCloser{StateReason: "duplicate"}, "a close by hand"},
		{f.CloseByPullRequest(8, "mallory", githubtest.ClosingPR{Merged: true, Repo: "O/R"}), github.ClosingCloser{Merge: true, StateReason: "completed"}, "this repo, in another case"},
	}
	f.AddIssue("owner", "Issue", "")
	f.AddIssue("owner", "Issue", "")
	forbidden := f.CloseByPullRequest(9, "mallory", githubtest.ClosingPR{Merged: true})
	f.ForbidCloser(9, forbidden)
	cases = append(cases, struct {
		event  int64
		want   github.ClosingCloser
		reason string
	}{forbidden, github.ClosingCloser{StateReason: "completed"}, "a closer GitHub won't show the App"}, struct {
		event  int64
		want   github.ClosingCloser
		reason string
	}{f.CloseByProject(10, "github-project-automation[bot]"), github.ClosingCloser{StateReason: "completed"}, "a project's workflow"})
	c := f.Client(t)
	for _, tc := range cases {
		got, err := c.ClosingCloser(context.Background(), "o", "r", "CE_"+strconv.FormatInt(tc.event, 10))
		if err != nil || got != tc.want {
			t.Errorf("%s: ClosingCloser = %+v, %v; want %+v", tc.reason, got, err, tc.want)
		}
	}
	for _, id := range []string{"CE_1", "nope", "REE_" + strconv.FormatInt(cases[0].event, 10)} {
		if got, err := c.ClosingCloser(context.Background(), "o", "r", id); err != nil || got != (github.ClosingCloser{}) {
			t.Errorf("ClosingCloser(%s) = %+v, %v; want nothing, no error", id, got, err)
		}
	}
}

// Only a FORBIDDEN or NOT_FOUND error under node says a close is no merge
// the App can tell, with no error. One of another type under node, or of
// none, is GitHub's error, which the caller asks again (#204), and once it
// has gone, the same question sees the merge.
func TestClosingCloserNodeErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "A", "")
	event := f.CloseByPullRequest(n, "mallory", githubtest.ClosingPR{Merged: true})
	id := "CE_" + strconv.FormatInt(event, 10)
	merge := github.ClosingCloser{Merge: true, StateReason: "completed"}
	c := f.Client(t)
	for _, tc := range []struct{ typ, name string }{{"", "an untyped error"}, {"INTERNAL", "an INTERNAL error"}} {
		f.FailCloser(n, event, tc.typ, "Something went wrong while executing your query.")
		for range 2 {
			if got, err := c.ClosingCloser(ctx, "o", "r", id); !errors.Is(err, github.ErrGraphQL) || got != (github.ClosingCloser{}) {
				t.Errorf("%s under node: ClosingCloser = %+v, %v; want GitHub's error, asked again", tc.name, got, err)
			}
		}
		f.MendCloser(n, event)
		if got, err := c.ClosingCloser(ctx, "o", "r", id); err != nil || got != merge {
			t.Errorf("once %s has gone: ClosingCloser = %+v, %v; want %+v", tc.name, got, err, merge)
		}
	}
	for _, typ := range []string{"FORBIDDEN", "NOT_FOUND"} {
		f.FailCloser(n, event, typ, "Resource not accessible by integration")
		if got, err := c.ClosingCloser(ctx, "o", "r", id); err != nil || got != (github.ClosingCloser{}) {
			t.Errorf("%s under node: ClosingCloser = %+v, %v; want no merge, no error", typ, got, err)
		}
		f.MendCloser(n, event)
	}
	f.FailGraphQLAt("", "Something went wrong while executing your query.", "node", "closer")
	if got, err := c.ClosingCloser(ctx, "o", "r", id); !errors.Is(err, github.ErrGraphQL) || got.Merge {
		t.Errorf("an untyped error at node.closer, beside the data: ClosingCloser = %+v, %v; want GitHub's error", got, err)
	}
	f.FailGraphQLAt("FORBIDDEN", "Resource not accessible by integration", "repository", "nameWithOwner")
	if got, err := c.ClosingCloser(ctx, "o", "r", id); !errors.Is(err, github.ErrGraphQL) || got.Merge {
		t.Errorf("FORBIDDEN on the repository: ClosingCloser = %+v, %v; want GitHub's error", got, err)
	}
	if got, err := c.ClosingCloser(ctx, "o", "r", id); err != nil || got != merge {
		t.Errorf("after the errors: ClosingCloser = %+v, %v; want %+v", got, err, merge)
	}
}

// A FORBIDDEN or NOT_FOUND error under node is logged as a close judged by
// hand, once each; an untyped one, an error, is not.
func TestClosingCloserNodeErrorsLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "A", "")
	event := f.CloseByPullRequest(n, "mallory", githubtest.ClosingPR{Merged: true})
	id := "CE_" + strconv.FormatInt(event, 10)
	c := f.Client(t)
	var warnings bytes.Buffer
	stop := logr.AddWriter(&warnings, logr.WithFilter(logr.W))
	for _, typ := range []string{"FORBIDDEN", "NOT_FOUND", ""} {
		f.FailCloser(n, event, typ, "message "+typ)
		if _, err := c.ClosingCloser(ctx, "o", "r", id); (err != nil) != (typ == "") {
			t.Errorf("%q under node: ClosingCloser error %v", typ, err)
		}
	}
	logr.Wait()
	stop()
	got := warnings.String()
	for _, want := range []string{"(FORBIDDEN: message FORBIDDEN at [node])", "(NOT_FOUND: message NOT_FOUND at [node])"} {
		if strings.Count(got, want) != 1 {
			t.Errorf("warnings %q; want %q once", got, want)
		}
	}
	if strings.Count(got, "judged as a close by hand") != 2 {
		t.Errorf("warnings %q; want two closes judged by hand, and none for the untyped error", got)
	}
}

// A failed question is an error, not a close by hand: GitHub's errors,
// other than NOT_FOUND, an answer with no repository, and a failed read.
func TestClosingCloserFails(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	event := f.CloseByPullRequest(f.AddIssue("owner", "A", ""), "mallory", githubtest.ClosingPR{Merged: true})
	c := f.Client(t)
	f.FailGraphQL("boom")
	if got, err := c.ClosingCloser(context.Background(), "o", "r", "CE_"+strconv.FormatInt(event, 10)); !errors.Is(err, github.ErrGraphQL) || got.Merge {
		t.Errorf("ClosingCloser = %+v, %v; want GitHub's error", got, err)
	}
	f.FailGraphQLAt("FORBIDDEN", "Resource not accessible by integration", "repository", "defaultBranchRef")
	if got, err := c.ClosingCloser(context.Background(), "o", "r", "CE_"+strconv.FormatInt(event, 10)); !errors.Is(err, github.ErrGraphQL) || got.Merge {
		t.Errorf("ClosingCloser with an error on the repository = %+v, %v; want GitHub's error", got, err)
	}
	if got, err := c.ClosingCloser(context.Background(), "o", "gone", "CE_"+strconv.FormatInt(event, 10)); err == nil || got.Merge {
		t.Errorf("ClosingCloser in another repo = %+v, %v; want an error", got, err)
	}
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/graphql" }, 500)
	if _, err := c.ClosingCloser(context.Background(), "o", "r", "CE_"+strconv.FormatInt(event, 10)); err == nil {
		t.Error("ClosingCloser = nil; want the read's error")
	}
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/issues/9/timeline" }, 500)
	if _, _, err := c.ClosingTimeline(context.Background(), "o", "r", 9); err == nil {
		t.Error("ClosingTimeline = nil; want the read's error")
	}
}
