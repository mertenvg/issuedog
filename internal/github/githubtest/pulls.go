package githubtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
)

// Pull requests. As on GitHub, a pull request is an issue too, with
// pull_request links (SetPullRequest), so the issue list, its labels, its
// assignees and its timeline are an issue's, and the issue list gives its
// pull_request.merged_at once it merged. The /pulls routes serve the ones
// the fake opened, for POST /pulls: each has a head and a base branch, and
// the SHA its head shows, read from a real bare repo (SetGitRemote) with
// git rev-parse when it's opened, updated, reopened and listed while open,
// in the issue list too. A head that moved since, as by a push, moves its
// updated_at, as GitHub's does (pullFollow). A closed one keeps the SHA it
// had, as GitHub stops following its branch. OpenPRAs opens one as someone
// else.
// As on GitHub, the fake refuses with a 422 a second open pull request with
// one head and base, and a reopen once the branch's tip no longer has the
// pull request's head in its history, as after a push by force while it
// was closed (SPEC §16). A merged one is never reopened.
// GET /pulls/{n} serves one, with merged, merged_by and merge_commit_sha,
// and PUT /pulls/{n}/merge squash-merges one as the App, writing the squash
// commit onto its base branch in the git remote (pullSquash), as MergePR
// does for a merge from the web page into a base other than main.

// pullRequest is what an issue the fake opened as a pull request has
// beyond an issue's: its head and base branches, the SHA its head showed
// when last read, whether it merged, who merged it and the squash commit
// its merge wrote, "" for a merge into main (MergePR), and its reviews,
// oldest first (reviews.go).
type pullRequest struct {
	head, base, headSHA string
	merged              bool
	mergedBy            string
	mergeCommit         string
	reviews             []*review
}

// pullState is the fake's git remote (SetGitRemote).
type pullState struct {
	remote string
}

// pullClosing matches a body's first line that closes an issue as a merge
// into the default branch does, with GitHub's keywords, such as
// "Closes #12".
var pullClosing = regexp.MustCompile(`(?i)^\s*(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s+#(\d+)\s*$`)

// handlePulls registers the pull request routes.
func (f *Fake) handlePulls() {
	f.handle("GET /pulls", f.listPulls)
	f.handle("POST /pulls", f.createPull)
	f.handle("PATCH /pulls/{n}", f.editPull)
	f.handle("GET /pulls/{n}", f.getPull)
	f.handle("PUT /pulls/{n}/merge", f.mergePull)
}

// SetGitRemote makes dir, a bare git repo, the one whose refs/heads/*
// give each pull request's head, and hold the commits a check run may be
// posted on (checkruns.go). Until it's set, a request that reads a head or
// a branch fails with a 500 naming it, as does one when the repo can't be
// read.
func (f *Fake) SetGitRemote(dir string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls.remote = dir
}

// remoteGit runs git in the git remote, with no system or global config and
// no inherited GIT_* variable, as internal/repo runs it, and returns its
// trimmed stdout. The error wraps git's *exec.ExitError, with its stderr.
func (f *Fake) remoteGit(ctx context.Context, args ...string) (string, error) {
	return f.remoteGitEnv(ctx, nil, args...)
}

