package githubtest

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
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
// write access can, and moves its issue's updated_at, as DeleteComment
// does. When login isn't the comment's author, it records a
// CommentDeletedEvent on its issue naming login and the comment's author,
// which the GraphQL comments query serves (gqlEventsPage), and returns the
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
	if login != "" && strings.EqualFold(login, c.GetUser().GetLogin()) {
		return ""
	}
	return f.deletedEventBy(login, c, at.Time)
}

// DeleteCommentByUnmoved deletes a comment as login, someone other than
// its author, as DeleteCommentBy does, but leaves its issue's updated_at
// alone, as SPEC §16 hasn't confirmed that another user's deletion moves
// it, and as an author's deletion leaves it for about 2 seconds. The
// event's time is the clock's next tick. As an author's deletion of their
// own comment is confirmed to move updated_at, one fails the test, as a
// comment the fake hasn't got does (fakeCommentID).
func (f *Fake) DeleteCommentByUnmoved(login string, id int64) string {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.fakeCommentID(id)
	if login != "" && strings.EqualFold(login, c.GetUser().GetLogin()) {
		f.t.Fatalf("githubtest: %s deleting their own comment %d moves updated_at: use DeleteCommentBy", login, id)
	}
	f.comments = slices.DeleteFunc(f.comments, func(o *comment) bool { return o == c })
	return f.deletedEventBy(login, c, f.tick().Time)
}

// deletedEventBy records a CommentDeletedEvent at at, of c by login, on
// c's issue, and returns its node ID. f is locked.
func (f *Fake) deletedEventBy(login string, c *comment, at time.Time) string {
	f.lastEvent++
	e := gqlEvent{node: fmt.Sprintf("CDE_%d", firstEvent+f.lastEvent), by: login, author: c.GetUser().GetLogin(), at: at}
	is := f.issueNumbered(c.issue)
	is.gqlEvents = append(is.gqlEvents, e)
	return e.node
}

// gqlEventsPage answers the comments query's timelineItems: the issue's
// CommentDeletedEvents and SubIssueRemovedEvents written by now, oldest
// first, but for one not written yet (LagEvents), 100 per page after
// cursor, which is the number of older events, each with its __typename.
// As on GitHub, a Bot's login has no "[bot]". A bad cursor is an error
// message.
func gqlEventsPage(is *issue, now time.Time, cursor *string) (map[string]any, string) {
	events := slices.DeleteFunc(slices.Clone(is.gqlEvents), func(e gqlEvent) bool { return e.at.After(now) })
	from := 0
	if cursor != nil {
		n, err := strconv.Atoi(*cursor)
		if err != nil || n < 0 || n > len(events) {
			return nil, fmt.Sprintf("`%s` does not appear to be a valid cursor.", *cursor)
		}
		from = n
	}
	to := min(from+100, len(events))
	nodes := []map[string]any{}
	for _, e := range events[from:to] {
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
	return map[string]any{"pageInfo": map[string]any{"hasNextPage": to < len(events), "endCursor": end}, "nodes": nodes}, ""
}
