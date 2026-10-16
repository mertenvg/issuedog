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
// its last edit, and a page of its CommentDeletedEvents. REST says when a
// body was edited, but not who, and lists no deletions. The deletions come
// in the same query (#186), so they cost no request of their own. An
// event's databaseId isn't read: GitHub's schema makes it a 32-bit Int,
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
          id fullDatabaseId body createdAt updatedAt lastEditedAt
          author { login __typename }
          editor { login __typename }
        }
      }
      timelineItems(itemTypes: [COMMENT_DELETED_EVENT], first: 100, after: $deletedCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          ... on CommentDeletedEvent {
            id createdAt
            actor { login __typename }
            deletedCommentAuthor { login __typename }
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
						Author               *actor
						CreatedAt, UpdatedAt time.Time
						editNode
					}
				}
				TimelineItems struct { // the CommentDeletedEvents
					PageInfo struct {
						HasNextPage bool
						EndCursor   string
					}
					Nodes []struct {
						ID                   string
						CreatedAt            time.Time
						Actor                *actor
						DeletedCommentAuthor *actor
					}
				}
			}
		}
	}
	gqlErrors
}

// gqlErrors is the errors list of a GraphQL answer.
type gqlErrors struct {
	Errors []struct{ Message string }
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
// edit, and its deleted comments (s.Deleted), oldest first, from one
// GraphQL query per 100 comments or deletions, whichever has more pages. A
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
				CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt, Edit: n.edit()})
		}
		for _, n := range is.TimelineItems.Nodes {
			// The query asks only for CommentDeletedEvents, so a node
			// without an ID isn't one: fail rather than miss a deletion.
			if n.ID == "" {
				return wrap(errDeletedNoID)
			}
			s.Deleted = append(s.Deleted, DeletedComment{ID: n.ID, By: n.Actor.author(),
				Author: n.DeletedCommentAuthor.author(), At: n.CreatedAt})
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

	errDeletedNoID = errors.New("graphql: a CommentDeletedEvent with no ID")
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
		Nodes []*struct {
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
	}
	gqlErrors
}

// History reads the edit history of each issue body or comment, by node ID
// (Snapshot.BodyNode, Comment.Node), oldest first. Once there is an edit,
// the first entry is the original post. A body's entries carry their Text.
// It asks for up to 100 nodes per query, then pages on its own each node
// with more than 100 entries.
func (c *Client) History(ctx context.Context, nodes []string) (map[string][]Edit, error) {
	all := map[string][]Edit{}
	for ids := range slices.Chunk(nodes, 100) {
		more, err := c.historyPage(ctx, ids, nil, all)
		if err != nil {
			return nil, fmt.Errorf("edit history: %w", err)
		}
		for _, id := range ids {
			read := map[string]bool{}
			for cursor, ok := more[id]; ok; {
				if cursor == "" || read[cursor] {
					return nil, fmt.Errorf("edit history of %s: %w: %q", id, errCursor, cursor)
				}
				read[cursor] = true
				next, err := c.historyPage(ctx, []string{id}, &cursor, all)
				if err != nil {
					return nil, fmt.Errorf("edit history of %s: %w", id, err)
				}
				cursor, ok = next[id]
			}
		}
	}
	for _, h := range all {
		slices.Reverse(h)
	}
	return all, nil
}

// historyPage adds a page of each node's edits to all, newest first, and
// returns the cursor of each node with another page.
func (c *Client) historyPage(ctx context.Context, ids []string, cursor *string, all map[string][]Edit) (map[string]string, error) {
	req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": historyQuery,
		"variables": map[string]any{"ids": ids, "cursor": cursor}})
	if err != nil {
		return nil, fmt.Errorf("history request: %w", err)
	}
	var out historyResponse
	if _, err := c.Do(req, &out); err != nil {
		return nil, fmt.Errorf("history query: %w", err)
	}
	if err := out.err(); err != nil {
		return nil, fmt.Errorf("history query: %w", err)
	}
	if len(out.Data.Nodes) != len(ids) {
		return nil, fmt.Errorf("%w: %d nodes for %d IDs", errNoNode, len(out.Data.Nodes), len(ids))
	}
	more := map[string]string{}
	for i, n := range out.Data.Nodes {
		if n == nil || n.ID != ids[i] {
			return nil, fmt.Errorf("%w: %s", errNoNode, ids[i])
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
	return more, nil
}
