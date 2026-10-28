package daemon_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/logr/v2"
)

// origin is a bare repo standing in for GitHub's, and a work tree that
// pushes to it. Its main starts with a config and a CODEOWNERS trusting owner.
type origin struct {
	t         testing.TB
	url, work string
}

func newOrigin(t testing.TB) *origin {
	t.Helper()
	dir := t.TempDir()
	o := &origin{t: t, url: filepath.Join(dir, "origin.git"), work: filepath.Join(dir, "work")}
	gitIn(t, dir, "init", "--bare", "--quiet", "--initial-branch=main", o.url)
	gitIn(t, dir, "init", "--quiet", "--initial-branch=main", o.work)
	o.commit(".github/CODEOWNERS", "* @owner\n")
	o.commit(config.Path, "types: labels\nloop:\n  idle_sleep: 7s\n")
	return o
}

// commit writes path on main and pushes it.
func (o *origin) commit(path, content string) {
	o.t.Helper()
	full := filepath.Join(o.work, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		o.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		o.t.Fatal(err)
	}
	gitIn(o.t, o.work, "add", path)
	gitIn(o.t, o.work, "commit", "--quiet", "-m", "write "+path)
	gitIn(o.t, o.work, "push", "--quiet", o.url, "main")
}

// gitIn runs git in dir and returns its trimmed output.
func gitIn(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") }),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// newDaemon is a fresh daemon of o/r on f, cloning o, with the /hold,
// /resume and /priority handlers. Its Reactions hook adds each issue it sees
// to *visits.
func newDaemon(t testing.TB, f *githubtest.Fake, o *origin, visits *[]int) *daemon.Daemon {
	t.Helper()
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
	st := &daemon.Step{Applier: a,
		Reactions: func(_ context.Context, _ *daemon.Applier, s *github.Snapshot, _ daemon.Dirt) error {
			if visits != nil {
				*visits = append(*visits, s.Number)
			}
			return nil
		}}
	st.Commands = daemon.Commands(st.CurrentTrust())
	return &daemon.Daemon{Step: st, Clone: repo.New(o.url, filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults()}
}

// newFake is o/r with owner an admin and mallory a writer, whom CODEOWNERS
// doesn't trust.
func newFake(t testing.TB) *githubtest.Fake {
	f := githubtest.New(t, "o/r", slug)
	f.SetPermission("owner", "admin")
	f.SetPermission("mallory", "write")
	return f
}

// fileAt files an issue at stage, as the App set it, with more labels the
// App added, and returns its number.
func fileAt(f *githubtest.Fake, stage, body string, labels ...string) int {
	n := f.AddIssue("owner", "Issue", body)
	for _, l := range append([]string{"stage:" + stage}, labels...) {
		f.AddLabel(n, app, l)
	}
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to="+stage+" run=r"+stage+" -->")
	return n
}

// quiet files issue 1 at ready with priority:p2, issue 2 at spec-review
// with a body its author edited, so Dirty reads its history, and revision 1
// of it, a pull request and a closed issue. Last, owner comments on issue 1,
// which makes it the newest: the issue at the cursor, which every poll lists
// again.
func quiet(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	fileAt(f, "ready", "do it", "priority:p2")
	f.AddLabel(f.AddIssue("owner", "Issue", "the spec"), app, "stage:spec-review")
	f.EditBody(2, "owner", "the spec, edited")
	f.AddComment(2, app, revision(1, "the spec, edited"))
	f.SetPullRequest(f.AddIssue("owner", "PR", ""))
	f.Close(f.AddIssue("owner", "Closed", ""))
	if id := f.AddComment(1, "owner", "hello"); id != hello {
		t.Fatalf("comment ID = %d, want %d", id, hello)
	}
	return f
}

// hello is quiet's comment from owner.
const hello = 1003

func iterate(t *testing.T, d *daemon.Daemon) {
	t.Helper()
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatalf("Iterate: %v", err)
	}
}

// newest is the newest updated_at that a full listing of f gives.
func newest(t *testing.T, f *githubtest.Fake) (issues, comments time.Time) {
	c := f.Client(t)
	is, err := c.IssuesSince(context.Background(), "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	cs, err := c.CommentsSince(context.Background(), "o", "r", 0, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range is {
		issues = later(issues, i.GetUpdatedAt().Time)
	}
	for _, c := range cs {
		comments = later(comments, c.GetUpdatedAt().Time)
	}
	return issues, comments
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func TestIterateUnchanged(t *testing.T) {
	t.Parallel()
	f := quiet(t)
	var visits []int
	d := newDaemon(t, f, newOrigin(t), &visits)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("the first iteration wrote %+v; want a quiet repo", w)
	}
	if !reflect.DeepEqual(visits, []int{1, 2}) {
		t.Errorf("the first iteration ran step 1 on %v, want every open issue, [1 2]", visits)
	}
	histories := f.HistoryQueries()
	if histories == 0 {
		t.Fatal("no history query: issue 2's edited body should need one")
	}
	issuesAt, commentsAt := newest(t, f)
	from := len(f.Requests())

	visits = nil
	iterate(t, d)
	reqs := f.Requests()[from:]
	if len(reqs) == 0 {
		t.Fatal("the second iteration sent no requests")
	}
	for _, r := range reqs {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("the second iteration sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if len(f.Writes()) != 0 || f.HistoryQueries() != histories {
		t.Errorf("the second iteration wrote %+v and sent %d history queries; want none", f.Writes(), f.HistoryQueries()-histories)
	}
	if !reflect.DeepEqual(visits, []int{2}) {
		t.Errorf("the second iteration ran step 1 on %v, want only the issue at spec-review, [2]", visits)
	}
	for path, want := range map[string]time.Time{"/issues": issuesAt, "/issues/comments": commentsAt} {
		i := slices.IndexFunc(reqs, func(r githubtest.Request) bool { return r.Path == path })
		if i < 0 {
			t.Errorf("the second iteration didn't poll %s", path)
			continue
		}
		q, err := url.ParseQuery(reqs[i].Query)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := time.Parse(time.RFC3339, q.Get("since")); err != nil || !got.Equal(want) {
			t.Errorf("%s polled since=%q, want the newest updated_at, %s", path, q.Get("since"), want.Format(time.RFC3339))
		}
	}
}

func TestIterateAppliesNewCommands(t *testing.T) {
	t.Parallel()
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	labels := func() []string { return f.State().Issues[0].Labels }

	f.AddComment(1, "eve", "/priority p0") // untrusted, without write access: ignored, and holds nothing
	cmd := f.AddComment(1, "owner", "/priority p1")
	iterate(t, d)
	if got := labels(); !reflect.DeepEqual(got, []string{"priority:p1", "stage:ready"}) {
		t.Errorf("labels after /priority p1 = %v, want priority:p1 and stage:ready", got)
	}
	comments := f.State().Issues[0].Comments
	if got := comments[len(comments)-1].Reactions; !reflect.DeepEqual(got, []string{app + " +1"}) {
		t.Errorf("reactions to comment %d = %v, want the App's 👍", cmd, got)
	}

	// A comment edit doesn't change the issue's updated_at, so only the
	// comment poll finds it. The App's writes above get read once first.
	iterate(t, d)
	f.EditComment(hello, "owner", "/priority p3") // never processed
	iterate(t, d)
	if got := labels(); !reflect.DeepEqual(got, []string{"priority:p3", "stage:ready"}) {
		t.Errorf("labels after an edit to /priority p3 = %v, want priority:p3 and stage:ready", got)
	}
}

// busy files issues for every kind of step 1 write: a /priority p1, an
// owner's manual stage change, mallory's priority label, and an unknown
// command.
func busy(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	fileAt(f, "ready", "a", "priority:p2")
	f.AddComment(1, "owner", "/priority p1")
	fileAt(f, "ready", "b")
	f.AddLabel(2, "owner", "stage:needs-human")
	fileAt(f, "ready", "c")
	f.AddLabel(3, "mallory", "priority:p0")
	fileAt(f, "ready", "d")
	f.AddComment(4, "owner", "/frob")
	return f
}

func TestIterateReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	pass := func(f *githubtest.Fake) error { return newDaemon(t, f, o, nil).Iterate(context.Background()) }
	f := busy(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	got := f.State().Issues
	if !reflect.DeepEqual(got[0].Labels, []string{"priority:p1", "stage:ready"}) || len(got[1].Comments) != 2 ||
		!reflect.DeepEqual(got[2].Labels, []string{"stage:ready"}) || len(got[3].Comments) != 3 {
		t.Fatalf("a clean iteration left %+v; want /priority applied, the override recorded, the label reverted and /frob rejected", got)
	}
	githubtest.Replay(t, busy, pass)
}

func TestIterateSkipsWorkAfterStepFails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	fileAt(f, "clarifying", "a")
	answer := f.AddComment(1, "owner", "my answer")
	fileAt(f, "ready", "b")
	f.AddComment(2, "owner", "/hold")
	d := newDaemon(t, f, newOrigin(t), nil)
	failing := true
	d.Step.Commands["hold"] = daemon.Handler{Apply: func(ctx context.Context, a *daemon.Applier, c daemon.Cmd) error {
		if failing {
			return errors.New("boom")
		}
		return a.Ack(ctx, c.Comment.ID)
	}}
	var ready [][]int
	var inputs []int64
	d.Work = func(_ context.Context, rs []daemon.Ready) error {
		var ns []int
		for _, r := range rs {
			ns = append(ns, r.S.Number)
			if r.S.Number == 1 {
				inputs = nil
				for _, c := range r.Inputs {
					inputs = append(inputs, c.ID)
				}
			}
		}
		ready = append(ready, ns)
		return nil
	}
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Iterate = %v, want issue 2's step 1 error", err)
	}
	failing = false
	iterate(t, d)
	if !reflect.DeepEqual(ready, [][]int{{1}, {1, 2}}) {
		t.Errorf("Work got issues %v, want [1] while issue 2's step 1 fails, then [1 2] once it's run again", ready)
	}
	if !reflect.DeepEqual(inputs, []int64{answer}) {
		t.Errorf("issue 1's Inputs = %v, want the answer %d", inputs, answer)
	}
}

