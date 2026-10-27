package github

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Edit is who last edited a body, and when. The zero Edit means never edited.
// By is empty if GitHub names no editor; a deleted account shows as ghost.
// In a History, an Edit is one edit.
type Edit struct {
	By Author
	At time.Time
	// Text is the whole text after the edit, only in the History of an
	// issue body: GitHub's userContentEdits diff, which is that text rather
	// than a diff (live, 2026-10-15). It is empty elsewhere, and where
	// someone deleted the revision.
	Text string
}

// commentsQuery reads an issue's body, a page of its comments, each with
// its last edit, and a page of its CommentDeletedEvents and
// SubIssueRemovedEvents, oldest first. REST says when a body was edited,
// but not who, and lists no deletions. The deletions come in the same
// query (#186), so they cost no request of their own, and so do the
// removals of its sub-issues' links, for an epic's close (SPEC §7.3):
// GitHub's GraphQL docs give SubIssueRemovedEvent an actor and a
// subIssue, where its REST docs list no sub_issue_removed event (SPEC
// §16). An event's databaseId isn't read: GitHub's schema makes it a 32-bit Int,
// and timeline event IDs are past 2^31 (33402593746 on #186, read live
// 2026-10-16), so the node ID is the event's stable ID.
const commentsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String, $deletedCursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      id
      body
      lastEditedAt
      editor { login __typename }
      comments(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id fullDatabaseId body createdAt updatedAt lastEditedAt authorAssociation
          author { login __typename }
          editor { login __typename }
        }
      }
      timelineItems(itemTypes: [COMMENT_DELETED_EVENT, SUB_ISSUE_REMOVED_EVENT], first: 100, after: $deletedCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          __typename
          ... on CommentDeletedEvent {
            id createdAt
            actor { login __typename }
            deletedCommentAuthor { login __typename }
          }
          ... on SubIssueRemovedEvent {
            id createdAt
            actor { login __typename }
            subIssue { number repository { nameWithOwner } }
          }
        }
      }
    }
  }
}`

// DeletedComment is one CommentDeletedEvent on an issue's timeline:
// someone deleted a comment on it (SPEC §6.4). GitHub says who deleted it,
// who wrote it and when, but not which comment it was. An Author is empty
// where GitHub names nobody; a deleted account shows as ghost.
type DeletedComment struct {
	// ID is the event's GraphQL node ID, the same on every read, so it can
	// key what the deletion leads to.
	ID string
	// By is who deleted the comment: the event's actor.
	By Author
	// Author is who wrote the deleted comment.
	Author Author
	// At is when the comment was deleted.
	At time.Time
}

// SubIssueRemoval is one SubIssueRemovedEvent on a parent's timeline:
// who removed the link of which sub-issue below it, and when (SPEC §7.3).
// By is empty where GitHub names nobody; a deleted account shows as ghost.
type SubIssueRemoval struct {
	// ID is the event's GraphQL node ID, as DeletedComment's.
	ID string
	// Repo is the sub-issue's repository, owner/name, and Number its
	// number there.
	Repo   string
	Number int
	By     Author
	At     time.Time
}

// commentsResponse is commentsQuery's answer.
type commentsResponse struct {
	Data struct {
		Repository *struct {
			Issue *struct {
				editNode
				Comments struct {
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
					Nodes []struct {
						FullDatabaseID       string `json:"fullDatabaseId"` // a BigInt
						AuthorAssociation    string
						Author               *actor
						CreatedAt, UpdatedAt time.Time
						editNode
					}
				}
				TimelineItems struct { // the CommentDeletedEvents and SubIssueRemovedEvents
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
					Nodes []struct {
						Typename             string `json:"__typename"`
						ID                   string
						CreatedAt            time.Time
						Actor                *actor
						DeletedCommentAuthor *actor
						SubIssue             *struct { // null for an issue GitHub doesn't show the App
							Number     int
							Repository struct{ NameWithOwner string }
						}
					}
				}
			}
		}
	}
	gqlErrors
}

// gqlErrors is the errors list of a GraphQL answer.
type gqlErrors struct {
	Errors []gqlError
}

// gqlError is one error of a GraphQL answer: its message, and the type
// and path GitHub may give it, which History reads for a node it can't
// resolve (historyNotFound).
type gqlError struct {
	Type, Message string
	Path          []any
}

// err returns the errors as one wrapping ErrGraphQL, or nil if there are none.
func (e gqlErrors) err() error {
	if len(e.Errors) == 0 {
		return nil
	}
	msgs := make([]string, 0, len(e.Errors))
	for _, m := range e.Errors {
		msgs = append(msgs, m.Message)
	}
	return fmt.Errorf("%w: %s", ErrGraphQL, strings.Join(msgs, "; "))
}

// editNode is the body and edit fields of an issue or comment.
type editNode struct {
	ID           string // the node ID, for History
	Body         string
	LastEditedAt *time.Time
	Editor       *actor
}

// actor is a GraphQL Actor, nil if GitHub names none (a deleted account
// shows as ghost).
type actor struct {
	Login    string
	Typename string `json:"__typename"`
}

// author returns a as REST shows it: GraphQL gives a Bot's login without the
// "[bot]", so it is added back.
func (a *actor) author() Author {
	if a == nil {
		return Author{}
	}
	if a.Typename == "Bot" && !strings.HasSuffix(a.Login, "[bot]") {
		return Author{Login: a.Login + "[bot]", Type: a.Typename}
	}
	return Author{Login: a.Login, Type: a.Typename}
}

func (n editNode) edit() Edit {
	if n.LastEditedAt == nil {
		return Edit{}
	}
	return Edit{By: n.Editor.author(), At: *n.LastEditedAt}
}

// readComments sets s's body and comments, oldest first, each with its last
// edit, its deleted comments (s.Deleted) and the links of its sub-issues
// removed (s.SubIssueRemovals), each oldest first, from one GraphQL query
// per 100 comments or such events, whichever has more pages. A
// body and its edit come from the same read, so they match. Once one list
// has no next page, the next query asks for it after its end cursor, which
// gives only what was added since, so nothing is read twice.
func (c *Client) readComments(ctx context.Context, owner, repo string, s *Snapshot) error {
	wrap := func(err error) error { return fmt.Errorf("comments of %s/%s#%d: %w", owner, repo, s.Number, err) }
	read := map[string]bool{} // the cursors read, by variable and cursor
	vars := map[string]any{"owner": owner, "name": repo, "number": s.Number, "cursor": nil, "deletedCursor": nil}
	for page := 0; ; page++ {
		req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": commentsQuery, "variables": vars})
		if err != nil {
			return wrap(err)
		}
		var out commentsResponse
		if _, err := c.Do(req, &out); err != nil {
			return wrap(err)
		}
		if err := out.err(); err != nil {
			return wrap(err)
		}
		if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
			return wrap(errNoIssue)
		}
		is := out.Data.Repository.Issue
		if page == 0 {
			s.Body, s.BodyEdit, s.BodyNode = is.Body, is.edit(), is.ID
		}
		for _, n := range is.Comments.Nodes {
			id, err := strconv.ParseInt(n.FullDatabaseID, 10, 64)
			if err != nil {
				return wrap(fmt.Errorf("comment ID: %w", err))
			}
			s.Comments = append(s.Comments, Comment{ID: id, Node: n.ID, Author: n.Author.author(), Body: n.Body,
				CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, Edit: n.edit(), Association: n.AuthorAssociation})
		}
		for _, n := range is.TimelineItems.Nodes {
			// The query asks only for CommentDeletedEvents and
			// SubIssueRemovedEvents, so a node without an ID, or of another
			// type, is neither: fail rather than miss one. A removal that
			// names no sub-issue names none the daemon holds a snapshot of.
			switch {
			case n.ID == "":
				return wrap(errEventNoID)
			case n.Typename == "CommentDeletedEvent":
				s.Deleted = append(s.Deleted, DeletedComment{ID: n.ID, By: n.Actor.author(),
					Author: n.DeletedCommentAuthor.author(), At: n.CreatedAt})
			case n.Typename != "SubIssueRemovedEvent":
				return wrap(fmt.Errorf("%w: %q", errEventType, n.Typename))
			case n.SubIssue != nil:
				s.SubIssueRemovals = append(s.SubIssueRemovals, SubIssueRemoval{ID: n.ID, Repo: n.SubIssue.Repository.NameWithOwner,
					Number: n.SubIssue.Number, By: n.Actor.author(), At: n.CreatedAt})
			}
		}
		more := false
		for _, p := range []struct {
			name   string
			next   bool
			cursor string
		}{
			{"cursor", is.Comments.PageInfo.HasNextPage, is.Comments.PageInfo.EndCursor},
			{"deletedCursor", is.TimelineItems.PageInfo.HasNextPage, is.TimelineItems.PageInfo.EndCursor},
		} {
			if p.next {
				if p.cursor == "" || read[p.name+" "+p.cursor] {
					return wrap(fmt.Errorf("%w: %s %q", errCursor, p.name, p.cursor))
				}
				read[p.name+" "+p.cursor], more = true, true
			}
			if p.cursor != "" {
				vars[p.name] = p.cursor
			}
		}
		if !more {
			return nil
		}
	}
}

// ErrGraphQL is wrapped by the error for a GraphQL answer that lists errors.
var ErrGraphQL = errors.New("graphql errors")

var (
	errNoIssue = errors.New("graphql: no such repository or issue")
	errCursor  = errors.New("graphql: a next page with no new cursor")
	errNoNode  = errors.New("graphql: no such node")

	errEventNoID = errors.New("graphql: a timeline event with no ID")
	errEventType = errors.New("graphql: a timeline event of a type the query doesn't ask for")
)

// historyQuery reads a page of each node's userContentEdits, newest first.
// Live (2026-10-12), GitHub lists the original post last, by its author,
// and keeps only the 99 newest edits after it. A revision someone deleted
// from the history keeps its entry, editor and time; only its text goes.
// For an issue body, it also reads each entry's diff, which live
// (2026-10-15) is the body's whole text after the edit: the newest one's
// equals the body. A comment's text isn't needed (state.Dirty), so the
// Issue fragment asks for it alone. GitHub merges the two selections of
// userContentEdits, which have the same arguments, for an Issue.
const historyQuery = `query($ids: [ID!]!, $cursor: String) {
  nodes(ids: $ids) {
    id
    ... on Comment {
      userContentEdits(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { editedAt editor { login __typename } }
      }
    }
    ... on Issue {
      userContentEdits(first: 100, after: $cursor) { nodes { diff } }
    }
  }
}`

// historyResponse is historyQuery's answer.
type historyResponse struct {
	Data struct {
		Nodes []*historyNode
	}
	gqlErrors
}

// historyNode is a node of historyQuery's answer, null for an ID GitHub
// can't resolve.
type historyNode struct {
	ID               string
	UserContentEdits struct {
		PageInfo struct {
			HasNextPage bool
			EndCursor   string
		}
		Nodes []struct {
			EditedAt time.Time
			Editor   *actor
			Diff     *string // null for a comment, or a deleted revision
		}
	}
}

// History reads the edit history of each issue body or comment, by node ID
// (Snapshot.BodyNode, Comment.Node), oldest first. Once there is an edit,
// the first entry is the original post. A body's entries carry their Text.
// It asks for up to 100 nodes per query, then pages on its own each node
// with more than 100 entries. A node GitHub resolves to none, as for an
// issue deleted since, fails the read only as a *HistoryGoneError, which
// holds the other nodes' histories.
func (c *Client) History(ctx context.Context, nodes []string) (map[string][]Edit, error) {
	all := map[string][]Edit{}
	var gone []string
	var goneErrs gqlErrors
	for ids := range slices.Chunk(nodes, 100) {
		more, missing, err := c.historyPage(ctx, ids, nil, all)
		if err != nil {
			return nil, fmt.Errorf("edit history: %w", err)
		}
		for _, id := range ids {
			if e, ok := missing[id]; ok {
				gone, goneErrs.Errors = append(gone, id), append(goneErrs.Errors, e)
			}
		}
		for _, id := range ids {
			read := map[string]bool{}
			for cursor, ok := more[id]; ok; {
				if cursor == "" || read[cursor] {
					return nil, fmt.Errorf("edit history of %s: %w: %q", id, errCursor, cursor)
				}
				read[cursor] = true
				next, missing, err := c.historyPage(ctx, []string{id}, &cursor, all)
				if err != nil {
					return nil, fmt.Errorf("edit history of %s: %w", id, err)
				}
				if e, ok := missing[id]; ok { // gone since its first page
					delete(all, id)
					gone, goneErrs.Errors = append(gone, id), append(goneErrs.Errors, e)
					break
				}
				cursor, ok = next[id]
			}
		}
	}
	for _, h := range all {
		slices.Reverse(h)
	}
	if len(gone) > 0 {
		return nil, &HistoryGoneError{Nodes: gone, Read: all, err: fmt.Errorf("edit history: history query: %w", goneErrs.err())}
	}
	return all, nil
}

// HistoryGoneError is History's error when GitHub resolves some of the
// nodes asked for to none, and lists no other error: each is null in the
// answer, with a NOT_FOUND error whose path names it (historyNotFound), as
// for an issue or comment deleted since. It wraps ErrGraphQL, with
// GitHub's messages. Read has the other nodes' histories, as History would
// return them, so one node that's gone doesn't fail the rest.
type HistoryGoneError struct {
	Nodes []string          // the nodes GitHub resolved to none, in the order asked
	Read  map[string][]Edit // the other nodes' histories, by node ID
	err   error
}

func (e *HistoryGoneError) Error() string { return e.err.Error() }
func (e *HistoryGoneError) Unwrap() error { return e.err }

// historyPage adds a page of each node's edits to all, newest first, and
// returns the cursor of each node with another page, and each node GitHub
// resolved to none (historyNotFound), with its error. Any other error
// GitHub lists fails the page.
func (c *Client) historyPage(ctx context.Context, ids []string, cursor *string, all map[string][]Edit) (map[string]string, map[string]gqlError, error) {
	req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": historyQuery,
		"variables": map[string]any{"ids": ids, "cursor": cursor}})
	if err != nil {
		return nil, nil, fmt.Errorf("history request: %w", err)
	}
	var out historyResponse
	if _, err := c.Do(req, &out); err != nil {
		return nil, nil, fmt.Errorf("history query: %w", err)
	}
	var missing map[string]gqlError
	if len(out.Data.Nodes) == len(ids) {
		missing = historyNotFound(out.Errors, out.Data.Nodes, ids)
	}
	if len(missing) < len(out.Errors) {
		if err := out.err(); err != nil {
			return nil, nil, fmt.Errorf("history query: %w", err)
		}
	}
	if len(out.Data.Nodes) != len(ids) {
		return nil, nil, fmt.Errorf("%w: %d nodes for %d IDs", errNoNode, len(out.Data.Nodes), len(ids))
	}
	more := map[string]string{}
	for i, n := range out.Data.Nodes {
		if _, ok := missing[ids[i]]; ok {
			continue
		}
		if n == nil || n.ID != ids[i] {
			return nil, nil, fmt.Errorf("%w: %s", errNoNode, ids[i])
		}
		for _, e := range n.UserContentEdits.Nodes {
			var text string
			if e.Diff != nil {
				text = *e.Diff
			}
			all[n.ID] = append(all[n.ID], Edit{By: e.Editor.author(), At: e.EditedAt, Text: text})
		}
		if n.UserContentEdits.PageInfo.HasNextPage {
			more[n.ID] = n.UserContentEdits.PageInfo.EndCursor
		}
	}
	return more, missing, nil
}

// historyNotFound returns, by ID, each of ids that errs and nodes, the
// history query's answer for them, resolve to none: its node is null, and
// a NOT_FOUND error's path is ["nodes", its index], as GitHub answers for
// the ID of an issue or comment that was deleted. Each comes with that
// error. A NOT_FOUND error with any other path isn't one.
func historyNotFound(errs []gqlError, nodes []*historyNode, ids []string) map[string]gqlError {
	missing := map[string]gqlError{}
	for _, e := range errs {
		if e.Type != "NOT_FOUND" || len(e.Path) != 2 || e.Path[0] != "nodes" {
			continue
		}
		f, ok := e.Path[1].(float64) // a JSON number
		if i := int(f); ok && float64(i) == f && i >= 0 && i < len(ids) && nodes[i] == nil {
			missing[ids[i]] = e
		}
	}
	return missing
}
