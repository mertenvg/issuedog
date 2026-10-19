package state

import (
	"reflect"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// heldStackSnap is an issue whose comments 1, 2, ... carry these transition
// markers, each by the App, or are the owner's where fields is "".
func heldStackSnap(fields ...string) *github.Snapshot {
	s := &github.Snapshot{Number: 7, Author: appBot}
	for i, f := range fields {
		c := github.Comment{ID: int64(i + 1), Author: appBot, Body: "<!-- issuedog:transition " + f + " -->"}
		if f == "" {
			c.Author, c.Body = owner, "a comment"
		}
		s.Comments = append(s.Comments, c)
	}
	return s
}

// heldStackWant is a HeldStack without its markers, and their key IDs.
type heldStackWant struct {
	At, Origin Stage
	Seen       int64
	Keys       []string
}

func heldStackGot(h HeldStack) heldStackWant {
	w := heldStackWant{At: h.At, Origin: h.Origin, Seen: h.Seen}
	for _, m := range h.Holds {
		w.Keys = append(w.Keys, m.Fields["cmd"]+m.Fields["event"]+m.Fields["run"])
	}
	return w
}

func TestHeldStack(t *testing.T) {
	const (
		toReady  = "from=intake to=ready run=a"
		readyRun = "from=intake to=ready run=a seen=9"
	)
	others := heldStackSnap(toReady, "from=ready to=held at=ready reason=owner cmd=5")
	others.Comments = append(others.Comments, github.Comment{ID: 3, Author: appBot,
		Body: "<!-- issuedog:reply reason=rejected cmd=2 -->\n<!-- issuedog:transition to=nowhere cmd=4 -->"})
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  heldStackWant
	}{
		{"not held, no stack", heldStackSnap(toReady), clean, heldStackWant{}},
		{"a hold keeps its own seen", heldStackSnap(readyRun, "from=ready to=held at=ready reason=owner cmd=5 seen=7"), clean,
			heldStackWant{At: Ready, Seen: 7, Keys: []string{"5"}}},
		{"a hold with no seen takes the bound before it: the marker's comment", heldStackSnap(toReady, "from=ready to=held at=ready reason=owner cmd=5"), clean,
			heldStackWant{At: Ready, Seen: 1, Keys: []string{"5"}}},
		{"a hold with no seen takes the bound before it: that marker's seen", heldStackSnap(readyRun, "from=ready to=held at=ready reason=owner cmd=5"), clean,
			heldStackWant{At: Ready, Seen: 9, Keys: []string{"5"}}},
		{"a hold by hand on a held issue stacks, and the first hold's fields stay", heldStackSnap(toReady,
			"from=ready to=held at=ready origin=planning reason=discussion event=3 seen=1",
			"from=held to=held at=spec-review origin=intake reason=owner by=owner event=4 seen=2"), clean,
			heldStackWant{At: Ready, Origin: Planning, Seen: 1, Keys: []string{"3", "4"}}},
		{"leaving held ends every hold", heldStackSnap(toReady, "from=ready to=held at=ready reason=owner cmd=5", "from=held to=held at=ready reason=owner by=owner event=6 seen=1",
			"from=held to=ready cmd=7 seen=1"), clean, heldStackWant{}},
		{"a new hold after leaving starts afresh", heldStackSnap(toReady, "from=ready to=held at=ready reason=owner cmd=5", "from=held to=held at=ready reason=owner by=owner event=6 seen=1",
			"from=held to=planning by=owner event=7 seen=1", "", "from=planning to=held at=planning reason=owner cmd=8"), clean,
			heldStackWant{At: Planning, Seen: 1, Keys: []string{"8"}}},
		{"a hold after a dirty move out of held starts afresh", heldStackSnap(toReady, "from=ready to=held at=ready reason=owner cmd=5",
			"from=held to=ready cmd=6 seen=1", "from=ready to=held at=ready reason=owner cmd=7"), dirtyIDs(3),
			heldStackWant{At: Ready, Seen: 2, Keys: []string{"7"}}},
		{"a held → held marker after a dirty first hold starts the stack", heldStackSnap(toReady, "from=ready to=held at=ready reason=owner cmd=5",
			"from=held to=held at=ready reason=owner by=owner event=6 seen=1"), dirtyIDs(2),
			heldStackWant{At: Ready, Seen: 1, Keys: []string{"6"}}},
		// #195's T7: a held child its parent's revision changed.
		{"a changed child's held → held, with no reason, sets at and origin, and isn't a hold", heldStackSnap(toReady,
			"from=ready to=held at=ready origin=ready reason=discussion event=3 seen=1",
			"from=held to=held at=spec-review origin=intake reason=owner by=owner event=4 seen=1",
			"from=held to=held at=planning origin=planning event=p9 seen=1"), clean,
			heldStackWant{At: Planning, Origin: Planning, Seen: 1, Keys: []string{"3", "4"}}},
		{"a hold by hand after a changed child's held → held keeps its at and origin", heldStackSnap(toReady,
			"from=ready to=held at=ready reason=owner cmd=5",
			"from=held to=held at=needs-human origin=needs-human event=p9 seen=1",
			"from=held to=held at=needs-human origin=needs-human reason=owner by=owner event=6 seen=1"), clean,
			heldStackWant{At: NeedsHuman, Origin: NeedsHuman, Seen: 1, Keys: []string{"5", "6"}}},
		{"a changed child's held → held after a dirty first hold sets at, origin and seen, with no hold", heldStackSnap(toReady,
			"from=ready to=held at=ready reason=owner cmd=5", "from=held to=held at=ready origin=ready event=p9 seen=1"), dirtyIDs(2),
			heldStackWant{At: Ready, Origin: Ready, Seen: 1}},
		{"leaving held after a changed child's held → held ends every hold", heldStackSnap(toReady,
			"from=ready to=held at=ready reason=owner cmd=5", "from=held to=held at=planning origin=planning event=p9 seen=1",
			"from=held to=planning cmd=7 seen=1"), clean, heldStackWant{}},
		{"other markers leave it alone", others, clean,
			heldStackWant{At: Ready, Seen: 1, Keys: []string{"5"}}},
	} {
		if got := heldStackGot(Derive(tt.s, slug, tt.dirty).HeldStack); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: held stack = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

func TestHeldStackFields(t *testing.T) {
	if got := HeldStackFields(nil, 0); !reflect.DeepEqual(got, map[string]string{}) {
		t.Errorf("HeldStackFields(nil, 0) = %v, want an empty map", got)
	}
	f := map[string]string{"at": "ready"}
	if got := HeldStackFields(f, 12); !reflect.DeepEqual(got, map[string]string{"at": "ready", "seen": "12"}) || f["seen"] != "12" {
		t.Errorf("HeldStackFields(f, 12) = %v, and f = %v; want seen=12 added to f", got, f)
	}
}

// #83's note and #80's note 2: a hold at clarifying keeps the round. A
// revision request's notes, and comments posted while held, are input
// after the hold.
func TestInputsAcrossAHold(t *testing.T) {
	const (
		sentBack = "from=spec-review to=clarifying cmd=2 seen=2"
		hold     = "from=clarifying to=held at=clarifying reason=owner cmd=4 seen=2"
		resume   = "from=held to=clarifying cmd=7 seen=2"
	)
	inputs := func(s *github.Snapshot) []int64 {
		var ids []int64
		is := Derive(s, slug, clean)
		for _, c := range is.Inputs(s, func(c github.Comment) bool { return c.Author == owner }) {
			ids = append(ids, c.ID)
		}
		return ids
	}
	// 1: the spec-review revision; 2: the request; 3: its move; 4: /hold;
	// 5: the hold; 6: a comment while held; 7: /resume; 8: the move back.
	base := func(markers ...string) *github.Snapshot {
		s := heldStackSnap("from=clarifying to=spec-review run=a", "", sentBack, "", hold, "", "")
		s.Comments[1].Body, s.Comments[3].Body, s.Comments[6].Body = "too long", "/hold", "/resume"
		for i, m := range markers {
			s.Comments = append(s.Comments, github.Comment{ID: int64(8 + i), Author: appBot, Body: "<!-- issuedog:transition " + m + " -->"})
		}
		return s
	}
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		want []int64
	}{
		{"while held, there's none", base(), nil},
		{"after /resume, the notes and what came while held", base(resume), []int64{2, 6}},
		{"after a hold by hand on it too", base("from=held to=held at=clarifying reason=owner by=owner event=9 seen=2", resume), []int64{2, 6}},
		{"after a hand-set clarifying, which keeps the round too", base("from=held to=clarifying by=owner event=9 seen=2"), []int64{2, 6}},
		{"a move out of held from before #87, with no seen, starts a new round", base("from=held to=clarifying cmd=7"), nil},
		{"after a move out of held to another stage, there are no notes", base("from=held to=planning by=owner event=9 seen=2"), []int64{6}},
		{"after the next round, the notes are gone", base(resume, "from=clarifying to=clarifying round=1 questions=2 run=b seen=6"), nil},
	} {
		if got := inputs(tt.s); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: inputs = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestNotesThroughAHold(t *testing.T) {
	for _, tt := range []struct {
		name    string
		markers []string
		want    int64
	}{
		{"sent back", []string{"from=spec-review to=clarifying cmd=5 seen=5"}, 5},
		{"through a hold", []string{"from=spec-review to=clarifying cmd=5 seen=5", "from=clarifying to=held at=clarifying reason=owner cmd=6 seen=5",
			"from=held to=clarifying cmd=7 seen=5"}, 5},
		{"held, no notes yet", []string{"from=spec-review to=clarifying cmd=5 seen=5", "from=clarifying to=held at=clarifying reason=owner cmd=6 seen=5"}, 0},
		{"a resume to another stage than the hold's", []string{"from=spec-review to=clarifying cmd=5 seen=5", "from=clarifying to=held at=clarifying reason=owner cmd=6 seen=5",
			"from=held to=spec-review by=owner event=8 seen=5"}, 0},
		{"a hold with nothing before it", []string{"from=intake to=held at=intake reason=owner cmd=6", "from=held to=intake cmd=7 seen=1"}, 0},
		{"a hold left by way of another stage drops them", []string{"from=spec-review to=clarifying cmd=5 seen=5",
			"from=clarifying to=held at=clarifying reason=owner cmd=6 seen=5", "from=held to=spec-review by=owner event=8 seen=5",
			"from=spec-review to=held at=spec-review reason=owner cmd=9 seen=5", "from=held to=clarifying by=owner event=10 seen=5"}, 0},
		{"a hold from spec-review back to clarifying isn't a request", []string{"from=spec-review to=held at=spec-review reason=owner cmd=6 seen=5",
			"from=held to=clarifying by=owner event=8 seen=5"}, 0},
	} {
		s := heldStackSnap(tt.markers...)
		if got := Derive(s, slug, clean).Notes(); got != tt.want {
			t.Errorf("%s: notes = %d, want %d", tt.name, got, tt.want)
		}
	}
}
