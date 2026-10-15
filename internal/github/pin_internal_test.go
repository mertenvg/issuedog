package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
)

// pinServer answers the issue's GET with issue, a GraphQL POST with gql,
// and a POST of an issue with created, each as JSON.
func pinServer(t *testing.T, issue, gql, created string) *Client {
	t.Helper()
	return newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := map[string]string{"GET /repos/o/r/issues/7": issue, "POST /graphql": gql, "POST /repos/o/r/issues": created}[r.Method+" "+r.URL.Path]
		if body == "" {
			w.WriteHeader(http.StatusNotFound)
			body = `{"message":"Not Found"}`
		}
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("reply to %s %s: %v", r.Method, r.URL.Path, err)
		}
	}))
}

// The client checks what GitHub answers, and fails rather than report a
// pin, a pinned state or an issue number it wasn't given.
func TestPinAnswersChecked(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, issue, gql, created string
		do                        func(*Client) error
		want                      error
	}{
		{"no node ID", `{"number":7}`, `{"data":{"pinIssue":{"issue":{"isPinned":true}}}}`, "",
			func(c *Client) error { return c.PinIssue(ctx, "o", "r", 7) }, errNoNodeID},
		{"not pinned", `{"number":7,"node_id":"I_7"}`, `{"data":{"pinIssue":{"issue":{"isPinned":false}}}}`, "",
			func(c *Client) error { return c.PinIssue(ctx, "o", "r", 7) }, errNotPinned},
		{"no payload", `{"number":7,"node_id":"I_7"}`, `{"data":{"pinIssue":null}}`, "",
			func(c *Client) error { return c.PinIssue(ctx, "o", "r", 7) }, errNotPinned},
		{"no issue in the payload", `{"number":7,"node_id":"I_7"}`, `{"data":{"pinIssue":{"issue":null}}}`, "",
			func(c *Client) error { return c.PinIssue(ctx, "o", "r", 7) }, errNotPinned},
		{"no issue to ask about", "", `{"data":{"repository":{"issue":null}}}`, "",
			func(c *Client) error { _, err := c.IsPinned(ctx, "o", "r", 7); return err }, errNoIssue},
		{"no repository to ask about", "", `{"data":{"repository":null}}`, "",
			func(c *Client) error { _, err := c.IsPinned(ctx, "o", "r", 7); return err }, errNoIssue},
		{"no number", "", "", `{"title":"issuedog digest"}`,
			func(c *Client) error { _, err := c.CreateIssue(ctx, "o", "r", "issuedog digest", ""); return err }, errNoNumber},
	} {
		if err := tc.do(pinServer(t, tc.issue, tc.gql, tc.created)); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// A good answer to each is no error.
	c := pinServer(t, `{"number":7,"node_id":"I_7"}`, `{"data":{"pinIssue":{"issue":{"isPinned":true}}}}`, `{"number":8}`)
	if err := c.PinIssue(ctx, "o", "r", 7); err != nil {
		t.Errorf("PinIssue: %v", err)
	}
	if n, err := c.CreateIssue(ctx, "o", "r", "issuedog digest", ""); err != nil || n != 8 {
		t.Errorf("CreateIssue = %d, %v; want 8", n, err)
	}
}
