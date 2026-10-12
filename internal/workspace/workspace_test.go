package workspace

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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

// newWorkspace makes a bare repo whose main has two commits, each writing
// f.txt, and returns a workspace for it with the first commit's SHA.
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

// TestPrepare checks out main and an older SHA under a restrictive umask.
func TestPrepare(t *testing.T) {
	w, first := newWorkspace(t)
	defer syscall.Umask(syscall.Umask(0o077))
	for _, tc := range []struct{ name, sha, want, head string }{
		{"main", "", "two", "main"},
		{"older-sha", first, "one", "HEAD"},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

// TestCleanup removes both dirs, even a read-only one in the checkout, and
// succeeds a second time.
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

// TestPrepareRejects refuses bad names and SHAs, and leaves nothing behind,
// including after a clone that succeeded but couldn't check out the SHA.
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
