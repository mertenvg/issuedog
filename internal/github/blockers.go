package github

import (
	"context"
	"fmt"
)

// Blocker is one issue blocking another: its number, which is its own
// repo's, as a blocker may be in another repo, and whether it's closed.
type Blocker struct {
	Number int
	Closed bool
}

// Blockers lists the issues blocking issue number, open and closed, in the
// order GitHub lists them, each with whether it's closed, reading every page
// through BlockedBy. A repeat of an unchanged listing is a 304, which costs
// no rate limit, and a blocker closed or reopened since changes the listing.
func (c *Client) Blockers(ctx context.Context, owner, repo string, number int) ([]Blocker, error) {
	issues, err := c.BlockedBy(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("blockers: %w", err)
	}
	blockers := make([]Blocker, 0, len(issues))
	for _, is := range issues {
		blockers = append(blockers, Blocker{Number: is.GetNumber(), Closed: is.GetState() == "closed"})
	}
	return blockers, nil
}
