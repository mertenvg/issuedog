package state

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"

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
// and the next one posted takes the dirty one's number. It reports false if
// there is none.
func LatestRevision(s *github.Snapshot, appSlug string, dirty func(int64) bool) (Revision, bool) {
	var latest Revision
	for _, c := range s.Comments {
		if dirty(c.ID) {
			continue
		}
		for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
			rev, err := strconv.Atoi(m.Fields["rev"])
			if m.Kind == "revision" && err == nil && rev > 0 && m.Fields["body"] != "" {
				latest = Revision{Rev: rev, Hash: m.Fields["body"], Comment: c.ID}
			}
		}
	}
	return latest, latest.Comment != 0
}
