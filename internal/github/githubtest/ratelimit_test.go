package githubtest

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// Each answer carries its rate resource's headers, as GitHub's do: search
// under /search/, graphql for /graphql, and core for the rest. An answer
// that isn't a 304 lowers its resource's remaining by one, a write's and
// a 404's too, and a 304 lowers none, so RateRemaining and the headers
// agree.
func TestFakeRateLimitHeaders(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	issue := "/repos/o/r/issues/" + strconv.Itoa(n)
	search := "/search/issues?advanced_search=true&order=asc&sort=created&q=" +
		"repo%3Ao%2Fr+is%3Aissue+is%3Aopen+label%3Astage%3Aready+is%3Ablocked"
	etag := ""
	steps := []struct {
		name, method, path, body string
		conditional              bool // If-None-Match: the issue's ETag
		status                   int
		resource                 string
		remaining                map[string]int // RateRemaining after it, by resource
	}{
		{"an issue read", http.MethodGet, issue, "", false, http.StatusOK, "core",
			map[string]int{"core": 4999, "search": 30, "graphql": 5000}},
		{"its repeat, a 304", http.MethodGet, issue, "", true, http.StatusNotModified, "core",
			map[string]int{"core": 4999, "search": 30, "graphql": 5000}},
		{"a 304 again", http.MethodGet, issue, "", true, http.StatusNotModified, "core",
			map[string]int{"core": 4999, "search": 30, "graphql": 5000}},
		{"a search", http.MethodGet, search, "", false, http.StatusOK, "search",
			map[string]int{"core": 4999, "search": 29, "graphql": 5000}},
		{"a GraphQL read", http.MethodPost, "/graphql", `{"query":"query { repository { issue { isPinned } } }"}`, false, http.StatusOK, "graphql",
			map[string]int{"core": 4999, "search": 29, "graphql": 4999}},
		{"a write", http.MethodPost, issue + "/comments", `{"body":"hi"}`, false, http.StatusCreated, "core",
			map[string]int{"core": 4998, "search": 29, "graphql": 4999}},
		{"a 404", http.MethodGet, "/repos/o/r/issues/99", "", false, http.StatusNotFound, "core",
			map[string]int{"core": 4997, "search": 29, "graphql": 4999}},
		{"the changed issue's read, no 304", http.MethodGet, issue, "", true, http.StatusOK, "core",
			map[string]int{"core": 4996, "search": 29, "graphql": 4999}},
	}
	limits := map[string]int{"core": 5000, "search": 30, "graphql": 5000}
	hc := &http.Client{Transport: f.Transport()}
	for _, s := range steps {
		req, err := http.NewRequest(s.method, f.URL()+s.path, strings.NewReader(s.body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "token fake-token")
		if s.conditional {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", s.name, err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			t.Fatalf("%s: read the body: %v", s.name, err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("%s: close the body: %v", s.name, err)
		}
		if s.path == issue && resp.StatusCode == http.StatusOK {
			etag = resp.Header.Get("ETag")
		}
		if resp.StatusCode != s.status {
			t.Fatalf("%s: status %d, want %d", s.name, resp.StatusCode, s.status)
		}
		want := s.remaining[s.resource]
		limit := limits[s.resource]
		h := resp.Header
		if h.Get("X-RateLimit-Resource") != s.resource || h.Get("X-RateLimit-Limit") != strconv.Itoa(limit) ||
			h.Get("X-RateLimit-Remaining") != strconv.Itoa(want) || h.Get("X-RateLimit-Used") != strconv.Itoa(limit-want) {
			t.Errorf("%s: resource %q, limit %q, remaining %q, used %q; want %s, %d, %d and %d", s.name,
				h.Get("X-RateLimit-Resource"), h.Get("X-RateLimit-Limit"), h.Get("X-RateLimit-Remaining"), h.Get("X-RateLimit-Used"),
				s.resource, limit, want, limit-want)
		}
		if h.Get("X-RateLimit-Reset") != "" {
			t.Errorf("%s: a reset of %q, want none, as the fake has none", s.name, h.Get("X-RateLimit-Reset"))
		}
		for resource, want := range s.remaining {
			if got := f.RateRemaining(resource); got != want {
				t.Errorf("%s: RateRemaining(%q) = %d, want %d", s.name, resource, got, want)
			}
		}
		if s.name == "a write" {
			// An edit of the body changes the issue's answer, so the ETag
			// held no longer matches.
			f.EditBody(n, "owner", "body, edited")
		}
	}
}
