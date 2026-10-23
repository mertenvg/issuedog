package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
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

func TestHistoryBadAnswers(t *testing.T) {
	page := func(id string, next bool, cursor string) string {
		return `{"data":{"nodes":[{"id":"` + id + `","userContentEdits":{"pageInfo":{"hasNextPage":` + strconv.FormatBool(next) +
			`,"endCursor":` + cursor + `},"nodes":[{"editedAt":"2026-10-12T01:00:00Z","editor":{"login":"issuedog-dev","__typename":"Bot"}}]}}]}}`
	}
	for _, tc := range []struct {
		name    string
		answers []string
		want    error
	}{
		{"no cursor", []string{page("A", true, "null")}, errCursor},
		{"a cursor seen before", []string{page("A", true, `"c1"`), page("A", true, `"c2"`), page("A", true, `"c1"`)}, errCursor},
		{"other node", []string{page("B", false, "null")}, errNoNode},
		{"null node", []string{`{"data":{"nodes":[null]}}`}, errNoNode},
		{"no nodes", []string{`{"data":{"nodes":[]}}`}, errNoNode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := graphQLAnswers(t, tc.answers...).History(context.Background(), []string{"A"})
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), "edit history") {
				t.Errorf("err = %v, want one wrapping %v", err, tc.want)
			}
		})
	}
	// A NOT_FOUND error is a gone node only at ["nodes", the index] of a
	// null node, and only with no other error listed: else the read fails
	// as a whole, as before.
	notFound := `{"type":"NOT_FOUND","path":["nodes",%s],"message":"Could not resolve to a node with the global id of 'A'"}`
	for _, tc := range []struct{ name, answer string }{
		{"another error too", `{"data":{"nodes":[null]},"errors":[` + fmt.Sprintf(notFound, "0") + `,{"message":"Something went wrong"}]}`},
		{"another index", `{"data":{"nodes":[null]},"errors":[` + fmt.Sprintf(notFound, "1") + `]}`},
		{"a node there", `{"data":{"nodes":[{"id":"A"}]},"errors":[` + fmt.Sprintf(notFound, "0") + `]}`},
		{"no type", `{"data":{"nodes":[null]},"errors":[{"path":["nodes",0],"message":"x"}]}`},
		{"a path below the node", `{"data":{"nodes":[null]},"errors":[{"type":"NOT_FOUND","path":["nodes",0,"userContentEdits"],"message":"x"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := graphQLAnswers(t, tc.answer).History(context.Background(), []string{"A"})
			var g *HistoryGoneError
			if !errors.Is(err, ErrGraphQL) || errors.As(err, &g) {
				t.Errorf("err = %v; want GitHub's errors, and no node gone", err)
			}
		})
	}
	// A NOT_FOUND error at a node that's there, while another node is null,
	// names neither gone: the error's index must be the null node's.
	_, err := graphQLAnswers(t, `{"data":{"nodes":[null,{"id":"B"}]},"errors":[`+fmt.Sprintf(notFound, "1")+`]}`).
		History(context.Background(), []string{"A", "B"})
	if g := (*HistoryGoneError)(nil); !errors.Is(err, ErrGraphQL) || errors.As(err, &g) {
		t.Errorf("err = %v; want GitHub's errors, and no node gone, for an error at B, which is there, with A null", err)
	}
	_, err = graphQLAnswers(t, `{"data":{"nodes":[null]},"errors":[`+fmt.Sprintf(notFound, "0")+`]}`).History(context.Background(), []string{"A"})
	if g := (*HistoryGoneError)(nil); !errors.As(err, &g) || !slices.Equal(g.Nodes, []string{"A"}) || len(g.Read) != 0 {
		t.Errorf("err = %v; want A gone, and nothing else read", err)
	}
	// A null history is no entries, not an error: the daemon fails closed on it.
	h, err := graphQLAnswers(t, `{"data":{"nodes":[{"id":"A","userContentEdits":null}]}}`).History(context.Background(), []string{"A"})
	if err != nil || len(h["A"]) != 0 {
		t.Errorf("history = %+v, %v, want none and no error", h, err)
	}
}
