package githubtest

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

// A helper given a comment ID the fake hasn't got, one never posted, one
// deleted, or one deleted with its issue, fails the test with a message
// naming the ID, and changes nothing, rather than panicking or doing
// nothing. The comment the fake has still takes every helper.
func TestFakeCommentUnknown(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f := New(tb, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "body")
	kept := f.AddComment(n, "owner", "kept")
	up := f.AddReaction(kept, "owner", "+1")
	deleted := f.AddComment(n, "owner", "deleted")
	f.DeleteComment(deleted)
	gone := f.AddIssue("owner", "Gone", "body")
	withIssue := f.AddComment(gone, "owner", "gone with its issue")
	f.DeleteIssue()
	before := f.State()
	for _, id := range []int64{9, deleted, withIssue} {
		for _, tc := range []struct {
			name string
			do   func()
		}{
			{"EditComment", func() { f.EditComment(id, "owner", "new") }},
			{"DeleteComment", func() { f.DeleteComment(id) }},
			{"DeleteCommentBy", func() { f.DeleteCommentBy("owner", id) }},
			{"AddReaction", func() { f.AddReaction(id, "owner", "eyes") }},
			{"RemoveReaction", func() { f.RemoveReaction(id, up) }},
		} {
			fatal, panicked := fakeNumberFatal(tb, tc.do)
			if panicked != nil {
				t.Errorf("%s(%d): panicked with %v; want the test failed", tc.name, id, panicked)
			}
			if want := []string{fmt.Sprintf("githubtest: no comment %d: the fake has 1", id)}; !slices.Equal(fatal, want) {
				t.Errorf("%s(%d): Fatalf %q; want %q", tc.name, id, fatal, want)
			}
		}
	}
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the unknown comments:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
	for _, tc := range []struct {
		name string
		do   func()
	}{
		{"EditComment", func() { f.EditComment(kept, "owner", "edited") }},
		{"AddReaction", func() { f.AddReaction(kept, "owner", "eyes") }},
		{"RemoveReaction", func() { f.RemoveReaction(kept, up) }},
	} {
		if fatal, panicked := fakeNumberFatal(tb, tc.do); fatal != nil || panicked != nil {
			t.Errorf("%s(%d): Fatalf %q, panicked with %v; want neither", tc.name, kept, fatal, panicked)
		}
	}
	want := []CommentState{{Author: "owner", Body: "edited", EditedBy: "owner", Reactions: []string{"owner eyes"}}}
	if got := f.State().Issues[n-1].Comments; !reflect.DeepEqual(got, want) {
		t.Errorf("the kept comment %+v; want %+v", got, want)
	}
	if fatal, panicked := fakeNumberFatal(tb, func() { f.DeleteCommentBy("owner", kept) }); fatal != nil || panicked != nil {
		t.Errorf("DeleteCommentBy(%d): Fatalf %q, panicked with %v; want neither", kept, fatal, panicked)
	}
	if got := f.State().Issues[n-1].Comments; len(got) != 0 {
		t.Errorf("comments after DeleteCommentBy %+v; want none", got)
	}
}

// fakeRevisionTexts is issue n's body history's texts, oldest first.
func fakeRevisionTexts(f *Fake, n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var texts []string
	for _, e := range f.issues[n-1].history {
		texts = append(texts, e.text)
	}
	return texts
}

