package githubtest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
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

// serveGraphQL runs the BeforeGraphQL hook, then serves a query as a read
// and a mutation as a write, which is logged and which FailWrite can fail.
func (f *Fake) serveGraphQL(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	do := f.beforeGQL
	f.beforeGQL = nil
	f.mu.Unlock()
	if do != nil {
		do()
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		send(w, http.StatusBadRequest, message(err.Error()))
		return
	}
	var req struct{ Query string }
	if err := json.Unmarshal(body, &req); err != nil {
		send(w, http.StatusUnprocessableEntity, message(err.Error()))
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	f.serve(w, r, fields(req.Query)["mutation"], f.graphql)
}

// answered is every field graphql answers for the comments query, which a
// query must name, with the one argument it checks (deletedFilter), so it
// can be sent as a query itself. answeredHistory, answeredPinned and answeredPin are the
// same for the history query, the isPinned query and the pinIssue mutation.
const (
	answered = `query { repository { issue { id body lastEditedAt editor { login __typename }
	comments { pageInfo { hasNextPage endCursor } nodes { id fullDatabaseId body createdAt updatedAt authorAssociation
	lastEditedAt author { login __typename } editor { login __typename } } }
	timelineItems(itemTypes: [COMMENT_DELETED_EVENT], first: 100) { pageInfo { hasNextPage endCursor } nodes { ... on CommentDeletedEvent {
	id createdAt actor { login __typename } deletedCommentAuthor { login __typename } } } } } } }`
	answeredHistory = `query { nodes { id ... on Comment { userContentEdits {
	pageInfo { hasNextPage endCursor } nodes { editedAt editor { login __typename } } } }
	... on Issue { userContentEdits { nodes { diff } } } } }`
	answeredPinned = `query { repository { issue { isPinned } } }`
	answeredPin    = `mutation { pinIssue { issue { isPinned } } }`
	answeredCloser = `query { repository { nameWithOwner defaultBranchRef { name } } node { ... on ClosedEvent { stateReason
	closer { __typename ... on PullRequest { merged baseRefName baseRepository { nameWithOwner } }
	... on Commit { repository { nameWithOwner } } } } } }`
)

var (
	answeredFields, historyFields = fields(answered), fields(answeredHistory)
	pinnedFields, pinFields       = fields(answeredPinned), fields(answeredPin)
	closerFields                  = fields(answeredCloser)
)

// HistoryQueries counts the edit history queries answered.
func (f *Fake) HistoryQueries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.histories
}

