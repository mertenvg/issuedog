package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// pullsRemote makes a bare git repo in a temporary directory with main and
// the branches named, each a commit that changes nothing, at a fixed date,
// so each call makes the same SHAs. It returns the repo's directory, for
// SetGitRemote, and a function that runs git there.
func pullsRemote(t testing.TB, branches ...string) (string, func(args ...string) string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "remote.git")
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"--git-dir=" + dir}, args...)...)
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
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--bare", "--quiet", "--initial-branch=main", dir)
	for _, b := range append([]string{"main"}, branches...) {
		git("update-ref", "refs/heads/"+b, git("commit-tree", git("mktree"), "-m", b))
	}
	return dir, git
}

// pullsFake is a fake of o/r whose git remote has the branches named.
func pullsFake(t testing.TB, branches ...string) (*githubtest.Fake, func(args ...string) string) {
	t.Helper()
	f := githubtest.New(t, "o/r", "issuedog")
	dir, git := pullsRemote(t, branches...)
	f.SetGitRemote(dir)
	return f, git
}

// PullRequestsByHead lists every pull request with the branch as its head,
// open, closed and merged, over pages of 100, newest first, each with its
// head's SHA, its base, state, title, body, author, labels, merge and
// times, and none with another head.
func TestPullRequestsByHead(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task", "other")
	c, ctx := f.Client(t), context.Background()
	var first, merged github.PullRequest
	for i := range 101 {
		pr, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: fmt.Sprintf("T%d", i), Head: "issuedog/7-task", Base: "main", Body: "B"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = pr
		}
		if i < 100 {
			f.ClosePR(pr.Number, "owner")
		}
		if i == 99 {
			f.ReopenPR(pr.Number, "owner")
			f.MergePR(pr.Number, "owner")
			merged = pr
		}
	}
	if _, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "Other", Head: "other", Base: "main"}); err != nil {
		t.Fatal(err)
	}
	f.AddLabel(first.Number, "owner", "priority:p1")
	from := len(f.Requests())
	prs, err := c.PullRequestsByHead(ctx, "o", "r", "issuedog/7-task")
	if err != nil {
		t.Fatal(err)
	}
	var queries []string
	for _, r := range f.Requests()[from:] {
		if r.Path == "/pulls" {
			queries = append(queries, r.Query)
		}
	}
	if len(queries) != 2 || !strings.Contains(queries[0], "head=o%3Aissuedog%2F7-task") || !strings.Contains(queries[0], "state=all") ||
		!strings.Contains(queries[0], "per_page=100") || !strings.Contains(queries[1], "page=2") {
		t.Errorf("queries %q; want two pages of 100, state=all, head=o:issuedog/7-task", queries)
	}
	if len(prs) != 101 {
		t.Fatalf("listed %d; want 101", len(prs))
	}
	tip := git("rev-parse", "refs/heads/issuedog/7-task")
	open := prs[0]
	if open.State != "open" || open.Title != "T100" || open.Number != first.Number+100 || open.Merged || !open.ClosedAt.IsZero() {
		t.Errorf("newest %+v; want T100, open, unmerged", open)
	}
	last := prs[100]
	want := github.PullRequest{Number: first.Number, Head: "issuedog/7-task", HeadSHA: tip, Base: "main", State: "closed",
		Title: "T0", Body: "B", Author: "issuedog[bot]", Labels: []string{"priority:p1"}, ClosedAt: last.ClosedAt, UpdatedAt: last.UpdatedAt}
	if !reflect.DeepEqual(last, want) || last.ClosedAt.IsZero() || last.UpdatedAt.Before(last.ClosedAt) {
		t.Errorf("oldest %+v; want %+v, with its close's time", last, want)
	}
	if m := prs[1]; m.Number != merged.Number || !m.Merged || m.State != "closed" {
		t.Errorf("second newest %+v; want #%d, merged, as merged_at says with no merged field", m, merged.Number)
	}
}

