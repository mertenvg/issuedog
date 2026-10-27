// Package workspace prepares and removes each run's checkout and context files (SPEC §3.2).
package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/mertenvg/logr/v2"
)

// shaPattern matches a full SHA-1 or SHA-256 object name.
var shaPattern = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// Workspace creates per-run checkouts and context dirs. All paths are absolute.
type Workspace struct {
	Bare       string              // the daemon's bare clone, /work/<repo>.git (repo.Repo.Dir)
	RunsDir    string              // /work/runs, owned by agent; checkouts go in RunsDir/<run-id>
	ContextDir string              // /run/issuedog, owned by the daemon; context files go in ContextDir/<run-id>
	Credential *syscall.Credential // agent's; nil (tests only) runs as the daemon

	testEnv []string // extra git environment, for tests only
}

// Dir returns the run's checkout, RunsDir/<run-id>.
func (w *Workspace) Dir(runID string) string { return filepath.Join(w.RunsDir, runID) }

// Context returns the run's context dir, ContextDir/<run-id>.
func (w *Workspace) Context(runID string) string { return filepath.Join(w.ContextDir, runID) }

// TargetRef is where a run's clone holds its named target (Commits.Target).
const TargetRef = "refs/heads/target"

// Commits are the commits the daemon names for a run (SPEC §3.4): the clone
// holds them and their history, and nothing else.
type Commits struct {
	Start  string // required: the clone's HEAD, detached
	Target string // optional: a commit to merge, at TargetRef
}

