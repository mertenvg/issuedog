package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

// pullRemote is a bare git repo in a temporary directory, for SetGitRemote,
// whose branches tests move with commits that change nothing, at a fixed
// date, so the same steps give the same SHAs.
type pullRemote struct {
	t   testing.TB
	dir string
	n   int // commits made, which tells their messages apart
}

func newPullRemote(t testing.TB) *pullRemote {
	t.Helper()
	r := &pullRemote{t: t, dir: filepath.Join(t.TempDir(), "remote.git")}
	r.git("init", "--bare", "--quiet", "--initial-branch=main", r.dir)
	r.commit("main")
	return r
}

// git runs git on the repo with no system or global config, and returns
// its trimmed stdout.
func (r *pullRemote) git(args ...string) string {
	r.t.Helper()
	return r.gitStdin("", args...)
}

// gitStdin runs git on the repo as git does, with stdin as its input.
func (r *pullRemote) gitStdin(stdin string, args ...string) string {
	r.t.Helper()
	cmd := exec.Command("git", append([]string{"--git-dir=" + r.dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
	out, err := cmd.Output()
	if err != nil {
		r.t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// commit moves branch on by one commit, or starts it with one, and returns
// its SHA.
func (r *pullRemote) commit(branch string) string {
	r.t.Helper()
	args := []string{"commit-tree", r.git("mktree"), "-m", fmt.Sprintf("commit %d", r.n)}
	if tip := r.tip(branch); tip != "" {
		args = append(args, "-p", tip)
	}
	return r.set(branch, args)
}

// force points branch at a new commit that has none of its history, as a
// push by force does, and returns its SHA.
func (r *pullRemote) force(branch string) string {
	r.t.Helper()
	return r.set(branch, []string{"commit-tree", r.git("mktree"), "-m", fmt.Sprintf("commit %d", r.n)})
}

// write moves branch on by one commit whose tree is its tip's with each
// file of files, a top-level path, set to its content, and returns its
// SHA.
func (r *pullRemote) write(branch string, files map[string]string) string {
	r.t.Helper()
	tip := r.tip(branch)
	var entries []string
	for line := range strings.Lines(r.git("ls-tree", tip)) {
		_, path, _ := strings.Cut(strings.TrimRight(line, "\n"), "\t")
		if _, set := files[path]; !set {
			entries = append(entries, strings.TrimRight(line, "\n"))
		}
	}
	for path, content := range files {
		entries = append(entries, "100644 blob "+r.gitStdin(content, "hash-object", "-w", "--stdin")+"\t"+path)
	}
	tree := r.gitStdin(strings.Join(entries, "\n")+"\n", "mktree")
	return r.set(branch, []string{"commit-tree", tree, "-p", tip, "-m", fmt.Sprintf("commit %d", r.n)})
}

// files is the content of each file in commit's tree, by path.
func (r *pullRemote) files(commit string) map[string]string {
	r.t.Helper()
	out := map[string]string{}
	for path := range strings.Lines(r.git("ls-tree", "--name-only", commit)) {
		path = strings.TrimRight(path, "\n")
		out[path] = r.git("show", commit+":"+path)
	}
	return out
}

func (r *pullRemote) set(branch string, commitTree []string) string {
	r.t.Helper()
	r.n++
	sha := r.git(commitTree...)
	r.git("update-ref", "refs/heads/"+branch, sha)
	return sha
}

// tip is branch's tip, or "" if there's no such branch.
func (r *pullRemote) tip(branch string) string {
	r.t.Helper()
	out, err := exec.Command("git", "--git-dir="+r.dir, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// pullFake is a fake of o/r whose git remote is a new pullRemote with
// main and the branches named, each one commit past main.
func pullFake(t testing.TB, branches ...string) (*Fake, *pullRemote) {
	t.Helper()
	f, r := New(t, "o/r", "issuedog"), newPullRemote(t)
	for _, b := range branches {
		r.git("update-ref", "refs/heads/"+b, r.tip("main"))
		r.commit(b)
	}
	f.SetGitRemote(r.dir)
	return f, r
}

// pullDo sends a request to the fake as the App, decodes a 2xx answer into
// out, if any, and returns the status and the message of any other, with
// the message of each entry in its errors list after it.
func pullDo(t *testing.T, f *Fake, method, path string, body, out any) (int, string) {
	t.Helper()
	c := f.Client(t)
	req, err := c.NewRequest(context.Background(), method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req, out)
	if er := (*gogithub.ErrorResponse)(nil); errors.As(err, &er) {
		msg := er.Message
		for _, e := range er.Errors {
			msg += ": " + e.Message
		}
		return resp.StatusCode, msg
	}
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, ""
}

// pullOpen opens a pull request from head to base as the App, and returns
// its number.
func pullOpen(t *testing.T, f *Fake, title, head, base, body string) int {
	t.Helper()
	var pr gogithub.PullRequest
	if status, msg := pullDo(t, f, "POST", "repos/o/r/pulls", map[string]string{"title": title, "head": head, "base": base, "body": body}, &pr); status != http.StatusCreated {
		t.Fatalf("open %s to %s: %d %s", head, base, status, msg)
	}
	return pr.GetNumber()
}

// pullList lists the pull requests at query, as "#<number> <head>-><base>
// <state>", in the order served.
func pullList(t *testing.T, f *Fake, query string) []string {
	t.Helper()
	var prs []*gogithub.PullRequest
	if status, msg := pullDo(t, f, "GET", "repos/o/r/pulls?"+query, nil, &prs); status != http.StatusOK {
		t.Fatalf("list %s: %d %s", query, status, msg)
	}
	var got []string
	for _, p := range prs {
		got = append(got, fmt.Sprintf("#%d %s->%s %s", p.GetNumber(), p.GetHead().GetRef(), p.GetBase().GetRef(), p.GetState()))
	}
	return got
}

// The list serves the pull requests the fake opened, newest first, open
// ones by default, with the state, head and base filters, and never an
// issue SetPullRequest marked, which has no head. A head with no owner,
// and a state GitHub doesn't have, are refused.
func TestPullsListFilters(t *testing.T) {
	t.Parallel()
	f, _ := pullFake(t, "a", "b", "dev")
	a := pullOpen(t, f, "A", "a", "main", "")
	f.SetPullRequest(f.AddIssue("owner", "Marked", ""))
	b := pullOpen(t, f, "B", "o:b", "main", "")
	aDev := pullOpen(t, f, "A to dev", "a", "dev", "")
	f.ClosePR(b, "owner")
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"", []string{fmt.Sprintf("#%d a->dev open", aDev), fmt.Sprintf("#%d a->main open", a)}},
		{"state=open", []string{fmt.Sprintf("#%d a->dev open", aDev), fmt.Sprintf("#%d a->main open", a)}},
		{"state=closed", []string{fmt.Sprintf("#%d b->main closed", b)}},
		{"state=all", []string{fmt.Sprintf("#%d a->dev open", aDev), fmt.Sprintf("#%d b->main closed", b), fmt.Sprintf("#%d a->main open", a)}},
		{"state=all&head=o:b", []string{fmt.Sprintf("#%d b->main closed", b)}},
		{"state=all&head=O:a&base=main", []string{fmt.Sprintf("#%d a->main open", a)}},
		{"head=o:b", nil},
		{"state=all&base=dev", []string{fmt.Sprintf("#%d a->dev open", aDev)}},
		{"state=all&head=x:a", nil},
		{"state=all&head=o:c", nil},
	} {
		if got := pullList(t, f, tc.query); !slices.Equal(got, tc.want) {
			t.Errorf("?%s: %q; want %q", tc.query, got, tc.want)
		}
	}
	for _, query := range []string{"head=a", "state=merged"} {
		if status, _ := pullDo(t, f, "GET", "repos/o/r/pulls?"+query, nil, nil); status != http.StatusUnprocessableEntity {
			t.Errorf("?%s: %d; want 422", query, status)
		}
	}
	// Two pages of one.
	if got := pullList(t, f, "state=all&per_page=1&page=2"); !slices.Equal(got, []string{fmt.Sprintf("#%d b->main closed", b)}) {
		t.Errorf("page 2 of 1: %q; want #%d", got, b)
	}
}

// A pull request's head is its branch's tip, read when it's opened,
// updated and listed while open. A closed one keeps the SHA it had, and a
// reopen reads it again. State has each one's head, base, title, body,
// state, merge and merger, by number.
func TestPullsHeadFromGit(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a")
	var pr gogithub.PullRequest
	pullDo(t, f, "POST", "repos/o/r/pulls", map[string]string{"title": "T", "head": "a", "base": "main", "body": "B"}, &pr)
	if got, want := pr.GetHead().GetSHA(), r.tip("a"); got != want || pr.GetUser().GetLogin() != "issuedog[bot]" || pr.GetMerged() {
		t.Errorf("opened: head %s, by %s, merged %v; want %s, by the App, unmerged", got, pr.GetUser().GetLogin(), pr.GetMerged(), want)
	}
	n := pr.GetNumber()
	head := func() string {
		t.Helper()
		var prs []*gogithub.PullRequest
		pullDo(t, f, "GET", "repos/o/r/pulls?state=all", nil, &prs)
		if len(prs) != 1 {
			t.Fatalf("listed %d pull requests; want 1", len(prs))
		}
		return prs[0].GetHead().GetSHA()
	}
	pushed := r.commit("a")
	if got := head(); got != pushed {
		t.Errorf("listed after a push: head %s; want %s", got, pushed)
	}
	pushed = r.commit("a")
	if pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", n), map[string]string{"title": "T2"}, &pr); pr.GetHead().GetSHA() != pushed || pr.GetTitle() != "T2" {
		t.Errorf("updated after a push: head %s, title %q; want %s, T2", pr.GetHead().GetSHA(), pr.GetTitle(), pushed)
	}
	f.ClosePR(n, "owner")
	r.commit("a")
	if got := head(); got != pushed {
		t.Errorf("listed closed after a push: head %s; want the head it closed with, %s", got, pushed)
	}
	want := []PRState{{Number: n, Head: "a", Base: "main", HeadSHA: pushed, Title: "T2", Body: "B", State: "closed"}}
	if got := f.State().PRs; !reflect.DeepEqual(got, want) {
		t.Errorf("State().PRs = %+v; want %+v", got, want)
	}
	f.ReopenPR(n, "owner")
	if got := head(); got != r.tip("a") {
		t.Errorf("listed reopened: head %s; want the tip, %s", got, r.tip("a"))
	}
	f.MergePR(n, "owner")
	want[0].HeadSHA, want[0].State, want[0].Merged, want[0].MergedBy = r.tip("a"), "closed", true, "owner"
	if got := f.State().PRs; !reflect.DeepEqual(got, want) {
		t.Errorf("State().PRs merged = %+v; want %+v", got, want)
	}
	pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", n), map[string]string{"body": "B2"}, &pr)
	if !pr.GetMerged() || pr.GetMergedBy().GetLogin() != "owner" || pr.GetBody() != "B2" {
		t.Errorf("a merged one's update: merged %v by %q, body %q; want merged by owner, B2", pr.GetMerged(), pr.GetMergedBy().GetLogin(), pr.GetBody())
	}
}

// The fake refuses, with a 422 and changing nothing, a second open pull
// request with one head and base, one whose head is no branch or in
// another repo, and a reopen once the branch was pushed past its head by
// force, once it was deleted, of a merged one, and of one whose head and
// base another open one has. A reopen
// after a fast-forward isn't refused. A remote that can't be read, or none,
// is a 500 that names it.
func TestPullsRefusals(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a", "b", "c")
	a := pullOpen(t, f, "A", "a", "main", "")
	before := f.State()
	open := func(head, base string) (int, string) {
		return pullDo(t, f, "POST", "repos/o/r/pulls", map[string]string{"title": "T", "head": head, "base": base}, nil)
	}
	if status, msg := open("a", "main"); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "already exists") {
		t.Errorf("a second open one: %d %q; want 422, already exists", status, msg)
	}
	if status, msg := open("nope", "main"); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "nope") {
		t.Errorf("no such head: %d %q; want 422 naming it", status, msg)
	}
	if status, msg := open("x:b", "main"); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "isn't in this repo") {
		t.Errorf("a head in another repo: %d %q; want 422, isn't in this repo", status, msg)
	}
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the refused opens:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
	reopened := 0 // each reopen asks for a new title, so a refused one that changed it shows
	reopen := func(n int) (int, string) {
		reopened++
		return pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", n), map[string]string{"state": "open", "title": fmt.Sprintf("New %d", reopened)}, nil)
	}
	f.ClosePR(a, "owner")
	r.commit("a")
	if status, msg := reopen(a); status != http.StatusOK {
		t.Errorf("reopen after a fast-forward: %d %q; want 200", status, msg)
	}
	f.ClosePR(a, "owner")
	r.force("a")
	before = f.State()
	if status, msg := reopen(a); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "force-pushed") {
		t.Errorf("reopen after a push by force: %d %q; want 422, force-pushed", status, msg)
	}
	other := pullOpen(t, f, "A again", "a", "main", "")
	b := pullOpen(t, f, "B", "b", "main", "")
	f.ClosePR(other, "owner")
	f.MergePR(b, "owner")
	r.commit("a")
	a2 := pullOpen(t, f, "A2", "a", "main", "")
	if status, msg := reopen(other); status != http.StatusUnprocessableEntity || !strings.Contains(msg, fmt.Sprintf("#%d", a2)) {
		t.Errorf("reopen beside another open one: %d %q; want 422 naming #%d", status, msg, a2)
	}
	if status, msg := reopen(b); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "merged") {
		t.Errorf("reopen a merged one: %d %q; want 422, merged", status, msg)
	}
	if got := f.State().PRs[0]; !reflect.DeepEqual(got, before.PRs[0]) {
		t.Errorf("the refused reopen changed #%d: %+v; want %+v", a, got, before.PRs[0])
	}
	gone := pullOpen(t, f, "C", "c", "main", "")
	f.ClosePR(gone, "owner")
	r.git("update-ref", "-d", "refs/heads/c")
	if status, msg := reopen(gone); status != http.StatusUnprocessableEntity || !strings.Contains(msg, "c branch was deleted") {
		t.Errorf("reopen with its branch deleted: %d %q; want 422, deleted", status, msg)
	}
	if status, msg := pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", a2), map[string]string{"state": "merged"}, nil); status != http.StatusUnprocessableEntity {
		t.Errorf("state=merged: %d %q; want 422", status, msg)
	}
	if status, _ := pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", f.AddIssue("owner", "Issue", "")), map[string]string{"title": "x"}, nil); status != http.StatusNotFound {
		t.Errorf("PATCH /pulls of an issue: %d; want 404", status)
	}

	// A branch gone, an unreadable remote, and none at all.
	r.git("update-ref", "-d", "refs/heads/a")
	if status, msg := pullDo(t, f, "GET", "repos/o/r/pulls", nil, nil); status != http.StatusInternalServerError || !strings.Contains(msg, "no branch a") {
		t.Errorf("list with a branch gone: %d %q; want 500 naming it", status, msg)
	}
	f.SetGitRemote(filepath.Join(t.TempDir(), "missing.git"))
	if status, msg := open("b", "dev"); status != http.StatusInternalServerError || !strings.Contains(msg, "missing.git") {
		t.Errorf("open with no repo there: %d %q; want 500 naming it", status, msg)
	}
	bare := New(t, "o/r", "issuedog")
	if status, msg := pullDo(t, bare, "POST", "repos/o/r/pulls", map[string]string{"title": "T", "head": "a", "base": "main"}, nil); status != http.StatusInternalServerError || !strings.Contains(msg, "SetGitRemote") {
		t.Errorf("open with no remote set: %d %q; want 500 naming SetGitRemote", status, msg)
	}
}

