package githubtest

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

// pinOf returns issue n's Closed and Pinned flags in f's State.
func pinOf(f *Fake, n int) (closed, pinned bool) {
	is := f.State().Issues[n-1]
	return is.Closed, is.Pinned
}

// Creating, reopening and pinning are writes, logged with their paths, and
// show in State. Reading whether an issue is pinned isn't a write.
func TestCreateReopenPinAreWrites(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	ctx, c := context.Background(), f.Client(t)
	n, err := c.CreateIssue(ctx, "o", "r", "issuedog digest", "the body")
	if err != nil {
		t.Fatal(err)
	}
	f.Close(n)
	if closed, pinned := pinOf(f, n); !closed || pinned {
		t.Errorf("closed %v and pinned %v after Close, want closed and not pinned", closed, pinned)
	}
	if err := c.ReopenIssue(ctx, "o", "r", n); err != nil {
		t.Fatal(err)
	}
	if err := c.PinIssue(ctx, "o", "r", n); err != nil {
		t.Fatal(err)
	}
	if pinned, err := c.IsPinned(ctx, "o", "r", n); err != nil || !pinned {
		t.Errorf("IsPinned = %v, %v; want true", pinned, err)
	}
	if closed, pinned := pinOf(f, n); closed || !pinned {
		t.Errorf("closed %v and pinned %v, want open and pinned", closed, pinned)
	}
	is := f.State().Issues[n-1]
	if is.Author != "issuedog[bot]" || is.Title != "issuedog digest" || is.Body != "the body" {
		t.Errorf("issue = %+v, want the App's, with its title and body", is)
	}
	var got []string
	for _, w := range f.Writes() {
		got = append(got, w.Method+" "+w.Path)
	}
	if want := []string{"POST /issues", "PATCH /issues/1", "POST /graphql"}; !reflect.DeepEqual(got, want) {
		t.Errorf("writes = %q, want %q", got, want)
	}
	if body := f.Writes()[2].Body; !strings.Contains(body, "pinIssue") || !strings.Contains(body, `"id":"I_1"`) {
		t.Errorf("pin write = %s, want the mutation on I_1", body)
	}
}

// FailWrite fails a pin before it changes anything, and FailWriteAfter
// applies it but answers 500.
func TestFailWriteOnPin(t *testing.T) {
	for _, after := range []bool{false, true} {
		f := New(t, "o/r", "issuedog")
		n := f.AddIssue("issuedog[bot]", "issuedog digest", "")
		if after {
			f.FailWriteAfter(1)
		} else {
			f.FailWrite(1)
		}
		err := f.Client(t).PinIssue(context.Background(), "o", "r", n)
		var er *gogithub.ErrorResponse
		if !errors.As(err, &er) || er.Response.StatusCode != http.StatusInternalServerError {
			t.Errorf("after %v: err = %v, want a 500", after, err)
		}
		if _, pinned := pinOf(f, n); pinned != after {
			t.Errorf("after %v: pinned = %v, want %v", after, pinned, after)
		}
		if w := f.Writes(); len(w) != 1 || !w[0].Failed || w[0].Path != "/graphql" {
			t.Errorf("after %v: writes = %+v, want the failed pin", after, w)
		}
	}
}

// FailPin answers each pin with GitHub's FORBIDDEN error and no pin, until
// it's cleared. Pinning again is idempotent.
func TestFailPin(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "issuedog digest", "")
	ctx, c := context.Background(), f.Client(t)
	f.FailPin("Resource not accessible by integration")
	for range 2 {
		err := c.PinIssue(ctx, "o", "r", n)
		if !errors.Is(err, github.ErrGraphQL) || !strings.Contains(err.Error(), "Resource not accessible by integration") {
			t.Errorf("err = %v, want GitHub's message", err)
		}
	}
	if _, pinned := pinOf(f, n); pinned {
		t.Error("pinned after a failed pin")
	}
	f.FailPin("")
	for range 2 {
		if err := c.PinIssue(ctx, "o", "r", n); err != nil {
			t.Fatal(err)
		}
	}
	if _, pinned := pinOf(f, n); !pinned {
		t.Error("not pinned once FailPin is cleared")
	}
	if w := f.Writes(); len(w) != 4 {
		t.Errorf("writes = %+v, want each of the 4 pins logged", w)
	}
	f.SetPinned(n, false)
	if pinned, err := c.IsPinned(ctx, "o", "r", n); err != nil || pinned {
		t.Errorf("IsPinned after an unpin by hand = %v, %v; want false", pinned, err)
	}
}

