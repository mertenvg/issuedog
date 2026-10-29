package github

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// ReadyBlocked skips a pull request in the search's answer, though
// is:issue should leave them out, and keeps the issues.
func TestReadyBlockedSkipsPullRequests(t *testing.T) {
	t.Parallel()
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v any
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
			w.WriteHeader(http.StatusCreated)
			v = map[string]any{"token": "tok", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues" && strings.Contains(r.URL.Query().Get("q"), "is:blocked"):
			v = map[string]any{"total_count": 3, "incomplete_results": false, "items": []map[string]any{
				{"number": 4},
				{"number": 5, "pull_request": map[string]any{"url": "https://api.github.com/repos/o/r/pulls/5"}},
				{"number": 6},
			}}
		default:
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Errorf("encode the answer: %v", err)
		}
	}))
	got, err := c.ReadyBlocked(context.Background(), "o", "r")
	if err != nil {
		t.Fatal(err)
	}
	if numbers := slices.Sorted(maps.Keys(got)); !slices.Equal(numbers, []int{4, 6}) {
		t.Errorf("found %v, want [4 6]: the pull request skipped", numbers)
	}
}
