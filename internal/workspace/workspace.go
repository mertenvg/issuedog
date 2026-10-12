// Package workspace prepares and removes each run's checkout and context files (SPEC §3.2).
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/mertenvg/logr/v2"
)

// shaPattern matches a full SHA-1 or SHA-256 object name.
var shaPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Workspace creates per-run checkouts and context dirs. All paths are absolute.
type Workspace struct {
	Bare       string              // the daemon's bare clone, /work/<repo>.git
	RunsDir    string              // /work/runs, owned by agent; checkouts go in RunsDir/<run-id>
	ContextDir string              // /run/issuedog, owned by the daemon; context files go in ContextDir/<run-id>
	Credential *syscall.Credential // agent's; nil (tests only) runs as the daemon

	testEnv []string // extra git environment, for tests only
}

// Dir returns the run's checkout, RunsDir/<run-id>.
func (w *Workspace) Dir(runID string) string { return filepath.Join(w.RunsDir, runID) }

// Context returns the run's context dir, ContextDir/<run-id>.
func (w *Workspace) Context(runID string) string { return filepath.Join(w.ContextDir, runID) }

// Prepare writes the run's context files as the daemon (0755 dir, 0644 files:
// agent can read, not change), then clones the bare repo as agent, at main or
// detached at sha. On failure it removes whatever it created.
func (w *Workspace) Prepare(ctx context.Context, runID, sha string, files map[string][]byte) (err error) {
	if !validName(runID) {
		return fmt.Errorf("prepare run: invalid run ID %q", runID)
	}
	for name := range files {
		if !validName(name) {
			return fmt.Errorf("prepare run %s: invalid context file name %q", runID, name)
		}
	}
	if sha != "" && !shaPattern.MatchString(sha) {
		return fmt.Errorf("prepare run %s: invalid SHA %q", runID, sha)
	}
	cdir := w.Context(runID)
	if err := os.Mkdir(cdir, 0o755); err != nil {
		return fmt.Errorf("prepare run %s: create context dir: %w", runID, err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(cdir))
		}
	}()
	if err := writeContext(cdir, files); err != nil {
		return fmt.Errorf("prepare run %s: %w", runID, err)
	}
	if err := w.writeGitConfig(); err != nil {
		return fmt.Errorf("prepare run %s: %w", runID, err)
	}
	if err := w.checkout(ctx, runID, sha); err != nil {
		return fmt.Errorf("prepare run %s: %w", runID, err)
	}
	return nil
}

// Cleanup removes the checkout as agent (the daemon can't delete in agent's dir),
// then the context dir. Missing dirs are fine.
func (w *Workspace) Cleanup(ctx context.Context, runID string) error {
	if !validName(runID) {
		return fmt.Errorf("clean up run: invalid run ID %q", runID)
	}
	if err := w.removeCheckout(ctx, runID); err != nil {
		return fmt.Errorf("clean up run %s: %w", runID, err)
	}
	if err := os.RemoveAll(w.Context(runID)); err != nil {
		return fmt.Errorf("clean up run %s: remove context dir: %w", runID, err)
	}
	return nil
}

// validName refuses empty names, path separators and leading dots.
func validName(s string) bool {
	return s != "" && !strings.ContainsRune(s, filepath.Separator) && !strings.HasPrefix(s, ".")
}

// writeContext sets the dir to 0755 and each file to 0644, whatever the umask.
func writeContext(dir string, files map[string][]byte) error {
	if err := os.Chmod(dir, 0o755); err != nil {
		return fmt.Errorf("chmod context dir: %w", err)
	}
	for name, data := range files {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fmt.Errorf("create context file %s: %w", name, err)
		}
		err = f.Chmod(0o644)
		if err == nil {
			_, err = f.Write(data)
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("write context file %s: %w", name, err)
		}
	}
	return nil
}

// checkout clones as agent with --no-hardlinks, sharing no object files with
// the daemon's repo. A failed detach removes the clone.
func (w *Workspace) checkout(ctx context.Context, runID, sha string) error {
	dir := w.Dir(runID)
	args := []string{"clone", "--quiet", "--no-hardlinks", "--branch", "main"}
	if sha != "" {
		args = append(args, "--no-checkout")
	}
	if err := w.git(ctx, append(args, "--", w.Bare, dir)...); err != nil {
		return fmt.Errorf("clone %s: %w", w.Bare, err)
	}
	if sha == "" {
		return nil
	}
	if err := w.git(ctx, "-C", dir, "checkout", "--quiet", "--detach", sha); err != nil {
		return errors.Join(fmt.Errorf("check out %s: %w", sha, err), w.removeCheckout(ctx, runID))
	}
	return nil
}

// removeCheckout deletes the checkout as agent, making read-only dirs writable first.
func (w *Workspace) removeCheckout(ctx context.Context, runID string) error {
	script := `chmod -R u+rwx -- "$1" 2>/dev/null; rm -rf -- "$1"`
	if err := w.asAgent(ctx, nil, "/bin/sh", "-c", script, "sh", w.Dir(runID)); err != nil {
		return fmt.Errorf("remove checkout: %w", err)
	}
	return nil
}

// gitConfig is agent's git config, in the daemon's dir; run IDs can't start with a dot.
func (w *Workspace) gitConfig() string { return filepath.Join(w.ContextDir, ".agent.gitconfig") }

// writeGitConfig writes gitConfig (0644). It must be a file: git clears
// GIT_CONFIG_COUNT for a local clone's upload-pack, which checks safe.directory.
func (w *Workspace) writeGitConfig() error {
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w.Bare)
	data := "[core]\n\thooksPath = /dev/null\n[safe]\n\tdirectory = \"" + quoted + "\"\n"
	f, err := os.CreateTemp(w.ContextDir, ".agent.gitconfig.*")
	if err != nil {
		return fmt.Errorf("write git config: %w", err)
	}
	defer func() {
		// After the rename the temp file is gone, which is fine.
		if err := os.Remove(f.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logr.Warnf("removing %s: %v", f.Name(), err)
		}
	}()
	err = f.Chmod(0o644)
	if err == nil {
		_, err = f.WriteString(data)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), w.gitConfig())
	}
	if err != nil {
		return fmt.Errorf("write git config: %w", err)
	}
	return nil
}

// git runs git as agent, inheriting nothing (no GIT_* or SSH_ASKPASS), with no
// system config and gitConfig as the global one: no hooks, and the daemon's
// bare repo, which agent doesn't own, is a safe.directory.
func (w *Workspace) git(ctx context.Context, args ...string) error {
	env := append([]string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + w.gitConfig(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=", // empty, so git doesn't fall back to core.askPass
	}, w.testEnv...)
	if err := w.asAgent(ctx, env, "git", args...); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

// asAgent runs a command as w.Credential from /, with only env; errors include its output.
func (w *Workspace) asAgent(ctx context.Context, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.Env = append([]string{}, env...) // never nil, so nothing is inherited
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: w.Credential}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}
