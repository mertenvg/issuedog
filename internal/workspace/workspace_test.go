package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// git runs a test-setup git command in dir and returns its trimmed output.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newWorkspace makes a bare repo with two commits to f.txt; it returns the first's SHA.
func newWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	git(t, root, "init", "--quiet", "--initial-branch=main", src)
	var first string
	for _, text := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "add", "f.txt")
		git(t, src, "commit", "--quiet", "-m", text)
		if first == "" {
			first = git(t, src, "rev-parse", "HEAD")
		}
	}
	bare := filepath.Join(root, "repo.git")
	git(t, root, "clone", "--quiet", "--bare", src, bare)
	w := &Workspace{Bare: bare, RunsDir: filepath.Join(root, "runs"), ContextDir: filepath.Join(root, "ctx")}
	for _, d := range []string{w.RunsDir, w.ContextDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return w, first
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// TestPrepare checks out main and an older SHA under umask 077, and clones a repo
// git treats as someone else's, as agent's clone of the daemon's repo is.
func TestPrepare(t *testing.T) {
	w, first := newWorkspace(t)
	defer syscall.Umask(syscall.Umask(0o077))
	for _, tc := range []struct {
		name, sha, want, head string
		env                   []string
	}{
		{"main", "", "two", "main", nil},
		{"older-sha", first, "one", "HEAD", nil},
		{"other-owner", "", "two", "main", []string{"GIT_TEST_ASSUME_DIFFERENT_OWNER=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w.testEnv = tc.env
			files := map[string][]byte{"prompt.md": []byte("p"), "system.md": []byte("s")}
			if err := w.Prepare(context.Background(), tc.name, tc.sha, files); err != nil {
				t.Fatal(err)
			}
			dir := w.Dir(tc.name)
			if got, _ := os.ReadFile(filepath.Join(dir, "f.txt")); string(got) != tc.want {
				t.Errorf("f.txt = %q, want %q", got, tc.want)
			}
			if got := git(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != tc.head {
				t.Errorf("HEAD = %q, want %q", got, tc.head)
			}
			if tc.sha != "" {
				if got := git(t, dir, "rev-parse", "HEAD"); got != tc.sha {
					t.Errorf("HEAD at %s, want %s", got, tc.sha)
				}
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

// TestCleanup removes both dirs, even a read-only one, and succeeds twice.
func TestCleanup(t *testing.T) {
	w, _ := newWorkspace(t)
	ctx := context.Background()
	if err := w.Prepare(ctx, "r1", "", map[string][]byte{"prompt.md": nil}); err != nil {
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

// TestPrepareRejects refuses bad names and SHAs, leaving nothing, even after a failed detach.
func TestPrepareRejects(t *testing.T) {
	w, _ := newWorkspace(t)
	missing := strings.Repeat("a", 40)
	for _, tc := range []struct{ runID, sha, file string }{
		{"", "", "p.md"}, {".", "", "p.md"}, {"..", "", "p.md"}, {".r", "", "p.md"},
		{"a/b", "", "p.md"}, {"/", "", "p.md"}, {"../r", "", "p.md"},
		{"r", "", ""}, {"r", "", ".p.md"}, {"r", "", "a/p.md"}, {"r", "", "../p.md"},
		{"r", "main", "p.md"}, {"r", "--help", "p.md"}, {"r", missing, "p.md"},
	} {
		err := w.Prepare(context.Background(), tc.runID, tc.sha, map[string][]byte{tc.file: nil})
		if err == nil {
			t.Errorf("Prepare(%q, %q, %q) succeeded", tc.runID, tc.sha, tc.file)
		}
		_ = os.Remove(w.gitConfig()) // shared by every run, so it rightly stays
		for _, d := range []string{w.RunsDir, w.ContextDir} {
			if entries, _ := os.ReadDir(d); len(entries) != 0 {
				t.Fatalf("Prepare(%q, %q, %q) left %v in %s", tc.runID, tc.sha, tc.file, entries, d)
			}
		}
	}
	if err := w.Cleanup(context.Background(), "../r"); err == nil {
		t.Error("Cleanup(../r) succeeded")
	}
}

// TestPrepareRunsNoHooks sets every way the daemon's environment could give
// the clone a post-checkout hook, and checks that none ran.
func TestPrepareRunsNoHooks(t *testing.T) {
	w, _ := newWorkspace(t)
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
	if err := w.Prepare(context.Background(), "r1", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a hook ran during the clone: %v", err)
	}
}

// TestSweepRemovesLeftoverRuns removes every run's checkout and context dir
// but keep's, a read-only checkout and a run with only one of the two
// among them, and the temp git configs, keeping the git config itself and
// any other dot name.
func TestSweepRemovesLeftoverRuns(t *testing.T) {
	t.Parallel()
	w, _ := newWorkspace(t)
	ctx := context.Background()
	for _, id := range []string{"live", "dead"} {
		if err := w.Prepare(ctx, id, "", map[string][]byte{"prompt.md": nil}); err != nil {
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
	w, _ := newWorkspace(t)
	ctx := context.Background()
	if err := w.Prepare(ctx, "dead", "", nil); err != nil {
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

// TestSweepGoesOnPastAFailure reports a run it can't remove, naming it, and
// still removes the runs after it.
func TestSweepGoesOnPastAFailure(t *testing.T) {
	t.Parallel()
	w, _ := newWorkspace(t)
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
	if err := w.Prepare(ctx, "b-dead", "", nil); err != nil {
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
