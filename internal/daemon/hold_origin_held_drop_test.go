package daemon_test

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file tests #497: a parent's revision that drops a held child closes
// it from held, which ends its hold, and its close carries no at or origin
// (SPEC §5). A trusted reopen then holds it again with the first hold's
// at, origin and seen, as on an issue that was held when it closed (SPEC
// §6.4), so the /resume after it goes back where that hold began, and
// carries no origin once a revision had changed the hold's at.

// holdOriginHeldDropKey is the key of the revision's close of the held
// child: another revision than holdOriginDropKey's, which changes it first
// in holdOriginHeldDropChanged.
const holdOriginHeldDropKey = "p78"

// holdOriginHeldDropCase is a held child that a revision drops: changed,
// whether a revision changed its hold's at and origin to planning first,
// and the hold the reopen holds it again with.
type holdOriginHeldDropCase struct {
	name       string
	changed    bool
	at, origin string
}

// holdOriginHeldDropCases are the held children a revision drops: one held
// from the clarifying after a hold, whose first hold has origin
// spec-review, and one a revision changed to planning first.
var holdOriginHeldDropCases = []holdOriginHeldDropCase{
	{"held from the clarifying after a hold", false, "clarifying", "spec-review"},
	{"changed by a revision first", true, "planning", "planning"},
}

// holdOriginHeldDropped is holdOriginAfter, then the owner's /hold of issue
// 1 at that clarifying, which a pass applies, then, with changed, a
// parent's revision's change of its hold's at and origin to planning by a
// held → held marker with no reason (SPEC §5, §6.4), then the App's close
// of it from held as a parent's revision that drops it closes it
// (droppedOne), keyed holdOriginHeldDropKey, which the App then judged,
// swapping stage:held for stage:discarded, then the owner's reopen, whose
// event it sets in event. It sets seen to the first hold's seen. The
// writes are forgotten.
func holdOriginHeldDropped(changed bool, event, seen *int64) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		holdOriginAfter(t, f)
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		hold := ivHoldMove(t, newestComment(f, 1))
		ivHoldFields(t, hold, map[string]string{"to": "held", "at": "clarifying", "origin": "spec-review", "reason": "owner"})
		s, err := strconv.ParseInt(hold.Fields[state.FieldSeen], 10, 64)
		if err != nil {
			t.Fatalf("the hold's seen: %v", err)
		}
		*seen = s
		if changed {
			f.AddComment(1, app, "Changed by #9's plan, rev 2. It stays on hold, and `/resume` takes it to `stage:planning`.\n\n"+
				"<!-- issuedog:transition from=held to=held at=planning origin=planning event="+holdOriginDropKey+" seen="+hold.Fields[state.FieldSeen]+" -->")
		}
		revMove(t, f, 1, daemon.Move{To: state.Closed, StateReason: "not_planned", Trigger: state.TriggerClosed,
			Key: state.Key{Field: "event", ID: holdOriginHeldDropKey}, Text: "Closed as not planned, since #9's plan, rev 3, drops S1."})
		f.AddLabel(1, app, "stage:discarded")
		f.RemoveLabel(1, app, "stage:held")
		f.ForgetWrites()
		*event = f.ReopenBy(1, "owner")
		return f
	}
}

// holdOriginHeldDropHeld is holdOriginHeldDropped, then a pass that holds
// issue 1 for the owner's reopen, then the owner's /resume, whose comment's
// ID it sets in resume.
func holdOriginHeldDropHeld(tc holdOriginHeldDropCase, resume *int64) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		var event, seen int64
		f := holdOriginHeldDropped(tc.changed, &event, &seen)(t)
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		ivHoldFields(t, ivHoldMove(t, newestComment(f, 1)), map[string]string{"from": "closed", "to": "held", "at": tc.at, "origin": tc.origin})
		f.ForgetWrites()
		*resume = f.AddComment(1, "owner", "/resume")
		return f
	}
}

// The App's close of a held child that a revision drops ends its hold. A
// trusted reopen then holds it with the first hold's at, origin and seen,
// as for an issue that was held when it closed (SPEC §6.4), not at
// clarifying with no origin, and with a revision's change of the hold
// kept. The write is safe to replay, and an unchanged pass after it
// writes nothing and sends only conditional GETs answered 304.
func TestHoldOriginHeldDropReopen(t *testing.T) {
	t.Parallel()
	for _, tc := range holdOriginHeldDropCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var event, seen int64
			setup := holdOriginHeldDropped(tc.changed, &event, &seen)
			closingCheck(t, setup, func(f *githubtest.Fake) closingWant {
				bodies := holdOriginBodies(f)
				dropped := ivHoldMove(t, bodies[len(bodies)-1])
				ivHoldFields(t, dropped, map[string]string{"from": "held", "to": "closed", "event": holdOriginHeldDropKey})
				for _, k := range []string{"at", "origin"} {
					if v, ok := dropped.Fields[k]; ok {
						t.Errorf("the held child's close carries %s=%s; want none (SPEC §5)", k, v)
					}
				}
				held := fmt.Sprintf("`owner` reopened this issue, so it's on hold, at `%s`, where it was before it closed. `/resume` here puts it back there.\n\n"+
					"<!-- issuedog:transition from=closed to=held at=%s origin=%s reason=owner event=%d seen=%d -->", tc.at, tc.at, tc.origin, event, seen)
				return closingWant{labels: []string{"stage:held", "type:epic"}, comments: append(bodies[1:], held)}
			})
			f := setup(t)
			d := newDaemon(t, f, digestNotifyOrigin(t), nil)
			iterate(t, d)
			iterate(t, d)
			if bad := discUnchanged(t, f, d); len(bad) != 0 {
				t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", bad)
			}
		})
	}
}

