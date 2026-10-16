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
	    {"id":"CDE_lADOVD_MO88AAAABXlPrSc8AAAAHxvMh0g","createdAt":"2026-10-12T01:00:00Z",
	     "actor":{"login":"mallory","__typename":"User"},"deletedCommentAuthor":{"login":"issuedog-dev","__typename":"Bot"}},
	    {"id":"CDE_2","createdAt":"2026-10-12T02:00:00Z","actor":null,"deletedCommentAuthor":null}]}}}}}`)
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

// A deletions page that can't be paged, or a node that isn't a
// CommentDeletedEvent, fails the read rather than miss a deletion.
func TestDeletedBadAnswers(t *testing.T) {
	page := func(next bool, cursor, node string) string {
		return `{"data":{"repository":{"issue":{"body":"","comments":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]},` +
			`"timelineItems":{"pageInfo":{"hasNextPage":` + strconv.FormatBool(next) + `,"endCursor":` + cursor + `},"nodes":[` + node + `]}}}}}`
	}
	event := `{"id":"CDE_1","createdAt":"2026-10-12T01:00:00Z"}`
	for _, tc := range []struct {
		name    string
		answers []string
		want    error
	}{
		{"no cursor", []string{page(true, "null", event)}, errCursor},
		{"same cursor", []string{page(true, `"d1"`, event)}, errCursor},
		{"cursor back", []string{page(true, `"d1"`, event), page(true, `"d2"`, event), page(true, `"d1"`, event)}, errCursor},
		{"a node with no ID", []string{page(false, "null", `{}`)}, errDeletedNoID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := graphQLAnswers(t, tc.answers...).readComments(context.Background(), "o", "r", &Snapshot{Number: 7})
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "comments of o/r#7") {
				t.Errorf("err = %v, want one wrapping %v with the issue", err, tc.want)
			}
		})
	}
}
