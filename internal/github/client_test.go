package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
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

func newClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
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

// A transferred issue's 301 stays on the API host and is followed. A
// redirect elsewhere isn't, so the token never leaves the host.
func TestRedirectsStayOnHost(t *testing.T) {
	var elsewhere []string // Authorization headers another host saw
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere = append(elsewhere, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"number":1}`))
	}))
	t.Cleanup(other.Close)
	f := &fakeGitHub{ttl: time.Hour}
	c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/issues/1", "/app":
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusMovedPermanently)
		case "/repos/o/r/issues/2":
			http.Redirect(w, r, "/repos/o/moved/issues/9", http.StatusMovedPermanently)
		case "/repos/o/r/issues/3": // the same host, but not TLS
			http.Redirect(w, r, "https://"+r.Host+r.URL.Path, http.StatusMovedPermanently)
		case "/repos/o/r/issues/4": // a loop
			http.Redirect(w, r, r.URL.Path, http.StatusMovedPermanently)
		case "/repos/o/moved/issues/9":
			_, _ = w.Write([]byte(`{"number":9}`))
		default:
			f.ServeHTTP(w, r)
		}
	}))
	if _, _, err := c.Issues.Get(context.Background(), "o", "r", 1); !errors.Is(err, ErrCrossHostRedirect) {
		t.Errorf("Get of an issue redirected to another host: %v, want ErrCrossHostRedirect", err)
	}
	if _, _, err := c.Issues.Get(context.Background(), "o", "r", 3); !errors.Is(err, ErrCrossHostRedirect) {
		t.Errorf("Get of an issue redirected to another scheme: %v, want ErrCrossHostRedirect", err)
	}
	if _, _, err := c.Issues.Get(context.Background(), "o", "r", 4); err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
		t.Errorf("Get of an issue in a redirect loop: %v, want it stopped after 10", err)
	}
	if _, err := c.AppSlug(context.Background()); !errors.Is(err, ErrCrossHostRedirect) {
		t.Errorf("AppSlug redirected to another host: %v, want ErrCrossHostRedirect", err)
	}
	if len(elsewhere) != 0 {
		t.Errorf("the other host got %d requests, with Authorization %q; want none", len(elsewhere), elsewhere)
	}
	if is, _, err := c.Issues.Get(context.Background(), "o", "r", 2); err != nil || is.GetNumber() != 9 {
		t.Errorf("Get of a transferred issue = %v, %v; want #9 in its new repo", is, err)
	}
	// Minting, with the App's JWT, follows no redirect at all.
	mint := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusFound)
	}))
	if _, err := mint.Token(context.Background()); err == nil {
		t.Error("Token with its mint redirected to another host succeeded, want an error")
	}
	if len(elsewhere) != 0 {
		t.Errorf("the other host got the mint, with Authorization %q; want none", elsewhere)
	}
}

func TestAppSlugFailures(t *testing.T) {
	for reply, want := range map[string]string{`{"id":7}`: "GET /app gave no slug", "": "github app: "} {
		c := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if reply == "" || r.URL.Path != "/app" {
				http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
				return
			}
			_, _ = w.Write([]byte(reply))
		}))
		if slug, err := c.AppSlug(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("reply %q: AppSlug = %q, %v; want an error with %q", reply, slug, err, want)
		}
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
