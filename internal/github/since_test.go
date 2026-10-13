package github

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"
)

func TestSinceHelpers(t *testing.T) {
	since := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		path      string
		reply     string
		call      func(*Client) (int, error)
		wantQuery map[string]string
		wantN     int
	}{
		{"issues", "/repos/o/r/issues", `[{"number":1},{"number":2}]`,
			func(c *Client) (int, error) { return count(c.IssuesSince(context.Background(), "o", "r", since)) },
			map[string]string{"since": "2026-10-01T12:00:00Z", "state": "all", "sort": "updated", "direction": "asc"}, 2},
		{"repo comments", "/repos/o/r/issues/comments", `[{"id":1}]`,
			func(c *Client) (int, error) { return count(c.CommentsSince(context.Background(), "o", "r", 0, since)) },
			map[string]string{"since": "2026-10-01T12:00:00Z", "sort": "updated", "direction": "desc"}, 1},
		{"timeline filters by creation time", "/repos/o/r/issues/5/timeline",
			`[{"created_at":"2026-09-30T00:00:00Z"},{"created_at":"2026-10-01T12:00:00Z"},
			  {"event":"committed"},{"created_at":"2026-10-02T00:00:00Z"},{"event":"reviewed","submitted_at":"2026-10-02T00:00:00Z"}]`,
			func(c *Client) (int, error) { return count(c.TimelineSince(context.Background(), "o", "r", 5, since)) },
			map[string]string{"per_page": "100", "since": ""}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got url.Values
			c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tt.path {
					http.NotFound(w, r)
					return
				}
				got = r.URL.Query()
				_, _ = w.Write([]byte(tt.reply))
			}))
			n, err := tt.call(c)
			if err != nil {
				t.Fatal(err)
			}
			if n != tt.wantN {
				t.Errorf("got %d items, want %d", n, tt.wantN)
			}
			for k, v := range tt.wantQuery {
				if got.Get(k) != v {
					t.Errorf("query %s = %q, want %q", k, got.Get(k), v)
				}
			}
		})
	}
}

func count[T any](items []T, err error) (int, error) { return len(items), err }
