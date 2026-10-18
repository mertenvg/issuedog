package state

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

// RevisionKind is a revision marker's kind field (plan #247, assumption 2):
// SpecRevision, with no kind, or PlanRevision. Each kind numbers its
// revisions on its own, from 1, and a reader of one kind never reads the
// other's markers, so a plan revision is never taken for a spec revision,
// nor a spec revision for a plan revision.
type RevisionKind string

const (
	// SpecRevision is a spec's revision, revision rev=N body=<hash>, in its
	// revision comment: its hash is BodyHash of the issue body.
	SpecRevision RevisionKind = ""
	// PlanRevision is a plan's, revision kind=plan rev=N body=<hash>, on the
	// last line of the plan comment: its hash is PlanHash of that comment.
	PlanRevision RevisionKind = "plan"
)

// Revision is a spec or plan revision (SPEC §7.4), from the App's revision
// marker in its revision comment (plan #5, assumption 5): revision rev=N
// body=<hash> for a spec, revision kind=plan rev=N body=<hash> for a plan.
type Revision struct {
	Kind RevisionKind
	Rev  int
	// Hash is the marker's body: BodyHash of the body a spec revision was
	// posted for, or PlanHash of a plan revision's comment.
	Hash string
	// Comment is the revision comment's ID.
	Comment int64
	// Hidden is set when a dirty App comment comes after it, or an App
	// comment was deleted after it was posted (#186). Its marker can't be
	// read, so it may be a newer revision, and then this one isn't the
	// latest: nothing may approve until a clean revision follows it.
	Hidden bool
	// Seen is the newest comment ID read before the revision was posted:
	// its marker's seen, else that of a transition in its comment, else 0.
	// Only a later comment can be a revision request on it.
	Seen int64
}

// Matches reports whether what r was posted for still has r's hash, as an
// approval of r needs (SPEC §7.4). For a spec revision, that's s's body
// (BodyHash). For a plan revision, it's r's comment in s, the plan comment
// (PlanHash): false if s lacks it, or if its last line is no longer a plan
// revision marker. It doesn't check Hidden, or whether the body is dirty:
// the caller does. A dirty revision comment is no revision (LatestRevision).
func (r Revision) Matches(s *github.Snapshot) bool {
	if r.Kind == SpecRevision {
		return BodyHash(s.Body) == r.Hash
	}
	for _, c := range s.Comments {
		if c.ID == r.Comment {
			hash, ok := PlanHash(c.Body)
			return ok && hash == r.Hash
		}
	}
	return false
}

// BodyHash is the hash a revision records of an issue body: the first 8 hex
// digits of the SHA-256 of body with its line endings normalised to "\n", as
// a web edit may turn them into CRLF. Nothing else is normalised: markers and
// a dirty note are part of the text, so any other change makes a mismatch.
func BodyHash(body string) string {
	body = normaliseLineEndings(body)
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:4])
}

// normaliseLineEndings turns CRLF and CR line endings into "\n".
func normaliseLineEndings(text string) string {
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
}

// PlanHash is the hash a plan revision records of its comment (plan #247,
// assumption 1): BodyHash of the comment's text above its last line. That
// covers the plan, the move's transition marker and any marker after it,
// such as the plan's data. Line endings are normalised first, as for
// BodyHash, so the last line is the text after the last line ending. It
// reports false unless that line is a plan revision marker, revision
// kind=plan, and nothing else. It doesn't check who wrote body: the caller
// does, as LatestRevision does.
func PlanHash(body string) (string, bool) {
	body = normaliseLineEndings(body)
	i := strings.LastIndex(body, "\n")
	if i < 0 {
		return "", false
	}
	above, last := body[:i], strings.TrimSpace(body[i+1:])
	ms := github.ParseMarkers(last)
	if len(ms) != 1 || !strings.HasPrefix(last, "<!--") || !strings.HasSuffix(last, "-->") ||
		ms[0].Kind != "revision" || RevisionKind(ms[0].Fields["kind"]) != PlanRevision {
		return "", false
	}
	return BodyHash(above), true
}

