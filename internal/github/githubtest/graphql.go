package githubtest

import (
	"fmt"
	"net/http"
	"regexp"
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

// answered is every field graphql answers for the comments query, which a
// query must name. answeredHistory is the same for the history query.
const (
	answered = `query { repository { issue { id body lastEditedAt editor { login __typename }
	comments { pageInfo { hasNextPage endCursor } nodes { id fullDatabaseId body createdAt updatedAt
	lastEditedAt author { login __typename } editor { login __typename } } } } } }`
	answeredHistory = `query { nodes { id ... on Comment { userContentEdits {
	pageInfo { hasNextPage endCursor } nodes { editedAt editor { login __typename } } } } } }`
)

var answeredFields, historyFields = fields(answered), fields(answeredHistory)

// HistoryQueries counts the edit history queries answered.
func (f *Fake) HistoryQueries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.histories
}

// graphql answers internal/github's comments query for the fake's repo,
// paging comments by 100, and its history query (see history). It refuses
// a query that doesn't name every field in answered, or answeredHistory
// for a query of nodes. As on GitHub, a Bot's login has no "[bot]", IDs are
// BigInt strings, a missing issue or a pull request is a NOT_FOUND error,
// and errors come with data.
func (f *Fake) graphql(c *call) (int, any) {
	var req struct {
		Query     string
		Variables struct {
			Number int
			Cursor *string
			IDs    []string
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
	var missing []string
	named, want := fields(req.Query), answeredFields
	if named["query.nodes"] {
		want = historyFields
	}
	for p := range want {
		if !named[p] {
			missing = append(missing, p)
		}
	}
	var extra []string // unknown fields and aliases, which GitHub would answer differently
	for p := range named {
		if !want[p] {
			extra = append(extra, p)
		}
	}
	if len(missing) > 0 || len(extra) > 0 {
		slices.Sort(missing)
		slices.Sort(extra)
		var msg []string
		if len(missing) > 0 {
			msg = append(msg, "doesn't name "+strings.Join(missing, ", "))
		}
		if len(extra) > 0 {
			msg = append(msg, "names unknown "+strings.Join(extra, ", "))
		}
		errs = append(errs, map[string]any{"message": "githubtest: the query " + strings.Join(msg, " and ")})
		return reply(nil)
	}
	if named["query.nodes"] {
		f.histories++
		nodes, err := f.history(v.IDs, v.Cursor)
		if err != "" {
			errs = append(errs, map[string]any{"message": err})
			return reply(nil)
		}
		return reply(map[string]any{"nodes": nodes})
	}
	if v.Number < 1 || v.Number > len(f.issues) || f.issues[v.Number-1].IsPullRequest() {
		errs = append(errs, map[string]any{"type": "NOT_FOUND", "message": fmt.Sprintf("Could not resolve to an Issue with the number of %d.", v.Number)})
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
		// The cursor is the last comment's ID, so it survives that comment's deletion.
		after, err := strconv.ParseInt(*v.Cursor, 10, 64)
		if err != nil {
			errs = append(errs, map[string]any{"message": fmt.Sprintf("`%s` does not appear to be a valid cursor.", *v.Cursor)})
			return reply(nil)
		}
		if from = slices.IndexFunc(list, func(cm *comment) bool { return cm.GetID() > after }); from < 0 {
			from = len(list)
		}
	}
	to := min(from+100, len(list))
	nodes := []map[string]any{}
	for _, cm := range list[from:to] {
		n := cm.lastEdit.graphql(cm.GetBody())
		n["id"], n["fullDatabaseId"] = commentNode(cm.GetID()), strconv.FormatInt(cm.GetID(), 10)
		n["author"], n["createdAt"], n["updatedAt"] = actor(cm.GetUser().GetLogin()), cm.GetCreatedAt(), cm.GetUpdatedAt()
		nodes = append(nodes, n)
	}
	var end any
	if to > from {
		end = strconv.FormatInt(list[to-1].GetID(), 10)
	}
	is := f.issues[v.Number-1]
	issue := is.lastEdit.graphql(is.GetBody())
	issue["id"] = issueNode(is.GetNumber())
	issue["comments"] = map[string]any{"pageInfo": map[string]any{"hasNextPage": to < len(list), "endCursor": end}, "nodes": nodes}
	return reply(map[string]any{"repository": map[string]any{"issue": issue}})
}

// issueNode and commentNode are an issue's and a comment's node IDs.
func issueNode(number int) string { return fmt.Sprintf("I_%d", number) }
func commentNode(id int64) string { return fmt.Sprintf("IC_%d", id) }

// history answers the history query: each node's edits, newest first, 100
// per page. Unlike GitHub, it keeps every edit, so paging past 100 can be
// tested. A cursor is the number of older edits, so it survives new ones.
// An unknown ID is an error.
func (f *Fake) history(ids []string, cursor *string) ([]any, string) {
	histories := map[string][]lastEdit{}
	for _, is := range f.issues {
		histories[issueNode(is.GetNumber())] = is.history
	}
	for _, c := range f.comments {
		histories[commentNode(c.GetID())] = c.history
	}
	nodes := []any{}
	for _, id := range ids {
		h, ok := histories[id]
		if !ok {
			return nil, fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id)
		}
		older := len(h)
		if cursor != nil {
			n, err := strconv.Atoi(*cursor)
			if err != nil || n < 0 || n > len(h) {
				return nil, fmt.Sprintf("`%s` does not appear to be a valid cursor.", *cursor)
			}
			older = n
		}
		end, page := max(older-100, 0), []any{}
		for i := older - 1; i >= end; i-- {
			page = append(page, map[string]any{"editedAt": h[i].at, "editor": actor(h[i].by)})
		}
		var endCursor any
		if len(page) > 0 {
			endCursor = strconv.Itoa(end)
		}
		nodes = append(nodes, map[string]any{"id": id, "userContentEdits": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end > 0, "endCursor": endCursor}, "nodes": page}})
	}
	return nodes, ""
}

// graphql returns the body, lastEditedAt and editor fields of a body last edited as e.
func (e lastEdit) graphql(body string) map[string]any {
	if e.at.IsZero() {
		return map[string]any{"body": body, "lastEditedAt": nil, "editor": nil}
	}
	return map[string]any{"body": body, "lastEditedAt": e.at, "editor": actor(e.by)}
}

// actor is login as a GraphQL Actor, or nil for "", a deleted account.
func actor(login string) any {
	if login == "" {
		return nil
	}
	if l, ok := strings.CutSuffix(login, "[bot]"); ok {
		return map[string]any{"login": l, "__typename": "Bot"}
	}
	return map[string]any{"login": login, "__typename": "User"}
}

var (
	queryArgs   = regexp.MustCompile(`\([^)]*\)`)
	queryTokens = regexp.MustCompile(`\.\.\.\s*on\s+\w+|[{}]|[A-Za-z_]\w*`)
)

// fields returns the paths of the fields a query names, such as
// "query.repository.issue.body". It skips arguments and variables. An
// inline fragment is a step of the path, such as "query.nodes.... on Comment".
func fields(query string) map[string]bool {
	paths := map[string]bool{}
	var outer []string // the enclosing fields' paths
	last := ""
	for _, tok := range queryTokens.FindAllString(queryArgs.ReplaceAllString(query, " "), -1) {
		switch tok {
		case "{":
			outer = append(outer, last)
		case "}":
			outer = outer[:max(len(outer)-1, 0)]
		default:
			last = strings.Join(strings.Fields(tok), " ")
			if len(outer) > 0 {
				last = outer[len(outer)-1] + "." + last
			}
			paths[last] = true
		}
	}
	return paths
}
