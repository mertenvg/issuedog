package githubtest

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// BeforeGraphQL runs do, once, just before the fake answers the next GraphQL
// query, as when the issue changes between Snapshot's REST and GraphQL reads.
// do may call the fake's other methods.
func (f *Fake) BeforeGraphQL(do func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beforeGQL = do
}

// serveGraphQL runs the BeforeGraphQL hook, then serves the query as a read.
func (f *Fake) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	do := f.beforeGQL
	f.beforeGQL = nil
	f.mu.Unlock()
	if do != nil {
		do()
	}
	f.serve(w, r, false, f.graphql)
}

// graphql answers internal/github's edits query from its variables alone,
// paging comments by 100. As on GitHub, a Bot's login has no "[bot]", IDs
// are BigInt strings, a missing repo or issue or a pull request is a
// NOT_FOUND error, and errors come with data.
func (f *Fake) graphql(c *call) (int, any) {
	var req struct {
		Variables struct {
			Owner, Name string
			Number      int
			Cursor      *string
		}
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	v := req.Variables
	var errs []map[string]any
	for _, m := range f.gqlErrors {
		errs = append(errs, map[string]any{"message": m})
	}
	f.gqlErrors = nil
	reply := func(data any) (int, any) {
		out := map[string]any{"data": data}
		if len(errs) > 0 {
			out["errors"] = errs
		}
		return http.StatusOK, out
	}
	notFound := func(m string) { errs = append(errs, map[string]any{"type": "NOT_FOUND", "message": m}) }
	if !strings.EqualFold(v.Owner+"/"+v.Name, f.repo) {
		notFound(fmt.Sprintf("Could not resolve to a Repository with the name '%s/%s'.", v.Owner, v.Name))
		return reply(map[string]any{"repository": nil})
	}
	if v.Number < 1 || v.Number > len(f.issues) || f.issues[v.Number-1].IsPullRequest() {
		notFound(fmt.Sprintf("Could not resolve to an Issue with the number of %d.", v.Number))
		return reply(map[string]any{"repository": map[string]any{"issue": nil}})
	}
	var list []*comment
	for _, cm := range f.comments {
		if cm.issue == v.Number {
			list = append(list, cm)
		}
	}
	from := 0
	if v.Cursor != nil {
		after, err := fromCursor(*v.Cursor)
		if err != nil {
			errs = append(errs, map[string]any{"message": fmt.Sprintf("`%s` does not appear to be a valid cursor.", *v.Cursor)})
			return reply(nil)
		}
		// Like GitHub's, the cursor survives a deleted comment.
		if from = slices.IndexFunc(list, func(cm *comment) bool { return cm.GetID() > after }); from < 0 {
			from = len(list)
		}
	}
	to := min(from+100, len(list))
	nodes := []map[string]any{}
	for _, cm := range list[from:to] {
		n := cm.lastEdit.graphql(cm.GetBody())
		n["fullDatabaseId"] = strconv.FormatInt(cm.GetID(), 10)
		nodes = append(nodes, n)
	}
	var end any
	if to > from {
		end = cursor(list[to-1].GetID())
	}
	is := f.issues[v.Number-1]
	issue := is.lastEdit.graphql(is.GetBody())
	issue["comments"] = map[string]any{"pageInfo": map[string]any{"hasNextPage": to < len(list), "endCursor": end}, "nodes": nodes}
	return reply(map[string]any{"repository": map[string]any{"issue": issue}})
}

// graphql returns the body, lastEditedAt and editor fields of a body last edited as e.
func (e lastEdit) graphql(body string) map[string]any {
	if e.at.IsZero() {
		return map[string]any{"body": body, "lastEditedAt": nil, "editor": nil}
	}
	var editor any
	if e.by != "" {
		login, typ := e.by, "User"
		if l, ok := strings.CutSuffix(login, "[bot]"); ok {
			login, typ = l, "Bot"
		}
		editor = map[string]any{"login": login, "__typename": typ}
	}
	return map[string]any{"body": body, "lastEditedAt": e.at, "editor": editor}
}

// cursor is the opaque cursor after the comment with this ID.
func cursor(id int64) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.FormatInt(id, 10)))
}

func fromCursor(s string) (int64, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("decode cursor %q: %w", s, err)
	}
	rest, ok := strings.CutPrefix(string(b), "cursor:")
	if !ok {
		return 0, fmt.Errorf("cursor %q: no cursor: prefix", s)
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cursor %q: %w", s, err)
	}
	return id, nil
}
