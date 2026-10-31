package verify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/workspace"
)

const testProxy = "http://127.0.0.1:3128"

// currentUser is a Credential tests can switch to without privileges, so a
// command takes the uid switch's code path, as agent's would.
func currentUser() *syscall.Credential {
	return &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), NoSetGroups: true}
}

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

// newCommands makes a bare repo whose main has two commits to f.txt, "one"
// and then "two", and Commands on a workspace of it, as cred, with each
// command's timeout. It returns them with the first commit, the head to
// check, so a checkout of main's tip would show.
func newCommands(t *testing.T, cred *syscall.Credential, timeout time.Duration) (*Commands, string) {
	t.Helper()
	root := t.TempDir()
	src := filepath.Join(root, "src")
	git(t, root, "init", "--quiet", "--initial-branch=main", src)
	var head string
	for _, text := range []string{"one", "two"} {
		if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		git(t, src, "add", "f.txt")
		git(t, src, "commit", "--quiet", "-m", text)
		if head == "" {
			head = git(t, src, "rev-parse", "HEAD")
		}
	}
	bare := filepath.Join(root, "repo.git")
	git(t, root, "clone", "--quiet", "--bare", src, bare)
	w := &workspace.Workspace{Bare: bare, RunsDir: filepath.Join(root, "runs"), ContextDir: filepath.Join(root, "ctx")}
	for _, d := range []string{w.RunsDir, w.ContextDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &Commands{Workspace: w, Credential: cred, Proxy: testProxy, Timeout: func() time.Duration { return timeout }}, head
}

// checkNothingLeft fails the test unless the verify's checkout, HOME and
// context dir are gone, leaving only the agent's git config.
func checkNothingLeft(t *testing.T, c *Commands) {
	t.Helper()
	for dir, want := range map[string][]string{
		c.Workspace.RunsDir:    nil,
		c.Workspace.ContextDir: {".agent.gitconfig"},
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range entries {
			got = append(got, e.Name())
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s holds %q after the verify, want %q", dir, got, want)
		}
	}
}

// TestCommandsRun runs every command, in order, after one fails too, each
// through the shell, in a checkout of the head alone with no remote, with
// an empty HOME and a TMPDIR in it, and removes them all afterwards.
func TestCommandsRun(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cred *syscall.Credential
	}{
		{name: "daemon uid"},
		{name: "with credential", cred: currentUser()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, head := newCommands(t, tt.cred, 10*time.Second)
			home := c.Workspace.Dir("v1-home")
			cmds := []string{
				"true",
				"echo out; echo err >&2; echo out2; exit 3",
				"pwd",
				`ls -A "$HOME"; ls -A "$TMPDIR"; echo "$TMPDIR"`,
				"cat f.txt; git rev-parse HEAD; git remote; git for-each-ref",
				`stat -c %a "$HOME"`,
			}
			got, err := c.Run(context.Background(), "v1", head, cmds)
			if err != nil {
				t.Fatal(err)
			}
			want := []CommandResult{
				{Command: cmds[0], Exit: 0, Timeout: 10 * time.Second, Output: ""},
				{Command: cmds[1], Exit: 3, Timeout: 10 * time.Second, Output: "out\nerr\nout2\n"},
				{Command: cmds[2], Exit: 0, Timeout: 10 * time.Second, Output: c.Workspace.Dir("v1") + "\n"},
				{Command: cmds[3], Exit: 0, Timeout: 10 * time.Second, Output: "tmp\n" + filepath.Join(home, "tmp") + "\n"},
				// The clone holds the head, detached, with no remote and no ref.
				{Command: cmds[4], Exit: 0, Timeout: 10 * time.Second, Output: "one" + head + "\n"},
				{Command: cmds[5], Exit: 0, Timeout: 10 * time.Second, Output: "700\n"},
			}
			if !slices.Equal(got, want) {
				t.Errorf("results:\n%+v\nwant:\n%+v", got, want)
			}
			checkNothingLeft(t, c)
		})
	}
}

