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
// and a rename; nothing accepts a comment, or a key of no known shape.
func TestResolveByNotice(t *testing.T) {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 5, Author: github.Author{Login: "owner", Type: "User"}},
		{ID: 6, Author: app},
	}}
	for _, tt := range []struct {
		name string
		tr   discTrigger
		want bool
	}{
		{"an edit of a trusted user's comment", discTrigger{Kind: discEdit, ID: 5, Of: "owner"}, true},
		{"an edit of the App's comment", discTrigger{Kind: discEdit, ID: 6, Of: "issuedog[bot]"}, false},
		{"an own edit", discTrigger{Kind: discOwnEdit, ID: 7}, true},
		{"a deletion of a trusted user's comment", discTrigger{Kind: discDeletion, Of: "owner"}, true},
		{"a deletion of the App's comment", discTrigger{Kind: discDeletion, Of: "issuedog[bot]"}, false},
		{"a deletion of nobody GitHub names", discTrigger{Kind: discDeletion}, false},
		{"a rename", discTrigger{Kind: discRename}, true},
		{"a recorded edit of a trusted user's comment still there", discTrigger{Kind: discEdit, ID: 5, FromRecord: true}, true},
		{"a recorded edit of the App's comment", discTrigger{Kind: discEdit, ID: 6, FromRecord: true}, false},
		{"a recorded edit of a comment gone", discTrigger{Kind: discEdit, ID: 8, FromRecord: true}, false},
		{"a recorded edit of the body", discTrigger{Kind: discEdit, ID: state.BodyID, FromRecord: true}, false},
		{"a recorded deletion", discTrigger{Kind: discDeletion, FromRecord: true}, false},
		{"a recorded rename", discTrigger{Kind: discRename, FromRecord: true}, true},
		{"a comment", discTrigger{Kind: discComment, ID: 5}, false},
		{"a key of no known shape", discTrigger{Key: "x1", FromRecord: true}, false},
	} {
		if got := resolveByNotice(tt.tr, s, "issuedog"); got != tt.want {
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

// The earliest trusted 👍 resolves, by its time: not a 👎, nor a 👍 by
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
	if err := a.resolveEnded(context.Background(), s, r, p, false); err != nil || reads != 0 || len(s.Comments) != 0 {
		t.Errorf("err %v, %d requests, comments %v; want nothing written", err, reads, s.Comments)
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

// An owner hold an ancestor's /resume ended counts no more, but for the
// /resume itself; a hold by hand, or one no record names, stays.
func TestResolveLeftAndOwnerHeld(t *testing.T) {
	m := func(reason, cmd string) github.Marker {
		f := map[string]string{"reason": reason}
		if cmd != "" {
			f["cmd"] = cmd
		}
		return github.Marker{Kind: "transition", Fields: f}
	}
	holds := []github.Marker{m("discussion", ""), m("owner", "5"), m("owner", "6"), m("owner", "")}
	p := resolvePeriod{owners: map[string]string{"5": "9", "": "9"}}
	if got := resolveLeft(holds, p, 0); !slices.EqualFunc(got, []github.Marker{holds[0], holds[2], holds[3]}, resolveSameMarker) {
		t.Errorf("left %v; want all but the hold /hold 5 made", got)
	}
	if got := resolveLeft(holds, p, 9); len(got) != 4 {
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
