package githubtest

import (
	"context"
	"strings"
	"testing"
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
// for CommentDeletedEvents and SubIssueRemovedEvents alone, as the fields
// it checks can't show that, and pages them by a cursor it checks.
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
		{"no filter", strings.Replace(answered, "itemTypes: [COMMENT_DELETED_EVENT, SUB_ISSUE_REMOVED_EVENT], ", "", 1), nil,
			"githubtest: the query's timelineItems doesn't ask for COMMENT_DELETED_EVENT and SUB_ISSUE_REMOVED_EVENT alone"},
		{"deletions alone", strings.Replace(answered, ", SUB_ISSUE_REMOVED_EVENT]", "]", 1), nil,
			"githubtest: the query's timelineItems doesn't ask for COMMENT_DELETED_EVENT and SUB_ISSUE_REMOVED_EVENT alone"},
		{"another filter", strings.Replace(answered, "SUB_ISSUE_REMOVED_EVENT]", "SUB_ISSUE_REMOVED_EVENT, RENAMED_TITLE_EVENT]", 1), nil,
			"githubtest: the query's timelineItems doesn't ask for COMMENT_DELETED_EVENT and SUB_ISSUE_REMOVED_EVENT alone"},
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
