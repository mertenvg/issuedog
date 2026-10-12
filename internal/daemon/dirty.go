package daemon

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Dirt is an issue's dirty content, by comment ID or state.BodyID (#91),
// each with the login of someone who edited it without approval (see
// Applier.Dirty for which one). "unknown" stands for an editor GitHub
// doesn't name.
type Dirt map[int64]string

// Has reports whether the comment or body id is dirty. It is the dirty
// function state.Derive and Applier.Transition take.
func (d Dirt) Has(id int64) bool {
	_, ok := d[id]
	return ok
}

// content is the issue body or one comment.
type content struct {
	id     int64 // a comment ID, or state.BodyID
	node   string
	author github.Author
	body   string
	edit   github.Edit
	note   string // the login a note at the top of body names, if any
	rest   string // body below the note, or all of it
}

// contents lists s's body, then its comments, oldest first.
func contents(s *github.Snapshot) []content {
	all := []content{{id: state.BodyID, node: s.BodyNode, author: s.Author, body: s.Body, edit: s.BodyEdit}}
	for _, c := range s.Comments {
		all = append(all, content{id: c.ID, node: c.Node, author: c.Author, body: c.Body, edit: c.Edit})
	}
	for i := range all {
		all[i].note, all[i].rest = noted(all[i].body)
	}
	return all
}

