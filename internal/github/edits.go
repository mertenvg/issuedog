package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Edit is who last edited a body, and when. The zero Edit means never edited.
// By is empty if the editor's account is gone.
type Edit struct {
	By Author
	At time.Time
}

// editsQuery reads the last edit of an issue's body and of a page of its
// comments. REST says when, but not who.
const editsQuery = `query($owner: String!, $name: String!, $number: Int!, $cursor: String) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) {
      lastEditedAt
      editor { login __typename }
      comments(first: 100, after: $cursor) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId lastEditedAt editor { login __typename } }
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
						DatabaseID int64 `json:"databaseId"`
						editNode
					}
				}
			}
		}
	}
	Errors []struct{ Message string }
}

// editNode is the edit fields of an issue or comment.
type editNode struct {
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

// edits reads the last edit of an issue's body, and of each comment by ID,
// with one GraphQL query per 100 comments.
func (c *Client) edits(ctx context.Context, owner, repo string, number int) (Edit, map[int64]Edit, error) {
	var body Edit
	comments := map[int64]Edit{}
	vars := map[string]any{"owner": owner, "name": repo, "number": number, "cursor": nil}
	for page := 0; ; page++ {
		req, err := c.NewRequest(ctx, "POST", "graphql", map[string]any{"query": editsQuery, "variables": vars})
		if err != nil {
			return Edit{}, nil, fmt.Errorf("edits of %s/%s#%d: %w", owner, repo, number, err)
		}
		var out editsResponse
		if _, err := c.Do(req, &out); err != nil {
			return Edit{}, nil, fmt.Errorf("edits of %s/%s#%d: %w", owner, repo, number, err)
		}
		if len(out.Errors) > 0 {
			msgs := make([]string, 0, len(out.Errors))
			for _, e := range out.Errors {
				msgs = append(msgs, e.Message)
			}
			return Edit{}, nil, fmt.Errorf("edits of %s/%s#%d: %w: %s", owner, repo, number, ErrGraphQL, strings.Join(msgs, "; "))
		}
		if out.Data.Repository == nil || out.Data.Repository.Issue == nil {
			return Edit{}, nil, fmt.Errorf("edits of %s/%s#%d: %w", owner, repo, number, errNoIssue)
		}
		is := out.Data.Repository.Issue
		if page == 0 {
			body = is.edit()
		}
		for _, n := range is.Comments.Nodes {
			comments[n.DatabaseID] = n.edit()
		}
		if !is.Comments.PageInfo.HasNextPage {
			return body, comments, nil
		}
		if is.Comments.PageInfo.EndCursor == "" {
			return Edit{}, nil, fmt.Errorf("edits of %s/%s#%d: %w", owner, repo, number, errNoCursor)
		}
		vars["cursor"] = is.Comments.PageInfo.EndCursor
	}
}

// ErrGraphQL is wrapped by the error for a GraphQL answer that lists errors.
var ErrGraphQL = errors.New("graphql errors")

var (
	errNoIssue  = errors.New("graphql: no such repository or issue")
	errNoCursor = errors.New("graphql: a next page with no cursor")
)
