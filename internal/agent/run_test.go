package agent

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fakeOutput = `{"type":"result","subtype":"success","is_error":false,"result":"Done."}` + "\n"

// currentUser is a Credential tests can switch to without privileges.
func currentUser() *syscall.Credential {
	return &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), NoSetGroups: true}
}

func fakeRun(t *testing.T, mode string, cred *syscall.Credential) (*Runner, Stage) {
	t.Helper()
	claude, err := filepath.Abs(filepath.Join("testdata", "fake-claude"))
	if err != nil {
		t.Fatal(err)
	}
	ctxDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(ctxDir, "prompt.md"), []byte("Implement #27.\nTwo lines."), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Claude: claude, StateDir: t.TempDir(), Credential: cred}
	return r, Stage{
		RunID:      "run-1",
		Dir:        t.TempDir(),
		ContextDir: ctxDir,
		Schema:     []byte(`{"type":"object"}`),
		MaxTurns:   200,
		Timeout:    10 * time.Second,
		Env:        []string{"FAKE_MODE=" + mode, "PATH=" + agentPath},
	}
}

// TestRunSuccess checks the full argument list, with every optional flag set.
func TestRunSuccess(t *testing.T) {
	r, s := fakeRun(t, "success", nil)
	s.MaxBudgetUSD = 2.5
	s.Model = "opus"
	s.Effort = "high"
	s.FallbackModels = []string{"sonnet", "haiku"}
	out, err := r.Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != fakeOutput {
		t.Errorf("output = %q, want %q", out, fakeOutput)
	}
	saved, err := os.ReadFile(filepath.Join(r.StateDir, "runs", "run-1.json"))
	if err != nil || !bytes.Equal(saved, out) {
		t.Errorf("result file = %q, %v; want the output", saved, err)
	}
	raw, err := os.ReadFile(filepath.Join(s.Dir, "args"))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	want := []string{
		"-p", "Implement #27.\nTwo lines.",
		"--output-format", "json",
		"--json-schema", `{"type":"object"}`,
		"--append-system-prompt-file", filepath.Join(s.ContextDir, "system.md"),
		"--dangerously-skip-permissions",
		"--no-session-persistence",
		"--max-turns", "200",
		"--max-budget-usd", "2.5",
		"--model", "opus",
		"--effort", "high",
		"--fallback-model", "sonnet,haiku",
	}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if a := args("p", Stage{}); slices.Contains(a, "--max-budget-usd") {
		t.Errorf("args without a budget = %q, want no --max-budget-usd", a)
	}
}

// TestArgsModelFlags checks that --model, --effort and --fallback-model are
// passed only when set, after the fixed flags.
func TestArgsModelFlags(t *testing.T) {
	base := args("p", Stage{})
	tests := []struct {
		name string
		s    Stage
		want []string
	}{
		{name: "none", want: []string{}},
		{name: "model", s: Stage{Model: "opus"}, want: []string{"--model", "opus"}},
		{name: "effort", s: Stage{Effort: "xhigh"}, want: []string{"--effort", "xhigh"}},
		{name: "one fallback", s: Stage{FallbackModels: []string{"sonnet"}}, want: []string{"--fallback-model", "sonnet"}},
		{name: "two fallbacks", s: Stage{FallbackModels: []string{"sonnet", "haiku"}}, want: []string{"--fallback-model", "sonnet,haiku"}},
		{name: "empty fallback list", s: Stage{FallbackModels: []string{}}, want: []string{}},
		{
			name: "all",
			s:    Stage{Model: "claude-opus-5-5", Effort: "low", FallbackModels: []string{"sonnet", "haiku"}},
			want: []string{"--model", "claude-opus-5-5", "--effort", "low", "--fallback-model", "sonnet,haiku"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := args("p", tt.s)
			if len(got) < len(base) || !slices.Equal(got[:len(base)], base) {
				t.Fatalf("args = %q, want them to start with %q", got, base)
			}
			if tail := got[len(base):]; !slices.Equal(tail, tt.want) {
				t.Errorf("model flags = %q, want %q", tail, tt.want)
			}
		})
	}
}

