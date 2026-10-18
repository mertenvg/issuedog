package state

import (
	"strconv"
	"strings"

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
	// Item is what Derive knows of the class. Its Human is Origin.Human: a
	// human task's origin marker carries human=1 (#224's reconcile decision
	// 4).
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
	// HeldStack is what holds the issue while Stage is Held, else the zero
	// HeldStack.
	HeldStack HeldStack
	// Origin is the plan item the App filed the issue from, from the
	// origin marker in its body or else in the newest clean App comment
	// that has one, or the zero Origin with none (originOf).
	Origin Origin
}

// Origin is an issue's origin marker (SPEC §16, "Created issues"): the
// plan item the App filed it from, as origin plan=#N rev=R item=ID. It's
// the one place the marker is read (#224's reconcile decision 3, plan #248,
// assumption 8), and what every later reader builds on:
//   - the digest's plan order (S2), which lists the stories one plan filed
//     together, in the plan's item order (waitRank);
//   - matching created children to their plan items, and Item.Human, from
//     a human task's human=1 (S3, Human);
//   - the class: Item.FromPlan is true exactly when there's one (#290).
//
// Derive reads it where SPEC §5 has it, "in the body … or reposted in a
// comment once that body is dirty" (#290, plan #195's assumption 3):
//   - the body's first whole marker, under the same rules as every other
//     marker there: the App's, in a body that isn't dirty and that nobody
//     but the App edited last;
//   - else, the newest clean App comment's: one that isn't dirty, so
//     nobody but the App edited it. That holds whoever wrote or edited the
//     body, so it covers a body someone else edited, whose marker the
//     daemon reposts in a comment (S5's T2, #291), and an adopted issue,
//     whose body a person wrote and whose App comment carries the marker.
//
// So a marker someone else wrote, in the body or a comment, gives none,
// and the body's marker wins while it's read. Of several clean App
// comments with a whole marker, the newest counts.
//
// Plan is above 0 whenever there is one: the zero Origin is none, so
// callers check Plan. Fields the marker has besides plan, rev, item, human
// and a human task's check fields aren't kept here, and don't stop it from
// being read.
type Origin struct {
	// Plan is the number of the issue whose plan the item is from, from
	// plan=#N.
	Plan int
	// Rev is the plan revision the item was filed from, from rev=. A kept
	// item keeps the rev it was filed at after a re-plan (SPEC §16).
	Rev int
	// Item is the item's ID in the plan, such as S2 or T4, from item=.
	Item string
	// Comment is the ID of the comment that holds the marker, or BodyID
	// for the body.
	Comment int64
	// Human is true for a human task (SPEC §9.3), from human=1, which the
	// App writes on a human task's marker when it creates it from a plan
	// (plan #249, assumption 9). Any other value of human, or none, is
	// false.
	Human bool
	// Check is a human task's "Done when" check, from the marker's check,
	// value and contains (CheckFields, plan #250, Q1a), or the zero Check
	// for a plain-text one, and for fields that don't record a whole check
	// (checkOf). It's read with the rest of the marker, so wherever Origin
	// is read from, it is too.
	Check Check
}

// originOf returns the Origin that marker m, read in comment id (or
// BodyID), records, or false if it isn't a whole one: an origin marker
// with plan=#N and rev=R, each above 0, and a non-empty item. Its human
// and check fields are read too, and its other fields ignored.
func originOf(m github.Marker, id int64) (Origin, bool) {
	num, hash := strings.CutPrefix(m.Fields["plan"], "#")
	plan, perr := strconv.Atoi(num)
	rev, rerr := strconv.Atoi(m.Fields["rev"])
	item := m.Fields["item"]
	if m.Kind != "origin" || !hash || perr != nil || plan <= 0 || rerr != nil || rev <= 0 || item == "" {
		return Origin{}, false
	}
	return Origin{Plan: plan, Rev: rev, Item: item, Comment: id, Human: m.Fields["human"] == "1", Check: checkOf(m.Fields)}, true
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
// comment the pass read; on a move into, within or out of held, the round
// bound from before the hold (HeldStack.Seen), so a hold doesn't end the
// round it came in. Only a comment above it is new input after it.
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

// CreatedRecord returns the children that the record of plan revision
// rev's children names (plan #249, assumption 7), by number in plan
// order, and true, or false if there's none: the latest created marker
// among is's markers that count, with rev=<rev> and children=<n>,<n>, each
// a number above 0. The App posts it on a parent once each child of rev is
// created, linked and moved, with its blocked-by links. A record in a
// dirty comment, as when someone else edited it, doesn't count, since
// Derive skips it, nor does one of another revision, or one whose
// children don't all read back.
func CreatedRecord(is Issue, rev int) ([]int, bool) {
	var found []int
	for _, m := range is.Markers {
		if m.Kind != "created" || m.Fields["rev"] != strconv.Itoa(rev) {
			continue
		}
		if children, ok := createdChildren(m.Fields["children"]); ok {
			found = children
		}
	}
	return found, found != nil
}

// createdChildren reads a created marker's children=<n>,<n>, or reports
// false if it's empty or any of them isn't a number above 0.
func createdChildren(field string) ([]int, bool) {
	if field == "" {
		return nil, false
	}
	var children []int
	for _, f := range strings.Split(field, ",") {
		n, err := strconv.Atoi(f)
		if err != nil || n <= 0 {
			return nil, false
		}
		children = append(children, n)
	}
	return children, true
}

// Derive reads s's state from the markers the App wrote (SPEC §5.1). Markers
// in content that dirty reports, by comment ID or BodyID, are skipped:
// dirty decides what an edit taints, not Derive. The body's are also
// skipped unless the App made its last edit, or nobody edited it: a
// trusted user's edit leaves the body clean (Sanctioned) but isn't the
// App's text (#169, Q2a). The App's own body edits either write the whole
// body or escape the text they keep (SPEC §5.1), so after one, every
// marker in the body is the App's. Origin comes from the body's markers
// while they're read, and else from the newest clean App comment's, and
// Item.FromPlan follows it (see Origin).
func Derive(s *github.Snapshot, appSlug string, dirty func(id int64) bool) Issue {
	is := Issue{Stage: Intake, Keys: map[Key]bool{}, Errors: map[ErrorKey]int64{}, Acks: map[ErrorKey]bool{},
		Item: Item{Type: s.Type, HasParent: s.Parent != 0}}
	read := func(id int64, author github.Author, body string) {
		if dirty(id) {
			return
		}
		if e := s.BodyEdit; id == BodyID && !e.At.IsZero() && !IsApp(e.By, appSlug) {
			return
		}
		for _, m := range github.TrustedMarkers(author, body, appSlug) {
			is.Markers = append(is.Markers, m)
			if b, ok := RoundBound(m, id); ok {
				is.HeldStack = is.HeldStack.heldStackNext(m, is.Round)
				is.Round, is.TransitionComment = b, id
			}
			is.readError(m, id)
			// The body's first whole origin marker, the one the App
			// writes, while the body's markers are read. Without one, the
			// newest clean App comment's: the body is read first, so a
			// comment's replaces any but the body's.
			if o, ok := originOf(m, id); ok && (is.Origin.Plan == 0 || is.Origin.Comment != BodyID) {
				is.Origin = o
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
	// FromPlan follows Origin, so the two always agree (#290): a marker
	// that isn't a whole one gives neither.
	is.Item.FromPlan = is.Origin.Plan != 0
	// A human task is known by its origin marker alone, read wherever
	// Origin is read (#275, #290).
	is.Item.Human = is.Origin.Human
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
