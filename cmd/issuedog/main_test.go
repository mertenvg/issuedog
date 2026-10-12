package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/egress"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, nil, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got, want := stdout.String(), "issuedog dev\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--nope"}, nil, nil, &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
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
	var logs, stdout, stderr bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()
	calls := fakeEgress(t, nil, nil)

	code := run(nil, []string{"ANTHROPIC_API_KEY=key"}, nil, &stdout, &stderr)
	logr.Wait()

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if got, want := strings.Join(*calls, ", "), "firewall agent, listen 3128"; got != want {
		t.Errorf("startup steps = %q, want %q", got, want)
	}
	if !strings.Contains(logs.String(), "egress proxy listening on http://127.0.0.1:") {
		t.Errorf("logs = %q, want the proxy on 127.0.0.1", logs.String())
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
			var logs, stdout, stderr bytes.Buffer
			stop := logr.AddWriter(&logs)
			defer stop()
			calls := fakeEgress(t, tt.fwErr, tt.listenErr)

			code := run(nil, []string{"ANTHROPIC_API_KEY=key"}, nil, &stdout, &stderr)
			logr.Wait()

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if got := strings.Join(*calls, ", "); got != tt.wantCalls {
				t.Errorf("startup steps = %q, want %q", got, tt.wantCalls)
			}
			if !strings.Contains(logs.String(), tt.want) {
				t.Errorf("logs = %q, want %q", logs.String(), tt.want)
			}
			if strings.Contains(logs.String(), "not implemented") {
				t.Errorf("logs = %q, want startup to stop at egress", logs.String())
			}
		})
	}
}

func TestRunNoArgsLogsError(t *testing.T) {
	var logs, stdout, stderr bytes.Buffer
	fakeEgress(t, nil, nil)
	stop := logr.AddWriter(&logs)
	defer stop()

	code := run(nil, []string{"ANTHROPIC_API_KEY=key"}, nil, &stdout, &stderr)
	logr.Wait()

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(logs.String(), "the daemon is not implemented yet") {
		t.Errorf("logs = %q, want the not-implemented error", logs.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestRunRefusesBadCredentials(t *testing.T) {
	for name, environ := range map[string][]string{
		"both set":    {"CLAUDE_CODE_OAUTH_TOKEN=oauth", "ANTHROPIC_API_KEY=key"},
		"neither set": {"GH_TOKEN=gh"},
	} {
		t.Run(name, func(t *testing.T) {
			var logs, stdout, stderr bytes.Buffer
			stop := logr.AddWriter(&logs)
			defer stop()

			code := run(nil, environ, nil, &stdout, &stderr)
			logr.Wait()

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(logs.String(), "checking the Claude credential: ") || !strings.Contains(logs.String(), "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Errorf("logs = %q, want the annotated credential error", logs.String())
			}
			if strings.Contains(logs.String(), "not implemented") {
				t.Errorf("logs = %q, want startup to stop at the credential check", logs.String())
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
		var stdout, stderr bytes.Buffer
		if code := run(tt.args, nil, strings.NewReader(tt.stdin), &stdout, &stderr); code != tt.want {
			t.Errorf("%q: exit code = %d, want %d; stderr: %s", tt.args, code, tt.want, stderr.String())
		}
	}
	logr.Wait()
	for _, want := range []string{"imported the App key to " + path, path + " is usable", "importing the App key: app key " + path, "unexpected arguments", "usage:"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
	}
}