func TestIterateKeepsLastGoodConfig(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.CaptureTo).
	var logs bytes.Buffer
	stop := logtest.CaptureTo(t, &logs, logr.All)
	defer stop()
	f := quiet(t)
	o := newOrigin(t)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	if d.Config.Loop.IdleSleep != 7*time.Second || d.Step.Trust == nil {
		t.Fatalf("idle_sleep = %s, trust %v; want main's 7s and a trust", d.Config.Loop.IdleSleep, d.Step.Trust)
	}
	good := d.Config

	o.commit(config.Path, "types: labels\nloop:\n  idle_sleep: 0s\n")
	iterate(t, d)
	trusted := d.Step.Trust
	// CODEOWNERS on main becomes a directory, which trust can't load.
	gitIn(t, o.work, "rm", "--quiet", ".github/CODEOWNERS")
	o.commit(".github/CODEOWNERS/README", "not a file\n")
	iterate(t, d)
	if err := os.RemoveAll(o.url); err != nil {
		t.Fatal(err)
	}
	iterate(t, d)
	logr.Wait()
	if d.Config != good || d.Step.Trust != trusted {
		t.Errorf("config = %+v, trust %v; want the last good ones kept", d.Config, d.Step.Trust)
	}
	for _, want := range []string{
		"loading the config of o/r from main failed, so the last good one stays: ", "loop.idle_sleep: must be positive",
		"loading the trust of o/r from main failed, so the last good one stays: ",
		"syncing the clone of o/r failed, so the last good config and trust stay: ",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
	}
}

