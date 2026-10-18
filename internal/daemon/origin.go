package daemon

import (
	"context"
	"fmt"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// originRepostText is the App's line above an origin marker it reposts.
const originRepostText = "This issue's description was edited, so I've copied its `origin` marker here to keep tracking it in its plan."

// originRepost posts again, in an App comment, the origin marker an
// App-filed issue's body no longer gives (S5's T2, #291; SPEC §5: "or
// reposted in a comment once that body is dirty"). Step.Run calls it
// right after MarkDirty, before the type check, so the issue keeps its
// class (state.Origin) from the first pass on, held or not. It acts only
// when all three hold:
//   - the App wrote the issue;
//   - s has no Origin (state.Derive), in the body or a clean App comment;
//   - the body's markers aren't read: it's dirty, or someone other than
//     the App edited it last, as a trusted user's edit does, even one
//     that keeps the marker's text.
//
// It then reads the body's edit history once (github.History), or uses
// the one Dirty kept for this edit (histories), and takes the marker from
// the newest version the App wrote that has a whole one, back to the
// original post, which the App wrote too. So a version of the App's that
// someone deleted from the history, or the App's own dirty note, which
// escapes the text it keeps, is passed over, down to the original post.
// The comment holds the whole marker, every field as it was written:
// human=1, and a check's fields, still percent-encoded
// (state.OriginMarker). It has no cc line, since there's nothing for
// anyone to do. The comment is the record: it's added to s, so Derive
// reads Origin from it in the same pass, and a replay finds it and posts
// nothing more. Should anyone edit it, it's dirty, so it gives no Origin,
// and it's reposted once more.
//
// With no marker in any version, it logs a warning and posts nothing. It
// keeps that in memory, by issue and body text, which an edit changes, so
// the next pass over the same body logs nothing and reads no history.
func (a *Applier) originRepost(ctx context.Context, s *github.Snapshot, d Dirt) error {
	if !state.IsApp(s.Author, a.AppSlug) || state.Derive(s, a.AppSlug, d.Has).Origin.Plan != 0 {
		return nil
	}
	if e := s.BodyEdit; !d.Has(state.BodyID) && (e.At.IsZero() || state.IsApp(e.By, a.AppSlug)) {
		return nil // the body's markers are read, and it has no origin
	}
	if body, ok := a.originNone[s.Number]; ok && body == s.Body {
		return nil
	}
	wrap := func(err error) error {
		return fmt.Errorf("repost the origin of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	if a.histories == nil {
		a.histories = histories{}
	}
	body := content{id: state.BodyID, node: s.BodyNode, author: s.Author, body: s.Body, edit: s.BodyEdit}
	read, err := a.histories.read(ctx, a.Client, []content{body})
	if err != nil {
		return wrap(err)
	}
	m, ok := originFromHistory(read[s.BodyNode], a.AppSlug)
	if !ok {
		logr.Warnf("%s/%s#%d: the App filed it, and its description has lost its origin marker, but no version of it the App wrote has one, so it's not reposted",
			a.Owner, a.Repo, s.Number)
		if a.originNone == nil {
			a.originNone = map[int]string{}
		}
		a.originNone[s.Number] = s.Body
		return nil
	}
	marker, err := github.FormatMarker(m)
	if err != nil {
		return wrap(err)
	}
	logr.Infof("%s/%s#%d: its description lost its origin marker, so posting it again in a comment", a.Owner, a.Repo, s.Number)
	if err := a.comment(ctx, s, join(originRepostText, marker)); err != nil {
		return wrap(err)
	}
	return nil
}

// originFromHistory returns the whole origin marker of the newest version
// in h, a body's history, oldest first, that the App wrote and that has
// one: an edit by the App, or the original post, h[0], whoever GitHub
// names as its author, since the caller knows the App filed the issue. A
// version someone deleted has no text, so it has none. It's false if no
// such version has one.
func originFromHistory(h []github.Edit, appSlug string) (github.Marker, bool) {
	for i := len(h) - 1; i >= 0; i-- {
		if i > 0 && !state.IsApp(h[i].By, appSlug) {
			continue
		}
		if m, ok := state.OriginMarker(h[i].Text); ok {
			return m, true
		}
	}
	return github.Marker{}, false
}
