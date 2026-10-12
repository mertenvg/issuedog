package github

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// graphQLAnswers answers each GraphQL query with the next of its answers,
// repeating the last.
func graphQLAnswers(t *testing.T, answers ...string) *Client {
	t.Helper()
	var n atomic.Int64
	return newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/graphql" {
			http.NotFound(w, r)
			return
		}
		a := answers[min(int(n.Add(1))-1, len(answers)-1)]
		if _, err := io.WriteString(w, a); err != nil {
			t.Errorf("write answer: %v", err)
		}
	}))
}

// TestEditBotLogin reads an answer shaped like GitHub's, whose Bot logins
// have no "[bot]", and checks that it is added once.
func TestEditBotLogin(t *testing.T) {
	c := graphQLAnswers(t, `{"data":{"repository":{"issue":{"body":"spec",
	  "lastEditedAt":"2026-10-12T03:00:00Z","editor":{"login":"issuedog-dev","__typename":"Bot"},
	  "comments":{"pageInfo":{"hasNextPage":false,"endCursor":"Y3Vyc29yOnYyOpHPAAAAAWxE-eE="},"nodes":[
	    {"fullDatabaseId":"6106375555","body":"a","createdAt":"2026-10-12T01:00:00Z","updatedAt":"2026-10-12T06:00:00Z",
	     "author":{"login":"issuedog-dev","__typename":"Bot"},
	     "lastEditedAt":"2026-10-12T04:00:00Z","editor":{"login":"issuedog-dev[bot]","__typename":"Bot"}},
	    {"fullDatabaseId":"6110641371","body":"b","createdAt":"2026-10-12T02:00:00Z","updatedAt":"2026-10-12T05:00:00Z",
	     "author":null,"lastEditedAt":"2026-10-12T05:00:00Z","editor":null},
	    {"fullDatabaseId":"6111427042","body":"d","createdAt":"2026-10-12T02:00:00Z","updatedAt":"2026-10-12T02:00:00Z",
	     "author":{"login":"mertenvg","__typename":"User"},"lastEditedAt":null,"editor":null}]}}}}}`)
	s := &Snapshot{Number: 1}
	if err := c.readComments(context.Background(), "o", "r", s); err != nil {
		t.Fatal(err)
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	app, owner := Author{Login: "issuedog-dev[bot]", Type: "Bot"}, Author{Login: "mertenvg", Type: "User"}
	want := &Snapshot{Number: 1, Body: "spec", BodyEdit: Edit{By: app, At: at(3)}, Comments: []Comment{
		{ID: 6106375555, Author: app, Body: "a", CreatedAt: at(1), UpdatedAt: at(6), Edit: Edit{By: app, At: at(4)}},
		{ID: 6110641371, Body: "b", CreatedAt: at(2), UpdatedAt: at(5), Edit: Edit{At: at(5)}},
		{ID: 6111427042, Author: owner, Body: "d", CreatedAt: at(2), UpdatedAt: at(2)},
	}}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("snapshot =\n%+v\nwant\n%+v", s, want)
	}
}

func TestReadCommentsBadAnswers(t *testing.T) {
	page := func(next bool, cursor, id string) string {
		return `{"data":{"repository":{"issue":{"body":"","comments":{"pageInfo":{"hasNextPage":` + strconv.FormatBool(next) +
			`,"endCursor":` + cursor + `},"nodes":[{"fullDatabaseId":` + id + `,"body":""}]}}}}}`
	}
	for _, tc := range []struct {
		name    string
		answers []string
		want    error
	}{
		{"no repository", []string{`{"data":{"repository":null}}`}, errNoIssue},
		{"no issue", []string{`{"data":{"repository":{"issue":null}}}`}, errNoIssue},
		{"no cursor", []string{page(true, "null", `"1"`)}, errCursor},
		{"same cursor", []string{page(true, `"c1"`, `"1"`)}, errCursor},
		{"cursor back", []string{page(true, `"c1"`, `"1"`), page(true, `"c2"`, `"2"`), page(true, `"c1"`, `"3"`)}, errCursor},
		{"bad ID", []string{page(false, "null", `"x1"`)}, strconv.ErrSyntax},
		{"null ID", []string{page(false, "null", `null`)}, strconv.ErrSyntax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := graphQLAnswers(t, tc.answers...).readComments(context.Background(), "o", "r", &Snapshot{Number: 7})
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "comments of o/r#7") {
				t.Errorf("err = %v, want one wrapping %v with the issue", err, tc.want)
			}
		})
	}
}
