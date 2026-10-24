package githubtest

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// deletedEvent is one CommentDeletedEvent on an issue's timeline.
type deletedEvent struct {
	node       string // its GraphQL node ID
	by, author string // who deleted the comment, and who wrote it
	at         time.Time
}

// DeleteCommentBy deletes a comment as login, as its author or someone with
// write access can, and moves its issue's updated_at, as DeleteComment
// does. When login isn't the comment's author, it records a
// CommentDeletedEvent on its issue naming login and the comment's author,
// which the GraphQL comments query serves (deletedPage), and returns the
// event's node ID, the ID internal/github gives it. An empty login is an
// event that names no actor. As on GitHub, an author's deletion of their
// own comment records no event (SPEC §16), so it returns "". A comment the
// fake hasn't got, one deleted already included, fails the test
// (fakeCommentID).
func (f *Fake) DeleteCommentBy(login string, id int64) string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.fakeCommentID(id)
	at := f.deleteComment(c)
	author := c.GetUser().GetLogin()
	if login != "" && strings.EqualFold(login, author) {
		return ""
	}
	f.lastEvent++
	e := deletedEvent{node: fmt.Sprintf("CDE_%d", firstEvent+f.lastEvent), by: login, author: author, at: at.Time}
	is := f.issues[c.issue-1]
	is.deletions = append(is.deletions, e)
	return e.node
}

// deletedPage answers the comments query's timelineItems: the issue's
// CommentDeletedEvents, oldest first, 100 per page after cursor, which is
// the number of older events. As on GitHub, a Bot's login has no "[bot]".
// A bad cursor is an error message.
func deletedPage(is *issue, cursor *string) (map[string]any, string) {
	from := 0
	if cursor != nil {
		n, err := strconv.Atoi(*cursor)
		if err != nil || n < 0 || n > len(is.deletions) {
			return nil, fmt.Sprintf("`%s` does not appear to be a valid cursor.", *cursor)
		}
		from = n
	}
	to := min(from+100, len(is.deletions))
	nodes := []map[string]any{}
	for _, e := range is.deletions[from:to] {
		nodes = append(nodes, map[string]any{"id": e.node, "createdAt": e.at, "actor": actor(e.by), "deletedCommentAuthor": actor(e.author)})
	}
	var end any
	if to > from {
		end = strconv.Itoa(to)
	}
	return map[string]any{"pageInfo": map[string]any{"hasNextPage": to < len(is.deletions), "endCursor": end}, "nodes": nodes}, ""
}
