package verify

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/workspace"
	"github.com/mertenvg/logr/v2"
)

const (
	// outputTailBytes bounds a command's output kept in memory.
	outputTailBytes = 4096
	// outputChars is how much of the end of a command's output its result
	// keeps.
	outputChars = 1000
	// homeSuffix names a verify's fresh HOME, beside its checkout:
	// RunsDir/<id>-home.
	homeSuffix = "-home"
)

// Commands runs verify.commands for rule 1 of SPEC §10.1: each through the
// shell, as agent, in a fresh checkout of the head, with a fresh HOME and no
// credential.
type Commands struct {
	Workspace  *workspace.Workspace // makes and removes the checkout and the HOME, as agent
	Credential *syscall.Credential  // agent's; nil (tests only) runs as the daemon
	Proxy      string               // the egress proxy's URL
	Timeout    func() time.Duration // verify.command_timeout, read as each command starts

	waitDelay time.Duration // bounds the wait for a command's output pipes; 0 (all but tests) uses agent.DefaultWaitDelay
}

// CommandResult is one command's outcome.
type CommandResult struct {
	Command  string        // the command, as verify.commands gives it
	Exit     int           // its exit status; -1 when a signal ended it, as on a timeout
	TimedOut bool          // it ran past Timeout, so its group was killed
	Timeout  time.Duration // the timeout it ran under
	Output   string        // the last 1,000 characters of its stdout and stderr, together
}

// Failure is why the command failed, in the daemon's words, or "" when it
// passed: the timeout it ran past, or its exit status.
func (r CommandResult) Failure() string {
	switch {
	case r.TimedOut:
		return fmt.Sprintf("timed out after %s", r.Timeout)
	case r.Exit != 0:
		return fmt.Sprintf("exit status %d", r.Exit)
	}
	return ""
}

// Run runs cmds, in order, in id's fresh checkout of head, which Workspace
// makes as agent: a clone of head alone, with no remote (SPEC §3.2). Every
// command runs, even after one fails, and gives one result. The commands get
// RunsDir/<id>-home, made empty as agent, as HOME, and a tmp dir in it as
// TMPDIR. Both dirs are removed afterwards, also on an error, and a crash's
// leftover is the startup sweep's. A failure to make them, to start or end a
// command, or the caller's ctx ending, is an error, not a result.
func (c *Commands) Run(ctx context.Context, id, head string, cmds []string) (results []CommandResult, err error) {
	home := id + homeSuffix
	defer func() {
		// The caller's ctx may be done, and the dirs must go anyway.
		clean := context.WithoutCancel(ctx)
		err = errors.Join(err, c.Workspace.Cleanup(clean, id), c.Workspace.Cleanup(clean, home))
	}()
	if err := c.Workspace.Prepare(ctx, id, workspace.Commits{Start: head}, nil); err != nil {
		return nil, fmt.Errorf("verify %s: %w", id, err)
	}
	if err := c.Workspace.PrepareHome(ctx, home); err != nil {
		return nil, fmt.Errorf("verify %s: %w", id, err)
	}
	env := append(agent.CommandEnv(c.Proxy, c.Workspace.Dir(home)), "TMPDIR="+filepath.Join(c.Workspace.Dir(home), "tmp"))
	for _, cmd := range cmds {
		r, err := c.run(ctx, c.Workspace.Dir(id), env, cmd)
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", id, err)
		}
		if f := r.Failure(); f != "" {
			logr.Infof("verify %s: %q failed: %s", id, cmd, f)
		} else {
			logr.Infof("verify %s: %q passed", id, cmd)
		}
		results = append(results, r)
	}
	return results, nil
}

// errCommandTimeout is a command's context's cause when it runs past its
// timeout, as against the caller's ctx ending.
var errCommandTimeout = errors.New("command timed out")

// run runs one command through /bin/sh in dir, as c.Credential, in its own
// process group, which is killed when the timeout passes. Once it's done,
// every agent process is killed (agent.KillLeftovers), as after a claude run.
func (c *Commands) run(ctx context.Context, dir string, env []string, command string) (CommandResult, error) {
	timeout := c.Timeout()
	cmdCtx, cancel := context.WithTimeoutCause(ctx, timeout, errCommandTimeout)
	defer cancel()
	out := agent.NewTail(outputTailBytes)
	cmd := exec.CommandContext(cmdCtx, "/bin/sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out // one pipe, so the output keeps its order
	cmd.SysProcAttr = agent.SysProcAttr(c.Credential)
	cmd.Cancel = func() error { return agent.KillGroup(c.Credential, cmd.Process.Pid) }
	cmd.WaitDelay = agent.DefaultWaitDelay
	if c.waitDelay > 0 {
		cmd.WaitDelay = c.waitDelay
	}
	if err := cmd.Start(); err != nil {
		return CommandResult{}, fmt.Errorf("starting %q: %w", command, err)
	}
	waitErr := cmd.Wait()
	if err := agent.KillLeftovers(c.Credential, cmd.Process.Pid); err != nil {
		return CommandResult{}, fmt.Errorf("after %q: %w", command, err)
	}
	if err := ctx.Err(); err != nil {
		return CommandResult{}, fmt.Errorf("running %q: %w", command, context.Cause(ctx))
	}
	if cmd.ProcessState == nil {
		return CommandResult{}, fmt.Errorf("running %q: %w", command, waitErr)
	}
	// It timed out only if the timeout's group kill ended it: a signal
	// ended the shell (exit -1) and the timeout has passed. As in
	// agent.Runner.Run, a command that ends cleanly as its timeout passes
	// has passed. A command that exits on its own keeps its own exit
	// status, also when a process it left holds its output open past the
	// timeout, until the wait delay ends the wait (exec.ErrWaitDelay).
	timedOut := cmd.ProcessState.ExitCode() == -1 && errors.Is(context.Cause(cmdCtx), errCommandTimeout)
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		// It exited, but a process it left held its output open, which
		// KillLeftovers has now killed. Its exit status still stands.
		logr.Warnf("%q exited, but a process it left held its output open for %s", command, cmd.WaitDelay)
	}
	return CommandResult{
		Command:  command,
		Exit:     cmd.ProcessState.ExitCode(),
		TimedOut: timedOut,
		Timeout:  timeout,
		Output:   lastChars(out.Text(), outputChars),
	}, nil
}

// lastChars is the last n characters of s.
func lastChars(s string, n int) string {
	for i := len(s); i > 0; {
		_, size := utf8.DecodeLastRuneInString(s[:i])
		i -= size
		if n--; n == 0 {
			return s[i:]
		}
	}
	return s
}