// LatestRevision returns the latest revision of kind (SPEC §7.4): the one in
// the last of s's comments with a revision marker of kind that counts, by
// the App and not dirty (as for Derive). Markers of any other kind are
// ignored, so each kind is read and numbered on its own, by the same rules.
// A dirty revision comment is no revision, so the one before it is the
// latest, and the next one posted takes the dirty one's number. Any dirty
// App comment after it marks it Hidden, whatever kind it may have held: its
// markers can't be read, so it may be a newer revision. Only the App writes
// revision markers, so a dirty comment by anyone else can't hide one. So
// does an App comment deleted after it was posted (deletedHides), which may
// have been a newer revision: else deleting the newer revisions would bring
// back an older one, and a 👍 left on it earlier (#87's note 3). It reports
// false if there is none.
func LatestRevision(s *github.Snapshot, appSlug string, kind RevisionKind, dirty func(int64) bool) (Revision, bool) {
	latest := Revision{Kind: kind}
	var latestAt time.Time // when the latest revision's comment was posted
	var lastDirty int64    // the last dirty App comment
	for _, c := range s.Comments {
		if dirty(c.ID) {
			if IsApp(c.Author, appSlug) {
				lastDirty = c.ID
			}
			continue
		}
		if found, ok := commentRevision(c, appSlug, kind); ok {
			latest, latestAt = found, c.CreatedAt
		}
	}
	latest.Hidden = lastDirty > latest.Comment || deletedHides(s.Deleted, appSlug, latestAt)
	return latest, latest.Comment != 0
}

// commentRevision returns the revision of kind that comment c holds: its
// last revision marker of kind the App wrote, with a rev above 0 and a
// body. Its Seen is the marker's seen, else that of the last transition in
// c, else 0. It reports false if c holds none. It doesn't check whether c
// is dirty: the caller does.
func commentRevision(c github.Comment, appSlug string, kind RevisionKind) (Revision, bool) {
	var found *Revision
	var moved int64 // a transition's seen
	for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
		rev, err := strconv.Atoi(m.Fields["rev"])
		seen, serr := strconv.ParseInt(m.Fields[FieldSeen], 10, 64)
		if serr != nil || seen < 0 {
			seen = 0
		}
		switch {
		case m.Kind == "revision" && RevisionKind(m.Fields["kind"]) == kind && err == nil && rev > 0 && m.Fields["body"] != "":
			found = &Revision{Kind: kind, Rev: rev, Hash: m.Fields["body"], Comment: c.ID, Seen: seen}
		case m.Kind == "transition":
			moved = seen
		}
	}
	if found == nil {
		return Revision{}, false
	}
	r := *found
	r.Seen = cmp.Or(r.Seen, moved)
	return r, true
}

// PlanReason is a reason the latest plan revision can't be approved
// (PlanApproval). Each caller words it itself: /approve's rejection, the
// digest's waiting item.
type PlanReason int

const (
	// PlanNoRevision is that there's no plan revision yet. It's then the
	// only reason.
	PlanNoRevision PlanReason = iota + 1
	// PlanHidden is that the latest plan revision is Hidden: a dirty or
	// deleted App comment after it may hide a newer one (SPEC §7.4). A
	// dirty plan comment is no revision, so it hides the one before it.
	PlanHidden
	// PlanChanged is that the plan comment no longer matches its hash
	// (Revision.Matches).
	PlanChanged
	// PlanBodyDirty is that the issue body has an edit that wasn't
	// approved (plan #248, Q2a). A trusted edit to the body leaves the plan
	// approvable: the plan's hash is of its own comment.
	PlanBodyDirty
)

