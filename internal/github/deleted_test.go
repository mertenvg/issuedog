package github_test

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A snapshot reads each CommentDeletedEvent, oldest first: who deleted the
// comment, who wrote it, when and the event's node ID. A Bot's login gets
// its "[bot]", and an actor GitHub doesn't name is no Author. A deletion
// with no event, as DeleteComment makes, isn't read.
func TestSnapshotDeleted(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")       // 1
	apps := f.AddComment(n, "issuedog[bot]", "rev") // 2
	mine := f.AddComment(n, "owner", "hi")          // 3
	quiet := f.AddComment(n, "mallory", "oops")     // 4
	kept := f.AddComment(n, "owner", "kept")        // 5
	first := f.DeleteCommentBy("mallory", apps)     // 6
	second := f.DeleteCommentBy("", mine)           // 7
	f.DeleteComment(quiet)
	other := f.AddIssue("owner", "Other", "body")
	f.DeleteCommentBy("owner", f.AddComment(other, "owner", "elsewhere"))
	s := snapshot(t, f, n)
	want := []github.DeletedComment{
		{ID: first, By: github.Author{Login: "mallory", Type: "User"}, Author: github.Author{Login: "issuedog[bot]", Type: "Bot"}, At: at(6)},
		{ID: second, Author: github.Author{Login: "owner", Type: "User"}, At: at(7)},
	}
	if !reflect.DeepEqual(s.Deleted, want) {
		t.Errorf("deleted =\n%+v\nwant\n%+v", s.Deleted, want)
	}
	if first == "" || first == second {
		t.Errorf("event IDs %q and %q; want two different ones", first, second)
	}
	if len(s.Comments) != 1 || s.Comments[0].ID != kept {
		t.Errorf("comments = %+v, want only %d", s.Comments, kept)
	}
	if again := snapshot(t, f, n); !reflect.DeepEqual(again.Deleted, s.Deleted) {
		t.Errorf("a second read gave %+v; want the same deletions, IDs included", again.Deleted)
	}
}

// The deletions page by 100 beside the comments, so either list may have
// more pages than the other, and nothing is read twice or missed.
func TestSnapshotDeletedPaging(t *testing.T) {
	for _, tc := range []struct{ comments, deleted int }{{150, 3}, {3, 250}, {0, 101}, {101, 0}, {120, 120}} {
		t.Run(fmt.Sprintf("%d comments, %d deleted", tc.comments, tc.deleted), func(t *testing.T) {
			f := githubtest.New(t, "o/r", "issuedog")
			n := f.AddIssue("owner", "Title", "body")
			var ids []string
			for range tc.deleted {
				ids = append(ids, f.DeleteCommentBy("owner", f.AddComment(n, "issuedog[bot]", "gone")))
			}
			for i := range tc.comments {
				f.AddComment(n, "owner", fmt.Sprintf("comment %d", i))
			}
			s := snapshot(t, f, n)
			if len(s.Comments) != tc.comments {
				t.Errorf("got %d comments, want %d", len(s.Comments), tc.comments)
			}
			var got []string
			for _, d := range s.Deleted {
				got = append(got, d.ID)
			}
			if !reflect.DeepEqual(got, ids) {
				t.Errorf("got %d deletions, want %d, in order", len(got), len(ids))
			}
		})
	}
}
