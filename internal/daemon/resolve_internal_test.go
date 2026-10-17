package daemon

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// Only /resume accepts a trigger that edited or deleted content that may
// have been the App's: discHitsApp's, and, failing closed, an edit known
// from its record alone of the body, of a comment gone, or of the App's.
// A notice accepts any other edit, a deletion of a trusted user's comment,
// a rename and a close, which hides no App content (#190's verify note);
// nothing accepts a comment, or a key of no known shape, nor a deletion
// during the hold, which may have hidden an applied /hold (#188's verify,
// round 2), though one that held the issue, made before, is accepted.
func TestResolveByNotice(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 5, Author: github.Author{Login: "owner", Type: "User"}},
		{ID: 6, Author: app},
	}}
	held := time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC)
	for _, tt := range []struct {
		name   string
		tr     discTrigger
		want   bool
		heldAt time.Time
	}{
		{"an edit of a trusted user's comment", discTrigger{Kind: discEdit, ID: 5, Of: "owner"}, true, time.Time{}},
		{"an edit of the App's comment", discTrigger{Kind: discEdit, ID: 6, Of: "issuedog[bot]"}, false, time.Time{}},
		{"an own edit", discTrigger{Kind: discOwnEdit, ID: 7}, true, time.Time{}},
		{"a deletion of a trusted user's comment", discTrigger{Kind: discDeletion, Of: "owner"}, true, time.Time{}},
		{"a deletion of the App's comment", discTrigger{Kind: discDeletion, Of: "issuedog[bot]"}, false, time.Time{}},
		{"a deletion of nobody GitHub names", discTrigger{Kind: discDeletion}, false, time.Time{}},
		{"a rename", discTrigger{Kind: discRename}, true, time.Time{}},
		{"a recorded edit of a trusted user's comment still there", discTrigger{Kind: discEdit, ID: 5, FromRecord: true}, true, time.Time{}},
		{"a recorded edit of the App's comment", discTrigger{Kind: discEdit, ID: 6, FromRecord: true}, false, time.Time{}},
		{"a recorded edit of a comment gone", discTrigger{Kind: discEdit, ID: 8, FromRecord: true}, false, time.Time{}},
		{"a recorded edit of the body", discTrigger{Kind: discEdit, ID: state.BodyID, FromRecord: true}, false, time.Time{}},
		{"a recorded deletion", discTrigger{Kind: discDeletion, FromRecord: true}, false, time.Time{}},
		{"a recorded rename", discTrigger{Kind: discRename, FromRecord: true}, true, time.Time{}},
		{"a close", discTrigger{Kind: discClose, Of: ""}, true, time.Time{}},
		{"a recorded close", discTrigger{Kind: discClose, FromRecord: true}, true, time.Time{}},
		{"a deletion of a trusted user's comment during the hold", discTrigger{Kind: discDeletion, Of: "owner", At: held}, false, held},
		{"a deletion of a trusted user's comment that held the issue", discTrigger{Kind: discDeletion, Of: "owner", At: held.Add(-time.Second)}, true, held},
		{"a comment", discTrigger{Kind: discComment, ID: 5}, false, time.Time{}},
		{"a key of no known shape", discTrigger{Key: "x1", FromRecord: true}, false, time.Time{}},
	} {
		if got := resolveByNotice(tt.tr, s, "issuedog", tt.heldAt); got != tt.want {
			t.Errorf("%s: resolveByNotice = %t; want %t", tt.name, got, tt.want)
		}
	}
}

