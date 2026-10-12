package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const fakeToken = "ghs_faketoken0123456789"

// run runs git in dir with a clean environment and returns its trimmed
// stdout.
func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(gitEnv(os.Environ(), ""),
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
	tests := []struct {
		name    string
		token   string
		want    []string
		notWant []string
	}{
		{
			name:  "with token",
			token: fakeToken,
			want: []string{
				"PATH=/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_COUNT=2",
				"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
				"GIT_CONFIG_KEY_1=http.extraHeader", "GIT_CONFIG_VALUE_1=" + header,
			},
			notWant: []string{"GIT_DIR=/agent/.git", "GIT_SSH_COMMAND=evil"},
		},
		{
			name:    "no token, no header",
			want:    []string{"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath"},
			notWant: []string{"GIT_CONFIG_KEY_1=http.extraHeader", "GIT_DIR=/agent/.git"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := gitEnv([]string{"PATH=/bin", "GIT_DIR=/agent/.git", "GIT_SSH_COMMAND=evil"}, tt.token)
			for _, w := range tt.want {
				if !slices.Contains(env, w) {
					t.Errorf("env lacks %q: %v", w, env)
				}
			}
			for _, nw := range tt.notWant {
				if slices.Contains(env, nw) {
					t.Errorf("env has %q", nw)
				}
			}
		})
	}
}
