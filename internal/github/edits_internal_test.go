package github

import (
	"context"
	"errors"
	"io"
	"net/http"
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
	    {"fullDatabaseId":"6106375555","body":"a","lastEditedAt":"2026-10-12T04:00:00Z","editor":{"login":"issuedog-dev[bot]","__typename":"Bot"}},
	    {"fullDatabaseId":"6110641371","body":"b","lastEditedAt":"2026-10-12T05:00:00Z","editor":{"login":"mertenvg","__typename":"User"}},
	    {"fullDatabaseId":"6111427041","body":"c","lastEditedAt":"2026-10-12T06:00:00Z","editor":null},
	    {"fullDatabaseId":"6111427042","body":"d","lastEditedAt":null,"editor":null}]}}}}}`)
	body, comments, err := c.edits(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	at := func(h int) time.Time { return time.Date(2026, 10, 12, h, 0, 0, 0, time.UTC) }
	app := Author{Login: "issuedog-dev[bot]", Type: "Bot"}
	if want := (edited{"spec", Edit{By: app, At: at(3)}}); body != want {
		t.Errorf("body = %+v, want %+v", body, want)
	}
	want := map[int64]edited{
		6106375555: {"a", Edit{By: app, At: at(4)}},
		6110641371: {"b", Edit{By: Author{Login: "mertenvg", Type: "User"}, At: at(5)}},
		6111427041: {"c", Edit{At: at(6)}},
		6111427042: {"d", Edit{}},
	}
	if len(comments) != len(want) {
		t.Errorf("read %d comments, want %d", len(comments), len(want))
	}
	for id, w := range want {
		if comments[id] != w {
			t.Errorf("comment %d = %+v, want %+v", id, comments[id], w)
		}
	}
}

func TestEditsBadAnswers(t *testing.T) {
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
		{"no cursor", []string{page(true, "null", `"1"`)}, errNoCursor},
		{"same cursor", []string{page(true, `"c1"`, `"1"`)}, errStuckCursor},
		{"cursor back", []string{page(true, `"c1"`, `"1"`), page(true, `"c2"`, `"2"`), page(true, `"c1"`, `"3"`)}, errStuckCursor},
		{"bad ID", []string{page(false, "null", `"x1"`)}, strconv.ErrSyntax},
		{"null ID", []string{page(false, "null", `null`)}, strconv.ErrSyntax},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := graphQLAnswers(t, tc.answers...).edits(context.Background(), "o", "r", 7)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "edits of o/r#7") {
				t.Errorf("err = %v, want one wrapping %v with the issue", err, tc.want)
			}
		})
	}
}