// A comment trigger is countered only by a later clean comment of a
// trusted user, not by itself, even once its author is trusted, nor by
// a dirty one; with none, the earliest trusted 👍 on it approves it, but
// not on a dirty or gone comment, whose reactions aren't read.
func TestResolverComment(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	owner := github.Author{Login: "owner", Type: "User"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 10, Author: owner, CreatedAt: at(10)},
		{ID: 11, Author: github.Author{Login: "mallory", Type: "User"}, CreatedAt: at(11)},
		{ID: 12, Author: owner, CreatedAt: at(12)},
	}}
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		tr    discTrigger
		dirty Dirt
		want  *resolveAct
		reads int
	}{
		{"a recorded comment of someone trusted since", discTrigger{Kind: discComment, Key: "c12", ID: 12}, Dirt{}, nil, 1},
		{"mallory's, countered", discTrigger{Kind: discComment, Key: "c11", ID: 11}, Dirt{}, &resolveAct{By: "owner", At: at(12), How: resolveCountered}, 0},
		{"mallory's, with the counter dirty", discTrigger{Kind: discComment, Key: "c11", ID: 11}, Dirt{12: "eve"}, nil, 1},
		{"mallory's, dirty itself, with the counter dirty", discTrigger{Kind: discComment, Key: "c11", ID: 11}, Dirt{11: "eve", 12: "eve"}, nil, 0},
		{"one gone", discTrigger{Kind: discComment, Key: "c13", ID: 13, FromRecord: true}, Dirt{}, nil, 0},
	} {
		reads := 0
		a := &Applier{Client: noReactions(t, &reads), Owner: "o", Repo: "r", AppSlug: "issuedog"}
		r := a.resolverOf(s, tt.dirty, a.discWhoOf(s, newDiscCounting()), map[string]int64{})
		got, err := r.comment(ctx, tt.tr)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if (got == nil) != (tt.want == nil) || got != nil && *got != *tt.want || reads != tt.reads {
			t.Errorf("%s: act %+v, with %d reads; want %+v, with %d", tt.name, got, reads, tt.want, tt.reads)
		}
	}
}

// The earliest trusted 👍 resolves, by its time: not a ❤️ or a 👎, nor a 👍 by
// someone untrusted or a bot, however early.
func TestResolverThumbsUp(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	user := func(l string) github.Author { return github.Author{Login: l, Type: "User"} }
	tr := newDiscCounting()
	tr.trusted["owner2"] = true
	s := &github.Snapshot{}
	a := &Applier{Owner: "o", Repo: "r", AppSlug: "issuedog"}
	r := a.resolverOf(s, Dirt{}, a.discWhoOf(s, tr), map[string]int64{})
	r.reactions[7] = []github.Reaction{
		{ID: 0, User: user("owner2"), Content: "heart", CreatedAt: at(0)},
		{ID: 1, User: user("owner"), Content: "-1", CreatedAt: at(1)},
		{ID: 2, User: user("mallory"), Content: "+1", CreatedAt: at(2)},
		{ID: 3, User: github.Author{Login: "helper[bot]", Type: "Bot"}, Content: "+1", CreatedAt: at(3)},
		{ID: 5, User: user("owner2"), Content: "+1", CreatedAt: at(5)},
		{ID: 4, User: user("owner"), Content: "+1", CreatedAt: at(4)},
	}
	got, err := r.thumbsUp(context.Background(), 7, resolveAccepted)
	if err != nil {
		t.Fatal(err)
	}
	if want := (resolveAct{By: "owner", At: at(4), How: resolveAccepted}); got == nil || *got != want {
		t.Errorf("act %+v; want %+v", got, want)
	}
}

