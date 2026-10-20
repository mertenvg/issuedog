package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/trust"
	"github.com/mertenvg/logr/v2"
)

// Discussion holds (SPEC §6.4, plan #87 rev 2, T3). A write collaborator
// who isn't trusted can take part in an issue, but not move it, so what
// they do on an issue in the pipeline holds that issue, and only it: each
// such act is a trigger. A trigger is
//   - found by discFind, from the snapshot, its dirty content and the
//     renamed events CheckLabels read, each keyed by its event= key (#87's
//     assumption 3);
//   - open from then until something resolves it: a move out of held
//     (discFloor), such as a stage label set by hand, which ends every hold
//     (SPEC §6.4), or /resume on the issue; or, from T4 (#188, resolve.go),
//     a trusted 👍 or a counter comment, which leave it out of discFind's
//     result, and the record of a discussion that ended while an owner
//     hold stayed (discFloor);
//   - handled once a marker records its key: the hold's transition marker,
//     keyed event=<key> with reason=discussion, or a notice's marker,
//     notice key=<key> (discRecords). So a second pass writes nothing, and
//     no key gets a second notice. A recorded trigger is found from its
//     record from then on (discRecorded), whatever its content shows now.
//     A comment trigger on an issue held already has no record at all
//     (Q2b): it is known by being open.
//
// An edit or a deletion of the App's own content can hide the markers that
// set the issue's stage, since markers in such content are never read
// again (SPEC §5.1). discRestore holds such an issue at the stage it was
// at before, before the label rules would put the label back to the stage
// the markers left show.
//
// T6 (#190) adds closes as a kind of its own (closing.go). A close hides no
// marker, so discRestore has nothing to restore for one. T8 (#192) lists the open triggers of
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
	// FromRecord is set for a trigger built from its record alone
	// (discRecorded), as the act is no longer in the snapshot as it was
	// recorded: only Kind, Key, ID and At are known, and By, Of and Title
	// are "".
	FromRecord bool
}

