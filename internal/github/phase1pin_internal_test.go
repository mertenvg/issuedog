package github

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestPhase1PinAppSlug pins AppSlug's success path, which epic #1's
// verifier found untested (#228): it returns the slug GET /app gives, not
// the App's name, and asks as the App itself, with its JWT, so no
// installation token is minted for it.
func TestPhase1PinAppSlug(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string // each request, as "METHOD /path Authorization-scheme"
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path+" "+scheme)
		mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/app" || scheme != "Bearer" {
			http.Error(w, `{"message":"only GET /app, with the App's JWT"}`, http.StatusUnauthorized)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{"id": 7, "slug": "issuedog-dev", "name": "Issuedog (dev)"}); err != nil {
			t.Error(err)
		}
	}))
	slug, err := c.AppSlug(context.Background())
	if err != nil || slug != "issuedog-dev" {
		t.Errorf("AppSlug = %q, %v; want the slug, issuedog-dev", slug, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "GET /app Bearer" {
		t.Errorf("requests %q; want one GET /app with the App's JWT, and no token minted", seen)
	}
}
