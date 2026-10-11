package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"
)

func TestRunVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if got, want := stdout.String(), "issuedog dev\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRunUnknownFlag(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestRunNoArgsLogsError(t *testing.T) {
	var logs, stdout, stderr bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()

	code := run(nil, &stdout, &stderr)
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
