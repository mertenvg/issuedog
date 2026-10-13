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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
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

func gitIn(t testing.TB, dir string, args ...string) {
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

// priority stands in for #81's /priority handler: it sets the label, drops
// the other priority labels, then 👍s.
var priority = daemon.Handler{Apply: func(ctx context.Context, a *daemon.Applier, c daemon.Cmd) error {
	want := "priority:" + c.Arg
	if !slices.Contains(c.S.Labels, want) {
		if _, _, err := a.Client.Issues.AddLabelsToIssue(ctx, a.Owner, a.Repo, c.S.Number, []string{want}); err != nil {
			return err
		}
		c.S.Labels = append(c.S.Labels, want)
	}
	for _, l := range slices.Clone(c.S.Labels) {
		if strings.HasPrefix(l, "priority:") && l != want {
			if _, err := a.Client.Issues.RemoveLabelForIssue(ctx, a.Owner, a.Repo, c.S.Number, l); err != nil {
				return err
			}
			c.S.Labels = slices.DeleteFunc(c.S.Labels, func(x string) bool { return x == l })
		}
	}
	return a.Ack(ctx, c.Comment.ID)
}}

// newDaemon is a fresh daemon of o/r on f, cloning o, with the /priority
// handler. Its Reactions hook adds each issue it sees to *visits.
func newDaemon(t testing.TB, f *githubtest.Fake, o *origin, visits *[]int) *daemon.Daemon {
	t.Helper()
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
	st := &daemon.Step{Applier: a, Commands: map[string]daemon.Handler{"priority": priority},
		Reactions: func(_ context.Context, _ *daemon.Applier, s *github.Snapshot, _ daemon.Dirt) error {
			if visits != nil {
				*visits = append(*visits, s.Number)
			}
			return nil
		}}
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
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	labels := func() []string { return f.State().Issues[0].Labels }

	f.AddComment(1, "mallory", "/priority p0") // untrusted: ignored
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
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
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

func TestRunStopsWhenCanceled(t *testing.T) {
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
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
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't iterate")
	}
	// The config on main sleeps 7s, so Run is asleep.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't stop when its context was canceled")
	}
	if iterations != 1 {
		t.Errorf("Run iterated %d times, want once before its 7s sleep", iterations)
	}
}

func TestCurrentTrust(t *testing.T) {
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
	for _, tt := range []struct {
		name   string
		change func(t *testing.T, f *githubtest.Fake, d *daemon.Daemon)
		last   string // the newest comment
		label  string
	}{
		{"the owner's edit posts the next revision",
			func(_ *testing.T, f *githubtest.Fake, _ *daemon.Daemon) { f.EditBody(1, "owner", "body v2") },
			posted(2, "body v2", "", "", "1001"), "stage:spec-review"},
		{"mallory dirtying the newest revision posts it again",
			func(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
				f.EditBody(1, "owner", "body v2")
				iterate(t, d) // posts revision 2, comment 1002
				f.EditComment(1002, "mallory", "nothing to approve here")
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
