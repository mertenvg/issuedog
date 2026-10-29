package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
)

// readyBlockedQ is the search the client makes for blocked ready tasks.
const readyBlockedQ = `repo:o/r is:issue is:open label:"stage:ready" is:blocked`

// searchFound runs the advanced issue search q, perPage to a page, and
// returns the numbers it finds, every page's, in order, and the total the
// first page gives, failing t on any error.
func searchFound(t *testing.T, c *github.Client, q string, perPage int) ([]int, int) {
	t.Helper()
	opts := &gogithub.SearchOptions{AdvancedSearch: new(true), ListOptions: gogithub.ListOptions{PerPage: perPage}}
	var found []int
	total := -1
	for {
		res, resp, err := c.Search.Issues(context.Background(), q, opts)
		if err != nil {
			t.Fatalf("search %q page %d: %v", q, opts.Page, err)
		}
		if total < 0 {
			total = res.GetTotal()
		}
		for _, is := range res.Issues {
			found = append(found, is.GetNumber())
		}
		if resp.NextPage == 0 {
			return found, total
		}
		opts.Page = resp.NextPage
	}
}

// searchStatuses is each search request's status since from, with whether
// it was conditional.
func searchStatuses(f *Fake, from int) []string {
	var got []string
	for _, r := range f.Requests()[from:] {
		if r.Path == "/search/issues" {
			got = append(got, fmt.Sprintf("%d %v", r.Status, r.Conditional))
		}
	}
	return got
}

// The search finds an open ready task with an open blocker, and not one
// whose only blocker is closed, one at another stage, one with no blocker,
// a closed one, or one transferred away. A pull request is found only
// without is:issue, and an issue without is:open is found closed too.
// Labels match in any case.
func TestSearchIssuesBlocked(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	blocker := f.AddIssue("owner", "Open blocker", "body")
	closedBlocker := f.AddIssue("owner", "Closed blocker", "body")
	f.Close(closedBlocker)
	blocked := f.AddIssue("owner", "Blocked", "body", "stage:ready")
	f.SetBlockedBy(blocked, closedBlocker)
	f.SetBlockedBy(blocked, blocker)
	closedOnly := f.AddIssue("owner", "Its blocker closed", "body", "stage:ready")
	f.SetBlockedBy(closedOnly, closedBlocker)
	clarifying := f.AddIssue("owner", "Another stage", "body", "stage:clarifying")
	f.SetBlockedBy(clarifying, blocker)
	unblocked := f.AddIssue("owner", "Unblocked", "body", "stage:ready")
	closed := f.AddIssue("owner", "Closed", "body", "stage:ready")
	f.SetBlockedBy(closed, blocker)
	f.Close(closed)
	pr := f.AddIssue("owner", "A pull request", "body", "stage:ready")
	f.SetPullRequest(pr)
	f.SetBlockedBy(pr, blocker)
	upper := f.AddIssue("owner", "Upper case", "body", "Stage:Ready")
	f.SetBlockedBy(upper, blocker)
	moved := f.AddIssue("owner", "Transferred", "body", "stage:ready")
	f.SetBlockedBy(moved, blocker)
	f.Transfer(moved, "o/other")
	c := f.Client(t)
	for _, tc := range []struct {
		q    string
		want []int
	}{
		{readyBlockedQ, []int{blocked, upper}},
		{`repo:o/r is:open label:"stage:ready" is:blocked`, []int{blocked, pr, upper}},
		{`repo:o/r is:issue label:"stage:ready" is:blocked`, []int{blocked, closed, upper}},
		{`repo:O/R is:issue is:open label:stage:ready`, []int{blocked, closedOnly, unblocked, upper}},
		{`repo:o/r is:issue is:blocked`, []int{blocked, clarifying, closed, upper}},
		{`repo:o/r label:"stage:ready" label:"stage:clarifying"`, nil},
	} {
		if got, total := searchFound(t, c, tc.q, 100); !slices.Equal(got, tc.want) || total != len(tc.want) {
			t.Errorf("search %q = %v, total %d; want %v", tc.q, got, total, tc.want)
		}
	}
}

// A search the fake doesn't read is a 422: a qualifier it doesn't know, a
// typo of one, free text, a negation, an empty label, an unclosed quote,
// no repo: or another repo, and a search that isn't advanced.
func TestSearchIssuesRefused(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Ready", "body", "stage:ready")
	c := f.Client(t)
	for _, q := range []string{
		`repo:o/r is:issue is:open label:"stage:ready" is:blokced`,
		`repo:o/r is:closed`,
		`repo:o/r is:pr`,
		`repo:o/r state:open`,
		`repo:o/r blocked`,
		`repo:o/r -label:"stage:ready"`,
		`repo:o/r label:`,
		`repo:o/r label:"stage:ready`,
		`is:issue is:open label:"stage:ready" is:blocked`,
		`repo:o/other is:issue`,
		`repo:o/r repo:o/other is:issue`,
	} {
		opts := &gogithub.SearchOptions{AdvancedSearch: new(true)}
		_, resp, err := c.Search.Issues(context.Background(), q, opts)
		var er *gogithub.ErrorResponse
		if !errors.As(err, &er) || resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("search %q: err %v, want a 422", q, err)
		}
	}
	_, resp, err := c.Search.Issues(context.Background(), readyBlockedQ, &gogithub.SearchOptions{})
	if err == nil || resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("a search that isn't advanced: err %v, want a 422", err)
	}
}