// The fake answers the pin query and mutation as GitHub does for a missing
// issue, and refuses a query that names fields it doesn't answer.
func TestPinGraphQLErrors(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	f.SetPullRequest(f.AddIssue("owner", "PR", ""))
	ctx, c := context.Background(), f.Client(t)
	if _, err := c.IsPinned(ctx, "o", "r", 9); err == nil || !strings.Contains(err.Error(), "Could not resolve to an Issue with the number of 9.") {
		t.Errorf("IsPinned of a missing issue: err = %v, want NOT_FOUND", err)
	}
	ask := func(query string, vars map[string]any) []string {
		t.Helper()
		req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": query, "variables": vars})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Errors []struct{ Type, Message string }
		}
		if _, err := c.Do(req, &out); err != nil {
			t.Fatal(err)
		}
		var msgs []string
		for _, e := range out.Errors {
			msgs = append(msgs, strings.TrimSpace(e.Type+" "+e.Message))
		}
		return msgs
	}
	for _, tc := range []struct {
		name, query string
		vars        map[string]any
		want        string
	}{
		{"unknown node", answeredPin, map[string]any{"id": "I_9"}, "NOT_FOUND Could not resolve to a node with the global id of 'I_9'"},
		{"pull request", answeredPin, map[string]any{"id": "I_1"}, "NOT_FOUND Could not resolve to a node with the global id of 'I_1'"},
		{"extra field", strings.Replace(answeredPin, "isPinned", "isPinned id", 1), map[string]any{"id": "I_1"},
			"githubtest: the query names unknown mutation.pinIssue.issue.id"},
		{"no isPinned", strings.Replace(answeredPin, "{ isPinned }", "{ id }", 1), map[string]any{"id": "I_1"},
			"githubtest: the query doesn't name mutation.pinIssue.issue.isPinned and names unknown mutation.pinIssue.issue.id"},
		{"isPinned with more", strings.Replace(answeredPinned, "isPinned", "isPinned body", 1), map[string]any{"number": 1},
			"githubtest: the query names unknown query.repository.issue.body"},
	} {
		if got := ask(tc.query, tc.vars); len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: errors = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// PATCH edits only the body and the state, which must be open or closed, and
// POST needs a title, as on GitHub.
func TestCreateAndEditIssueRefusals(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	ctx, c := context.Background(), f.Client(t)
	for _, tc := range []struct {
		name string
		do   func() error
	}{
		{"no title", func() error {
			_, _, err := c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Body: new("body")})
			return err
		}},
		{"a bad state", func() error {
			_, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{State: new("gone")})
			return err
		}},
		{"only a title", func() error {
			_, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Title: new("New")})
			return err
		}},
	} {
		var er *gogithub.ErrorResponse
		if err := tc.do(); !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s: err = %v, want a 422", tc.name, err)
		}
	}
	// Body and state together, then the state alone: closing works too.
	if _, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Body: new("new"), State: new("closed")}); err != nil {
		t.Fatal(err)
	}
	if is := f.State().Issues[0]; !is.Closed || is.Body != "new" || is.BodyEditedBy != "issuedog[bot]" {
		t.Errorf("issue = %+v, want closed with the App's new body", is)
	}
	if len(f.State().Issues) != 1 {
		t.Errorf("issues = %+v, want no issue created without a title", f.State().Issues)
	}
}

// Replay compares the closed and pinned flags: a pass that pins only when
// it didn't reopen first leaves the issue pinned after a crash, and unpinned
// in the clean run.
func TestReplayComparesClosedAndPinned(t *testing.T) {
	setup := func(t testing.TB) *Fake {
		f := New(t, "o/r", "issuedog")
		f.Close(f.AddIssue("issuedog[bot]", "issuedog digest", ""))
		f.ForgetWrites()
		return f
	}
	pass := func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		is, _, err := c.Issues.Get(ctx, "o", "r", 1)
		if err != nil {
			return err
		}
		if is.GetState() == "closed" {
			return c.ReopenIssue(ctx, "o", "r", 1)
		}
		pinned, err := c.IsPinned(ctx, "o", "r", 1)
		if err != nil || pinned {
			return err
		}
		return c.PinIssue(ctx, "o", "r", 1)
	}
	problems := replay(t, setup, pass)
	if len(problems) == 0 || !strings.HasPrefix(problems[0], "crash after write 1 applied: the state") || !strings.Contains(problems[0], "Pinned:true") {
		t.Errorf("problems = %q, want the pin after a crash once the reopen applied", problems)
	}
	// A pass that creates without looking first makes a second issue.
	create := func(f *Fake) error {
		_, err := f.Client(t).CreateIssue(context.Background(), "o", "r", "issuedog digest", "")
		return err
	}
	problems = replay(t, func(t testing.TB) *Fake { return New(t, "o/r", "issuedog") }, create)
	if len(problems) == 0 || !strings.HasPrefix(problems[0], "crash after write 1 applied: the state") {
		t.Errorf("problems = %q, want a duplicate issue after write 1 applied", problems)
	}
}