// graphql answers internal/github's comments query for the fake's repo,
// paging comments by 100 and, as its timelineItems, CommentDeletedEvents
// by 100 (deletedPage), its history query (see history), its isPinned
// query and its pinIssue mutation (see pinIssue), each of which answers
// an HTTP status alone under FailIsPinned or FailPinStatus. It refuses a query that
// doesn't name every field in answered, or answeredHistory for a query of
// nodes, answeredPinned for one of isPinned, or answeredPin for a mutation.
// As on GitHub, a Bot's login has no "[bot]", IDs are BigInt strings, a
// missing issue or a pull request is a NOT_FOUND error, and errors come
// with data.
func (f *Fake) graphql(c *call) (int, any) {
	var req struct {
		Query     string
		Variables struct {
			Number        int
			Cursor        *string
			DeletedCursor *string // the comments query's timelineItems
			IDs           []string
			ID            string // pinIssue's issue
			Owner, Name   string // the closer query's repo
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
	errs, f.closingErrs = append(errs, f.closingErrs...), nil
	reply := func(data any) (int, any) {
		out := map[string]any{"data": data}
		if len(errs) > 0 {
			out["errors"] = errs
		}
		return http.StatusOK, out
	}
	var missing []string
	named, want := fields(req.Query), answeredFields
	switch {
	case named["mutation"]:
		want = pinFields
	case named["query.nodes"]:
		want = historyFields
	case named["query.repository.issue.isPinned"]:
		want = pinnedFields
	case named["query.node"]:
		want = closerFields
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
		nodes, notFound, err := f.history(v.IDs, v.Cursor)
		if err != "" {
			errs = append(errs, map[string]any{"message": err})
			return reply(nil)
		}
		errs = append(errs, notFound...)
		return reply(map[string]any{"nodes": nodes})
	}
	if named["query.node"] {
		data, notFound := f.closingAnswer(v.Owner, v.Name, v.ID)
		errs = append(errs, notFound...)
		return reply(data)
	}
	if named["mutation"] && f.pinStatus != 0 {
		return f.pinStatus, message(http.StatusText(f.pinStatus))
	}
	if named["query.repository.issue.isPinned"] && f.pinnedFail != 0 {
		return f.pinnedFail, message(http.StatusText(f.pinnedFail))
	}
	if named["mutation"] {
		data, failed := f.pinIssue(req.Variables.ID)
		errs = append(errs, failed...)
		return reply(data)
	}
	// A transferred issue isn't this repo's to answer for (transfer.go).
	if v.Number < 1 || v.Number > len(f.issues) || f.issues[v.Number-1].IsPullRequest() || !f.issues[v.Number-1].listed() {
		errs = append(errs, map[string]any{"type": "NOT_FOUND", "message": fmt.Sprintf("Could not resolve to an Issue with the number of %d.", v.Number)})
		return reply(map[string]any{"repository": map[string]any{"issue": nil}})
	}
	if named["query.repository.issue.isPinned"] {
		return reply(map[string]any{"repository": map[string]any{"issue": map[string]any{"isPinned": f.issues[v.Number-1].pinned}}})
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
		n["authorAssociation"] = f.discAssociation(cm.GetUser().GetLogin())
		nodes = append(nodes, n)
	}
	var end any
	if to > from {
		end = strconv.FormatInt(list[to-1].GetID(), 10)
	}
	is := f.issues[v.Number-1]
	if !deletedFilter.MatchString(req.Query) {
		errs = append(errs, map[string]any{"message": "githubtest: the query's timelineItems doesn't ask for COMMENT_DELETED_EVENT alone"})
		return reply(nil)
	}
	deleted, msg := deletedPage(is, v.DeletedCursor)
	if msg != "" {
		errs = append(errs, map[string]any{"message": msg})
		return reply(nil)
	}
	issue := is.lastEdit.graphql(is.GetBody())
	issue["id"] = issueNode(is.GetNumber())
	issue["comments"] = map[string]any{"pageInfo": map[string]any{"hasNextPage": to < len(list), "endCursor": end}, "nodes": nodes}
	issue["timelineItems"] = deleted
	return reply(map[string]any{"repository": map[string]any{"issue": issue}})
}

// issueNode and commentNode are an issue's and a comment's node IDs.
func issueNode(number int) string { return fmt.Sprintf("I_%d", number) }
func commentNode(id int64) string { return fmt.Sprintf("IC_%d", id) }

// HistoryCap sets how many entries a history lists: the original post and
// the n-1 newest edits. It is 100 from New, as on GitHub, and n < 1 lists
// every edit, so paging past 100 can be tested.
func (f *Fake) HistoryCap(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyCap = n
}

// history answers the history query: each node's edits, newest first, 100
// per page, capped as HistoryCap says, with an issue body's text after each
// edit as its diff. A cursor is the number of older
// entries. As on GitHub, an unknown ID's node is null, with a NOT_FOUND
// error whose path names it, as is a transferred issue's node and its
// comments' (transfer.go). A bad cursor is the only other error.
func (f *Fake) history(ids []string, cursor *string) ([]any, []map[string]any, string) {
	histories, bodies := map[string][]lastEdit{}, map[string]bool{}
	for _, is := range f.issues {
		if is.listed() {
			histories[issueNode(is.GetNumber())], bodies[issueNode(is.GetNumber())] = is.history, true
		}
	}
	for _, c := range f.comments {
		if f.issues[c.issue-1].listed() {
			histories[commentNode(c.GetID())] = c.history
		}
	}
	nodes, errs := []any{}, []map[string]any{}
	for i, id := range ids {
		h, ok := histories[id]
		if !ok {
			nodes = append(nodes, nil)
			errs = append(errs, map[string]any{"type": "NOT_FOUND", "path": []any{"nodes", i},
				"message": fmt.Sprintf("Could not resolve to a node with the global id of '%s'", id)})
			continue
		}
		if f.historyCap > 0 && len(h) > f.historyCap {
			h = append(h[:1:1], h[len(h)-f.historyCap+1:]...)
		}
		older := len(h)
		if cursor != nil {
			n, err := strconv.Atoi(*cursor)
			if err != nil || n < 0 || n > len(h) {
				return nil, nil, fmt.Sprintf("`%s` does not appear to be a valid cursor.", *cursor)
			}
			older = n
		}
		end, page := max(older-100, 0), []any{}
		for i := older - 1; i >= end; i-- {
			e := map[string]any{"editedAt": h[i].at, "editor": actor(h[i].by)}
			if bodies[id] { // an Issue: a comment has no diff in the query
				e["diff"] = nil
				if h[i].text != "" {
					e["diff"] = h[i].text
				}
			}
			page = append(page, e)
		}
		var endCursor any
		if len(page) > 0 {
			endCursor = strconv.Itoa(end)
		}
		nodes = append(nodes, map[string]any{"id": id, "userContentEdits": map[string]any{
			"pageInfo": map[string]any{"hasNextPage": end > 0, "endCursor": endCursor}, "nodes": page}})
	}
	return nodes, errs, ""
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
	// deletedFilter is the argument the comments query's timelineItems
	// needs, which fields skips: the fake serves only the deletions.
	deletedFilter = regexp.MustCompile(`timelineItems\(itemTypes:\s*\[COMMENT_DELETED_EVENT\],`)
	queryArgs     = regexp.MustCompile(`\([^)]*\)`)
	queryTokens   = regexp.MustCompile(`\.\.\.\s*on\s+\w+|[{}]|[A-Za-z_]\w*`)
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
