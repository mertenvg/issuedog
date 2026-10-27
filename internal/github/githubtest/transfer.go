package githubtest

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Transferred issues (#199). GitHub's transfer moves an issue, with its
// comments, to another repo. The issue then drops out of this repo's
// issue and comment listings, and a request to any of its old paths, a
// write or one about one of its comments included, gets a 301 whose
// Location is the same path in the other repo, named by the repo's ID,
// /repositories/{id}/..., as GitHub names a moved resource.
//
// Go's http.Client, which go-github uses, follows a 301 to the same host
// for every method, and sends a write (POST, PATCH, PUT, DELETE) on as a
// GET with no body (net/http's redirectBehavior). go-github only returns
// a RedirectionError from a client that doesn't follow redirects, which
// none of internal/github's calls use. So a write to a transferred issue
// writes nothing, and its answer is the GET of the new path: the moved
// issue itself for the issue's own path, or a 404 for a path that has no
// GET, such as /issues/{n}/assignees.
//
// The fake serves GETs at the new home for the moved issue and its
// comments, by the paths this repo's handlers serve, with the issue's
// repository_url naming the other repo. It serves nothing else of the
// other repo: any other path there, or any other method, gets a 404. The
// moved issue keeps its number there, which GitHub doesn't promise.
//
// GraphQL treats a transferred issue's number as not this repo's (#239),
// with GitHub's answer for one it can't find: the comments and isPinned
// queries, by its number, answer a null issue with a NOT_FOUND error at
// its path, repository.issue.
// Node IDs are global, so a lookup by node answers from the issue's new
// repo (#413). What GitHub answers for one, with an installation that
// covers that repo and with one that doesn't, is to be confirmed live
// (SPEC §16). Here the history query, for its node or one of its
// comments', and the closer query, for one of its closed events, answer
// as for any issue, and the pinIssue mutation pins it there. So a caller
// that pins by a node ID it read must first check the issue is still
// this repo's, as PinIssue does.

// Transfer moves issue number to repo ("owner/name"), as GitHub's
// transfer does (see the top of this file). A transfer to this repo, in
// any case, brings the issue back, with repository_url naming repo as
// given, so a test can check that a repo named in another case is no
// transfer. Repos are given IDs from 2 on, in the order they're first
// transferred to (this repo is 1).
func (f *Fake) Transfer(number int, repo string) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakeNumberIssue(number)
	is.RepositoryURL, is.movedTo = new(f.srv.URL+"/repos/"+repo), ""
	if strings.EqualFold(repo, f.repo) {
		return
	}
	is.movedTo = strings.ToLower(repo)
	if f.repoIDs == nil {
		f.repoIDs = map[string]int64{}
	}
	if _, ok := f.repoIDs[is.movedTo]; !ok {
		f.repoIDs[is.movedTo] = int64(len(f.repoIDs) + 2)
	}
}

// transferHome is the context key of a request the new home rewrote
// (serveHome), whose value is a homeRequest.
type transferHome struct{}

// homeRequest is a request at a new home (serveHome): the repo the issue
// it names must have moved to, in lower case, and the path it came with,
// which a page's next link names (page).
type homeRequest struct{ repo, path string }

// homePath is the path r came with: its new home's, if serveHome rewrote
// it, or its own.
func homePath(r *http.Request) string {
	if h, ok := r.Context().Value(transferHome{}).(homeRequest); ok {
		return h.path
	}
	return r.URL.Path
}

// handleTransfer registers the new home of transferred issues.
func (f *Fake) handleTransfer() {
	f.mux.HandleFunc("/repositories/{id}/{rest...}", f.serveHome)
}

// serveHome serves a GET at a transferred issue's new home,
// /repositories/{id}/{rest}, as this repo's handlers serve
// /repos/{owner}/{repo}/{rest}, for the issue moved to repo id and its
// comments (moved). Any other method, and a path this repo serves no GET
// for, gets a 404, as GitHub gives for a path it doesn't have.
func (f *Fake) serveHome(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f.mu.Lock()
	repo := ""
	for name, rid := range f.repoIDs {
		if err == nil && rid == id {
			repo = name
		}
	}
	f.mu.Unlock()
	if repo == "" || r.Method != http.MethodGet {
		send(w, http.StatusNotFound, message("Not Found"))
		return
	}
	in := r.Clone(context.WithValue(r.Context(), transferHome{}, homeRequest{repo, r.URL.Path}))
	in.URL.Path, in.URL.RawPath = "/repos/"+f.repo+"/"+r.PathValue("rest"), ""
	if _, pattern := f.mux.Handler(in); pattern == "" {
		send(w, http.StatusNotFound, message("Not Found"))
		return
	}
	f.mux.ServeHTTP(w, in)
}

// moved answers a call that names a transferred issue or one of its
// comments, and reports whether it did. At the issue's old path, it
// answers a 301 to the new home. At a new home (serveHome), it answers a
// 404 for an issue or comment not moved to that repo, and for a path
// that names neither, such as a listing of the whole repo. The caller
// holds f.mu.
func (f *Fake) moved(c *call) bool {
	is := c.issue
	if c.comment != nil {
		is = f.issueNumbered(c.comment.issue)
	}
	home, atHome := c.r.Context().Value(transferHome{}).(homeRequest)
	switch {
	case atHome && (is == nil || is.movedTo != home.repo):
		send(c.w, http.StatusNotFound, message("Not Found"))
		return true
	case atHome || is == nil || is.movedTo == "":
		return false
	}
	loc := fmt.Sprintf("%s/repositories/%d/%s", f.srv.URL, f.repoIDs[is.movedTo], strings.TrimPrefix(c.r.URL.Path, "/repos/"+f.repo+"/"))
	if c.r.URL.RawQuery != "" {
		loc += "?" + c.r.URL.RawQuery
	}
	c.w.Header().Set("Location", loc)
	send(c.w, http.StatusMovedPermanently, map[string]string{"message": "Moved Permanently", "url": loc,
		"documentation_url": "https://docs.github.com/rest"})
	return true
}

// listed reports whether is shows in this repo's listings: it hasn't
// been transferred away.
func (is *issue) listed() bool { return is.movedTo == "" }
