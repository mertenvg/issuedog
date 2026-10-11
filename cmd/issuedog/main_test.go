package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got, want := stdout.String(), "issuedog dev\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--nope"}, nil, &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunNoArgsLogsError(t *testing.T) {
	var logs, stdout, stderr bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()

	code := run(nil, []string{"ANTHROPIC_API_KEY=key"}, &stdout, &stderr)
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

			code := run(nil, environ, &stdout, &stderr)
			logr.Wait()

			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(logs.String(), "CLAUDE_CODE_OAUTH_TOKEN") {
				t.Errorf("logs = %q, want the credential error", logs.String())
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