// The record of a discussion's end is written once for each resolution: an
// act no later than the latest record's time, as one in its second that a
// comparison by time finds again, or one from before the hold, writes
// nothing, and costs no request.
func TestResolveEndedOnce(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	reads := 0
	a := &Applier{Client: noReactions(t, &reads), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	s := &github.Snapshot{Number: 1}
	r := a.resolverOf(s, Dirt{}, a.discWhoOf(s, newDiscCounting()), map[string]int64{})
	r.acts = []resolveAct{
		{Key: "c5", By: "owner", At: at(9), How: resolveCountered, Recorded: true},
		{Key: "c4", By: "owner", At: at(2), How: resolveCountered, Recorded: true},
	}
	p := resolvePeriod{start: 3, at: at(3), ended: true, endedAt: at(9)}
	if err := a.resolveEnded(context.Background(), s, r, p, false, nil); err != nil || reads != 0 || len(s.Comments) != 0 {
		t.Errorf("err %v, %d requests, comments %v; want nothing written", err, reads, s.Comments)
	}
}

// A trigger in the same second as the /resume counts as after it, failing
// closed: the /resume doesn't end its hold, and the issue stays held.
func TestResolveHeldSameSecond(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	mallory := github.Author{Login: "mallory", Type: "User"}
	s := &github.Snapshot{Number: 1, Comments: []github.Comment{
		{ID: 8, Author: app, Body: "<!-- issuedog:transition from=intake to=ready run=a -->", CreatedAt: at(8)},
		{ID: 9, Author: mallory, Association: "COLLABORATOR", Body: "Wait.", CreatedAt: at(9)},
		{ID: 10, Author: app, Body: "<!-- issuedog:transition from=ready to=held at=ready origin=ready reason=discussion event=c9 seen=8 -->", CreatedAt: at(10)},
		{ID: 11, Author: github.Author{Login: "owner", Type: "User"}, Body: "/resume", CreatedAt: at(12)},
		{ID: 12, Author: mallory, Association: "COLLABORATOR", Body: "Again.", CreatedAt: at(12)},
	}}
	a := &Applier{Client: noReactions(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	is, _, err := a.resolveHeld(context.Background(), s, Dirt{}, newDiscCounting(), state.Derive(s, "issuedog", func(int64) bool { return false }), s.Comments[3], true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ended, left := heldStackResume(is, true, madeAboveSet{}); ended != 1 || len(left) != 1 || left[0].Fields["event"] != "c12" {
		t.Errorf("ended %d, left %v; want the hold for comment 9 ended, and comment 12's left", ended, left)
	}
}

// After a bound by time, a recorded act is open when its record is in the
// bound's comment or later, or when the act is at or after the bound's
// time, its own second included; one known only from its record, at the
// zero time, is judged by its record.
func TestResolveOpenRecordedByTime(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	open := discOpenAfter(discBound{ID: 20, At: at(12), ByTime: true}, map[string]int64{"77": 10, "78": 20})
	for _, tt := range []struct {
		name string
		key  string
		at   time.Time
		want bool
	}{
		{"recorded before, made in the bound's second", "77", at(12), true},
		{"recorded before, made after", "77", at(13), true},
		{"recorded before, made before", "77", at(11), false},
		{"recorded before, known from its record alone", "77", time.Time{}, false},
		{"recorded in the bound's comment", "78", at(1), true},
	} {
		if o, recorded := open(tt.key, tt.at, 0); o != tt.want || !recorded {
			t.Errorf("%s: open = %t, %t; want %t, true", tt.name, o, recorded, tt.want)
		}
	}
}

// A hold began at the clarifying after a hold only when the transition
// before it is to clarifying and carries an open origin but held: one to
// any other stage, even with an origin, didn't.
func TestResolveHeldAfterHold(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	at := func(markers ...string) state.Issue {
		s := &github.Snapshot{}
		for i, m := range markers {
			s.Comments = append(s.Comments, github.Comment{ID: int64(10 + i), Author: app, Body: "<!-- issuedog:transition " + m + " -->"})
		}
		return state.Derive(s, "issuedog", func(int64) bool { return false })
	}
	const hold = "to=held at=clarifying reason=owner cmd=5"
	for _, tt := range []struct {
		name   string
		before string
		want   bool
	}{
		{"the clarifying after a hold", "from=held to=clarifying at=spec-review origin=spec-review event=h3", true},
		{"a clarifying with no origin", "from=spec-review to=clarifying cmd=4", false},
		{"another stage, with an origin", "from=clarifying to=spec-review origin=spec-review run=a", false},
		{"an origin of held", "from=held to=clarifying at=ready origin=held event=h3", false},
	} {
		o, ok := resolveHeldAfterHold(at(tt.before, "from=x "+hold))
		if ok != tt.want || ok && o != state.SpecReview {
			t.Errorf("%s: %q, %t; want %t", tt.name, o, ok, tt.want)
		}
	}
}

// Only a move out of held is bounded by the time of the comment its cmd
// names: a first marker keyed by a command, as /track's, is bounded by its
// own comment, by ID.
func TestResolveFloorFirstCommand(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 5, Author: github.Author{Login: "owner", Type: "User"}, Body: "/track", CreatedAt: at(3)},
		{ID: 10, Author: app, Body: "<!-- issuedog:transition from=intake to=clarifying cmd=5 -->", CreatedAt: at(10)},
	}}
	if b, ok := discFloor(s, "issuedog", func(int64) bool { return false }); !ok || b != (discBound{ID: 10, At: at(10)}) {
		t.Errorf("floor = %+v, %v; want the marker's own comment, by ID", b, ok)
	}
}

// A move out of held is bounded by its resolved time, else by the time of
// the comment on the issue its cmd names; an "ended reason=discussion"
// record by its resolved time alone.
func TestResolveActAt(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	s := &github.Snapshot{Comments: []github.Comment{{ID: 5, CreatedAt: at(5)}}}
	for _, tt := range []struct {
		name   string
		fields map[string]string
		want   time.Time
		ok     bool
	}{
		{"its resolved field", map[string]string{"resolved": "1767225609", "cmd": "5"}, at(9), true},
		{"its /resume here", map[string]string{"cmd": "5"}, at(5), true},
		{"a /resume elsewhere", map[string]string{"cmd": "6"}, time.Time{}, false},
		{"a reaction's key", map[string]string{"cmd": "r5"}, time.Time{}, false},
		{"a resolved field that's no time", map[string]string{"resolved": "soon"}, time.Time{}, false},
		{"a resolved field of zero", map[string]string{"resolved": "0"}, time.Time{}, false},
		{"neither", map[string]string{"event": "h3"}, time.Time{}, false},
	} {
		got, ok := resolveActAt(s, github.Marker{Kind: "transition", Fields: tt.fields})
		if !got.Equal(tt.want) || ok != tt.ok {
			t.Errorf("%s: resolveActAt = %v, %t; want %v, %t", tt.name, got, ok, tt.want, tt.ok)
		}
	}
	for _, tt := range []struct {
		name string
		m    github.Marker
		ok   bool
	}{
		{"a discussion's end", github.Marker{Kind: "ended", Fields: map[string]string{"reason": "discussion", "resolved": "1767225609"}}, true},
		{"one with no time", github.Marker{Kind: "ended", Fields: map[string]string{"reason": "discussion"}}, false},
		{"one with a time of zero", github.Marker{Kind: "ended", Fields: map[string]string{"reason": "discussion", "resolved": "0"}}, false},
		{"an owner hold's end", github.Marker{Kind: "ended", Fields: map[string]string{"reason": "owner", "resolved": "1767225609"}}, false},
		{"another kind", github.Marker{Kind: "transition", Fields: map[string]string{"reason": "discussion", "resolved": "1767225609"}}, false},
	} {
		if got, ok := resolveEndedAt(tt.m); ok != tt.ok || ok && !got.Equal(at(9)) {
			t.Errorf("%s: resolveEndedAt = %v, %t; want %t", tt.name, got, ok, tt.ok)
		}
	}
}

// The hold an issue is in begins at its move into held, or at a held →
// held marker with no hold before it in clean content, and ends at a move
// out; only the records in it count.
func TestResolvePeriodOf(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	snap := func(bodies ...string) *github.Snapshot {
		s := &github.Snapshot{}
		for i, b := range bodies {
			s.Comments = append(s.Comments, github.Comment{ID: int64(10 + i), Author: app, Body: b, CreatedAt: at(10 + i)})
		}
		return s
	}
	const (
		ready   = "<!-- issuedog:transition from=intake to=ready run=a -->"
		hold    = "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=5 -->"
		onHeld  = "<!-- issuedog:transition from=held to=held at=ready reason=owner by=owner event=7 -->"
		out     = "<!-- issuedog:transition from=held to=ready cmd=8 -->"
		ended   = "<!-- issuedog:ended reason=discussion resolved=1767225600 -->"
		later   = "<!-- issuedog:ended reason=discussion resolved=1767225605 -->"
		earlier = "<!-- issuedog:ended reason=discussion resolved=1767225601 -->"
		owner   = "<!-- issuedog:ended reason=owner key=5 cmd=9 -->"
	)
	none := func(int64) bool { return false }
	for _, tt := range []struct {
		name   string
		s      *github.Snapshot
		dirty  func(int64) bool
		start  int64
		ended  time.Time
		owners map[string]string
	}{
		{"not held", snap(ready, ended), none, 0, time.Time{}, nil},
		{"held, with its records", snap(ready, hold, onHeld, later, earlier, owner), none, 11, at(5), map[string]string{"5": "9"}},
		{"a record before the hold doesn't count", snap(ready, ended, hold), none, 12, time.Time{}, map[string]string{}},
		{"nor one from a hold since ended", snap(ready, hold, ended, out, hold), none, 14, time.Time{}, map[string]string{}},
		{"a held → held with the hold's comment dirty begins it", snap(ready, hold, onHeld), func(id int64) bool { return id == 11 }, 12, time.Time{}, map[string]string{}},
		{"a move into held from another stage begins it afresh, as a restore after a hidden move out", snap(ready, hold, owner, hold), none, 13, time.Time{}, map[string]string{}},
	} {
		p := resolvePeriodOf(tt.s, "issuedog", tt.dirty)
		if p.start != tt.start || !p.endedAt.Equal(tt.ended) || p.ended != !tt.ended.IsZero() || !maps.Equal(p.owners, tt.owners) {
			t.Errorf("%s: period %+v; want start %d, ended at %v, owners %v", tt.name, p, tt.start, tt.ended, tt.owners)
		}
		if p.start != 0 && !p.at.Equal(at(int(tt.start))) {
			t.Errorf("%s: begun at %v; want %v", tt.name, p.at, at(int(tt.start)))
		}
	}
}

// A record's held= /holds are read with the issue each counted as posted
// on, from its heldon=, in order; one with no issue there, as in a record
// from before heldon=, or an ill-formed one, has none, and counts as it
// did (resolveHeldOn).
func TestResolvePeriodOfHeldOn(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	for _, tt := range []struct {
		name   string
		record string
		held   []int64
		on     map[int64]int
	}{
		{"each with its issue", "held=21,22 heldon=3,4", []int64{21, 22}, map[int64]int{21: 3, 22: 4}},
		{"an old record has none", "held=21", []int64{21}, nil},
		{"a short heldon names the first ones", "held=21,22 heldon=3", []int64{21, 22}, map[int64]int{21: 3}},
		{"an ill-formed issue names none", "held=21,22 heldon=x,0", []int64{21, 22}, nil},
		{"an ill-formed /hold is skipped, with its issue", "held=x,22 heldon=3,4", []int64{22}, map[int64]int{22: 4}},
	} {
		s := &github.Snapshot{Comments: []github.Comment{
			{ID: 10, Author: app, Body: "<!-- issuedog:transition from=intake to=ready run=a -->"},
			{ID: 11, Author: app, Body: "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=5 -->"},
			{ID: 12, Author: app, Body: "<!-- issuedog:ended reason=discussion " + tt.record + " -->"},
		}}
		p := resolvePeriodOf(s, "issuedog", func(int64) bool { return false })
		if !slices.Equal(p.held, tt.held) || !maps.Equal(p.heldOn, tt.on) {
			t.Errorf("%s: held %v on %v; want %v on %v", tt.name, p.held, p.heldOn, tt.held, tt.on)
		}
	}
}

// An owner hold a /resume's record names counts no more, by its /hold's
// cmd or its event, but for the /resume itself; one no record names, or
// one with neither, stays.
func TestResolveLeftAndOwnerHeld(t *testing.T) {
	m := func(reason, cmd string) github.Marker {
		f := map[string]string{"reason": reason}
		if cmd != "" {
			f["cmd"] = cmd
		}
		return github.Marker{Kind: "transition", Fields: f}
	}
	hand := github.Marker{Kind: "transition", Fields: map[string]string{"reason": "owner", "by": "owner", "event": "7"}}
	holds := []github.Marker{m("discussion", ""), m("owner", "5"), m("owner", "6"), m("owner", ""), hand}
	p := resolvePeriod{owners: map[string]string{"c5": "9", "e7": "9", "": "9", "5": "9"}}
	if got := resolveLeft(holds, p, 0); !slices.EqualFunc(got, []github.Marker{holds[0], holds[2], holds[3]}, resolveSameMarker) {
		t.Errorf("left %v; want all but the hold /hold 5 made and the one by hand", got)
	}
	if got := resolveLeft(holds, p, 9); len(got) != 5 {
		t.Errorf("left for /resume 9 itself %v; want every hold", got)
	}
	if !resolveOwnerHeld(holds) || resolveOwnerHeld(holds[:1]) || resolveOwnerHeld(nil) {
		t.Error("resolveOwnerHeld is wrong: want it set only for a hold that isn't a discussion's")
	}
	if !resolveOwnerHeld([]github.Marker{m("dropped", "")}) {
		t.Error("a dropped hold isn't counted as staying")
	}
}

func resolveSameMarker(a, b github.Marker) bool {
	return a.Kind == b.Kind && maps.Equal(a.Fields, b.Fields)
}

// The move out of a hold to clarifying carries at and origin, the first
// hold's, falling back to at, each only when it's an open stage but held,
// the bound from before the hold, and resolved when known.
func TestResolveFields(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC)
	for _, tt := range []struct {
		name     string
		h        state.HeldStack
		resolved time.Time
		want     map[string]string
	}{
		{"origin", state.HeldStack{At: state.Clarifying, Origin: state.SpecReview, Seen: 7}, at,
			map[string]string{"at": "clarifying", "origin": "spec-review", "seen": "7", "resolved": "1767225609"}},
		{"no origin", state.HeldStack{At: state.Ready, Seen: 7}, time.Time{}, map[string]string{"at": "ready", "origin": "ready", "seen": "7"}},
		{"an origin of held", state.HeldStack{At: state.Ready, Origin: state.Held}, time.Time{}, map[string]string{"at": "ready", "origin": "ready"}},
		{"an at of held", state.HeldStack{At: state.Held, Origin: state.Ready}, time.Time{}, map[string]string{"origin": "ready"}},
		{"no stage at all", state.HeldStack{At: state.Closed}, time.Time{}, map[string]string{}},
	} {
		if got := resolveFields(tt.h, tt.resolved); !maps.Equal(got, tt.want) {
			t.Errorf("%s: fields %v; want %v", tt.name, got, tt.want)
		}
	}
}

