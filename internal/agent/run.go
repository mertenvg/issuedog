package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mertenvg/issuedog/internal/dirsync"
	"github.com/mertenvg/logr/v2"
)

// defaultWaitDelay bounds the wait for claude's output pipes to close after
// it exits.
const defaultWaitDelay = 5 * time.Second

// Stage is one stage run of claude -p (SPEC §3.3).
type Stage struct {
	RunID        string        // names the result file, runs/<run-id>.json
	Dir          string        // the run's checkout, claude's working directory
	ContextDir   string        // /run/issuedog/<run-id>, holding prompt.md and system.md
	Schema       []byte        // the stage's JSON schema
	MaxTurns     int           // stages.max_turns.<stage>
	MaxBudgetUSD float64       // stages.max_budget_usd.<stage>; 0 leaves the flag out
	Timeout      time.Duration // stages.timeouts.<stage>; 0 fails the run
	Env          []string      // the agent environment, from Env
	// Model, Effort and FallbackModels are the run's resolved settings; each
	// left empty leaves its flag out, so claude uses its default.
	Model          string
	Effort         string
	FallbackModels []string // tried in order, passed comma-joined
}

// Runner starts stage runs.
type Runner struct {
	Claude     string              // path to the claude binary
	StateDir   string              // /state; results go to StateDir/runs/<run-id>.json
	Credential *syscall.Credential // agent's, from UserCredential; nil (tests only) runs as the daemon
	Stderr     io.Writer           // claude's stderr; nil discards it

	dirSyncFunc func(dir string) error // syncs the runs dir after a result's rename; nil (all but tests) uses dirsync.Sync
	waitDelay   time.Duration          // bounds the wait for claude's output pipes; 0 (all but tests) uses defaultWaitDelay
}

// dirSync flushes dir's entries to disk, through r.dirSyncFunc if it's set.
func (r *Runner) dirSync(dir string) error {
	if r.dirSyncFunc != nil {
		return r.dirSyncFunc(dir)
	}
	return dirsync.Sync(dir)
}

