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

// This file tests #417: the App's own close of an issue at the clarifying
// after a hold, as a parent's revision closes a dropped child, carries no
// origin (SPEC §5), so a trusted reopen's hold and the /resume after it
// take the origin of the move before the close (SPEC §6.4), and the
// interviewer's unchanged still goes back to where the issue was before
// its first discussion hold. And a /resume to a stage a parent's revision
// set as the hold's at, which isn't that clarifying, carries no origin, so
// it doesn't make a story's first plan wait for review (SPEC §7.2,
// Skipping plan review).

// holdOriginDropKey is the key of the parent's revision's moves of its
// children, the parent's comment holding the revision's move to planned
// (SPEC §5).
const holdOriginDropKey = "p77"

// holdOriginDropped is holdOriginAfter, then the App's close of issue 1 as
// a parent's revision that drops it closes it (droppedOne): clarifying →
// closed, as not planned, keyed holdOriginDropKey, which the App then
// judged, swapping stage:clarifying for stage:discarded. The writes are
// forgotten.
func holdOriginDropped(t testing.TB, f *githubtest.Fake) {
	t.Helper()
	holdOriginAfter(t, f)
	revMove(t, f, 1, daemon.Move{To: state.Closed, StateReason: "not_planned", Trigger: state.TriggerClosed,
		Key: state.Key{Field: "event", ID: holdOriginDropKey}, Text: "Closed as not planned, since #9's plan, rev 2, drops S1."})
	f.AddLabel(1, app, "stage:discarded")
	f.RemoveLabel(1, app, "stage:clarifying")
	f.ForgetWrites()
}

// holdOriginDroppedReopened is holdOriginDropped, then the owner's reopen,
// whose event it sets in event.
func holdOriginDroppedReopened(event *int64) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		holdOriginDropped(t, f)
		*event = f.ReopenBy(1, "owner")
		return f
	}
}

// The App's close of a dropped child at the clarifying after a hold
// carries no origin, as a parent's revision's move of its child carries
// none (SPEC §5). A trusted reopen then holds it at clarifying with the
// origin of the move before the close, spec-review. The write is safe to
// replay, and an unchanged pass after it writes nothing and sends only
// conditional GETs answered 304.
func TestHoldOriginDroppedReopen(t *testing.T) {
	t.Parallel()
	var event int64
	setup := holdOriginDroppedReopened(&event)
	closingCheck(t, setup, func(f *githubtest.Fake) closingWant {
		bodies := holdOriginBodies(f)
		dropped := ivHoldMove(t, bodies[len(bodies)-1])
		ivHoldFields(t, dropped, map[string]string{"from": "clarifying", "to": "closed", "event": holdOriginDropKey})
		if o, ok := dropped.Fields["origin"]; ok {
			t.Errorf("the dropped child's close carries origin=%s; want none (SPEC §5)", o)
		}
		// The close carries no seen, so the round's bound is its comment.
		seen := ivHoldFind(t, f, "to=closed ")
		held := fmt.Sprintf("`owner` reopened this issue, so it's on hold, at `clarifying`, where it was before it closed. `/resume` here puts it back there.\n\n"+
			"<!-- issuedog:transition from=closed to=held at=clarifying origin=spec-review reason=owner event=%d seen=%d -->", event, seen)
		return closingWant{labels: []string{"stage:held", "type:epic"}, comments: append(bodies[1:], held)}
	})
	f := setup(t)
	d := newDaemon(t, f, digestNotifyOrigin(t), nil)
	iterate(t, d)
	iterate(t, d)
	if bad := discUnchanged(t, f, d); len(bad) != 0 {
		t.Errorf("the unchanged pass sent %+v; want only conditional GETs answered 304", bad)
	}
}

