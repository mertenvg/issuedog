package githubtest

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// graphql answers internal/github's edits query from its variables alone,
// paging comments by 100. As on GitHub, a Bot's login has no "[bot]", a
// missing repo or issue is a NOT_FOUND error, and errors come with data.
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
	if v.Number < 1 || v.Number > len(f.issues) {
		notFound(fmt.Sprintf("Could not resolve to an Issue with the number of %d.", v.Number))
		return reply(map[string]any{"repository": map[string]any{"issue": nil}})
	}
	from := 0
	if v.Cursor != nil {
		n, err := fromCursor(*v.Cursor)
		if err != nil {
			errs = append(errs, map[string]any{"message": fmt.Sprintf("`%s` does not appear to be a valid cursor.", *v.Cursor)})
			return reply(nil)
		}
		from = n + 1
	}
	var list []*comment
	for _, cm := range f.comments {
		if cm.issue == v.Number {
			list = append(list, cm)
		}
	}
	to := min(from+100, len(list))
	from = min(from, to)
	nodes := []map[string]any{}
	for _, cm := range list[from:to] {
		n := cm.lastEdit.graphql()
		n["databaseId"] = cm.GetID()
		nodes = append(nodes, n)
	}
	var end any
	if to > from {
		end = cursor(to - 1)
	}
	is := f.issues[v.Number-1].lastEdit.graphql()
	is["comments"] = map[string]any{"pageInfo": map[string]any{"hasNextPage": to < len(list), "endCursor": end}, "nodes": nodes}
	return reply(map[string]any{"repository": map[string]any{"issue": is}})
}

// graphql returns e's lastEditedAt and editor fields.
func (e lastEdit) graphql() map[string]any {
	if e.at.IsZero() {
		return map[string]any{"lastEditedAt": nil, "editor": nil}
	}
	var editor any
	if e.by != "" {
		login, typ := e.by, "User"
		if l, ok := strings.CutSuffix(login, "[bot]"); ok {
			login, typ = l, "Bot"
		}
		editor = map[string]any{"login": login, "__typename": typ}
	}
	return map[string]any{"lastEditedAt": e.at, "editor": editor}
}

// cursor is the opaque cursor of the comment at index i.
func cursor(i int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(i)))
}

func fromCursor(s string) (int, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return 0, fmt.Errorf("decode cursor %q: %w", s, err)
	}
	rest, ok := strings.CutPrefix(string(b), "cursor:")
	if !ok {
		return 0, fmt.Errorf("cursor %q: no cursor: prefix", s)
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, fmt.Errorf("cursor %q: %w", s, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("cursor %q: negative", s)
	}
	return n, nil
}
