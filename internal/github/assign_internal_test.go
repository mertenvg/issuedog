package github

import (
	"errors"
	"slices"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// An assignee write's answer that is an issue of another repo, as when a
// write to a transferred issue is sent on to the issue itself, is
// ErrTransferred: the write wrote nothing. One of this repo, in any case,
// gives its assignees, and an error is passed on as it is.
func TestAssignAnswerMoved(t *testing.T) {
	t.Parallel()
	is := func(url string) *gogithub.Issue {
		return &gogithub.Issue{RepositoryURL: &url, Assignees: []*gogithub.User{{Login: new("owner")}}}
	}
	if _, err := assignAnswer("o", "r", is("https://api.github.com/repos/o/elsewhere"), nil); !errors.Is(err, ErrTransferred) {
		t.Errorf("an answer from o/elsewhere = %v; want ErrTransferred", err)
	}
	if got, err := assignAnswer("o", "r", is("https://api.github.com/repos/O/R"), nil); err != nil || !slices.Equal(got, []string{"owner"}) {
		t.Errorf("an answer from O/R = %q, %v; want owner, and no error", got, err)
	}
	failed := errors.New("failed")
	if _, err := assignAnswer("o", "r", nil, failed); !errors.Is(err, failed) {
		t.Errorf("a failed write = %v; want its error", err)
	}
}
