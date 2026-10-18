package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// addBlocker makes issue n blocked by the issue whose ID is id through the
// API, and returns the answer's status, failing t on anything but an API
// error.
func addBlocker(t *testing.T, c *github.Client, n int, id int64) int {
	t.Helper()
	_, resp, err := c.Issues.AddBlockedBy(context.Background(), "o", "r", int64(n), gogithub.IssueDependencyRequest{IssueID: id})
	var er *gogithub.ErrorResponse
	if err != nil && !errors.As(err, &er) {
		t.Fatal(err)
	}
	return resp.StatusCode
}

// blockers lists the issues blocking n as "repo#number state", two to a
// page.
func blockers(t *testing.T, c *github.Client, n int) []string {
	t.Helper()
	var got []string
	for is, err := range c.Issues.ListBlockedByIter(context.Background(), "o", "r", int64(n), &gogithub.ListOptions{PerPage: 2}) {
		if err != nil {
			t.Fatal(err)
		}
		_, repo, _ := strings.Cut(is.GetRepositoryURL(), "/repos/")
		got = append(got, fmt.Sprintf("%s#%d %s", repo, is.GetNumber(), is.GetState()))
	}
	return got
}

// The blocking issues are listed open and closed, in the order added, each
// with its repository_url, two pages of 2 here. An issue with none lists
// none. A repeat of an unchanged listing is a 304 for each page, and an
// added link changes the page it lands on.
func TestBlockedBy(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	blocked := f.AddIssue("owner", "Blocked", "body")
	for range 5 {
		f.AddIssue("owner", "Blocker", "body")
	}
	f.SetBlockedBy(blocked, 4)
	f.SetBlockedBy(blocked, 2)
	f.SetBlockedBy(blocked, 3)
	f.SetBlockedBy(blocked, 4) // a link it has already
	f.Close(2)
	c := f.Client(t)
	first, resp, err := c.Issues.ListBlockedBy(context.Background(), "o", "r", int64(blocked), &gogithub.ListOptions{PerPage: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || resp.NextPage != 2 {
		t.Errorf("first page: %d blockers, next page %d, want 2 and 2", len(first), resp.NextPage)
	}
	if got, want := blockers(t, c, blocked), []string{"o/r#4 open", "o/r#2 closed", "o/r#3 open"}; !reflect.DeepEqual(got, want) {
		t.Errorf("blockers = %q, want %q", got, want)
	}
	if got := blockers(t, c, 5); got != nil {
		t.Errorf("blockers of #5 = %q, want none", got)
	}
	notModified := f.NotModified()
	blockers(t, c, blocked)
	if got := f.NotModified(); got != notModified+2 {
		t.Errorf("NotModified() = %d after a repeated listing, want %d, one for each page", got, notModified+2)
	}
	if got := addBlocker(t, c, blocked, issueID(6)); got != http.StatusCreated {
		t.Fatalf("add #6: status %d, want 201", got)
	}
	if got, want := blockers(t, c, blocked), []string{"o/r#4 open", "o/r#2 closed", "o/r#3 open", "o/r#6 open"}; !reflect.DeepEqual(got, want) {
		t.Errorf("blockers after an add = %q, want %q", got, want)
	}
	if got := f.NotModified(); got != notModified+3 {
		t.Errorf("NotModified() = %d after an add, want %d: only the first page unchanged", got, notModified+3)
	}
	if got, want := f.State().Issues[blocked-1].BlockedBy, []int{2, 3, 4, 6}; !reflect.DeepEqual(got, want) {
		t.Errorf("State blocked-by = %v, want %v, sorted", got, want)
	}
}

// A link added through the API, by the blocker's ID, answers 201 with the
// blocking issue and moves the blocked issue's updated_at, not the
// blocker's. As on GitHub, a link it has already, the issue itself and a
// 51st blocker get a 422, and an unknown ID a 404, and nothing changes.
func TestAddBlockedBy(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	blocked := f.AddIssue("owner", "Blocked", "body")
	blocker := f.AddIssue("owner", "Blocker", "body")
	full := f.AddIssue("owner", "Full", "body")
	for range maxBlockedBy {
		f.SetBlockedBy(full, f.AddIssue("owner", "Blocker", "body"))
	}
	ctx, c := context.Background(), f.Client(t)
	blockedAt, blockerAt := issueUpdatedAt(t, c, blocked), issueUpdatedAt(t, c, blocker)
	is, resp, err := c.Issues.AddBlockedBy(ctx, "o", "r", int64(blocked), gogithub.IssueDependencyRequest{IssueID: issueID(blocker)})
	if err != nil || resp.StatusCode != http.StatusCreated || is.GetNumber() != blocker {
		t.Fatalf("add = #%d, %v; want 201 with #%d", is.GetNumber(), err, blocker)
	}
	if got := issueUpdatedAt(t, c, blocked); !got.After(blockedAt) {
		t.Errorf("blocked updated_at %v, want after %v", got, blockedAt)
	}
	if got := issueUpdatedAt(t, c, blocker); !got.Equal(blockerAt) {
		t.Errorf("blocker updated_at %v, want %v unchanged", got, blockerAt)
	}
	before := f.State()
	for _, tc := range []struct {
		name string
		n    int
		id   int64
		want int
	}{
		{"the same link again", blocked, issueID(blocker), http.StatusUnprocessableEntity},
		{"the issue itself", blocked, issueID(blocked), http.StatusUnprocessableEntity},
		{"a 51st blocker", full, issueID(blocker), http.StatusUnprocessableEntity},
		{"an unknown ID", blocked, issueID(999), http.StatusNotFound},
		{"a number for an ID", blocked, int64(full), http.StatusNotFound},
	} {
		if got := addBlocker(t, c, tc.n, tc.id); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the refusals:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
	// The other way round is a link of its own.
	if got := addBlocker(t, c, blocker, issueID(blocked)); got != http.StatusCreated {
		t.Errorf("#%d blocked by #%d: status %d, want 201", blocker, blocked, got)
	}
}

// Replay compares blocked-by links: a pass that adds one without reading
// first gets a 422 on its second pass, once its first add applied.
func TestReplayComparesBlockedBy(t *testing.T) {
	t.Parallel()
	setup := func(t testing.TB) *Fake {
		f := New(t, "o/r", "issuedog")
		f.AddIssue("owner", "Blocked", "body")
		f.AddIssue("owner", "Blocker", "body")
		return f
	}
	blind := func(f *Fake) error {
		_, _, err := f.Client(t).Issues.AddBlockedBy(context.Background(), "o", "r", 1, gogithub.IssueDependencyRequest{IssueID: issueID(2)})
		return err
	}
	problems := replay(t, setup, blind)
	if len(problems) == 0 || !strings.HasPrefix(problems[0], "crash after write 1 applied: second pass") || !strings.Contains(problems[0], "422") {
		t.Errorf("problems = %q, want a 422 on the second pass after write 1 applied", problems)
	}
	// A pass that takes any link for all of them misses the second after a
	// crash once the first applied.
	anyLink := func(f *Fake) error {
		ctx, c := context.Background(), f.Client(t)
		have, _, err := c.Issues.ListBlockedBy(ctx, "o", "r", 1, nil)
		if err != nil || len(have) > 0 {
			return err
		}
		for _, n := range []int{2, 3} {
			if _, _, err := c.Issues.AddBlockedBy(ctx, "o", "r", 1, gogithub.IssueDependencyRequest{IssueID: issueID(n)}); err != nil {
				return err
			}
		}
		return nil
	}
	setup3 := func(t testing.TB) *Fake {
		f := setup(t)
		f.AddIssue("owner", "Blocker", "body")
		return f
	}
	problems = replay(t, setup3, anyLink)
	if len(problems) == 0 || !strings.HasPrefix(problems[0], "crash after write 1 applied: the state") || !strings.Contains(problems[0], "BlockedBy:[2]") {
		t.Errorf("problems = %q, want the missing blocked-by link in the state", problems)
	}
}