// DeleteRevision given an entry an issue body's history hasn't got fails
// the test, naming the entry and the history's length, and changes
// nothing: any entry of a body never edited, one past the last, and one
// below 0. An entry the history has is deleted.
func TestFakeRevisionUnknown(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f := New(tb, "o/r", "issuedog")
	n := f.AddIssue("owner", "Issue", "first")
	fatal, panicked := fakeNumberFatal(tb, func() { f.DeleteRevision(n, 0) })
	if want := []string{"githubtest: no revision 0 of issue #1: its history has 0"}; panicked != nil || !slices.Equal(fatal, want) {
		t.Errorf("DeleteRevision(%d, 0) of a body never edited: Fatalf %q, panicked with %v; want Fatalf %q", n, fatal, panicked, want)
	}
	f.EditBody(n, "owner", "second")
	for _, i := range []int{2, -1} {
		fatal, panicked := fakeNumberFatal(tb, func() { f.DeleteRevision(n, i) })
		if want := []string{fmt.Sprintf("githubtest: no revision %d of issue #1: its history has 2", i)}; panicked != nil || !slices.Equal(fatal, want) {
			t.Errorf("DeleteRevision(%d, %d): Fatalf %q, panicked with %v; want Fatalf %q", n, i, fatal, panicked, want)
		}
	}
	if got, want := fakeRevisionTexts(f, n), []string{"first", "second"}; !slices.Equal(got, want) {
		t.Errorf("history after the unknown entries %q; want %q", got, want)
	}
	if fatal, panicked := fakeNumberFatal(tb, func() { f.DeleteRevision(n, 1) }); fatal != nil || panicked != nil {
		t.Errorf("DeleteRevision(%d, 1): Fatalf %q, panicked with %v; want neither", n, fatal, panicked)
	}
	if got, want := fakeRevisionTexts(f, n), []string{"first", ""}; !slices.Equal(got, want) {
		t.Errorf("history after DeleteRevision(%d, 1) %q; want %q", n, got, want)
	}
}

// fakeDeleteSubs lists issue n's sub-issues as "repo#number".
func fakeDeleteSubs(t *testing.T, c *github.Client, n int) []string {
	t.Helper()
	got := []string{}
	for sub, err := range c.SubIssue.ListByIssueIter(context.Background(), "o", "r", int64(n), nil) {
		if err != nil {
			t.Fatal(err)
		}
		is := (*gogithub.Issue)(sub)
		_, repo, _ := strings.Cut(is.GetRepositoryURL(), "/repos/")
		got = append(got, fmt.Sprintf("%s#%d", repo, is.GetNumber()))
	}
	return got
}

// DeleteIssue drops the deleted issue's links, as GitHub would: its
// sub-issues have no parent, its parent lists it no more, nor does a
// LinkSubIssue listing that names this repo, in any case, and the
// sub-issues LinkSubIssue listed under it go, and no blocked-by link names
// it, either way. An issue filed after it, which gets its number, has none
// of them. Another repo's issue of that number stays listed.
func TestFakeDeleteIssueLinks(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	parent, child := f.AddIssue("owner", "Parent", "body"), f.AddIssue("owner", "Child", "body")
	blocker, blockedIs := f.AddIssue("owner", "Blocker", "body"), f.AddIssue("owner", "Blocked", "body")
	gone := f.AddIssue("owner", "Gone", "body")
	f.SetParent(gone, parent)
	f.SetParent(child, gone)
	f.SetBlockedBy(gone, blocker)
	f.SetBlockedBy(blockedIs, gone)
	f.LinkSubIssue(gone, "o/other", 7)
	f.LinkSubIssue(parent, "O/R", gone)
	f.LinkSubIssue(parent, "o/other", gone)
	c := f.Client(t)
	other := fmt.Sprintf("o/other#%d", gone)
	if got, want := fakeDeleteSubs(t, c, parent), []string{fmt.Sprintf("o/r#%d", gone), fmt.Sprintf("O/R#%d", gone), other}; !slices.Equal(got, want) {
		t.Fatalf("the parent's sub-issues before the delete %q; want %q", got, want)
	}
	f.DeleteIssue()
	if got, want := fakeDeleteSubs(t, c, parent), []string{other}; !slices.Equal(got, want) {
		t.Errorf("the parent's sub-issues %q; want %q", got, want)
	}
	if status, msg := fakeNumberAnswer(t, c, http.MethodGet, fmt.Sprintf("/issues/%d/parent", child), nil); status != http.StatusNotFound {
		t.Errorf("the child's parent: %d %q; want a 404, as it has none", status, msg)
	}
	if got := blockers(t, c, blockedIs); len(got) != 0 {
		t.Errorf("the issues blocking #%d %q; want none", blockedIs, got)
	}
	if got := blocked(t, c, blocker); len(got) != 0 {
		t.Errorf("the issues #%d blocks %q; want none", blocker, got)
	}
	for _, st := range f.State().Issues {
		if st.Parent == gone || slices.Contains(st.SubIssues, gone) || slices.Contains(st.BlockedBy, gone) {
			t.Errorf("#%d after the delete %+v; want no link to #%d", st.Number, st, gone)
		}
	}
	refiled := f.AddIssue("owner", "Refiled", "body")
	if refiled != gone {
		t.Fatalf("refiled as #%d; want #%d", refiled, gone)
	}
	if got := fakeDeleteSubs(t, c, refiled); len(got) != 0 {
		t.Errorf("the refiled issue's sub-issues %q; want none", got)
	}
	if got := blockers(t, c, refiled); len(got) != 0 {
		t.Errorf("the issues blocking the refiled issue %q; want none", got)
	}
	if got := blocked(t, c, refiled); len(got) != 0 {
		t.Errorf("the issues the refiled issue blocks %q; want none", got)
	}
	if got, want := fakeDeleteSubs(t, c, parent), []string{other}; !slices.Equal(got, want) {
		t.Errorf("the parent's sub-issues after the refile %q; want %q", got, want)
	}
}

