package githubtest

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// As on GitHub, the fake never gives a new issue a deleted issue's
// number: each gets the one after the last filed, so a deletion leaves a
// gap. A deleted issue's paths answer 404 and its GraphQL lookup by
// number NOT_FOUND, at the issue's path, after a later issue is filed
// too, and a deletion after a gap takes the newest issue and its comments.
func TestFakeNumbersNotReused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := New(t, "o/r", "issuedog")
	c := f.Client(t)
	kept := f.AddIssue("owner", "Kept", "body")
	f.AddIssue("owner", "First gone", "body")
	f.DeleteIssue()
	second := f.AddIssue("owner", "Second gone", "body")
	if second != kept+2 {
		t.Fatalf("filed after a deletion as #%d; want #%d, not the deleted #%d", second, kept+2, kept+1)
	}
	f.AddComment(second, "owner", "goes with its issue")
	f.DeleteIssue()
	last := f.AddIssue("owner", "Last", "body")
	if last != second+1 {
		t.Fatalf("filed after a second deletion as #%d; want #%d", last, second+1)
	}
	var got []int
	for _, is := range f.State().Issues {
		got = append(got, is.Number)
	}
	if want := []int{kept, last}; !slices.Equal(got, want) {
		t.Errorf("issues %v; want %v", got, want)
	}
	if comments, _, err := c.Issues.ListComments(ctx, "o", "r", 0, nil); err != nil || len(comments) != 0 {
		t.Errorf("the repo's comments = %d, %v; want none, as the deleted #%d's went with it", len(comments), err, second)
	}
	for _, gone := range []int{kept + 1, second} {
		if status, msg := fakeNumberAnswer(t, c, http.MethodGet, fmt.Sprintf("/issues/%d", gone), nil); status != http.StatusNotFound {
			t.Errorf("GET of the deleted #%d: %d %q; want a 404", gone, status, msg)
		}
		if _, err := c.IsPinned(ctx, "o", "r", gone); !errors.Is(err, github.ErrIssueNotFound) {
			t.Errorf("IsPinned of the deleted #%d = %v; want ErrIssueNotFound", gone, err)
		}
	}
	if pinned, err := c.IsPinned(ctx, "o", "r", last); err != nil || pinned {
		t.Errorf("IsPinned of #%d = %t, %v; want false, nil", last, pinned, err)
	}
}
