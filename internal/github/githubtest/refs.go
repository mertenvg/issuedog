package githubtest

import (
	"context"
	"fmt"
	"net/http"
)

// Git refs. DELETE /git/refs/heads/{branch} deletes a branch of the git
// remote (SetGitRemote), as the App, and answers 204, or GitHub's 422
// "Reference does not exist" for a branch the remote hasn't got, one
// deleted already included. It's a write, logged and failed as any is. No
// other ref is served. As GitHub does (SPEC §16), a delete closes each
// open pull request whose head or base is the branch, unmerged, with a
// closed event naming the deleter (pullCloseFor). DeleteBranchAs deletes
// one as someone else, as an admin may from the web page, and
// RestoreBranch puts one back, as GitHub's "Restore branch" does, which
// reopens none of them.

// handleRefs registers the git ref routes.
func (f *Fake) handleRefs() {
	f.handle("DELETE /git/refs/heads/{branch...}", f.deleteRef)
}

// deleteRef deletes the branch the path names in the git remote as the
// App (refDelete). A remote that can't be read, none set included, is a
// 500 naming why.
func (f *Fake) deleteRef(c *call) (int, any) {
	found, err := f.refDelete(c.r.Context(), c.r.PathValue("branch"), f.slug+"[bot]")
	switch {
	case err != nil:
		return http.StatusInternalServerError, message(err.Error())
	case !found:
		return http.StatusUnprocessableEntity, message("Reference does not exist")
	}
	return http.StatusNoContent, nil
}

// refDelete deletes branch in the git remote as login, and reports whether
// it was there: one the remote hasn't got is false, with nothing changed.
// The open pull requests into it and from it are closed first, while
// their heads can still be read (pullCloseFor), and the branch is then
// deleted only from the tip read, so nothing else moves it between. A
// remote that can't be read is an error. f is locked.
func (f *Fake) refDelete(ctx context.Context, branch, login string) (bool, error) {
	tip, err := f.pullTip(ctx, branch)
	switch {
	case err != nil:
		return false, fmt.Errorf("githubtest: delete %s: %w", branch, err)
	case tip == "":
		return false, nil
	}
	if err := f.pullCloseFor(ctx, branch, login); err != nil {
		return false, fmt.Errorf("githubtest: delete %s: %w", branch, err)
	}
	if _, err := f.remoteGit(ctx, "update-ref", "-d", "refs/heads/"+branch, tip); err != nil {
		return false, fmt.Errorf("githubtest: delete %s: %w", branch, err)
	}
	return true, nil
}

// pullCloseFor closes each open pull request whose head or base is branch,
// a branch about to be deleted, as login, unmerged, with a closed event
// naming login, as GitHub closes them once the branch is deleted. Each is
// read again first (pullFollow), so one a push merged before the delete
// reads merged, as on GitHub, and isn't closed again. Its head keeps the
// SHA it had. A head that can't be read is an error, and closes none.
// f is locked.
func (f *Fake) pullCloseFor(ctx context.Context, branch, login string) error {
	var open []*issue
	for _, is := range f.issues {
		if is.pr != nil && is.GetState() == "open" && (is.pr.head == branch || is.pr.base == branch) {
			open = append(open, is)
		}
	}
	for _, is := range open {
		if err := f.pullFollow(ctx, is); err != nil {
			return fmt.Errorf("githubtest: close the pull requests of %s: %w", branch, err)
		}
	}
	for _, is := range open {
		f.setState(is, "closed", "", login)
	}
	return nil
}

// DeleteBranchAs deletes branch in the git remote as login, as an admin
// may from the web page, past the ruleset that keeps others from it: it
// closes the open pull requests into it and from it as a DELETE through
// the API does, each with a closed event naming login (refDelete). A
// branch the remote hasn't got, and a remote that can't be read, are an
// error, with nothing changed.
func (f *Fake) DeleteBranchAs(login, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	found, err := f.refDelete(context.Background(), branch, login)
	if err != nil {
		return fmt.Errorf("githubtest: delete %s as %s: %w", branch, login, err)
	}
	if !found {
		return fmt.Errorf("githubtest: delete %s as %s: the git remote has no such branch", branch, login)
	}
	return nil
}

// RestoreBranch creates branch in the git remote at sha, a commit it
// holds, as GitHub's "Restore branch" on a closed pull request puts a
// deleted branch back. It reopens none of the pull requests the delete
// closed; each can be reopened then, as ReopenPR and PATCH /pulls/{n} do.
// A branch the remote has already, a sha it hasn't got, and a remote that
// can't be read, are an error, with nothing changed.
func (f *Fake) RestoreBranch(branch, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, err := f.remoteGit(context.Background(), "update-ref", "refs/heads/"+branch, sha, ""); err != nil {
		return fmt.Errorf("githubtest: restore %s at %s: %w", branch, sha, err)
	}
	return nil
}
