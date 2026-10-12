package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// etagServer serves etag (empty: none) and body, 304s a match, records If-None-Match.
type etagServer struct {
	mu   sync.Mutex
	etag string
	body string
	inm  []string
}

func (s *etagServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inm := r.Header.Get("If-None-Match")
	s.inm = append(s.inm, inm)
	if s.etag != "" && inm == s.etag {
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if s.etag != "" {
		w.Header().Set("ETag", s.etag)
	}
	w.Header().Set("X-RateLimit-Remaining", "4998")
	_, _ = io.WriteString(w, s.body)
}

func TestETagCache(t *testing.T) {
	type step struct {
		method, accept string
		etag, body     string // what the server holds before this request
		wantINM        string
		wantBody       string
	}
	tests := []struct {
		name  string
		steps []step
	}{
		{"second GET is conditional and a 304 returns the cached body", []step{
			{"GET", "", `"v1"`, "a", "", "a"},
			{"GET", "", `"v1"`, "b", `"v1"`, "a"},
		}},
		{"a 200 with a new ETag replaces the entry", []step{
			{"GET", "", `"v1"`, "a", "", "a"},
			{"GET", "", `"v2"`, "b", `"v1"`, "b"},
			{"GET", "", `"v2"`, "c", `"v2"`, "b"},
		}},
		{"non-GET requests bypass the cache", []step{
			{"GET", "", `"v1"`, "a", "", "a"},
			{"PATCH", "", `"v1"`, "b", "", "b"},
			{"POST", "", `"v1"`, "c", "", "c"},
		}},
		{"responses without an ETag are not cached", []step{
			{"GET", "", "", "a", "", "a"},
			{"GET", "", "", "b", "", "b"},
		}},
		{"a full last page is not cached, since its 304 would hide a new page", []step{
			{"GET", "", `"v1"`, "[1,2]", "", "[1,2]"},
			{"GET", "", `"v1"`, "[1,2]", "", "[1,2]"},
		}},
		{"each Accept has its own entry", []step{
			{"GET", "application/json", `"v1"`, "a", "", "a"},
			{"GET", "text/plain", `"v1"`, "b", "", "b"},
			{"GET", "application/json", `"v1"`, "c", `"v1"`, "a"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &etagServer{}
			ts := httptest.NewServer(srv)
			defer ts.Close()
			hc := &http.Client{Transport: newETagCache(http.DefaultTransport)}
			for i, st := range tt.steps {
				srv.mu.Lock()
				srv.etag, srv.body = st.etag, st.body
				srv.mu.Unlock()
				req, _ := http.NewRequest(st.method, ts.URL+"/res?per_page=2", nil)
				req.Header.Set("Accept", st.accept)
				resp, err := hc.Do(req)
				if err != nil {
					t.Fatalf("step %d: %v", i, err)
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK || string(body) != st.wantBody {
					t.Errorf("step %d: got %d %q, want 200 %q", i, resp.StatusCode, body, st.wantBody)
				}
				if got := srv.inm[i]; got != st.wantINM {
					t.Errorf("step %d: If-None-Match = %q, want %q", i, got, st.wantINM)
				}
			}
		})
	}
}

// newClientWith returns a Client on a fake GitHub that mints tokens and passes the rest to h.
func newClientWith(t *testing.T, h http.Handler) *Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/42/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.Handle("/", h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 7, InstallationID: 42, Key: testKey(t), BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// go-github must see a 304 as a success with fresh rate limits, race-free under -race.
func TestClientTreats304AsCachedSuccess(t *testing.T) {
	srv := &etagServer{etag: `"v1"`, body: `{"name":"issuedog"}`}
	c := newClientWith(t, srv)
	for i, wantRemaining := range []int{4998, 4999} {
		repo, resp, err := c.Repositories.Get(context.Background(), "mertenvg", "issuedog")
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if repo.GetName() != "issuedog" || resp.Rate.Remaining != wantRemaining {
			t.Errorf("call %d: name %q, remaining %d; want issuedog, %d", i, repo.GetName(), resp.Rate.Remaining, wantRemaining)
		}
	}
	if got := strings.Join(srv.inm, ","); got != `,"v1"` {
		t.Errorf("If-None-Match headers = %s, want none then \"v1\"", got)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if repo, _, err := c.Repositories.Get(context.Background(), "mertenvg", "issuedog"); err != nil || repo.GetName() != "issuedog" {
				t.Errorf("concurrent call: %v, %v", repo, err)
			}
		})
	}
	wg.Wait()
}