// TestCommandsEnvHoldsNoCredential gives each command only the proxy, a
// fixed PATH, its fresh HOME and TMPDIR: no Claude or GitHub credential the
// daemon holds reaches it. It sets the daemon's environment, so it can't
// run in parallel.
func TestCommandsEnvHoldsNoCredential(t *testing.T) {
	for _, kv := range []string{"CLAUDE_CODE_OAUTH_TOKEN=oauth", "ANTHROPIC_API_KEY=key", "GH_TOKEN=gh", "GITHUB_TOKEN=gh", "ISSUEDOG_SECRET=s"} {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	c, head := newCommands(t, nil, 10*time.Second)
	got, err := c.Run(context.Background(), "v1", head, []string{"env"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Exit != 0 {
		t.Fatalf("results = %+v, want env to pass", got)
	}
	home := c.Workspace.Dir("v1-home")
	vars := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(got[0].Output), "\n") {
		k, v, _ := strings.Cut(line, "=")
		vars[k] = v
	}
	for k, want := range map[string]string{
		"HTTPS_PROXY": testProxy,
		"HTTP_PROXY":  testProxy,
		"HOME":        home,
		"TMPDIR":      filepath.Join(home, "tmp"),
		"PATH":        "/usr/local/bin:/usr/bin:/bin",
	} {
		if vars[k] != want {
			t.Errorf("%s = %q, want %q", k, vars[k], want)
		}
	}
	for k := range vars {
		// The shell sets PWD and may set SHLVL and OLDPWD itself.
		switch k {
		case "HTTPS_PROXY", "HTTP_PROXY", "HOME", "TMPDIR", "PATH", "PWD", "SHLVL", "OLDPWD", "_":
		default:
			t.Errorf("the command's environment holds %s, which nothing gives it", k)
		}
	}
}

// waitDead fails the test unless pid exits (or is a zombie) within a
// minute, which is generous so a loaded machine doesn't fail it. Each pid a
// test waits on sleeps 60s, so one that wasn't killed still fails.
func waitDead(t *testing.T, pid int) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			return
		}
		if i := bytes.LastIndexByte(stat, ')'); i > 0 && len(stat) > i+2 && (stat[i+2] == 'Z' || stat[i+2] == 'X') {
			return
		}
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Errorf("killing %d: %v", pid, err)
	}
	t.Errorf("process %d outlived the verify", pid)
}

// pidIn is the pid a command printed on the first line of its output.
func pidIn(t *testing.T, r CommandResult) int {
	t.Helper()
	line, _, _ := strings.Cut(r.Output, "\n")
	pid, err := strconv.Atoi(line)
	if err != nil {
		t.Fatalf("%q printed no pid first: %q", r.Command, r.Output)
	}
	return pid
}

// TestCommandsTimeoutKillsGroup kills a command past its timeout with its
// whole group, so a child that ignores SIGTERM and holds the output open
// dies with it, and fails it, naming the timeout. The next command still
// runs.
func TestCommandsTimeoutKillsGroup(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cred *syscall.Credential
	}{
		{name: "daemon uid"},
		{name: "with credential", cred: currentUser()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, head := newCommands(t, tt.cred, time.Second)
			// The wait for the output is an hour, so only the group kill ends
			// the command within the bound below.
			c.waitDelay = time.Hour
			cmds := []string{`sh -c 'trap "" TERM; sleep 60' & echo $!; wait`, "echo after"}
			start := time.Now()
			got, err := c.Run(context.Background(), "v1", head, cmds)
			if err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d >= time.Minute {
				t.Errorf("the verify took %v; the group kill should end the command at its timeout", d)
			}
			if len(got) != 2 {
				t.Fatalf("results = %+v, want 2", got)
			}
			waitDead(t, pidIn(t, got[0]))
			if !got[0].TimedOut || got[0].Exit != -1 || got[0].Failure() != "timed out after 1s" {
				t.Errorf("result = %+v, failure %q; want it timed out after 1s, ended by a signal", got[0], got[0].Failure())
			}
			if got[1].TimedOut || got[1].Exit != 0 || got[1].Output != "after\n" {
				t.Errorf("the command after the timeout = %+v, want it run", got[1])
			}
			checkNothingLeft(t, c)
		})
	}
}

// TestCommandsKillLeftovers kills what a passing command left running in
// the background before the next command starts.
func TestCommandsKillLeftovers(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		cred *syscall.Credential
	}{
		{name: "daemon uid"},
		{name: "with credential", cred: currentUser()},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, head := newCommands(t, tt.cred, 10*time.Second)
			// The second command reads the first's pid from the HOME they
			// share, and sees it dead, or a zombie no one reaped, within 5s,
			// as SIGKILL takes a moment to land. A leftover sleeps 60s.
			cmds := []string{
				`sleep 60 >/dev/null 2>&1 & echo $!; echo $! >"$HOME/pid"`,
				`stat=/proc/$(cat "$HOME/pid")/stat; i=0
				while [ $i -lt 100 ]; do
					case "$(cut -d' ' -f3 "$stat" 2>/dev/null)" in ""|Z|X) echo dead; exit 0;; esac
					sleep 0.05; i=$((i+1))
				done
				echo alive`,
			}
			got, err := c.Run(context.Background(), "v1", head, cmds)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].Exit != 0 {
				t.Fatalf("results = %+v, want the first to pass", got)
			}
			waitDead(t, pidIn(t, got[0]))
			if got[1].Output != "dead\n" {
				t.Errorf("the next command found the leftover %q, want it dead", got[1].Output)
			}
			checkNothingLeft(t, c)
		})
	}
}

