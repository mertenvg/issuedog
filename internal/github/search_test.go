package github_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// readyBlockedSearches is each search request since from, as readsSince
// shows it, with its query.
func readyBlockedSearches(t *testing.T, f *githubtest.Fake, from int) ([]string, []url.Values) {
	t.Helper()
	var queries []url.Values
	for _, r := range f.Requests()[from:] {
		if r.Path != "/search/issues" {
			continue
		}
		q, err := url.ParseQuery(r.Query)
		if err != nil {
			t.Fatalf("query %q: %v", r.Query, err)
		}
		queries = append(queries, q)
	}
	return readsSince(f, from), queries
}

// ReadyBlocked asks advanced search for this repo's open stage:ready issues
// that are blocked, oldest first, 100 to a page, and finds the one with an open blocker,
// not one whose blocker closed, one at another stage or one with none. A
// repeat with nothing changed is a conditional 304. While the search lags,
// a blocker's close doesn't show; once it catches up, it does.
func TestReadyBlocked(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	blocker := f.AddIssue("owner", "Blocker", "body")
	closedBlocker := f.AddIssue("owner", "Closed blocker", "body")
	f.Close(closedBlocker)
	blocked := f.AddIssue("owner", "Blocked", "body", "stage:ready")
	f.SetBlockedBy(blocked, blocker)
	closedOnly := f.AddIssue("owner", "Its blocker closed", "body", "stage:ready")
	f.SetBlockedBy(closedOnly, closedBlocker)
	clarifying := f.AddIssue("owner", "Another stage", "body", "stage:clarifying")
	f.SetBlockedBy(clarifying, blocker)
	f.AddIssue("owner", "Unblocked", "body", "stage:ready")
	ctx, c := context.Background(), f.Client(t)
	// find calls ReadyBlocked, wanting the numbers and the one request it
	// makes, as readsSince shows it.
	find := func(name string, want []int, wantRead string) {
		t.Helper()
		from := len(f.Requests())
		got, err := c.ReadyBlocked(ctx, "o", "r")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if numbers := slices.Sorted(maps.Keys(got)); !slices.Equal(numbers, want) {
			t.Errorf("%s: found %v, want %v", name, numbers, want)
		}
		reads, queries := readyBlockedSearches(t, f, from)
		if !slices.Equal(reads, []string{wantRead}) {
			t.Errorf("%s: requests = %q, want %q", name, reads, wantRead)
		}
		wantQuery := url.Values{
			"q":               {`repo:o/r is:issue is:open label:"stage:ready" is:blocked`},
			"sort":            {"created"},
			"order":           {"asc"},
			"per_page":        {"100"},
			"advanced_search": {"true"},
		}
		for _, q := range queries {
			if q.Encode() != wantQuery.Encode() {
				t.Errorf("%s: query %q, want %q", name, q.Encode(), wantQuery.Encode())
			}
		}
	}
	const path = "GET /search/issues "
	find("first", []int{blocked}, path+"200 plain")
	find("unchanged", []int{blocked}, path+"304 conditional")
	f.FreezeSearch()
	f.Close(blocker)
	find("lagging behind the close", []int{blocked}, path+"304 conditional")
	f.ThawSearch()
	find("caught up", nil, path+"200 conditional")
	find("unchanged after the close", nil, path+"304 conditional")
}

// ReadyBlocked reads every page: 101 blocked tasks are two pages of 100 and
// 1, and a repeat is a 304 for each page.
func TestReadyBlockedPages(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	blocker := f.AddIssue("owner", "Blocker", "body")
	var want []int
	for range 101 {
		n := f.AddIssue("owner", "Blocked", "body", "stage:ready")
		f.SetBlockedBy(n, blocker)
		want = append(want, n)
	}
	ctx, c := context.Background(), f.Client(t)
	for _, tc := range []struct {
		name string
		want []string
	}{
		{"first", []string{"GET /search/issues 200 plain", "GET /search/issues 200 plain"}},
		{"unchanged", []string{"GET /search/issues 304 conditional", "GET /search/issues 304 conditional"}},
	} {
		from := len(f.Requests())
		got, err := c.ReadyBlocked(ctx, "o", "r")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if numbers := slices.Sorted(maps.Keys(got)); !slices.Equal(numbers, want) {
			t.Errorf("%s: found %d tasks, want %d", tc.name, len(numbers), len(want))
		}
		reads, queries := readyBlockedSearches(t, f, from)
		if !slices.Equal(reads, tc.want) {
			t.Errorf("%s: requests = %q, want %q", tc.name, reads, tc.want)
		}
		if len(queries) == 2 && (queries[0].Has("page") || queries[1].Get("page") != "2") {
			t.Errorf("%s: pages %q and %q, want none and 2", tc.name, queries[0].Get("page"), queries[1].Get("page"))
		}
	}
}

// A refused search, such as a 422, is ReadyBlocked's error, naming the
// repo and the page, and wrapping GitHub's.
func TestReadyBlockedRefused(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/search/issues" }, http.StatusUnprocessableEntity)
	got, err := f.Client(t).ReadyBlocked(context.Background(), "o", "r")
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("ReadyBlocked = %v, %v; want a 422", got, err)
	}
	if want := "search o/r for blocked ready tasks, page 1: "; !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if got != nil {
		t.Errorf("found %v with the error, want nil", got)
	}
}