// CreatePullRequest opens a pull request as the App, from the head to the
// base, with the title and body, and returns it with its head's SHA. A
// second open one with that head and base is GitHub's 422, which is no
// refused reopen, but ErrPullRequestExists, and so is one when someone
// else's is open. Any other 422, such as a head that's no branch, isn't.
func TestCreatePullRequest(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task", "theirs")
	c, ctx := f.Client(t), context.Background()
	pr, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "feat: a thing", Head: "issuedog/7-task", Base: "main", Body: "Closes #1"})
	if err != nil {
		t.Fatal(err)
	}
	want := github.PullRequest{Number: pr.Number, Head: "issuedog/7-task", HeadSHA: git("rev-parse", "refs/heads/issuedog/7-task"), Base: "main",
		State: "open", Title: "feat: a thing", Body: "Closes #1", Author: "issuedog[bot]", UpdatedAt: pr.UpdatedAt}
	if !reflect.DeepEqual(pr, want) || pr.UpdatedAt.IsZero() {
		t.Errorf("opened %+v; want %+v", pr, want)
	}
	if w := f.Writes(); len(w) != 1 || w[0].Method != "POST" || w[0].Path != "/pulls" ||
		w[0].Body != `{"title":"feat: a thing","head":"issuedog/7-task","base":"main","body":"Closes #1"}`+"\n" {
		t.Errorf("writes %+v; want one POST /pulls with the title, head, base and body", w)
	}
	_, err = c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "again", Head: "issuedog/7-task", Base: "main"})
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity || errors.Is(err, github.ErrReopenRefused) ||
		!errors.Is(err, github.ErrPullRequestExists) {
		t.Errorf("a second open one: %v; want GitHub's 422, ErrPullRequestExists, not a refused reopen", err)
	}
	f.OpenPRAs("mallory", "theirs", "main", "Theirs", "")
	_, err = c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "T", Head: "theirs", Base: "main"})
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity || !errors.Is(err, github.ErrPullRequestExists) {
		t.Errorf("an open one with mallory's open: %v; want GitHub's 422, ErrPullRequestExists", err)
	}
	_, err = c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "T", Head: "nope", Base: "main"})
	if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity || errors.Is(err, github.ErrPullRequestExists) {
		t.Errorf("an open from no branch: %v; want GitHub's 422, not ErrPullRequestExists", err)
	}
}

// UpdatePullRequest sends only the fields given, and returns the pull
// request. A 422 to a reopen wraps ErrReopenRefused, beside GitHub's
// error. Any other failure doesn't: a 422 to an edit that isn't a reopen,
// and a reopen's 500.
func TestUpdatePullRequest(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task")
	c, ctx := f.Client(t), context.Background()
	pr, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "T", Head: "issuedog/7-task", Base: "main", Body: "B"})
	if err != nil {
		t.Fatal(err)
	}
	n := pr.Number
	if pr, err = c.UpdatePullRequest(ctx, "o", "r", n, new("T2"), nil, nil); err != nil || pr.Title != "T2" || pr.Body != "B" || pr.State != "open" {
		t.Errorf("title only: %+v, %v; want T2, the body kept, open", pr, err)
	}
	if pr, err = c.UpdatePullRequest(ctx, "o", "r", n, nil, new("B2"), new("closed")); err != nil || pr.Body != "B2" || pr.State != "closed" || pr.ClosedAt.IsZero() {
		t.Errorf("body and close: %+v, %v; want B2, closed", pr, err)
	}
	git("update-ref", "refs/heads/issuedog/7-task", git("commit-tree", git("mktree"), "-p", pr.HeadSHA, "-m", "next"))
	if pr, err = c.UpdatePullRequest(ctx, "o", "r", n, nil, nil, new("open")); err != nil || pr.State != "open" ||
		pr.HeadSHA != git("rev-parse", "refs/heads/issuedog/7-task") {
		t.Errorf("reopen after a fast-forward: %+v, %v; want open, at the new tip", pr, err)
	}
	var bodies []string
	for _, w := range f.Writes() {
		bodies = append(bodies, w.Method+" "+w.Path+" "+strings.TrimSpace(w.Body))
	}
	want := []string{"POST /pulls " + `{"title":"T","head":"issuedog/7-task","base":"main","body":"B"}`,
		fmt.Sprintf(`PATCH /pulls/%d {"title":"T2"}`, n), fmt.Sprintf(`PATCH /pulls/%d {"body":"B2","state":"closed"}`, n),
		fmt.Sprintf(`PATCH /pulls/%d {"state":"open"}`, n)}
	if !reflect.DeepEqual(bodies, want) {
		t.Errorf("writes %q; want %q", bodies, want)
	}

	// A push by force while it's closed: GitHub won't reopen it.
	f.ClosePR(n, "owner")
	git("update-ref", "refs/heads/issuedog/7-task", git("commit-tree", git("mktree"), "-m", "forced"))
	_, err = c.UpdatePullRequest(ctx, "o", "r", n, new("T3"), nil, new("open"))
	var er *gogithub.ErrorResponse
	if !errors.Is(err, github.ErrReopenRefused) || !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("reopen after a push by force: %v; want ErrReopenRefused and GitHub's 422", err)
	}
	if _, err := c.UpdatePullRequest(ctx, "o", "r", n, nil, nil, new("merged")); err == nil || errors.Is(err, github.ErrReopenRefused) {
		t.Errorf("state=merged: %v; want an error that isn't ErrReopenRefused", err)
	}
	f.FailWrite(len(f.Writes()) + 1)
	if _, err := c.UpdatePullRequest(ctx, "o", "r", n, nil, nil, new("open")); err == nil || errors.Is(err, github.ErrReopenRefused) {
		t.Errorf("a reopen's 500: %v; want an error that isn't ErrReopenRefused", err)
	}
}

