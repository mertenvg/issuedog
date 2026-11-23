package githubtest

import (
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/logr/v2"
)

// DELETE /git/refs/heads/{branch} deletes the branch in the git remote,
// with a 204, counted as a write, and leaves the other branches. A branch
// that's gone, deleted already or never there, is GitHub's 422 "Reference
// does not exist", and is a write too. State's Branches are the remote's
// branches under issuedog/, by name, with their tips, before and after,
// and never main or another branch outside issuedog/.
func TestRefsDeleteBranch(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "issuedog/7-task", "issuedog/5-story", "other")
	task, story := r.tip("issuedog/7-task"), r.tip("issuedog/5-story")
	if got, want := f.State().Branches, []BranchState{{"issuedog/5-story", story}, {"issuedog/7-task", task}}; !reflect.DeepEqual(got, want) {
		t.Errorf("State().Branches %+v; want %+v", got, want)
	}
	if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete: %d %q; want 204", status, msg)
	}
	if got := r.tip("issuedog/7-task"); got != "" {
		t.Errorf("the deleted branch's tip %s; want it gone", got)
	}
	if got, want := f.State().Branches, []BranchState{{"issuedog/5-story", story}}; !reflect.DeepEqual(got, want) {
		t.Errorf("State().Branches after the delete %+v; want %+v", got, want)
	}
	for _, branch := range []string{"issuedog/7-task", "issuedog/9-never"} {
		if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/"+branch, nil, nil); status != http.StatusUnprocessableEntity || msg != "Reference does not exist" {
			t.Errorf("delete %s: %d %q; want 422 Reference does not exist", branch, status, msg)
		}
	}
	w := f.Writes()
	if len(w) != 3 || w[0].Method != "DELETE" || w[0].Path != "/git/refs/heads/issuedog/7-task" || w[2].Path != "/git/refs/heads/issuedog/9-never" {
		t.Errorf("writes %+v; want the three DELETEs", w)
	}
	if r.tip("other") == "" || r.tip("main") == "" {
		t.Error("a branch other than the one named went")
	}
}

// A delete that FailWrite fails changes nothing, and one FailWriteAfter
// fails deletes the branch though it answers 500.
func TestRefsDeleteFailed(t *testing.T) {
	t.Parallel()
	for _, after := range []bool{false, true} {
		f, r := pullFake(t, "issuedog/7-task")
		if after {
			f.FailWriteAfter(1)
		} else {
			f.FailWrite(1)
		}
		if status, _ := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil); status != http.StatusInternalServerError {
			t.Errorf("after %v: delete %d; want 500", after, status)
		}
		if gone := r.tip("issuedog/7-task") == ""; gone != after {
			t.Errorf("after %v: the branch gone %v; want %v", after, gone, after)
		}
	}
}

// A delete's 204 is sent with no body, as GitHub's is, so the fake logs no
// failed write of one. It reads logr, so it isn't parallel.
func TestRefsDeleteNoBody(t *testing.T) {
	f, _ := pullFake(t, "issuedog/7-task")
	stop := logtest.Capture(t, logr.All)
	status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil)
	if logs := stop(); status != http.StatusNoContent || logs != "" {
		t.Errorf("delete: %d %q, logged %q; want 204, nothing logged", status, msg, logs)
	}
}

// With no git remote, a delete is a 500 naming SetGitRemote, and State
// has no branches.
func TestRefsNoRemote(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	if status, msg := pullDo(t, f, "DELETE", "repos/o/r/git/refs/heads/issuedog/7-task", nil, nil); status != http.StatusInternalServerError ||
		!strings.HasSuffix(msg, "githubtest: no git remote to read branches from: call SetGitRemote") {
		t.Errorf("delete: %d %q; want a 500 naming SetGitRemote", status, msg)
	}
	if got := f.State().Branches; got != nil {
		t.Errorf("State().Branches %+v; want none", got)
	}
}