// A pull request is an issue too: the issue list gives it with its
// pull_request links, merged_at once it merged, and its labels and
// assignees, which the issue writes set, and its timeline gives its
// merged, closed and reopened events, each naming who made it.
func TestPullsAsIssues(t *testing.T) {
	t.Parallel()
	f, _ := pullFake(t, "a")
	n := pullOpen(t, f, "A", "a", "main", "")
	c, ctx := f.Client(t), context.Background()
	if _, _, err := c.Issues.AddLabelsToIssue(ctx, "o", "r", n, []string{"priority:p1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddAssignees(ctx, "o", "r", n, []string{"owner"}); err != nil {
		t.Fatal(err)
	}
	f.ClosePR(n, "mallory")
	f.ReopenPR(n, "owner")
	f.MergePR(n, "owner")
	issues, _, err := c.Issues.ListByRepo(ctx, "o", "r", &gogithub.IssueListByRepoOptions{State: "all"})
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !issues[0].IsPullRequest() || issues[0].GetPullRequestLinks().MergedAt == nil ||
		issues[0].GetState() != "closed" || len(issues[0].Labels) != 1 || len(issues[0].Assignees) != 1 {
		t.Fatalf("issues %+v; want the merged pull request, closed, with its label and assignee", issues)
	}
	var prs []*gogithub.PullRequest
	pullDo(t, f, "GET", "repos/o/r/pulls?state=closed", nil, &prs)
	if len(prs) != 1 || prs[0].Merged != nil || prs[0].MergedAt == nil || len(prs[0].Labels) != 1 || len(prs[0].Assignees) != 1 {
		t.Errorf("listed %+v; want it with merged_at, its label and assignee, and no merged field, as GitHub's list", prs)
	}
	req, err := c.NewRequest(ctx, "GET", fmt.Sprintf("repos/o/r/issues/%d/timeline", n), nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []*gogithub.Timeline
	if _, err := c.Do(req, &events); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.GetEvent()+" "+e.GetActor().GetLogin())
	}
	want := []string{"labeled issuedog[bot]", "closed mallory", "reopened owner", "merged owner", "closed owner"}
	if !slices.Equal(got, want) {
		t.Errorf("timeline %q; want %q", got, want)
	}
}