// A pull request is in IssuesSince, as a pull request, with merged_at once
// it merged, and its timeline gives its closed and reopened events, each
// naming who made it. A merge into main closes the issue its body's
// "Closes #N" names, with the merged pull request as its closer.
func TestPullRequestAsIssue(t *testing.T) {
	t.Parallel()
	f, _ := pullsFake(t, "issuedog/1-task")
	task := f.AddIssue("owner", "Task", "")
	c, ctx := f.Client(t), context.Background()
	pr, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: "T", Head: "issuedog/1-task", Base: "main", Body: fmt.Sprintf("Closes #%d", task)})
	if err != nil {
		t.Fatal(err)
	}
	closed := f.ClosePR(pr.Number, "mallory")
	reopened := f.ReopenPR(pr.Number, "owner")
	merge := f.MergePR(pr.Number, "owner")
	issues, err := c.IssuesSince(ctx, "o", "r", githubtest.Start)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 2 || issues[0].GetNumber() != pr.Number || !issues[0].IsPullRequest() || issues[0].GetPullRequestLinks().MergedAt == nil ||
		issues[1].GetNumber() != task || issues[1].IsPullRequest() {
		t.Errorf("IssuesSince %+v; want the pull request with merged_at, then the task its merge closed after it", issues)
	}
	timeline, err := c.TimelineSince(ctx, "o", "r", pr.Number, githubtest.Start)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range timeline {
		got = append(got, fmt.Sprintf("%d %s %s", e.GetID(), e.GetEvent(), e.GetActor().GetLogin()))
	}
	want := []string{fmt.Sprintf("%d closed mallory", closed), fmt.Sprintf("%d reopened owner", reopened),
		fmt.Sprintf("%d merged owner", merge-1), fmt.Sprintf("%d closed owner", merge)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("timeline %q; want %q", got, want)
	}
	events, err := c.TimelineSince(ctx, "o", "r", task, githubtest.Start)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].GetEvent() != "closed" {
		t.Fatalf("the task's timeline %+v; want its close", events)
	}
	closer, err := c.ClosingCloser(ctx, "o", "r", fmt.Sprintf("CE_%d", events[0].GetID()))
	if err != nil || !closer.Merge {
		t.Errorf("the task's closer %+v, %v; want a merge into main", closer, err)
	}
}

// pullsReplayPass finds the task's pull request by its head, as the daemon
// does (SPEC §7.5), and opens one, or reopens and updates a closed one, or
// updates an open one whose title or body differ.
func pullsReplayPass(t *testing.T) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c, ctx := f.Client(t), context.Background()
		prs, err := c.PullRequestsByHead(ctx, "o", "r", "issuedog/7-task")
		if err != nil {
			return fmt.Errorf("find: %w", err)
		}
		title, body := "feat: the task", "Closes #1"
		for _, pr := range prs {
			switch {
			case pr.Merged:
				continue
			case pr.State == "closed":
				_, err = c.UpdatePullRequest(ctx, "o", "r", pr.Number, &title, &body, new("open"))
			case pr.Title != title || pr.Body != body:
				_, err = c.UpdatePullRequest(ctx, "o", "r", pr.Number, &title, &body, nil)
			}
			if err != nil {
				return fmt.Errorf("update: %w", err)
			}
			return nil
		}
		if _, err := c.CreatePullRequest(ctx, "o", "r", github.NewPullRequest{Title: title, Head: "issuedog/7-task", Base: "main", Body: body}); err != nil {
			return fmt.Errorf("open: %w", err)
		}
		return nil
	}
}

// Opening a pull request, and reopening and updating a closed one, are
// safe to replay after a crash at any write: no second pull request, and
// the same head, base, title, body and state.
func TestPullRequestReplay(t *testing.T) {
	t.Parallel()
	t.Run("open", func(t *testing.T) {
		t.Parallel()
		githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
			f, _ := pullsFake(tb, "issuedog/7-task")
			f.AddIssue("owner", "Task", "")
			return f
		}, pullsReplayPass(t))
	})
	t.Run("reopen", func(t *testing.T) {
		t.Parallel()
		githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
			f, git := pullsFake(tb, "issuedog/7-task")
			f.AddIssue("owner", "Task", "")
			pr, err := f.Client(tb).CreatePullRequest(context.Background(), "o", "r", github.NewPullRequest{Title: "old", Head: "issuedog/7-task", Base: "main"})
			if err != nil {
				tb.Fatal(err)
			}
			f.ClosePR(pr.Number, "owner")
			git("update-ref", "refs/heads/issuedog/7-task", git("commit-tree", git("mktree"), "-p", pr.HeadSHA, "-m", "next"))
			f.ForgetWrites()
			return f
		}, pullsReplayPass(t))
	})
}
