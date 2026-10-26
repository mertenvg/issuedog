package workspace

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
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