// A command counts as applied on an issue only by a marker on it that
// names it and isn't a reply: a move, or an "ended" record, but not the
// rejection's reply, nor an "edited" one, nor a marker naming another
// comment. Only the rejection's reply says it was rejected.
func TestResolveApplied(t *testing.T) {
	is := func(markers ...string) state.Issue {
		s := &github.Snapshot{Comments: []github.Comment{{ID: 900, Author: github.Author{Login: "issuedog[bot]", Type: "Bot"}, Body: ""}}}
		for _, m := range markers {
			s.Comments[0].Body += "<!-- issuedog:" + m + " -->\n"
		}
		return state.Derive(s, "issuedog", func(int64) bool { return false })
	}
	for _, tt := range []struct {
		name              string
		is                state.Issue
		applied, rejected bool
	}{
		{"no marker", is(), false, false},
		{"a move", is("transition from=held to=ready cmd=7"), true, false},
		{"an ended record", is("ended reason=discussion resolved=1 cmd=7"), true, false},
		{"the rejection's reply", is("reply reason=rejected cmd=7"), false, true},
		{"an edited reply", is("reply reason=edited cmd=7"), false, false},
		{"another comment's move", is("transition from=held to=ready cmd=8"), false, false},
		{"another comment's rejection", is("reply reason=rejected cmd=8"), false, false},
	} {
		if got := resolveApplied(tt.is, 7); got != tt.applied {
			t.Errorf("%s: resolveApplied = %t; want %t", tt.name, got, tt.applied)
		}
		if got := resolveRejected(tt.is, 7); got != tt.rejected {
			t.Errorf("%s: resolveRejected = %t; want %t", tt.name, got, tt.rejected)
		}
	}
}

