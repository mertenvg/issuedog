package workspace

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/mertenvg/issuedog/internal/repo"
)

// tryGit runs a test-setup git command in dir and returns its trimmed
// output and its error.
func tryGit(dir string, env []string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"), env...)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// git runs a test-setup git command in dir and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := tryGit(dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

// repoSHAs are the commits of newRepo's bare repo.
type repoSHAs struct {
	first, second string // main's two commits, to f.txt: "one", then "two"
	other         string // on branch other, after second: f.txt "three"
	run           string // only under refs/issuedog/runs/x, after second
	tag           string // the annotated tag v1's object, of second
}

// newWorkspace makes newRepo's bare repo, of SHA-1 objects, and a workspace on it.
func newWorkspace(t *testing.T) (*Workspace, repoSHAs) { return newWorkspaceFormat(t, "sha1") }

// newWorkspaceFormat makes a bare repo of the object format: main with two
// commits to f.txt, a branch other with a third, a commit only a
// refs/issuedog/* ref holds, and an annotated tag of main's tip. So main's
// first commit is the one no ref advertises.
// It returns a workspace on the repo, and the repo's SHAs.
func newWorkspaceFormat(t *testing.T, format string) (*Workspace, repoSHAs) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	git(t, root, "init", "--quiet", "--initial-branch=main", "--object-format="+format, src)
	commit := func(text string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "add", "f.txt")
		git(t, src, "commit", "--quiet", "-m", text)
		return git(t, src, "rev-parse", "HEAD")
	}
	var s repoSHAs
	s.first = commit("one")
	s.second = commit("two")
	git(t, src, "tag", "--annotate", "-m", "v1", "v1", s.second)
	s.tag = git(t, src, "rev-parse", "v1")
	git(t, src, "checkout", "--quiet", "-b", "other")
	s.other = commit("three")
	git(t, src, "checkout", "--quiet", "--detach", s.second)
	s.run = commit("run")
	git(t, src, "update-ref", "refs/issuedog/runs/x", s.run)
	git(t, src, "checkout", "--quiet", "main")
	bare := filepath.Join(root, "repo.git")
	git(t, root, "clone", "--quiet", "--bare", src, bare)
	git(t, src, "push", "--quiet", bare, "refs/issuedog/runs/x:refs/issuedog/runs/x")
	w := &Workspace{Bare: bare, RunsDir: filepath.Join(root, "runs"), ContextDir: filepath.Join(root, "ctx")}
	for _, d := range []string{w.RunsDir, w.ContextDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w, s
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TestPrepare checks out main's tip and an older SHA under umask 077, and
// fetches from a repo git treats as someone else's, as agent's clone of the
// daemon's repo is. HEAD is always the start, detached.
func TestPrepare(t *testing.T) {
	w, s := newWorkspace(t)
	defer syscall.Umask(syscall.Umask(0o077))
	for _, tc := range []struct {
		name, sha, want string
		env             []string
	}{
		{"main", s.second, "two", nil},
		{"older-sha", s.first, "one", nil},
		// Git then takes every repo as someone else's, the clone too, which
		// agent owns: the env's config makes the clone safe, and upload-pack,
		// which a local fetch gives none of it, reads gitConfig's safe.directory.
		{"other-owner", s.second, "two", []string{"GIT_TEST_ASSUME_DIFFERENT_OWNER=1",
			"GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=safe.directory", "GIT_CONFIG_VALUE_0=" + w.Dir("other-owner")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.testEnv = tc.env
			files := map[string][]byte{"prompt.md": []byte("p"), "system.md": []byte("s")}
			if err := w.Prepare(context.Background(), tc.name, Commits{Start: tc.sha}, files); err != nil {
				t.Fatal(err)
			}
			dir := w.Dir(tc.name)
			if got, _ := os.ReadFile(filepath.Join(dir, "f.txt")); string(got) != tc.want {
				t.Errorf("f.txt = %q, want %q", got, tc.want)
			}
			if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "HEAD" {
				t.Errorf("HEAD = %q, want it detached", got)
			}
			if got := git(t, dir, "rev-parse", "HEAD"); got != tc.sha {
				t.Errorf("HEAD at %s, want %s", got, tc.sha)
			}
			if m := mode(t, w.gitConfig()); m != 0o644 {
				t.Errorf("git config mode %o, want 644", m)
			}
			if m := mode(t, w.Context(tc.name)); m != 0o755 {
				t.Errorf("context dir mode %o, want 755", m)
			}
			for name, data := range files {
				path := filepath.Join(w.Context(tc.name), name)
				if m := mode(t, path); m != 0o644 {
					t.Errorf("%s mode %o, want 644", name, m)
				}
				if got, _ := os.ReadFile(path); string(got) != string(data) {
					t.Errorf("%s = %q, want %q", name, got, data)
				}
			}
		})
	}
}

// TestPrepareHoldsOnlyNamedCommits checks the clone of a start, with and
// without a target: it has no remote, and holds the named commits and their
// whole history, the target at TargetRef, and nothing else, as its own
// copies (SPEC §3.2).
func TestPrepareHoldsOnlyNamedCommits(t *testing.T) {
	t.Parallel()
	for _, format := range []string{"sha1", "sha256"} {
		w, s := newWorkspaceFormat(t, format)
		for _, tc := range []struct {
			name    string
			c       Commits
			missing []string // commits and objects the clone mustn't hold
		}{
			{"start", Commits{Start: s.second}, []string{s.other, s.run, s.tag}},
			{"target", Commits{Start: s.second, Target: s.other}, []string{s.run, s.tag}},
		} {
			t.Run(format+"/"+tc.name, func(t *testing.T) {
				runID := format + "-" + tc.name
				if err := w.Prepare(context.Background(), runID, tc.c, nil); err != nil {
					t.Fatal(err)
				}
				checkNamedClone(t, w, w.Dir(runID), tc.c, tc.missing)
			})
		}
	}
}

// checkNamedClone checks that dir is a clone of exactly c's commits from
// w's bare repo, and lacks each of missing.
func checkNamedClone(t *testing.T, w *Workspace, dir string, c Commits, missing []string) {
	t.Helper()
	if got := git(t, dir, "rev-parse", "HEAD"); got != c.Start {
		t.Errorf("HEAD at %s, want the start %s", got, c.Start)
	}
	if c.Target != "" {
		if got := git(t, dir, "rev-parse", TargetRef); got != c.Target {
			t.Errorf("%s at %s, want the target %s", TargetRef, got, c.Target)
		}
	}
	if got := git(t, dir, "remote"); got != "" {
		t.Errorf("git remote lists %q, want none", got)
	}
	if got, err := tryGit(dir, nil, "config", "--get-regexp", `^remote\.`); err == nil || got != "" {
		t.Errorf("the clone's config has remote keys: %q, %v", got, err)
	}
	refs := strings.Fields(git(t, dir, "for-each-ref", "--format=%(refname)"))
	want := []string{}
	if c.Target != "" {
		want = append(want, TargetRef)
	}
	if !slices.Equal(refs, want) {
		t.Errorf("the clone's refs are %q, want %q: no refs/remotes, tags or refs/issuedog", refs, want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "FETCH_HEAD")); err == nil {
		t.Error("the clone has a FETCH_HEAD, naming the bare repo")
	}
	named := []string{c.Start}
	if c.Target != "" {
		named = append(named, c.Target)
	}
	wantList := strings.Fields(git(t, w.Bare, append([]string{"rev-list"}, named...)...))
	gotList := strings.Fields(git(t, dir, "rev-list", "--all"))
	slices.Sort(wantList)
	slices.Sort(gotList)
	if !slices.Equal(gotList, wantList) {
		t.Errorf("rev-list --all in the clone = %q, want the named history %q", gotList, wantList)
	}
	for _, sha := range missing {
		if _, err := tryGit(dir, nil, "cat-file", "-e", sha); err == nil {
			t.Errorf("the clone holds %s, which the daemon didn't name", sha)
		}
	}
	if got := git(t, dir, "rev-parse", "--is-shallow-repository"); got != "false" {
		t.Errorf("is-shallow-repository = %q, want false", got)
	}
	objects := 0
	err := filepath.WalkDir(filepath.Join(dir, ".git", "objects"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		objects++
		st, ok := fi.Sys().(*syscall.Stat_t)
		switch {
		case !ok:
			t.Errorf("%s: no link count in %T", path, fi.Sys())
		case st.Nlink != 1:
			t.Errorf("%s has %d links, want its own copy", path, st.Nlink)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if objects == 0 {
		t.Error("the clone has no object files")
	}
}

// TestPrepareFetchesUnadvertisedSHA fetches a start that's no ref's tip in
// the bare repo, nor a tag's commit, so its upload-pack doesn't advertise
// it. Git's default, protocol v2, allows that, so a run can start from any
// commit the daemon names. Under protocol v0 the same fetch fails, which
// shows the SHA is unadvertised.
func TestPrepareFetchesUnadvertisedSHA(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	if tips := git(t, w.Bare, "for-each-ref", "--format=%(objectname) %(*objectname)"); strings.Contains(tips, s.first) {
		t.Fatalf("a ref of the bare repo names %s: %s", s.first, tips)
	}
	c := Commits{Start: s.first}
	if err := w.Prepare(context.Background(), "v2", c, nil); err != nil {
		t.Fatalf("protocol v2 fetch of %s: %v", c.Start, err)
	}
	checkNamedClone(t, w, w.Dir("v2"), c, []string{s.other, s.run, s.tag})
	w.testEnv = []string{"GIT_CONFIG_PARAMETERS='protocol.version'='0'"}
	if err := w.Prepare(context.Background(), "v0", Commits{Start: s.first}, nil); err == nil {
		t.Errorf("protocol v0 fetched %s, which no ref advertises; the test shows nothing", s.first)
	}
}

// TestCleanup removes both dirs, even a read-only one, and succeeds twice.
func TestCleanup(t *testing.T) {
	w, s := newWorkspace(t)
	ctx := context.Background()
	if err := w.Prepare(ctx, "r1", Commits{Start: s.second}, map[string][]byte{"prompt.md": nil}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(w.Dir("r1"), ".git"), 0o500); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := w.Cleanup(ctx, "r1"); err != nil {
			t.Fatalf("cleanup %d: %v", i+1, err)
		}
	}
	for _, d := range []string{w.Dir("r1"), w.Context("r1")} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s still exists: %v", d, err)
		}
	}
}

// TestPrepareHome makes an empty 0700 HOME with a 0700 tmp dir in it,
// whatever the umask, which Cleanup removes.
func TestPrepareHome(t *testing.T) {
	w, _ := newWorkspace(t)
	defer syscall.Umask(syscall.Umask(0o022))
	ctx := context.Background()
	if err := w.PrepareHome(ctx, "v1-home"); err != nil {
		t.Fatal(err)
	}
	home := w.Dir("v1-home")
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "tmp" || !entries[0].IsDir() {
		t.Errorf("the home holds %v, want only a tmp dir", entries)
	}
	for _, d := range []string{home, filepath.Join(home, "tmp")} {
		if m := mode(t, d); m != 0o700 {
			t.Errorf("%s mode %o, want 700", d, m)
		}
	}
	if err := w.Cleanup(ctx, "v1-home"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Errorf("%s still exists: %v", home, err)
	}
}

// TestPrepareHomeRejects refuses a bad name, and a home that's there
// already, which it leaves as it was, so nothing a run left there reaches a
// verify.
func TestPrepareHomeRejects(t *testing.T) {
	t.Parallel()
	w, _ := newWorkspace(t)
	ctx := context.Background()
	for _, id := range []string{"", ".hidden", "a/b"} {
		if err := w.PrepareHome(ctx, id); err == nil || !strings.Contains(err.Error(), "invalid run ID") {
			t.Errorf("PrepareHome(%q) = %v, want an invalid run ID", id, err)
		}
	}
	old := w.Dir("old-home")
	if err := os.Mkdir(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "cache"), []byte("planted"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.PrepareHome(ctx, "old-home"); err == nil {
		t.Error("PrepareHome of an existing home = nil, want an error")
	}
	if got, err := os.ReadFile(filepath.Join(old, "cache")); err != nil || string(got) != "planted" {
		t.Errorf("the existing home's file = %q, %v; want it left alone", got, err)
	}
	if _, err := os.Stat(filepath.Join(old, "tmp")); !os.IsNotExist(err) {
		t.Errorf("PrepareHome made a tmp dir in the existing home: %v", err)
	}
}

// TestPrepareHomeTmpFails removes the home it made when making the tmp dir
// in it fails, so no half-made home is left. A mkdir on the PATH fails for
// the tmp dir alone and runs the real mkdir otherwise. It sets PATH, so it
// can't run in parallel.
func TestPrepareHomeTmpFails(t *testing.T) {
	w, _ := newWorkspace(t)
	ctx := context.Background()
	real, err := exec.LookPath("mkdir")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in */tmp) echo 'mkdir: no tmp' >&2; exit 1;; esac; done\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "mkdir"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	err = w.PrepareHome(ctx, "v1-home")
	if err == nil || !strings.Contains(err.Error(), "prepare home v1-home") || !strings.Contains(err.Error(), "no tmp") {
		t.Fatalf("PrepareHome = %v, want the tmp dir's failure", err)
	}
	if _, err := os.Lstat(w.Dir("v1-home")); !os.IsNotExist(err) {
		t.Errorf("the home is left after its tmp dir failed: %v", err)
	}
}

// TestPrepareRejects refuses bad names, and a start or target SHA that's
// missing, not a full SHA, or not in the bare repo, leaving nothing, even
// after a failed fetch.
func TestPrepareRejects(t *testing.T) {
	w, s := newWorkspace(t)
	missing := strings.Repeat("a", 40)
	for _, tc := range []struct{ runID, start, target, file string }{
		{"", s.second, "", "p.md"}, {".", s.second, "", "p.md"}, {"..", s.second, "", "p.md"}, {".r", s.second, "", "p.md"},
		{"a/b", s.second, "", "p.md"}, {"/", s.second, "", "p.md"}, {"../r", s.second, "", "p.md"},
		{"r", s.second, "", ""}, {"r", s.second, "", ".p.md"}, {"r", s.second, "", "a/p.md"}, {"r", s.second, "", "../p.md"},
		{"r", "", "", "p.md"}, {"r", "main", "", "p.md"}, {"r", "--help", "", "p.md"}, {"r", s.second[:12], "", "p.md"},
		{"r", missing, "", "p.md"}, {"r", strings.ToUpper(s.second), "", "p.md"},
		{"r", s.second, "main", "p.md"}, {"r", s.second, "--help", "p.md"}, {"r", s.second, s.other[:12], "p.md"},
		{"r", s.second, missing, "p.md"},
	} {
		c := Commits{Start: tc.start, Target: tc.target}
		err := w.Prepare(context.Background(), tc.runID, c, map[string][]byte{tc.file: nil})
		if err == nil {
			t.Errorf("Prepare(%q, %+v, %q) succeeded", tc.runID, c, tc.file)
		}
		_ = os.Remove(w.gitConfig()) // shared by every run, so it rightly stays
		for _, d := range []string{w.RunsDir, w.ContextDir} {
			if entries, _ := os.ReadDir(d); len(entries) != 0 {
				t.Fatalf("Prepare(%q, %+v, %q) left %v in %s", tc.runID, c, tc.file, entries, d)
			}
		}
	}
	if err := w.Cleanup(context.Background(), "../r"); err == nil {
		t.Error("Cleanup(../r) succeeded")
	}
}

// TestPrepareRefusesExistingCheckout fails on a run whose checkout dir is
// there already, and leaves that dir as it was.
func TestPrepareRefusesExistingCheckout(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	left := filepath.Join(w.Dir("r1"), "left.txt")
	if err := os.MkdirAll(filepath.Dir(left), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(left, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Prepare(context.Background(), "r1", Commits{Start: s.second}, nil); err == nil {
		t.Error("Prepare succeeded into an existing checkout")
	}
	if got, err := os.ReadFile(left); err != nil || string(got) != "x" {
		t.Errorf("the existing checkout's file reads %q, %v; want it untouched", got, err)
	}
	if _, err := os.Stat(w.Context("r1")); !os.IsNotExist(err) {
		t.Errorf("the context dir is left: %v", err)
	}
}

// TestPrepareRunsNoHooks sets every way the daemon's environment could give
// the clone a post-checkout hook, and checks that none ran.
func TestPrepareRunsNoHooks(t *testing.T) {
	w, s := newWorkspace(t)
	root := t.TempDir()
	hooks, marker := filepath.Join(root, "tmpl", "hooks"), filepath.Join(root, "ran")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\ntouch " + marker + "\n"
	if err := os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(global, []byte("[core]\n\thooksPath = "+hooks+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_TEMPLATE_DIR", filepath.Dir(hooks))
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.hooksPath'='"+hooks+"'")
	if err := w.Prepare(context.Background(), "r1", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a hook ran during the clone: %v", err)
	}
}

// commitIn writes name with content in the clone at dir and commits every
// change there, as a run would, and returns the new HEAD.
func commitIn(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "--all")
	git(t, dir, "commit", "--quiet", "-m", "change "+name)
	return git(t, dir, "rev-parse", "HEAD")
}

// bundleFile runs Bundle for the run into a new 0600 file in a dir of the
// test's own, as the daemon's dir would be, and returns the file's path and
// Bundle's answer.
func bundleFile(t *testing.T, w *Workspace, runID, start string) (string, bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), runID+".bundle")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := w.Bundle(context.Background(), runID, start, f)
	if cerr := f.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil {
		t.Fatalf("Bundle(%s, %s): %v", runID, start, err)
	}
	return path, ok
}

// TestBundle writes nothing for a clone with no commits past its start, or
// one whose HEAD went back before it, and a bundle of start..HEAD for one
// with commits, with the start as its one prerequisite and HEAD as its one
// ref.
func TestBundle(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	ctx := context.Background()
	if err := w.Prepare(ctx, "r1", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	dir := w.Dir("r1")
	var none bytes.Buffer
	if ok, err := w.Bundle(ctx, "r1", s.second, &none); err != nil || ok || none.Len() != 0 {
		t.Errorf("Bundle with no commits = %v, %v, and %d bytes; want false, no error and none", ok, err, none.Len())
	}

	commitIn(t, dir, "a.txt", "a")
	head := commitIn(t, dir, "b.txt", "b")
	var b bytes.Buffer
	ok, err := w.Bundle(ctx, "r1", s.second, &b)
	if err != nil || !ok {
		t.Fatalf("Bundle with two commits = %v, %v; want true", ok, err)
	}
	header := "# v2 git bundle\n-" + s.second + " two\n" + head + " HEAD\n\n"
	if got := b.String(); !strings.HasPrefix(got, header) {
		t.Errorf("the bundle starts %q, want %q", got[:min(len(got), len(header))], header)
	}

	git(t, dir, "checkout", "--quiet", "--detach", s.first)
	none.Reset()
	if ok, err := w.Bundle(ctx, "r1", s.second, &none); err != nil || ok || none.Len() != 0 {
		t.Errorf("Bundle of a HEAD before the start = %v, %v, and %d bytes; want false, no error and none", ok, err, none.Len())
	}
}

// TestBundleRejects refuses a bad run ID or start, and a run with no
// clone, and writes nothing.
func TestBundleRejects(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	if err := w.Prepare(context.Background(), "r1", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	commitIn(t, w.Dir("r1"), "a.txt", "a")
	for _, tc := range []struct{ runID, start string }{
		{"", s.second}, {".r1", s.second}, {"../r1", s.second}, {"./r1", s.second}, {"a/b", s.second},
		{"r1", ""}, {"r1", "main"}, {"r1", "HEAD~1"}, {"r1", s.second[:12]}, {"r1", strings.ToUpper(s.second)},
		{"r2", s.second},
	} {
		var b bytes.Buffer
		if ok, err := w.Bundle(context.Background(), tc.runID, tc.start, &b); err == nil || ok || b.Len() != 0 {
			t.Errorf("Bundle(%q, %q) = %v, %v, and %d bytes; want an error and nothing", tc.runID, tc.start, ok, err, b.Len())
		}
	}
}

// TestBundleHandoff hands a run's commits to the bare repo: Bundle as
// agent, then repo.FetchBundle as the daemon, into the run's ref. A run can
// then start from them. A run with no commits gives no bundle, and one
// whose HEAD doesn't descend from its start writes no ref.
func TestBundleHandoff(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	ctx := context.Background()
	bare := repo.New("", w.Bare)
	if err := w.Prepare(ctx, "r1", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	commitIn(t, w.Dir("r1"), "f.txt", "four")
	want := commitIn(t, w.Dir("r1"), "g.txt", "five")
	path, ok := bundleFile(t, w, "r1", s.second)
	if !ok {
		t.Fatal("Bundle of two commits = false")
	}
	head, err := bare.FetchBundle(ctx, path, "r1", s.second)
	if err != nil || head != want {
		t.Fatalf("FetchBundle = %q, %v; want the clone's HEAD %s", head, err, want)
	}
	if got := git(t, w.Bare, "rev-parse", repo.RunsRef+"r1"); got != want {
		t.Errorf("%sr1 = %s, want %s", repo.RunsRef, got, want)
	}
	if err := w.Prepare(ctx, "r2", Commits{Start: head}, nil); err != nil {
		t.Fatalf("Prepare from the fetched head: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(w.Dir("r2"), "f.txt")); err != nil || string(got) != "four" {
		t.Errorf("f.txt from the fetched head reads %q, %v; want four", got, err)
	}

	if err := w.Prepare(ctx, "r3", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	if path, ok := bundleFile(t, w, "r3", s.second); ok {
		t.Errorf("Bundle with no commits = true, wrote %s", path)
	}

	if err := w.Prepare(ctx, "r4", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	git(t, w.Dir("r4"), "checkout", "--quiet", "--orphan", "fresh")
	commitIn(t, w.Dir("r4"), "f.txt", "unrelated")
	path, ok = bundleFile(t, w, "r4", s.second)
	if !ok {
		t.Fatal("Bundle of an unrelated history = false")
	}
	if head, err := bare.FetchBundle(ctx, path, "r4", s.second); err == nil || !strings.Contains(err.Error(), "in its history") {
		t.Errorf("FetchBundle of an unrelated history = %q, %v; want an error that says the start isn't in its history", head, err)
	}
	if out, err := tryGit(w.Bare, nil, "rev-parse", "--verify", "--quiet", repo.RunsRef+"r4"); err == nil {
		t.Errorf("the refused run's ref was written: %s", out)
	}
}

// TestBundleReadsCommitsAsMade plants, in a run's clone with two commits
// past its start, a replace ref and a graft, each giving the start the
// clone's HEAD as its parent, so that start..HEAD looks empty. Git
// without Bundle's environment sees it so, so each plant takes effect.
// Bundle ignores it, and the fetched run's ref holds both commits.
func TestBundleReadsCommitsAsMade(t *testing.T) {
	t.Parallel()
	for name, plant := range map[string]func(t *testing.T, dir, start, head string){
		"replace ref": func(t *testing.T, dir, start, head string) {
			git(t, dir, "replace", "--graft", start, head)
		},
		"graft": func(t *testing.T, dir, start, head string) {
			grafts := filepath.Join(dir, ".git", "info", "grafts")
			if err := os.MkdirAll(filepath.Dir(grafts), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(grafts, []byte(start+" "+head+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, s := newWorkspace(t)
			ctx := context.Background()
			if err := w.Prepare(ctx, "r1", Commits{Start: s.second}, nil); err != nil {
				t.Fatal(err)
			}
			dir := w.Dir("r1")
			commitIn(t, dir, "a.txt", "a")
			head := commitIn(t, dir, "b.txt", "b")
			plant(t, dir, s.second, head)
			if got := git(t, dir, "-c", "advice.graftFileDeprecated=false", "rev-list", "--count", s.second+"..HEAD"); got != "0" {
				t.Fatalf("with the %s planted, git sees %s commits past the start, want 0, so the test shows nothing", name, got)
			}

			path, ok := bundleFile(t, w, "r1", s.second)
			if !ok {
				t.Fatalf("Bundle with a %s planted = false, want the run's two commits", name)
			}
			got, err := repo.New("", w.Bare).FetchBundle(ctx, path, "r1", s.second)
			if err != nil || got != head {
				t.Fatalf("FetchBundle = %q, %v; want the clone's HEAD %s", got, err, head)
			}
			if n := git(t, w.Bare, "rev-list", "--count", s.second+".."+repo.RunsRef+"r1"); n != "2" {
				t.Errorf("the run's ref has %s commits past the start, want 2", n)
			}
		})
	}
}

// hookNames are the hooks githooks(5) lists.
var hookNames = []string{
	"applypatch-msg", "pre-applypatch", "post-applypatch", "pre-commit", "pre-merge-commit",
	"prepare-commit-msg", "commit-msg", "post-commit", "pre-rebase", "post-checkout", "post-merge",
	"pre-push", "pre-receive", "update", "proc-receive", "post-receive", "post-update",
	"reference-transaction", "push-to-checkout", "pre-auto-gc", "post-rewrite", "sendemail-validate",
	"fsmonitor-watchman", "p4-changelist", "p4-prepare-changelist", "p4-post-changelist",
	"p4-pre-submit", "post-index-change",
}

// TestBundleHandoffRunsNothingFromClone plants, in a run's clone, every hook
// in .git/hooks and in a core.hooksPath, a core.fsmonitor, a
// core.sshCommand, and a filter and a diff driver that the run's own
// .gitattributes names for every file. It plants every hook in the bare
// repo's hooks dir too. Each writes a marker when it runs. None runs while
// the clone's commits are bundled and fetched (SPEC §3.2). Then git, invited
// to, runs each, so each plant would show if it ran.
func TestBundleHandoffRunsNothingFromClone(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	ctx := context.Background()
	if err := w.Prepare(ctx, "r1", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	dir := w.Dir("r1")
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("* filter=planted diff=planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	want := commitIn(t, dir, "f.txt", "changed")

	markers, scripts, hooksPath := t.TempDir(), t.TempDir(), filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hooksPath, 0o755); err != nil {
		t.Fatal(err)
	}
	// plant writes an executable at path that appends its name to the
	// marker file named marker, then, for a filter, passes its input on.
	plant := func(path, marker string, filter bool) {
		t.Helper()
		script := "#!/bin/sh\necho \"$0\" >>" + filepath.Join(markers, marker) + "\n"
		if filter {
			script += "exec cat\n"
		}
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, h := range hookNames {
		plant(filepath.Join(dir, ".git", "hooks", h), "hooks-dir", false)
		plant(filepath.Join(hooksPath, h), "hooks-path", false)
		plant(filepath.Join(w.Bare, "hooks", h), "bare-hooks", false)
	}
	for _, name := range []string{"fsmonitor", "ssh", "filter", "diff"} {
		plant(filepath.Join(scripts, name), name, name == "filter")
	}
	for _, kv := range [][2]string{
		{"core.hooksPath", hooksPath},
		{"core.fsmonitor", filepath.Join(scripts, "fsmonitor")},
		{"core.sshCommand", filepath.Join(scripts, "ssh")},
		{"filter.planted.clean", filepath.Join(scripts, "filter")},
		{"filter.planted.smudge", filepath.Join(scripts, "filter")},
		{"diff.planted.command", filepath.Join(scripts, "diff")},
	} {
		git(t, dir, "config", kv[0], kv[1])
	}

	path, ok := bundleFile(t, w, "r1", s.second)
	if !ok {
		t.Fatal("Bundle of the run's commit = false")
	}
	head, err := repo.New("", w.Bare).FetchBundle(ctx, path, "r1", s.second)
	if err != nil || head != want {
		t.Fatalf("FetchBundle = %q, %v; want %s", head, err, want)
	}
	if ran, err := os.ReadDir(markers); err != nil || len(ran) != 0 {
		t.Errorf("planted hooks or config ran during the handoff: %v, %v", ran, err)
	}

	// Invited: a status runs the fsmonitor, a diff of a changed file its
	// filter and diff driver, a commit the hooks path's hooks and, with
	// that pointed back at .git/hooks, the hooks dir's, and an ssh URL the
	// sshCommand. In the bare repo, a ref update runs its
	// reference-transaction hook.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("again"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"status"},
		{"diff"},
		{"commit", "--quiet", "--all", "-m", "again"},
		{"-c", "core.hooksPath=" + filepath.Join(dir, ".git", "hooks"), "commit", "--quiet", "--allow-empty", "-m", "empty"},
		{"ls-remote", "ssh://example.invalid/r.git"},
	} {
		if out, err := tryGit(dir, nil, args...); err != nil {
			t.Logf("git %v in the clone: %v: %s", args, err, out) // the ssh one fails, after running the command
		}
	}
	git(t, w.Bare, "update-ref", "refs/invited", head)
	for _, marker := range []string{"hooks-dir", "hooks-path", "bare-hooks", "fsmonitor", "ssh", "filter", "diff"} {
		if _, err := os.Stat(filepath.Join(markers, marker)); err != nil {
			t.Errorf("the %s plant didn't run when git was invited to run it, so the test shows nothing: %v", marker, err)
		}
	}
}

// gitCall is one call the recording git saw.
type gitCall struct {
	global, dir string // its GIT_CONFIG_GLOBAL and working dir
	args        []string
}

// TestBundleHandoffGitCalls puts a git on the PATH that records each call's
// GIT_CONFIG_GLOBAL, working dir and args, then runs the real git. It makes
// a run's clone, its bundle and the fetch. Every call is the agent's, told
// by the agent's GIT_CONFIG_GLOBAL, or the daemon's, with /dev/null. The
// agent's all run from /, through the workspace's agent runner, in the
// run's clone. The daemon's name only the bare repo, never the runs dir
// (SPEC §3.2).
func TestBundleHandoffGitCalls(t *testing.T) {
	w, s := newWorkspace(t)
	ctx := context.Background()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin, calls := t.TempDir(), filepath.Join(t.TempDir(), "calls")
	script := "#!/bin/sh\n{\n" +
		"\tprintf 'call\\t%s\\t%s\\n' \"$GIT_CONFIG_GLOBAL\" \"$(pwd)\"\n" +
		"\tfor a in \"$@\"; do printf 'arg\\t%s\\n' \"$a\"; done\n" +
		"} >>" + calls + "\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := w.Prepare(ctx, "r1", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	clone := w.Dir("r1")
	// The run's commit, by the real git, so the record holds only the
	// workspace's and the daemon's calls.
	if err := os.WriteFile(filepath.Join(clone, "f.txt"), []byte("four"), 0o644); err != nil {
		t.Fatal(err)
	}
	commit := exec.Command(real, "-C", clone, "commit", "--quiet", "--all", "-m", "four")
	commit.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit in the clone: %v: %s", err, out)
	}
	path, ok := bundleFile(t, w, "r1", s.second)
	if !ok {
		t.Fatal("Bundle of the run's commit = false")
	}
	if _, err := repo.New("", w.Bare).FetchBundle(ctx, path, "r1", s.second); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	var recorded []gitCall
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		kind, rest, _ := strings.Cut(line, "\t")
		switch {
		case kind == "call":
			global, dir, _ := strings.Cut(rest, "\t")
			recorded = append(recorded, gitCall{global: global, dir: dir})
		case kind == "arg" && len(recorded) > 0:
			recorded[len(recorded)-1].args = append(recorded[len(recorded)-1].args, rest)
		default:
			t.Fatalf("bad record line %q", line)
		}
	}
	var agentOps, daemonOps []string
	for _, c := range recorded {
		switch c.global {
		case w.gitConfig():
			if c.dir != "/" || !slices.Contains(c.args, clone) {
				t.Errorf("agent's git %q ran in %s; want it from /, in the run's clone %s", c.args, c.dir, clone)
			}
			if len(c.args) > 2 && c.args[0] == "-C" {
				agentOps = append(agentOps, c.args[2])
			} else if len(c.args) > 0 {
				agentOps = append(agentOps, c.args[0])
			}
		case "/dev/null":
			if len(c.args) < 2 || c.args[0] != "--git-dir="+w.Bare {
				t.Errorf("daemon's git %q doesn't start with --git-dir=%s", c.args, w.Bare)
				continue
			}
			for _, a := range append([]string{c.dir}, c.args...) {
				if strings.Contains(a, w.RunsDir) {
					t.Errorf("daemon's git %q, in %s, names the runs dir %s", c.args, c.dir, w.RunsDir)
				}
			}
			daemonOps = append(daemonOps, c.args[1])
		default:
			t.Errorf("git %q, in %s, ran with GIT_CONFIG_GLOBAL %q, neither the agent's nor the daemon's", c.args, c.dir, c.global)
		}
	}
	for _, op := range []string{"init", "fetch", "checkout", "rev-list", "bundle"} {
		if !slices.Contains(agentOps, op) {
			t.Errorf("the agent ran no git %s; its calls: %q", op, agentOps)
		}
	}
	for _, op := range []string{"bundle", "index-pack", "update-ref"} {
		if !slices.Contains(daemonOps, op) {
			t.Errorf("the daemon ran no git %s; its calls: %q", op, daemonOps)
		}
	}
}

// TestSweepRemovesLeftoverRuns removes every run's checkout and context dir
// but keep's, a read-only checkout and a run with only one of the two
// among them, and the temp git configs, keeping the git config itself and
// any other dot name.
func TestSweepRemovesLeftoverRuns(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	ctx := context.Background()
	for _, id := range []string{"live", "dead"} {
		if err := w.Prepare(ctx, id, Commits{Start: s.second}, map[string][]byte{"prompt.md": nil}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(w.Dir("dead"), ".git"), 0o500); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{w.Dir("clone-only"), w.Context("context-only"), filepath.Join(w.RunsDir, ".dot")} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".agent.gitconfig.123", ".agent.gitconfig.x9"} {
		if err := os.WriteFile(filepath.Join(w.ContextDir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Sweep(ctx, "live"); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for dir, want := range map[string][]string{
		w.RunsDir:    {".dot", "live"},
		w.ContextDir: {".agent.gitconfig", "live"},
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s holds %q after the sweep, want %q", dir, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(w.Context("live"), "prompt.md")); err != nil {
		t.Errorf("the kept run's context file is gone: %v", err)
	}
}

// TestSweepKeepsNothingAndSkipsMissingDirs keeps no run for an empty keep,
// and finds nothing to remove in dirs that don't exist.
func TestSweepKeepsNothingAndSkipsMissingDirs(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	ctx := context.Background()
	if err := w.Prepare(ctx, "dead", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.Sweep(ctx, ""); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for _, d := range []string{w.Dir("dead"), w.Context("dead")} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s is still there: %v", d, err)
		}
	}
	gone := &Workspace{RunsDir: filepath.Join(t.TempDir(), "runs"), ContextDir: filepath.Join(t.TempDir(), "ctx")}
	if err := gone.Sweep(ctx, ""); err != nil {
		t.Errorf("Sweep of missing dirs = %v, want nothing to remove", err)
	}
}

// TestSweepRemovesALeftoverVerify removes what a crash left of a verify:
// its checkout of the head, its empty context dir, and its HOME beside the
// checkout, with whatever its commands wrote there.
func TestSweepRemovesALeftoverVerify(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	ctx := context.Background()
	if err := w.Prepare(ctx, "v1", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	if err := w.PrepareHome(ctx, "v1-home"); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(w.Dir("v1-home"), "tmp", "cache")
	if err := os.WriteFile(cache, []byte("cached"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := w.Sweep(ctx, ""); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	for _, dir := range []string{w.RunsDir, w.ContextDir} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Name() != ".agent.gitconfig" {
				t.Errorf("%s still holds %s after the sweep", dir, e.Name())
			}
		}
	}
}

// TestSweepGoesOnPastAFailure reports a run it can't remove, naming it, and
// still removes the runs after it.
func TestSweepGoesOnPastAFailure(t *testing.T) {
	t.Parallel()
	w, s := newWorkspace(t)
	ctx := context.Background()
	stuck := w.Context("a-stuck")
	if err := os.MkdirAll(filepath.Join(stuck, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stuck, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(stuck, 0o755); err != nil {
			t.Error(err)
		}
	})
	if err := w.Prepare(ctx, "b-dead", Commits{Start: s.second}, nil); err != nil {
		t.Fatal(err)
	}
	err := w.Sweep(ctx, "")
	if err == nil || !strings.Contains(err.Error(), "clean up run a-stuck") {
		t.Errorf("Sweep = %v, want it to name run a-stuck", err)
	}
	if _, err := os.Stat(w.Dir("b-dead")); !os.IsNotExist(err) {
		t.Errorf("the run after the failure is still there: %v", err)
	}
}

// TestSweepUnlinksAPlantedSymlink removes a symlink planted as a run's
// checkout, and leaves the read-only dir it points to, with its mode and
// contents, as it was: chmod would follow it.
func TestSweepUnlinksAPlantedSymlink(t *testing.T) {
	t.Parallel()
	w, _ := newWorkspace(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(outside, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(outside, 0o755); err != nil {
			t.Error(err)
		}
	})
	if err := os.Symlink(outside, w.Dir("planted")); err != nil {
		t.Fatal(err)
	}
	if err := w.Sweep(ctx, ""); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Lstat(w.Dir("planted")); !os.IsNotExist(err) {
		t.Errorf("the planted symlink is still there: %v", err)
	}
	if got := mode(t, outside); got != 0o500 {
		t.Errorf("the dir the symlink points to has mode %o, want 500", got)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keep.txt" {
		t.Errorf("the dir the symlink points to holds %v, want only keep.txt", entries)
	}
	data, err := os.ReadFile(filepath.Join(outside, "keep.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "kept" {
		t.Errorf("keep.txt holds %q, want %q", data, "kept")
	}
}

// TestSweepReportsAnUnreadableDir reports a dir it can't list, for a reason
// other than its being missing, naming it, and still sweeps the other dir's
// temp git config.
func TestSweepReportsAnUnreadableDir(t *testing.T) {
	t.Parallel()
	w, _ := newWorkspace(t)
	tempConfig := filepath.Join(w.ContextDir, ".agent.gitconfig.7")
	if err := os.WriteFile(tempConfig, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(w.RunsDir); err != nil {
		t.Fatal(err)
	}
	// A file where the runs dir goes can't be listed as one.
	if err := os.WriteFile(w.RunsDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := w.Sweep(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "sweep "+w.RunsDir) {
		t.Errorf("Sweep = %v, want it to name %s", err, w.RunsDir)
	}
	if _, err := os.Stat(tempConfig); !os.IsNotExist(err) {
		t.Errorf("the temp git config past the unreadable dir is still there: %v", err)
	}
}

// TestSweepLeavesTempGitConfigNamesInTheRunsDir removes temp git configs
// only where writeGitConfig writes them, the context dir: a name like one
// in the runs dir is the agent's, and stays.
func TestSweepLeavesTempGitConfigNamesInTheRunsDir(t *testing.T) {
	t.Parallel()
	w, _ := newWorkspace(t)
	planted := filepath.Join(w.RunsDir, ".agent.gitconfig.123")
	if err := os.WriteFile(planted, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := w.Sweep(context.Background(), ""); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := os.Stat(planted); err != nil {
		t.Errorf("the runs dir's %s is gone: %v", filepath.Base(planted), err)
	}
}