// An edit that sets a type the org has no type of, ignoring case, is
// refused with a 422, as a create is, and changes nothing: the org gets no
// such type from it, so a create with it is refused after it too. A type
// the org has, in any case, is still set by an edit.
func TestFakeTypeUnknownOnEdit(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	task := f.IssueTypeID("Task")
	n := f.AddIssue("owner", "Issue", "")
	ctx, c := context.Background(), f.Client(t)
	before := f.State()
	_, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Type: new("Epic")})
	fakeTypeWant422(t, "edit with Epic, which the org hasn't got", err)
	if got := f.State(); !reflect.DeepEqual(got, before) {
		t.Errorf("state after the refused edit:\n%+v\nwant it unchanged:\n%+v", got, before)
	}
	_, _, err = c.Issues.Create(ctx, "o", "r", gogithub.CreateIssueRequest{Title: "After", Type: new("epic")})
	fakeTypeWant422(t, "create with epic after the refused edit", err)
	if is, _, err := c.Issues.Update(ctx, "o", "r", n, gogithub.UpdateIssueRequest{Type: new("task")}); err != nil || is.GetType().GetID() != task {
		t.Errorf("edit with task = %+v, %v; want the org's Task, %d", is.GetType(), err, task)
	}
}

// Both reaction listings, an issue's and a comment's, answer a content=
// that is no reaction GitHub has with a 422, and take every one it has.
func TestFakeReactionContentUnknown(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	cm := f.AddComment(n, "owner", "hi")
	f.AddIssueReaction(n, "owner", "+1")
	f.AddReaction(cm, "owner", "+1")
	c := f.Client(t)
	for _, path := range []string{fmt.Sprintf("/issues/%d/reactions", n), fmt.Sprintf("/issues/comments/%d/reactions", cm)} {
		for _, content := range []string{"thumbsup", "+2", "Heart"} {
			status, msg := fakeNumberAnswer(t, c, http.MethodGet, path+"?content="+url.QueryEscape(content), nil)
			if status != http.StatusUnprocessableEntity || !strings.HasPrefix(msg, "Validation Failed") {
				t.Errorf("%s with content %q: %d %q; want a 422, Validation Failed", path, content, status, msg)
			}
		}
		for _, content := range []string{"+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"} {
			if status, msg := fakeNumberAnswer(t, c, http.MethodGet, path+"?content="+url.QueryEscape(content), nil); status != http.StatusOK {
				t.Errorf("%s with content %q: %d %q; want a 200", path, content, status, msg)
			}
		}
	}
}