// begun counts a command as begun only by a marker of its own that isn't a
// reply, on any issue in the family: a rejection's reply begins nothing.
func TestBegunNotByReply(t *testing.T) {
	a := &Applier{AppSlug: "issuedog"}
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	with := func(body string) node {
		return node{s: &github.Snapshot{Comments: []github.Comment{{ID: 900, Author: app, Body: body}}}, open: true}
	}
	c := Cmd{Comment: github.Comment{ID: 7}}
	if a.begun([]node{with("<!-- issuedog:reply reason=rejected cmd=7 -->")}, c) {
		t.Errorf("begun by the rejection's reply; want not")
	}
	if !a.begun([]node{with(""), with("<!-- issuedog:ended reason=owner key=c5 cmd=7 -->")}, c) {
		t.Errorf("not begun by its record on a descendant; want begun")
	}
}

// resolveCommands finds only the clean, valid commands of its name by a
// trusted user, above after and below before: not one Check rejects at
// held, as a /hold with an argument, nor another command, a dirty one, an
// untrusted user's, or one out of range.
func TestResolveCommands(t *testing.T) {
	owner := github.Author{Login: "owner", Type: "User"}
	s := &github.Snapshot{Number: 1, Comments: []github.Comment{
		{ID: 10, Author: owner, Association: "OWNER", Body: "/hold"},
		{ID: 11, Author: owner, Association: "OWNER", Body: "/hold now"},
		{ID: 12, Author: owner, Association: "OWNER", Body: "/resume"},
		{ID: 13, Author: owner, Association: "OWNER", Body: "/hold"},
		{ID: 14, Author: github.Author{Login: "eve", Type: "User"}, Association: "CONTRIBUTOR", Body: "/hold"},
		{ID: 15, Author: owner, Association: "OWNER", Body: "/hold"},
		{ID: 16, Author: owner, Association: "OWNER", Body: "/hold"},
	}}
	a := &Applier{AppSlug: "issuedog"}
	got, err := a.resolveCommands(context.Background(), s, Dirt{13: "mallory"}, ownerOnly{}, "hold", 9, 16)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []int64{10, 15}) {
		t.Errorf("resolveCommands = %v; want [10 15]", got)
	}
}
