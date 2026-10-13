package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

const fakeToken = "ghs_faketoken0123456789"

// run runs git in dir with a clean environment and returns its trimmed
// stdout.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(os.Environ(), "", ""),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// remote is a local bare repo standing in for GitHub, with a work tree that
// pushes commits to it.
type remote struct {
	t    *testing.T
	url  string
	work string
}

func newRemote(t *testing.T) *remote {
	t.Helper()
	dir := t.TempDir()
	r := &remote{t: t, url: filepath.Join(dir, "remote.git"), work: filepath.Join(dir, "work")}
	run(t, dir, "init", "--bare", "--quiet", "--initial-branch=main", r.url)
	run(t, dir, "init", "--quiet", "--initial-branch=main", r.work)
	r.commit("README.md", "hello\n")
	return r
}

// commit writes name with content, commits it and pushes main. It returns
// the new commit's SHA.
func (r *remote) commit(name, content string) string {
	r.t.Helper()
	path := filepath.Join(r.work, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	run(r.t, r.work, "add", name)
	run(r.t, r.work, "commit", "--quiet", "-m", "add "+name)
	run(r.t, r.work, "push", "--quiet", r.url, "main")
	return run(r.t, r.work, "rev-parse", "HEAD")
}

func synced(t *testing.T) (*remote, *Repo) {
	t.Helper()
	rem := newRemote(t)
	r := New(rem.url, filepath.Join(t.TempDir(), "target.git"))
	if err := r.Sync(context.Background(), fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return rem, r
}

func TestSyncClones(t *testing.T) {
	rem, r := synced(t)
	if got := run(t, r.dir, "rev-parse", "--is-bare-repository"); got != "true" {
		t.Errorf("is-bare-repository = %q, want true", got)
	}
	want := run(t, rem.work, "rev-parse", "HEAD")
	if got := run(t, r.dir, "rev-parse", "main"); got != want {
		t.Errorf("main = %s, want the remote's %s", got, want)
	}
}

func TestSyncFetchesNewCommit(t *testing.T) {
	rem, r := synced(t)
	want := rem.commit("docs/new.md", "new\n")
	if err := r.Sync(context.Background(), fakeToken); err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if got := run(t, r.dir, "rev-parse", "main"); got != want {
		t.Errorf("main = %s, want the new commit %s", got, want)
	}
}

func TestSyncIgnoresInheritedGitEnv(t *testing.T) {
	elsewhere := t.TempDir()
	t.Setenv("GIT_DIR", elsewhere)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='"+elsewhere+"'")
	_, r := synced(t)
	if _, err := os.Stat(filepath.Join(r.dir, "HEAD")); err != nil {
		t.Errorf("bare repo not at %s: %v", r.dir, err)
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("git wrote to the inherited GIT_DIR: %v", entries)
	}
}

func TestSyncRunsNoHooks(t *testing.T) {
	rem, r := synced(t)
	marker := filepath.Join(t.TempDir(), "ran")
	hook := filepath.Join(r.dir, "hooks", "reference-transaction")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	rem.commit("b.txt", "b\n")
	if err := r.Sync(context.Background(), fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the reference-transaction hook ran")
	}
}

func TestReadFile(t *testing.T) {
	rem, r := synced(t)
	rem.commit(".issuedog/config.yaml", "types: labels\n")
	if err := r.Sync(context.Background(), fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tests := []struct {
		name     string
		path     string
		want     string
		notFound bool
		wantErr  bool
	}{
		{name: "top level", path: "README.md", want: "hello\n"},
		{name: "nested", path: ".issuedog/config.yaml", want: "types: labels\n"},
		{name: "missing", path: "CODEOWNERS", notFound: true, wantErr: true},
		{name: "missing nested", path: ".issuedog/prompts/planner.md", notFound: true, wantErr: true},
		{name: "directory", path: ".issuedog", wantErr: true},
		{name: "empty path", path: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.ReadFile(context.Background(), tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ReadFile(%q) err = %v, want error %v", tt.path, err, tt.wantErr)
			}
			if errors.Is(err, ErrNotFound) != tt.notFound {
				t.Errorf("ReadFile(%q) err = %v, want ErrNotFound %v", tt.path, err, tt.notFound)
			}
			if string(got) != tt.want {
				t.Errorf("ReadFile(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestTokenNeverOnDisk(t *testing.T) {
	rem, r := synced(t)
	rem.commit("c.txt", "c\n")
	if err := r.Sync(context.Background(), fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	secrets := []string{fakeToken, base64.StdEncoding.EncodeToString([]byte("x-access-token:" + fakeToken))}
	err := filepath.WalkDir(r.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, s := range secrets {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s contains the token", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.dir, "config")); err != nil {
		t.Errorf("no config file to check: %v", err)
	}
}

func TestGitEnv(t *testing.T) {
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeToken))
	base := []string{
		"PATH=/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=", "LC_ALL=C",
	}
	const remote = "https://github.com/o/r.git"
	tests := []struct {
		name   string
		remote string
		token  string
		want   []string
	}{
		{
			name:   "remote and token",
			remote: remote,
			token:  fakeToken,
			want: append(slices.Clone(base), "GIT_CONFIG_COUNT=4",
				"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
				"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false",
				"GIT_CONFIG_KEY_2=http."+remote+".followRedirects", "GIT_CONFIG_VALUE_2=false",
				"GIT_CONFIG_KEY_3=http.extraHeader", "GIT_CONFIG_VALUE_3="+header,
			),
		},
		{
			name:  "token, no remote",
			token: fakeToken,
			want: append(slices.Clone(base), "GIT_CONFIG_COUNT=3",
				"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
				"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false",
				"GIT_CONFIG_KEY_2=http.extraHeader", "GIT_CONFIG_VALUE_2="+header,
			),
		},
		{
			name: "no remote, no token, no header",
			want: append(slices.Clone(base), "GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
				"GIT_CONFIG_KEY_1=http.followRedirects", "GIT_CONFIG_VALUE_1=false",
			),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := gitEnv([]string{"PATH=/bin", "GIT_DIR=/agent/.git", "GIT_SSH_COMMAND=evil", "SSH_ASKPASS=/evil"}, tt.remote, tt.token)
			if !slices.Equal(env, tt.want) {
				t.Errorf("env =\n%q\nwant\n%q", env, tt.want)
			}
		})
	}
}

// TestSyncRefusesRedirect has a remote answer every request under /old.git
// with a redirect to /moved.git, on another server or on itself. Git must not
// follow it, so /moved.git gets no request, let alone the token.
func TestSyncRefusesRedirect(t *testing.T) {
	header := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeToken))
	tests := []struct {
		name      string
		code      int
		crossHost bool
		follow    bool // the bare repo's own config asks to follow redirects from the remote
	}{
		{name: "cross host", code: http.StatusMovedPermanently, crossHost: true},
		{name: "same host", code: http.StatusTemporaryRedirect},
		{name: "cross host, repo config follows", code: http.StatusFound, crossHost: true, follow: true},
		{name: "same host, repo config follows", code: http.StatusMovedPermanently, follow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked, moved []string // requests to /old.git and to /moved.git, with their Authorization
			record := func(to *[]string, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				*to = append(*to, r.Method+" "+r.URL.Path+" Authorization="+r.Header.Get("Authorization"))
			}
			target := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				record(&moved, r)
				http.NotFound(w, r)
			})
			other := httptest.NewServer(target)
			t.Cleanup(other.Close)
			prefix := "" // a path, so the same host
			if tt.crossHost {
				prefix = other.URL
			}
			mux := http.NewServeMux()
			mux.Handle("/moved.git/", target)
			mux.HandleFunc("/old.git/", func(w http.ResponseWriter, r *http.Request) {
				record(&asked, r)
				to := prefix + "/moved.git/" + strings.TrimPrefix(r.URL.Path, "/old.git/") + "?" + r.URL.RawQuery
				http.Redirect(w, r, to, tt.code)
			})
			first := httptest.NewServer(mux)
			t.Cleanup(first.Close)

			url := first.URL + "/old.git"
			dir := filepath.Join(t.TempDir(), "target.git")
			if tt.follow {
				run(t, filepath.Dir(dir), "init", "--bare", "--quiet", "--initial-branch=main", dir)
				run(t, dir, "config", "http."+url+".followRedirects", "true")
			}
			r := New(url, dir)
			err := r.Sync(context.Background(), fakeToken)
			if err == nil {
				t.Fatal("Sync followed a redirect; want an error")
			}
			code := strconv.Itoa(tt.code)
			for _, want := range []string{"fetch " + url + ": the remote redirected (HTTP " + code + ")", "ISSUEDOG_REPO", "returned error: " + code} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Sync err = %v, want it to contain %q", err, want)
				}
			}
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Errorf("Sync err = %v, want it to wrap git's *exec.ExitError", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if want := "GET /old.git/info/refs Authorization=" + header; !slices.Equal(asked, []string{want}) {
				t.Errorf("/old.git got %q, want only %q", asked, want)
			}
			if len(moved) != 0 {
				t.Errorf("git followed the redirect: /moved.git got %q", moved)
			}
		})
	}
}

func TestSyncOtherFailureNamesNoRedirect(t *testing.T) {
	refuse := func(code int) string {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, http.StatusText(code), code)
		}))
		t.Cleanup(s.Close)
		return s.URL + "/repo.git"
	}
	tests := []struct {
		name   string
		remote string
		want   string // in git's error
	}{
		{name: "403", remote: refuse(http.StatusForbidden), want: "returned error: 403"},
		{name: "404", remote: refuse(http.StatusNotFound), want: "not found"},
		{name: "missing local repo", remote: filepath.Join(t.TempDir(), "missing.git"), want: "does not appear to be a git repository"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(tt.remote, filepath.Join(t.TempDir(), "target.git"))
			err := r.Sync(context.Background(), fakeToken)
			if err == nil {
				t.Fatal("Sync: want an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Sync err = %v, want it to contain %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "redirect") || strings.Contains(err.Error(), "ISSUEDOG_REPO") {
				t.Errorf("Sync err = %v, want no redirect hint", err)
			}
		})
	}
}