// A pull request's updated_at moves as GitHub's does: on a push to its
// branch while it's open, which the issue list sees, as /pulls does, and
// on a close and a reopen, so a poll since its last updated_at lists it
// for each. A listing with no change moves nothing, and a push while it's
// closed moves nothing either.
func TestPullsUpdatedAt(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a")
	n := pullOpen(t, f, "A", "a", "main", "")
	updated := func() time.Time {
		t.Helper()
		var issues []*gogithub.Issue
		pullDo(t, f, "GET", "repos/o/r/issues?state=all", nil, &issues)
		if len(issues) != 1 || issues[0].GetNumber() != n {
			t.Fatalf("listed %+v; want the pull request alone", issues)
		}
		return issues[0].GetUpdatedAt().Time
	}
	last := updated()
	for _, tt := range []struct {
		name  string
		act   func()
		moves bool
	}{
		{"nothing", func() {}, false},
		{"a push", func() { r.commit("a") }, true},
		{"a listing after the push", func() {}, false},
		{"a push listed by /pulls first", func() {
			r.commit("a")
			var prs []*gogithub.PullRequest
			pullDo(t, f, "GET", "repos/o/r/pulls", nil, &prs)
		}, true},
		{"a close", func() { f.ClosePR(n, "owner") }, true},
		{"a push while closed", func() { r.commit("a") }, false},
		{"a reopen", func() { f.ReopenPR(n, "owner") }, true},
	} {
		tt.act()
		got := updated()
		if moved := got.After(last); moved != tt.moves {
			t.Errorf("%s: updated_at %s, after %s: moved %t; want %t", tt.name, got, last, moved, tt.moves)
		}
		last = got
	}
}

