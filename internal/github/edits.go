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

// commentsQuery reads an issue's body and a page of its comments, each with
// its last edit. REST says when a body was edited, but not who.
const commentsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
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
    }
  }
}`

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
// edit, from one GraphQL query per 100 comments. A body and its edit come
// from the same read, so they match.
func (c *Client) readComments(ctx context.Context, owner, repo string, s *Snapshot) error {
	wrap := func(err error) error { return fmt.Errorf("comments of %s/%s#%d: %w", owner, repo, s.Number, err) }
	read := map[string]bool{}
	vars := map[string]any{"owner": owner, "name": repo, "number": s.Number, "cursor": nil}
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
		cursor := is.Comments.PageInfo.EndCursor
		if !is.Comments.PageInfo.HasNextPage {
			return nil
		}
		if cursor == "" || read[cursor] {
			return wrap(fmt.Errorf("%w: %q", errCursor, cursor))
		}
		read[cursor] = true
		vars["cursor"] = cursor
	}
}

// ErrGraphQL is wrapped by the error for a GraphQL answer that lists errors.
var ErrGraphQL = errors.New("graphql errors")

var (
	errNoIssue = errors.New("graphql: no such repository or issue")
	errCursor  = errors.New("graphql: a next page with no new cursor")
	errNoNode  = errors.New("graphql: no such node")
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
