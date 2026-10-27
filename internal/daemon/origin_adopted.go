package daemon

import (
	"context"
	"fmt"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// adoptedRepostText is the App's line above the origin marker it reposts
// for an adopted child (adoptedRepost).
const adoptedRepostText = "I rewrote this issue's description from its plan, but it was filed by hand, so the `origin` marker there isn't read, " +
	"and no comment of mine that's still read carries it. So I've copied it here, to keep tracking the issue in its plan."

// adoptedRepost posts again, in an App comment, the origin marker of an
// adopted child's body, while Derive reads another Origin for the issue,
// or none (#374; SPEC §6.4, "A changed item adopted from a person"). An
// adopted child is one a person filed, whose body the App rewrote whole
// from a revision (adoptedRewrite). Derive reads no marker from a body a
// person filed, so the rewrite's marker is read from the move's comment
// that carries it (childChanged). Once anyone edits that comment, it's
// dirty, so Derive falls back to an older clean App comment with one,
// such as the one that adopted the issue, with its earlier rev, and the
// planner would see that revision's item. Step.Run calls it right after
// originRepost, before the type check, so the issue reads the rewrite's
// Origin from the first pass on.
//
// The body's text is the App's whole, and clean, so its marker is the
// App's own (SPEC §5.1): an edit of it since makes it dirty, and then
// adoptedRewrite gives nothing, and Derive reads the move's comment, if
// it's clean. The comment holds the whole marker, every field as it was
// written, and no cc line, since there's nothing for anyone to do. It's
// the record: it's added to s, so Derive reads the Origin from it in the
// same pass, and a replay posts nothing more; so is a move's comment
// that carries the same marker. Should it be posted between the rewrite
// and its move, as after a crash, the move carries no marker of its own
// (childChanged), as Derive reads the revision's Origin from it already.
func (a *Applier) adoptedRepost(ctx context.Context, s *github.Snapshot, d Dirt) error {
	m, o, ok := adoptedRewrite(s, a.AppSlug, d.Has)
	if !ok {
		return nil
	}
	read := state.Derive(s, a.AppSlug, d.Has).Origin
	read.Comment, o.Comment = 0, 0
	if read == o {
		return nil
	}
	marker, err := github.FormatMarker(m)
	if err != nil {
		return fmt.Errorf("repost the origin of %s/%s#%d from its rewritten description: %w", a.Owner, a.Repo, s.Number, err)
	}
	logr.Infof("%s/%s#%d: it was filed by hand, and no comment that's read carries the origin marker of the description I rewrote, so posting it again in a comment",
		a.Owner, a.Repo, s.Number)
	if err := a.comment(ctx, s, join(adoptedRepostText, marker)); err != nil {
		return fmt.Errorf("repost the origin of %s/%s#%d from its rewritten description: %w", a.Owner, a.Repo, s.Number, err)
	}
	return nil
}
