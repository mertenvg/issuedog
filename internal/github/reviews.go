package github

import (
	"cmp"
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v92/github"
)

// The states a pull request's review has in its `reviewed` timeline event,
// lower case as that event gives them (the reviews list gives them upper
// case). A pending review has no event.
const (
	ReviewCommented        = "commented"
	ReviewChangesRequested = "changes_requested"
	ReviewApproved         = "approved"
	ReviewDismissed        = "dismissed"
)

// ReviewComment is one line comment of a pull request's review: its ID,
// its author's login, the file it's on, the line it's on, and its text.
// Line is the line in the file as the PR shows it now, or, when the
// comment is outdated, the line it was made on, or 0 for a comment on the
// whole file.
type ReviewComment struct {
	ID           int64
	Author, Path string
	Line         int
	Body         string
}

// ReviewComments lists the line comments of review on pull request pr in
// owner/repo, oldest first, as GitHub serves them, reading every page of
// GET /repos/{owner}/{repo}/pulls/{pr}/reviews/{review}/comments. The
// review's ID is its `reviewed` timeline event's (TimelineSince).
func (c *Client) ReviewComments(ctx context.Context, owner, repo string, pr int, review int64) ([]ReviewComment, error) {
	all, err := collect(c.PullRequests.ListReviewCommentsIter(ctx, owner, repo, pr, review, &gogithub.ListOptions{PerPage: 100}))
	if err != nil {
		return nil, fmt.Errorf("list the comments of review %d on %s/%s#%d: %w", review, owner, repo, pr, err)
	}
	comments := make([]ReviewComment, 0, len(all))
	for _, cm := range all {
		comments = append(comments, ReviewComment{ID: cm.GetID(), Author: cm.GetUser().GetLogin(), Path: cm.GetPath(),
			Line: *cmp.Or(cm.Line, cm.OriginalLine, new(0)), Body: cm.GetBody()})
	}
	return comments, nil
}
