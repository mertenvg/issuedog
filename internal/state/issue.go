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
	// Errors are the state errors posted (SPEC §5.3), by kind and key, each
	// with the ID of the earliest comment that carries it, or BodyID. An ack
	// reply isn't one: it's in Acks.
	Errors map[ErrorKey]int64
	// Acks are the acknowledged state errors, by kind and key. An ack reply
	// names the error's kind (plan #7, Q5a). One from before it did has an
	// empty Kind and covers its key for every kind: see Acked.
	Acks map[ErrorKey]bool
	// Item is what Derive knows of the class. Human is never set: no marker
	// records it yet.
	Item Item
	// Round is the latest transition's RoundBound: comments with a higher
	// ID are this round's input. 0 if there is no transition.
	Round int64
	// TransitionComment is the ID of the comment holding the latest
	// transition marker, the one that sets Stage. It's 0 if there's none or
	// it's in the body, which comes before every comment, so an error whose
	// comment ID is higher was posted after that transition.
	TransitionComment int64
	// Triage is the latest triage marker in a comment, or none.
	Triage TriageVerdict
}

// TriageVerdict is a triage marker (plan #6, assumption 3): a decision on
// an issue with no stage label, by a triage run or a code owner's answer.
type TriageVerdict struct {
	// Verdict is "ask" for a flag, which waits for a code owner, or "leave"
	// for an issue left to people. It's empty if there's no marker.
	Verdict string
	// Recommend is a flag's recommendation, "handle" or "leave", which a
	// code owner's 👍 on it takes.
	Recommend string
	// Comment is the ID of the comment holding the marker.
	Comment int64
}

// FieldSeen is the marker field seen=<the newest comment ID read>, for
// what came before a write: on the transition that ends an interviewer run,
// the newest comment the run was given (plan #5, amended assumption 5); on
// a move by /revise, a revision request or /approve, that comment; on an
// approval by 👍, and on a revision posted without a move, the newest
// comment the pass read. Only a comment above it is new input after it.
const FieldSeen = "seen"

// RoundBound returns the bound on input that marker m, in comment id, sets:
// comments with a higher ID are the next round's. A transition sets it, to
// its seen if it has one, or else to id, so on entering a stage only later
// comments count. Comment IDs only increase, so a comment posted during a
// run is above its seen. Other markers set none.
func RoundBound(m github.Marker, id int64) (int64, bool) {
	if to := Stage(m.Fields["to"]); m.Kind != "transition" || (!to.Open() && to != Closed) {
		return 0, false
	}
	if seen, err := strconv.ParseInt(m.Fields[FieldSeen], 10, 64); err == nil && seen > 0 {
		return seen, true
	}
	return id, true
}

// ErrorKey names a state error: its kind and key (SPEC §5.3).
type ErrorKey struct{ Kind, Key string }

// Reported reports whether the state error of this kind and key needs no
// comment: one was posted, or a trusted user acknowledged it.
func (is Issue) Reported(kind, key string) bool {
	_, posted := is.Errors[ErrorKey{kind, key}]
	return posted || is.Acked(kind, key)
}

// Acked reports whether an ack reply covers the state error of this kind
// and key: one that names the kind and key, or one with no kind that names
// the key, which covers it for every kind (plan #7, Q5a).
func (is Issue) Acked(kind, key string) bool {
	return is.Acks[ErrorKey{kind, key}] || is.Acks[ErrorKey{"", key}]
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
	is := Issue{Stage: Intake, Keys: map[Key]bool{}, Errors: map[ErrorKey]int64{}, Acks: map[ErrorKey]bool{},
		Item: Item{Type: s.Type, HasParent: s.Parent != 0}}
	read := func(id int64, author github.Author, body string) {
		if dirty(id) {
			return
		}
		for _, m := range github.TrustedMarkers(author, body, appSlug) {
			is.Markers = append(is.Markers, m)
			if b, ok := RoundBound(m, id); ok {
				is.Round, is.TransitionComment = b, id
			}
			is.readError(m, id)
			if m.Kind == "origin" && id == BodyID && m.Fields["plan"] != "" {
				is.Item.FromPlan = true
			}
			if m.Kind == "triage" && id != BodyID {
				is.Triage = TriageVerdict{Verdict: m.Fields["verdict"], Recommend: m.Fields["recommend"], Comment: id}
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

// readError records m, read in comment id (or BodyID), if it's an error
// marker with a key: an ack reply in Acks, under its kind, which may be
// empty; or else a posted error in Errors, which needs a kind. A posted
// error keeps the first comment that carries it, since Derive reads the
// body, then the comments in order.
func (is *Issue) readError(m github.Marker, id int64) {
	key, kind := m.Fields["key"], m.Fields["kind"]
	switch {
	case m.Kind != "error" || key == "":
	case m.Fields["ack"] != "":
		is.Acks[ErrorKey{kind, key}] = true
	case kind != "":
		if _, ok := is.Errors[ErrorKey{kind, key}]; !ok {
			is.Errors[ErrorKey{kind, key}] = id
		}
	}
}
