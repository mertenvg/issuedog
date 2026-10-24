package github

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestDeletedAnswer reads deletions shaped as GitHub's schema gives them
// (CommentDeletedEvent: id, createdAt, actor and deletedCommentAuthor,
// each an Actor), whose Bot logins have no "[bot]", beside the comments.
func TestDeletedAnswer(t *testing.T) {
	c := graphQLAnswers(t, `{"data":{"repository":{"issue":{"body":"spec","lastEditedAt":null,"editor":null,
	  "comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},
	  "timelineItems":{"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOnYyOpPPAAABmgA="},"nodes":[
	    {"__typename":"CommentDeletedEvent","id":"CDE_lADOVD_MO88AAAABXlPrSc8AAAAHxvMh0g","createdAt":"2026-10-12T01:00:00Z",
	     "actor":{"login":"mallory","__typename":"User"},"deletedCommentAuthor":{"login":"issuedog-dev","__typename":"Bot"}},
	    {"__typename":"CommentDeletedEvent","id":"CDE_2","createdAt":"2026-10-12T02:00:00Z","actor":null,"deletedCommentAuthor":null}]}}}}}`)
	s := &Snapshot{Number: 1}
	if err := c.readComments(context.Background(), "o", "r", s); err != nil {
		t.Fatal(err)
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	want := []DeletedComment{
		{ID: "CDE_lADOVD_MO88AAAABXlPrSc8AAAAHxvMh0g", By: Author{Login: "mallory", Type: "User"},
			Author: Author{Login: "issuedog-dev[bot]", Type: "Bot"}, At: at(1)},
		{ID: "CDE_2", At: at(2)},
	}
	if !reflect.DeepEqual(s.Deleted, want) {
		t.Errorf("deleted =\n%+v\nwant\n%+v", s.Deleted, want)
	}
}

// TestSubIssueRemovedAnswer reads removals of sub-issue links shaped as
// GitHub's GraphQL docs give them (SubIssueRemovedEvent: id, createdAt,
// actor, an Actor, and subIssue, an Issue, here with its number and its
// repository's nameWithOwner), oldest first, among the deletions, which
// they don't join. One whose subIssue is null, as for an issue GitHub
// doesn't show the App, names no sub-issue, and isn't kept.
func TestSubIssueRemovedAnswer(t *testing.T) {
	t.Parallel()
	c := graphQLAnswers(t, `{"data":{"repository":{"issue":{"body":"spec","lastEditedAt":null,"editor":null,
	  "comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},
	  "timelineItems":{"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOnYyOpPPAAABmgA="},"nodes":[
	    {"__typename":"SubIssueRemovedEvent","id":"SIRE_1","createdAt":"2026-10-12T01:00:00Z",
	     "actor":{"login":"mallory","__typename":"User"},"subIssue":{"number":7,"repository":{"nameWithOwner":"o/r"}}},
	    {"__typename":"CommentDeletedEvent","id":"CDE_2","createdAt":"2026-10-12T02:00:00Z","actor":null,"deletedCommentAuthor":null},
	    {"__typename":"SubIssueRemovedEvent","id":"SIRE_3","createdAt":"2026-10-12T03:00:00Z","actor":null,"subIssue":null},
	    {"__typename":"SubIssueRemovedEvent","id":"SIRE_4","createdAt":"2026-10-12T04:00:00Z",
	     "actor":{"login":"issuedog-dev","__typename":"Bot"},"subIssue":{"number":2,"repository":{"nameWithOwner":"o/other"}}}]}}}}}`)
	s := &Snapshot{Number: 1}
	if err := c.readComments(context.Background(), "o", "r", s); err != nil {
		t.Fatal(err)
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	want := []SubIssueRemoval{
		{ID: "SIRE_1", Repo: "o/r", Number: 7, By: Author{Login: "mallory", Type: "User"}, At: at(1)},
		{ID: "SIRE_4", Repo: "o/other", Number: 2, By: Author{Login: "issuedog-dev[bot]", Type: "Bot"}, At: at(4)},
	}
	if !reflect.DeepEqual(s.SubIssueRemovals, want) {
		t.Errorf("removals =\n%+v\nwant\n%+v", s.SubIssueRemovals, want)
	}
	if want := []DeletedComment{{ID: "CDE_2", At: at(2)}}; !reflect.DeepEqual(s.Deleted, want) {
		t.Errorf("deleted =\n%+v\nwant\n%+v", s.Deleted, want)
	}
}

// A deletions page that can't be paged, or a node that isn't a
// CommentDeletedEvent or a SubIssueRemovedEvent, fails the read rather
// than miss a deletion or a removal.
func TestDeletedBadAnswers(t *testing.T) {
	page := func(next bool, cursor, node string) string {
		return `{"data":{"repository":{"issue":{"body":"","comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},` +
			`"timelineItems":{"pageInfo":{"hasNextPage":` + strconv.FormatBool(next) + `,"endCursor":` + cursor + `},"nodes":[` + node + `]}}}}}`
	}
	event := `{"__typename":"CommentDeletedEvent","id":"CDE_1","createdAt":"2026-10-12T01:00:00Z"}`
	for _, tc := range []struct {
		name    string
		answers []string
		want    error
	}{
		{"no cursor", []string{page(true, "null", event)}, errCursor},
		{"no cursor, then a last page", []string{page(true, "null", event), page(false, "null", event)}, errCursor},
		{"same cursor", []string{page(true, `"d1"`, event)}, errCursor},
		{"cursor back", []string{page(true, `"d1"`, event), page(true, `"d2"`, event), page(true, `"d1"`, event)}, errCursor},
		{"a node with no ID", []string{page(false, "null", `{}`)}, errEventNoID},
		{"a node of another type", []string{page(false, "null", `{"__typename":"SubIssueAddedEvent","id":"SIAE_1","createdAt":"2026-10-12T01:00:00Z","subIssue":{"number":3,"repository":{"nameWithOwner":"o/r"}}}`)}, errEventType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := graphQLAnswers(t, tc.answers...).readComments(context.Background(), "o", "r", &Snapshot{Number: 7})
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "comments of o/r#7") {
				t.Errorf("err = %v, want one wrapping %v with the issue", err, tc.want)
			}
		})
	}
}
