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

	gogithub "github.com/google/go-github/v92/github"
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
	cmd := exec.Command("git", append([]string{"--git-dir=" + r.dir}, args...)...)
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
// out, if any, and returns the status and the message of any other.
func pullDo(t *testing.T, f *Fake, method, path string, body, out any) (int, string) {
	t.Helper()
	c := f.Client(t)
	req, err := c.NewRequest(context.Background(), method, path, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req, out)
	if er := (*gogithub.ErrorResponse)(nil); errors.As(err, &er) {
		return resp.StatusCode, er.Message
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
// state and merge, by number.
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
	want[0].HeadSHA, want[0].State, want[0].Merged = r.tip("a"), "closed", true
	if got := f.State().PRs; !reflect.DeepEqual(got, want) {
		t.Errorf("State().PRs merged = %+v; want %+v", got, want)
	}
	pullDo(t, f, "PATCH", fmt.Sprintf("repos/o/r/pulls/%d", n), map[string]string{"body": "B2"}, &pr)
	if !pr.GetMerged() || pr.GetMergedBy().GetLogin() != "owner" || pr.GetBody() != "B2" {
		t.Errorf("a merged one's update: merged %v by %q, body %q; want merged by owner, B2", pr.GetMerged(), pr.GetMergedBy().GetLogin(), pr.GetBody())
	}
}

// The fake refuses, with a 422 and changing nothing, a second open pull
// request with one head and base, one whose head is no branch, and a
// reopen once the branch was pushed past its head by force, of a merged
// one, and of one whose head and base another open one has. A reopen
// after a fast-forward isn't refused. A remote that can't be read, or none,
// is a 500 that names it.
func TestPullsRefusals(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a", "b")
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
	if status, _ := open("x:a", "main"); status != http.StatusUnprocessableEntity {
		t.Errorf("a head in another repo: %d; want 422", status)
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
// open as a pull request, and ReopenPR for a reopen GitHub refuses.
func TestPullsHelpersFail(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f, r := pullFake(tb, "a")
	marked := f.AddIssue("owner", "Marked", "")
	f.SetPullRequest(marked)
	n := pullOpen(t, f, "A", "a", "main", "")
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
	} {
		fatal, panicked := fakeNumberFatal(tb, tc.do)
		if panicked != nil || !slices.Equal(fatal, []string{tc.want}) {
			t.Errorf("%s: Fatalf %q, panicked with %v; want Fatalf %q", tc.name, fatal, panicked, tc.want)
		}
	}
}