// UserCredential looks up a user's uid and gid, with no supplementary groups.
func UserCredential(name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("looking up user %s: %w", name, err)
	}
	uid, uidErr := strconv.ParseUint(u.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(u.Gid, 10, 32)
	if err := errors.Join(uidErr, gidErr); err != nil {
		return nil, fmt.Errorf("user %s ids: %w", name, err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, nil
}

// SetupError is a stage run's failure before claude starts: setting the run
// up, or starting claude. It isn't the agent's, as claude never ran.
type SetupError struct{ Err error }

func (e *SetupError) Error() string { return e.Err.Error() }
func (e *SetupError) Unwrap() error { return e.Err }

// Run executes claude -p for one stage and saves its raw JSON result to
// StateDir/runs/<run-id>.json before returning it. A run that times out or
// exits non-zero fails, and so does one whose result can't be saved; its
// output is returned with the error, unsaved, so the caller can still match
// usage limits (SPEC §3.3). A timeout's error wraps a *PublicCauseError
// naming s.Timeout. A failure before claude starts is a *SetupError.
func (r *Runner) Run(ctx context.Context, s Stage) ([]byte, error) {
	if filepath.Base(s.RunID) != s.RunID || strings.HasPrefix(s.RunID, ".") {
		return nil, &SetupError{fmt.Errorf("invalid run ID %q", s.RunID)}
	}
	if len(s.Env) == 0 {
		// An empty Env would make exec pass the daemon's environment on.
		return nil, &SetupError{fmt.Errorf("run %s has no agent environment", s.RunID)}
	}
	prompt, err := os.ReadFile(filepath.Join(s.ContextDir, "prompt.md"))
	if err != nil {
		return nil, &SetupError{fmt.Errorf("reading the prompt: %w", err)}
	}
	// The timeout's cause names it from config, in the daemon's words, so a
	// public comment may quote it (SPEC §5.3), where the context's own
	// error would only say a deadline passed.
	ctx, cancel := context.WithTimeoutCause(ctx, s.Timeout, &PublicCauseError{fmt.Sprintf("run timed out after %s", s.Timeout)})
	defer cancel()

	var stdout bytes.Buffer
	cmd := r.command(ctx, s, string(prompt), &stdout)
	if err := cmd.Start(); err != nil {
		return nil, &SetupError{fmt.Errorf("starting claude: %w", err)}
	}
	err = cmd.Wait()
	if err != nil && ctx.Err() != nil {
		// The timeout, rather than "signal: killed", or the caller's
		// cancellation, whose cause is its own.
		err = context.Cause(ctx)
	}
	if cmd.Process != nil {
		err = errors.Join(err, r.cleanup(cmd.Process.Pid))
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("running claude: %w", err)
	}
	if err := r.saveResult(s.RunID, stdout.Bytes()); err != nil {
		return stdout.Bytes(), fmt.Errorf("saving the result of run %s: %w", s.RunID, err)
	}
	return stdout.Bytes(), nil
}

// command builds the claude command for one stage run: in its own process
// group, killed as a group when ctx is done, with r.waitDelay, or else
// defaultWaitDelay, bounding the wait for its output pipes.
//
// Run must build its command here. TestRunFailures's timeout subtests set
// r.waitDelay to an hour, so that only the group kill ends their run within
// the test's bound, and TestRunCommandCancelKillsGroup tests this command's
// Cancel. A command Run built for itself would wait the default 5s instead,
// so a group kill in it that killed nothing would go unnoticed.
func (r *Runner) command(ctx context.Context, s Stage, prompt string, stdout io.Writer) *exec.Cmd {
	cmd := exec.CommandContext(ctx, r.Claude, args(prompt, s)...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.Stdout = stdout
	cmd.Stderr = r.Stderr
	cmd.SysProcAttr = r.sysProcAttr()
	cmd.Cancel = func() error { return r.killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = defaultWaitDelay
	if r.waitDelay > 0 {
		cmd.WaitDelay = r.waitDelay
	}
	return cmd
}

// args builds claude's arguments (SPEC §3.3).
func args(prompt string, s Stage) []string {
	a := []string{
		"-p", prompt,
		"--output-format", "json",
		"--json-schema", string(s.Schema),
		"--append-system-prompt-file", filepath.Join(s.ContextDir, "system.md"),
		"--dangerously-skip-permissions",
		"--no-session-persistence",
		"--max-turns", strconv.Itoa(s.MaxTurns),
	}
	if s.MaxBudgetUSD > 0 {
		a = append(a, "--max-budget-usd", strconv.FormatFloat(s.MaxBudgetUSD, 'f', -1, 64))
	}
	if s.Model != "" {
		a = append(a, "--model", s.Model)
	}
	if s.Effort != "" {
		a = append(a, "--effort", s.Effort)
	}
	if len(s.FallbackModels) > 0 {
		a = append(a, "--fallback-model", strings.Join(s.FallbackModels, ","))
	}
	return a
}

// sysProcAttr starts claude in its own process group as r.Credential.
func (r *Runner) sysProcAttr() *syscall.SysProcAttr {
	return SysProcAttr(r.Credential)
}

// SysProcAttr starts a process in its own process group as cred, with no
// AmbientCaps, so it starts with no capabilities (SPEC §3).
func SysProcAttr(cred *syscall.Credential) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: cred}
}

// cleanup kills every process of the agent uid, even setsid ones, as only one
// run is active (SPEC §8.2). The daemon's own uid (tests) gets just the group.
func (r *Runner) cleanup(pgid int) error {
	if r.Credential == nil || r.Credential.Uid == uint32(os.Getuid()) {
		return r.killGroup(pgid)
	}
	// kill(-1) skips the calling shell, and fails only if nothing was left.
	if err := r.asAgent(`kill -KILL -1 2>/dev/null || true`); err != nil {
		return fmt.Errorf("killing the agent's processes: %w", err)
	}
	return nil
}

// killGroup kills a run's process group, if it still exists. Without CAP_KILL
// the daemon can't signal another uid, so with a Credential it runs as that uid.
func (r *Runner) killGroup(pgid int) error {
	if r.Credential == nil {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("killing process group %d: %w", pgid, err)
		}
		return nil
	}
	if err := r.asAgent(`kill -KILL -"$1" 2>/dev/null || ! kill -0 -"$1" 2>/dev/null`, strconv.Itoa(pgid)); err != nil {
		return fmt.Errorf("killing process group %d: %w", pgid, err)
	}
	return nil
}

// asAgent runs a /bin/sh script as r.Credential, with an empty environment.
func (r *Runner) asAgent(script string, args ...string) error {
	cmd := exec.Command("/bin/sh", append([]string{"-c", script, "sh"}, args...)...)
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: r.Credential}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// saveResult writes runs/<run-id>.json via a temp file and a rename, so restart
// recovery (SPEC §8.2) never reads half a result, then syncs the runs dir, so
// the rename survives a crash.
func (r *Runner) saveResult(runID string, raw []byte) error {
	dir := filepath.Join(r.StateDir, "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the runs dir: %w", err)
	}
	f, err := os.CreateTemp(dir, runID+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp result file: %w", err)
	}
	defer func() {
		// After the rename the temp file is gone, which is fine.
		if err := os.Remove(f.Name()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			logr.Warnf("removing %s: %v", f.Name(), err)
		}
	}()
	_, err = f.Write(raw)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(dir, runID+".json"))
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", f.Name(), err)
	}
	if err := r.dirSync(dir); err != nil {
		return fmt.Errorf("after writing %s.json: %w", runID, err)
	}
	return nil
}