// OpenPRAs opens a pull request as someone other than the App, from a
// branch to a base, which the list serves with its author and head, and
// which keeps the App from opening its own for that head and base, as
// GitHub does, with the 422 whose errors list says one exists. A second
// one for that head and base fails the test, naming why.
func TestOpenPRAs(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f, r := pullFake(tb, "a")
	n := f.OpenPRAs("mallory", "a", "main", "Theirs", "Mine.")
	var prs []*gogithub.PullRequest
	pullDo(t, f, "GET", "repos/o/r/pulls?head=o:a", nil, &prs)
	if len(prs) != 1 || prs[0].GetNumber() != n || prs[0].GetUser().GetLogin() != "mallory" || prs[0].GetHead().GetSHA() != r.tip("a") ||
		prs[0].GetTitle() != "Theirs" || prs[0].GetBody() != "Mine." || prs[0].GetBase().GetRef() != "main" {
		t.Errorf("listed %+v; want mallory's, from a at %s to main, titled Theirs", prs, r.tip("a"))
	}
	status, msg := pullDo(t, f, "POST", "repos/o/r/pulls", map[string]string{"title": "T", "head": "a", "base": "main"}, nil)
	if status != http.StatusUnprocessableEntity || msg != "Validation Failed: A pull request already exists for o:a." {
		t.Errorf("the App's open: %d %q; want 422, Validation Failed, with one in its errors list", status, msg)
	}
	fatal, panicked := fakeNumberFatal(tb, func() { f.OpenPRAs("eve", "a", "main", "Again", "") })
	if panicked != nil || len(fatal) != 1 || !strings.HasPrefix(fatal[0], "githubtest: open a pull request from a to main as eve: 422 ") ||
		!strings.Contains(fatal[0], "already exists") {
		t.Errorf("a second open: Fatalf %q, panicked with %v; want one Fatalf naming the 422", fatal, panicked)
	}
}

// A close or a reopen through PATCH /pulls names the App as its actor in
// the timeline, as GitHub names the token's user, so a reader can tell the
// App's own closes and reopens from a person's.
func TestPullsPatchActor(t *testing.T) {
	t.Parallel()
	f, _ := pullFake(t, "a")
	n := pullOpen(t, f, "A", "a", "main", "")
	for _, state := range []string{"closed", "open"} {
		if status, msg := pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", n), map[string]string{"state": state}, nil); status != http.StatusOK {
			t.Fatalf("PATCH state=%s: %d %q; want 200", state, status, msg)
		}
	}
	c, ctx := f.Client(t), context.Background()
	req, err := c.NewRequest(ctx, "GET", fmt.Sprintf("repos/o/r/issues/%d/timeline", n), nil)
	if err != nil {
		t.Fatal(err)
	}
	var events []*gogithub.Timeline
	if _, err := c.Do(req, &events); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.GetEvent()+" "+e.GetActor().GetLogin())
	}
	if want := []string{"closed issuedog[bot]", "reopened issuedog[bot]"}; !slices.Equal(got, want) {
		t.Errorf("timeline %q; want %q", got, want)
	}
}