// A reload that allows a new model creates its labels, once. One that adds
// none doesn't touch the repo's labels, and a failure is tried again.
func TestIterateCreatesLabelsForNewModels(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.CaptureTo).
	var logs bytes.Buffer
	stop := logtest.CaptureTo(t, &logs, logr.All)
	defer stop()
	f := quiet(t)
	o := newOrigin(t)
	d := newDaemon(t, f, o, nil)
	labelCalls := func() int {
		n := 0
		for _, r := range f.Requests() {
			if r.Path == "/labels" {
				n++
			}
		}
		return n
	}
	iterate(t, d)
	if n := labelCalls(); n != 0 {
		t.Fatalf("%d calls to the repo's labels, want none while no model is allowed", n)
	}
	const sonnet = "{name: us.anthropic.claude-sonnet-4-5-20250929-v1:0, label: sonnet-4-5}"
	o.commit(config.Path, "types: labels\nmodels: {allowed: [opus]}\n")
	iterate(t, d)
	labels := f.RepoLabels()
	for _, want := range []string{"model:opus", "verifier-model:opus", "effort:low", "verifier-effort:max", "stage:ready", "type:task"} {
		if !slices.Contains(labels, want) {
			t.Errorf("repo labels = %v, want %s", labels, want)
		}
	}
	logr.Wait()
	if !strings.Contains(logs.String(), "created the labels stage:intake, ") || !strings.Contains(logs.String(), ", verifier-effort:max on o/r") {
		t.Errorf("logs = %q, want the labels created, from stage:intake to verifier-effort:max on o/r", logs.String())
	}
	calls := labelCalls()
	iterate(t, d)
	o.commit(config.Path, "types: labels\nmodels: {allowed: [Opus]}\n") // same label
	iterate(t, d)
	if n := labelCalls(); n != calls {
		t.Errorf("%d more calls to the repo's labels, want none when no model is new", n-calls)
	}

	o.commit(config.Path, "types: labels\nmodels: {allowed: [{name: opus, label: claude-opus}]}\n") // a new label
	iterate(t, d)
	if labels := f.RepoLabels(); !slices.Contains(labels, "model:claude-opus") || !slices.Contains(labels, "verifier-model:claude-opus") {
		t.Errorf("repo labels = %v, want model:claude-opus and verifier-model:claude-opus for opus's new label", labels)
	}

	o.commit(config.Path, "types: labels\nmodels: {allowed: [opus, "+sonnet+"]}\n")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "POST" && r.Path == "/labels" }, 500)
	iterate(t, d)
	logr.Wait()
	if slices.Contains(f.RepoLabels(), "model:sonnet-4-5") {
		t.Fatal("model:sonnet-4-5 was created through a failure")
	}
	if want := "creating the labels of the new models on o/r failed, so it's tried again next iteration: "; !strings.Contains(logs.String(), want) {
		t.Errorf("logs = %q, want %q", logs.String(), want)
	}
	// retried checks that the next iteration creates the model's labels.
	retried := func(suffix string) {
		t.Helper()
		iterate(t, d)
		labels := f.RepoLabels()
		for _, want := range []string{"model:" + suffix, "verifier-model:" + suffix} {
			if !slices.Contains(labels, want) {
				t.Errorf("repo labels = %v, want %s after the retry", labels, want)
			}
		}
	}
	retried("sonnet-4-5") // with the same config, which loads

	// The retry doesn't wait on a config that loads: the last good one stays.
	o.commit(config.Path, "types: labels\nmodels: {allowed: [opus, "+sonnet+", haiku]}\n")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "POST" && r.Path == "/labels" }, 500)
	iterate(t, d)
	o.commit(config.Path, "types: labels\nloop: {idle_sleep: 0s}\n")
	retried("haiku")
	logr.Wait()
	if want := "loading the config of o/r from main failed"; !strings.Contains(logs.String(), want) {
		t.Errorf("logs = %q, want %q", logs.String(), want)
	}
	calls = labelCalls()
	iterate(t, d)
	o.commit(config.Path, "types: labels\nmodels: {allowed: ["+sonnet+"]}\n") // one removed
	iterate(t, d)
	if n := labelCalls(); n != calls {
		t.Errorf("%d more calls to the repo's labels, want none once they're created", n-calls)
	}
	seen := map[string]bool{}
	for _, l := range f.RepoLabels() {
		if seen[strings.ToLower(l)] {
			t.Errorf("label %s created twice", l)
		}
		seen[strings.ToLower(l)] = true
	}
}