// Prepare writes the run's context files as the daemon (0755 dir, 0644 files:
// agent can read, not change), then makes the run's clone of c as agent
// (checkout). Start must be a full SHA, and Target one too if it's set. On
// failure it removes whatever it created.
func (w *Workspace) Prepare(ctx context.Context, runID string, c Commits, files map[string][]byte) (err error) {
	if !validName(runID) {
		return fmt.Errorf("prepare run: invalid run ID %q", runID)
	}
	for name := range files {
		if !validName(name) {
			return fmt.Errorf("prepare run %s: invalid context file name %q", runID, name)
		}
	}
	if !shaPattern.MatchString(c.Start) {
		return fmt.Errorf("prepare run %s: invalid start SHA %q", runID, c.Start)
	}
	if c.Target != "" && !shaPattern.MatchString(c.Target) {
		return fmt.Errorf("prepare run %s: invalid target SHA %q", runID, c.Target)
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
	if err := w.WriteGitConfig(); err != nil {
		return fmt.Errorf("prepare run %s: %w", runID, err)
	}
	if err := w.checkout(ctx, runID, c); err != nil {
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

// Bundle writes the run's commits past start, start..HEAD in its clone, to
// out as a git bundle, made as agent (SPEC §3.2). The daemon gives an out
// in a dir only it can write, such as a file it created there, so it never
// opens a path agent controls. A clone with no commits past start writes
// nothing to out, and gives false and no error. On an error, out may hold
// part of a bundle. Its git calls read the commits as they were made
// (asCommitted).
func (w *Workspace) Bundle(ctx context.Context, runID, start string, out io.Writer) (bool, error) {
	if !validName(runID) {
		return false, fmt.Errorf("bundle run: invalid run ID %q", runID)
	}
	if !shaPattern.MatchString(start) {
		return false, fmt.Errorf("bundle run %s: invalid start SHA %q", runID, start)
	}
	dir, commits := w.Dir(runID), start+"..HEAD"
	var count bytes.Buffer
	if err := w.gitTo(ctx, &count, asCommitted, "-C", dir, "rev-list", "--count", commits, "--"); err != nil {
		return false, fmt.Errorf("bundle run %s: count commits past %s: %w", runID, start, err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(count.String()))
	if err != nil {
		return false, fmt.Errorf("bundle run %s: count commits past %s: %w", runID, start, err)
	}
	if n == 0 {
		return false, nil
	}
	if err := w.gitTo(ctx, out, asCommitted, "-C", dir, "bundle", "create", "--quiet", "-", commits, "--"); err != nil {
		return false, fmt.Errorf("bundle run %s: %w", runID, err)
	}
	return true, nil
}

// asCommitted is the environment that makes git read a clone's commits as
// they were made: a replace ref (refs/replace/*) or a graft (info/grafts)
// in the clone can't change what history git sees, such as by making
// start..HEAD look empty. The graft file is one that can't exist, not
// /dev/null, which git reads and then warns of grafts' deprecation.
var asCommitted = []string{"GIT_NO_REPLACE_OBJECTS=1", "GIT_GRAFT_FILE=/dev/null/no-grafts"}

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

// checkout makes the run's clone as agent, holding only c's commits and
// their history, with no remote (SPEC §3.2). It creates the dir, which
// mustn't exist yet, then fills it (fetch). A failure after that removes
// the dir.
func (w *Workspace) checkout(ctx context.Context, runID string, c Commits) error {
	dir := w.Dir(runID)
	if err := w.asAgent(ctx, nil, "mkdir", "--", dir); err != nil {
		return fmt.Errorf("create checkout: %w", err)
	}
	if err := w.fetch(ctx, dir, c); err != nil {
		return errors.Join(fmt.Errorf("clone %s: %w", c.Start, err), w.removeCheckout(ctx, runID))
	}
	return nil
}

// fetch inits a repo in dir, of Start's object format, and fetches c into
// it from the bare repo by SHA, with no tags and no FETCH_HEAD, so nothing
// names the bare repo: Target to TargetRef, and Start to no ref, as the
// detached HEAD it checks out holds it. The fetch copies the objects, so
// the clone shares no object files with the daemon's repo. Fetching a SHA
// no ref of the bare repo advertises works under protocol v2, git's
// default.
func (w *Workspace) fetch(ctx context.Context, dir string, c Commits) error {
	format := "sha1"
	if len(c.Start) == 64 {
		format = "sha256"
	}
	if err := w.git(ctx, "init", "--quiet", "--object-format="+format, "--", dir); err != nil {
		return fmt.Errorf("init clone: %w", err)
	}
	args := []string{"-C", dir, "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--", w.Bare, c.Start}
	if c.Target != "" {
		args = append(args, c.Target+":"+TargetRef)
	}
	if err := w.git(ctx, args...); err != nil {
		return fmt.Errorf("fetch from %s: %w", w.Bare, err)
	}
	if err := w.git(ctx, "-C", dir, "checkout", "--quiet", "--detach", c.Start); err != nil {
		return fmt.Errorf("check out %s: %w", c.Start, err)
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

// WriteGitConfig writes gitConfig (0644), the global config of agent's git,
// as Prepare does for each run. It must be a file: git clears
// GIT_CONFIG_COUNT for a local fetch's upload-pack, which checks safe.directory.
func (w *Workspace) WriteGitConfig() error {
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
	return w.gitTo(ctx, nil, nil, args...)
}

// gitTo is git, with git's stdout going to stdout and extra in its
// environment; errors include its stderr.
func (w *Workspace) gitTo(ctx context.Context, stdout io.Writer, extra []string, args ...string) error {
	env := append(append([]string{
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + w.gitConfig(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_ASKPASS=", // empty, so git doesn't fall back to core.askPass
	}, extra...), w.testEnv...)
	if err := w.asAgentTo(ctx, stdout, env, "git", args...); err != nil {
		return fmt.Errorf("git %s: %w", args[0], err)
	}
	return nil
}

// asAgent runs a command as w.Credential from /, with only env; errors include its output.
func (w *Workspace) asAgent(ctx context.Context, env []string, name string, args ...string) error {
	return w.asAgentTo(ctx, nil, env, name, args...)
}

// asAgentTo is asAgent, with the command's stdout going to stdout, and only
// its stderr in errors. A nil stdout keeps both in errors, as asAgent does.
func (w *Workspace) asAgentTo(ctx context.Context, stdout io.Writer, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = "/"
	cmd.Env = append([]string{}, env...) // never nil, so nothing is inherited
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: w.Credential}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &out
	if stdout == nil {
		cmd.Stdout = &out // one buffer, so the output keeps its order, as CombinedOutput's does
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out.Bytes()))
	}
	return nil
}
