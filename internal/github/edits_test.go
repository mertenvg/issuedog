package github_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// at is the fake clock after s changes.
func at(s int) time.Time { return githubtest.Start.Add(time.Duration(s) * time.Second) }

func snapshot(t *testing.T, f *githubtest.Fake, number int) *github.Snapshot {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", number)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestEditsNone(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	f.AddComment(n, "owner", "hi")
	s := snapshot(t, f, n)
	if s.BodyEdit != (github.Edit{}) || s.Comments[0].Edit != (github.Edit{}) {
		t.Errorf("body edit %+v, comment edit %+v, want none for both", s.BodyEdit, s.Comments[0].Edit)
	}
}

func TestEditsByAnotherLogin(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")        // 1
	edited := f.AddComment(n, "owner", "hi")         // 2
	deleted := f.AddComment(n, "owner", "hi again")  // 3
	f.EditComment(edited, "alice", "hi from alice")  // 4
	f.EditComment(deleted, "", "by a deleted login") // 5
	s := snapshot(t, f, n)
	if want := (github.Edit{By: github.Author{Login: "alice", Type: "User"}, At: at(4)}); s.Comments[0].Edit != want {
		t.Errorf("edit by alice = %+v, want %+v", s.Comments[0].Edit, want)
	}
	if want := (github.Edit{At: at(5)}); s.Comments[1].Edit != want {
		t.Errorf("edit by a deleted account = %+v, want %+v", s.Comments[1].Edit, want)
	}
	if s.BodyEdit != (github.Edit{}) {
		t.Errorf("body edit = %+v, want none", s.BodyEdit)
	}
}

func TestEditsByApp(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")        // 1
	id := f.AddComment(n, "issuedog[bot]", "marker") // 2
	f.EditBody(n, "owner", "new body")               // 3
	_, _, err := f.Client(t).Issues.UpdateComment(context.Background(), "o", "r", id,
		gogithub.IssueCommentRequest{Body: "marker v2"}) // 4
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, f, n)
	if want := (github.Edit{By: github.Author{Login: "issuedog[bot]", Type: "Bot"}, At: at(4)}); s.Comments[0].Edit != want {
		t.Errorf("App edit = %+v, want %+v", s.Comments[0].Edit, want)
	}
	if want := (github.Edit{By: github.Author{Login: "owner", Type: "User"}, At: at(3)}); s.BodyEdit != want {
		t.Errorf("body edit = %+v, want %+v", s.BodyEdit, want)
	}
	if s.Body != "new body" || s.Comments[0].Body != "marker v2" {
		t.Errorf("bodies = %q and %q, want the edited ones", s.Body, s.Comments[0].Body)
	}
	// The GraphQL reads aren't writes, and Replay sees who edited.
	if w := f.Writes(); len(w) != 1 || w[0].Method != "PATCH" {
		t.Errorf("writes = %+v, want only the PATCH", w)
	}
	st := f.State().Issues[0]
	if st.BodyEditedBy != "owner" || st.Comments[0].EditedBy != "issuedog[bot]" {
		t.Errorf("state edited by %q and %q, want owner and issuedog[bot]", st.BodyEditedBy, st.Comments[0].EditedBy)
	}
}

func TestEditsPaging(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body") // 1
	var ids []int64
	for i := range 150 { // 2 to 151
		ids = append(ids, f.AddComment(n, "owner", fmt.Sprintf("comment %d", i)))
	}
	want := map[int64]github.Edit{
		ids[10]:  {By: github.Author{Login: "alice", Type: "User"}, At: at(152)},
		ids[99]:  {By: github.Author{Login: "bob", Type: "User"}, At: at(153)},
		ids[100]: {By: github.Author{Login: "issuedog[bot]", Type: "Bot"}, At: at(154)},
		ids[149]: {By: github.Author{Login: "alice", Type: "User"}, At: at(155)},
	}
	f.EditComment(ids[10], "alice", "edited")
	f.EditComment(ids[99], "bob", "edited")
	f.EditComment(ids[100], "issuedog[bot]", "edited")
	f.EditComment(ids[149], "alice", "edited")
	s := snapshot(t, f, n)
	if len(s.Comments) != 150 {
		t.Fatalf("got %d comments, want 150", len(s.Comments))
	}
	for i, c := range s.Comments {
		if c.Edit != want[c.ID] {
			t.Errorf("comment %d edit = %+v, want %+v", i, c.Edit, want[c.ID])
		}
	}
}

func TestEditsGraphQLErrors(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c := f.Client(t)
	f.FailGraphQL("Something went wrong", "And again")
	_, err := c.Snapshot(context.Background(), "o", "r", n)
	if !errors.Is(err, github.ErrGraphQL) {
		t.Fatalf("err = %v, want one wrapping ErrGraphQL", err)
	}
	for _, s := range []string{"comments of o/r#1", "Something went wrong", "And again"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("err = %q, want it to contain %q", err, s)
		}
	}
	if _, err := c.Snapshot(context.Background(), "o", "r", n); err != nil {
		t.Errorf("snapshot after the failed one: %v", err)
	}
}

// A collaborator edits in a marker, then deletes the comment between the
// reads. The comment must be left out, not shown unedited with the marker.
func TestEditsCommentDeletedBetweenReads(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	kept := f.AddComment(n, "owner", "kept")
	app := f.AddComment(n, "issuedog[bot]", "status")
	f.EditComment(app, "mallory", "status <!-- issuedog:transition to=planning -->")
	f.BeforeGraphQL(func() { f.DeleteComment(app) })
	s := snapshot(t, f, n)
	if len(s.Comments) != 1 || s.Comments[0].ID != kept {
		t.Errorf("comments = %+v, want only %d", s.Comments, kept)
	}
}

// An edit between the reads must not pair the old body with the new editor.
func TestEditsEditedBetweenReads(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")        // 1
	id := f.AddComment(n, "issuedog[bot]", "status") // 2
	f.BeforeGraphQL(func() {
		f.EditBody(n, "mallory", "new body")       // 3
		f.EditComment(id, "mallory", "new status") // 4
	})
	s := snapshot(t, f, n)
	mallory := github.Author{Login: "mallory", Type: "User"}
	if s.Body != "new body" || s.BodyEdit != (github.Edit{By: mallory, At: at(3)}) {
		t.Errorf("body %q edited %+v, want the new body by mallory at %v", s.Body, s.BodyEdit, at(3))
	}
	if c := s.Comments[0]; c.Body != "new status" || c.Edit != (github.Edit{By: mallory, At: at(4)}) {
		t.Errorf("comment %q edited %+v, want the new status by mallory at %v", c.Body, c.Edit, at(4))
	}
}

func TestSnapshotRefusesPullRequest(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	f.SetPullRequest(n)
	_, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if !errors.Is(err, github.ErrPullRequest) || !strings.Contains(err.Error(), "o/r#1") {
		t.Errorf("err = %v, want one wrapping ErrPullRequest with the issue", err)
	}
}