// A merge into main closes the issue the body's first line names with a
// closing keyword, as CloseByPullRequest does, the merger as its closer:
// not one named on a later line, nor by a merge into another branch, nor
// a pull request.
func TestMergePRCloses(t *testing.T) {
	t.Parallel()
	f, _ := pullFake(t, "a", "b", "c", "d", "dev")
	issue := f.AddIssue("owner", "Task", "")
	later := f.AddIssue("owner", "Later", "")
	other := f.AddIssue("owner", "Other", "")
	toDev := pullOpen(t, f, "To dev", "b", "dev", fmt.Sprintf("Closes #%d", other))
	notFirst := pullOpen(t, f, "Not first", "c", "main", fmt.Sprintf("Something\nCloses #%d", later))
	aPR := pullOpen(t, f, "A PR", "d", "main", "")
	closesPR := pullOpen(t, f, "Closes a PR", "dev", "main", fmt.Sprintf("Closes #%d", aPR))
	main := pullOpen(t, f, "Main", "a", "main", fmt.Sprintf("fixes #%d\n\nWhat changed", issue))
	for _, n := range []int{toDev, notFirst, closesPR} {
		if f.MergePR(n, "owner") == 0 {
			t.Errorf("merging #%d gave no closed event", n)
		}
	}
	if f.MergePR(toDev, "owner") != 0 {
		t.Error("merging a merged pull request again gave a closed event")
	}
	f.MergePR(main, "owner")
	closed := map[int]bool{}
	for _, is := range f.State().Issues {
		closed[is.Number] = is.Closed
	}
	if !closed[issue] || closed[later] || closed[other] || closed[aPR] {
		t.Errorf("closed %v; want only #%d", closed, issue)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.issueNumbered(issue)
	e := is.labelEvents[len(is.labelEvents)-1]
	if got, want := is.closingClosers[e.GetID()], (closingCloser{reason: "completed", typename: "PullRequest", merged: true, base: "main", repo: "o/r"}); e.GetEvent() != "closed" ||
		e.GetActor().GetLogin() != "owner" || !reflect.DeepEqual(got, want) {
		t.Errorf("the close: %s by %s, closer %+v; want closed by owner, closer %+v", e.GetEvent(), e.GetActor().GetLogin(), got, want)
	}
}

// The helpers fail the test, naming why, for an issue the fake didn't
// open as a pull request, ReopenPR for a reopen GitHub refuses, and
// MergePR for a squash GitHub refuses, as for a conflict with the base,
// which then stays where it was.
func TestPullsHelpersFail(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f, r := pullFake(tb, "a", "b", "s")
	marked := f.AddIssue("owner", "Marked", "")
	f.SetPullRequest(marked)
	n := pullOpen(t, f, "A", "a", "main", "")
	r.write("b", map[string]string{"x.txt": "b's"})
	story := r.write("s", map[string]string{"x.txt": "s's"})
	conflict := pullOpen(t, f, "B", "b", "s", "")
	for _, tc := range []struct {
		name string
		do   func()
		want string
	}{
		{"MergePR", func() { f.MergePR(marked, "owner") }, fmt.Sprintf("githubtest: #%d is no pull request the fake opened", marked)},
		{"ClosePR", func() { f.ClosePR(marked, "owner") }, fmt.Sprintf("githubtest: #%d is no pull request the fake opened", marked)},
		{"ReopenPR", func() { f.ReopenPR(marked, "owner") }, fmt.Sprintf("githubtest: #%d is no pull request the fake opened", marked)},
		{"ReopenPR forced", func() {
			f.ClosePR(n, "owner")
			r.force("a")
			f.ReopenPR(n, "owner")
		}, fmt.Sprintf("githubtest: reopen #%d: 422 map[message:Validation Failed: state cannot be changed. The a branch was force-pushed or recreated.]", n)},
		{"MergePR conflict", func() { f.MergePR(conflict, "owner") },
			fmt.Sprintf("githubtest: merge #%d into s: 405 map[githubtest:b conflicts with s message:Pull Request is not mergeable]", conflict)},
	} {
		fatal, panicked := fakeNumberFatal(tb, tc.do)
		if panicked != nil || !slices.Equal(fatal, []string{tc.want}) {
			t.Errorf("%s: Fatalf %q, panicked with %v; want Fatalf %q", tc.name, fatal, panicked, tc.want)
		}
	}
	if got := r.tip("s"); got != story {
		t.Errorf("s moved to %s on a refused merge; want it left at %s", got, story)
	}
}

// A push to an open pull request's branch, then the issue list with
// since= from after its last change, lists the pull request as changed,
// as GitHub is believed to (SPEC §16): the list reads its head again
// (pullFollow), which moves its updated_at. The pull request then shows
// the new head. With no push, that list is empty.
func TestPullsPushListedSince(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a")
	n := pullOpen(t, f, "A", "a", "main", "")
	c, ctx := f.Client(t), t.Context()
	all, err := c.IssuesSince(ctx, "o", "r", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].GetNumber() != n {
		t.Fatalf("listed %+v; want the pull request alone", all)
	}
	since := all[0].GetUpdatedAt().Add(time.Second)
	f.Advance(time.Second)
	if quiet, err := c.IssuesSince(ctx, "o", "r", since); err != nil || len(quiet) != 0 {
		t.Fatalf("listed since %s with no push: %+v, %v; want none", since, quiet, err)
	}
	pushed := r.commit("a")
	changed, err := c.IssuesSince(ctx, "o", "r", since)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 || changed[0].GetNumber() != n || !changed[0].IsPullRequest() || changed[0].GetUpdatedAt().Before(since) {
		t.Fatalf("listed since %s after a push: %+v; want the pull request, updated since", since, changed)
	}
	prs, err := c.PullRequestsByHead(ctx, "o", "r", "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || prs[0].Number != n || prs[0].HeadSHA != pushed || !prs[0].UpdatedAt.Equal(changed[0].GetUpdatedAt().Time) {
		t.Errorf("pull requests from a %+v; want #%d at the pushed head %s, updated as listed", prs, n, pushed)
	}
}

// pullSquashStory is a fake of o/r whose git remote has main, the story
// branch issuedog/5-story one commit past it, and the task branch
// issuedog/7-task one commit past the story branch, writing task.txt, and
// the App's open pull request from the task branch into the story
// branch. It returns the fake, its remote, the pull request's number and
// its head.
func pullSquashStory(t *testing.T) (*Fake, *pullRemote, int, string) {
	t.Helper()
	f, r := pullFake(t, "issuedog/5-story")
	r.git("update-ref", "refs/heads/issuedog/7-task", r.tip("issuedog/5-story"))
	head := r.write("issuedog/7-task", map[string]string{"task.txt": "the task"})
	n := pullOpen(t, f, "feat: the task", "issuedog/7-task", "issuedog/5-story", "Task #7, in story #5.")
	return f, r, n, head
}

// pullMergeEvents are the events on pull request n's timeline, as "<event>
// <actor>", oldest first.
func pullMergeEvents(t *testing.T, f *Fake, n int) []string {
	t.Helper()
	var events []*gogithub.Timeline
	if status, msg := pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/issues/%d/timeline", n), nil, &events); status != http.StatusOK {
		t.Fatalf("the timeline of #%d: %d %s", n, status, msg)
	}
	var got []string
	for _, e := range events {
		got = append(got, e.GetEvent()+" "+e.GetActor().GetLogin())
	}
	return got
}

// A squash-merge through PUT /pulls/{n}/merge, onto a story branch that
// moved since the head branched, writes one commit onto it: its only
// parent is the branch's tip at the merge, its tree the merge of that tip
// and the head, its message the commit_title and commit_message sent, its
// author the App and its committer GitHub. The answer names it. GET
// /pulls/{n} then serves the pull request closed, merged by the App, with
// the head it merged and the commit as merge_commit_sha, and its timeline
// has a merged and a closed event, each naming the App. The head branch
// stays as it was, and State has both branches with their tips and the
// App as the merger.
func TestPullsSquashMovedBase(t *testing.T) {
	t.Parallel()
	f, r, n, head := pullSquashStory(t)
	moved := r.write("issuedog/5-story", map[string]string{"sibling.txt": "a sibling's merge"})
	var res gogithub.PullRequestMergeResult
	status, msg := pullDo(t, f, "PUT", fmt.Sprintf("repos/o/r/pulls/%d/merge", n), map[string]string{"merge_method": "squash", "sha": head,
		"commit_title": "feat: the task (#2)", "commit_message": "Task #7, in story #5."}, &res)
	tip := r.tip("issuedog/5-story")
	if status != http.StatusOK || res.GetSHA() != tip || !res.GetMerged() || tip == moved {
		t.Fatalf("merge: %d %q, %+v; want 200, merged, naming the story branch's new tip %s", status, msg, res, tip)
	}
	if got, want := r.git("rev-list", "--parents", "-n", "1", tip), tip+" "+moved; got != want {
		t.Errorf("the squash commit's parents %q; want %q, the moved tip alone", got, want)
	}
	if got, want := r.files(tip), map[string]string{"sibling.txt": "a sibling's merge", "task.txt": "the task"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the squash commit's files %v; want %v, the merge of the tip and the head", got, want)
	}
	if got, want := r.git("log", "-1", "--format=%B|%an <%ae> %aI|%cn <%ce> %cI", tip),
		"feat: the task (#2)\n\nTask #7, in story #5.\n|issuedog[bot] <issuedog[bot]@users.noreply.github.com> 2026-01-01T00:00:00+00:00|GitHub <noreply@github.com> 2026-01-01T00:00:00+00:00"; got != want {
		t.Errorf("the squash commit's message, author and committer, each dated Start, %q; want %q", got, want)
	}
	if got := r.tip("issuedog/7-task"); got != head {
		t.Errorf("the head branch's tip %s; want it left at %s", got, head)
	}
	var pr gogithub.PullRequest
	if status, msg := pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d", n), nil, &pr); status != http.StatusOK || pr.GetState() != "closed" ||
		!pr.GetMerged() || pr.GetMergedBy().GetLogin() != "issuedog[bot]" || pr.GetMergeCommitSHA() != tip || pr.GetHead().GetSHA() != head ||
		pr.MergedAt == nil {
		t.Errorf("read after the merge: %d %q, %+v; want closed, merged by the App at %s, merge_commit_sha %s", status, msg, pr, head, tip)
	}
	if got, want := pullMergeEvents(t, f, n), []string{"merged issuedog[bot]", "closed issuedog[bot]"}; !slices.Equal(got, want) {
		t.Errorf("timeline %q; want %q", got, want)
	}
	s := f.State()
	if want := []BranchState{{"issuedog/5-story", tip}, {"issuedog/7-task", head}}; !reflect.DeepEqual(s.Branches, want) {
		t.Errorf("State().Branches %+v; want %+v", s.Branches, want)
	}
	if len(s.PRs) != 1 || !s.PRs[0].Merged || s.PRs[0].MergedBy != "issuedog[bot]" {
		t.Errorf("State().PRs %+v; want #%d merged by the App", s.PRs, n)
	}
}