// The labels a reload creates for a new model are safe to replay after a
// crash at any write.
func TestIterateReplaysLabelsForNewModels(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	o.commit(config.Path, "models: {allowed: [opus]}\n")
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddRepoLabel("stage:intake")
		fileAt(f, "ready", "a")
		return f
	}
	pass := func(f *githubtest.Fake) error { return newDaemon(t, f, o, nil).Iterate(context.Background()) }
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	labels := f.State().RepoLabels
	for _, want := range []string{"model:opus ", "verifier-model:opus ", "effort:low ", "verifier-effort:max ", "stage:held "} {
		if !slices.ContainsFunc(labels, func(l string) bool { return strings.HasPrefix(l, want) }) {
			t.Errorf("repo labels = %q, want %s", labels, want)
		}
	}
	githubtest.Replay(t, setup, pass)
}

func TestLabelOptions(t *testing.T) {
	t.Parallel()
	cfg, err := config.Parse([]byte("types: labels\nscopes: [daemon]\nmodels: {allowed: [opus, {name: claude-sonnet-x, label: sonnet}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := github.LabelOptions{Scopes: []string{"daemon"}, TypeLabels: true,
		Models:  []github.ModelLabel{{Suffix: "opus", Name: "opus"}, {Suffix: "sonnet", Name: "claude-sonnet-x"}},
		Efforts: []string{"low", "medium", "high", "xhigh", "max"}}
	if got := daemon.LabelOptions(cfg); !reflect.DeepEqual(got, want) {
		t.Errorf("LabelOptions = %+v, want %+v", got, want)
	}
	if got := daemon.LabelOptions(config.Defaults()); got.TypeLabels || len(got.Models) != 0 || len(got.Efforts) != 5 {
		t.Errorf("LabelOptions of the defaults = %+v, want no type labels or models, and five efforts", got)
	}
}

// A long name with no label falls back to its last 35 characters, and says so.
func TestLabelOptionsLogsTheFallback(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.CaptureTo).
	const long = "x.us.anthropic.claude-sonnet-4-5-20250929-v1:0"
	at35, wide := strings.Repeat("a", 35), strings.Repeat("é", 36) // 35 characters, and 36 in 72 bytes
	cfg, err := config.Parse([]byte("models: {allowed: [opus, " + long + ", {name: y." + long + ", label: sonnet}, " + at35 + ", " + wide + "]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	stop := logtest.CaptureTo(t, &logs, logr.I)
	defer stop()
	got := daemon.LabelOptions(cfg)
	logr.Wait()
	if got.Models[1].Suffix != "pic.claude-sonnet-4-5-20250929-v1:0" {
		t.Errorf("suffix of %s = %q, want its last 35 characters", long, got.Models[1].Suffix)
	}
	want := `models.allowed: "` + long + `" has no label and is longer than 35 characters, so its labels use its last 35 characters, "pic.claude-sonnet-4-5-20250929-v1:0"`
	wideWant := `models.allowed: "` + wide + `" has no label and is longer than 35 characters, so its labels use its last 35 characters, "` + wide[2:] + `"`
	if !strings.Contains(logs.String(), want) || !strings.Contains(logs.String(), wideWant) || strings.Count(logs.String(), "has no label") != 2 {
		t.Errorf("info logs = %q, want %q and %q, and no others", logs.String(), want, wideWant)
	}
}

func TestRunStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	f := quiet(t)
	o := newOrigin(t)
	// An hour's sleep, so only the cancellation can stop Run within the
	// bound below, which is generous so a loaded machine doesn't fail it.
	o.commit(config.Path, "types: labels\nloop:\n  idle_sleep: 1h\n")
	const bound = time.Minute
	d := newDaemon(t, f, o, nil)
	iterations := 0
	iterated := make(chan struct{}, 1)
	d.Work = func(context.Context, []daemon.Ready) error {
		iterations++
		iterated <- struct{}{}
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()
	select {
	case <-iterated:
	case <-time.After(bound):
		t.Fatal("Run didn't iterate")
	}
	// The config on main sleeps 1h, so Run is asleep.
	cancel()
	select {
	case <-done:
	case <-time.After(bound):
		t.Fatal("Run didn't stop when its context was canceled")
	}
	if iterations != 1 {
		t.Errorf("Run iterated %d times, want once before its 1h sleep", iterations)
	}
}

func TestCurrentTrust(t *testing.T) {
	t.Parallel()
	st := &daemon.Step{Trust: trusted{"owner": true}}
	tr := st.CurrentTrust()
	if ok, err := tr.Trusted(context.Background(), "owner"); err != nil || !ok {
		t.Errorf("Trusted(owner) = %t, %v; want true", ok, err)
	}
	st.Trust = failTrust{}
	if _, err := tr.Trusted(context.Background(), "owner"); !errors.Is(err, errTrust) {
		t.Errorf("Trusted(owner) after the trust changed: %v, want the new trust's error", err)
	}
}

// A 👍 changes no issue's updated_at, so the visit to an issue at spec-review
// approves it. Trust is main's as of each pass: once owner2 leaves
// CODEOWNERS, their 👍 no longer approves.
func TestIterateApprovesAtSpecReview(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.SetPermission("owner2", "admin")
	n := f.AddIssue("owner", "Spec", "body")
	f.SetType(n, "Story")
	f.AddLabel(n, app, "stage:spec-review")
	rev := f.AddComment(n, app, revision(1, "body"))
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @owner @owner2\n")
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	// Wired as main wires it, once trust has loaded.
	d.Step.Commands["approve"] = daemon.ApproveCommand()
	d.Step.Reactions = daemon.ApproveByReaction(d.Step.CurrentTrust())
	labels := func() []string { return f.State().Issues[0].Labels }

	o.commit(".github/CODEOWNERS", "* @owner\n")
	f.AddReaction(rev, "owner2", "+1")
	iterate(t, d)
	if got := labels(); !reflect.DeepEqual(got, []string{"stage:spec-review"}) {
		t.Errorf("labels after owner2's 👍 = %v, want stage:spec-review: owner2 left CODEOWNERS", got)
	}
	f.AddReaction(rev, "owner", "+1")
	iterate(t, d)
	if got := labels(); !reflect.DeepEqual(got, []string{"stage:planning"}) {
		t.Errorf("labels after owner's 👍 = %v, want stage:planning", got)
	}
}

// chain files tasks 1 > 2 > 3 at ready, in-progress and verifying.
func chain(t testing.TB) *githubtest.Fake {
	f := newFake(t)
	for n, stage := range []string{"ready", "in-progress", "verifying"} {
		fileAt(f, stage, "task")
		f.SetType(n+1, "Task")
		if n > 0 {
			f.SetParent(n+1, n)
		}
	}
	return f
}

// A whole pass with a /hold on a tree is safe to replay, and the issues it
// touched, read again on the next Iterate, need nothing more.
func TestIterateHoldReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := func(t testing.TB) *githubtest.Fake {
		f := chain(t)
		f.AddComment(1, "owner", "/hold")
		return f
	}
	pass := func(f *githubtest.Fake) error {
		d := newDaemon(t, f, o, nil)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		return d.Iterate(context.Background())
	}
	githubtest.Replay(t, setup, pass)
	f := setup(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	for _, is := range f.State().Issues {
		if !reflect.DeepEqual(is.Labels, []string{"stage:held"}) || len(is.Comments) != map[int]int{1: 3, 2: 2, 3: 2}[is.Number] {
			t.Errorf("#%d: labels %q and %d comments, want held, with only the hold's comments added", is.Number, is.Labels, len(is.Comments))
		}
	}
}

// Work never gets an issue a command wrote to in the same pass, even one
// whose step 1 ran before the command: #1 is #2's child.
func TestIterateWorkSkipsTouched(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	fileAt(f, "ready", "child")
	fileAt(f, "ready", "parent")
	f.SetParent(1, 2)
	f.AddComment(2, "owner", "/hold")
	d := newDaemon(t, f, newOrigin(t), nil)
	var ready [][]int
	d.Work = func(_ context.Context, rs []daemon.Ready) error {
		var ns []int
		for _, r := range rs {
			ns = append(ns, r.S.Number)
		}
		ready = append(ready, ns)
		return nil
	}
	iterate(t, d)
	iterate(t, d)
	if !reflect.DeepEqual(ready, [][]int{{2}, {1, 2}}) {
		t.Errorf("Work got issues %v, want [2] while #1's snapshot is out of date, then [1 2]", ready)
	}
}

// Commands on a parent and a child in the same poll end as they do a poll
// apart: the child, which the parent's command may have written to, is read
// again before its step 1, even when nothing was written to it. Comment IDs,
// which differ, are left out.
func TestIterateSamePollCommands(t *testing.T) {
	t.Parallel()
	ids := regexp.MustCompile(`=[0-9]{4,}`)
	for _, tc := range []struct {
		name    string
		priorOn int // the issue a /hold is run on first, if any
		cmds    [2]string
	}{
		{"/resume on the parent, then /hold on the child", 1, [2]string{"/resume", "/hold"}},
		{"/hold on the parent, then /resume on the child", 0, [2]string{"/hold", "/resume"}},
		{"/hold on the parent, which writes nothing to the held child, then /resume on it", 2, [2]string{"/hold", "/resume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOrigin(t)
			run := func(apart bool) [][]string {
				f := chain(t)
				d := newDaemon(t, f, o, nil)
				if tc.priorOn != 0 {
					f.AddComment(tc.priorOn, "owner", "/hold")
				}
				iterate(t, d)
				for i, cmd := range tc.cmds {
					f.AddComment(i+1, "owner", cmd)
					if apart {
						iterate(t, d)
						iterate(t, d)
					}
				}
				for range 3 {
					iterate(t, d)
				}
				var got [][]string
				for _, is := range f.State().Issues {
					bodies := is.Labels
					for _, c := range is.Comments {
						bodies = append(bodies, ids.ReplaceAllString(c.Body, "=N")+" "+strings.Join(c.Reactions, ","))
					}
					slices.Sort(bodies[len(is.Labels):])
					got = append(got, bodies)
				}
				return got
			}
			if same, apart := run(false), run(true); !reflect.DeepEqual(same, apart) {
				t.Errorf("in the same poll:\n%q\nwant as a poll apart:\n%q", same, apart)
			}
		})
	}
}

// reviewing files issue 1, a story at spec-review with revision 1 of its
// body, and runs a first, quiet iteration of a daemon that handles /revise.
func reviewing(t *testing.T) (*githubtest.Fake, *daemon.Daemon) {
	f := newFake(t)
	n := f.AddIssue("owner", "Spec", "body")
	f.SetType(n, "Story")
	f.AddLabel(n, app, "stage:spec-review")
	f.AddComment(n, app, revision(1, "body"))
	d := newDaemon(t, f, newOrigin(t), nil)
	d.Step.Commands["revise"] = daemon.ReviseCommand()
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("the first iteration wrote %+v, want nothing", w)
	}
	return f, d
}

// A failed ReviewVisit fails the issue's pass as Run would: Work doesn't get
// it, and the next pass reads it again and posts the revision.
func TestIterateRetriesAFailedReviewVisit(t *testing.T) {
	t.Parallel()
	f, d := reviewing(t)
	work := recordWork(d)
	f.EditBody(1, "owner", "body v2")
	f.FailWrite(1) // the revision
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "revision of o/r#1") {
		t.Errorf("Iterate = %v, want the revision's error", err)
	}
	f.FailWrite(0)
	iterate(t, d)
	got := f.State().Issues[0].Comments
	if last := got[len(got)-1].Body; last != posted(2, "body v2", "", "", "1001") || !reflect.DeepEqual(*work, [][]int{{}, {1}}) {
		t.Errorf("newest comment %q and Work got %v; want revision 2, and issue 1 only once it's posted", last, *work)
	}
}

