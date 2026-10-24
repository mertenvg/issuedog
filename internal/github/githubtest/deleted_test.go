package githubtest

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// deletedQuery sends query to f for issue number with the deletions'
// cursor, and returns the errors and timelineItems it answers.
func deletedQuery(t *testing.T, f *Fake, query string, number int, cursor any) ([]string, []struct{ ID string }) {
	t.Helper()
	c := f.Client(t)
	vars := map[string]any{"owner": "o", "name": "r", "number": number, "cursor": nil, "deletedCursor": cursor}
	req, err := c.NewRequest(context.Background(), "POST", "graphql", map[string]any{"query": query, "variables": vars})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data struct {
			Repository *struct {
				Issue *struct {
					TimelineItems struct{ Nodes []struct{ ID string } }
				}
			}
		}
		Errors []struct{ Message string }
	}
	if _, err := c.Do(req, &out); err != nil {
		t.Fatal(err)
	}
	var errs []string
	for _, e := range out.Errors {
		errs = append(errs, e.Message)
	}
	if r := out.Data.Repository; r != nil && r.Issue != nil {
		return errs, r.Issue.TimelineItems.Nodes
	}
	return errs, nil
}

// The fake serves the deletions only to a query whose timelineItems asks
// for CommentDeletedEvents alone, as the fields it checks can't show that,
// and pages them by a cursor it checks.
func TestGraphQLDeleted(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	id := f.DeleteCommentBy("owner", f.AddComment(n, "issuedog[bot]", "gone"))
	errs, nodes := deletedQuery(t, f, answered, n, nil)
	if len(errs) != 0 || len(nodes) != 1 || nodes[0].ID != id {
		t.Errorf("errors %q and nodes %+v; want only %s", errs, nodes, id)
	}
	if errs, nodes = deletedQuery(t, f, answered, n, "1"); len(errs) != 0 || len(nodes) != 0 {
		t.Errorf("after the last one: errors %q and nodes %+v; want neither", errs, nodes)
	}
	for _, tc := range []struct {
		name, query string
		cursor      any
		want        string
	}{
		{"no filter", strings.Replace(answered, "itemTypes: [COMMENT_DELETED_EVENT], ", "", 1), nil,
			"githubtest: the query's timelineItems doesn't ask for COMMENT_DELETED_EVENT alone"},
		{"another filter", strings.Replace(answered, "[COMMENT_DELETED_EVENT]", "[COMMENT_DELETED_EVENT, RENAMED_TITLE_EVENT]", 1), nil,
			"githubtest: the query's timelineItems doesn't ask for COMMENT_DELETED_EVENT alone"},
		{"bad cursor", answered, "nope", "`nope` does not appear to be a valid cursor."},
		{"cursor past the end", answered, "2", "`2` does not appear to be a valid cursor."},
		{"no deletedCommentAuthor", strings.Replace(answered, " deletedCommentAuthor { login __typename }", "", 1), nil,
			"githubtest: the query doesn't name " +
				"query.repository.issue.timelineItems.nodes.... on CommentDeletedEvent.deletedCommentAuthor, " +
				"query.repository.issue.timelineItems.nodes.... on CommentDeletedEvent.deletedCommentAuthor.__typename, " +
				"query.repository.issue.timelineItems.nodes.... on CommentDeletedEvent.deletedCommentAuthor.login"},
	} {
		errs, nodes := deletedQuery(t, f, tc.query, n, tc.cursor)
		if len(errs) != 1 || errs[0] != tc.want || nodes != nil {
			t.Errorf("%s: errors %q and nodes %+v; want only the error %q", tc.name, errs, nodes, tc.want)
		}
	}
}

// DeleteComment records no event, and DeleteCommentBy records one on the
// comment's own issue.
func TestDeleteCommentBy(t *testing.T) {
	f := New(t, "o/r", "issuedog")
	one, two := f.AddIssue("owner", "One", "body"), f.AddIssue("owner", "Two", "body")
	f.DeleteComment(f.AddComment(one, "owner", "hi"))
	id := f.DeleteCommentBy("mallory", f.AddComment(two, "owner", "hi"))
	if _, nodes := deletedQuery(t, f, answered, one, nil); len(nodes) != 0 {
		t.Errorf("issue %d's deletions = %+v; want none", one, nodes)
	}
	if _, nodes := deletedQuery(t, f, answered, two, nil); len(nodes) != 1 || nodes[0].ID != id {
		t.Errorf("issue %d's deletions = %+v; want %s", two, nodes, id)
	}
	if c := f.State().Issues[1].Comments; len(c) != 0 {
		t.Errorf("issue %d's comments = %+v; want the deleted one gone", two, c)
	}
}