// With no commit_title, the squash commit's title is GitHub's default, the
// pull request's title and number, and with no commit_message, its
// message is the pull request's body. An empty commit_message leaves the
// title alone. Nothing but the method is needed: a merge with no sha
// isn't checked against a head.
func TestPullsSquashMessage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  map[string]any
		want string
	}{
		{"defaults", map[string]any{"merge_method": "squash"}, "feat: the task (#1)\n\nTask #7, in story #5.\n"},
		{"empty message", map[string]any{"merge_method": "squash", "commit_title": "T", "commit_message": ""}, "T\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, r, n, _ := pullSquashStory(t)
			if status, msg := pullDo(t, f, "PUT", fmt.Sprintf("repos/o/r/pulls/%d/merge", n), tc.req, nil); status != http.StatusOK {
				t.Fatalf("merge: %d %q; want 200", status, msg)
			}
			if got := r.git("log", "-1", "--format=%B", r.tip("issuedog/5-story")) + "\n"; got != tc.want {
				t.Errorf("the squash commit's message %q; want %q", got, tc.want)
			}
		})
	}
}

// The merge is refused as GitHub refuses it, with nothing merged, the
// story branch and the pull request left as they were, and the request
// logged as a write: a 405 for a method other than squash, none included,
// a closed or merged pull request, a base branch gone, and a head that
// conflicts with the base; a 409 for a sha that isn't the head, which is
// checked before the conflict; a 404 for an issue that's no pull request;
// and a 500 naming SetGitRemote with no git remote.
func TestPullsMergeRefusals(t *testing.T) {
	t.Parallel()
	squash := func(head string) map[string]string { return map[string]string{"merge_method": "squash", "sha": head} }
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, f *Fake, r *pullRemote, n int) int // returns the number to merge
		req    func(head string) map[string]string
		status int
		msg    string
	}{
		{"method merge", nil, func(head string) map[string]string { return map[string]string{"merge_method": "merge", "sha": head} },
			http.StatusMethodNotAllowed, `githubtest: merge_method "merge" isn't allowed on this repository, which allows only squash`},
		{"method rebase", nil, func(string) map[string]string { return map[string]string{"merge_method": "rebase"} },
			http.StatusMethodNotAllowed, `githubtest: merge_method "rebase" isn't allowed on this repository, which allows only squash`},
		{"no method", nil, func(head string) map[string]string { return map[string]string{"sha": head} },
			http.StatusMethodNotAllowed, `githubtest: merge_method "" isn't allowed on this repository, which allows only squash`},
		{"closed", func(t *testing.T, f *Fake, _ *pullRemote, n int) int { f.ClosePR(n, "owner"); return n }, squash,
			http.StatusMethodNotAllowed, "Pull Request is not mergeable"},
		{"merged", func(t *testing.T, f *Fake, _ *pullRemote, n int) int {
			if status, msg := pullDo(t, f, "PUT", fmt.Sprintf("repos/o/r/pulls/%d/merge", n), map[string]string{"merge_method": "squash"}, nil); status != http.StatusOK {
				t.Fatalf("the first merge: %d %q", status, msg)
			}
			return n
		}, squash, http.StatusMethodNotAllowed, "Pull Request is not mergeable"},
		{"conflict", func(t *testing.T, f *Fake, r *pullRemote, n int) int {
			r.write("issuedog/5-story", map[string]string{"task.txt": "a sibling's version"})
			return n
		}, squash, http.StatusMethodNotAllowed, "Pull Request is not mergeable"},
		{"head moved", func(t *testing.T, f *Fake, r *pullRemote, n int) int {
			r.write("issuedog/7-task", map[string]string{"more.txt": "more"})
			return n
		}, squash, http.StatusConflict, "Head branch was modified. Review and try the merge again."},
		{"head moved and conflict", func(t *testing.T, f *Fake, r *pullRemote, n int) int {
			r.write("issuedog/5-story", map[string]string{"task.txt": "a sibling's version"})
			r.write("issuedog/7-task", map[string]string{"more.txt": "more"})
			return n
		}, squash, http.StatusConflict, "Head branch was modified. Review and try the merge again."},
		{"base gone", func(t *testing.T, f *Fake, r *pullRemote, n int) int {
			r.git("update-ref", "-d", "refs/heads/issuedog/5-story")
			return n
		}, squash, http.StatusMethodNotAllowed, "Pull Request is not mergeable"},
		{"no pull request", func(t *testing.T, f *Fake, _ *pullRemote, _ int) int { return f.AddIssue("owner", "An issue", "") }, squash,
			http.StatusNotFound, "Not Found"},
		{"no git remote", func(t *testing.T, f *Fake, _ *pullRemote, n int) int { f.SetGitRemote(""); return n }, squash,
			http.StatusInternalServerError, "call SetGitRemote"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, r, n, head := pullSquashStory(t)
			if tc.before != nil {
				n = tc.before(t, f, r, n)
			}
			tip, before, writes := r.tip("issuedog/5-story"), f.State(), len(f.Writes())
			status, msg := pullDo(t, f, "PUT", fmt.Sprintf("repos/o/r/pulls/%d/merge", n), tc.req(head), nil)
			if status != tc.status || !strings.Contains(msg, tc.msg) {
				t.Errorf("merge: %d %q; want %d %q", status, msg, tc.status, tc.msg)
			}
			if got := r.tip("issuedog/5-story"); got != tip {
				t.Errorf("the story branch moved to %s; want it left at %s", got, tip)
			}
			if got := f.State(); !reflect.DeepEqual(got, before) {
				t.Errorf("state after the refusal:\n%+v\nwant it unchanged:\n%+v", got, before)
			}
			if w := f.Writes(); len(w) != writes+1 || w[writes].Method != "PUT" || w[writes].Path != fmt.Sprintf("/pulls/%d/merge", n) {
				t.Errorf("writes %+v; want one more, the PUT", w[writes:])
			}
		})
	}
}