// Dirty finds the dirty content of s (state.Dirty), whoever wrote it. It
// reads histories only where the snapshot can't decide, all in one query:
//   - Content last edited by anyone but its author or the App (see
//     state.Sanctioned) is dirty, by its last editor, whatever note it
//     carries: its history isn't read to find an older editor.
//   - Content with a note is dirty, by whoever the note names, if the App
//     made the last edit (the App wrote the note) or nobody edited it (its
//     author typed it, which only makes their own content ignored).
//   - Other unedited content is clean.
//   - Content last edited by its author or the App needs its history,
//     since someone else may have edited it before them. It is dirty by
//     the oldest editor whose edit isn't sanctioned, else by whoever its
//     note names, else it is clean. So the App's own note never makes
//     content look clean: the history keeps the earlier edit, and the note
//     keeps it dirty after GitHub drops that edit, as it keeps only the 99
//     newest edits (github.History). Only the author can undo that: after
//     99 edits of their own, one that drops the note leaves no trace of the
//     earlier edit. Nobody else can, since their own edit makes it dirty.
//
// A history that can't show the last edit, with fewer than two entries (the
// original post and an edit) or a newest entry older than the snapshot's
// edit, fails closed: the content is dirty by whoever its note names, else
// by its last editor, with a warning.
func (a *Applier) Dirty(ctx context.Context, s *github.Snapshot) (Dirt, error) {
	d := Dirt{}
	var need []content
	var nodes []string
	for _, c := range contents(s) {
		edited := !c.edit.At.IsZero()
		switch {
		case edited && !state.Sanctioned(c.author, c.edit.By, a.AppSlug):
			d[c.id] = login(c.edit.By)
		case c.note != "" && (!edited || state.IsApp(c.edit.By, a.AppSlug)):
			d[c.id] = c.note
		case !edited:
		default:
			need, nodes = append(need, c), append(nodes, c.node)
		}
	}
	if len(need) == 0 {
		return d, nil
	}
	history, err := a.Client.History(ctx, nodes)
	if err != nil {
		return nil, fmt.Errorf("dirty content of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	for _, c := range need {
		h := history[c.node]
		if len(h) < 2 || h[len(h)-1].At.Before(c.edit.At) {
			logr.Warnf("%s/%s#%d: taking %s %d as dirty: its history has %d entries and doesn't reach its last edit at %s",
				a.Owner, a.Repo, s.Number, what(c.id), c.id, len(h), c.edit.At.Format(time.RFC3339))
			d[c.id] = cmp.Or(c.note, login(c.edit.By))
		} else if by, ok := state.Dirty(c.author, h, a.AppSlug); ok {
			d[c.id] = login(by)
		} else if c.note != "" {
			d[c.id] = c.note
		}
	}
	return d, nil
}

// MarkDirty puts a visible note at the top of each dirty body or comment in
// s, naming who d says made it dirty, and keeps the rest of the text,
// escaped (SPEC §5.1). A note that someone removed is put back, and one
// that names anyone else, such as a forged one, is replaced, with the text
// below it escaped again. Each write is added to s. It writes from s, so an
// edit made since s was read is lost, but that content is dirty already.
// The App's edit can't make the content clean (see Dirty).
func (a *Applier) MarkDirty(ctx context.Context, s *github.Snapshot, d Dirt) error {
	app := github.Author{Login: a.AppSlug + "[bot]", Type: "Bot"}
	for i, c := range contents(s) {
		by, ok := d[c.id]
		if !ok || c.note == by {
			continue
		}
		body, err := note(c.id, by, c.rest)
		if err != nil {
			return fmt.Errorf("mark %s/%s#%d dirty: %w", a.Owner, a.Repo, s.Number, err)
		}
		logr.Infof("%s/%s#%d: noting that %s edited %s without approval", a.Owner, a.Repo, s.Number, by, what(c.id))
		if c.id == state.BodyID {
			is, _, err := a.Client.Issues.Update(ctx, a.Owner, a.Repo, s.Number, gogithub.UpdateIssueRequest{Body: &body})
			if err != nil {
				return fmt.Errorf("mark the body of %s/%s#%d dirty: %w", a.Owner, a.Repo, s.Number, err)
			}
			s.Body, s.BodyEdit = is.GetBody(), github.Edit{By: app, At: is.GetUpdatedAt().Time}
			continue
		}
		cm, _, err := a.Client.Issues.UpdateComment(ctx, a.Owner, a.Repo, c.id, gogithub.IssueCommentRequest{Body: body})
		if err != nil {
			return fmt.Errorf("mark comment %d on %s/%s#%d dirty: %w", c.id, a.Owner, a.Repo, s.Number, err)
		}
		s.Comments[i-1].Body, s.Comments[i-1].Edit = cm.GetBody(), github.Edit{By: app, At: cm.GetUpdatedAt().Time}
	}
	return nil
}

// notePrefix starts the visible line of a note.
const notePrefix = "> ⚠️ issuedog ignores this "

// note returns text with the note for content id, edited by by, on top:
// its marker on the first line, so noted finds it, then the visible line.
func note(id int64, by, text string) (string, error) {
	marker, err := github.FormatMarker(github.Marker{Kind: "dirty", Fields: map[string]string{"by": by}})
	if err != nil {
		return "", fmt.Errorf("note: %w", err)
	}
	return fmt.Sprintf("%s\n%s%s: **%s** edited it without approval.\n\n%s", marker, notePrefix, what(id), by, github.Escape(text)), nil
}

// what names content id in a note.
func what(id int64) string {
	if id == state.BodyID {
		return "issue description"
	}
	return "comment"
}

// noted returns the login a note names and the text below the note, if
// text starts with one, else "" and all of text. A web edit may turn its
// line endings into CRLF.
func noted(text string) (by, rest string) {
	line, below, _ := strings.Cut(text, "\n")
	m := github.ParseMarkers(line)
	if len(m) != 1 || m[0].Kind != "dirty" || !strings.HasPrefix(below, notePrefix) {
		return "", text
	}
	by = m[0].Fields["by"]
	if by == "" || strings.TrimSuffix(line, "\r") != "<!-- issuedog:dirty by="+by+" -->" {
		return "", text
	}
	_, below, _ = strings.Cut(below, "\n")
	return by, strings.TrimLeft(below, "\r\n")
}

// login is who an editor is, as a note names them.
func login(a github.Author) string {
	if a.Login == "" {
		return "unknown"
	}
	return a.Login
}
