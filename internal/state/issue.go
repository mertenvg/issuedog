package state

import (
	"strconv"

	"github.com/mertenvg/issuedog/internal/github"
)

// BodyID stands for the issue body where dirty takes a comment ID.
const BodyID int64 = 0

// Key is an idempotency key (plan #5, assumption 4): marker field Field, one
// of "cmd", "run" or "event", with value ID.
type Key struct{ Field, ID string }

// CmdKey is the key of a command comment.
func CmdKey(commentID int64) Key { return Key{"cmd", strconv.FormatInt(commentID, 10)} }

// ReactionKey is the key of a reaction command: cmd=r<reaction id>.
func ReactionKey(reactionID int64) Key { return Key{"cmd", "r" + strconv.FormatInt(reactionID, 10)} }

// Comment returns the comment ID a cmd key names, or false for any other key.
func (k Key) Comment() (int64, bool) {
	if k.Field != "cmd" {
		return 0, false
	}
	id, err := strconv.ParseInt(k.ID, 10, 64)
	return id, err == nil && id > 0
}

// Issue is an issue's state, derived from the markers in its snapshot.
type Issue struct {
	// Stage is the latest transition marker's to=, or Intake if there is none.
	Stage Stage
	// Markers are every marker that counts, the body's first, then by comment.
	Markers []github.Marker
	// Keys are the cmd, run and event IDs the markers name.
	Keys map[Key]bool
	// Errors are the state errors posted (SPEC §5.3), by kind and key.
	Errors map[ErrorKey]bool
	// Acked are the keys of acknowledged state errors. An ack reply names
	// no kind, so it covers its key for every kind.
	Acked map[string]bool
	// Item is what Derive knows of the class. Human is never set: no marker
	// records it yet.
	Item Item
}

// ErrorKey names a state error: its kind and key (SPEC §5.3).
type ErrorKey struct{ Kind, Key string }

// Reported reports whether the state error of this kind and key needs no
// comment: one was posted, or a trusted user acknowledged its key.
func (is Issue) Reported(kind, key string) bool {
	return is.Errors[ErrorKey{kind, key}] || is.Acked[key]
}

// Moved reports whether a transition marker carries k: the transition it
// keys has been written (SPEC §5.2).
func (is Issue) Moved(k Key) bool {
	for _, m := range is.Markers {
		if m.Kind == "transition" && k.ID != "" && m.Fields[k.Field] == k.ID {
			return true
		}
	}
	return false
}

// Derive reads s's state from the markers the App wrote (SPEC §5.1). Markers
// in content that dirty reports, by comment ID or BodyID, are skipped:
// dirty decides what an edit taints, not Derive.
func Derive(s *github.Snapshot, appSlug string, dirty func(id int64) bool) Issue {
	is := Issue{Stage: Intake, Keys: map[Key]bool{}, Errors: map[ErrorKey]bool{}, Acked: map[string]bool{},
		Item: Item{Type: s.Type, HasParent: s.Parent != 0}}
	read := func(id int64, author github.Author, body string) {
		if dirty(id) {
			return
		}
		for _, m := range github.TrustedMarkers(author, body, appSlug) {
			is.Markers = append(is.Markers, m)
			if m.Kind == "origin" && id == BodyID && m.Fields["plan"] != "" {
				is.Item.FromPlan = true
			}
		}
	}
	read(BodyID, s.Author, s.Body)
	for _, c := range s.Comments {
		read(c.ID, c.Author, c.Body)
	}
	for _, m := range is.Markers {
		for _, f := range []string{"cmd", "run", "event"} {
			if id := m.Fields[f]; id != "" {
				is.Keys[Key{f, id}] = true
			}
		}
		if key := m.Fields["key"]; m.Kind == "error" && key != "" {
			if kind := m.Fields["kind"]; kind != "" {
				is.Errors[ErrorKey{kind, key}] = true
			}
			if m.Fields["ack"] != "" {
				is.Acked[key] = true
			}
		}
		for _, f := range []string{"from", "to", "at", "origin"} {
			switch Stage(m.Fields[f]) {
			case Ready, InProgress, Verifying, PROpen:
				is.Item.ReachedReady = true
			}
		}
		if to := Stage(m.Fields["to"]); m.Kind == "transition" && (to.Open() || to == Closed) {
			is.Stage = to
		}
	}
	return is
}