// MergePR into a base other than main writes the squash commit onto it,
// as a merge from the web page does: titled with the pull request's title
// and number, with its body as the message and the merger as its author,
// the base's tip as its only parent, the head read first, so a push just
// before is merged and served as its head, and the commit served as
// merge_commit_sha. A
// merge into main writes no commit, as before, and main stays where it
// was.
func TestMergePRSquash(t *testing.T) {
	t.Parallel()
	f, r, n, _ := pullSquashStory(t)
	r.git("update-ref", "refs/heads/a", r.tip("main"))
	r.commit("a")
	toMain := pullOpen(t, f, "A", "a", "main", "")
	story := r.tip("issuedog/5-story")
	pushed := r.write("issuedog/7-task", map[string]string{"more.txt": "more"})
	f.MergePR(n, "owner")
	tip := r.tip("issuedog/5-story")
	if got, want := r.git("log", "-1", "--format=%P|%B|%an", tip), story+"|feat: the task (#1)\n\nTask #7, in story #5.\n|owner"; tip == story || got != want {
		t.Errorf("the story branch's new tip %s: %q; want %q", tip, got, want)
	}
	if got, want := r.files(tip), map[string]string{"more.txt": "more", "task.txt": "the task"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the squash commit's files %v; want %v, the head pushed just before the merge", got, want)
	}
	var pr gogithub.PullRequest
	if pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d", n), nil, &pr); pr.GetMergeCommitSHA() != tip || pr.GetMergedBy().GetLogin() != "owner" ||
		pr.GetHead().GetSHA() != pushed {
		t.Errorf("read after MergePR: merge_commit_sha %q, merged_by %q, head %s; want %s, owner, %s", pr.GetMergeCommitSHA(), pr.GetMergedBy().GetLogin(),
			pr.GetHead().GetSHA(), tip, pushed)
	}
	main := r.tip("main")
	f.MergePR(toMain, "owner")
	if got := r.tip("main"); got != main {
		t.Errorf("main moved to %s on a merge into it; want it left at %s", got, main)
	}
	var intoMain gogithub.PullRequest
	if pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d", toMain), nil, &intoMain); !intoMain.GetMerged() || intoMain.MergeCommitSHA != nil {
		t.Errorf("read the merge into main: merged %v, merge_commit_sha %v; want merged with none", intoMain.GetMerged(), intoMain.MergeCommitSHA)
	}
}

// GET /pulls/{n} serves one pull request, open and unmerged, with its head
// read again from the git remote, and a 304 to a repeat with its ETag
// while nothing changed; a push changes it. An issue that's no pull
// request is a 404.
func TestPullsGetOne(t *testing.T) {
	t.Parallel()
	f, r, n, head := pullSquashStory(t)
	var pr gogithub.PullRequest
	if status, msg := pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d", n), nil, &pr); status != http.StatusOK || pr.GetState() != "open" ||
		pr.GetMerged() || pr.MergedBy != nil || pr.MergeCommitSHA != nil || pr.GetHead().GetSHA() != head || pr.GetBase().GetRef() != "issuedog/5-story" {
		t.Errorf("read: %d %q, %+v; want open, unmerged, at %s, into issuedog/5-story", status, msg, pr, head)
	}
	get := func(etag string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", f.URL()+fmt.Sprintf("/repos/o/r/pulls/%d", n), nil)
		if err != nil {
			t.Fatal(err)
		}
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		resp, err := f.Transport().RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header.Get("ETag")
	}
	status, etag := get("")
	if status != http.StatusOK || etag == "" {
		t.Fatalf("read: %d, ETag %q; want 200 with an ETag", status, etag)
	}
	if status, _ := get(etag); status != http.StatusNotModified {
		t.Errorf("repeat with nothing changed: %d; want 304", status)
	}
	pushed := r.write("issuedog/7-task", map[string]string{"more.txt": "more"})
	if status, after := get(etag); status != http.StatusOK || after == etag {
		t.Errorf("repeat after a push: %d, ETag %q; want 200 with another ETag than %q", status, after, etag)
	}
	if pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d", n), nil, &pr); pr.GetHead().GetSHA() != pushed {
		t.Errorf("read after a push: head %s; want %s", pr.GetHead().GetSHA(), pushed)
	}
	issue := f.AddIssue("owner", "An issue", "")
	if status, msg := pullDo(t, f, "GET", fmt.Sprintf("repos/o/r/pulls/%d", issue), nil, nil); status != http.StatusNotFound {
		t.Errorf("read an issue as a pull request: %d %q; want 404", status, msg)
	}
}

