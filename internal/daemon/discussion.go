package daemon

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// Discussion holds (SPEC §6.4, plan #87 rev 2, T3). A write collaborator
// who isn't trusted can take part in an issue, but not move it, so what
// they do on an issue in the pipeline holds that issue, and only it: each
// such act is a trigger. A trigger is
//   - found by discFind, from the snapshot, its dirty content and the
//     renamed events CheckLabels read, each keyed by its event= key (#87's
//     assumption 3);
//   - open from then until something resolves it. In this task only a move
//     out of held does (discFloor): a stage label set by hand, which ends
//     every hold (SPEC §6.4). T4 (#188) adds a trusted 👍, a counter
//     comment and /resume on the issue, by leaving out of discFind's
//     result each trigger it finds resolved;
//   - handled once a marker records its key: the hold's transition marker,
//     keyed event=<key> with reason=discussion, or a notice's marker,
//     notice key=<key> (discRecords). So a second pass writes nothing, and
//     no key gets a second notice. A comment trigger on an issue held
//     already has no record at all (Q2b): it is known by being open.
//
// An edit or a deletion of the App's own content can hide the markers that
// set the issue's stage, since markers in such content are never read
// again (SPEC §5.1). discRestore holds such an issue at the stage it was
// at before, before the label rules would put the label back to the stage
// the markers left show.
//
// T6 (#190) adds closes as a kind of its own, and restores through
// discRestore what a close hides too. T8 (#192) lists the open triggers of
// a held issue in the digest and assignment.

// discKind is what kind of act a discussion trigger is.
type discKind string

const (
	discComment  discKind = "comment"  // a new comment: key c<comment id>
	discEdit     discKind = "edit"     // an edit of the App's or a trusted user's content: e<id>-<edit time>
	discOwnEdit  discKind = "own-edit" // an edit of their own comment after a trusted 👍: e<id>-<edit time>
	discDeletion discKind = "deletion" // a comment deleted: d<event node id>
	discRename   discKind = "rename"   // a rename of the issue: <timeline event id>
)

// discTrigger is one discussion trigger on an issue.
type discTrigger struct {
	Kind discKind
	// Key is its event= key (#87's assumption 3). An edit's names the
	// content, a comment ID or state.BodyID, and the edit's time in Unix
	// seconds, so each edit is a trigger of its own.
	Key string
	By  string    // who did it
	At  time.Time // when
	// ID is the comment for a comment, or the content for an edit, a
	// comment ID or state.BodyID; 0 for any other kind.
	ID int64
	// Of is who wrote what an edit or a deletion changed, "" for nobody
	// GitHub names; Title is the title a rename set. Neither is the App's
	// text, so discWhat escapes them.
	Of, Title string
}

// discWhat says what tr is, for its hold's comment or its notice. The
// logins and the title aren't the App's text, so the logins are code and
// defused, the title is defused and its tags escaped (inline), and the
// caller escapes the whole (github.Escape).
func discWhat(tr discTrigger) string {
	by := discLogin(tr.By)
	switch tr.Kind {
	case discComment:
		return fmt.Sprintf("%s posted comment %d", by, tr.ID)
	case discEdit:
		if tr.ID == state.BodyID {
			return fmt.Sprintf("%s edited the issue description, written by %s", by, discLogin(tr.Of))
		}
		return fmt.Sprintf("%s edited comment %d by %s", by, tr.ID, discLogin(tr.Of))
	case discOwnEdit:
		return fmt.Sprintf("%s edited their comment %d after a code owner approved it", by, tr.ID)
	case discDeletion:
		return fmt.Sprintf("%s deleted a comment by %s", by, discLogin(tr.Of))
	}
	return fmt.Sprintf("%s renamed this issue to “%s”", by, inline(tr.Title))
}

// discLogin is login as code, defused, or words for nobody GitHub names.
func discLogin(login string) string {
	if login == "" || login == "unknown" {
		return "someone GitHub doesn't name"
	}
	return "`" + defuse(login) + "`"
}