// remoteGitEnv runs git in the git remote as remoteGit does, with env, as
// "KEY=value", set as well, such as a commit's author.
func (f *Fake) remoteGitEnv(ctx context.Context, env []string, args ...string) (string, error) {
	if f.pulls.remote == "" {
		return "", errors.New("githubtest: no git remote to read branches from: call SetGitRemote")
	}
	cmd := exec.CommandContext(ctx, "git", append([]string{"--git-dir=" + f.pulls.remote}, args...)...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("githubtest: git %s in %s: %w: %s", strings.Join(args, " "), f.pulls.remote, err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// remoteNo reports whether err is git's exit status 1, which rev-parse
// --verify --quiet gives for a ref or a commit that isn't there, and
// merge-base --is-ancestor for a commit that isn't an ancestor.
func remoteNo(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() == 1
}

// pullTip is branch's tip in the git remote, or "" if it has no such
// branch. f is locked.
func (f *Fake) pullTip(ctx context.Context, branch string) (string, error) {
	sha, err := f.remoteGit(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	switch {
	case remoteNo(err):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("githubtest: the tip of %s: %w", branch, err)
	}
	return sha, nil
}

// pullHead is the SHA an open pull request's head shows now: its branch's
// tip, or an error when that can't be read, its branch gone included. f is
// locked.
func (f *Fake) pullHead(ctx context.Context, is *issue) (string, error) {
	sha, err := f.pullTip(ctx, is.pr.head)
	if err != nil {
		return "", fmt.Errorf("githubtest: the head of pull request #%d: %w", is.GetNumber(), err)
	}
	if sha == "" {
		return "", fmt.Errorf("githubtest: the head of pull request #%d: the git remote has no branch %s", is.GetNumber(), is.pr.head)
	}
	return sha, nil
}

// pullFollow reads open pull request is's head again from the git remote,
// as GitHub follows its branch, and moves its updated_at when the head
// moved, as a push does on GitHub. A pull request the fake didn't open, or
// a closed one, is left as it is. A branch gone is an error, as for
// pullHead. f is locked.
func (f *Fake) pullFollow(ctx context.Context, is *issue) error {
	if is.pr == nil || is.GetState() != "open" {
		return nil
	}
	sha, err := f.pullHead(ctx, is)
	if err != nil {
		return fmt.Errorf("githubtest: follow pull request #%d: %w", is.GetNumber(), err)
	}
	if sha != is.pr.headSHA {
		is.pr.headSHA, is.UpdatedAt = sha, f.tick()
	}
	return nil
}

// pullOpenWith is the open pull request other than skip with head and
// base, or nil. f is locked.
func (f *Fake) pullOpenWith(head, base string, skip *issue) *issue {
	for _, is := range f.issues {
		if is != skip && is.pr != nil && is.GetState() == "open" && is.pr.head == head && is.pr.base == base {
			return is
		}
	}
	return nil
}

// pullReopen is the SHA closed pull request is's head shows once it's
// reopened, its branch's tip, with 0 and nil, or the status and message
// GitHub refuses the reopen with: a 422 for a merged one, one whose branch
// is gone or no longer has its head in its history, and one whose head and
// base another open pull request has. A tip that can't be read is a 500.
// f is locked.
func (f *Fake) pullReopen(ctx context.Context, is *issue) (string, int, any) {
	refuse := func(why string) (string, int, any) {
		return "", http.StatusUnprocessableEntity, message("Validation Failed: state cannot be changed. " + why)
	}
	if is.pr.merged {
		return refuse("The pull request was merged.")
	}
	if other := f.pullOpenWith(is.pr.head, is.pr.base, is); other != nil {
		return refuse(fmt.Sprintf("A pull request already exists for %s, #%d.", is.pr.head, other.GetNumber()))
	}
	tip, err := f.pullTip(ctx, is.pr.head)
	if err != nil {
		return "", http.StatusInternalServerError, message(err.Error())
	}
	if tip == "" {
		return refuse(fmt.Sprintf("The %s branch was deleted.", is.pr.head))
	}
	switch _, err := f.remoteGit(ctx, "merge-base", "--is-ancestor", is.pr.headSHA, tip); {
	case remoteNo(err):
		return refuse(fmt.Sprintf("The %s branch was force-pushed or recreated.", is.pr.head))
	case err != nil:
		return "", http.StatusInternalServerError, message(err.Error())
	}
	return tip, 0, nil
}

// pullServe is is as /pulls serves it. The list leaves out merged and
// merged_by, as GitHub's does, so a reader tells a merge by merged_at.
// merge_commit_sha is served once a merge wrote a squash commit, in the
// list too, as GitHub's has it. The fake has no test merge, which GitHub
// gives an open pull request as its merge_commit_sha.
func (f *Fake) pullServe(is *issue, listed bool) gogithub.PullRequest {
	owner, name, _ := strings.Cut(f.repo, "/")
	repo := &gogithub.Repository{Name: &name, FullName: &f.repo, Owner: user(owner)}
	branch := func(ref string, sha *string) *gogithub.PullRequestBranch {
		return &gogithub.PullRequestBranch{Label: new(owner + ":" + ref), Ref: &ref, SHA: sha, Repo: repo, User: user(owner)}
	}
	p := gogithub.PullRequest{ID: is.ID, Number: is.Number, NodeID: is.NodeID, State: is.State, Title: is.Title, Body: is.Body,
		User: is.User, Labels: is.Labels, Assignees: is.Assignees, CreatedAt: is.CreatedAt, UpdatedAt: is.UpdatedAt,
		ClosedAt: is.ClosedAt, MergedAt: is.PullRequestLinks.MergedAt, URL: is.PullRequestLinks.URL,
		IssueURL: new(fmt.Sprintf("%s/repos/%s/issues/%d", f.srv.URL, f.repo, is.GetNumber())),
		Head:     branch(is.pr.head, &is.pr.headSHA),
		Base:     branch(is.pr.base, nil),
	}
	if is.pr.mergeCommit != "" {
		p.MergeCommitSHA = &is.pr.mergeCommit
	}
	if !listed {
		p.Merged = new(is.pr.merged)
		if is.pr.mergedBy != "" {
			p.MergedBy = user(is.pr.mergedBy)
		}
	}
	return p
}

// listPulls serves the pull requests the fake opened, newest first, as
// GitHub's default order is, paged: open ones unless state= says closed or
// all, only those whose head is head= and whose base is base=, if given.
// head= is "<owner>:<branch>", as GitHub takes it, and a head= with no
// owner is refused with a 422 here, so a caller that leaves it out is
// caught. Each open one's head is read again from the git remote first
// (pullFollow).
func (f *Fake) listPulls(c *call) (int, any) {
	q := c.r.URL.Query()
	state := q.Get("state")
	switch state {
	case "":
		state = "open"
	case "open", "closed", "all":
	default:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: state %q is not open, closed or all", state))
	}
	var headOwner, head string
	if h := q.Get("head"); h != "" {
		var ok bool
		if headOwner, head, ok = strings.Cut(h, ":"); !ok {
			return http.StatusUnprocessableEntity, message(fmt.Sprintf("githubtest: head %q names no owner: give it as <owner>:<branch>", h))
		}
	}
	owner, _, _ := strings.Cut(f.repo, "/")
	out := []gogithub.PullRequest{}
	for _, is := range slices.Backward(f.issues) {
		if is.pr == nil || state != "all" && is.GetState() != state || q.Has("base") && is.pr.base != q.Get("base") ||
			head != "" && (!strings.EqualFold(headOwner, owner) || is.pr.head != head) {
			continue
		}
		if err := f.pullFollow(c.r.Context(), is); err != nil {
			return http.StatusInternalServerError, message(err.Error())
		}
		out = append(out, f.pullServe(is, true))
	}
	return page(c, out)
}

// createPull opens a pull request as the App, from a branch of this repo,
// given as "<branch>" or "<owner>:<branch>", to base, and answers 201 with
// it. It's a new issue, numbered as one, with its head read from the git
// remote. As on GitHub, it answers 422 with no title, a head or base
// missing, a head branch the git remote hasn't got, and a head and base an
// open pull request has already (pullCreate).
func (f *Fake) createPull(c *call) (int, any) {
	var req struct{ Title, Head, Base, Body string }
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	is, status, msg := f.pullCreate(c.r.Context(), f.slug+"[bot]", req.Title, req.Head, req.Base, req.Body)
	if msg != nil {
		return status, msg
	}
	return http.StatusCreated, f.pullServe(is, false)
}

// pullCreate opens a pull request as author, from head, a branch of this
// repo, given as "<branch>" or "<owner>:<branch>", to base, with title and
// body, and returns it, or the status and message GitHub refuses it with:
// a 422 with no title, a head or base missing, a head branch the git
// remote hasn't got, and a head and base an open pull request has already,
// the last two with GitHub's errors list: an invalid head's, and one whose
// message says one already exists. A tip that can't be read is a 500. It's a new issue, numbered as
// one, with its head read from the git remote. f is locked.
func (f *Fake) pullCreate(ctx context.Context, author, title, head, base, body string) (*issue, int, any) {
	owner, _, _ := strings.Cut(f.repo, "/")
	if o, branch, ok := strings.Cut(head, ":"); ok {
		if !strings.EqualFold(o, owner) {
			return nil, http.StatusUnprocessableEntity, message(fmt.Sprintf("githubtest: head %q isn't in this repo; forks aren't modelled", head))
		}
		head = branch
	}
	if title == "" || head == "" || base == "" {
		return nil, http.StatusUnprocessableEntity, message("Validation Failed: a pull request needs a title, a head and a base")
	}
	if other := f.pullOpenWith(head, base, nil); other != nil {
		return nil, http.StatusUnprocessableEntity, map[string]any{"message": "Validation Failed",
			"errors": []map[string]string{{"resource": "PullRequest", "code": "custom",
				"message": fmt.Sprintf("A pull request already exists for %s:%s.", owner, head)}}}
	}
	sha, err := f.pullTip(ctx, head)
	if err != nil {
		return nil, http.StatusInternalServerError, message(err.Error())
	}
	if sha == "" {
		return nil, http.StatusUnprocessableEntity, map[string]any{"message": fmt.Sprintf("Validation Failed: head %q is no branch of the repo", head),
			"errors": []map[string]string{{"resource": "PullRequest", "field": "head", "code": "invalid"}}}
	}
	is := f.fileIssue(author, title, body, nil)
	f.pullLinks(is)
	is.pr = &pullRequest{head: head, base: base, headSHA: sha}
	return is, 0, nil
}

// OpenPRAs opens a pull request as login, from head, a branch of the git
// remote, to base, with title and body, as someone other than the App may
// from the web page, and returns its number. One POST /pulls would refuse
// (pullCreate) fails the test, naming why.
func (f *Fake) OpenPRAs(login, head, base, title, body string) int {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is, status, msg := f.pullCreate(context.Background(), login, title, head, base, body)
	if msg != nil {
		f.fakeFail("githubtest: open a pull request from %s to %s as %s: %d %v", head, base, login, status, msg)
	}
	return is.GetNumber()
}

// editPull edits a pull request's title, its body, its state (open or
// closed), or any of them, as the App, as editIssue edits an issue's, and
// answers with it. A reopen is refused as pullReopen says, and an open
// pull request's head is read again from the git remote, both before
// anything changes, so a refused edit changes nothing.
func (f *Fake) editPull(c *call) (int, any) {
	is := c.issue
	if is.pr == nil {
		return http.StatusNotFound, message("Not Found")
	}
	var req struct{ Title, Body, State *string }
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.Title == nil && req.Body == nil && req.State == nil {
		return http.StatusUnprocessableEntity, message("githubtest: only the title, the body and the state can be edited")
	}
	state := is.GetState()
	if req.State != nil {
		if *req.State != "open" && *req.State != "closed" {
			return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: state %q is not open or closed", *req.State))
		}
		state = *req.State
	}
	sha := is.pr.headSHA
	switch {
	case state == "open" && is.GetState() == "closed":
		tip, status, msg := f.pullReopen(c.r.Context(), is)
		if msg != nil {
			return status, msg
		}
		sha = tip
	case state == "open":
		tip, err := f.pullHead(c.r.Context(), is)
		if err != nil {
			return http.StatusInternalServerError, message(err.Error())
		}
		sha = tip
	}
	if req.Title != nil {
		f.retitle(is, f.slug+"[bot]", *req.Title)
	}
	if req.Body != nil {
		is.editBy(f.slug+"[bot]", *req.Body, f.tick())
	}
	f.setState(is, state, "", f.slug+"[bot]")
	is.pr.headSHA = sha
	return http.StatusOK, f.pullServe(is, false)
}

// pullLinks gives is the pull_request links that make it a pull request in
// the issue list. f is locked.
func (f *Fake) pullLinks(is *issue) {
	is.PullRequestLinks = &gogithub.PullRequestLinks{URL: new(fmt.Sprintf("%s/repos/%s/pulls/%d", f.srv.URL, f.repo, is.GetNumber()))}
}

// fakePull returns pull request number for a helper a test called with it,
// or fails the test if the fake hasn't got it, or didn't open it as a pull
// request (POST /pulls). f is locked.
func (f *Fake) fakePull(number int) *issue {
	f.t.Helper()
	is := f.fakeNumberIssue(number)
	if is.pr == nil {
		f.fakeFail("githubtest: #%d is no pull request the fake opened", number)
	}
	return is
}

// getPull serves one pull request the fake opened, with merged, merged_by
// and merge_commit_sha, as GitHub's GET /pulls/{n} does, and a 404 for an
// issue that isn't one. An open one's head is read again from the git
// remote first (pullFollow), so a push changes the answer, and its ETag.
func (f *Fake) getPull(c *call) (int, any) {
	is := c.issue
	if is.pr == nil {
		return http.StatusNotFound, message("Not Found")
	}
	if err := f.pullFollow(c.r.Context(), is); err != nil {
		return http.StatusInternalServerError, message(err.Error())
	}
	return http.StatusOK, f.pullServe(is, false)
}

// mergePull merges a pull request as the App, as GitHub's PUT
// /pulls/{n}/merge does, and answers 200 with the squash commit's SHA. The
// repo allows only squash merging (SPEC §15), so a merge_method other than
// squash, an absent one included, as GitHub takes that for a merge commit,
// is refused with a 405 before anything else. The rest is pullSquash's,
// with sha, if given, the head the merge must be at, commit_title the
// squash commit's title, or GitHub's default, the pull request's title and
// number, and commit_message its message, an empty one included, or the
// pull request's body. Then it's merged as MergePR marks it, by the App.
func (f *Fake) mergePull(c *call) (int, any) {
	is := c.issue
	if is.pr == nil {
		return http.StatusNotFound, message("Not Found")
	}
	var req struct {
		SHA           string  `json:"sha"`
		MergeMethod   string  `json:"merge_method"`
		CommitTitle   *string `json:"commit_title"`
		CommitMessage *string `json:"commit_message"`
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.MergeMethod != "squash" {
		return http.StatusMethodNotAllowed, message(fmt.Sprintf("githubtest: merge_method %q isn't allowed on this repository, which allows only squash", req.MergeMethod))
	}
	title, body := fmt.Sprintf("%s (#%d)", is.GetTitle(), is.GetNumber()), is.GetBody()
	if req.CommitTitle != nil {
		title = *req.CommitTitle
	}
	if req.CommitMessage != nil {
		body = *req.CommitMessage
	}
	commit, status, msg := f.pullSquash(c.r.Context(), is, f.slug+"[bot]", req.SHA, title, body)
	if msg != nil {
		return status, msg
	}
	f.pullMerged(is, f.slug+"[bot]", commit)
	return http.StatusOK, gogithub.PullRequestMergeResult{SHA: &commit, Merged: new(true), Message: new("Pull Request successfully merged")}
}

// pullSquash writes open pull request is's squash commit onto its base
// branch in the git remote, as login, and returns its SHA, or the status
// and message GitHub refuses the merge with, having changed nothing: a 405
// for one closed or merged, or whose base branch is gone or whose head
// conflicts with it, and a 409 when sha, if given, isn't its head. Its head
// is read again first, as GitHub follows its branch (pullHead), and kept
// as its head. The squash commit has the merge of the base's tip and the
// head as its tree, built with git merge-tree, the base's tip as its only
// parent, title and body as its message, an empty body leaving the
// title alone, login as its author and GitHub as its committer. It's dated
// Start, so a replay's squash of the same tree onto the same tip has the
// clean run's SHA. The base moves to it only from the tip read, so nothing
// else moves it between. A git remote that can't be read, none set
// included, is a 500 naming why. f is locked.
func (f *Fake) pullSquash(ctx context.Context, is *issue, login, sha, title, body string) (string, int, any) {
	notMergeable := func(why string) (string, int, any) {
		return "", http.StatusMethodNotAllowed, map[string]string{"message": "Pull Request is not mergeable", "githubtest": why}
	}
	if is.GetState() != "open" {
		why := "is closed"
		if is.pr.merged {
			why = "merged already"
		}
		return notMergeable(fmt.Sprintf("#%d %s", is.GetNumber(), why))
	}
	head, err := f.pullHead(ctx, is)
	if err != nil {
		return "", http.StatusInternalServerError, message(err.Error())
	}
	if sha != "" && sha != head {
		return "", http.StatusConflict, message("Head branch was modified. Review and try the merge again.")
	}
	base, err := f.pullTip(ctx, is.pr.base)
	switch {
	case err != nil:
		return "", http.StatusInternalServerError, message(err.Error())
	case base == "":
		return notMergeable(fmt.Sprintf("the base branch %s is gone", is.pr.base))
	}
	out, err := f.remoteGit(ctx, "merge-tree", "--write-tree", "--no-messages", base, head)
	switch {
	case remoteNo(err):
		return notMergeable(fmt.Sprintf("%s conflicts with %s", is.pr.head, is.pr.base))
	case err != nil:
		return "", http.StatusInternalServerError, message(err.Error())
	}
	tree, _, _ := strings.Cut(out, "\n")
	args := []string{"commit-tree", tree, "-p", base, "-m", title}
	if body != "" {
		args = append(args, "-m", body)
	}
	date := Start.Format(time.RFC3339)
	commit, err := f.remoteGitEnv(ctx, []string{"GIT_AUTHOR_NAME=" + login, "GIT_AUTHOR_EMAIL=" + login + "@users.noreply.github.com",
		"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_NAME=GitHub", "GIT_COMMITTER_EMAIL=noreply@github.com", "GIT_COMMITTER_DATE=" + date}, args...)
	if err != nil {
		return "", http.StatusInternalServerError, message(err.Error())
	}
	if _, err := f.remoteGit(ctx, "update-ref", "refs/heads/"+is.pr.base, commit, base); err != nil {
		return "", http.StatusInternalServerError, message(err.Error())
	}
	is.pr.headSHA = head
	return commit, 0, nil
}

// pullMerged marks pull request is merged by login, with commit as its
// merge commit, "" for none: merged and closed, its merged_at set, with a
// merged event and a closed event, each naming login, as GitHub's do, and
// returns the closed event's ID. f is locked.
func (f *Fake) pullMerged(is *issue, login, commit string) int64 {
	now := f.tick()
	is.pr.merged, is.pr.mergedBy, is.pr.mergeCommit, is.PullRequestLinks.MergedAt = true, login, commit, now
	f.lastEvent++
	id := firstEvent + f.lastEvent
	is.labelEvents = append(is.labelEvents, &gogithub.Timeline{ID: &id, Event: new("merged"), Actor: user(login),
		CreatedAt: &gogithub.Timestamp{Time: now.Add(f.eventLag)}})
	return f.setState(is, "closed", "", login)
}

// MergePR merges open pull request number as login, as a merge from the
// web page does: with its head read first, it's merged and closed, its
// merged_at set, with a merged event and a closed event, each naming login,
// as GitHub's do (pullMerged). A merge into main, the fake's default
// branch, writes no commit, and then closes the issue its body's first
// line names, as "Closes #N" does, as CloseByPullRequest closes it. A
// merge into any other base is a squash, as the repo allows only those
// (SPEC §15): its commit, titled with the pull request's title and number
// and with its body as the message, is written onto the base in the git
// remote, as pullSquash writes it, and is its merge_commit_sha. It returns
// the closed event's ID, or 0 if the pull request wasn't open and nothing
// changed. A pull request the fake didn't open, whose head can't be read,
// or whose squash GitHub would refuse, fails the test.
func (f *Fake) MergePR(number int, login string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakePull(number)
	if is.GetState() != "open" {
		return 0
	}
	if is.pr.base != "main" {
		commit, status, msg := f.pullSquash(context.Background(), is, login, "", fmt.Sprintf("%s (#%d)", is.GetTitle(), number), is.GetBody())
		if msg != nil {
			f.fakeFail("githubtest: merge #%d into %s: %d %v", number, is.pr.base, status, msg)
		}
		return f.pullMerged(is, login, commit)
	}
	sha, err := f.pullHead(context.Background(), is)
	if err != nil {
		f.fakeFail("githubtest: merge #%d: %v", number, err)
	}
	is.pr.headSHA = sha
	closed := f.pullMerged(is, login, "")
	line, _, _ := strings.Cut(is.GetBody(), "\n")
	if m := pullClosing.FindStringSubmatch(line); m != nil {
		n, err := strconv.Atoi(m[1])
		if target := f.issueNumbered(n); err == nil && target != nil && !target.IsPullRequest() {
			f.closeByPullRequest(target, login, ClosingPR{Merged: true})
		}
	}
	return closed
}

// ClosePR closes open pull request number as login, unmerged, with a
// closed event naming login, and returns its ID, or 0 if it was closed
// already. Its head keeps the SHA it had. A pull request the fake didn't
// open fails the test.
func (f *Fake) ClosePR(number int, login string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setState(f.fakePull(number), "closed", "", login)
}

// ReopenPR reopens closed pull request number as login, with a reopened
// event naming login, and its head read again, and returns the event's ID,
// or 0 if it was open already. One GitHub won't reopen (pullReopen), and
// one the fake didn't open, fails the test, naming why.
func (f *Fake) ReopenPR(number int, login string) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakePull(number)
	if is.GetState() == "open" {
		return 0
	}
	sha, status, msg := f.pullReopen(context.Background(), is)
	if msg != nil {
		f.fakeFail("githubtest: reopen #%d: %d %v", number, status, msg)
	}
	is.pr.headSHA = sha
	return f.setState(is, "open", "", login)
}
