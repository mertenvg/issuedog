// Package workspace prepares each run's checkout and context files, and
// removes them afterwards (SPEC §3.2).
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// shaPattern matches a full SHA-1 or SHA-256 object name.
var shaPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Workspace creates per-run checkouts and context dirs. All paths are absolute.
type Workspace struct {
	Bare       string              // the daemon's bare clone, /work/<repo>.git
	RunsDir    string              // /work/runs, owned by agent; checkouts go in RunsDir/<run-id>
	ContextDir string              // /run/issuedog, owned by the daemon; context files go in ContextDir/<run-id>
	Credential *syscall.Credential // agent's; nil (tests only) runs as the daemon
}

// Dir returns the run's checkout, RunsDir/<run-id>.
func (w *Workspace) Dir(runID string) string { return filepath.Join(w.RunsDir, runID) }

// Context returns the run's context dir, ContextDir/<run-id>.
func (w *Workspace) Context(runID string) string { return filepath.Join(w.ContextDir, runID) }

// Prepare writes the run's context files as the daemon, in a 0755 dir with
// 0644 files that agent can read but not change. It then clones the bare repo
// as agent into a fresh checkout, at main when sha is "", otherwise detached
// at sha. On failure it removes whatever it created.
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
	if err := w.checkout(ctx, runID, sha); err != nil {
		return fmt.Errorf("prepare run %s: %w", runID, err)
	}
	return nil
}

// Cleanup removes the run's checkout as agent, since the daemon can't delete
// in agent's dir, then the context dir as the daemon. Missing dirs are fine.
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

// writeContext writes each file with mode 0644, and sets the dir to 0755,
// both explicitly so the umask can't change them.
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

// checkout clones the bare repo as agent, with --no-hardlinks so the checkout
// shares no object files with the daemon's repo. A failed clone removes its
// own dir; a failed detach removes the clone.
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

// removeCheckout deletes the checkout as agent. It first makes every dir
// writable, so a read-only dir left by the run can't block the delete.
func (w *Workspace) removeCheckout(ctx context.Context, runID string) error {
	script := `chmod -R u+rwx -- "$1" 2>/dev/null; rm -rf -- "$1"`
	if err := w.asAgent(ctx, nil, "/bin/sh", "-c", script, "sh", w.Dir(runID)); err != nil {
		return fmt.Errorf("remove checkout: %w", err)
	}
	return nil
}

// git runs git as agent with a safe environment:
//   - nothing is inherited, so no GIT_* variable or SSH_ASKPASS applies;
//   - the system and global config are ignored and hooks are disabled;
//   - the daemon's bare repo is a safe.directory, as agent doesn't own it.
func (w *Workspace) git(ctx context.Context, args ...string) error {
	env := []string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=", // empty, so git doesn't fall back to core.askPass
		"GIT_CONFIG_COUNT=2",
		"GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0=/dev/null",
		"GIT_CONFIG_KEY_1=safe.directory", "GIT_CONFIG_VALUE_1=" + w.Bare,
	}
	if err := w.asAgent(ctx, env, "git", args...); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

// asAgent runs a command as w.Credential from /, with only env. The error
// includes the command's output.
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
