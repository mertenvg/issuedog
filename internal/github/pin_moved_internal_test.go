package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"sync"
	"testing"
)

// pinMovedServer answers each "METHOD /path" in replies with its status
// and JSON body, sending a Location for a 301, and anything else with a
// 404. It returns a client of it and the requests it got, as "METHOD
// /path", in order.
func pinMovedServer(t *testing.T, replies map[string]struct {
	status         int
	location, body string
}) (*Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var got []string
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.URL.Path)
		mu.Unlock()
		reply, ok := replies[r.Method+" "+r.URL.Path]
		if !ok {
			reply.status, reply.body = http.StatusNotFound, `{"message":"Not Found"}`
		}
		if reply.location != "" {
			w.Header().Set("Location", reply.location)
		}
		w.WriteHeader(reply.status)
		if _, err := io.WriteString(w, reply.body); err != nil {
			t.Errorf("reply to %s %s: %v", r.Method, r.URL.Path, err)
		}
	}))
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(got)
	}
}

// PinIssue pins nothing when its read of the issue shows it moved to
// another repo (#413), as node IDs are global and pinIssue would pin it
// there: a read GitHub answered from another path, which the client
// followed a 301 to, though the issue names no repo, or an issue whose
// repository_url names another repo, though it was read at this repo's
// path. Its error wraps ErrTransferred, and no mutation is sent. An issue
// that names this repo in another case is no move, and is pinned.
func TestPinMovedNotPinned(t *testing.T) {
	t.Parallel()
	type reply = struct {
		status         int
		location, body string
	}
	pinned := reply{http.StatusOK, "", `{"data":{"pinIssue":{"issue":{"isPinned":true}}}}`}
	for _, tc := range []struct {
		name    string
		replies map[string]reply
		want    string // the error's text after "pin o/r#7: ", or "" for none
		reqs    []string
	}{
		{"redirected", map[string]reply{
			"GET /repos/o/r/issues/7":      {http.StatusMovedPermanently, "/repositories/2/issues/7", `{"message":"Moved Permanently"}`},
			"GET /repositories/2/issues/7": {http.StatusOK, "", `{"number":7,"node_id":"I_7"}`},
			"POST /graphql":                pinned,
		}, "transferred: answered by GET /repositories/2/issues/7", []string{"GET /repos/o/r/issues/7", "GET /repositories/2/issues/7"}},
		{"another repo's issue", map[string]reply{
			"GET /repos/o/r/issues/7": {http.StatusOK, "", `{"number":7,"node_id":"I_7","repository_url":"https://api.github.com/repos/o/elsewhere"}`},
			"POST /graphql":           pinned,
		}, "transferred to https://api.github.com/repos/o/elsewhere", []string{"GET /repos/o/r/issues/7"}},
		{"this repo in another case", map[string]reply{
			"GET /repos/o/r/issues/7": {http.StatusOK, "", `{"number":7,"node_id":"I_7","repository_url":"https://api.github.com/repos/O/R"}`},
			"POST /graphql":           pinned,
		}, "", []string{"GET /repos/o/r/issues/7", "POST /graphql"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, reqs := pinMovedServer(t, tc.replies)
			err := c.PinIssue(context.Background(), "o", "r", 7)
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("PinIssue: %v", err)
			case tc.want != "" && (!errors.Is(err, ErrTransferred) || err.Error() != "pin o/r#7: "+tc.want):
				t.Errorf("PinIssue = %v; want %q, wrapping ErrTransferred", err, "pin o/r#7: "+tc.want)
			}
			if got := reqs(); !slices.Equal(got, tc.reqs) {
				t.Errorf("requests = %q; want %q", got, tc.reqs)
			}
		})
	}
}