// discMarkers calls do with each marker s's state is read from, as
// state.Derive reads them: the App's, not in dirty content, and in the
// body only while the App made its last edit. The body's come first, at
// the issue's creation, then each comment's, with its ID and time.
func discMarkers(s *github.Snapshot, appSlug string, dirty func(int64) bool, do func(id int64, at time.Time, m github.Marker)) {
	read := func(id int64, author github.Author, body string, at time.Time) {
		if dirty(id) {
			return
		}
		if e := s.BodyEdit; id == state.BodyID && !e.At.IsZero() && !state.IsApp(e.By, appSlug) {
			return
		}
		for _, m := range github.TrustedMarkers(author, body, appSlug) {
			do(id, at, m)
		}
	}
	read(state.BodyID, s.Author, s.Body, s.CreatedAt)
	for _, c := range s.Comments {
		read(c.ID, c.Author, c.Body, c.CreatedAt)
	}
}

// discBound bounds an issue's open discussion triggers (discFloor): an act
// counts at or after At, and a comment, unless ByTime, only above ID. A
// trigger a marker records counts if that marker is in comment ID or a
// later one (discRecords).
type discBound struct {
	ID     int64     // the comment holding the marker that sets the bound
	At     time.Time // when the bound begins
	ByTime bool      // compare comments by their creation, not their ID
}

// discFloor returns what bounds an issue's open discussion triggers: its
// first transition marker, or else its latest move out of held, whichever
// is later.
//   - Nothing before the first transition marker counts, so old comments
//     don't hold issues on the day this ships (#87's assumption 2).
//   - A move out of held resolves every trigger before it. In this task the
//     only one a discussion hold allows is a stage label set by hand, an
//     override, which ends every hold (SPEC §6.4). The App records it on
//     its next pass, after the label's event, so the bound is that event's
//     time (s.DiscLabelTimes), and comments are compared by when they were
//     posted: an act between the label and its record still counts. With
//     the event's time unknown, as on a snapshot CheckLabels hasn't
//     checked, it's the record's.
//
// The markers are read as state.Derive reads them (discMarkers). With none,
// ok is false, as on an issue that isn't in the pipeline, which nothing
// holds, but for one with a stage label past intake: its markers were
// hidden by an edit or a deletion of the App's content, which discRestore
// holds, so every act counts, failing closed.
func discFloor(s *github.Snapshot, appSlug string, dirty func(int64) bool) (b discBound, ok bool) {
	discMarkers(s, appSlug, dirty, func(id int64, at time.Time, m github.Marker) {
		if _, bounds := state.RoundBound(m, id); !bounds {
			return
		}
		out := state.Stage(m.Fields["from"]) == state.Held && state.Stage(m.Fields["to"]) != state.Held
		if ok && !out {
			return
		}
		b, ok = discBound{ID: id, At: at}, true
		if ev, err := strconv.ParseInt(m.Fields["event"], 10, 64); out && m.Fields["by"] != "" && err == nil {
			if t, known := s.DiscLabelTimes[ev]; known {
				b.At, b.ByTime = t, true
			}
		}
	})
	if !ok && slices.ContainsFunc(s.Labels, func(l string) bool {
		stage, staged := state.ParseLabel(l)
		return staged && stage != state.Intake
	}) {
		return discBound{}, true
	}
	return b, ok
}

// discRecords returns the trigger keys s's markers record, each with the
// ID of the first comment holding one: the event of a discussion hold's
// transition marker, and the key of a notice marker. Markers are read as
// state.Derive reads them (discMarkers).
func discRecords(s *github.Snapshot, appSlug string, dirty func(int64) bool) map[string]int64 {
	keys := map[string]int64{}
	discMarkers(s, appSlug, dirty, func(id int64, _ time.Time, m github.Marker) {
		key := ""
		switch {
		case m.Kind == "notice":
			key = m.Fields["key"]
		case m.Kind == "transition" && m.Fields["reason"] == "discussion":
			key = m.Fields["event"]
		}
		if _, seen := keys[key]; key != "" && !seen {
			keys[key] = id
		}
	})
	return keys
}

// discWho asks trust about each login once: whether they're trusted, and
// whether they're a write collaborator who isn't (github.DiscTrust). A
// Trust that isn't a DiscTrust knows no such collaborator. No bot is either,
// nor an outsider: someone every association the snapshot gives says has
// no access (github.DiscOutsider), so a stranger's comment costs no
// permission request, which GitHub would answer with a 404 on every pass.
type discWho struct {
	trust    github.Trust
	access   map[string][2]bool // by login in lower case
	outsider map[string]bool    // by login in lower case
}

