package githubtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// transferFake is o/r with issue 1, moved, which has a label, two
// comments and a 👍 on the first, and issue 2, which stays, with a
// comment. Issue 1 is then transferred to o/elsewhere.
func transferFake(t *testing.T) (*Fake, int64) {
	t.Helper()
	f := New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Moved", "")
	f.AddIssue("owner", "Here", "")
	f.AddLabel(1, "owner", "bug")
	first := f.AddComment(1, "owner", "first")
	f.AddComment(1, "owner", "second")
	f.AddReaction(first, "owner", "+1")
	f.AddComment(2, "owner", "here")
	f.Transfer(1, "o/elsewhere")
	return f, first
}

// noFollow is a client of f that follows no redirect, to see the 301
// itself.
func noFollow(f *Fake) *http.Client {
	return &http.Client{Transport: f.Transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// rawDo sends one request to f with client and returns its status, its
// headers and its body.
func rawDo(t *testing.T, client *http.Client, method, url string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close the body: %v", err)
		}
	}()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// statuses is the requests from the from'th on, as "METHOD /path status",
// but for the App's (a token's mint).
func statuses(f *Fake, from int) []string {
	var got []string
	for _, r := range f.Requests()[from:] {
		if !strings.HasPrefix(r.Path, "/app") {
			got = append(got, fmt.Sprintf("%s %s %d", r.Method, r.Path, r.Status))
		}
	}
	return got
}

// A transferred issue drops out of the repo's issue listing, and its
// comments out of the repo's comment listing, while the other issue
// stays in both.
func TestTransferListings(t *testing.T) {
	t.Parallel()
	f, _ := transferFake(t)
	c, ctx := f.Client(t), context.Background()
	issues, err := c.IssuesSince(ctx, "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].GetNumber() != 2 {
		t.Errorf("issues listed = %v; want #2 alone", issues)
	}
	all, _, err := c.Issues.ListByRepo(ctx, "o", "r", &gogithub.IssueListByRepoOptions{State: "all"})
	if err != nil || len(all) != 1 || all[0].GetNumber() != 2 {
		t.Errorf("issues listed in any state = %v, %v; want #2 alone", all, err)
	}
	comments, err := c.CommentsSince(ctx, "o", "r", 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(comments) != 1 || comments[0].GetBody() != "here" {
		t.Errorf("the repo's comments = %v; want #2's alone", comments)
	}
}

// Every request about a transferred issue, at its old path, gets a 301
// whose Location is the same path, query and all, at its new home,
// /repositories/{id}/..., with GitHub's message: a GET, a write, and a
// request about one of its comments, by the comment's own path, alike.
func TestTransferRedirects(t *testing.T) {
	t.Parallel()
	f, first := transferFake(t)
	raw, base := noFollow(f), f.URL()
	for _, tc := range []struct{ method, path, query string }{
		{http.MethodGet, "/issues/1", ""},
		{http.MethodGet, "/issues/1/comments", "per_page=5"},
		{http.MethodGet, "/issues/1/timeline", ""},
		{http.MethodGet, "/issues/1/parent", ""},
		{http.MethodPost, "/issues/1/comments", ""},
		{http.MethodPatch, "/issues/1", ""},
		{http.MethodPost, "/issues/1/labels", ""},
		{http.MethodDelete, "/issues/1/labels/bug", ""},
		{http.MethodPost, "/issues/1/assignees", ""},
		{http.MethodDelete, "/issues/1/assignees", ""},
		{http.MethodPatch, fmt.Sprintf("/issues/comments/%d", first), ""},
		{http.MethodGet, fmt.Sprintf("/issues/comments/%d/reactions", first), ""},
		{http.MethodPost, fmt.Sprintf("/issues/comments/%d/reactions", first), ""},
	} {
		url := base + "/repos/o/r" + tc.path
		want := base + "/repositories/2" + tc.path
		if tc.query != "" {
			url, want = url+"?"+tc.query, want+"?"+tc.query
		}
		status, h, body := rawDo(t, raw, tc.method, url)
		var msg struct{ Message, URL string }
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Errorf("%s %s: the body %q: %v", tc.method, tc.path, body, err)
		}
		if status != http.StatusMovedPermanently || h.Get("Location") != want || msg.Message != "Moved Permanently" || msg.URL != want {
			t.Errorf("%s %s = %d, Location %q, body %q; want a 301 to %s", tc.method, tc.path, status, h.Get("Location"), body, want)
		}
	}
	if status, _, _ := rawDo(t, raw, http.MethodGet, base+"/repos/o/r/issues/2"); status != http.StatusOK {
		t.Errorf("GET /issues/2 = %d; want 200, as it stays", status)
	}
}

// A write to a transferred issue writes nothing: go-github's client
// follows the 301 with a GET of the new home, as net/http does for every
// method, and gets the moved issue for the issue's own path, a list for a
// listing's, which doesn't decode as one item, or a 404 for a path the
// fake serves no GET for. The write is logged, not failed, and the state
// is as it was.
func TestTransferWrites(t *testing.T) {
	t.Parallel()
	f, first := transferFake(t)
	c, ctx := f.Client(t), context.Background()
	want := f.State()
	from, writes := len(f.Requests()), len(f.Writes())
	is, _, err := c.Issues.Update(ctx, "o", "r", 1, gogithub.UpdateIssueRequest{Body: new("edited")})
	if err != nil || is.GetBody() != "" || !strings.HasSuffix(is.GetRepositoryURL(), "/repos/o/elsewhere") {
		t.Errorf("the edit = %v, %v; want the moved issue as it was, from o/elsewhere", is, err)
	}
	_, _, err = c.Issues.CreateComment(ctx, "o", "r", 1, gogithub.IssueCommentRequest{Body: "hi"})
	if err == nil {
		t.Errorf("the comment = nil; want an error, as the GET of the comments gives a list")
	}
	if _, _, err := c.Issues.AddLabelsToIssue(ctx, "o", "r", 1, []string{"x"}); !notFound(err) {
		t.Errorf("the label add = %v; want a 404", err)
	}
	if _, err := c.Issues.RemoveLabelForIssue(ctx, "o", "r", 1, "bug"); !notFound(err) {
		t.Errorf("the label removal = %v; want a 404", err)
	}
	if _, _, err := c.Issues.AddAssignees(ctx, "o", "r", 1, []string{"owner"}); !notFound(err) {
		t.Errorf("the assignee add = %v; want a 404", err)
	}
	if _, _, err := c.Issues.UpdateComment(ctx, "o", "r", first, gogithub.IssueCommentRequest{Body: "edited"}); !notFound(err) {
		t.Errorf("the comment edit = %v; want a 404", err)
	}
	if _, _, err := c.Client.Reactions.CreateIssueCommentReaction(ctx, "o", "r", first, "heart"); err == nil {
		t.Errorf("the reaction = nil; want an error, as the GET of the reactions gives a list")
	}
	if got := f.State(); !reflect.DeepEqual(got, want) {
		t.Errorf("the state after the writes = %+v; want it as it was, %+v", got, want)
	}
	wantReqs := []string{
		"PATCH /issues/1 301", "GET /repositories/2/issues/1 200",
		"POST /issues/1/comments 301", "GET /repositories/2/issues/1/comments 200",
		"POST /issues/1/labels 301", "GET /repositories/2/issues/1/labels 404",
		"DELETE /issues/1/labels/bug 301", "GET /repositories/2/issues/1/labels/bug 404",
		"POST /issues/1/assignees 301", "GET /repositories/2/issues/1/assignees 404",
		fmt.Sprintf("PATCH /issues/comments/%d 301", first), fmt.Sprintf("GET /repositories/2/issues/comments/%d 404", first),
		fmt.Sprintf("POST /issues/comments/%d/reactions 301", first), fmt.Sprintf("GET /repositories/2/issues/comments/%d/reactions 200", first),
	}
	if got := statuses(f, from); !slices.Equal(got, wantReqs) {
		t.Errorf("requests = %q; want %q", got, wantReqs)
	}
	for _, w := range f.Writes()[writes:] {
		if w.Failed {
			t.Errorf("write %+v is failed; want none failed, only redirected", w)
		}
	}
	if got := len(f.Writes()) - writes; got != 7 {
		t.Errorf("%d writes logged; want the 7 redirected", got)
	}
}

// notFound reports whether err is GitHub's 404.
func notFound(err error) bool {
	var ge *gogithub.ErrorResponse
	return errors.As(err, &ge) && ge.Response.StatusCode == http.StatusNotFound
}

// The new home serves GETs of the moved issue and its comments, the
// issue naming its new repo, and pages at its own path. It serves
// nothing else: no write, no issue not moved there, no listing of the
// repo, and no repo ID nothing was moved to. A second repo gets the next
// ID.
func TestTransferHome(t *testing.T) {
	t.Parallel()
	f, first := transferFake(t)
	raw, home := noFollow(f), f.URL()+"/repositories/2"
	status, _, body := rawDo(t, raw, http.MethodGet, home+"/issues/1")
	if status != http.StatusOK || !strings.Contains(body, `"repository_url":"`+f.URL()+`/repos/o/elsewhere"`) {
		t.Errorf("GET of the new home = %d, %q; want 200, the issue in o/elsewhere", status, body)
	}
	status, h, body := rawDo(t, raw, http.MethodGet, home+"/issues/1/comments?per_page=1")
	if status != http.StatusOK || !strings.Contains(body, `"body":"first"`) || !strings.Contains(h.Get("Link"), "/repositories/2/issues/1/comments?") {
		t.Errorf("GET of the comments at the new home = %d, %q, Link %q; want 200, the first, and the next page there", status, body, h.Get("Link"))
	}
	status, _, body = rawDo(t, raw, http.MethodGet, fmt.Sprintf("%s/issues/comments/%d/reactions", home, first))
	if status != http.StatusOK || !strings.Contains(body, `"content":"+1"`) {
		t.Errorf("GET of the reactions at the new home = %d, %q; want 200, the 👍", status, body)
	}
	for _, tc := range []struct{ method, url string }{
		{http.MethodPost, home + "/issues/1/comments"},
		{http.MethodPatch, home + "/issues/1"},
		{http.MethodGet, home + "/issues/2"},
		{http.MethodGet, home + "/issues/2/comments"},
		{http.MethodGet, home + "/issues"},
		{http.MethodGet, home + "/issues/comments"},
		{http.MethodGet, home + "/labels"},
		{http.MethodGet, home + "/issues/1/assignees"},
		{http.MethodGet, f.URL() + "/repositories/3/issues/1"},
		{http.MethodGet, f.URL() + "/repositories/x/issues/1"},
	} {
		if status, _, body := rawDo(t, raw, tc.method, tc.url); status != http.StatusNotFound {
			t.Errorf("%s %s = %d, %q; want 404", tc.method, tc.url, status, body)
		}
	}
	f.Transfer(2, "other/repo")
	if status, h, _ := rawDo(t, raw, http.MethodGet, f.URL()+"/repos/o/r/issues/2"); status != http.StatusMovedPermanently ||
		h.Get("Location") != f.URL()+"/repositories/3/issues/2" {
		t.Errorf("GET of #2 moved to a second repo = %d, Location %q; want a 301 to /repositories/3/issues/2", status, h.Get("Location"))
	}
	f.Transfer(1, "o/elsewhere") // again: the same ID
	if status, h, _ := rawDo(t, raw, http.MethodGet, f.URL()+"/repos/o/r/issues/1"); h.Get("Location") != home+"/issues/1" {
		t.Errorf("GET of #1 moved there again = %d, Location %q; want a 301 to %s/issues/1", status, h.Get("Location"), home)
	}
}

// A transfer back to the repo, in any case, ends it: the issue is listed
// again and served at its own path, naming the repo as given, and the
// new home no longer serves it.
func TestTransferBack(t *testing.T) {
	t.Parallel()
	f, _ := transferFake(t)
	f.Transfer(1, "O/R")
	c, ctx := f.Client(t), context.Background()
	issues, err := c.IssuesSince(ctx, "o", "r", time.Time{})
	if err != nil || len(issues) != 2 {
		t.Errorf("issues listed = %v, %v; want both", issues, err)
	}
	from := len(f.Requests())
	is, _, err := c.Issues.Get(ctx, "o", "r", 1)
	if err != nil || is.GetRepositoryURL() != f.URL()+"/repos/O/R" {
		t.Errorf("GET of #1 = %v, %v; want it, naming O/R", is, err)
	}
	if got := statuses(f, from); !slices.Equal(got, []string{"GET /issues/1 200"}) {
		t.Errorf("requests = %q; want the GET alone, answered", got)
	}
	if status, _, _ := rawDo(t, noFollow(f), http.MethodGet, f.URL()+"/repositories/2/issues/1"); status != http.StatusNotFound {
		t.Errorf("GET of the old new home = %d; want 404", status)
	}
}