// holdOriginDroppedHeld is holdOriginDroppedReopened, then a pass that
// holds issue 1 for the owner's reopen.
func holdOriginDroppedHeld(t testing.TB) *githubtest.Fake {
	t.Helper()
	var event int64
	f := holdOriginDroppedReopened(&event)(t)
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if c := newestComment(f, 1); !strings.Contains(c, "from=closed to=held at=clarifying origin=spec-review ") {
		t.Fatalf("newest comment %q; want the reopen's hold, with origin", c)
	}
	f.ForgetWrites()
	return f
}

// The owner's /resume of a dropped child that a trusted reopen held, from
// the clarifying after a hold, goes back to that clarifying with the
// origin of the move before the App's close, not the close's, which has
// none. The write is safe to replay.
func TestHoldOriginDroppedResume(t *testing.T) {
	t.Parallel()
	var resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f := holdOriginDroppedHeld(t)
		resume = f.AddComment(1, "owner", "/resume")
		return f
	}
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	ivHoldFields(t, ivHoldMove(t, newestComment(f, 1)), map[string]string{"from": "held", "to": "clarifying", "origin": "spec-review",
		"cmd": strconv.FormatInt(resume, 10)})
}

// Once a dropped child's reopen hold ends, the interviewer's unchanged
// takes it back to spec-review, where it was before its first discussion
// hold, not to clarifying: after a /resume alone, which takes the origin
// from the move before the close, and after a discussion during the hold,
// which sends it to clarifying by the hold's origin.
func TestHoldOriginDroppedUnchanged(t *testing.T) {
	t.Parallel()
	for name, discussion := range map[string]bool{"a /resume": false, "a /resume after a discussion": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := holdOriginDroppedHeld(t)
			var more func(f *githubtest.Fake)
			if discussion {
				more = holdOriginDiscussion(t, true)
			}
			holdOriginUnchanged(t, f, more)
		})
	}
}

// holdOriginChanged is holdOriginAfter, then the owner's /hold of issue 1
// at that clarifying, which a pass applies, then a parent's revision's
// change of it, which sets its hold's at and origin to planning by a held →
// held marker with no reason (SPEC §5, §6.4), then the owner's /resume,
// whose comment's ID it sets in resume.
func holdOriginChanged(resume *int64) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		holdOriginAfter(t, f)
		f.AddComment(1, "owner", "/hold")
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		hold := ivHoldMove(t, newestComment(f, 1))
		ivHoldFields(t, hold, map[string]string{"to": "held", "at": "clarifying", "origin": "spec-review", "reason": "owner"})
		f.AddComment(1, app, "Changed by #9's plan, rev 2. It stays on hold, and `/resume` takes it to `stage:planning`.\n\n"+
			"<!-- issuedog:transition from=held to=held at=planning origin=planning event="+holdOriginDropKey+" seen="+hold.Fields[state.FieldSeen]+" -->")
		f.ForgetWrites()
		*resume = f.AddComment(1, "owner", "/resume")
		return f
	}
}

// A /resume of an issue held from the clarifying after a hold, whose hold's
// at a parent's revision changed to planning, goes to planning with no
// origin: it's no move out of that clarifying, so the old origin doesn't
// ride onto it and make a story's first plan wait for review. The write is
// safe to replay.
func TestHoldOriginChangedResume(t *testing.T) {
	t.Parallel()
	var resume int64
	setup := holdOriginChanged(&resume)
	githubtest.Replay(t, setup, resolvePass(t, 1))
	f := run(t, setup, resolvePass(t, 1))
	m := ivHoldMove(t, newestComment(f, 1))
	ivHoldFields(t, m, map[string]string{"from": "held", "to": "planning", "cmd": strconv.FormatInt(resume, 10)})
	if o, ok := m.Fields["origin"]; ok {
		t.Errorf("the /resume to planning carries origin=%s; want none", o)
	}
	if l := ivLabels(f, 1); !slices.Equal(l, []string{"stage:planning", "type:epic"}) {
		t.Errorf("labels %q; want stage:planning", l)
	}
}
