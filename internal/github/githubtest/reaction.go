package githubtest

import (
	"slices"

	gogithub "github.com/google/go-github/v92/github"
)

// RemoveReaction removes a reaction from a comment, as the user who made it
// can, and reports whether it was there.
func (f *Fake) RemoveReaction(commentID, reactionID int64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.findComment(commentID)
	n := len(c.reactions)
	c.reactions = slices.DeleteFunc(c.reactions, func(r *gogithub.Reaction) bool { return r.GetID() == reactionID })
	return len(c.reactions) < n
}
