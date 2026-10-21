package state

import (
	"slices"
	"strconv"

	"github.com/mertenvg/issuedog/internal/github"
)

// HeldStack is what holds a held issue (SPEC §6.4): holds stack.
// The first hold wins: it records where the issue goes back to. A /hold or
// a discussion hold on an issue that's held already writes no marker. Only
// a hold set by hand on a held issue does: a held → held transition marker
// with reason=owner and its event= and by= key, keeping the first hold's at
// and origin. So Holds lists the hold markers since the issue last entered
// held, and a later task can add the holds it knows by other means, such as
// a discussion hold's unresolved triggers.
type HeldStack struct {
	// At and Origin are the first hold's at and origin (SPEC §6.4): where
	// /resume restores the issue to, and where it was before its first
	// discussion hold. Origin is "" if the first hold has none.
	At, Origin Stage
	// Seen is the round bound from before the first hold (SPEC §5, §6.4):
	// the hold's seen, or for a hold written without one, the bound the
	// transition before it set (RoundBound). Every move into, within or out
	// of held carries it as seen, so a comment posted while held is input
	// in the stage the issue lands in, and the round the hold came in goes
	// on after it. 0 if there's no bound.
	Seen int64
	// Holds are the transition markers that hold the issue, oldest first:
	// the one that moved it into held, then each held → held marker of a
	// hold set by hand on it. Empty when the issue isn't held.
	Holds []github.Marker
}

// heldStackNext returns h after m, a transition marker that sets the round
// bound (RoundBound), when the bound before it was before. A move to any
// stage but held ends every hold. A move into held starts h afresh, and a
// held → held marker adds its hold to h. One with no hold before it, as when
// the first hold's comment is dirty, starts h afresh too.
//
// A held → held marker with no reason is no hold of its own (SPEC §5,
// §6.4): the parent's newly approved revision changed the
// held issue, so the stage it would have gone to becomes its At and
// Origin, which /resume and the interviewer's unchanged take it to. It
// sets those and adds nothing to Holds. With no hold before it, as when
// the first hold's comment is dirty, it sets Seen too, from its own seen
// or else before, and Holds stays empty, so /resume on the issue ends the
// hold (heldStackResume).
func (h HeldStack) heldStackNext(m github.Marker, before int64) HeldStack {
	seen := before
	if s, err := strconv.ParseInt(m.Fields[FieldSeen], 10, 64); err == nil && s > 0 {
		seen = s
	}
	switch {
	case Stage(m.Fields["to"]) != Held:
		return HeldStack{}
	case heldStackChanged(m):
		if len(h.Holds) == 0 {
			h.Seen = seen
		}
		h.At, h.Origin = Stage(m.Fields["at"]), Stage(m.Fields["origin"])
		return h
	case len(h.Holds) == 0 || Stage(m.Fields["from"]) != Held:
		return HeldStack{At: Stage(m.Fields["at"]), Origin: Stage(m.Fields["origin"]), Seen: seen, Holds: []github.Marker{m}}
	}
	h.Holds = append(slices.Clone(h.Holds), m)
	return h
}

// heldStackChanged reports whether m is the held → held marker of a held
// issue that its parent's newly approved revision changed: one with no
// reason, which every hold's marker carries.
func heldStackChanged(m github.Marker) bool {
	_, reason := m.Fields["reason"]
	return Stage(m.Fields["from"]) == Held && Stage(m.Fields["to"]) == Held && !reason
}

// HeldStackFields sets seen=<bound> in fields, unless bound is 0, and
// returns fields, made if nil: a move into, within or out of held carries
// the round bound from before the hold (HeldStack.Seen).
func HeldStackFields(fields map[string]string, bound int64) map[string]string {
	if fields == nil {
		fields = map[string]string{}
	}
	if bound > 0 {
		fields[FieldSeen] = strconv.FormatInt(bound, 10)
	}
	return fields
}

// heldStackThrough reports whether m, a transition after last, both setting
// the round bound (RoundBound), leaves a round that last began as it was, so
// a reader of last's round looks through m, as a hold keeps the round it
// came in (SPEC §5): a move into held,
// or one out of held back to last's stage that carries the bound from
// before the hold (HeldStack.Seen). A move out of held from before #87, with
// no seen, began a new round.
func heldStackThrough(last, m github.Marker) bool {
	from, to := Stage(m.Fields["from"]), Stage(m.Fields["to"])
	switch {
	case to == Held:
		return true
	case from != Held || m.Fields[FieldSeen] == "":
		return false
	}
	return to == Stage(last.Fields["to"])
}
