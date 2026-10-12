package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"
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

func TestRunNoArgsLogsError(t *testing.T) {
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
