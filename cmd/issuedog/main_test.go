package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/egress"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
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

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stdout closed") }

func TestRunPlainOutputFails(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
	code := run([]string{"--version"}, nil, nil, failingWriter{})
	logr.Wait()
	stop()
	if code != 1 || !strings.Contains(logs.String(), "writing to stdout: stdout closed") {
		t.Errorf("exit code %d, logs %q; want 1 and the write error", code, logs.String())
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

// fakeAppKey points the daemon at a key file with mode perm in a temp dir,
// and returns its path. A zero perm leaves the file missing.
func fakeAppKey(t *testing.T, perm os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "app-key.pem")
	oldPath := appKeyPath
	t.Cleanup(func() { appKeyPath = oldPath })
	appKeyPath = path
	if perm == 0 {
		return path
	}
	if err := os.WriteFile(path, []byte("test key"), perm); err != nil {
		t.Fatal(err)
	}
	// Chmod as well, so the umask can't change the mode under test.
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
	return path
}

// daemonEnv is the environment the daemon starts with.
var daemonEnv = []string{"ANTHROPIC_API_KEY=key", "ISSUEDOG_REPO=mertenvg/issuedog",
	"ISSUEDOG_APP_ID=5275196", "ISSUEDOG_INSTALLATION_ID=170308396"}

// fakeGitHub points the daemon at a fake GitHub for mertenvg/issuedog, with
// mertenvg an admin and mallory a writer, and at a local bare repo as the
// remote, whose main has files (path to content). It writes a usable App key.
func fakeGitHub(t *testing.T, files map[string]string) *githubtest.Fake {
	t.Helper()
	f := githubtest.New(t, "mertenvg/issuedog", "issuedog-dev")
	f.SetPermission("mertenvg", "admin")
	f.SetPermission("mallory", "write")
	dir := t.TempDir()
	remote, work := filepath.Join(dir, "remote.git"), filepath.Join(dir, "work")
	gitIn(t, dir, "init", "--bare", "--quiet", "--initial-branch=main", remote)
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main", work)
	files["README.md"] = "hello\n"
	for path, content := range files {
		if err := os.MkdirAll(filepath.Join(work, filepath.Dir(path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(work, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, work, "add", path)
	}
	gitIn(t, work, "commit", "--quiet", "-m", "init")
	gitIn(t, work, "push", "--quiet", remote, "main")

	oldAPI, oldRemote, oldWork := apiURL, gitRemote, workDir
	t.Cleanup(func() { apiURL, gitRemote, workDir = oldAPI, oldRemote, oldWork })
	apiURL, workDir = f.URL(), t.TempDir()
	gitRemote = func(owner, name string) string {
		if owner+"/"+name != "mertenvg/issuedog" {
			t.Errorf("remote of %s/%s, want mertenvg/issuedog", owner, name)
		}
		return remote
	}
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := fakeAppKey(t, 0)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") }),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
}

// thisRepo is this repo's own config, with example.com added to
// network.allow, and CODEOWNERS.
func thisRepo(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, path := range []string{".issuedog/config.yaml", ".github/CODEOWNERS"} {
		data, err := os.ReadFile(filepath.Join("..", "..", path))
		if err != nil {
			t.Fatal(err)
		}
		files[path] = string(data)
	}
	const allow = "allow: [api.anthropic.com,"
	if !strings.Contains(files[".issuedog/config.yaml"], allow) {
		t.Fatalf("this repo's config has no %q", allow)
	}
	files[".issuedog/config.yaml"] = strings.Replace(files[".issuedog/config.yaml"], allow, "allow: [example.com, api.anthropic.com,", 1)
	return files
}

// cancelOnCall makes the daemon's signal context one the returned func
// cancels, and records the signals it was asked to stop on.
func cancelOnCall(t *testing.T) (cancel func(), signals *[]os.Signal) {
	var sigs []os.Signal
	ctx, stop := context.WithCancel(context.Background())
	old := notifyContext
	t.Cleanup(func() { notifyContext = old; stop() })
	notifyContext = func(parent context.Context, s ...os.Signal) (context.Context, context.CancelFunc) {
		sigs = s
		return ctx, func() {}
	}
	return stop, &sigs
}

func TestRunDaemon(t *testing.T) {
	f := fakeGitHub(t, thisRepo(t))
	f.AddRepoLabel("stage:intake")
	n := f.AddIssue("mertenvg", "Task", "do it")
	f.AddLabel(n, "issuedog-dev[bot]", "stage:ready")
	f.AddComment(n, "issuedog-dev[bot]", "<!-- issuedog:transition from=intake to=ready run=a -->")
	f.AddComment(n, "mallory", "/frob")
	f.AddComment(n, "mertenvg", "/frob")
	calls := fakeEgress(t, nil, nil)
	cancel, signals := cancelOnCall(t)
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()
	done := make(chan int, 1)
	go func() { done <- run(nil, daemonEnv, nil, io.Discard) }()

	// The first iteration rejects mertenvg's /frob, as CODEOWNERS on main
	// trusts only mertenvg, then sleeps the config's 60s.
	for deadline := time.Now().Add(10 * time.Second); len(f.State().Issues[0].Comments) < 4 && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon didn't stop when its context was canceled")
	}
	logr.Wait()

	if !reflect.DeepEqual(*signals, []os.Signal{os.Interrupt, syscall.SIGTERM}) {
		t.Errorf("stops on %v, want SIGINT and SIGTERM", *signals)
	}
	reqs := f.Requests()
	for _, want := range []string{"GET /app", "POST /app/installations/170308396/access_tokens", "GET /labels", "GET /issues"} {
		if !slices.ContainsFunc(reqs, func(r githubtest.Request) bool { return r.Method+" "+r.Path == want }) {
			t.Errorf("requests = %+v, want %s", reqs, want)
		}
	}
	gitConfig, err := os.ReadFile(filepath.Join(workDir, "issuedog.git", "config"))
	if err != nil {
		t.Errorf("no clone at %s/issuedog.git: %v", workDir, err)
	}
	// The fake mints "fake-token"; git gets it base64-encoded in a header.
	for _, secret := range []string{"fake-token", base64.StdEncoding.EncodeToString([]byte("x-access-token:fake-token"))} {
		if strings.Contains(logs.String(), secret) || strings.Contains(string(gitConfig), secret) {
			t.Errorf("the token is in the logs or the clone's config")
		}
	}
	labels := f.RepoLabels()
	for _, want := range []string{"stage:spec-review", "scope:daemon", "scope:docs", "type:task"} {
		if !slices.Contains(labels, want) {
			t.Errorf("repo labels = %v, want %s from this repo's config", labels, want)
		}
	}
	if len(slices.DeleteFunc(slices.Clone(labels), func(l string) bool { return l != "stage:intake" })) != 1 {
		t.Errorf("repo labels = %v, want stage:intake, which existed, once", labels)
	}
	comments := f.State().Issues[0].Comments
	if len(comments) != 4 || len(comments[1].Reactions) != 0 || !reflect.DeepEqual(comments[2].Reactions, []string{"issuedog-dev[bot] -1"}) {
		t.Errorf("comments = %+v, want mertenvg's /frob alone 👎'd and replied to", comments)
	}
	if got, want := strings.Join(*calls, ", "), "firewall agent, listen 3128"; got != want {
		t.Errorf("startup steps = %q, want %q", got, want)
	}
	for _, want := range []string{"egress proxy listening on http://127.0.0.1:", "allowing example.com, api.anthropic.com",
		"running as issuedog-dev[bot] on mertenvg/issuedog", "stopping on a signal"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
	}
}

func TestRunStartupFailures(t *testing.T) {
	without := func(key string) []string {
		return slices.DeleteFunc(slices.Clone(daemonEnv), func(kv string) bool { return strings.HasPrefix(kv, key+"=") })
	}
	for name, tt := range map[string]struct {
		environ   []string
		setup     func(t *testing.T) // after fakeGitHub
		files     map[string]string  // main's, else this repo's
		failWrite int                // the write to fail
		want      string
	}{
		"no repo":            {environ: without("ISSUEDOG_REPO"), want: `reading the settings: ISSUEDOG_REPO is "", not owner/name`},
		"bad repo":           {environ: append(without("ISSUEDOG_REPO"), "ISSUEDOG_REPO=mertenvg"), want: `ISSUEDOG_REPO is "mertenvg", not owner/name`},
		"bad app ID":         {environ: append(without("ISSUEDOG_APP_ID"), "ISSUEDOG_APP_ID=abc"), want: `reading the settings: ISSUEDOG_APP_ID is "abc", not a positive number`},
		"no installation ID": {environ: without("ISSUEDOG_INSTALLATION_ID"), want: `ISSUEDOG_INSTALLATION_ID is "", not a positive number`},
		"clone": {setup: func(t *testing.T) {
			gitRemote = func(string, string) string { return filepath.Join(t.TempDir(), "none.git") }
		},
			want: "starting the daemon: syncing the clone: fetch "},
		"config": {files: map[string]string{}, want: "starting the daemon: loading the config from main: load config: read main:.issuedog/config.yaml: not found"},
		"labels": {failWrite: 1, want: "starting the daemon: creating the labels: create label stage:intake"},
	} {
		t.Run(name, func(t *testing.T) {
			files := tt.files
			if files == nil {
				files = thisRepo(t)
			}
			f := fakeGitHub(t, files)
			f.FailWrite(tt.failWrite)
			if tt.setup != nil {
				tt.setup(t)
			}
			calls := fakeEgress(t, nil, nil)
			environ := tt.environ
			if environ == nil {
				environ = daemonEnv
			}
			code, logs := runLogged(t, nil, environ)
			if code != 1 || !strings.Contains(logs, tt.want) {
				t.Errorf("exit code %d, logs %q; want 1 and %q", code, logs, tt.want)
			}
			if len(*calls) != 0 {
				t.Errorf("startup steps = %q, want none of egress", *calls)
			}
		})
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

func TestRunRefusesBadAppKey(t *testing.T) {
	for name, tt := range map[string]struct {
		perm os.FileMode
		want string
	}{
		"missing":   {0, "no such file or directory"},
		"mode 0640": {0o640, "mode 0640 gives access to group or others"},
	} {
		t.Run(name, func(t *testing.T) {
			path := fakeAppKey(t, tt.perm)
			calls := fakeEgress(t, nil, nil)
			code, logs := runLogged(t, nil, daemonEnv)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if !strings.Contains(logs, "loading the App key: ") || !strings.Contains(logs, path) || !strings.Contains(logs, tt.want) {
				t.Errorf("logs = %q, want the annotated key error naming %s and %q", logs, path, tt.want)
			}
			if len(*calls) != 0 || strings.Contains(logs, "egress proxy listening") {
				t.Errorf("startup steps = %q, logs %q; want startup to stop before the firewall and proxy", *calls, logs)
			}
		})
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
			f := fakeGitHub(t, thisRepo(t))
			calls := fakeEgress(t, tt.fwErr, tt.listenErr)
			code, logs := runLogged(t, nil, daemonEnv)
			if code != 1 {
				t.Errorf("exit code = %d, want 1", code)
			}
			if got := strings.Join(*calls, ", "); got != tt.wantCalls {
				t.Errorf("startup steps = %q, want %q", got, tt.wantCalls)
			}
			if !strings.Contains(logs, tt.want) {
				t.Errorf("logs = %q, want %q", logs, tt.want)
			}
			if slices.ContainsFunc(f.Requests(), func(r githubtest.Request) bool { return r.Path == "/issues" }) {
				t.Errorf("requests = %+v, want startup to stop at egress, before the loop", f.Requests())
			}
		})
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
			if strings.Contains(logs, "reading the settings") {
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