// The search pages by per_page, three pages of 2 here for five found, each
// with the total. A repeat with nothing changed is a 304 for each page, and
// a sixth found changes the last page and the total, so every page is a
// 200 again.
func TestSearchIssuesPaging(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	blocker := f.AddIssue("owner", "Blocker", "body")
	var want []int
	for range 5 {
		n := f.AddIssue("owner", "Blocked", "body", "stage:ready")
		f.SetBlockedBy(n, blocker)
		want = append(want, n)
	}
	c := f.Client(t)
	from := len(f.Requests())
	if got, total := searchFound(t, c, readyBlockedQ, 2); !slices.Equal(got, want) || total != 5 {
		t.Errorf("found %v, total %d; want %v, total 5", got, total, want)
	}
	if got, want := searchStatuses(f, from), []string{"200 false", "200 false", "200 false"}; !slices.Equal(got, want) {
		t.Errorf("first search = %q, want %q", got, want)
	}
	from = len(f.Requests())
	searchFound(t, c, readyBlockedQ, 2)
	if got, want := searchStatuses(f, from), []string{"304 true", "304 true", "304 true"}; !slices.Equal(got, want) {
		t.Errorf("repeat = %q, want %q", got, want)
	}
	n := f.AddIssue("owner", "Blocked", "body", "stage:ready")
	f.SetBlockedBy(n, blocker)
	from = len(f.Requests())
	if got, total := searchFound(t, c, readyBlockedQ, 2); !slices.Equal(got, append(want, n)) || total != 6 {
		t.Errorf("found %v, total %d after a sixth; want %v, total 6", got, total, append(want, n))
	}
	if got, want := searchStatuses(f, from), []string{"200 true", "200 true", "200 true"}; !slices.Equal(got, want) {
		t.Errorf("search after a sixth = %q, want %q", got, want)
	}
}

// The search's answer changes, and so its ETag, when a label of an issue it
// finds changes, when a blocked-by link is added to one, and when a blocker
// of one closes, though each still finds the same issue. A repeat with
// nothing changed is a 304 each time.
func TestSearchIssuesETag(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	first := f.AddIssue("owner", "First blocker", "body")
	second := f.AddIssue("owner", "Second blocker", "body")
	blocked := f.AddIssue("owner", "Blocked", "body", "stage:ready")
	f.SetBlockedBy(blocked, first)
	c := f.Client(t)
	searchFound(t, c, readyBlockedQ, 100)
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"a label added", func() { f.AddLabel(blocked, "owner", "priority:p1") }},
		{"a blocked-by link added", func() { f.SetBlockedBy(blocked, second) }},
		{"a blocker closed", func() { f.Close(first) }},
		{"a blocker reopened", func() { f.Reopen(first) }},
	} {
		from := len(f.Requests())
		searchFound(t, c, readyBlockedQ, 100)
		tc.change()
		if got, total := searchFound(t, c, readyBlockedQ, 100); !slices.Equal(got, []int{blocked}) || total != 1 {
			t.Errorf("%s: found %v, total %d; want [%d]", tc.name, got, total, blocked)
		}
		if got, want := searchStatuses(f, from), []string{"304 true", "200 true"}; !slices.Equal(got, want) {
			t.Errorf("%s: searches = %q, want %q", tc.name, got, want)
		}
	}
}

// A frozen search answers as it did at FreezeSearch, items and ETag, so a
// repeat is a 304, while a blocker closes, a link and a label are added,
// and an issue is filed. ThawSearch shows them all. A second FreezeSearch
// takes the issues as they are then.
func TestSearchIssuesFrozen(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	blocker := f.AddIssue("owner", "Blocker", "body")
	blocked := f.AddIssue("owner", "Blocked", "body", "stage:ready")
	f.SetBlockedBy(blocked, blocker)
	linked := f.AddIssue("owner", "Linked later", "body", "stage:ready")
	labelled := f.AddIssue("owner", "Labelled later", "body")
	f.SetBlockedBy(labelled, linked)
	c := f.Client(t)
	searchFound(t, c, readyBlockedQ, 100)
	f.FreezeSearch()
	f.Close(blocker)
	f.SetBlockedBy(linked, labelled)
	f.AddLabel(labelled, "owner", "stage:ready")
	filed := f.AddIssue("owner", "Filed later", "body", "stage:ready")
	f.SetBlockedBy(filed, linked)
	from := len(f.Requests())
	if got, total := searchFound(t, c, readyBlockedQ, 100); !slices.Equal(got, []int{blocked}) || total != 1 {
		t.Errorf("frozen: found %v, total %d; want [%d]", got, total, blocked)
	}
	if got, want := searchStatuses(f, from), []string{"304 true"}; !slices.Equal(got, want) {
		t.Errorf("frozen: searches = %q, want %q", got, want)
	}
	f.ThawSearch()
	want := []int{linked, labelled, filed}
	if got, total := searchFound(t, c, readyBlockedQ, 100); !slices.Equal(got, want) || total != 3 {
		t.Errorf("thawed: found %v, total %d; want %v", got, total, want)
	}
	f.FreezeSearch()
	f.Close(labelled)
	if got, _ := searchFound(t, c, readyBlockedQ, 100); !slices.Equal(got, want) {
		t.Errorf("frozen again: found %v, want %v", got, want)
	}
	f.ThawSearch()
	if got, _ := searchFound(t, c, readyBlockedQ, 100); !slices.Equal(got, []int{filed}) {
		t.Errorf("thawed again: found %v, want [%d]", got, filed)
	}
}
