package agent

import (
	"bytes"
	"context"
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

// TestRunSuccess checks the full argument list, with a budget and without.
func TestRunSuccess(t *testing.T) {
	r, s := fakeRun(t, "success", nil)
	s.MaxBudgetUSD = 2.5
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
	}
	if !slices.Equal(got, want) {
		t.Errorf("args = %q, want %q", got, want)
	}
	if a := args("p", Stage{}); slices.Contains(a, "--max-budget-usd") {
		t.Errorf("args without a budget = %q, want no --max-budget-usd", a)
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
	}{
		{name: "non-zero exit", mode: "fail", wantErr: "exit status 1", wantOut: "error_max_turns"},
		{name: "timeout", mode: "hang", wantErr: "deadline exceeded"},
		{name: "timeout with credential", mode: "hang", cred: currentUser(), wantErr: "deadline exceeded"},
		{name: "run ID with a path", mode: "success", runID: "../run-1", wantErr: "invalid run ID"},
		{name: "empty run ID", mode: "success", runID: "", wantErr: "invalid run ID"},
		{name: "no environment", mode: "success", wantErr: "no agent environment"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, s := fakeRun(t, tt.mode, tt.cred)
			if tt.mode == "hang" {
				s.Timeout = 300 * time.Millisecond
			}
			if tt.wantErr == "invalid run ID" {
				s.RunID = tt.runID
			}
			if tt.wantErr == "no agent environment" {
				s.Env = nil
			}
			start := time.Now()
			out, err := r.Run(context.Background(), s)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
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
			if d := time.Since(start); d >= waitDelay {
				t.Errorf("Run took %v; the group kill should close claude's output at once", d)
			}
			waitChildDead(t, s.Dir)
		})
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
// dir/child.pid exits (or is a zombie) soon.
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
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
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