// TestCommandsOutputKeepsTheEnd keeps a failing command's exit status and
// the last 1,000 characters of its output.
func TestCommandsOutputKeepsTheEnd(t *testing.T) {
	t.Parallel()
	c, head := newCommands(t, nil, 10*time.Second)
	cmd := `i=0; while [ $i -lt 600 ]; do printf '%03d\n' $i; i=$((i+1)); done; exit 2`
	got, err := c.Run(context.Background(), "v1", head, []string{cmd})
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for i := 350; i < 600; i++ {
		want.WriteString(strconv.Itoa(i) + "\n")
	}
	if len(got) != 1 || got[0].Exit != 2 || got[0].Output != want.String() || got[0].Failure() != "exit status 2" {
		t.Errorf("results = %+v, want exit status 2 and the last 250 lines", got)
	}
}

// TestCommandsSetupFailures fails a verify whose checkout or HOME can't be
// made, with no results, running no command, and leaves nothing behind.
func TestCommandsSetupFailures(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, wantErr string
		setup         func(t *testing.T, c *Commands) string // gives the head
	}{
		{
			name:    "head not in the repo",
			wantErr: "clone",
			setup: func(t *testing.T, c *Commands) string {
				return strings.Repeat("ab", 20)
			},
		},
		{
			name:    "home there already",
			wantErr: "prepare home v1-home",
			setup: func(t *testing.T, c *Commands) string {
				if err := os.Mkdir(c.Workspace.Dir("v1-home"), 0o700); err != nil {
					t.Fatal(err)
				}
				return ""
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c, head := newCommands(t, nil, 10*time.Second)
			if h := tt.setup(t, c); h != "" {
				head = h
			}
			marker := filepath.Join(t.TempDir(), "ran")
			got, err := c.Run(context.Background(), "v1", head, []string{"touch " + marker})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) || got != nil {
				t.Fatalf("Run = %+v, %v; want no results and an error containing %q", got, err, tt.wantErr)
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a command ran: %v", err)
			}
			entries, err := os.ReadDir(c.Workspace.RunsDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Errorf("the runs dir holds %v, want nothing", entries)
			}
		})
	}
}

// TestCommandsCallerCancel ends a verify whose caller's ctx ends with an
// error, not a result, kills the command, and still removes its dirs.
func TestCommandsCallerCancel(t *testing.T) {
	t.Parallel()
	c, head := newCommands(t, nil, time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pidFile := filepath.Join(t.TempDir(), "pid")
	go func() {
		// Cancel once the command has started.
		for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if raw, err := os.ReadFile(pidFile); err == nil && bytes.HasSuffix(raw, []byte("\n")) {
				break
			}
		}
		cancel()
	}()
	got, err := c.Run(ctx, "v1", head, []string{"sleep 60 & echo $! >" + pidFile + "; wait"})
	if !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("Run = %+v, %v; want no results and the caller's cancellation", got, err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	waitDead(t, pid)
	checkNothingLeft(t, c)
}

func TestCommandResultFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		r    CommandResult
		want string
	}{
		{CommandResult{Exit: 0, Timeout: time.Second}, ""},
		{CommandResult{Exit: 3}, "exit status 3"},
		{CommandResult{Exit: -1, TimedOut: true, Timeout: 20 * time.Minute}, "timed out after 20m0s"},
	} {
		if got := tt.r.Failure(); got != tt.want {
			t.Errorf("%+v.Failure() = %q, want %q", tt.r, got, tt.want)
		}
	}
}

func TestLastChars(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		s    string
		n    int
		want string
	}{
		{"", 3, ""},
		{"ab", 3, "ab"},
		{"abc", 3, "abc"},
		{"abcd", 3, "bcd"},
		{"aéü€", 3, "éü€"},
		{"€€€€", 2, "€€"},
	} {
		if got := lastChars(tt.s, tt.n); got != tt.want {
			t.Errorf("lastChars(%q, %d) = %q, want %q", tt.s, tt.n, got, tt.want)
		}
	}
}