// discWhoOf is a discWho over trust for s, knowing the associations of its
// author and of its comments' authors.
func discWhoOf(s *github.Snapshot, trust github.Trust) discWho {
	w := discWho{trust: trust, access: map[string][2]bool{}, outsider: map[string]bool{}}
	note := func(login, association string) {
		l := strings.ToLower(login)
		if was, ok := w.outsider[l]; !ok || was {
			w.outsider[l] = github.DiscOutsider(association)
		}
	}
	note(s.Author.Login, s.AuthorAssociation)
	for _, c := range s.Comments {
		note(c.Author.Login, c.Association)
	}
	return w
}

func (w discWho) ask(ctx context.Context, a github.Author) (trusted, writer bool, err error) {
	l := strings.ToLower(a.Login)
	if a.Login == "" || a.Login == "unknown" || a.Type == "Bot" || w.trust == nil || w.outsider[l] {
		return false, false, nil
	}
	if v, ok := w.access[l]; ok {
		return v[0], v[1], nil
	}
	if dt, ok := w.trust.(github.DiscTrust); ok {
		trusted, writer, err = dt.DiscAccess(ctx, a.Login)
	} else {
		trusted, err = w.trust.Trusted(ctx, a.Login)
	}
	if err != nil {
		return false, false, fmt.Errorf("access of %s: %w", a.Login, err)
	}
	w.access[l] = [2]bool{trusted, writer}
	return trusted, writer, nil
}

// discKnowsWriters reports whether tr can name a write collaborator who
// isn't trusted: it's a github.DiscTrust, or the visit's or the current
// trust over one. With any other, nothing holds an issue, so discFind asks
// it about nobody.
func discKnowsWriters(tr github.Trust) bool {
	switch t := tr.(type) {
	case *trustOnceVisit:
		return discKnowsWriters(t.trust)
	case currentTrust:
		return discKnowsWriters(t.st.Trust)
	case github.DiscTrust:
		return true
	}
	return false
}

// writer reports whether a is a write collaborator who isn't trusted.
func (w discWho) writer(ctx context.Context, a github.Author) (bool, error) {
	_, writer, err := w.ask(ctx, a)
	return writer, err
}

// trusted reports whether a is trusted.
func (w discWho) trusted(ctx context.Context, a github.Author) (bool, error) {
	trusted, _, err := w.ask(ctx, a)
	return trusted, err
}

// discOpen reports whether an act keyed key, made at at, is open after the
// bound b: recorded by a marker in b's comment or a later one, or, with no
// record, made at or after b's time, failing closed in its second, or for
// a comment id, unless b is by time, above b's comment.
type discOpen func(key string, at time.Time, id int64) bool

func discOpenAfter(b discBound, records map[string]int64) discOpen {
	return func(key string, at time.Time, id int64) bool {
		if r, recorded := records[key]; recorded {
			return r >= b.ID
		}
		if id != 0 && !b.ByTime {
			return id > b.ID
		}
		return !at.Before(b.At)
	}
}