// As on GitHub (SPEC §16), an author's deletion of their own comment
// records no event, whatever the login's case, though a deletion that names
// no actor of a comment GitHub names no author of does, as either may be
// someone else. Every deletion, with an event or not, moves the issue's
// updated_at, to the deletion's time, and an event's time is the same.
func TestDeleteCommentByAuthor(t *testing.T) {
	t.Parallel()
	f := New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c, ctx := f.Client(t), context.Background()
	updated := func() time.Time {
		t.Helper()
		is, _, err := c.Issues.Get(ctx, "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return is.GetUpdatedAt().Time
	}
	for name, del := range map[string]func(id int64) string{
		"DeleteComment":               func(id int64) string { f.DeleteComment(id); return "" },
		"DeleteCommentBy(its author)": func(id int64) string { return f.DeleteCommentBy("Mallory", id) },
	} {
		id := f.AddComment(n, "mallory", "Oops.")
		before := updated()
		if node := del(id); node != "" {
			t.Errorf("%s: event %q; want none", name, node)
		}
		if got := updated(); !got.After(before) {
			t.Errorf("%s: updated_at %v, %v before; want it moved", name, got, before)
		}
	}
	if errs, nodes := deletedQuery(t, f, answered, n, nil); len(errs) != 0 || len(nodes) != 0 {
		t.Errorf("after the authors' deletions: errors %q and deletions %+v; want neither", errs, nodes)
	}
	ghost := f.DeleteCommentBy("", f.AddComment(n, "", "A ghost's note."))
	other := f.DeleteCommentBy("owner", f.AddComment(n, "mallory", "Spam."))
	at := updated()
	if _, nodes := deletedQuery(t, f, answered, n, nil); len(nodes) != 2 || nodes[0].ID != ghost || nodes[1].ID != other {
		t.Errorf("deletions %+v; want the ghost's %s, then the owner's %s", nodes, ghost, other)
	}
	f.mu.Lock()
	s := slices.Clone(f.issues[n-1].deletions)
	f.mu.Unlock()
	if len(s) != 2 || !s[1].at.Equal(at) {
		t.Errorf("deletions %+v; want the last one at updated_at, %v", s, at)
	}
}

// DeleteCommentByUnmoved deletes the comment and records the event, as
// DeleteCommentBy does for someone else's deletion, but leaves updated_at
// where it was. An author's deletion of their own comment, which SPEC §16
// confirms moves updated_at, fails the test and changes nothing. A
// deletion that names no actor, of a comment GitHub names no author of,
// is no author's own, as an empty login is never the author: it's recorded,
// and leaves updated_at where it was too.
func TestDeleteCommentByUnmoved(t *testing.T) {
	t.Parallel()
	tb := &fakeNumberTB{TB: t}
	f := New(tb, "o/r", "issuedog")
	n := f.AddIssue("owner", "Title", "body")
	c, ctx := f.Client(t), context.Background()
	updated := func() time.Time {
		t.Helper()
		is, _, err := c.Issues.Get(ctx, "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		return is.GetUpdatedAt().Time
	}
	own := f.AddComment(n, "mallory", "Oops.")
	notice := f.AddComment(n, "issuedog[bot]", "A notice.")
	before := updated()
	if fatal, panicked := fakeNumberFatal(tb, func() { f.DeleteCommentByUnmoved("Mallory", own) }); panicked != nil ||
		!slices.Equal(fatal, []string{fmt.Sprintf("githubtest: Mallory deleting their own comment %d moves updated_at: use DeleteCommentBy", own)}) {
		t.Errorf("an author's own deletion: Fatalf %q, panicked with %v; want the test failed", fatal, panicked)
	}
	if got := f.State().Issues[0].Comments; len(got) != 2 {
		t.Errorf("comments after the author's refused deletion %+v; want both kept", got)
	}
	node := f.DeleteCommentByUnmoved("mallory", notice)
	if got := updated(); !got.Equal(before) {
		t.Errorf("updated_at %v; want it unmoved, at %v", got, before)
	}
	if _, nodes := deletedQuery(t, f, answered, n, nil); node == "" || len(nodes) != 1 || nodes[0].ID != node {
		t.Errorf("deletions %+v; want only %q", nodes, node)
	}
	if got := f.State().Issues[0].Comments; len(got) != 1 || got[0].Author != "mallory" {
		t.Errorf("comments %+v; want only mallory's, the notice gone", got)
	}
	gone := f.AddComment(n, "", "A ghost's note.")
	before = updated()
	var ghost string
	if fatal, panicked := fakeNumberFatal(tb, func() { ghost = f.DeleteCommentByUnmoved("", gone) }); panicked != nil || len(fatal) != 0 {
		t.Fatalf("a deletion naming no actor of a comment naming no author: Fatalf %q, panicked with %v; want neither", fatal, panicked)
	}
	if got := updated(); !got.Equal(before) {
		t.Errorf("updated_at after the ghost's deletion %v; want it unmoved, at %v", got, before)
	}
	if _, nodes := deletedQuery(t, f, answered, n, nil); ghost == "" || len(nodes) != 2 || nodes[1].ID != ghost {
		t.Errorf("deletions %+v; want %q, then the ghost's %q", nodes, node, ghost)
	}
	if got := f.State().Issues[0].Comments; len(got) != 1 || got[0].Author != "mallory" {
		t.Errorf("comments %+v; want only mallory's, the ghost's note gone", got)
	}
}
