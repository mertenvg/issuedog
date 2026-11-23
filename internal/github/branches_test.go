package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// DeleteBranch deletes a branch under issuedog/ with one DELETE of its
// ref, and the remote no longer has it, nor State. A branch gone already,
// GitHub's 422 "Reference does not exist", or a 404, is no error. Any
// other failure is, a 422 with another message included.
func TestDeleteBranch(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task", "issuedog/5-story")
	c, ctx := f.Client(t), context.Background()
	if err := c.DeleteBranch(ctx, "o", "r", "issuedog/7-task"); err != nil {
		t.Fatal(err)
	}
	if got := git("for-each-ref", "--format=%(refname)", "refs/heads/"); got != "refs/heads/issuedog/5-story\nrefs/heads/main" {
		t.Errorf("the remote's branches %q; want the story branch and main left", got)
	}
	story := git("rev-parse", "refs/heads/issuedog/5-story")
	if got, want := f.State().Branches, []githubtest.BranchState{{Name: "issuedog/5-story", SHA: story}}; !reflect.DeepEqual(got, want) {
		t.Errorf("State().Branches %+v; want %+v", got, want)
	}
	if w := f.Writes(); len(w) != 1 || w[0].Method != http.MethodDelete || w[0].Path != "/git/refs/heads/issuedog/7-task" {
		t.Errorf("writes %+v; want one DELETE /git/refs/heads/issuedog/7-task", w)
	}
	if err := c.DeleteBranch(ctx, "o", "r", "issuedog/7-task"); err != nil {
		t.Errorf("delete a branch gone already: %v; want nil", err)
	}
	isDelete := func(r githubtest.Request) bool { return r.Method == http.MethodDelete }
	f.AnswerOnce(isDelete, http.StatusNotFound)
	if err := c.DeleteBranch(ctx, "o", "r", "issuedog/5-story"); err != nil {
		t.Errorf("delete answered 404: %v; want nil", err)
	}
	var er *gogithub.ErrorResponse
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusInternalServerError} {
		f.AnswerOnce(isDelete, status)
		if err := c.DeleteBranch(ctx, "o", "r", "issuedog/5-story"); !errors.As(err, &er) || er.Response.StatusCode != status {
			t.Errorf("delete answered %d alone: %v; want GitHub's %d", status, err, status)
		}
	}
	if got := git("rev-parse", "refs/heads/issuedog/5-story"); got != story {
		t.Errorf("the story branch's tip %s after the answers alone; want it left at %s", got, story)
	}
}

// DeleteBranch refuses a branch outside issuedog/, main included, and
// issuedog/ itself, before any request.
func TestDeleteBranchRefused(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "other", "issuedogs/1")
	c, ctx := f.Client(t), context.Background()
	from := len(f.Requests())
	for _, branch := range []string{"main", "other", "issuedog/", "issuedogs/1", ""} {
		if err := c.DeleteBranch(ctx, "o", "r", branch); err == nil || err.Error() != fmt.Sprintf("delete branch %q of o/r: it isn't one of the App's, under issuedog/", branch) {
			t.Errorf("delete %q: %v; want it refused as no branch of the App's", branch, err)
		}
	}
	if got := f.Requests()[from:]; len(got) != 0 {
		t.Errorf("requests %+v; want none", got)
	}
	if got := git("for-each-ref", "--format=%(refname)", "refs/heads/"); got != "refs/heads/issuedogs/1\nrefs/heads/main\nrefs/heads/other" {
		t.Errorf("the remote's branches %q; want all left", got)
	}
}

// Deleting a branch is safe to replay after a crash at any write: the
// branch is gone, and a second delete finds it gone and deletes nothing
// more.
func TestDeleteBranchReplay(t *testing.T) {
	t.Parallel()
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f, _ := pullsFake(tb, "issuedog/7-task", "issuedog/5-story")
		return f
	}, func(f *githubtest.Fake) error {
		if err := f.Client(t).DeleteBranch(context.Background(), "o", "r", "issuedog/7-task"); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		return nil
	})
}
