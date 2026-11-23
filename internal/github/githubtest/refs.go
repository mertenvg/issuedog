package githubtest

import (
	"net/http"
)

// Git refs. DELETE /git/refs/heads/{branch} deletes a branch of the git
// remote (SetGitRemote), as the App, and answers 204, or GitHub's 422
// "Reference does not exist" for a branch the remote hasn't got, one
// deleted already included. It's a write, logged and failed as any is. No
// other ref is served. GitHub closes an open pull request whose head or
// base branch is deleted; the fake doesn't, and leaves an open one's head
// to fail when it's next read (pullHead), so a test deletes only the
// branch of a merged or closed one.

// handleRefs registers the git ref routes.
func (f *Fake) handleRefs() {
	f.handle("DELETE /git/refs/heads/{branch...}", f.deleteRef)
}

// deleteRef deletes the branch the path names in the git remote, only from
// the tip read, so nothing else moves it between. A remote that can't be
// read, none set included, is a 500 naming why.
func (f *Fake) deleteRef(c *call) (int, any) {
	branch := c.r.PathValue("branch")
	tip, err := f.pullTip(c.r.Context(), branch)
	switch {
	case err != nil:
		return http.StatusInternalServerError, message(err.Error())
	case tip == "":
		return http.StatusUnprocessableEntity, message("Reference does not exist")
	}
	if _, err := f.remoteGit(c.r.Context(), "update-ref", "-d", "refs/heads/"+branch, tip); err != nil {
		return http.StatusInternalServerError, message(err.Error())
	}
	return http.StatusNoContent, nil
}