// discWhat says what tr is, for its hold's comment or its notice. The
// logins and the title aren't the App's text, so the logins are code and
// defused, the title is defused and its tags escaped (inline), and the
// caller escapes the whole (github.Escape).
func discWhat(tr discTrigger) string {
	if tr.Kind == discClose {
		return closingWhat(tr)
	}
	if tr.FromRecord {
		return discWhatRecorded(tr)
	}
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

// editNoticeApproval is the sentence that follows what tr is (discWhat)
// in its hold's comment and its notice, when tr is a write collaborator's
// edit of their own comment after a code owner's 👍 approved it, and ""
// for any other trigger. A 👍 approves only the text it post-dates, so
// accepting the edit, by a 👍 on its notice or by /resume, releases the
// hold but doesn't approve the new text: a trusted 👍 after the edit does,
// and as GitHub keeps one 👍 a user, a code owner who approved the old
// text removes theirs and adds it again (SPEC §6.4). One known from its
// record alone is never an own edit (discFromKey makes it an edit), but
// says nothing either way. It's the App's text only. The digest says it
// shorter (editNoticeDigest).
func editNoticeApproval(tr discTrigger) string {
	if !editNoticeOwn(tr) {
		return ""
	}
	return fmt.Sprintf("Accepting the edit releases the hold but doesn't approve the new text: to approve it, %s.", editNoticeHow(tr))
}

// editNoticeDigest is editNoticeApproval for tr's line in the digest,
// after how it's accepted, which says the hold ends: only that accepting
// it doesn't approve the new text, and how to approve it.
func editNoticeDigest(tr discTrigger) string {
	if !editNoticeOwn(tr) {
		return ""
	}
	return fmt.Sprintf("Accepting it doesn't approve the new text: %s.", editNoticeHow(tr))
}

// editNoticeOwn reports whether tr is an own edit of an approved comment
// that the snapshot shows, which editNoticeApproval speaks of.
func editNoticeOwn(tr discTrigger) bool { return tr.Kind == discOwnEdit && !tr.FromRecord }

// editNoticeHow says how a code owner approves an own edit's new text.
func editNoticeHow(tr discTrigger) string {
	return fmt.Sprintf("remove your 👍 from comment %d and add it again", tr.ID)
}

// editNoticeAppend is text, then sentence, if any, after a space.
func editNoticeAppend(text, sentence string) string {
	if sentence != "" {
		return text + " " + sentence
	}
	return text
}

// discWhatRecorded says what tr, a trigger built from its record alone
// (discRecorded), is, from what its key tells: its kind, its comment or
// content, and an edit's time. Only an edit's time isn't fixed text, and
// it's the App's, from the key.
func discWhatRecorded(tr discTrigger) string {
	what := "an act on this issue"
	switch tr.Kind {
	case discComment:
		what = fmt.Sprintf("comment %d, which is gone now", tr.ID)
	case discEdit:
		of := fmt.Sprintf("comment %d", tr.ID)
		if tr.ID == state.BodyID {
			of = "the issue description"
		}
		what = fmt.Sprintf("an edit of %s at %s", of, tr.At.UTC().Format(time.RFC3339))
	case discDeletion:
		what = "a deletion of a comment"
	case discRename:
		what = "a rename of this issue"
	}
	return what + ", recorded earlier"
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
//     posted: an act between the label and its record still counts. So is
//     a first marker that records a stage label set by hand, as when the
//     owner brings an issue into the pipeline by label. With the event's
//     time unknown, as on a snapshot CheckLabels hasn't checked, it's the
//     record's.
//   - Any other move out of held, and the record of a discussion that
//     ended while an owner hold stayed, is bounded the same way by when
//     the act that resolved it happened, where that's known (resolveActAt,
//     T4): a later act still counts.
//
// The markers are read as state.Derive reads them (discMarkers). With none,
// ok is false, as on an issue that isn't in the pipeline, which nothing
// holds, but for one with a stage label past intake: its markers were
// hidden by an edit or a deletion of the App's content, which discRestore
// holds, so every act counts, failing closed.
func discFloor(s *github.Snapshot, appSlug string, dirty func(int64) bool) (b discBound, ok bool) {
	discMarkers(s, appSlug, dirty, func(id int64, at time.Time, m github.Marker) {
		if t, ended := resolveEndedAt(m); ended && ok {
			b = discBound{ID: id, At: t, ByTime: true}
			return
		}
		if _, bounds := state.RoundBound(m, id); !bounds {
			return
		}
		out := state.Stage(m.Fields["from"]) == state.Held && state.Stage(m.Fields["to"]) != state.Held
		if ok && !out {
			return
		}
		b, ok = discBound{ID: id, At: at}, true
		if ev, err := strconv.ParseInt(m.Fields["event"], 10, 64); m.Fields["by"] != "" && err == nil {
			if t, known := s.DiscLabelTimes[ev]; known {
				b.At, b.ByTime = t, true
			}
		} else if t, known := resolveActAt(s, m); out && known {
			b.At, b.ByTime = t, true
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
// Nor is a login GitHub had no permission record of on an earlier pass
// (discNoRecord), unless the snapshot shows them as a collaborator again.
//
// Only writer asks DiscAccess, and only about who made an act that is open
// and that no marker records (discFind): what a marker records was judged
// when it was made, so nobody is asked about it again. A question of trust
// alone goes to Trusted (trusted).
type discWho struct {
	a        *Applier
	number   int // the issue, for discNoRecord
	trust    github.Trust
	access   map[string][2]bool // by login in lower case
	verdicts map[string]bool    // Trusted's alone, by login in lower case
	outsider map[string]bool    // by login in lower case
	member   map[string]bool    // shown as the owner, a member or a collaborator, by login in lower case
}

// discWhoOf is a discWho over trust for s, knowing the associations of its
// author and of its comments' authors.
func (a *Applier) discWhoOf(s *github.Snapshot, trust github.Trust) discWho {
	w := discWho{a: a, number: s.Number, trust: trust, access: map[string][2]bool{}, verdicts: map[string]bool{},
		outsider: map[string]bool{}, member: map[string]bool{}}
	note := func(login, association string) {
		l := strings.ToLower(login)
		if was, ok := w.outsider[l]; !ok || was {
			w.outsider[l] = github.DiscOutsider(association)
		}
		switch association {
		case "OWNER", "MEMBER", "COLLABORATOR":
			w.member[l] = true
		}
	}
	note(s.Author.Login, s.AuthorAssociation)
	for _, c := range s.Comments {
		note(c.Author.Login, c.Association)
	}
	return w
}

// nobody reports whether a is surely neither trusted nor a writer, with no
// request: a bot, nobody GitHub names, an outsider (discWhoOf), anyone
// with no Trust, or a login GitHub had no permission record of lately
// (discNoRecordKnown), unless the snapshot shows them as a collaborator,
// which forgets that record.
func (w discWho) nobody(a github.Author) bool {
	l := strings.ToLower(a.Login)
	switch {
	case a.Login == "" || a.Login == "unknown" || a.Type == "Bot" || w.trust == nil || w.outsider[l]:
		return true
	case w.a == nil:
		return false
	case w.member[l]:
		w.a.discNoRecordForget(l)
		return false
	}
	return w.a.discNoRecordKnown(l, w.number)
}

func (w discWho) ask(ctx context.Context, a github.Author) (trusted, writer bool, err error) {
	l := strings.ToLower(a.Login)
	if w.nobody(a) {
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
	if errors.Is(err, trust.ErrNoRecord) {
		if w.a != nil {
			w.a.discNoRecordNote(l, w.number)
		}
		trusted, writer, err = false, false, nil
	}
	if err != nil {
		return false, false, fmt.Errorf("access of %s: %w", a.Login, err)
	}
	w.access[l] = [2]bool{trusted, writer}
	return trusted, writer, nil
}

// discKnowsWriters reports whether tr can name a write collaborator who
// isn't trusted: it's a github.DiscTrust, or the visit's or the current
// trust over one, and not one that says nobody can be (DiscNoWriters), as
// with no CODEOWNERS file. With any other, nothing holds an issue, so
// discFind asks it about nobody.
func discKnowsWriters(tr github.Trust) bool {
	switch t := tr.(type) {
	case *trustOnceVisit:
		return discKnowsWriters(t.trust)
	case currentTrust:
		return discKnowsWriters(t.st.Trust)
	case github.DiscTrust:
		none, ok := t.(interface{ DiscNoWriters() bool })
		return !ok || !none.DiscNoWriters()
	}
	return false
}

// writer reports whether a is a write collaborator who isn't trusted.
func (w discWho) writer(ctx context.Context, a github.Author) (bool, error) {
	_, writer, err := w.ask(ctx, a)
	return writer, err
}

// trusted reports whether a is trusted.
// It asks only Trusted, which needs no request for a login CODEOWNERS
// doesn't name, unless ask already has both answers: a deleted comment's
// author or a reactor has no association in the snapshot, so DiscAccess
// would ask GitHub about them on every pass, for a 404 no ETag saves.
func (w discWho) trusted(ctx context.Context, a github.Author) (bool, error) {
	l := strings.ToLower(a.Login)
	if w.nobody(a) {
		return false, nil
	}
	if v, ok := w.access[l]; ok {
		return v[0], nil
	}
	if v, ok := w.verdicts[l]; ok {
		return v, nil
	}
	v, err := w.trust.Trusted(ctx, a.Login)
	if err != nil {
		return false, fmt.Errorf("trust of %s: %w", a.Login, err)
	}
	w.verdicts[l] = v
	return v, nil
}

// discNoRecordWait is how long a login GitHub had no permission record of
// is remembered as such (discNoRecord). Within it, a removed collaborator
// who is added back holds nothing by an act the snapshot shows no
// association for, a rename, a deletion or an edit of another's content;
// their comment shows them as a collaborator, which ends it at once.
const discNoRecordWait = time.Hour

// discNoRecord is a login GitHub answered with no permission record (a
// 404, trust.ErrNoRecord), such as a removed collaborator or a deleted
// account, which no ETag saves. It's remembered, in memory, so an
// unchanged pass doesn't ask again (SPEC §8.1). It ends:
//   - after discNoRecordWait, when the issues it was asked for are read
//     again (discNoRecordDue), so an act a re-added writer made meanwhile
//     is judged then, on any issue, visited on every pass or not: the
//     window is bounded. The cost, each discNoRecordWait, is three
//     requests no ETag saves: for each issue, the snapshot's GraphQL
//     query and its parent's GET, and for each login, the permission
//     404. It goes on for as long as an open act of theirs is on an issue
//     visited: on one that isn't held, nothing moves the floor past the
//     act;
//   - when a snapshot shows the login as the owner, a member or a
//     collaborator (discWho.nobody), as a re-added writer's comment does;
//   - on a restart, with the rest of the daemon's memory.
//
// A trust reload doesn't end it: trust reloads on every pass, and a
// permission record isn't in CODEOWNERS.
type discNoRecord struct {
	at     time.Time    // when GitHub first answered so
	issues map[int]bool // the issues it was asked for
}

// discNow is the time on GitHub's clock, or the local one with no client.
func (a *Applier) discNow() time.Time {
	if a.Client == nil {
		return time.Now()
	}
	return a.Client.Now()
}

// discNoRecordKnown reports whether login, in lower case, had no
// permission record within discNoRecordWait, and if so adds issue n to
// those read again when it ends.
func (a *Applier) discNoRecordKnown(login string, n int) bool {
	r, ok := a.discNoRecords[login]
	if !ok || a.discNow().Sub(r.at) >= discNoRecordWait {
		return false
	}
	r.issues[n] = true
	return true
}

// discNoRecordNote remembers that login, in lower case, had no permission
// record when asked for issue n.
func (a *Applier) discNoRecordNote(login string, n int) {
	if a.discNoRecords == nil {
		a.discNoRecords = map[string]*discNoRecord{}
	}
	now := a.discNow()
	r, ok := a.discNoRecords[login]
	if !ok {
		r = &discNoRecord{issues: map[int]bool{}}
		a.discNoRecords[login] = r
	}
	if !ok || now.Sub(r.at) >= discNoRecordWait {
		r.at = now // asked again once its wait was up: the wait starts again
	}
	r.issues[n] = true
	logr.Infof("%s/%s: %s has no permission record, so they aren't asked about for %s", a.Owner, a.Repo, login, discNoRecordWait)
}

// discNoRecordForget forgets that login, in lower case, had no permission
// record.
func (a *Applier) discNoRecordForget(login string) { delete(a.discNoRecords, login) }

// discNoRecordDue forgets each login whose discNoRecordWait is up by now,
// and returns the issues it was asked for, sorted, to read again.
func (a *Applier) discNoRecordDue(now time.Time) []int {
	due := map[int]bool{}
	for l, r := range a.discNoRecords {
		if now.Sub(r.at) >= discNoRecordWait {
			maps.Copy(due, r.issues)
			delete(a.discNoRecords, l)
		}
	}
	return slices.Sorted(maps.Keys(due))
}

// discOpen reports whether an act keyed key, made at at, is open after the
// bound b, and whether a marker records it: recorded by a marker in b's
// comment or a later one, or, with no record, made at or after b's time,
// failing closed in its second, or for a comment id, unless b is by time,
// above b's comment. A recorded act is open too when b is by time and the
// act, its time known, is at or after b's time (T4): a record written
// after the act that resolved the triggers, as a notice posted in the
// pass that applies a /resume posted before the act, doesn't close it. An
// act known from its record alone, at the zero time, is judged by its
// record (discRecorded), but for an edit, whose key gives its time.
type discOpen func(key string, at time.Time, id int64) (open, recorded bool)

func discOpenAfter(b discBound, records map[string]int64) discOpen {
	return func(key string, at time.Time, id int64) (bool, bool) {
		if r, recorded := records[key]; recorded {
			return r >= b.ID || b.ByTime && !at.Before(b.At), true
		}
		if id != 0 && !b.ByTime {
			return id > b.ID, false
		}
		return !at.Before(b.At), false
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
//     none of them itself. A snapshot CheckLabels hasn't checked has none;
//   - their close of the issue, from s.Closings, read the same way, which
//     the App reverted when it judged the close (closingTriggers).
//
// An open act a marker records is one, with no question: it was judged
// when the marker was written. Each such key is a trigger from its record,
// whatever the snapshot shows now (discRecorded): a later edit, another
// writer's or a bot's, a deletion, or the editor's loss of access changes
// the content or the answers the passes above rebuild a trigger from, but
// not the record. Each pass above still finds a recorded act it sees as it
// was, with what the snapshot says of it.
//
// Of the rest, it asks about each open act only, and only once nothing
// else rules it out, so an act before the floor, or by a login known to be
// nobody (discWho.nobody), costs no request. It reads the reactions only
// of a writer's comment they edited after the floor.
//
// A trigger resolved by a trusted 👍 or a counter comment (resolver, T4)
// is left out: a comment is judged so before anyone is asked whether its
// author is a writer, so a resolved one costs no question.
func (a *Applier) discFind(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust) ([]discTrigger, error) {
	return a.discFindResolved(ctx, s, d, trust, nil)
}

// discFindResolved is discFind, which also sets *out, if out isn't nil, to
// the resolver that left out the triggers it found resolved, and knows
// what resolved them (resolver.acts): one with none when it finds none.
func (a *Applier) discFindResolved(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, out **resolver) ([]discTrigger, error) {
	wrap := func(err error) error {
		return fmt.Errorf("discussion triggers on %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
	}
	if out != nil {
		*out = &resolver{}
	}
	b, ok := discFloor(s, a.AppSlug, d.Has)
	if !ok || !discKnowsWriters(trust) {
		return nil, nil
	}
	records := discRecords(s, a.AppSlug, d.Has)
	open, who := discOpenAfter(b, records), a.discWhoOf(s, trust)
	rs := a.resolverOf(s, d, who, records)
	if out != nil {
		*out = rs
	}
	// counts reports whether an act by by, keyed key, is a trigger: open,
	// and recorded or by a writer.
	counts := func(key string, at time.Time, id int64, by github.Author) (bool, error) {
		o, recorded := open(key, at, id)
		if !o || recorded {
			return o, nil
		}
		return who.writer(ctx, by)
	}
	var found []discTrigger
	for _, c := range s.Comments {
		key := "c" + strconv.FormatInt(c.ID, 10)
		tr := discTrigger{Kind: discComment, Key: key, By: c.Author.Login, At: c.CreatedAt, ID: c.ID}
		if o, recorded := open(key, c.CreatedAt, c.ID); o {
			// Resolved, or surely no trigger, before the writer question.
			skip, err := rs.early(ctx, tr, recorded, c.Author)
			if err != nil {
				return nil, wrap(err)
			}
			if skip {
				continue
			}
		}
		ok, err := counts(key, c.CreatedAt, c.ID, c.Author)
		if err != nil {
			return nil, wrap(err)
		}
		if ok {
			found = append(found, tr)
		}
	}
	edits, err := a.discEdits(ctx, s, d, who, b, open)
	if err != nil {
		return nil, wrap(err)
	}
	own, err := a.discOwnEdits(ctx, s, d, who, open)
	if err != nil {
		return nil, wrap(err)
	}
	found = append(append(found, edits...), own...)
	for _, e := range s.Deleted {
		key := "d" + e.ID
		o, recorded := open(key, e.At, 0)
		if !o || !recorded && strings.EqualFold(e.By.Login, e.Author.Login) {
			continue
		}
		if !recorded {
			w, err := who.writer(ctx, e.By)
			if err != nil {
				return nil, wrap(err)
			}
			trustedOf := e.Author.Login == "" || state.IsApp(e.Author, a.AppSlug)
			if w && !trustedOf {
				if trustedOf, err = who.trusted(ctx, e.Author); err != nil {
					return nil, wrap(err)
				}
			}
			if !w || !trustedOf {
				continue
			}
		}
		found = append(found, discTrigger{Kind: discDeletion, Key: key, By: e.By.Login, At: e.At, Of: e.Author.Login})
	}
	for _, e := range s.DiscRenames {
		key := strconv.FormatInt(e.ID, 10)
		ok, err := counts(key, e.At, 0, e.By)
		if err != nil {
			return nil, wrap(err)
		}
		if ok {
			found = append(found, discTrigger{Kind: discRename, Key: key, By: e.By.Login, At: e.At, Title: e.Title})
		}
	}
	closes, err := a.closingTriggers(ctx, s, open, who) // T6 (closing.go)
	if err != nil {
		return nil, wrap(err)
	}
	found = append(found, closes...)
	found = append(found, discRecorded(s, records, open, found)...)
	// A key a marker can't hold, as from an event ID GitHub changed the
	// shape of, could hold nothing, and would fail every pass.
	found = slices.DeleteFunc(found, func(tr discTrigger) bool {
		if markerValue(tr.Key) {
			return false
		}
		logr.Warnf("%s/%s#%d: skipping the discussion trigger keyed %q, which a marker can't hold", a.Owner, a.Repo, s.Number, tr.Key)
		return true
	})
	// Resolved, by a 👍 on a notice, or a comment by a counter comment
	// or a 👍 judged above already (resolver.drop, T4).
	if found, err = rs.drop(ctx, found); err != nil {
		return nil, wrap(err)
	}
	slices.SortStableFunc(found, func(x, y discTrigger) int { return x.At.Compare(y.At) })
	rs.open = found
	return found, nil
}

// discEditKey is the key of an edit at at of content id.
func discEditKey(id int64, at time.Time) string { return fmt.Sprintf("e%d-%d", id, at.Unix()) }

// discRecorded returns a trigger for each key records holds that open says
// is open, that found, the triggers rebuilt from the snapshot, lacks
// (discFind), in key order. Each is built from its record and its
// key alone (discFromKey), with no question and no read: it was judged
// when its marker was written, and what the snapshot shows now can't undo
// that. So a recorded edit counts after a later edit of the same content,
// by its writer, another writer or a bot, after the content is deleted,
// and after its editor loses access; a recorded comment after it's
// deleted; and a recorded deletion or rename when the snapshot no longer
// lists it, as one CheckLabels hasn't checked has no renames.
//
// Two notices can be open on the same content at once, as when a writer
// edits a comment, gets its notice, and edits it again: each edit is a
// trigger of its own, keyed by its time, with a notice of its own, and
// only the newest is the content as it is now. Each is resolved by its own
// notice's 👍, or by /resume on the issue: a 👍 on the newer notice
// doesn't accept the older edit (resolver.notice, T4).
func discRecorded(s *github.Snapshot, records map[string]int64, open discOpen, found []discTrigger) []discTrigger {
	have := map[string]bool{}
	for _, tr := range found {
		have[tr.Key] = true
	}
	when := map[int64]time.Time{state.BodyID: s.CreatedAt} // when each comment was posted, the body as the issue
	for _, c := range s.Comments {
		when[c.ID] = c.CreatedAt
	}
	var more []discTrigger
	for _, key := range slices.Sorted(maps.Keys(records)) {
		// Each key here is recorded, so open judges it by its record, and
		// an edit by its time too, which its key gives.
		tr, at := discFromKey(key, when[records[key]]), time.Time{}
		if tr.Kind == discEdit {
			at = tr.At
		}
		if o, _ := open(key, at, 0); have[key] || !o {
			continue
		}
		more = append(more, closingFromKey(s, tr))
	}
	return more
}

// discFromKey is the trigger keyed key, as its key tells it, recorded at
// recordedAt (discRecorded): its kind by its shape (#87's assumption 3),
// with c<comment id> a comment, e<id>-<edit time> an edit, its content and
// time in it, d<event node id> a deletion, and a timeline event ID a
// rename. An edit of either kind is a discEdit, as the key doesn't say
// whose content it changed, and nothing that acts on a recorded trigger
// needs to know. Any other kind is at recordedAt, as is a key of a shape
// none of these has, which is still a trigger, failing closed: a marker
// the App wrote recorded it.
//
// A close is keyed by its timeline event ID too, so a recorded one is a
// rename here; discRecorded tells the two apart by the timeline
// (closingFromKey).
func discFromKey(key string, recordedAt time.Time) discTrigger {
	tr := discTrigger{Key: key, At: recordedAt, FromRecord: true}
	switch {
	case strings.HasPrefix(key, "c"):
		if id, err := strconv.ParseInt(key[1:], 10, 64); err == nil {
			tr.Kind, tr.ID = discComment, id
		}
	case strings.HasPrefix(key, "e"):
		id, at, _ := strings.Cut(key[1:], "-") // with no "-", at is "", which doesn't parse
		i, err := strconv.ParseInt(id, 10, 64)
		t, terr := strconv.ParseInt(at, 10, 64)
		if err == nil && terr == nil {
			tr.Kind, tr.ID, tr.At = discEdit, i, time.Unix(t, 0).UTC()
		}
	case strings.HasPrefix(key, "d") && len(key) > 1:
		tr.Kind = discDeletion
	default:
		if _, err := strconv.ParseInt(key, 10, 64); err == nil {
			tr.Kind = discRename
		}
	}
	return tr
}

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
//
// Nothing is read or asked for content last edited before the floor b, as
// the snapshot dates its last edit, since an edit of it is older still;
// nor for content an editor known to be nobody made dirty (discWho.nobody).
// An edit a marker records is one, with no question, and with no read
// either: where this doesn't find it, discRecorded does, as when the
// content changed since, or the edit is from before b, as one the hold
// discRestore writes can be, from before that hold's own marker. Of the
// others, only the open ones' editors are asked about.
func (a *Applier) discEdits(ctx context.Context, s *github.Snapshot, d Dirt, who discWho, b discBound, open discOpen) ([]discTrigger, error) {
	type cand struct {
		c  content
		by github.Author
	}
	var found []discTrigger
	var need []cand
	for _, c := range contents(s) {
		login, dirty := d[c.id]
		by := github.Author{Login: login}
		if !dirty || c.edit.At.Before(b.At) || who.nobody(by) {
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
		o, recorded := open(key, at, 0)
		if !o || !recorded && strings.EqualFold(of.Login, n.by.Login) {
			continue
		}
		if !recorded {
			w, err := who.writer(ctx, n.by)
			if err != nil {
				return nil, fmt.Errorf("who edited %s %d: %w", what(n.c.id), n.c.id, err)
			}
			ok := w && state.IsApp(of, a.AppSlug)
			if w && !ok {
				if ok, err = who.trusted(ctx, of); err != nil {
					return nil, fmt.Errorf("the writer of %s %d: %w", what(n.c.id), n.c.id, err)
				}
			}
			if !ok {
				continue
			}
		}
		found = append(found, discTrigger{Kind: discEdit, Key: key, By: n.by.Login, At: at, ID: n.c.id, Of: of.Login})
	}
	return found, nil
}

// discOwnEdits returns the triggers of a write collaborator who isn't
// trusted editing their own clean comment at or after a trusted 👍 on it,
// which approved it (#87's assumption 6), if open says the edit counts.
// GitHub dates only the last edit, so it's that one's. One a marker
// records is one, with no question and no read. Otherwise it reads the
// reactions only of a comment such a writer edited last, when that counts:
// the reads are conditional, so an unchanged pass costs no rate limit.
func (a *Applier) discOwnEdits(ctx context.Context, s *github.Snapshot, d Dirt, who discWho, open discOpen) ([]discTrigger, error) {
	var found []discTrigger
	for _, c := range s.Comments {
		if d.Has(c.ID) || c.Edit.At.IsZero() || !strings.EqualFold(c.Edit.By.Login, c.Author.Login) {
			continue
		}
		key := discEditKey(c.ID, c.Edit.At)
		tr := discTrigger{Kind: discOwnEdit, Key: key, By: c.Author.Login, At: c.Edit.At, ID: c.ID}
		o, recorded := open(key, c.Edit.At, 0)
		switch {
		case !o:
			continue
		case recorded:
			found = append(found, tr)
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
				found = append(found, tr)
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
// comment it edited or deleted, is no notice of it. A deletion event
// doesn't say which comment went, nor when it was posted, so any deletion
// of an App comment after tr counts, even the owner's cleanup of an old
// one: then tr gets no notice, which fails toward no second one, and the
// issue stays held all the same.
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
// assumption 7), an own edit's sentence (editNoticeApproval) after its
// own, then the sentence also, if any, and more markers after its own. A hold for a trigger that isn't a comment is also
// its notice, carrying its notice marker, unless a marker records tr or it
// may have had a notice already (discLost). The hold's comment ends in cc
// @<notify> (assumption 5).
func (a *Applier) discHoldMove(s *github.Snapshot, d Dirt, tr discTrigger, from, at, origin state.Stage, seen int64, also string, more ...github.Marker) Move {
	fields := map[string]string{"at": string(at), "origin": string(origin), "reason": "discussion"}
	text := fmt.Sprintf("On hold for discussion, from `%s`: %s. Only a code owner can move this issue, "+
		"so it stays on hold until they resolve this.", at, discWhat(tr))
	text = editNoticeAppend(text, editNoticeApproval(tr))
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
//     (heldStackEnds), with the same at and origin. Whether it was the
//     owner's or a discussion's isn't known, so it fails closed: an open
//     trigger keeps the issue held as before, and an owner hold made on an
//     ancestor needs /resume here.
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
		// The owner hold keeps the first hold's at and origin, as one set by
		// hand on a held issue does (SPEC §5).
		origin := discOrigin(is, is.Stage)
		owner := state.HeldStackFields(map[string]string{"from": string(state.Held), "to": string(state.Held), "at": string(is.Stage),
			"origin": string(origin), "reason": "owner", "event": tr.Key}, is.Round)
		m = a.discHoldMove(s, d, tr, before, is.Stage, origin, is.Round,
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
		// A move the table lacks, as for a type the pipeline doesn't know,
		// holds nothing: the type error, or the transition error, stands
		// for it, and no stage runs on such an issue, so a run's apply
		// doesn't drop its result for it either (Runs.apply).
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
		text := editNoticeAppend(fmt.Sprintf("Also on hold for discussion: %s. This issue stays on hold until a code owner accepts it.", discWhat(tr)), editNoticeApproval(tr))
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
// /resume knows of it: /resume on an ancestor ends only owner holds
// (heldStackEnds), so the issue stays held while one is open, and /resume
// on the issue itself accepts them (T4). The renames are known only once
// CheckLabels has checked s, so an unchecked snapshot misses those, as one
// not yet visited misses any trigger: the next pass holds the issue for it
// again. It also returns the resolver that found them (discFindResolved),
// or nil when s isn't held.
func (a *Applier) discHeldStack(ctx context.Context, s *github.Snapshot, d Dirt, trust github.Trust, is state.Issue) (state.Issue, *resolver, error) {
	if is.Stage != state.Held {
		return is, nil, nil
	}
	var rs *resolver
	found, err := a.discFindResolved(ctx, s, d, trust, &rs)
	if err != nil {
		return is, nil, fmt.Errorf("held stack: %w", err)
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
	return is, rs, nil
}

// discHeldLabel reports whether label is stage:held: such an issue is
// visited on every pass, as a 👍 that resolves a trigger (T4) changes no
// updated_at. A deletion does move it, about 2 seconds later, so the
// snapshot is read again then (SPEC §16).
func discHeldLabel(label string) bool {
	stage, ok := state.ParseLabel(label)
	return ok && stage == state.Held
}
