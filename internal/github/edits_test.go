package github_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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

func history(t *testing.T, f *githubtest.Fake, nodes ...string) map[string][]github.Edit {
	t.Helper()
	h, err := f.Client(t).History(context.Background(), nodes)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// Bodies and comments are read in one query, oldest first, the original
// post first, and an unedited one has none.
func TestHistory(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")    // 1
	app := f.AddComment(n, "issuedog[bot]", "a") // 2
	f.AddComment(n, "owner", "b")                // 3
	f.EditComment(app, "mallory", "a2")          // 4
	f.EditComment(app, "issuedog[bot]", "a3")    // 5
	f.EditBody(n, "owner", "body2")              // 6
	s := snapshot(t, f, n)
	h := history(t, f, s.BodyNode, s.Comments[0].Node, s.Comments[1].Node)
	bot, owner := github.Author{Login: "issuedog[bot]", Type: "Bot"}, github.Author{Login: "owner", Type: "User"}
	want := map[string][]github.Edit{
		s.BodyNode:         {{By: owner, At: at(1)}, {By: owner, At: at(6)}},
		s.Comments[0].Node: {{By: bot, At: at(2)}, {By: github.Author{Login: "mallory", Type: "User"}, At: at(4)}, {By: bot, At: at(5)}},
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("history =\n%+v\nwant\n%+v", h, want)
	}
	if q := f.HistoryQueries(); q != 1 {
		t.Errorf("history queries = %d, want 1 for three nodes", q)
	}
}

// A history of more than 100 entries is read in full, beside a short one.
// GitHub caps histories at 100, so the fake's cap is lifted for this.
func TestHistoryPaging(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	f.HistoryCap(0)
	n := f.AddIssue("owner", "Title", "body") // 1
	id := f.AddComment(n, "owner", "hi")      // 2
	other := f.AddComment(n, "owner", "yo")   // 3
	f.EditComment(id, "mallory", "evil")      // 4
	for i := range 250 {                      // 5 to 254
		f.EditComment(id, "owner", fmt.Sprintf("v%d", i))
	}
	f.EditComment(other, "owner", "yo2") // 255
	s := snapshot(t, f, n)
	h := history(t, f, s.Comments[0].Node, s.Comments[1].Node)
	got := h[s.Comments[0].Node]
	if len(got) != 252 {
		t.Fatalf("got %d entries, want 252", len(got))
	}
	if got[0].At != at(2) || got[1].By.Login != "mallory" || got[1].At != at(4) || got[251].At != at(254) {
		t.Errorf("entries 0, 1 and 251 = %+v, %+v and %+v, want the post, mallory's edit and the last", got[0], got[1], got[251])
	}
	if o := h[s.Comments[1].Node]; len(o) != 2 || o[1].At != at(255) {
		t.Errorf("the other comment's history = %+v, want its post and one edit", o)
	}
	if q := f.HistoryQueries(); q != 3 {
		t.Errorf("history queries = %d, want 3", q)
	}

	// With GitHub's cap: the original post and the 99 newest edits.
	f.HistoryCap(100)
	got = history(t, f, s.Comments[0].Node)[s.Comments[0].Node]
	if len(got) != 100 || got[0].At != at(2) || got[1].At != at(156) || got[99].At != at(254) {
		t.Errorf("capped history = %d entries from %v, %v to %v, want 100: the post, then %v to %v",
			len(got), got[0].At, got[min(1, len(got)-1)].At, got[len(got)-1].At, at(156), at(254))
	}
}

func TestHistoryUnknownNode(t *testing.T) {
	f := githubtest.New(t, "o/r", "issuedog")
	_, err := f.Client(t).History(context.Background(), []string{"IC_9"})
	if !errors.Is(err, github.ErrGraphQL) || !strings.Contains(err.Error(), "IC_9") {
		t.Errorf("err = %v, want one wrapping ErrGraphQL that names the node", err)
	}
}
