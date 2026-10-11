package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

// fakeGitHub serves the installation token endpoint and one repo endpoint.
// Each minted token lives for ttl. If failMint is set, minting returns 500.
type fakeGitHub struct {
	mu       sync.Mutex
	ttl      time.Duration
	failMint bool
	minted   int
	auth     []string // Authorization headers seen on the repo endpoint
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/42/access_tokens":
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "missing app JWT", http.StatusUnauthorized)
			return
		}
		if f.failMint {
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
			return
		}
		f.minted++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      fmt.Sprintf("tok-%d", f.minted),
			"expires_at": time.Now().Add(f.ttl).UTC().Format(time.RFC3339),
		})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/mertenvg/issuedog":
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "name": "issuedog"})
	default:
		http.NotFound(w, r)
	}
}

func newClient(t *testing.T, f *fakeGitHub) *Client {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{AppID: 7, InstallationID: 42, Key: testKey(t), BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func getRepo(t *testing.T, c *Client) error {
	t.Helper()
	_, _, err := c.Repositories.Get(context.Background(), "mertenvg", "issuedog")
	return err
}

func TestTokenMintedOnFirstCallThenReused(t *testing.T) {
	f := &fakeGitHub{ttl: time.Hour}
	c := newClient(t, f)
	if f.minted != 0 {
		t.Fatalf("New minted %d tokens, want 0", f.minted)
	}
	for range 2 {
		if err := getRepo(t, c); err != nil {
			t.Fatal(err)
		}
	}
	if f.minted != 1 {
		t.Errorf("minted %d tokens for two calls, want 1", f.minted)
	}
	for _, a := range f.auth {
		if a != "token tok-1" {
			t.Errorf("Authorization = %q, want the installation token", a)
		}
	}
	tok, err := c.Token(context.Background())
	if err != nil || tok != "tok-1" {
		t.Errorf("Token() = %q, %v; want the cached tok-1", tok, err)
	}
}

func TestTokenRefreshedNearExpiry(t *testing.T) {
	// ghinstallation treats a token expiring within a minute as expired.
	f := &fakeGitHub{ttl: 30 * time.Second}
	c := newClient(t, f)
	for range 2 {
		if err := getRepo(t, c); err != nil {
			t.Fatal(err)
		}
	}
	if f.minted != 2 {
		t.Errorf("minted %d tokens, want 2 (the first was near expiry)", f.minted)
	}
	if got := f.auth[len(f.auth)-1]; got != "token tok-2" {
		t.Errorf("second call used %q, want the new token", got)
	}
}

func TestMintFailureSurfaces(t *testing.T) {
	f := &fakeGitHub{ttl: time.Hour, failMint: true}
	c := newClient(t, f)
	if err := getRepo(t, c); err == nil || !strings.Contains(err.Error(), "github installation transport") {
		t.Fatalf("API call err = %v, want an annotated minting error", err)
	}
	if len(f.auth) != 0 {
		t.Errorf("repo endpoint was called %d times without a token", len(f.auth))
	}
	if _, err := c.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "github installation token") {
		t.Errorf("Token() err = %v, want an annotated minting error", err)
	}
}

func TestNewRejectsBadKey(t *testing.T) {
	_, err := New(Config{AppID: 7, InstallationID: 42, Key: []byte("not a key")})
	if err == nil || !strings.Contains(err.Error(), "github app 7 installation 42") {
		t.Fatalf("err = %v, want an annotated key error", err)
	}
}

// A failed mint must not leave its connection open. ghinstallation keeps the
// response in its error without closing it.
func TestMintFailureDoesNotLeak(t *testing.T) {
	f := &fakeGitHub{ttl: time.Hour, failMint: true}
	c := newClient(t, f)
	_ = getRepo(t, c) // let the client's connection pool settle
	before := runtime.NumGoroutine()
	for range 25 {
		_ = getRepo(t, c)
		_, _ = c.Token(context.Background())
	}
	// Closed connections' goroutines exit asynchronously.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before+5 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Errorf("goroutines went from %d to %d over 50 failed mints", before, after)
	}
}
