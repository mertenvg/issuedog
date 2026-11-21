package daemon

import (
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A task a revision dropped while its PR is open, on its own (SPEC §6.4,
// §9.4): its digest item, its hold's fields and key, and the order step 2
// closes such issues in.

// droppedItemHold is the App's comment holding task 1, from pr-open, for
// PR 7, as droppedOne writes it.
const droppedItemHold = "On hold, not closed, since #4's plan, rev 2, drops T1, while its PR #7 is open ([the plan](https://github.com/o/r/issues/4#issuecomment-30)).\n\n" +
	"Once the PR merges, its `Closes` closes this issue, and once it's closed without merging, I close this issue as not planned. " +
	"Or close it yourself. `/resume` doesn't end this hold.\n\n" +
	"<!-- issuedog:transition from=pr-open to=held at=pr-open reason=dropped event=p20 -->"

// A task held for its PR, listed open, has the item dropped-<n>: it ranks
// as a discussion hold, copies the first paragraph of its hold's comment,
// names the PR and its raw title, says how the hold ends, links the PR,
// and assigns the issue, not the PR. One whose hold's comment someone
// else edited has none, as its hold isn't read. With its PR listed closed
// or merged, or none listed, or held for another reason, it has none. Its
// kind reads back from the digest's body.
func TestWaitFindDropped(t *testing.T) {
	t.Parallel()
	open := &listedPR{Number: 7, Title: "feat: x <b> @eve", Open: true}
	in := waitIn(prItemSnap("held", prItemMoved("pr-open", " pr=7"), droppedItemHold), Dirt{})
	in.PR = open
	it := waitFindOne(t, in)
	want := []waitLine{
		{Text: "On hold, not closed, since #4's plan, rev 2, drops T1, while its PR #7 is open ([the plan](https://github.com/o/r/issues/4#issuecomment-30)).", Copied: true},
		{Text: "feat: x <b> @eve", PR: 7},
		{Text: "Merge the PR, and its `Closes` closes this task; close the PR, and I close the task as not planned; or close the task yourself. `/resume` doesn't end this hold."},
	}
	if it.Kind != waitDropped || it.Key != "dropped-1" || it.Link != "https://github.com/o/r/pull/7" || it.AssignPR != 0 || it.Comment != state.BodyID ||
		!slices.Equal(it.Lines, want) {
		t.Errorf("the item = %+v; want one keyed dropped-1, linking PR 7, assigning the issue, its lines %+v", it, want)
	}
	if waitDropped.rank() != waitHold.rank() || waitDropped.String() != "dropped" || waitKeyPrefix[waitDropped] != "dropped" {
		t.Errorf("a dropped task ranks %d, a hold %d, named %s, keyed %s; want the same rank, dropped and dropped",
			waitDropped.rank(), waitHold.rank(), waitDropped, waitKeyPrefix[waitDropped])
	}
	if k, ok := digestKeptKind(digestRenderKindName(waitDropped)); !ok || k != waitDropped {
		t.Errorf("the kind named %q reads back as %s, %t; want dropped", digestRenderKindName(waitDropped), k, ok)
	}

	s := prItemSnap("held", prItemMoved("pr-open", " pr=7"), droppedItemHold)
	edited := waitIn(s, Dirt{s.Comments[len(s.Comments)-1].ID: "mallory"})
	edited.PR = open
	if items, err := waitFind(edited); err != nil || slices.ContainsFunc(items, func(it waitItem) bool { return it.Kind == waitDropped }) {
		// Its marker is in dirty content, which isn't read, so the issue
		// isn't held for its PR at all, as the markers left show.
		t.Errorf("with its hold's comment edited, the items are %+v, %v; want no dropped task's", items, err)
	}

	closed, merged := *open, *open
	closed.Open, merged.Open, merged.Merged = false, false, true
	for name, c := range map[string]struct {
		hold string
		pr   *listedPR
	}{
		"no PR listed":      {droppedItemHold, nil},
		"its PR closed":     {droppedItemHold, &closed},
		"its PR merged":     {droppedItemHold, &merged},
		"held by the owner": {"<!-- issuedog:transition from=pr-open to=held at=pr-open reason=owner cmd=900 -->", open},
	} {
		in := waitIn(prItemSnap("held", prItemMoved("pr-open", " pr=7"), c.hold), Dirt{})
		in.PR = c.pr
		if items, err := waitFind(in); err != nil || slices.ContainsFunc(items, func(it waitItem) bool { return it.Kind == waitDropped }) {
			t.Errorf("%s: the items are %+v, %v; want no dropped task's", name, items, err)
		}
	}
}

// A hold for a PR records where the issue was, with reason=dropped: an
// open issue's stage and round bound, and a held one's first hold's at,
// origin and bound, which a held → held marker keeps. Its key is the one
// its marker carries, and the issue is held so only while that marker is
// one of its holds.
func TestDroppedHoldFields(t *testing.T) {
	t.Parallel()
	open := state.Derive(prItemSnap("pr-open", prItemMoved("pr-open", " pr=7 seen=15")), "issuedog", Dirt{}.Has)
	if got := droppedHoldFields(open); got["reason"] != "dropped" || got["at"] != "pr-open" || got["seen"] != "15" || len(got) != 3 {
		t.Errorf("an open task's hold fields = %v; want reason=dropped at=pr-open seen=15", got)
	}
	held := state.Derive(prItemSnap("held", prItemMoved("pr-open", " pr=7"),
		"<!-- issuedog:transition from=pr-open to=held at=pr-open reason=discussion origin=ready event=c9 seen=12 -->"), "issuedog", Dirt{}.Has)
	if got := droppedHoldFields(held); got["reason"] != "dropped" || got["at"] != "pr-open" || got["origin"] != "ready" || got["seen"] != "12" || len(got) != 4 {
		t.Errorf("a held task's hold fields = %v; want reason=dropped, and its first hold's at, origin and seen", got)
	}
	if _, ok := droppedHeld(held); ok {
		t.Error("a task held for discussion is held for its PR; want not")
	}
	both := state.Derive(prItemSnap("held", prItemMoved("pr-open", " pr=7"),
		"<!-- issuedog:transition from=pr-open to=held at=pr-open reason=owner cmd=900 -->",
		"<!-- issuedog:transition from=held to=held at=pr-open reason=dropped event=p20-closed -->"), "issuedog", Dirt{}.Has)
	m, ok := droppedHeld(both)
	if key, kok := droppedHoldKey(m); !ok || !kok || key != (state.Key{Field: "event", ID: "p20-closed"}) {
		t.Errorf("an owner-held task held for its PR too gives %v, %t, its key %v, %t; want its held → held marker, keyed event=p20-closed", m, ok, key, kok)
	}
	if _, ok := droppedHoldKey(github.Marker{Kind: "transition", Fields: map[string]string{"to": "held"}}); ok {
		t.Error("a marker with no key has one; want none")
	}
}

// Step 2 closes the issues held for a PR deepest first, by the parents in
// the snapshots held, then by number, so an issue closes after those below
// it. A loop of parents ends.
func TestDroppedOrder(t *testing.T) {
	t.Parallel()
	d := &Daemon{snaps: map[int]*github.Snapshot{
		2: {Number: 2, Parent: 1}, 4: {Number: 4, Parent: 2}, 5: {Number: 5, Parent: 4}, 3: {Number: 3, Parent: 2},
		8: {Number: 8, Parent: 9}, 9: {Number: 9, Parent: 8},
	}, droppedDirt: map[int]Dirt{2: nil, 3: nil, 4: nil, 5: nil, 8: nil}}
	if got, want := d.droppedOrder(), []int{5, 3, 4, 8, 2}; !slices.Equal(got, want) {
		t.Errorf("the order = %v; want %v", got, want)
	}
	if got := d.droppedBelow(4); got != 5 {
		t.Errorf("below #4 is #%d; want #5", got)
	}
	d.snaps[5].Closed = true
	d.closingBelow = map[int]int{6: 4}
	if got := d.droppedBelow(4); got != 6 {
		t.Errorf("below #4 is #%d; want #6, whose close is still to judge", got)
	}
	delete(d.closingBelow, 6)
	if got := d.droppedBelow(4); got != 0 {
		t.Errorf("below #4 is #%d; want none", got)
	}
}
