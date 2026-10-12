package github

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Edit is who last edited a body, and when. The zero Edit means never edited.
// By is empty if GitHub names no editor; a deleted account shows as ghost.
type Edit struct {
	By Author
	At time.Time
}

// commentsQuery reads an issue's body and a page of its comments, each with
// its last edit. REST says when a body was edited, but not who.
const commentsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      body
      lastEditedAt
      editor { login __typename }
      comments(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          fullDatabaseId body createdAt updatedAt lastEditedAt
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
	Errors []struct{ Message string }
}

// editNode is the body and edit fields of an issue or comment.
type editNode struct {
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
		if len(out.Errors) > 0 {
			msgs := make([]string, 0, len(out.Errors))
			for _, e := range out.Errors {
				msgs = append(msgs, e.Message)
			}
			return wrap(fmt.Errorf("%w: %s", ErrGraphQL, strings.Join(msgs, "; ")))
		}
		if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
			return wrap(errNoIssue)
		}
		is := out.Data.Repository.Issue
		if page == 0 {
			s.Body, s.BodyEdit = is.Body, is.edit()
		}
		for _, n := range is.Comments.Nodes {
			id, err := strconv.ParseInt(n.FullDatabaseID, 10, 64)
			if err != nil {
				return wrap(fmt.Errorf("comment ID: %w", err))
			}
			s.Comments = append(s.Comments, Comment{ID: id, Author: n.Author.author(), Body: n.Body,
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
)