// discFind returns the open discussion triggers on s, oldest first: each
// act after its floor (discFloor, discOpen) by a write collaborator who
// isn't trusted, as trust says (#87's assumptions 1 and 2):
//   - a comment of theirs (discComment);
//   - their edit of content the App or a trusted user wrote, which made it
//     dirty, d says by them, at their newest edit of it (discEdits);
//   - their edit of their own comment at or after a trusted 👍 on it,
//     which approved it (discOwnEdits);
//   - their deletion of a comment by the App, a trusted user or someone
//     GitHub doesn't name, from s.Deleted, which fails closed for the
//     last. Their deletion of their own comment, or of another untrusted
//     user's, changes nothing anyone trusted wrote, so it's none;
//   - their rename of the issue, from s.DiscRenames, which CheckLabels set
//     from the timeline it reads, so this costs no request; it acts on
//     none of them itself. A snapshot CheckLabels hasn't checked has none.
//
// It asks trust only about the logins of such acts, and reads the
// reactions only of a writer's comment they edited after the floor.
func (a *Applier) discFind(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust) ([]discTrigger, error) {
	wrap := func(err error) error {
		return fmt.Errorf("discussion triggers on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	b, ok := discFloor(s, a.AppSlug, d.Has)
	if !ok || !discKnowsWriters(trust) {
		return nil, nil
	}
	open, who := discOpenAfter(b, discRecords(s, a.AppSlug, d.Has)), discWhoOf(s, trust)
	var found []discTrigger
	for _, c := range s.Comments {
		key := "c" + strconv.FormatInt(c.ID, 10)
		if !open(key, c.CreatedAt, c.ID) {
			continue
		}
		w, err := who.writer(ctx, c.Author)
		if err != nil {
			return nil, wrap(err)
		}
		if w {
			found = append(found, discTrigger{Kind: discComment, Key: key, By: c.Author.Login, At: c.CreatedAt, ID: c.ID})
		}
	}
	edits, err := a.discEdits(ctx, s, d, who, open)
	if err != nil {
		return nil, wrap(err)
	}
	own, err := a.discOwnEdits(ctx, s, d, who, open)
	if err != nil {
		return nil, wrap(err)
	}
	found = append(append(found, edits...), own...)
	for _, e := range s.Deleted {
		if !open("d"+e.ID, e.At, 0) || strings.EqualFold(e.By.Login, e.Author.Login) {
			continue
		}
		w, err := who.writer(ctx, e.By)
		if err != nil || !w {
			if err != nil {
				return nil, wrap(err)
			}
			continue
		}
		trustedOf := e.Author.Login == "" || state.IsApp(e.Author, a.AppSlug)
		if !trustedOf {
			if trustedOf, err = who.trusted(ctx, e.Author); err != nil {
				return nil, wrap(err)
			}
		}
		if trustedOf {
			found = append(found, discTrigger{Kind: discDeletion, Key: "d" + e.ID, By: e.By.Login, At: e.At, Of: e.Author.Login})
		}
	}
	for _, e := range s.DiscRenames {
		key := strconv.FormatInt(e.ID, 10)
		if !open(key, e.At, 0) {
			continue
		}
		w, err := who.writer(ctx, e.By)
		if err != nil {
			return nil, wrap(err)
		}
		if w {
			found = append(found, discTrigger{Kind: discRename, Key: key, By: e.By.Login, At: e.At, Title: e.Title})
		}
	}
	// A key a marker can't hold, as from an event ID GitHub changed the
	// shape of, could hold nothing, and would fail every pass.
	found = slices.DeleteFunc(found, func(tr discTrigger) bool {
		if markerValue(tr.Key) {
			return false
		}
		logr.Warnf("%s/%s#%d: skipping the discussion trigger keyed %q, which a marker can't hold", a.Owner, a.Repo, s.Number, tr.Key)
		return true
	})
	slices.SortStableFunc(found, func(x, y discTrigger) int { return x.At.Compare(y.At) })
	return found, nil
}

// discEditKey is the key of an edit at at of content id.
func discEditKey(id int64, at time.Time) string { return fmt.Sprintf("e%d-%d", id, at.Unix()) }

// discEdits returns the edit triggers among s's dirty content, d: content
// someone made dirty (Dirty), who is a write collaborator who isn't
// trusted, at their newest edit of it, if open says it counts, when the
// content is the App's or a trusted user's as it stood before that edit.
// The body is the App's after the App rewrote it whole
// (state.AuthorEditWriter), so the issue author's own edit after that is
// one (#87, Q4a). Content they wrote themselves, as when they typed a note
// into their own comment, is none.
//
// The App's note on dirty content (MarkDirty) is the content's last edit
// from then on, so the edit's time comes from the history, always, so the
// key is the same before the note and after it. The Applier keeps the
// history (Dirty), so it's read once for each change of the content: an
// unchanged pass reads none. An edit the history no longer has, past its
// 99 newest, is skipped, logged: it was handled, or nothing can key it.
func (a *Applier) discEdits(ctx context.Context, s *github.Snapshot, d Dirt, who discWho, open discOpen) ([]discTrigger, error) {
	type cand struct {
		c  content
		by github.Author
	}
	var found []discTrigger
	var need []cand
	for _, c := range contents(s) {
		login, dirty := d[c.id]
		by := github.Author{Login: login}
		if !dirty {
			continue
		}
		if w, err := who.writer(ctx, by); err != nil || !w {
			if err != nil {
				return nil, fmt.Errorf("who edited %s %d: %w", what(c.id), c.id, err)
			}
			continue
		}
		need = append(need, cand{c, by})
	}
	if len(need) == 0 {
		return found, nil
	}
	if a.histories == nil {
		a.histories = histories{}
	}
	var cs []content
	for _, n := range need {
		cs = append(cs, n.c)
	}
	history, err := a.histories.read(ctx, a.Client, cs)
	if err != nil {
		return nil, fmt.Errorf("edit times: %w", err)
	}
	for _, n := range need {
		h := history[n.c.node]
		if len(h) >= 2 && !h[len(h)-1].At.Before(n.c.edit.At) {
			a.histories[n.c.node] = kept{at: n.c.edit.At, body: n.c.body, edits: h}
		}
		i := len(h) - 1
		for i >= 0 && !strings.EqualFold(h[i].By.Login, n.by.Login) {
			i--
		}
		if i < 0 {
			logr.Infof("%s/%s#%d: %s %d is dirty by %s, but its history has no edit of theirs, so it's no discussion trigger",
				a.Owner, a.Repo, s.Number, what(n.c.id), n.c.id, n.by.Login)
			continue
		}
		at, of := h[i].At, state.AuthorEditWriter(n.c.id, n.c.author, h[:i], a.AppSlug)
		key := discEditKey(n.c.id, at)
		if !open(key, at, 0) || strings.EqualFold(of.Login, n.by.Login) {
			continue
		}
		ok := state.IsApp(of, a.AppSlug)
		if !ok {
			if ok, err = who.trusted(ctx, of); err != nil {
				return nil, fmt.Errorf("the writer of %s %d: %w", what(n.c.id), n.c.id, err)
			}
		}
		if ok {
			found = append(found, discTrigger{Kind: discEdit, Key: key, By: n.by.Login, At: at, ID: n.c.id, Of: of.Login})
		}
	}
	return found, nil
}

// discOwnEdits returns the triggers of a write collaborator who isn't
// trusted editing their own clean comment at or after a trusted 👍 on it,
// which approved it (#87's assumption 6), if open says the edit counts.
// GitHub dates only the last edit, so it's that one's. It reads the
// reactions only of a comment such a writer edited last, when that counts:
// the reads are conditional, so an unchanged pass costs no rate limit.
func (a *Applier) discOwnEdits(ctx context.Context, s *github.Snapshot, d Dirt, who discWho, open discOpen) ([]discTrigger, error) {
	var found []discTrigger
	for _, c := range s.Comments {
		if d.Has(c.ID) || c.Edit.At.IsZero() || !strings.EqualFold(c.Edit.By.Login, c.Author.Login) {
			continue
		}
		key := discEditKey(c.ID, c.Edit.At)
		if !open(key, c.Edit.At, 0) {
			continue
		}
		w, err := who.writer(ctx, c.Author)
		if err != nil {
			return nil, fmt.Errorf("who wrote comment %d: %w", c.ID, err)
		}
		if !w {
			continue
		}
		reactions, err := a.Client.Reactions(ctx, a.Owner, a.Repo, c.ID)
		if err != nil {
			return nil, fmt.Errorf("approvals of comment %d: %w", c.ID, err)
		}
		for _, r := range reactions {
			if r.Content != "+1" || c.Edit.At.Before(r.CreatedAt) {
				continue
			}
			t, err := who.trusted(ctx, r.User)
			if err != nil {
				return nil, fmt.Errorf("approvals of comment %d: %w", c.ID, err)
			}
			if t {
				found = append(found, discTrigger{Kind: discOwnEdit, Key: key, By: c.Author.Login, At: c.Edit.At, ID: c.ID})
				break
			}
		}
	}
	return found, nil
}

// discLost reports whether a notice for tr may have been posted and then
// deleted or made dirty, so its marker is gone: an App comment posted at
// or after tr is dirty, or one by the App, or by someone GitHub doesn't
// name, was deleted at or after it. Then no notice is posted again, so no
// key gets a second one; the trigger stays open, and T4's /resume on the
// issue accepts it (#87's assumption 4). The trigger's own content, an App
// comment it edited or deleted, is no notice of it.
func discLost(s *github.Snapshot, d Dirt, tr discTrigger, appSlug string) bool {
	for _, c := range s.Comments {
		own := tr.Kind == discEdit && c.ID == tr.ID
		if !own && state.IsApp(c.Author, appSlug) && !c.CreatedAt.Before(tr.At) && d.Has(c.ID) {
			return true
		}
	}
	return slices.ContainsFunc(s.Deleted, func(e github.DeletedComment) bool {
		own := tr.Kind == discDeletion && "d"+e.ID == tr.Key
		return !own && (e.Author.Login == "" || state.IsApp(e.Author, appSlug)) && !e.At.Before(tr.At)
	})
}

// discOrigin is the origin of a discussion hold, from stage, on an issue
// in state is: where it was before its first discussion hold (SPEC §6.4).
// In the clarifying after a hold, every marker carries the origin forward
// (T4 and T5 write it there), so it's the latest transition's; anywhere
// else, and with none, it's stage, as for a first hold.
func discOrigin(is state.Issue, stage state.Stage) state.Stage {
	for i := len(is.Markers) - 1; i >= 0; i-- {
		m := is.Markers[i]
		if _, bounds := state.RoundBound(m, 0); !bounds {
			continue
		}
		if o := state.Stage(m.Fields["origin"]); stage == state.Clarifying && o.Open() && o != state.Held {
			return o
		}
		break
	}
	return stage
}

// discHitsApp reports whether tr is an edit or a deletion of the App's
// content, or of a comment GitHub names no author of, which may have held
// the markers the issue's stage is read from.
func discHitsApp(tr discTrigger, appSlug string) bool {
	app := strings.EqualFold(tr.Of, appSlug+"[bot]")
	return tr.Kind == discEdit && app || tr.Kind == discDeletion && (app || tr.Of == "")
}

// discHoldMove is the discussion hold for tr on s, at at with origin,
// from from if set (Move.DiscFrom), with the round bound seen (#87's
// assumption 7), the sentence also, if any, after its own, and more
// markers after its own. A hold for a trigger that isn't a comment is also
// its notice, carrying its notice marker, unless a marker records tr or it
// may have had a notice already (discLost). The hold's comment ends in cc
// @<notify> (assumption 5).
func (a *Applier) discHoldMove(s *github.Snapshot, d Dirt, tr discTrigger, from, at, origin state.Stage, seen int64, also string, more ...github.Marker) Move {
	fields := map[string]string{"at": string(at), "origin": string(origin), "reason": "discussion"}
	text := fmt.Sprintf("On hold for discussion, from `%s`: %s. Only a code owner can move this issue, "+
		"so it stays on hold until they resolve this.", at, discWhat(tr))
	if also != "" {
		text += " " + also
	}
	m := Move{To: state.Held, Trigger: state.TriggerDiscussionHold, Key: state.Key{Field: "event", ID: tr.Key},
		Fields: state.HeldStackFields(fields, seen), DiscFrom: from, Markers: more, Text: discCC(text, a.Notify)}
	if _, recorded := discRecords(s, a.AppSlug, d.Has)[tr.Key]; tr.Kind != discComment && !recorded && !discLost(s, d, tr, a.AppSlug) {
		m.Markers = append(m.Markers, github.Marker{Kind: "notice", Fields: map[string]string{"key": tr.Key}})
	}
	return m
}

// discRestore holds s, before the label rules act on events (the
// disagreement or override CheckLabels found), when a write collaborator's
// edit or deletion of the App's content (discHitsApp) hid the markers that
// set its stage. It reports whether it held s, so the events are left
// alone: the hold sets the labels. It holds nothing when an override
// stands, as the owner's change wins, nor when the App's own write was cut
// short (Unfinished), which CheckLabels finishes.
//
// The stage before the act is the one stage label on s, which the label
// rules have just judged: an untrusted change of it is reverted already,
// and the App set it with its last move, so it shows the stage the hidden
// markers set. That is so for an edit and a deletion alike: a deleted
// comment's text is gone, and an edited one's old text is in its history
// only while nobody deletes the revision, and is never read for state
// (SPEC §5.1). The markers left, in clean content, show where the issue
// was before the hidden moves. The hold, keyed by the oldest open such act
// that no marker records:
//   - from a stage that isn't held, is at that stage, from it
//     (Move.DiscFrom), with its origin as a first hold's (discOrigin);
//   - from held, is at the stage the markers left show, the one before the
//     hidden move into held, and also records the hidden hold again as an
//     owner hold with no cmd, which only /resume on the issue itself ends
//     (heldStackEnds). Whether it was the owner's or a discussion's isn't
//     known, so it fails closed: an open trigger keeps the issue held as
//     before, and an owner hold made on an ancestor needs /resume here.
//
// Either way no label moves but held's, and no disagreement is posted.
// Markers in the edited or deleted content are never read again: the hold
// is the issue's state from then on. Nothing hides a stage that the label
// and the markers left agree on, so then this holds nothing, and step 1's
// discussion holds the issue as for any trigger.
func (a *Applier) discRestore(ctx context.Context, s *github.Snapshot, trust github.Trust, d Dirt, events []github.LabelEvent) (bool, error) {
	if slices.ContainsFunc(events, func(ev github.LabelEvent) bool { return ev.Kind == github.LabelOverride || ev.Unfinished }) {
		return false, nil
	}
	var stages []state.Stage
	for _, l := range s.Labels {
		if !isStageLabel(l) {
			continue
		}
		stage, ok := state.ParseLabel(l)
		if !ok {
			return false, nil
		}
		stages = append(stages, stage)
	}
	is := state.Derive(s, a.AppSlug, d.Has)
	if len(stages) != 1 || !stages[0].Open() || stages[0] == is.Stage {
		return false, nil
	}
	before := stages[0]
	found, err := a.discFind(ctx, s, d, trust)
	if err != nil {
		return false, fmt.Errorf("restore: %w", err)
	}
	records := discRecords(s, a.AppSlug, d.Has)
	i := slices.IndexFunc(found, func(tr discTrigger) bool {
		_, recorded := records[tr.Key]
		return discHitsApp(tr, a.AppSlug) && !recorded
	})
	if i < 0 {
		return false, nil
	}
	tr := found[i]
	m := a.discHoldMove(s, d, tr, before, before, discOrigin(is, before), is.Round, "")
	if before == state.Held {
		owner := state.HeldStackFields(map[string]string{"from": string(state.Held), "to": string(state.Held), "at": string(is.Stage),
			"reason": "owner", "event": tr.Key}, is.Round)
		m = a.discHoldMove(s, d, tr, before, is.Stage, discOrigin(is, is.Stage), is.Round,
			"It was on hold already, so it also stays on hold until `/resume` here.", github.Marker{Kind: "transition", Fields: owner})
	}
	err = a.Transition(ctx, s, d.Has, m)
	if StateError(err) {
		logr.Warnf("%s/%s#%d: no discussion hold restoring %s for %s: %v", a.Owner, a.Repo, s.Number, before, tr.Key, err)
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("restore %s: %w", before, err)
	}
	logr.Infof("%s/%s#%d: on hold for discussion at %s, which %s hid, for %s", a.Owner, a.Repo, s.Number, before, tr.By, tr.Key)
	return true, nil
}

// discussion is step 1's look for discussion triggers (SPEC §6.4, §8.1),
// after the type check and before the commands, and the check before a
// run's result is applied (§8.2). It returns the open triggers on s
// (discFind). If there are any:
//   - An issue that isn't held goes to held (discHoldMove), keyed by the
//     oldest that no transition marker records, at its stage, with its
//     origin (discOrigin) and the round's bound in seen. One that hid the
//     issue's stage was held by discRestore already, at the stage before.
//   - An issue held already gets no hold marker (Q2b): it stays held while
//     the trigger is open (discHeldStack).
//   - Each trigger that isn't a comment, and that no marker records
//     (discRecords), gets one notice: an App comment naming it, carrying
//     notice key=<its key> (#87's assumption 4), unless it may have had
//     one already (discLost).
//
// The hold's comment and each notice end in cc @<notify> (assumption 5).
// The writes go hold first, then the notices oldest first, each checked
// first, so a pass that crashed part way can run again. A move the table
// lacks, as for a type the pipeline doesn't know, is reported once by
// Transition, and then nothing more is written.
func (st *Step) discussion(ctx context.Context, s *github.Snapshot, d Dirt) ([]discTrigger, error) {
	wrap := func(err error) error {
		return fmt.Errorf("discussion hold of %s/%s#%d: %w", st.Owner, st.Repo, s.Number, err)
	}
	found, err := st.discFind(ctx, s, d, st.trustOnceTrust())
	if err != nil || len(found) == 0 {
		if err != nil {
			return nil, wrap(err)
		}
		return nil, nil
	}
	is := state.Derive(s, st.AppSlug, d.Has)
	if i := slices.IndexFunc(found, func(tr discTrigger) bool { return !is.Moved(state.Key{Field: "event", ID: tr.Key}) }); is.Stage != state.Held && i >= 0 {
		first := found[i]
		err := st.Transition(ctx, s, d.Has, st.discHoldMove(s, d, first, "", is.Stage, discOrigin(is, is.Stage), is.Round, ""))
		if StateError(err) {
			logr.Warnf("%s/%s#%d: no discussion hold for %s: %v", st.Owner, st.Repo, s.Number, first.Key, err)
			return found, nil
		}
		if err != nil {
			return nil, wrap(err)
		}
		logr.Infof("%s/%s#%d: on hold for discussion, from %s, for %s", st.Owner, st.Repo, s.Number, is.Stage, first.Key)
	}
	records := discRecords(s, st.AppSlug, d.Has)
	for _, tr := range found {
		if _, recorded := records[tr.Key]; tr.Kind == discComment || recorded {
			continue
		}
		if discLost(s, d, tr, st.AppSlug) {
			logr.Infof("%s/%s#%d: no notice for %s, which may have had one deleted or made dirty", st.Owner, st.Repo, s.Number, tr.Key)
			continue
		}
		marker, err := github.FormatMarker(github.Marker{Kind: "notice", Fields: map[string]string{"key": tr.Key}})
		if err != nil {
			return nil, wrap(err)
		}
		text := fmt.Sprintf("Also on hold for discussion: %s. This issue stays on hold until a code owner accepts it.", discWhat(tr))
		if err := st.comment(ctx, s, join(github.Escape(discCC(text, st.Notify)), marker)); err != nil {
			return nil, wrap(err)
		}
		logr.Infof("%s/%s#%d: posted the discussion notice for %s", st.Owner, st.Repo, s.Number, tr.Key)
	}
	return found, nil
}

// discCC is text, then its cc line for notify, if any (ccLines).
func discCC(text string, notify []string) string {
	return strings.Join(append([]string{text}, ccLines(notify)...), "\n\n")
}

// discHeldStack returns is, the state of s, with each open discussion
// trigger on s (discFind) that no hold marker records added to its
// HeldStack's Holds as a discussion hold, while s is held. A discussion
// hold on an issue held already writes no marker (Q2b), so this is how
// /resume knows of it: it ends only owner holds (heldStackEnds), so the
// issue stays held while one is open, as the criterion says. T4 makes
// /resume on the issue itself accept them. The renames are known only
// once CheckLabels has checked s, so an unchecked snapshot misses those,
// as one not yet visited misses any trigger: the next pass holds the
// issue for it again.
func (a *Applier) discHeldStack(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, is state.Issue) (state.Issue, error) {
	if is.Stage != state.Held {
		return is, nil
	}
	found, err := a.discFind(ctx, s, d, trust)
	if err != nil {
		return is, fmt.Errorf("held stack: %w", err)
	}
	held := map[string]bool{}
	for _, m := range is.HeldStack.Holds {
		if m.Fields["reason"] == "discussion" {
			held[m.Fields["event"]] = true
		}
	}
	holds := slices.Clone(is.HeldStack.Holds)
	for _, tr := range found {
		if !held[tr.Key] {
			holds = append(holds, github.Marker{Kind: "transition", Fields: map[string]string{"from": string(state.Held), "to": string(state.Held),
				"reason": "discussion", "event": tr.Key}})
		}
	}
	is.HeldStack.Holds = holds
	return is, nil
}

// discHeldLabel reports whether label is stage:held: such an issue is
// visited on every pass, as a 👍 that resolves a trigger (T4) changes no
// updated_at, and a deletion may not either (SPEC §16).
func discHeldLabel(label string) bool {
	stage, ok := state.ParseLabel(label)
	return ok && stage == state.Held
}