// A push that moves the base to the head, a fast-forward, merges the pull
// request, as GitHub marks it (SPEC §16), found when it's next listed or
// read, by GET /pulls/{n}, by the list of pull requests and by the issue
// list alike. It then reads, through Client.PullRequest as the daemon reads
// a merge, closed and merged into its base by the pusher, the App unless
// SetPusher names another, with the head as its merge commit and its head
// SHA, and its timeline has a merged and a closed event, each naming the
// pusher. State has the pusher as the merger. The open list leaves it out,
// and the closed list has it.
func TestPullsFastForward(t *testing.T) {
	t.Parallel()
	readers := map[string]func(t *testing.T, f *Fake, c *github.Client, n int){
		"get": func(t *testing.T, f *Fake, c *github.Client, n int) {
			if _, err := c.PullRequest(t.Context(), "o", "r", n); err != nil {
				t.Fatal(err)
			}
		},
		"list pulls": func(t *testing.T, f *Fake, c *github.Client, n int) {
			prs, err := c.PullRequestsByHead(t.Context(), "o", "r", "issuedog/7-task")
			if err != nil {
				t.Fatal(err)
			}
			if len(prs) != 1 || prs[0].Number != n || !prs[0].Merged || prs[0].State != "closed" {
				t.Errorf("listed %+v; want #%d merged and closed", prs, n)
			}
		},
		"list closed pulls": func(t *testing.T, f *Fake, c *github.Client, n int) {
			var prs []*gogithub.PullRequest
			if status, msg := pullDo(t, f, "GET", "repos/o/r/pulls?state=closed", nil, &prs); status != http.StatusOK {
				t.Fatalf("list closed: %d %s", status, msg)
			}
			if len(prs) != 1 || prs[0].GetNumber() != n || prs[0].MergedAt == nil {
				t.Errorf("closed pull requests %+v; want #%d, merged", prs, n)
			}
		},
		"list issues": func(t *testing.T, f *Fake, c *github.Client, n int) {
			all, err := c.IssuesSince(t.Context(), "o", "r", time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(all) != 1 || all[0].GetNumber() != n || all[0].GetState() != "closed" || all[0].GetPullRequestLinks().GetMergedAt().IsZero() {
				t.Errorf("listed %+v; want #%d closed, with merged_at", all, n)
			}
		},
	}
	for name, read := range readers {
		for _, pusher := range []string{"", "owner"} {
			t.Run(fmt.Sprintf("%s by %q", name, pusher), func(t *testing.T) {
				t.Parallel()
				f, r, n, head := pullSquashStory(t)
				want := "issuedog[bot]"
				if pusher != "" {
					f.SetPusher(pusher)
					want = pusher
				}
				r.git("update-ref", "refs/heads/issuedog/5-story", head)
				c := f.Client(t)
				read(t, f, c, n)
				pr, err := c.PullRequest(t.Context(), "o", "r", n)
				if err != nil {
					t.Fatal(err)
				}
				if !pr.Merged || pr.State != "closed" || pr.MergedBy != want || pr.MergeCommitSHA != head || pr.HeadSHA != head || pr.Base != "issuedog/5-story" {
					t.Errorf("read %+v; want closed, merged into issuedog/5-story by %s, at %s, with %s as its merge commit", pr, want, head, head)
				}
				if got, wantEvents := pullMergeEvents(t, f, n), []string{"merged " + want, "closed " + want}; !reflect.DeepEqual(got, wantEvents) {
					t.Errorf("the timeline %q; want %q", got, wantEvents)
				}
				if got := f.State().PRs; len(got) != 1 || !got[0].Merged || got[0].MergedBy != want || got[0].State != "closed" || got[0].HeadSHA != head {
					t.Errorf("State().PRs %+v; want #%d merged by %s at %s", got, n, want, head)
				}
				if got := pullList(t, f, "state=open"); len(got) != 0 {
					t.Errorf("open pull requests %q; want none", got)
				}
				if got, wantClosed := pullList(t, f, "state=closed"), []string{fmt.Sprintf("#%d issuedog/7-task->issuedog/5-story closed", n)}; !reflect.DeepEqual(got, wantClosed) {
					t.Errorf("closed pull requests %q; want %q", got, wantClosed)
				}
			})
		}
	}
}

// A push that moves the base on without the head, as a sibling's merge,
// leaves the pull request open and unmerged, and so does a head that
// merged the base's new tip in, which has the base in its history but
// isn't in the base's. Neither writes an event.
func TestPullsBaseOnlyMoved(t *testing.T) {
	t.Parallel()
	f, r, n, head := pullSquashStory(t)
	c := f.Client(t)
	moved := r.write("issuedog/5-story", map[string]string{"sibling.txt": "a sibling's merge"})
	pr, err := c.PullRequest(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Merged || pr.State != "open" || pr.MergedBy != "" || pr.MergeCommitSHA != "" || pr.HeadSHA != head {
		t.Errorf("read after the base moved: %+v; want open and unmerged at %s", pr, head)
	}
	merge := r.git("commit-tree", r.git("rev-parse", head+"^{tree}"), "-p", head, "-p", moved, "-m", "merge the story in")
	r.git("update-ref", "refs/heads/issuedog/7-task", merge)
	if got, want := pullList(t, f, "state=open"), []string{fmt.Sprintf("#%d issuedog/7-task->issuedog/5-story open", n)}; !reflect.DeepEqual(got, want) {
		t.Errorf("open pull requests after the head merged the base in %q; want %q", got, want)
	}
	if pr, err = c.PullRequest(t.Context(), "o", "r", n); err != nil || pr.Merged || pr.State != "open" || pr.HeadSHA != merge {
		t.Errorf("read after the head merged the base in: %+v, %v; want open and unmerged at %s", pr, err, merge)
	}
	if got := pullMergeEvents(t, f, n); len(got) != 0 {
		t.Errorf("the timeline %q; want no event", got)
	}
}

// A head that's a merge commit, of a newer main commit and the base's tip,
// as a conflict bug's is, merges once the base is fast-forwarded to it,
// with that merge commit as its merge_commit_sha. Before the push it's
// open, though it has the base's tip in its history.
func TestPullsFastForwardMergeHead(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "issuedog/5-story")
	newer := r.commit("main")
	story := r.write("issuedog/5-story", map[string]string{"story.txt": "the story"})
	merge := r.git("commit-tree", r.git("rev-parse", story+"^{tree}"), "-p", newer, "-p", story, "-m", "Merge main into the story")
	r.git("update-ref", "refs/heads/issuedog/9-conflict", merge)
	n := pullOpen(t, f, "fix: the conflict", "issuedog/9-conflict", "issuedog/5-story", "Bug #9, in story #5.")
	c := f.Client(t)
	pr, err := c.PullRequest(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if pr.Merged || pr.State != "open" {
		t.Errorf("read before the push: %+v; want open and unmerged", pr)
	}
	r.git("update-ref", "refs/heads/issuedog/5-story", merge, story)
	if pr, err = c.PullRequest(t.Context(), "o", "r", n); err != nil || !pr.Merged || pr.State != "closed" || pr.MergedBy != "issuedog[bot]" ||
		pr.MergeCommitSHA != merge || pr.HeadSHA != merge {
		t.Errorf("read after the fast-forward: %+v, %v; want merged by the App, with %s as its head and merge commit", pr, err, merge)
	}
}

// A pull request opened with its head in its base's history already,
// which GitHub refuses as having no commits between them and the fake
// opens, isn't merged by any push while that's its head, even as the base
// moves on. Once its head moves on and the base is fast-forwarded to it,
// it merges.
func TestPullsOpenedInBase(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t)
	r.git("update-ref", "refs/heads/a", r.tip("main"))
	n := pullOpen(t, f, "A", "a", "main", "")
	c := f.Client(t)
	for _, step := range []string{"opened", "main moved on"} {
		if step == "main moved on" {
			r.commit("main")
		}
		if pr, err := c.PullRequest(t.Context(), "o", "r", n); err != nil || pr.Merged || pr.State != "open" {
			t.Errorf("%s: read %+v, %v; want open and unmerged", step, pr, err)
		}
	}
	r.git("update-ref", "refs/heads/a", r.tip("main"))
	head := r.commit("a")
	r.git("update-ref", "refs/heads/main", head)
	if pr, err := c.PullRequest(t.Context(), "o", "r", n); err != nil || !pr.Merged || pr.MergeCommitSHA != head {
		t.Errorf("read after a new head was fast-forwarded onto main: %+v, %v; want merged at %s", pr, err, head)
	}
}