// The owner's /resume of a held child that a revision dropped and a
// trusted reopen held lands where its first hold began: at the clarifying
// after a hold, with that clarifying's origin, or at planning, where a
// revision changed the hold to, with no origin, so the old origin doesn't
// ride onto it. The write is safe to replay, and an unchanged pass after it
// sends only conditional GETs answered 304.
func TestHoldOriginHeldDropResume(t *testing.T) {
	t.Parallel()
	for _, tc := range holdOriginHeldDropCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var resume int64
			setup := holdOriginHeldDropHeld(tc, &resume)
			githubtest.Replay(t, setup, resolvePass(t, 1))
			f := run(t, setup, resolvePass(t, 1))
			m := ivHoldMove(t, newestComment(f, 1))
			ivHoldFields(t, m, map[string]string{"from": "held", "to": tc.at, "cmd": strconv.FormatInt(resume, 10)})
			want := "stage:" + tc.at
			if tc.changed {
				if o, ok := m.Fields["origin"]; ok {
					t.Errorf("the /resume to planning carries origin=%s; want none", o)
				}
			} else {
				ivHoldFields(t, m, map[string]string{"origin": tc.origin})
			}
			if l := ivLabels(f, 1); !slices.Equal(l, []string{want, "type:epic"}) {
				t.Errorf("labels %q; want %s", l, want)
			}
			holdOriginUnchangedPass(t, setup, want)
		})
	}
}

// Once a held child that a revision dropped is reopened and resumed, the
// interviewer's unchanged takes it back to spec-review, where it was before
// its first discussion hold. The /resume's move, which the interviewer's
// run reads, carries the first hold's seen, which the reopen's hold kept,
// not the bound after the close, so the comments since that hold's bound
// are the interviewer's input (SPEC §5, §6.4), and that clarifying's
// origin.
func TestHoldOriginHeldDropUnchanged(t *testing.T) {
	t.Parallel()
	var event, seen int64
	f := holdOriginHeldDropped(false, &event, &seen)(t)
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.ForgetWrites()
	holdOriginUnchanged(t, f, nil)
	var resumed []string
	for _, c := range f.State().Issues[0].Comments {
		if strings.Contains(c.Body, "<!-- issuedog:transition from=held to=clarifying ") {
			resumed = append(resumed, c.Body)
		}
	}
	if len(resumed) == 0 {
		t.Fatal("no move from held to clarifying; want the /resume's")
	}
	ivHoldFields(t, ivHoldMove(t, resumed[len(resumed)-1]), map[string]string{"to": "clarifying", "origin": "spec-review",
		state.FieldSeen: strconv.FormatInt(seen, 10)})
}

// A held child that a revision dropped, whose first hold's comment the
// owner edited after the close, so its markers count for nothing (SPEC
// §5.1): a trusted reopen holds it at the stage the markers left before
// that hold show, the clarifying after a hold, with that clarifying's
// origin and bound, as for hidden markers on an open issue (SPEC §6.4),
// not at clarifying with no origin. The write is safe to replay.
func TestHoldOriginHeldDropDirtyHold(t *testing.T) {
	t.Parallel()
	var event, seen int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := holdOriginHeldDropped(false, &event, &seen)(t)
		hold := ivHoldFind(t, f, "from=clarifying to=held at=clarifying origin=spec-review reason=owner ")
		f.EditComment(hold, "owner", "On hold, by the owner.")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	m := ivHoldMove(t, newestComment(f, 1))
	ivHoldFields(t, m, map[string]string{"from": "closed", "to": "held", "at": "clarifying", "origin": "spec-review", "reason": "owner",
		"event": strconv.FormatInt(event, 10), state.FieldSeen: strconv.FormatInt(seen, 10)})
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:held", "type:epic"}) {
		t.Errorf("labels %q; want stage:held", l)
	}
}

// holdOriginUnchangedPass runs the daemon twice on a fresh fake from setup,
// checks that issue 1 then has the stage label want, and that an unchanged
// pass after it sends only conditional GETs answered 304.
func holdOriginUnchangedPass(t *testing.T, setup func(testing.TB) *githubtest.Fake, want string) {
	t.Helper()
	f := setup(t)
	d := newDaemon(t, f, digestNotifyOrigin(t), nil)
	iterate(t, d)
	iterate(t, d)
	if l := ivLabels(f, 1); !slices.Equal(l, []string{want, "type:epic"}) {
		t.Errorf("labels %q after the daemon's passes; want %s", l, want)
	}
	if bad := discUnchanged(t, f, d); len(bad) != 0 {
		t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", bad)
	}
}