// PlanApproval returns the latest plan revision (LatestRevision), and the
// reasons it can't be approved, in this order: PlanHidden, PlanChanged and
// PlanBodyDirty, each when it holds. With none, it can be approved. With no
// plan revision, it returns the zero plan revision and PlanNoRevision,
// alone. dirty is as for Derive, BodyID included. It doesn't check who
// approves, when, or the stage: the callers do. A spec revision is never
// read.
func PlanApproval(s *github.Snapshot, appSlug string, dirty func(int64) bool) (Revision, []PlanReason) {
	latest, ok := LatestRevision(s, appSlug, PlanRevision, dirty)
	if !ok {
		// LatestRevision may report one Hidden with none before it.
		return Revision{Kind: PlanRevision}, []PlanReason{PlanNoRevision}
	}
	var why []PlanReason
	if latest.Hidden {
		why = append(why, PlanHidden)
	}
	if !latest.Matches(s) {
		why = append(why, PlanChanged)
	}
	if dirty(BodyID) {
		why = append(why, PlanBodyDirty)
	}
	return latest, why
}

// ApprovedPlan returns the plan revision the latest move into planned
// approved, with its comment, for S3 to read the plan's data from
// (readPlanData, in internal/daemon). That move is the last transition
// marker with to=planned and a rev, in a clean comment the App wrote, that
// names the revision's hash:
//   - by its body field, as an approval from plan-review does;
//   - or else, for a move from planning that skips review, by its own
//     comment's plan revision marker of the same rev: that move is the
//     plan comment (Move.PlanRev), whose transition marker has rev but no
//     body.
//
// A move with neither, such as one back from a hold, names none, so the
// move before it counts. The revision is the last clean App comment whose
// plan revision marker has that rev and hash, and that still matches it
// (Revision.Matches). It reports false if there's no such move, or no such
// comment: the comment is gone, dirty, or edited since. The body's markers
// aren't read: a move is always a comment. A spec revision is never
// returned. The revision isn't marked Hidden: once approved, a later dirty
// comment doesn't unapprove it.
func ApprovedPlan(s *github.Snapshot, appSlug string, dirty func(int64) bool) (Revision, github.Comment, bool) {
	rev, hash := 0, ""
	for _, c := range s.Comments {
		if dirty(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			r, err := strconv.Atoi(m.Fields["rev"])
			if m.Kind != "transition" || Stage(m.Fields["to"]) != Planned || err != nil || r <= 0 {
				continue
			}
			h := m.Fields["body"]
			if pr, ok := commentRevision(c, appSlug, PlanRevision); h == "" && ok && pr.Rev == r {
				h = pr.Hash
			}
			if h != "" {
				rev, hash = r, h
			}
		}
	}
	if rev == 0 {
		return Revision{}, github.Comment{}, false
	}
	var found Revision
	var comment github.Comment
	for _, c := range s.Comments {
		if dirty(c.ID) {
			continue
		}
		if r, ok := commentRevision(c, appSlug, PlanRevision); ok && r.Rev == rev && r.Hash == hash && r.Matches(s) {
			found, comment = r, c
		}
	}
	return found, comment, found.Comment != 0
}

// deletedHides reports whether a comment the App may have written was
// deleted after since, when the latest revision's comment was posted. A
// CommentDeletedEvent doesn't say which comment went, so any App comment
// deleted after it counts, even one older than it: that fails closed, and
// the next revision the daemon posts comes after the deletion. So does a
// deleted comment whose author GitHub doesn't name, who may be the App. Who
// deleted it doesn't matter. Times are to the second, so a deletion in the
// same second as the revision's post doesn't count: it could only have
// deleted a comment posted in that second too, and a revision the daemon
// posts just after a deletion it read stays the latest, so it isn't posted
// again.
func deletedHides(deleted []github.DeletedComment, appSlug string, since time.Time) bool {
	for _, d := range deleted {
		if (d.Author.Login == "" || IsApp(d.Author, appSlug)) && d.At.After(since) {
			return true
		}
	}
	return false
}
