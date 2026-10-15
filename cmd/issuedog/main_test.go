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
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
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

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/egress"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
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
// Runs would be the test's own uid's, with state in a temp dir.
func fakeGitHub(t *testing.T, files map[string]string) *githubtest.Fake {
	t.Helper()
	fakeAgent(t, nil)
	oldState, oldRun := stateDir, runDir
	t.Cleanup(func() { stateDir, runDir = oldState, oldRun })
	stateDir, runDir = t.TempDir(), t.TempDir()
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

// revisionOf is the App's comment for revision 1 of body, with its move to
// spec-review.
func revisionOf(body string) string {
	h := state.BodyHash(body)
	return "<!-- issuedog:transition from=clarifying to=spec-review rev=1 body=" + h + " run=r1 -->\n" +
		"<!-- issuedog:revision rev=1 body=" + h + " -->"
}

// trustSet trusts the logins set true.
type trustSet map[string]bool

func (ts trustSet) Trusted(_ context.Context, login string) (bool, error) { return ts[login], nil }

// The loop's reloads replace the step's Trust, and the 👍 trigger must use
// the trust of the moment, not the one newStep got.
func TestNewStepUsesCurrentTrust(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog-dev")
	n := f.AddIssue("owner", "Spec", "body")
	f.SetType(n, "Story")
	f.AddLabel(n, "issuedog-dev[bot]", "stage:spec-review")
	f.AddReaction(f.AddComment(n, "issuedog-dev[bot]", revisionOf("body")), "owner2", "+1")
	c := f.Client(t)
	st := newStep(&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog-dev"}, trustSet{"owner2": true}, noModels)
	step := func() []string {
		t.Helper()
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Run(context.Background(), s); err != nil {
			t.Fatal(err)
		}
		return f.State().Issues[0].Labels
	}
	st.Trust = trustSet{} // a reload: owner2 left CODEOWNERS
	if got := step(); !slices.Equal(got, []string{"stage:spec-review"}) {
		t.Errorf("labels = %v, want stage:spec-review: owner2 isn't trusted any more", got)
	}
	st.Trust = trustSet{"owner2": true}
	if got := step(); !slices.Equal(got, []string{"stage:planning"}) {
		t.Errorf("labels = %v, want stage:planning once owner2 is trusted again", got)
	}
}

// newStep has every command handler, and /hold checks a child's labels with
// the trust of the moment: owner2's hand-set stage counts once a reload
// trusts owner2.
func TestNewStepCommands(t *testing.T) {
	const app = "issuedog-dev[bot]"
	f := githubtest.New(t, "o/r", "issuedog-dev")
	for n := 1; n <= 2; n++ {
		f.AddIssue("owner", "Task", "body")
		f.SetType(n, "Task")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
	}
	f.SetParent(2, 1)
	f.RemoveLabel(2, "owner2", "stage:ready")
	f.AddLabel(2, "owner2", "stage:planned")
	f.AddComment(1, "owner", "/hold")
	c := f.Client(t)
	st := newStep(&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog-dev"}, trustSet{}, noModels)
	for _, name := range []string{"approve", "hold", "resume", "priority", "model", "effort", "track", "ignore"} {
		if _, ok := st.Commands[name]; !ok {
			t.Errorf("newStep has no /%s handler", name)
		}
	}
	st.Trust = trustSet{"owner": true, "owner2": true} // a reload
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	child := f.State().Issues[1].Comments
	if last := child[len(child)-1].Body; !strings.Contains(last, "to=held at=planned") {
		t.Errorf("#2's last comment = %q, want its hold from owner2's planned", last)
	}
}

// newStep's reaction triggers both run: in one Step, a 👍 on a revision
// approves it, and a 👍 on a flag admits its issue. A failing trigger's
// error comes back wrapped.
func TestNewStepFlagAnswers(t *testing.T) {
	const app = "issuedog-dev[bot]"
	f := githubtest.New(t, "o/r", "issuedog-dev")
	spec := f.AddIssue("owner", "Spec", "body")
	f.SetType(spec, "Story")
	f.AddLabel(spec, app, "stage:spec-review")
	f.AddReaction(f.AddComment(spec, app, revisionOf("body")), "owner", "+1")
	flagged := f.AddIssue("eve", "Idea", "body")
	f.AddLabel(flagged, app, "triage:ask")
	flag := f.AddComment(flagged, app, "Flag.\n\n<!-- issuedog:triage verdict=ask run=t1 recommend=handle -->")
	f.AddReaction(flag, "owner", "+1")
	c := f.Client(t)
	st := newStep(&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog-dev"}, trustSet{"owner": true}, noModels)
	for _, n := range []int{spec, flagged} {
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Run(context.Background(), s); err != nil {
			t.Fatal(err)
		}
	}
	for i, want := range []string{"stage:planning", "stage:clarifying"} {
		if got := f.State().Issues[i].Labels; !slices.Contains(got, want) {
			t.Errorf("#%d's labels = %v, want %s", i+1, got, want)
		}
	}
	// Each trigger's error: its comment is gone after the read, so its
	// reactions can't be listed.
	spec2 := f.AddIssue("owner", "Spec", "body")
	f.SetType(spec2, "Story")
	f.AddLabel(spec2, app, "stage:spec-review")
	revision := f.AddComment(spec2, app, revisionOf("body"))
	flagged2 := f.AddIssue("eve", "Idea", "body")
	flag2 := f.AddComment(flagged2, app, "Flag.\n\n<!-- issuedog:triage verdict=ask run=t2 recommend=handle -->")
	for _, tc := range []struct {
		issue   int
		comment int64
		want    string
	}{{spec2, revision, "approval by 👍: "}, {flagged2, flag2, "flag answer by 👍: "}} {
		s, err := c.Snapshot(context.Background(), "o", "r", tc.issue)
		if err != nil {
			t.Fatal(err)
		}
		f.DeleteComment(tc.comment)
		if err := st.Reactions(context.Background(), st.Applier, s, daemon.Dirt{}); err == nil || !strings.HasPrefix(err.Error(), tc.want) {
			t.Errorf("#%d: Reactions = %v, want an error starting %q", tc.issue, err, tc.want)
		}
	}
}

// newStep's state error trigger runs too: a trusted 👍 on a state error
// gets its ack. Its error comes back wrapped.
func TestNewStepErrAck(t *testing.T) {
	const app = "issuedog-dev[bot]"
	const errorText = "Disagreed.\n\n<!-- issuedog:error kind=disagreement key=9 -->"
	f := githubtest.New(t, "o/r", "issuedog-dev")
	for range 2 {
		n := f.AddIssue("owner", "Task", "body")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
	}
	r := f.AddReaction(f.AddComment(1, app, errorText), "owner", "+1")
	gone := f.AddComment(2, app, errorText)
	c := f.Client(t)
	st := newStep(&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog-dev"}, trustSet{"owner": true}, noModels)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("Acknowledged, by `owner`'s 👍.\n\n<!-- issuedog:error kind=disagreement key=9 ack=r%d -->", r)
	if got := f.State().Issues[0].Comments; len(got) != 3 || got[2].Body != want {
		t.Errorf("#1's comments = %+v; want the ack last: %q", got, want)
	}
	// Its error: the error comment is gone after the read, so its
	// reactions can't be listed.
	if s, err = c.Snapshot(context.Background(), "o", "r", 2); err != nil {
		t.Fatal(err)
	}
	f.DeleteComment(gone)
	if err := st.Reactions(context.Background(), st.Applier, s, daemon.Dirt{}); err == nil || !strings.HasPrefix(err.Error(), "state error ack by 👍: ") {
		t.Errorf("Reactions = %v, want an error starting %q", err, "state error ack by 👍: ")
	}
}

// noModels is a models.allowed that's empty.
func noModels() config.Models { return config.Models{} }

// /model asks newStep's models func on each call, not once: a reload that
// allows opus lets /model opus through.
func TestNewStepUsesCurrentModels(t *testing.T) {
	const app = "issuedog-dev[bot]"
	f := githubtest.New(t, "o/r", "issuedog-dev")
	n := f.AddIssue("owner", "Task", "body")
	f.SetType(n, "Task")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
	f.AddComment(n, "owner", "/model opus")
	c := f.Client(t)
	var allowed config.Models
	st := newStep(&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog-dev"}, trustSet{"owner": true},
		func() config.Models { return allowed })
	allowed = config.Models{Allowed: []config.Model{{Name: "opus"}}} // a reload
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if got := f.State().Issues[0].Labels; !slices.Equal(got, []string{"model:opus", "stage:ready"}) {
		t.Errorf("labels = %v, want model:opus, allowed since the reload", got)
	}
}

// /model tells a begun Apply's add from the label rules' revert with the
// trust of the moment, not the one newStep got. owner2 removed
// model:opus5, then the owner posted /model claude-opus-5-5, whose Apply
// added it back before a reload dropped the model. Once a reload trusts
// owner2, that removal was accepted, so the add restores nothing: it's the
// begun Apply's, which finishes.
func TestNewStepSettingsUseCurrentTrust(t *testing.T) {
	const app = "issuedog-dev[bot]"
	f := githubtest.New(t, "o/r", "issuedog-dev")
	n := f.AddIssue("owner", "Task", "body")
	f.SetType(n, "Task")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
	f.AddLabel(n, app, "model:opus5")
	f.AddLabel(n, app, "model:sonnet")
	f.RemoveLabel(n, "owner2", "model:opus5")
	f.AddComment(n, "owner", "/model claude-opus-5-5")
	f.AddLabel(n, app, "model:opus5")
	c := f.Client(t)
	st := newStep(&daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog-dev"}, trustSet{"owner": true},
		func() config.Models { return config.Models{Allowed: []config.Model{{Name: "sonnet"}}} })
	st.Trust = trustSet{"owner": true, "owner2": true} // a reload
	s, err := c.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Run(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"model:opus5", "stage:ready"}) {
		t.Errorf("labels = %v, want model:opus5 alone", is.Labels)
	}
	if got := is.Comments[1].Reactions; !slices.Equal(got, []string{app + " +1"}) {
		t.Errorf("the command's reactions = %q, want the App's 👍", got)
	}
}

// startDaemon wires /model to the daemon's config of the moment: the
// startup config's models.allowed applies, then a reload's from main.
func TestStartDaemonUsesCurrentModels(t *testing.T) {
	const app = "issuedog-dev[bot]"
	files := thisRepo(t)
	files[".issuedog/config.yaml"] += "\nmodels:\n  allowed: [opus]\n"
	f := fakeGitHub(t, files)
	n := f.AddIssue("mertenvg", "Task", "do it")
	f.AddLabel(n, app, "stage:ready")
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=a -->")
	f.AddComment(n, "mertenvg", "/model opus")
	key, err := os.ReadFile(appKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d, err := startDaemon(ctx, settings{"mertenvg", "issuedog", 1, 1}, key)
	if err != nil {
		t.Fatal(err)
	}
	iterate := func(want []string) {
		t.Helper()
		if err := d.Iterate(ctx); err != nil {
			t.Fatal(err)
		}
		if got := f.State().Issues[0].Labels; !slices.Equal(got, want) {
			t.Errorf("labels = %v, want %v", got, want)
		}
	}
	iterate([]string{"model:opus", "stage:ready"})

	dir := t.TempDir()
	work := filepath.Join(dir, "work")
	gitIn(t, dir, "clone", "--quiet", gitRemote("mertenvg", "issuedog"), work)
	path := filepath.Join(work, ".issuedog", "config.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(files[".issuedog/config.yaml"], "[opus]", "[opus, sonnet]", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, work, "commit", "--quiet", "-am", "allow sonnet")
	gitIn(t, work, "push", "--quiet", "origin", "main")
	f.AddComment(n, "mertenvg", "/model sonnet")
	iterate([]string{"model:sonnet", "stage:ready"})
}

// newStep handles the phase 1 spec commands, so one left out fails here, not
// through TestRunDaemon's timeout.
func TestNewStepHandlesSpecCommands(t *testing.T) {
	st := newStep(&daemon.Applier{}, trustSet{}, noModels)
	for _, name := range []string{"approve", "revise"} {
		if h, ok := st.Commands[name]; !ok || h.Check == nil || h.Apply == nil {
			t.Errorf("Commands[%q] = %+v, %t; want a handler with Check and Apply", name, h, ok)
		}
	}
	if st.Reactions == nil {
		t.Error("Reactions is nil, want the 👍 trigger")
	}
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
	// Specs approved by mertenvg's 👍 on the revision, and by /approve.
	for _, approve := range []func(m int, rev int64){
		func(_ int, rev int64) { f.AddReaction(rev, "mertenvg", "+1") },
		func(m int, _ int64) { f.AddComment(m, "mertenvg", "/approve") },
	} {
		m := f.AddIssue("mertenvg", "Spec", "body")
		f.SetType(m, "Story")
		f.AddLabel(m, "issuedog-dev[bot]", "stage:spec-review")
		approve(m, f.AddComment(m, "issuedog-dev[bot]", revisionOf("body")))
	}
	// A lease for a stage with no Stage registered, planning, which stage
	// work drops. clarifying has the interviewer, and intake triage.
	lease := filepath.Join(stateDir, "lease.json")
	if err := os.WriteFile(lease, []byte(`{"issue":1,"stage":"planning","run_id":"r1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := fakeEgress(t, nil, nil)
	cancel, signals := cancelOnCall(t)
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()
	done := make(chan int, 1)
	go func() { done <- run(nil, daemonEnv, nil, io.Discard) }()

	// The first iteration rejects mertenvg's /frob, as CODEOWNERS on main
	// trusts only mertenvg, approves both specs, and drops the lease, then
	// sleeps the config's 60s.
	planning := []string{"stage:planning"}
	iterated := func() bool {
		is := f.State().Issues
		_, err := os.Stat(lease)
		return len(is[0].Comments) >= 4 && slices.Equal(is[1].Labels, planning) && slices.Equal(is[2].Labels, planning) && err != nil
	}
	for deadline := time.Now().Add(10 * time.Second); !iterated() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !iterated() {
		t.Error("the first iteration didn't finish in 10s")
	}
	if _, err := os.Stat(lease); !os.IsNotExist(err) {
		t.Errorf("the lease is still there: %v", err)
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
	for i, is := range f.State().Issues[1:] {
		if !slices.Equal(is.Labels, planning) {
			t.Errorf("spec %d labels = %v, want %v: approved by %s", i+2, is.Labels, planning, []string{"👍", "/approve"}[i])
		}
	}
	comments := f.State().Issues[0].Comments
	if len(comments) != 4 || len(comments[1].Reactions) != 0 || !reflect.DeepEqual(comments[2].Reactions, []string{"issuedog-dev[bot] -1"}) {
		t.Errorf("comments = %+v, want mertenvg's /frob alone 👎'd and replied to", comments)
	}
	if got, want := strings.Join(*calls, ", "), "firewall agent, listen 3128"; got != want {
		t.Errorf("startup steps = %q, want %q", got, want)
	}
	for _, want := range []string{"egress proxy listening on http://127.0.0.1:", "allowing example.com, api.anthropic.com",
		"running as issuedog-dev[bot] on mertenvg/issuedog", "stopping on a signal",
		"dropping the lease of run r1 on #1: no Stage is registered for planning"} {
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
		files     func(t *testing.T) map[string]string // main's, else this repo's
		setup     func(t *testing.T, f *githubtest.Fake)
		failWrite int // the write to fail
		code      int
		want      string
	}{
		"no repo": {environ: without("ISSUEDOG_REPO"), code: 1, want: `reading the settings: ISSUEDOG_REPO is "", not owner/name`},
		"slug": {setup: func(t *testing.T, _ *githubtest.Fake) {
			dead := httptest.NewServer(http.NotFoundHandler())
			dead.Close()
			apiURL = dead.URL
		}, code: 1, want: "starting the daemon: reading the App's slug: github app: "},
		"token": {setup: func(t *testing.T, f *githubtest.Fake) {
			target, err := url.Parse(f.URL())
			if err != nil {
				t.Fatal(err)
			}
			front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/access_tokens") {
					http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
					return
				}
				httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
			}))
			t.Cleanup(front.Close)
			apiURL = front.URL
		}, code: 1, want: "starting the daemon: minting a token: github installation token: "},
		"clone": {setup: func(t *testing.T, _ *githubtest.Fake) {
			gitRemote = func(string, string) string { return filepath.Join(t.TempDir(), "none.git") }
		}, code: 1, want: "starting the daemon: syncing the clone: fetch "},
		"config": {files: func(*testing.T) map[string]string { return map[string]string{} }, code: 1,
			want: "starting the daemon: loading the config from main: load config: read main:.issuedog/config.yaml: not found"},
		"labels": {failWrite: 1, code: 1, want: "starting the daemon: creating the labels: create label stage:intake"},
		"signal": {setup: func(t *testing.T, _ *githubtest.Fake) {
			cancel, _ := cancelOnCall(t)
			cancel()
		}, code: 0, want: "stopping on a signal during startup: "},
	} {
		t.Run(name, func(t *testing.T) {
			files := thisRepo
			if tt.files != nil {
				files = tt.files
			}
			f := fakeGitHub(t, files(t))
			f.FailWrite(tt.failWrite)
			if tt.setup != nil {
				tt.setup(t, f)
			}
			calls := fakeEgress(t, nil, nil)
			environ := tt.environ
			if environ == nil {
				environ = daemonEnv
			}
			code, logs := runBounded(t, environ)
			if code != tt.code || !strings.Contains(logs, tt.want) {
				t.Errorf("exit code %d, logs %q; want %d and %q", code, logs, tt.code, tt.want)
			}
			if len(*calls) != 0 {
				t.Errorf("startup steps = %q, want none of egress", *calls)
			}
		})
	}
}

func TestReadSettings(t *testing.T) {
	set, err := readSettings(daemonEnv)
	if want := (settings{"mertenvg", "issuedog", 5275196, 170308396}); err != nil || set != want {
		t.Errorf("readSettings = %+v, %v; want %+v", set, err, want)
	}
	for kv, want := range map[string]string{
		"ISSUEDOG_REPO=":                    `ISSUEDOG_REPO is "", not owner/name`,
		"ISSUEDOG_REPO=mertenvg":            `ISSUEDOG_REPO is "mertenvg", not owner/name`,
		"ISSUEDOG_REPO=/issuedog":           `ISSUEDOG_REPO is "/issuedog", not owner/name`,
		"ISSUEDOG_REPO=mertenvg/":           `ISSUEDOG_REPO is "mertenvg/", not owner/name`,
		"ISSUEDOG_REPO=mertenvg/issuedog/x": `ISSUEDOG_REPO is "mertenvg/issuedog/x", not owner/name`,
		"ISSUEDOG_APP_ID=abc":               `ISSUEDOG_APP_ID is "abc", not a positive number`,
		"ISSUEDOG_APP_ID=0":                 `ISSUEDOG_APP_ID is "0", not a positive number`,
		"ISSUEDOG_INSTALLATION_ID=-1":       `ISSUEDOG_INSTALLATION_ID is "-1", not a positive number`,
	} {
		// The later setting wins, as in an environment.
		if _, err := readSettings(append(slices.Clone(daemonEnv), kv)); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", kv, err, want)
		}
	}
}

// A trust that can't load, with CODEOWNERS on main a directory, stops
// startup. Called directly, since run would go on into the loop if it didn't.
func TestStartDaemonFailsOnTrust(t *testing.T) {
	files := thisRepo(t)
	delete(files, ".github/CODEOWNERS")
	files[".github/CODEOWNERS/README"] = "not a file\n"
	fakeGitHub(t, files)
	key, err := os.ReadFile(appKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	d, err := startDaemon(context.Background(), settings{"mertenvg", "issuedog", 1, 1}, key)
	if d != nil || err == nil || !strings.Contains(err.Error(), "loading trust from main: load trust for mertenvg/issuedog: find CODEOWNERS: ") {
		t.Errorf("startDaemon = %v, %v; want the trust error", d, err)
	}
}

// runBounded is runLogged, failing t if run hasn't returned in 20s, as when
// startup goes on into the loop.
func runBounded(t *testing.T, environ []string) (int, string) {
	t.Helper()
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()
	done := make(chan int, 1)
	go func() { done <- run(nil, environ, nil, io.Discard) }()
	select {
	case code := <-done:
		logr.Wait()
		return code, logs.String()
	case <-time.After(20 * time.Second):
		t.Fatal("run didn't return; want startup to stop")
		return 0, ""
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

// Stage runs are agent's, workspace and claude both, with the Claude
// credential and the egress proxy as the only way out, in the repo's clone,
// and the gate matches the config's usage_limit.pattern.
func TestStageWork(t *testing.T) {
	fakeAgent(t, nil)
	oldWork, oldState, oldRun := workDir, stateDir, runDir
	t.Cleanup(func() { workDir, stateDir, runDir = oldWork, oldState, oldRun })
	workDir, stateDir, runDir = t.TempDir(), t.TempDir(), t.TempDir()
	cfg := config.Defaults()
	cfg.UsageLimit.Pattern = "frobnicated"
	cred := agent.Cred{Name: agent.APIKeyVar, Value: "key"}
	d := &daemon.Daemon{Config: cfg}
	runs, err := stageWork(d, "repo", cred)
	if err != nil {
		t.Fatal(err)
	}
	uid := uint32(os.Getuid())
	if c := runs.Runner.Credential; c == nil || c.Uid != uid {
		t.Errorf("claude runs as %+v, want agent's credential", c)
	}
	if c := runs.Workspace.Credential; c == nil || c.Uid != uid {
		t.Errorf("the workspace's git runs as %+v, want agent's credential", c)
	}
	wantEnv := []string{"ANTHROPIC_API_KEY=key", "HTTPS_PROXY=http://127.0.0.1:3128", "HTTP_PROXY=http://127.0.0.1:3128",
		"HOME=/home/agent", "PATH=/usr/local/bin:/usr/bin:/bin"}
	if !slices.Equal(runs.Env, wantEnv) {
		t.Errorf("the agent's environment = %q, want %q", runs.Env, wantEnv)
	}
	ws := runs.Workspace
	if ws.Bare != filepath.Join(workDir, "repo.git") || ws.RunsDir != filepath.Join(workDir, "runs") || ws.ContextDir != runDir ||
		runs.Runner.StateDir != stateDir || runs.Runner.Claude != "claude" {
		t.Errorf("workspace %+v, runner %+v; want the clone, runs and context dirs, and the state dir", ws, runs.Runner)
	}
	// The interviewer at clarifying and triage as Unlabeled, both on d, and
	// nothing else: a Stage at intake beside triage fails CheckRegistered.
	if iv, ok := runs.Stages[state.Clarifying].(*daemon.Interviewer); len(runs.Stages) != 1 || !ok || iv.Daemon != d {
		t.Errorf("stages = %v, want only the interviewer on d at clarifying", runs.Stages)
	}
	if tr, ok := runs.Unlabeled.(*daemon.Triage); !ok || tr.Daemon != d {
		t.Errorf("Unlabeled = %#v, want triage on d", runs.Unlabeled)
	}
	if err := runs.CheckRegistered(); err != nil {
		t.Errorf("CheckRegistered = %v, want nil", err)
	}
	now := time.Now()
	if runs.Gate.Observe(now, true, "usage limit reached") || !runs.Gate.Observe(now, true, "frobnicated") {
		t.Error("the gate doesn't match by the config's usage_limit.pattern")
	}

	fakeAgent(t, errors.New("no such user"))
	if _, err := stageWork(&daemon.Daemon{Config: cfg}, "repo", cred); err == nil || !strings.Contains(err.Error(), "looking up agent: no such user") {
		t.Errorf("stageWork = %v, want the lookup error", err)
	}
}

// A Stage registered at intake while Unlabeled is set fails startup, with
// the reason, before egress and the loop: every result of that Stage would
// be dropped (#130's verify).
func TestRegisterStagesChecked(t *testing.T) {
	f := fakeGitHub(t, thisRepo(t))
	calls := fakeEgress(t, nil, nil)
	old := registerStages
	t.Cleanup(func() { registerStages = old })
	registerStages = func(d *daemon.Daemon) (map[state.Stage]daemon.Stage, daemon.Stage) {
		stages, unlabeled := old(d)
		stages[state.Intake] = &daemon.Interviewer{Daemon: d}
		return stages, unlabeled
	}
	// Startup that passes the check would run the loop until canceled, so
	// it gets 10s, then a cancel, and fails rather than hangs.
	cancel, _ := cancelOnCall(t)
	var buf bytes.Buffer
	stop := logr.AddWriter(&buf)
	defer stop()
	done := make(chan int, 1)
	go func() { done <- run(nil, daemonEnv, nil, io.Discard) }()
	var code int
	select {
	case code = <-done:
	case <-time.After(10 * time.Second):
		cancel()
		<-done
		t.Fatal("startup passed the check and ran the loop")
	}
	logr.Wait()
	logs := buf.String()
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if want := "setting up stage runs: registering the stages: a Stage (*daemon.Interviewer) is registered at intake while Unlabeled (*daemon.Triage) is set: " +
		"Unlabeled's leases record intake, so every result of the Stage at intake would be dropped"; !strings.Contains(logs, want) {
		t.Errorf("logs = %q, want %q", logs, want)
	}
	if len(*calls) != 0 || slices.ContainsFunc(f.Requests(), func(r githubtest.Request) bool { return r.Path == "/issues" }) {
		t.Errorf("startup steps %q, requests %+v; want startup to stop before egress and the loop", *calls, f.Requests())
	}
}
