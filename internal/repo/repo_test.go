package repo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
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
		notFile  bool
		wantErr  bool
	}{
		{name: "top level", path: "README.md", want: "hello\n"},
		{name: "nested", path: ".issuedog/config.yaml", want: "types: labels\n"},
		{name: "missing", path: "CODEOWNERS", notFound: true, wantErr: true},
		{name: "missing nested", path: ".issuedog/prompts/planner.md", notFound: true, wantErr: true},
		{name: "directory", path: ".issuedog", notFile: true, wantErr: true},
		{name: "under a file", path: "README.md/x", notFound: true, wantErr: true},
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
			if errors.Is(err, ErrNotFile) != tt.notFile {
				t.Errorf("ReadFile(%q) err = %v, want ErrNotFile %v", tt.path, err, tt.notFile)
			}
			if string(got) != tt.want {
				t.Errorf("ReadFile(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// TestResolve resolves main to its commit locally, and refuses a missing
// ref, a ref that names a tree rather than a commit, and an option.
func TestResolve(t *testing.T) {
	t.Parallel()
	rem, r := synced(t)
	main := run(t, rem.work, "rev-parse", "HEAD")
	tree := run(t, r.dir, "rev-parse", "main^{tree}")
	run(t, r.dir, "update-ref", "refs/issuedog/tree", tree)
	got, err := r.Resolve(context.Background(), "main")
	if err != nil || got != main {
		t.Errorf("Resolve(main) = %q, %v; want %s", got, err, main)
	}
	for _, ref := range []string{"nope", "refs/issuedog/tree", "--all"} {
		if got, err := r.Resolve(context.Background(), ref); err == nil {
			t.Errorf("Resolve(%s) = %q, want an error", ref, got)
		}
	}
}

// TestHasCommit finds main's commit in the bare repo, and no commit for a
// SHA it lacks, a tree, or text that isn't a full SHA, none of them an
// error. A repo that can't be read is an error.
func TestHasCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, r := synced(t)
	main := run(t, r.dir, "rev-parse", "main")
	tree := run(t, r.dir, "rev-parse", "main^{tree}")
	for _, tt := range []struct {
		sha  string
		want bool
	}{
		{main, true},
		{strings.Repeat("1", 40), false},
		{tree, false},
		{main[:12], false},
		{"--all", false},
	} {
		if got, err := r.HasCommit(ctx, tt.sha); got != tt.want || err != nil {
			t.Errorf("HasCommit(%q) = %t, %v; want %t, nil", tt.sha, got, err, tt.want)
		}
	}
	gone := New("", filepath.Join(t.TempDir(), "gone.git"))
	if got, err := gone.HasCommit(ctx, main); err == nil {
		t.Errorf("HasCommit on a repo that isn't there = %t, nil; want an error", got)
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

// commitOnly writes name with content in the remote's work tree and commits
// it, without pushing. It returns the new commit's SHA.
func (r *remote) commitOnly(name, content string) string {
	r.t.Helper()
	if err := os.WriteFile(filepath.Join(r.work, name), []byte(content), 0o644); err != nil {
		r.t.Fatal(err)
	}
	run(r.t, r.work, "add", name)
	run(r.t, r.work, "commit", "--quiet", "-m", "add "+name)
	return run(r.t, r.work, "rev-parse", "HEAD")
}

// bundle writes a bundle of revs, made in the remote's work tree, to a new
// file, and returns its path.
func (r *remote) bundle(revs ...string) string {
	r.t.Helper()
	path := filepath.Join(r.t.TempDir(), "run.bundle")
	run(r.t, r.work, append([]string{"bundle", "create", "--quiet", path}, revs...)...)
	return path
}

// refs lists the bare repo's refs, with the objects they name.
func refs(t *testing.T, r *Repo) string {
	t.Helper()
	return run(t, r.dir, "for-each-ref", "--format=%(objectname) %(refname)")
}

// packs lists the bare repo's pack files.
func packs(t *testing.T, r *Repo) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(r.dir, "objects", "pack", "*"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// TestFetchBundle takes a run's two commits past main into the bare repo
// from a bundle that also holds a tag. Only its HEAD is written, to the
// run's ref. Fetching it again writes nothing and returns the same head,
// without reading the file.
func TestFetchBundle(t *testing.T) {
	t.Parallel()
	rem, r := synced(t)
	ctx := context.Background()
	start := run(t, rem.work, "rev-parse", "HEAD")
	run(t, rem.work, "checkout", "--quiet", "--detach")
	rem.commitOnly("a.txt", "a\n")
	head := rem.commitOnly("b.txt", "b\n")
	run(t, rem.work, "tag", "--annotate", "-m", "v9", "v9", head)
	path := rem.bundle(start+"..HEAD", "v9")
	if got := run(t, rem.work, "bundle", "list-heads", path); !strings.Contains(got, "refs/tags/v9") {
		t.Fatalf("the bundle holds no tag, so the test shows nothing: %s", got)
	}

	got, err := r.FetchBundle(ctx, path, "r1", start)
	if err != nil || got != head {
		t.Fatalf("FetchBundle = %q, %v; want %s", got, err, head)
	}
	want := start + " refs/heads/main\n" + head + " " + RunsRef + "r1"
	if got := refs(t, r); got != want {
		t.Errorf("refs after the fetch =\n%s\nwant\n%s", got, want)
	}
	if got := run(t, r.dir, "rev-list", "--count", start+".."+RunsRef+"r1"); got != "2" {
		t.Errorf("the run's ref has %s commits past the start, want 2", got)
	}

	before := packs(t, r)
	again, err := r.FetchBundle(ctx, filepath.Join(t.TempDir(), "missing.bundle"), "r1", start)
	if err != nil || again != head {
		t.Errorf("FetchBundle again = %q, %v; want %s, without reading the file", again, err, head)
	}
	if got := refs(t, r); got != want {
		t.Errorf("refs after fetching again =\n%s\nwant\n%s", got, want)
	}
	if after := packs(t, r); !slices.Equal(after, before) {
		t.Errorf("fetching again wrote packs: %q, then %q", before, after)
	}
}

// TestFetchBundleSHA256 takes a run's commit from a bundle into a bare
// repo of SHA-256 objects.
func TestFetchBundleSHA256(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rem := &remote{t: t, url: filepath.Join(dir, "remote.git"), work: filepath.Join(dir, "work")}
	run(t, dir, "init", "--bare", "--quiet", "--initial-branch=main", "--object-format=sha256", rem.url)
	run(t, dir, "init", "--quiet", "--initial-branch=main", "--object-format=sha256", rem.work)
	start := rem.commit("README.md", "hello\n")
	r := New(rem.url, filepath.Join(dir, "target.git"))
	run(t, dir, "clone", "--quiet", "--bare", rem.url, r.dir)
	head := rem.commitOnly("a.txt", "a\n")
	got, err := r.FetchBundle(context.Background(), rem.bundle(start+"..HEAD"), "r1", start)
	if err != nil || got != head {
		t.Errorf("FetchBundle = %q, %v; want %s", got, err, head)
	}
}

// TestFetchBundleRefuses refuses bad run IDs and starts, files that aren't
// bundles of a run's commits past its start, objects fsck rejects or that
// name objects the pack lacks, and a run's ref another fetch wrote after
// the read. Each leaves the refs as they were, the other fetch's aside,
// and the error says why. Each leaves nothing in the bare repo's objects
// dir either, while the one that lost the race keeps its pack, which the
// other fetch's ref may need.
func TestFetchBundleRefuses(t *testing.T) {
	t.Parallel()
	rem, r := synced(t)
	ctx := context.Background()
	first := run(t, rem.work, "rev-parse", "HEAD")
	start := rem.commit("main.txt", "main\n") // main's tip, the run's start
	if err := r.Sync(ctx, fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}

	notBundle := filepath.Join(t.TempDir(), "run.bundle")
	if err := os.WriteFile(notBundle, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// The start itself, with no commits past it.
	atStart := rem.bundle("HEAD")

	// A commit off main's first commit, so not after the start.
	run(t, rem.work, "checkout", "--quiet", "--detach", first)
	rem.commitOnly("side.txt", "side\n")
	sideways := rem.bundle(first + "..HEAD")

	// A commit past one the bare repo lacks.
	run(t, rem.work, "checkout", "--quiet", "--detach", start)
	unpushed := rem.commitOnly("a.txt", "a\n")
	rem.commitOnly("b.txt", "b\n")
	noPrereq := rem.bundle(unpushed + "..HEAD")

	// Commits past the start, under a branch but no HEAD.
	run(t, rem.work, "checkout", "--quiet", "-b", "feat", start)
	rem.commitOnly("feat.txt", "feat\n")
	noHead := rem.bundle(start + "..feat")

	// renameRef rewrites the bundle at path so its ref from names to
	// instead, which git bundle verify doesn't check.
	renameRef := func(path, from, to string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		oid := run(t, rem.work, "rev-parse", from)
		renamed := bytes.Replace(data, []byte(oid+" "+from+"\n"), []byte(oid+" "+to+"\n"), 1)
		if bytes.Equal(renamed, data) {
			t.Fatalf("the bundle %s has no %s %s line to rename", path, oid, from)
		}
		if err := os.WriteFile(path, renamed, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Two HEADs: a bundle of HEAD and the branch, with the branch's ref
	// renamed HEAD.
	twoHeads := rem.bundle(start+"..HEAD", "refs/heads/feat")
	renameRef(twoHeads, "refs/heads/feat", "HEAD")

	// A HEAD that names an annotated tag, not a commit: a bundle of the
	// tag, with its ref renamed HEAD.
	run(t, rem.work, "tag", "--annotate", "-m", "t1", "t1")
	tagged := rem.bundle(start + "..refs/tags/t1")
	renameRef(tagged, "refs/tags/t1", "HEAD")

	// A branch named HEAD, refs/heads/HEAD, and no HEAD: a bundle of the
	// branch, with its ref renamed so.
	branchHEAD := rem.bundle(start + "..feat")
	renameRef(branchHEAD, "refs/heads/feat", "refs/heads/HEAD")

	// A commit past the start whose pack lacks the blob its tree names: a
	// bundle written by hand around a pack of only the commit and its tree.
	run(t, rem.work, "checkout", "--quiet", "--detach", start)
	blobless := rem.commitOnly("blob.txt", "blob\n")
	blob := run(t, rem.work, "rev-parse", blobless+":blob.txt")
	pack := exec.Command("git", "pack-objects", "--quiet", "--stdout")
	pack.Dir, pack.Env = rem.work, gitEnv(os.Environ(), "", "")
	pack.Stdin = strings.NewReader(blobless + "\n" + run(t, rem.work, "rev-parse", blobless+"^{tree}") + "\n")
	packData, err := pack.Output()
	if err != nil {
		t.Fatalf("pack-objects: %v", err)
	}
	noBlob := filepath.Join(t.TempDir(), "run.bundle")
	header := "# v2 git bundle\n-" + start + " main\n" + blobless + " HEAD\n\n"
	if err := os.WriteFile(noBlob, append([]byte(header), packData...), 0o644); err != nil {
		t.Fatal(err)
	}

	// A commit past the start that passes every check, for a fetch that
	// another fetch of the run beats to the run's ref.
	run(t, rem.work, "checkout", "--quiet", "--detach", start)
	rem.commitOnly("raced.txt", "raced\n")
	raced := rem.bundle(start + "..HEAD")

	// A commit past the start whose author line fsck rejects.
	tree := run(t, rem.work, "rev-parse", start+"^{tree}")
	body := "tree " + tree + "\nparent " + start + "\nauthor bad <x> notadate\ncommitter t <t@t> 1 +0000\n\nbad\n"
	cmd := exec.Command("git", "hash-object", "-t", "commit", "-w", "--literally", "--stdin")
	cmd.Dir, cmd.Env, cmd.Stdin = rem.work, gitEnv(os.Environ(), "", ""), strings.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	run(t, rem.work, "update-ref", "--no-deref", "HEAD", strings.TrimSpace(string(out)))
	broken := rem.bundle(start + "..HEAD")

	tests := []struct {
		name, path, runID, start string
		want                     string // in the error
		raced                    bool   // another fetch writes the run's ref, at the start, just before this one does
	}{
		{name: "not a bundle", path: notBundle, runID: "r", start: start, want: "does not look like a v2 or v3 bundle"},
		{name: "missing file", path: filepath.Join(t.TempDir(), "none"), runID: "r", start: start, want: "verify bundle"},
		{name: "missing prerequisite", path: noPrereq, runID: "r", start: start, want: "lacks these prerequisite commits"},
		{name: "head is the start", path: atStart, runID: "r", start: start, want: "its HEAD is the start " + start},
		{name: "not after the start", path: sideways, runID: "r", start: start, want: "doesn't have the start " + start + " in its history"},
		{name: "start not in the repo", path: sideways, runID: "r", start: strings.Repeat("a", 40), want: "descends from"},
		{name: "no HEAD", path: noHead, runID: "r", start: start, want: "want one HEAD"},
		{name: "two HEADs", path: twoHeads, runID: "r", start: start, want: "want one HEAD"},
		{name: "HEAD a tag", path: tagged, runID: "r", start: start, want: "isn't a commit"},
		{name: "fsck", path: broken, runID: "r", start: start, want: "badDate"},
		{name: "branch named HEAD", path: branchHEAD, runID: "r", start: start, want: "want one HEAD"},
		{name: "missing blob", path: noBlob, runID: "r", start: start, want: "did not receive expected object " + blob},
		{name: "run ref written since the read", path: raced, runID: "r", start: start, want: "reference already exists", raced: true},
		{name: "empty run ID", path: atStart, runID: "", start: start, want: "invalid run ID"},
		{name: "run ID path", path: atStart, runID: "a/b", start: start, want: "invalid run ID"},
		{name: "run ID dot", path: atStart, runID: ".r", start: start, want: "invalid run ID"},
		{name: "run ID revision", path: atStart, runID: "r~1", start: start, want: "invalid run ID"},
		{name: "run ID glob", path: atStart, runID: "r*", start: start, want: "invalid run ID"},
		{name: "run ID lock", path: atStart, runID: "r.lock", start: start, want: "invalid run ID"},
		{name: "start a ref", path: atStart, runID: "r", start: "main", want: "invalid start SHA"},
		{name: "start short", path: atStart, runID: "r", start: start[:12], want: "invalid start SHA"},
		{name: "start uppercase", path: atStart, runID: "r", start: strings.ToUpper(start), want: "invalid start SHA"},
	}
	want := refs(t, r)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantRefs, runRef := want, RunsRef+tt.runID
			if tt.raced {
				wantRefs += "\n" + start + " " + runRef
				r.beforeRunRef = func() { run(t, r.dir, "update-ref", runRef, start) }
				defer func() {
					r.beforeRunRef = nil
					run(t, r.dir, "update-ref", "-d", runRef)
				}()
			}
			before := objectFiles(t, r)
			head, err := r.FetchBundle(ctx, tt.path, tt.runID, tt.start)
			if err == nil {
				t.Fatalf("FetchBundle = %q, want an error", head)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("FetchBundle err = %v, want it to contain %q", err, tt.want)
			}
			if got := refs(t, r); got != wantRefs {
				t.Errorf("refs after a refused fetch =\n%s\nwant\n%s", got, wantRefs)
			}
			after := objectFiles(t, r)
			if !tt.raced {
				if !slices.Equal(after, before) {
					t.Errorf("a refused fetch left objects behind: %q, then %q", before, after)
				}
				return
			}
			// The other fetch's ref may need the pack this one moved in,
			// so it stays, and nothing else does.
			var added []string
			for _, f := range after {
				if !slices.Contains(before, f) {
					added = append(added, f)
				}
			}
			for _, f := range added {
				if filepath.Dir(f) != "pack" || !strings.HasPrefix(filepath.Base(f), "pack-") {
					t.Errorf("a fetch that lost the race to the run's ref left %s, which isn't a pack", f)
				}
			}
			if len(added) == 0 {
				t.Errorf("a fetch that lost the race to the run's ref kept no pack, so the test shows nothing: %q", after)
			}
		})
	}
}

// objectFiles lists the entries under the bare repo's objects dir, dirs
// and files, relative to it, leaving out its info and pack dirs
// themselves.
func objectFiles(t *testing.T, r *Repo) []string {
	t.Helper()
	objects := filepath.Join(r.dir, "objects")
	var entries []string
	err := filepath.WalkDir(objects, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(objects, path)
		if err != nil {
			return err
		}
		if rel != "." && rel != "info" && rel != "pack" {
			entries = append(entries, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// TestOwnRefs reads, moves and deletes a task's head under refs/issuedog/.
// ReadRef matches the name exactly: a branch synced from GitHub whose name
// is the ref's, and a ref below it, both read as missing. A missing ref
// deletes fine.
func TestOwnRefs(t *testing.T) {
	t.Parallel()
	rem, r := synced(t)
	ctx := context.Background()
	first := run(t, rem.work, "rev-parse", "HEAD")
	second := rem.commit("b.txt", "b\n")
	ref := TasksRef + "7"
	run(t, rem.work, "push", "--quiet", rem.url, "main:refs/heads/"+ref)
	if err := r.Sync(ctx, fakeToken); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	run(t, r.dir, "update-ref", ref+"0", second)
	run(t, r.dir, "update-ref", TasksRef+"8/below", second)

	readRef := func(ref, want string) {
		t.Helper()
		got, ok, err := r.ReadRef(ctx, ref)
		if err != nil || got != want || ok != (want != "") {
			t.Errorf("ReadRef(%s) = %q, %v, %v; want %q, %v", ref, got, ok, err, want, want != "")
		}
	}
	readRef(ref, "")
	readRef(TasksRef+"8", "")
	if err := r.UpdateRef(ctx, ref, first); err != nil {
		t.Fatalf("UpdateRef: %v", err)
	}
	readRef(ref, first)
	if err := r.UpdateRef(ctx, ref, second); err != nil {
		t.Fatalf("UpdateRef again: %v", err)
	}
	readRef(ref, second)
	if got := run(t, r.dir, "rev-parse", "refs/heads/"+ref); got != second {
		t.Errorf("the synced branch named like the ref = %s, want it left at %s", got, second)
	}
	for range 2 {
		if err := r.DeleteRef(ctx, ref); err != nil {
			t.Errorf("DeleteRef: %v", err)
		}
		readRef(ref, "")
	}
	readRef(ref+"0", second)
}

// TestOwnRefsRefuse refuses a ref outside refs/issuedog/, one that isn't a
// valid ref name, and an update to what isn't a full SHA, or names an
// object the bare repo lacks. Nothing is written.
func TestOwnRefsRefuse(t *testing.T) {
	t.Parallel()
	rem, r := synced(t)
	ctx := context.Background()
	main := run(t, rem.work, "rev-parse", "HEAD")
	want := refs(t, r)
	for _, ref := range []string{"refs/heads/main", "main", "HEAD", "refs/issuedogs/x", "refs/issuedog/",
		"refs/issuedog/a..b", "refs/issuedog/x.lock", "refs/issuedog/x~1"} {
		if got, ok, err := r.ReadRef(ctx, ref); err == nil {
			t.Errorf("ReadRef(%s) = %q, %v; want an error", ref, got, ok)
		}
		if err := r.UpdateRef(ctx, ref, main); err == nil {
			t.Errorf("UpdateRef(%s) gave no error", ref)
		}
		if err := r.DeleteRef(ctx, ref); err == nil {
			t.Errorf("DeleteRef(%s) gave no error", ref)
		}
	}
	for _, sha := range []string{"main", main[:12], strings.ToUpper(main), strings.Repeat("0", 39) + "1"} {
		if err := r.UpdateRef(ctx, TasksRef+"1", sha); err == nil {
			t.Errorf("UpdateRef to %q gave no error", sha)
		}
	}
	if got := refs(t, r); got != want {
		t.Errorf("refs after the refusals =\n%s\nwant\n%s", got, want)
	}
}
