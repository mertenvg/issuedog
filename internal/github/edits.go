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
// By is empty if the editor's account is gone.
type Edit struct {
	By Author
	At time.Time
}

// edited is a body and its last edit, read together so they match.
type edited struct {
	Body string
	Edit Edit
}

// editsQuery reads the body and last edit of an issue and of a page of its
// comments. REST says when a body was edited, but not who.
const editsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      body
      lastEditedAt
      editor { login __typename }
      comments(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { fullDatabaseId body lastEditedAt editor { login __typename } }
      }
    }
  }
}`

// editsResponse is editsQuery's answer.
type editsResponse struct {
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
						FullDatabaseID string `json:"fullDatabaseId"` // a BigInt
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
	Editor       *struct {
		Login    string
		Typename string `json:"__typename"`
	}
}

// edit returns n's Edit. GraphQL gives a Bot's login without the "[bot]"
// that REST shows, so it is added back to match.
func (n editNode) edit() Edit {
	if n.LastEditedAt == nil {
		return Edit{}
	}
	e := Edit{At: *n.LastEditedAt}
	if n.Editor != nil {
		e.By = Author{Login: n.Editor.Login, Type: n.Editor.Typename}
		if e.By.Type == "Bot" && !strings.HasSuffix(e.By.Login, "[bot]") {
			e.By.Login += "[bot]"
		}
	}
	return e
}

// edits reads the body and last edit of an issue, and of each comment by ID,
// with one GraphQL query per 100 comments.
func (c *Client) edits(ctx context.Context, owner, repo string, number int) (edited, map[int64]edited, error) {
	wrap := func(err error) error { return fmt.Errorf("edits of %s/%s#%d: %w", owner, repo, number, err) }
	var body edited
	comments := map[int64]edited{}
	read := map[string]bool{}
	vars := map[string]any{"owner": owner, "name": repo, "number": number, "cursor": nil}
	for page := 0; ; page++ {
		req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": editsQuery, "variables": vars})
		if err != nil {
			return edited{}, nil, wrap(err)
		}
		var out editsResponse
		if _, err := c.Do(req, &out); err != nil {
			return edited{}, nil, wrap(err)
		}
		if len(out.Errors) > 0 {
			msgs := make([]string, 0, len(out.Errors))
			for _, e := range out.Errors {
				msgs = append(msgs, e.Message)
			}
			return edited{}, nil, wrap(fmt.Errorf("%w: %s", ErrGraphQL, strings.Join(msgs, "; ")))
		}
		if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
			return edited{}, nil, wrap(errNoIssue)
		}
		is := out.Data.Repository.Issue
		if page == 0 {
			body = edited{is.Body, is.edit()}
		}
		for _, n := range is.Comments.Nodes {
			id, err := strconv.ParseInt(n.FullDatabaseID, 10, 64)
			if err != nil {
				return edited{}, nil, wrap(fmt.Errorf("comment ID: %w", err))
			}
			comments[id] = edited{n.Body, n.edit()}
		}
		cursor := is.Comments.PageInfo.EndCursor
		switch {
		case !is.Comments.PageInfo.HasNextPage:
			return body, comments, nil
		case cursor == "":
			return edited{}, nil, wrap(errNoCursor)
		case read[cursor]:
			return edited{}, nil, wrap(fmt.Errorf("%w: %q", errStuckCursor, cursor))
		}
		read[cursor] = true
		vars["cursor"] = cursor
	}
}

// ErrGraphQL is wrapped by the error for a GraphQL answer that lists errors.
var ErrGraphQL = errors.New("graphql errors")

var (
	errNoIssue     = errors.New("graphql: no such repository or issue")
	errNoCursor    = errors.New("graphql: a next page with no cursor")
	errStuckCursor = errors.New("graphql: a next page at a cursor already read")
)