// ReviewVisit runs only after Run succeeds: one after a failed Run would post
// a revision above a request Run didn't get to, and lose it.
func TestIterateSkipsReviewVisitWhenRunFails(t *testing.T) {
	t.Parallel()
	f, d := reviewing(t)
	f.EditBody(1, "owner", "body v2")
	f.AddComment(1, "owner", "too long")
	f.FailWrite(1) // sending the spec back
	if err := d.Iterate(context.Background()); err == nil {
		t.Error("Iterate = nil, want the failed write's error")
	}
	if w := f.Writes(); len(w) != 1 {
		t.Errorf("the failed pass wrote %+v, want only the failed write", w)
	}
	f.FailWrite(0)
	iterate(t, d)
	got := f.State().Issues[0]
	if last := got.Comments[len(got.Comments)-1].Body; last != sentBack("1002") || !slices.Equal(got.Labels, []string{"stage:clarifying"}) {
		t.Errorf("newest comment %q and labels %v, want the spec sent back", last, got.Labels)
	}
}

// Each pass on an issue at spec-review ends with ReviewVisit: the owner's
// edit of the body posts the next revision, and so does mallory dirtying the
// newest one; an owner's plain comment sends the spec back.
func TestIterateReviewsSpecs(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(t *testing.T, f *githubtest.Fake, d *daemon.Daemon)
		last   string // the newest comment
		label  string
	}{
		{"the owner's edit posts the next revision",
			func(_ *testing.T, f *githubtest.Fake, _ *daemon.Daemon) { f.EditBody(1, "owner", "body v2") },
			posted(2, "body v2", "", "", "1001"), "stage:spec-review"},
		// eve has no write access, so her edit holds nothing (SPEC §6.4).
		{"eve dirtying the newest revision posts it again",
			func(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
				f.EditBody(1, "owner", "body v2")
				iterate(t, d) // posts revision 2, comment 1002
				f.EditComment(1002, "eve", "nothing to approve here")
			},
			posted(2, "body v2", "", "", "1001"), "stage:spec-review"},
		{"the owner's plain comment sends the spec back",
			func(_ *testing.T, f *githubtest.Fake, _ *daemon.Daemon) { f.AddComment(1, "owner", "too long") },
			sentBack("1002"), "stage:clarifying"},
		{"with an edit in the same poll, the comment still sends it back: Run comes first",
			func(_ *testing.T, f *githubtest.Fake, _ *daemon.Daemon) {
				f.EditBody(1, "owner", "body v2")
				f.AddComment(1, "owner", "too long")
			},
			sentBack("1002"), "stage:clarifying"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, d := reviewing(t)
			tt.change(t, f, d)
			iterate(t, d)
			got := f.State().Issues[0]
			if last := got.Comments[len(got.Comments)-1].Body; last != tt.last || !slices.Equal(got.Labels, []string{tt.label}) {
				t.Errorf("newest comment %q and labels %v, want %q and %s", last, got.Labels, tt.last, tt.label)
			}
		})
	}
}
