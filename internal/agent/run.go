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

	"github.com/mertenvg/logr/v2"
)

// waitDelay bounds the wait for claude's output pipes to close after it exits.
const waitDelay = 5 * time.Second

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
}

// Runner starts stage runs.
type Runner struct {
	Claude     string              // path to the claude binary
	StateDir   string              // /state; results go to StateDir/runs/<run-id>.json
	Credential *syscall.Credential // agent's, from UserCredential; nil (tests only) runs as the daemon
	Stderr     io.Writer           // claude's stderr; nil discards it
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

// Run executes claude -p for one stage and saves its raw JSON result to
// StateDir/runs/<run-id>.json before returning it. A run that times out or
// exits non-zero fails; its output is returned with the error, unsaved, so the
// caller can still match usage limits (SPEC §3.3).
func (r *Runner) Run(ctx context.Context, s Stage) ([]byte, error) {
	if filepath.Base(s.RunID) != s.RunID || strings.HasPrefix(s.RunID, ".") {
		return nil, fmt.Errorf("invalid run ID %q", s.RunID)
	}
	if len(s.Env) == 0 {
		// An empty Env would make exec pass the daemon's environment on.
		return nil, fmt.Errorf("run %s has no agent environment", s.RunID)
	}
	prompt, err := os.ReadFile(filepath.Join(s.ContextDir, "prompt.md"))
	if err != nil {
		return nil, fmt.Errorf("reading the prompt: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, s.Timeout)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, r.Claude, args(string(prompt), s)...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.Stdout = &stdout
	cmd.Stderr = r.Stderr
	cmd.SysProcAttr = r.sysProcAttr()
	cmd.Cancel = func() error { return r.killGroup(cmd.Process.Pid) }
	cmd.WaitDelay = waitDelay
	err = cmd.Run()
	if err != nil && ctx.Err() != nil {
		err = ctx.Err() // the timeout, rather than "signal: killed"
	}
	if cmd.Process != nil {
		err = errors.Join(err, r.cleanup(cmd.Process.Pid))
	}
	if err != nil {
		return stdout.Bytes(), fmt.Errorf("running claude: %w", err)
	}
	if err := r.saveResult(s.RunID, stdout.Bytes()); err != nil {
		return nil, fmt.Errorf("saving the result of run %s: %w", s.RunID, err)
	}
	return stdout.Bytes(), nil
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
	return a
}

// sysProcAttr starts claude in its own process group as r.Credential, with no
// AmbientCaps, so it starts with no capabilities (plan rev 2 on #3).
func (r *Runner) sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: r.Credential}
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
// recovery (SPEC §8.2) never reads half a result.
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
	return nil
}
