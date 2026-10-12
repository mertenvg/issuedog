package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/egress"
)

// runLogged runs the program and returns its exit code and what it logged.
func runLogged(t *testing.T, args, environ []string) (int, string) {
	t.Helper()
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()
	code := run(args, environ, nil, io.Discard)
	logr.Wait()
	return code, logs.String()
}

// TestRunPlainOutput checks that the version and help text go to stdout as
// plain text, not through logr, which would add log metadata.
func TestRunPlainOutput(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--version"}, "issuedog dev\n"},
		{[]string{"-h"}, usage + "\n"},
		{[]string{"key", "import", "-h"}, usage + "\n"},
	} {
		var logs, stdout bytes.Buffer
		stop := logr.AddWriter(&logs)
		code := run(tc.args, nil, nil, &stdout)
		logr.Wait()
		stop()
		if code != 0 || stdout.String() != tc.want || logs.Len() != 0 {
			t.Errorf("%q: exit code %d, stdout %q, logs %q; want 0, %q and no logs", tc.args, code, stdout.String(), logs.String(), tc.want)
		}
	}
}

func TestRunUnknownFlag(t *testing.T) {
	code, logs := runLogged(t, []string{"--nope"}, nil)
	if code != 2 || !strings.Contains(logs, "flag provided but not defined: -nope") || !strings.Contains(logs, "usage:") {
		t.Errorf("exit code %d, logs %q; want 2, the flag error and the usage", code, logs)
	}
}

// fakeEgress replaces the startup steps: the firewall returns fwErr, and the
// proxy listens on a free port, or fails with listenErr. It records the
// arguments each step got.
func fakeEgress(t *testing.T, fwErr, listenErr error) *[]string {
	t.Helper()
	var calls []string
	oldApply, oldListen := applyFirewall, listenProxy
	t.Cleanup(func() { applyFirewall, listenProxy = oldApply, oldListen })
	applyFirewall = func(user string) error {
		calls = append(calls, "firewall "+user)
		return fwErr
	}
	listenProxy = func(port int) (net.Listener, error) {
		calls = append(calls, fmt.Sprintf("listen %d", port))
		if listenErr != nil {
			return nil, listenErr
		}
		return egress.Listen(0)
	}
	return &calls
}

func TestRunStartsEgress(t *testing.T) {
	calls := fakeEgress(t, nil, nil)
	code, logs := runLogged(t, nil, []string{"ANTHROPIC_API_KEY=key"})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got, want := strings.Join(*calls, ", "), "firewall agent, listen 3128"; got != want {
		t.Errorf("startup steps = %q, want %q", got, want)
	}
	if !strings.Contains(logs, "egress proxy listening on http://127.0.0.1:") {
		t.Errorf("logs = %q, want the proxy on 127.0.0.1", logs)
	}
}

// TestEgressCommandStopsOnSignal runs the unlisted egress command, which
// sandbox/test.sh relies on, and stops it the way the container does.
func TestEgressCommandStopsOnSignal(t *testing.T) {
	listening := make(chan struct{})
	oldApply, oldListen := applyFirewall, listenProxy
	t.Cleanup(func() { applyFirewall, listenProxy = oldApply, oldListen })
	applyFirewall = func(string) error { return nil }
	listenProxy = func(int) (net.Listener, error) {
		defer close(listening)
		return egress.Listen(0)
	}
	done := make(chan int, 1)
	go func() { done <- command([]string{"egress"}, nil, io.Discard) }()
	<-listening
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("egress didn't stop on SIGTERM")
	}
}

// fakeAgent makes as-agent switch to the test's own uid, which needs no
// privileges, and records how often it looked the user up.
func fakeAgent(t *testing.T, err error) *int {
	t.Helper()
	lookups := 0
	old := agentCredential
	t.Cleanup(func() { agentCredential = old })
	agentCredential = func(user string) (*syscall.Credential, error) {
		lookups++
		if user != "agent" {
			t.Errorf("looked up user %q, want agent", user)
		}
		if err != nil {
			return nil, err
		}
		return &syscall.Credential{Uid: uint32(os.Getuid()), Gid: uint32(os.Getgid()), NoSetGroups: true}, nil
	}
	return &lookups
}

func TestAsAgentArgs(t *testing.T) {
	lookups := fakeAgent(t, nil)
	for _, args := range [][]string{
		{"as-agent"},
		{"as-agent", "--"},
		{"as-agent", "true"},
		{"as-agent", "-x", "true"},
	} {
		code, logs := runLogged(t, args, nil)
		if code != 2 || !strings.Contains(logs, asAgentUsage) {
			t.Errorf("%q: exit code %d, logs %q; want 2 and the usage", args, code, logs)
		}
	}
	if *lookups != 0 {
		t.Errorf("looked agent up %d times, want none for bad arguments", *lookups)
	}
	if strings.Contains(usage, "as-agent") {
		t.Errorf("usage lists as-agent: %q", usage)
	}
}

