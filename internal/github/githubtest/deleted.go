package githubtest

import (
	"fmt"
	"slices"
	"strconv"
	"time"
)

// gqlEvent is one CommentDeletedEvent or SubIssueRemovedEvent on an
// issue's timeline, as the GraphQL comments query serves them
// (gqlEventsPage).
type gqlEvent struct {
	node string // its GraphQL node ID
	// by is who deleted the comment, or removed the sub-issue's link, and
	// author who wrote the deleted comment.
	by, author string
	at         time.Time
	// sub is the sub-issue whose link a SubIssueRemovedEvent removed, nil
	// for a CommentDeletedEvent.
	sub *linked
}

// DeleteCommentBy deletes a comment as login, as its author or someone with
// write access can, and records a CommentDeletedEvent on its issue naming
// login and the comment's author, which the GraphQL comments query serves
// (gqlEventsPage). It returns the event's node ID, the ID internal/github
// gives it. An empty login is an event that names no actor. DeleteComment
// records no event. A comment the fake hasn't got, one deleted already
// included, fails the test (fakeCommentID).
func (f *Fake) DeleteCommentBy(login string, id int64) string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.fakeCommentID(id)
	f.comments = slices.DeleteFunc(f.comments, func(c *comment) bool { return c.GetID() == id })
	f.lastEvent++
	e := gqlEvent{node: fmt.Sprintf("CDE_%d", firstEvent+f.lastEvent), by: login, author: c.GetUser().GetLogin(), at: f.tick().Time}
	is := f.issues[c.issue-1]
	is.gqlEvents = append(is.gqlEvents, e)
	return e.node
}

// gqlEventsPage answers the comments query's timelineItems: the issue's
// CommentDeletedEvents and SubIssueRemovedEvents, oldest first, 100 per
// page after cursor, which is the number of older events, each with its
// __typename. As on GitHub, a Bot's login has no "[bot]". A bad cursor is
// an error message.
func gqlEventsPage(is *issue, cursor *string) (map[string]any, string) {
	from := 0
	if cursor != nil {
		n, err := strconv.Atoi(*cursor)
		if err != nil || n < 0 || n > len(is.gqlEvents) {
			return nil, fmt.Sprintf("`%s` does not appear to be a valid cursor.", *cursor)
		}
		from = n
	}
	to := min(from+100, len(is.gqlEvents))
	nodes := []map[string]any{}
	for _, e := range is.gqlEvents[from:to] {
		node := map[string]any{"__typename": "CommentDeletedEvent", "id": e.node, "createdAt": e.at, "actor": actor(e.by)}
		if e.sub == nil {
			node["deletedCommentAuthor"] = actor(e.author)
		} else {
			node["__typename"] = "SubIssueRemovedEvent"
			node["subIssue"] = map[string]any{"number": e.sub.number, "repository": map[string]any{"nameWithOwner": e.sub.repo}}
		}
		nodes = append(nodes, node)
	}
	var end any
	if to > from {
		end = strconv.Itoa(to)
	}
	return map[string]any{"pageInfo": map[string]any{"hasNextPage": to < len(is.gqlEvents), "endCursor": end}, "nodes": nodes}, ""
}
