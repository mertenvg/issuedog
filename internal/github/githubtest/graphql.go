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

// answered is every field graphql answers, which a query must name.
const answered = `query { repository { issue { body lastEditedAt editor { login __typename }
	comments { pageInfo { hasNextPage endCursor } nodes { fullDatabaseId body createdAt updatedAt
	lastEditedAt author { login __typename } editor { login __typename } } } } } }`

var answeredFields = fields(answered)

// graphql answers internal/github's comments query for the fake's repo,
// paging comments by 100, and refuses a query that doesn't name every field
// in answered. As on GitHub, a Bot's login has no "[bot]", IDs are BigInt
// strings, a missing issue or a pull request is a NOT_FOUND error, and
// errors come with data.
func (f *Fake) graphql(c *call) (int, any) {
	var req struct {
		Query     string
		Variables struct {
			Number int
			Cursor *string
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
	named := fields(req.Query)
	for p := range answeredFields {
		if !named[p] {
			missing = append(missing, p)
		}
	}
	var extra []string // unknown fields and aliases, which GitHub would answer differently
	for p := range named {
		if !answeredFields[p] {
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
		n["fullDatabaseId"] = strconv.FormatInt(cm.GetID(), 10)
		n["author"], n["createdAt"], n["updatedAt"] = actor(cm.GetUser().GetLogin()), cm.GetCreatedAt(), cm.GetUpdatedAt()
		nodes = append(nodes, n)
	}
	var end any
	if to > from {
		end = strconv.FormatInt(list[to-1].GetID(), 10)
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
	queryTokens = regexp.MustCompile(`[{}]|[A-Za-z_]\w*`)
)

// fields returns the paths of the fields a query names, such as
// "query.repository.issue.body". It skips arguments and variables.
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
			last = tok
			if len(outer) > 0 {
				last = outer[len(outer)-1] + "." + tok
			}
			paths[last] = true
		}
	}
	return paths
}
