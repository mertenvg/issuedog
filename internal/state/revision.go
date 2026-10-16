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

// Revision is a spec revision (SPEC §7.4), from the App's revision marker,
// revision rev=N body=<hash>, in its revision comment (plan #5, assumption 5).
type Revision struct {
	Rev int
	// Hash is BodyHash of the body the revision was posted for.
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

// BodyHash is the hash a revision records of an issue body: the first 8 hex
// digits of the SHA-256 of body with its line endings normalised to "\n", as
// a web edit may turn them into CRLF. Nothing else is normalised: markers and
// a dirty note are part of the text, so any other change makes a mismatch.
func BodyHash(body string) string {
	body = strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\r", "\n")
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:4])
}

// LatestRevision returns the revision in the last of s's comments with a
// revision marker that counts: by the App and not dirty (as for Derive). A
// dirty revision comment is no revision, so the one before it is the latest,
// and the next one posted takes the dirty one's number. Any dirty App
// comment after it marks it Hidden: only the App writes revision markers, so
// a dirty comment by anyone else can't hide one. So does an App comment
// deleted after it was posted (deletedHides), which may have been a newer
// revision: else deleting the newer revisions would bring back an older
// one, and a 👍 left on it earlier (#87's note 3). It reports false if
// there is none.
func LatestRevision(s *github.Snapshot, appSlug string, dirty func(int64) bool) (Revision, bool) {
	var latest Revision
	var latestAt time.Time // when the latest revision's comment was posted
	var lastDirty int64    // the last dirty App comment
	for _, c := range s.Comments {
		if dirty(c.ID) {
			if IsApp(c.Author, appSlug) {
				lastDirty = c.ID
			}
			continue
		}
		var found *Revision
		var moved int64 // a transition's seen
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			rev, err := strconv.Atoi(m.Fields["rev"])
			seen, serr := strconv.ParseInt(m.Fields[FieldSeen], 10, 64)
			if serr != nil || seen < 0 {
				seen = 0
			}
			switch {
			case m.Kind == "revision" && err == nil && rev > 0 && m.Fields["body"] != "":
				found = &Revision{Rev: rev, Hash: m.Fields["body"], Comment: c.ID, Seen: seen}
			case m.Kind == "transition":
				moved = seen
			}
		}
		if found != nil {
			latest, latestAt = *found, c.CreatedAt
			latest.Seen = cmp.Or(latest.Seen, moved)
		}
	}
	latest.Hidden = lastDirty > latest.Comment || deletedHides(s.Deleted, appSlug, latestAt)
	return latest, latest.Comment != 0
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
