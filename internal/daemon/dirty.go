package daemon

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
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

// Dirty finds the dirty content of s (state.Dirty), whoever wrote it, as
// trust holds now (SPEC §6.1; nil trusts nobody). An edit is sanctioned if
// its author, the App or, except on a comment the App wrote, a trusted user
// made it (state.Sanctioned), and for the body only the history after the
// App's last full rewrite counts (state.Rewrote), with the App as its
// author (state.AuthorEditWriter), so an untrusted issue author's own edit
// after a rewrite makes the body dirty (#87, SPEC §5.1). It reads histories
// only where the snapshot can't decide, all in one query:
//   - Content last edited by anyone whose edit isn't sanctioned is dirty,
//     by its last editor, whatever note it carries: its history isn't read
//     to find an older editor.
//   - Content with a note is dirty, by whoever the note names, if the App
//     made the last edit (the App wrote the note) or nobody edited it (its
//     author typed it, which only makes their own content ignored).
//   - Other unedited content is clean.
//   - A body the App last edited with a full rewrite is clean: the body s
//     read is the text of that edit, so it starts with the rewrite marker
//     (rewritten), and no edit after it exists.
//   - Other content last edited with a sanctioned edit needs its history,
//     since someone else may have edited it before. It is dirty by the
//     oldest editor whose edit isn't sanctioned (after the last rewrite,
//     for the body), else by whoever its note names, else it is clean. So
//     the App's own note never makes content look clean: the history keeps
//     the earlier edit, and the note keeps it dirty after GitHub drops that
//     edit, as it keeps only the 99 newest edits (github.History). Only
//     the author or a trusted user can undo that, or the App's full
//     rewrite of a body: after 99 edits of their own, one that drops the
//     note leaves no trace of the earlier edit. Nobody else can, since
//     their own edit makes it dirty. So a trusted user who loses trust
//     leaves their earlier edits dirty for good once MarkDirty notes them.
//
// A history that can't show the last edit, with fewer than two entries (the
// original post and an edit) or a newest entry older than the snapshot's
// edit, fails closed: the content is dirty by whoever its note names, else
// by its last editor, with a warning.
//
// A history that reaches the last edit is kept, by node, and used again
// while the content has the same last edit and body, so the loop's visits
// to an unchanged issue send no query (#84). Trust is asked once per login
// in each call, and only for an editor whose edit it decides (dirtyTrust):
// none whose edits all come before the body's last full rewrite. So trust
// that changed is used on the next call, unless trust keeps its verdicts,
// as a visit's does (trustOnceVisit): then it is used on the next pass.
func (a *Applier) Dirty(ctx context.Context, s *github.Snapshot, trust github.Trust) (Dirt, error) {
	wrap := func(err error) error {
		return fmt.Errorf("dirty content of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	tr := dirtyTrust{trust: trust, appSlug: a.AppSlug, verdicts: map[string]bool{}}
	d := Dirt{}
	var need []content
	for _, c := range contents(s) {
		edited := !c.edit.At.IsZero()
		if edited {
			if err := tr.ask(ctx, c, c.edit.By); err != nil {
				return nil, wrap(err)
			}
		}
		switch {
		case edited && !state.Sanctioned(c.id, c.author, c.edit.By, a.AppSlug, tr.trusted):
			d[c.id] = login(c.edit.By)
		case c.note != "" && (!edited || state.IsApp(c.edit.By, a.AppSlug)):
			d[c.id] = c.note
		case !edited || rewritten(c, a.AppSlug):
		default:
			need = append(need, c)
		}
	}
	if len(need) == 0 {
		return d, nil
	}
	if a.histories == nil {
		a.histories = histories{}
	}
	history, err := a.histories.read(ctx, a.Client, need)
	if err != nil {
		return nil, wrap(err)
	}
	for _, c := range need {
		h := history[c.node]
		if len(h) < 2 || h[len(h)-1].At.Before(c.edit.At) {
			logr.Warnf("%s/%s#%d: taking %s %d as dirty: its history has %d entries and doesn't reach its last edit at %s",
				a.Owner, a.Repo, s.Number, what(c.id), c.id, len(h), c.edit.At.Format(time.RFC3339))
			d[c.id] = cmp.Or(c.note, login(c.edit.By))
			continue
		}
		a.histories[c.node] = kept{at: c.edit.At, body: c.body, edits: h}
		// An edit before the body's last full rewrite no longer counts, so
		// its editor isn't asked about: their trust decides nothing, and a
		// failed lookup would fail the call for nothing (#174). Each edit
		// is asked about with the author state.Dirty judges it with: after
		// a rewrite the App is the body's author, so the issue's author is
		// asked about like anyone else (#87, SPEC §5.1).
		since := state.SinceRewrite(c.id, h, a.AppSlug)
		for i, e := range since {
			w := c
			w.author = state.AuthorEditWriter(c.id, c.author, h[:len(h)-len(since)+i], a.AppSlug)
			if err := tr.ask(ctx, w, e.By); err != nil {
				return nil, wrap(err)
			}
		}
		if by, ok := state.Dirty(c.id, c.author, h, a.AppSlug, tr.trusted); ok {
			d[c.id] = login(by)
		} else if c.note != "" {
			d[c.id] = c.note
		}
	}
	return d, nil
}

// rewritten reports whether c is the body, last edited by the App with a
// full rewrite: its text, which is that edit's, starts with the rewrite
// marker (state.Rewrote). Its note, if any, is checked before this.
func rewritten(c content, appSlug string) bool {
	return c.id == state.BodyID && state.Rewrote(github.Edit{By: c.edit.By, Text: c.body}, appSlug)
}

// dirtyTrust is the trust Dirty asks, each login once per call, by login
// in lower case, since GitHub's logins ignore case.
type dirtyTrust struct {
	trust    github.Trust
	appSlug  string
	verdicts map[string]bool
}

// ask asks trust about editor if only trust decides whether editor's edit
// to c is sanctioned (state.Sanctioned): an editor who is a person, not
// c's author, on the body or a comment the App didn't write. A nil trust
// trusts nobody.
func (t dirtyTrust) ask(ctx context.Context, c content, editor github.Author) error {
	switch {
	case t.trust == nil || editor.Login == "" || editor.Type == "Bot" || editor.Login == c.author.Login:
		return nil
	case c.id != state.BodyID && state.IsApp(c.author, t.appSlug):
		return nil
	}
	l := strings.ToLower(editor.Login)
	if _, ok := t.verdicts[l]; ok {
		return nil
	}
	v, err := t.trust.Trusted(ctx, editor.Login)
	if err != nil {
		return fmt.Errorf("trust %s, who edited %s %d: %w", editor.Login, what(c.id), c.id, err)
	}
	t.verdicts[l] = v
	return nil
}

// trusted is the verdict ask got for login, false if it asked none.
func (t dirtyTrust) trusted(login string) bool {
	return t.verdicts[strings.ToLower(login)]
}

// trustOnceVisit is the trust one visit to an issue shares between its
// Dirty calls (#174). Iterate's visit finds the issue's dirty content in
// Run, ReviewVisit, IntakeVisit and FlagVisit, and once more itself for
// openErrAny and the digest, and each call would ask trust about the same
// editors again: up to five permission requests per editor on an
// unchanged pass. It asks trust about each login once, ignoring case, as
// GitHub's logins do. It keeps verdicts only: an error isn't kept, so it
// fails the call that got it, and so the visit, and the next call asks
// again.
//
// Iterate makes one for each visit (Step.trustOnceBegin) and drops it
// after (Step.trustOnceEnd), so no verdict outlives the visit: each pass
// asks afresh, and trust that changed between passes is used on the next.
// The visit's own Dirty calls use it (Step.trustOnceTrust), and so do
// step 1's look for discussion triggers, its count of trusted comments
// (Step.counted), the hold item's look (Step.holdItemFind), and anything
// that asks Step.CurrentTrust during the visit, as the reaction triggers
// do, since a held issue's are looked for there again (#188). The
// visit's other trust lookups, a command's (Cmd.Trust), and stage work's,
// which comes after the visits, ask Step.Trust as before.
type trustOnceVisit struct {
	trust    github.Trust
	verdicts map[string]bool // by login in lower case
	// discWriters are DiscAccess's second answers, by login in lower case
	// (discussion.go). Its first answers are kept in verdicts.
	discWriters map[string]bool
}

// DiscAccess asks t.trust's DiscAccess about login, unless t has both its
// answers: a trusted login is no write collaborator who isn't trusted, so
// a verdict of trusted needs no request. A t.trust that isn't a
// github.DiscTrust knows no such collaborator.
func (t *trustOnceVisit) DiscAccess(ctx context.Context, login string) (trusted, writer bool, err error) {
	l := strings.ToLower(login)
	if w, ok := t.discWriters[l]; ok {
		return t.verdicts[l], w, nil
	}
	if t.verdicts[l] {
		return true, false, nil
	}
	dt, ok := t.trust.(github.DiscTrust)
	if !ok {
		v, err := t.Trusted(ctx, login)
		if err != nil {
			return false, false, fmt.Errorf("access of %s: %w", login, err)
		}
		return v, false, nil
	}
	if trusted, writer, err = dt.DiscAccess(ctx, login); err != nil {
		return false, false, fmt.Errorf("trust for this visit: %w", err)
	}
	if t.discWriters == nil {
		t.discWriters = map[string]bool{}
	}
	t.verdicts[l], t.discWriters[l] = trusted, writer
	return trusted, writer, nil
}

// Trusted asks t.trust about login, unless t already has its verdict.
func (t *trustOnceVisit) Trusted(ctx context.Context, login string) (bool, error) {
	l := strings.ToLower(login)
	if v, ok := t.verdicts[l]; ok {
		return v, nil
	}
	v, err := t.trust.Trusted(ctx, login)
	if err != nil {
		return false, fmt.Errorf("trust for this visit: %w", err)
	}
	t.verdicts[l] = v
	return v, nil
}

// trustOnceBegin starts a visit to an issue: until trustOnceEnd, the
// visit's Dirty calls share one trustOnceVisit over st.Trust as it is now.
// With no Trust there is nothing to share, and they use st.Trust, nil.
func (st *Step) trustOnceBegin() {
	st.trustOnce = nil
	if st.Trust != nil {
		st.trustOnce = &trustOnceVisit{trust: st.Trust, verdicts: map[string]bool{}}
	}
}

// trustOnceEnd ends the visit trustOnceBegin started, dropping its verdicts.
func (st *Step) trustOnceEnd() { st.trustOnce = nil }

// trustOnceTrust is the trust for one of a visit's Dirty calls: the
// visit's while one is under way, else st.Trust, as outside Iterate.
func (st *Step) trustOnceTrust() github.Trust {
	if st.trustOnce != nil {
		return st.trustOnce
	}
	return st.Trust
}

// histories are the edit histories Dirty kept, by node. Nothing is removed,
// so it grows with the content the daemon has seen edited.
type histories map[string]kept

// kept is a history read for content with this last edit and body. The key
// leaves out the editor: any later edit changes lastEditedAt.
type kept struct {
	at    time.Time
	body  string
	edits []github.Edit
}

// read returns the history of each of need, by node: a kept one while the
// content's last edit and body match it, else from one query for the rest.
func (hs histories) read(ctx context.Context, client *github.Client, need []content) (map[string][]github.Edit, error) {
	all := map[string][]github.Edit{}
	var nodes []string
	for _, c := range need {
		if h, ok := hs[c.node]; ok && h.at.Equal(c.edit.At) && h.body == c.body {
			all[c.node] = h.edits
		} else {
			nodes = append(nodes, c.node)
		}
	}
	if len(nodes) == 0 {
		return all, nil
	}
	read, err := client.History(ctx, nodes)
	if err != nil {
		return nil, fmt.Errorf("read edit histories: %w", err)
	}
	maps.Copy(all, read)
	return all, nil
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
			// A history kept for the body as it was gains the App's edit,
			// as GitHub's does, so originLost, right after, reads none for
			// the noted body (#501).
			if h, ok := a.histories[c.node]; ok && h.at.Equal(c.edit.At) && h.body == c.body {
				a.histories[c.node] = kept{at: s.BodyEdit.At, body: s.Body, edits: append(slices.Clip(h.edits), github.Edit{By: app, At: s.BodyEdit.At, Text: s.Body})}
			}
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