func TestAsAgentRunsCommand(t *testing.T) {
	fakeAgent(t, nil)
	var logs, stdout bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()
	script := `echo "$HOME $PATH $(pwd)"; echo oops >&2; exit 3`
	code := run([]string{"as-agent", "--", "sh", "-c", script}, nil, nil, &stdout)
	logr.Wait()
	if code != 3 {
		t.Errorf("exit code = %d, want the command's 3", code)
	}
	if want := "/home/agent /usr/local/bin:/usr/bin:/bin /\n"; stdout.String() != want {
		t.Errorf("stdout = %q, want %q", stdout.String(), want)
	}
	if !strings.Contains(logs.String(), "sh as agent wrote to stderr: oops") {
		t.Errorf("logs = %q, want the command's stderr", logs.String())
	}
}

func TestAsAgentFailures(t *testing.T) {
	fakeAgent(t, errors.New("no such user"))
	code, logs := runLogged(t, []string{"as-agent", "--", "true"}, nil)
	if code != 1 || !strings.Contains(logs, "running true as agent: no such user") {
		t.Errorf("exit code %d, logs %q; want 1 and the lookup error", code, logs)
	}
	fakeAgent(t, nil)
	code, logs = runLogged(t, []string{"as-agent", "--", "/no/such/command"}, nil)
	if code != 1 || !strings.Contains(logs, "running /no/such/command as agent: ") {
		t.Errorf("exit code %d, logs %q; want 1 and the start error", code, logs)
	}
}

func TestRunStopsWhenEgressFails(t *testing.T) {
	for name, tt := range map[string]struct {
		fwErr, listenErr error
		wantCalls, want  string
	}{
		"firewall": {errors.New("no NET_ADMIN"), nil, "firewall agent", "starting egress: applying the agent firewall: no NET_ADMIN"},
		"proxy":    {nil, errors.New("port in use"), "firewall agent, listen 3128", "starting egress: listening for the egress proxy: port in use"},
	} {
		t.Run(name, func(t *testing.T) {
			calls := fakeEgress(t, tt.fwErr, tt.listenErr)
			code, logs := runLogged(t, nil, []string{"ANTHROPIC_API_KEY=key"})
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if got := strings.Join(*calls, ", "); got != tt.wantCalls {
				t.Errorf("startup steps = %q, want %q", got, tt.wantCalls)
			}
			if !strings.Contains(logs, tt.want) {
				t.Errorf("logs = %q, want %q", logs, tt.want)
			}
			if strings.Contains(logs, "not implemented") {
				t.Errorf("logs = %q, want startup to stop at egress", logs)
			}
		})
	}
}

func TestRunNoArgsLogsError(t *testing.T) {
	fakeEgress(t, nil, nil)
	code, logs := runLogged(t, nil, []string{"ANTHROPIC_API_KEY=key"})
	if code != 1 || !strings.Contains(logs, "the daemon is not implemented yet") {
		t.Errorf("exit code %d, logs %q; want 1 and the not-implemented error", code, logs)
	}
}

func TestRunRefusesBadCredentials(t *testing.T) {
	for name, environ := range map[string][]string{
		"both set":    {"CLAUDE_CODE_OAUTH_TOKEN=oauth", "ANTHROPIC_API_KEY=key"},
		"neither set": {"GH_TOKEN=gh"},
	} {
		t.Run(name, func(t *testing.T) {
			code, logs := runLogged(t, nil, environ)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(logs, "checking the Claude credential: ") || !strings.Contains(logs, "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Errorf("logs = %q, want the annotated credential error", logs)
			}
			if strings.Contains(logs, "not implemented") {
				t.Errorf("logs = %q, want startup to stop at the credential check", logs)
			}
		})
	}
}

func TestSetupLoggingRoutesByLevel(t *testing.T) {
	var stdout, stderr bytes.Buffer
	stop := setupLogging(&stdout, &stderr)
	defer stop()

	logr.Info("info message")
	logr.Warn("warn message")
	logr.Error("error message")
	logr.Wait()

	for _, msg := range []string{"info message", "warn message", "error message"} {
		if !strings.Contains(stdout.String(), msg) {
			t.Errorf("stdout is missing %q; got %q", msg, stdout.String())
		}
	}
	if !strings.Contains(stderr.String(), "error message") {
		t.Errorf("stderr is missing the error; got %q", stderr.String())
	}
	for _, msg := range []string{"info message", "warn message"} {
		if strings.Contains(stderr.String(), msg) {
			t.Errorf("stderr contains %q; want errors and worse only", msg)
		}
	}
}

func TestRunKeyCommands(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()

	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	path := filepath.Join(t.TempDir(), "app-key.pem")
	for _, tt := range []struct {
		args  []string
		stdin string
		want  int
	}{
		{[]string{"key", "import", "-path", path}, string(key), 0},
		{[]string{"key", "check", "-path", path}, "", 0},
		{[]string{"key", "import", "-path", path}, "not a key", 1},
		{[]string{"key"}, "", 2},
		{[]string{"key", "nope"}, "", 2},
		{[]string{"key", "import", "extra"}, "", 2},
		{[]string{"key", "check", "-nope"}, "", 2},
	} {
		if code := run(tt.args, nil, strings.NewReader(tt.stdin), io.Discard); code != tt.want {
			t.Errorf("%q: exit code = %d, want %d", tt.args, code, tt.want)
		}
	}
	logr.Wait()
	for _, want := range []string{"imported the App key to " + path, path + " is usable", "importing the App key: app key " + path, "unexpected arguments", "usage:"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
	}
}