func TestRunFailures(t *testing.T) {
	tests := []struct {
		name    string
		mode    string
		runID   string
		cred    *syscall.Credential
		wantErr string
		wantOut string
		setup   bool // a *SetupError, not the agent's failure
	}{
		{name: "non-zero exit", mode: "fail", wantErr: "exit status 1", wantOut: "error_max_turns"},
		{name: "timeout", mode: "hang", wantErr: "deadline exceeded"},
		{name: "timeout with credential", mode: "hang", cred: currentUser(), wantErr: "deadline exceeded"},
		{name: "run ID with a path", mode: "success", runID: "../run-1", wantErr: "invalid run ID", setup: true},
		{name: "empty run ID", mode: "success", runID: "", wantErr: "invalid run ID", setup: true},
		{name: "no environment", mode: "success", wantErr: "no agent environment", setup: true},
		{name: "no prompt", mode: "success", wantErr: "reading the prompt", setup: true},
		{name: "no claude", mode: "success", wantErr: "starting claude", setup: true},
		{name: "result not saved", mode: "success", wantErr: "saving the result", wantOut: "Done."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, s := fakeRun(t, tt.mode, tt.cred)
			if tt.mode == "hang" {
				s.Timeout = 300 * time.Millisecond
				// claude's child holds its output open for 300s, and the wait
				// for that output is an hour. So only the group kill ends the
				// run within the bound below, which is generous so a loaded
				// machine doesn't fail it.
				r.waitDelay = time.Hour
			}
			switch tt.wantErr {
			case "invalid run ID":
				s.RunID = tt.runID
			case "no agent environment":
				s.Env = nil
			case "reading the prompt":
				s.ContextDir = t.TempDir()
			case "starting claude":
				r.Claude = filepath.Join(t.TempDir(), "none")
			case "saving the result":
				// A file where the runs dir goes.
				if err := os.WriteFile(filepath.Join(r.StateDir, "runs"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			start := time.Now()
			out, err := r.Run(context.Background(), s)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
			}
			var setup *SetupError
			if errors.As(err, &setup) != tt.setup {
				t.Errorf("err = %v, a *SetupError: %t, want %t", err, !tt.setup, tt.setup)
			}
			if !strings.Contains(string(out), tt.wantOut) {
				t.Errorf("output = %q, want it to contain %q", out, tt.wantOut)
			}
			if entries, _ := os.ReadDir(filepath.Join(r.StateDir, "runs")); len(entries) != 0 {
				t.Errorf("a failed run left files in runs: %v", entries)
			}
			if tt.mode != "hang" {
				return
			}
			if d := time.Since(start); d >= time.Minute {
				t.Errorf("Run took %v; the group kill should close claude's output at once", d)
			}
			waitChildDead(t, s.Dir)
		})
	}
}

// TestRunWaitDelay: claude's WaitDelay is 5s unless a test sets another.
func TestRunWaitDelay(t *testing.T) {
	t.Parallel()
	if got := (&Runner{}).cmdWaitDelay(); got != 5*time.Second {
		t.Errorf("WaitDelay = %v, want 5s", got)
	}
	if got := (&Runner{waitDelay: time.Hour}).cmdWaitDelay(); got != time.Hour {
		t.Errorf("WaitDelay with an hour set = %v, want 1h", got)
	}
}

func TestRunKillsLeftovers(t *testing.T) {
	r, s := fakeRun(t, "background", nil)
	if _, err := r.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	waitChildDead(t, s.Dir)
}

// waitChildDead fails the test unless the child the fake script recorded in
// dir/child.pid exits (or is a zombie) within a minute. The child sleeps 300s,
// so one that wasn't killed still fails. The minute is generous so a loaded
// machine doesn't fail it.
func waitChildDead(t *testing.T, dir string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			return
		}
		if i := bytes.LastIndexByte(stat, ')'); i > 0 && len(stat) > i+2 && (stat[i+2] == 'Z' || stat[i+2] == 'X') {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("child %d outlived the run", pid)
}

func TestSysProcAttr(t *testing.T) {
	cred := &syscall.Credential{Uid: 10002, Gid: 10002, Groups: []uint32{}}
	a := (&Runner{Credential: cred}).sysProcAttr()
	if !a.Setpgid || a.Credential != cred || len(a.AmbientCaps) != 0 {
		t.Errorf("SysProcAttr = %+v, want its own process group, the credential and no ambient capabilities", a)
	}
}

func TestUserCredential(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	c, err := UserCredential(u.Username)
	if err != nil {
		t.Fatal(err)
	}
	if int(c.Uid) != os.Getuid() || int(c.Gid) != os.Getgid() || c.Groups == nil || len(c.Groups) != 0 || c.NoSetGroups {
		t.Errorf("credential = %+v, want uid %d, gid %d and no supplementary groups", c, os.Getuid(), os.Getgid())
	}
	if _, err := UserCredential("no-such-user-issuedog"); err == nil {
		t.Error("an unknown user should fail")
	}
}

// TestSaveResultDirSyncAfterRename syncs the runs dir once, after the
// result is renamed into place.
func TestSaveResultDirSyncAfterRename(t *testing.T) {
	t.Parallel()
	r := &Runner{StateDir: t.TempDir()}
	dir := filepath.Join(r.StateDir, "runs")
	path := filepath.Join(dir, "run-1.json")
	var synced []string
	r.dirSyncFunc = func(d string) error {
		synced = append(synced, d)
		if got, err := os.ReadFile(path); err != nil || string(got) != fakeOutput {
			t.Errorf("at the sync, the result file = %q, %v; want the output in place", got, err)
		}
		return nil
	}
	if err := r.saveResult("run-1", []byte(fakeOutput)); err != nil {
		t.Fatal(err)
	}
	if len(synced) != 1 || synced[0] != dir {
		t.Errorf("synced %q, want [%q]", synced, dir)
	}
}

// TestSaveResultDirSyncErrorReturned returns a failed sync's error, wrapped
// and naming the result, and leaves no temp file. The renamed result
// stays: the data in it is synced already, and a run whose result wasn't
// saved fails, so the daemon's cleanup removes it.
func TestSaveResultDirSyncErrorReturned(t *testing.T) {
	t.Parallel()
	r := &Runner{StateDir: t.TempDir()}
	errSync := errors.New("sync failed")
	r.dirSyncFunc = func(string) error { return errSync }
	err := r.saveResult("run-1", []byte(fakeOutput))
	if !errors.Is(err, errSync) {
		t.Fatalf("err = %v, want the sync error", err)
	}
	if !strings.Contains(err.Error(), "run-1.json") {
		t.Errorf("err = %v, want it to name run-1.json", err)
	}
	entries, rerr := os.ReadDir(filepath.Join(r.StateDir, "runs"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 1 || entries[0].Name() != "run-1.json" {
		t.Errorf("runs has %v, want only run-1.json", entries)
	}
}

// TestRunDirSyncErrorFailsTheRun fails a run whose result's sync fails,
// as one whose result can't be saved, and still returns its output.
func TestRunDirSyncErrorFailsTheRun(t *testing.T) {
	t.Parallel()
	r, s := fakeRun(t, "success", nil)
	errSync := errors.New("sync failed")
	r.dirSyncFunc = func(string) error { return errSync }
	out, err := r.Run(context.Background(), s)
	if !errors.Is(err, errSync) || !strings.Contains(err.Error(), "saving the result") {
		t.Fatalf("err = %v, want the sync error, as a failure to save the result", err)
	}
	var setup *SetupError
	if errors.As(err, &setup) {
		t.Errorf("err = %v is a *SetupError, want it not to be", err)
	}
	if string(out) != fakeOutput {
		t.Errorf("output = %q, want %q", out, fakeOutput)
	}
}
