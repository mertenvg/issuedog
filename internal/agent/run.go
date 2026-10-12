package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
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

// waitDelay bounds how long Run waits for claude's output pipes to close
// after the process exits or is killed, in case something outside its
// process group still holds them.
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
	Claude   string // path to the claude binary
	StateDir string // /state; results go to StateDir/runs/<run-id>.json
	// Credential is the uid and gid claude runs as, normally agent's from
	// UserCredential. Nil runs it as the daemon's own user, which only tests
	// do.
	Credential *syscall.Credential
	Stderr     io.Writer // claude's stderr; nil discards it
}

// UserCredential looks up a user's uid and gid. It has no supplementary
// groups, so a process started with it doesn't keep the daemon's groups.
func UserCredential(name string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("looking up user %s: %w", name, err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %s uid: %w", name, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("user %s gid: %w", name, err)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, nil
}

// Run executes claude -p for one stage and returns its raw JSON result, after
// writing it to StateDir/runs/<run-id>.json. A run that times out or exits
// non-zero fails. Its output isn't written, but it's returned with the error
// so the caller can still match usage limits (SPEC §3.3).
func (r *Runner) Run(ctx context.Context, s Stage) ([]byte, error) {
	if filepath.Base(s.RunID) != s.RunID || strings.HasPrefix(s.RunID, ".") {
		return nil, fmt.Errorf("invalid run ID %q", s.RunID)
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
	cmd.Cancel = func() error {
		err := r.killGroup(cmd.Process.Pid)
		if err != nil {
			logr.Errorf("stopping claude for run %s: %v", s.RunID, err)
		}
		return err
	}
	cmd.WaitDelay = waitDelay
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			err = ctx.Err() // the timeout, rather than "signal: killed"
		}
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

// sysProcAttr starts claude in its own process group, as r.Credential. It
// sets no AmbientCaps: the daemon's file capabilities aren't inheritable, so
// without ambient ones claude starts with none (plan rev 2 on #3).
func (r *Runner) sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true, Credential: r.Credential}
}

// killGroup kills a run's whole process group. The daemon has no CAP_KILL,
// so it can't signal another uid's processes. With a Credential, the kill
// runs as that uid instead, through the shell's kill builtin.
func (r *Runner) killGroup(pgid int) error {
	if r.Credential == nil {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil {
			return fmt.Errorf("killing process group %d: %w", pgid, err)
		}
		return nil
	}
	kill := exec.Command("/bin/sh", "-c", `kill -KILL -"$1"`, "sh", strconv.Itoa(pgid))
	kill.Dir = "/"
	kill.Env = []string{}
	kill.SysProcAttr = &syscall.SysProcAttr{Credential: r.Credential}
	if out, err := kill.CombinedOutput(); err != nil {
		return fmt.Errorf("killing process group %d: %w: %s", pgid, err, bytes.TrimSpace(out))
	}
	return nil
}

// saveResult writes a run's raw result to StateDir/runs/<run-id>.json
// through a temp file and a rename, so a crash never leaves half a result
// for restart recovery (SPEC §8.2) to read.
func (r *Runner) saveResult(runID string, raw []byte) error {
	dir := filepath.Join(r.StateDir, "runs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating the runs dir: %w", err)
	}
	f, err := os.CreateTemp(dir, runID+".*.tmp")
	if err != nil {
		return fmt.Errorf("creating a temp result file: %w", err)
	}
	defer os.Remove(f.Name()) // fails harmlessly once renamed
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("writing the result: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("syncing the result: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing the result: %w", err)
	}
	if err := os.Rename(f.Name(), filepath.Join(dir, runID+".json")); err != nil {
		return fmt.Errorf("renaming the result: %w", err)
	}
	return nil
}
