package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/state"
)

// The stalled task's item for the PR step's waits, and for a closed PR at
// pr-open, on their own (SPEC §9.4).

// prStallTip is someone else's tip on the task's branch in these
// snapshots.
var prStallTip = strings.Repeat("ef", 20)

// The PR step's wait on a task at verifying, given to waitFind, is a
// stalled task's item keyed stalled-<n>-<run>, by the pass's run, ranked
// as a hand-off: for someone else's open PR, it names and links that PR,
// or with its number unknown, names none and links the issue; for someone
// else's tip, it names the head and the tip, and links the issue. Each
// says it holds the worker and what the owner can do. A pass that moved
// the task out once already (waitStalledRun) is the item instead, as the
// PR step then judges nothing, and at any other stage there's none.
func TestWaitFindPRStall(t *testing.T) {
	t.Parallel()
	holds := " It holds the worker, so no other task is picked."
	for name, c := range map[string]struct {
		st    stall
		link  string
		lines []string
	}{
		"someone else's PR": {stall{Why: stalledPR, Run: "rv", PR: 9}, "https://github.com/o/r/pull/9", []string{
			"This task's head passed, but someone else's open PR #9 from its branch to `main` stands, so GitHub won't open the task's, and it waits at `verifying`." + holds,
			"Close PR #9, and the task's opens once a poll lists it closed; or set `stage:ready` to free the worker for a new round, though its next head waits here again while that PR is open.",
		}},
		"someone else's PR, unknown": {stall{Why: stalledPR, Run: "rv"}, "https://github.com/o/r/issues/1", []string{
			"This task's head passed, but someone else's open PR from its branch to `main` stands, so GitHub won't open the task's, and it waits at `verifying`." + holds,
			"Close that PR, and the task's opens once a poll lists it closed; or set `stage:ready` to free the worker for a new round, though its next head waits here again while that PR is open.",
		}},
		"someone else's tip": {stall{Why: stalledTip, Run: "rv", Head: prItemHead, Tip: prStallTip}, "https://github.com/o/r/issues/1", []string{
			"This task's head `" + prItemHead + "` passed, but its branch is at `" + prStallTip + "`, which isn't in the head's history and no App marker names, " +
				"so it's someone else's commits: nothing is pushed over them, and it waits at `verifying`." + holds,
			"Reset the branch to a commit in `" + prItemHead + "`'s history, and the head is pushed and its PR opened; or set `stage:ready` to free the worker for a new round, though its next head waits here again while the branch is at those commits.",
		}},
	} {
		in := waitIn(prItemSnap("verifying"), Dirt{})
		in.Stall = &prStall{stall: c.st}
		it := waitFindOne(t, in)
		if it.Kind != waitStalled || it.Key != "stalled-1-rv" || it.Link != c.link || it.AssignPR != 0 || it.ClosedPR != 0 || it.Comment != state.BodyID ||
			it.Priority != 1 || it.Title != in.S.Title || !it.Created.Equal(in.S.CreatedAt) || !slices.Equal(waitTexts(it.Lines), c.lines) {
			t.Errorf("%s: the item = %+v; want a stalled task keyed stalled-1-rv, linking %s, p1, its lines %q", name, it, c.link, c.lines)
		}
	}

	wait := &prStall{stall: stall{Why: stalledTip, Run: "rv", Head: prItemHead, Tip: prStallTip}}
	in := waitIn(prItemSnap("verifying", prItemMoved("pr-open", " pr=7"), "<!-- issuedog:transition from=pr-open to=verifying event=l900 -->"), Dirt{})
	in.Stall = wait
	if it := waitFindOne(t, in); !strings.HasPrefix(it.Lines[0].Text, "This task is back at `verifying`, but its pass, run `rv`, moved it") {
		t.Errorf("with the pass moved once already, the item = %+v; want that pass's", it)
	}
	in = waitIn(prItemSnap("pr-open", prItemMoved("pr-open", " pr=7")), Dirt{})
	in.Stall = wait
	if items, err := waitFind(in); err != nil || len(items) != 0 {
		t.Errorf("at pr-open, got %+v, %v; want no item", items, err)
	}
}

// After a restart, the item read back from the digest's body stands for
// the PR step's wait: waitFind gives it as the body showed it, with the
// issue's created time for its rank.
func TestWaitFindPRStallKept(t *testing.T) {
	t.Parallel()
	kept := waitItem{Issue: 1, Kind: waitStalled, Priority: 1, Link: "https://github.com/o/r/pull/9", Comment: state.BodyID,
		Kept: &waitKept{Title: "Refuse", Lines: []string{"- It waits."}}}
	in := waitIn(prItemSnap("verifying"), Dirt{})
	in.Stall = &prStall{Kept: &kept}
	it := waitFindOne(t, in)
	if it.Kind != waitStalled || it.Kept == nil || it.Kept.Lines[0] != "- It waits." || it.Link != kept.Link || !it.Created.Equal(in.S.CreatedAt) {
		t.Errorf("the item = %+v; want the one read back, with the issue's created time", it)
	}
}

// A task at pr-open whose PR is listed closed, not merged, with that close
// handed off already, as after a stage:pr-open set back by hand, is
// stalled: its item, stalled-<n>-pr<PR>, says the close moves it no more,
// to reopen the PR or set stage:ready, or close the issue, links the PR,
// and carries it as ClosedPR, while assignment assigns the issue. A close
// not handed off yet, which step 2 hands off, or a PR merged, has none.
func TestWaitFindStalledClosed(t *testing.T) {
	t.Parallel()
	closed := listedPR{Number: 7, Title: "feat: x", ClosedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	handoff := "<!-- issuedog:transition from=pr-open to=needs-human event=" + prClosedKey(closed).ID + " -->"
	hand := "<!-- issuedog:transition from=needs-human to=pr-open event=l900 -->"
	in := waitIn(prItemSnap("pr-open", prItemMoved("pr-open", " pr=7"), handoff, hand), Dirt{})
	in.PR = &closed
	it := waitFindOne(t, in)
	want := []string{
		"This task is at `pr-open`, but its PR #7 is closed, and its close was handed off already, so the close moves it no more.",
		"Reopen PR #7 to wait on its review, or set `stage:ready` for a new round, or close this issue.",
	}
	if it.Kind != waitStalled || it.Key != "stalled-1-pr7" || it.Link != "https://github.com/o/r/pull/7" || it.ClosedPR != 7 || it.AssignPR != 0 ||
		!slices.Equal(waitTexts(it.Lines), want) {
		t.Errorf("the item = %+v; want a stalled task keyed stalled-1-pr7, linking PR 7, carrying it as ClosedPR, its lines %q", it, want)
	}
	if prClosedKey(closed).ID != "c7-1767323045" {
		t.Errorf("the close's key = %s; want c7-1767323045", prClosedKey(closed).ID)
	}

	merged := closed
	merged.Merged = true
	later := closed
	later.ClosedAt = closed.ClosedAt.Add(time.Hour)
	for name, c := range map[string]struct {
		pr       listedPR
		comments []string
	}{
		"a close not handed off yet": {later, []string{prItemMoved("pr-open", " pr=7"), handoff, hand}},
		"no hand-off":                {closed, []string{prItemMoved("pr-open", " pr=7")}},
		"its PR merged":              {merged, []string{prItemMoved("pr-open", " pr=7"), handoff, hand}},
	} {
		in := waitIn(prItemSnap("pr-open", c.comments...), Dirt{})
		in.PR = &c.pr
		if items, err := waitFind(in); err != nil || len(items) != 0 {
			t.Errorf("%s: got %+v, %v; want no item", name, items, err)
		}
	}
}

// A task back at verifying by a new head that's the head its pass moved
// it out on once already, as when an admin resets the branch to a head
// it verified before, is stalled with its own first line: the branch was
// put back at that head. A stage set by hand keeps the first line it had.
func TestWaitFindStalledReset(t *testing.T) {
	t.Parallel()
	reset := "<!-- issuedog:transition from=pr-open to=verifying event=n" + prItemHead + " head=" + prItemHead + " -->"
	it := waitFindOne(t, waitIn(prItemSnap("verifying", prItemMoved("pr-open", " pr=7"), reset), Dirt{}))
	want := []string{
		"This task's branch was put back at `" + prItemHead + "`, a head it verified before, so it's back at `verifying`, but its pass there, run `rv`, " +
			"moved it to `pr-open` once already, so the pass moves it no more. It holds the worker, so no other task is picked.",
		"Set `stage:pr-open` back to wait on its PR, or set `stage:ready` for a new round.",
	}
	if it.Kind != waitStalled || it.Key != "stalled-1-rv" || !slices.Equal(waitTexts(it.Lines), want) {
		t.Errorf("the item = %+v; want stalled-1-rv, its lines %q", it, want)
	}
}

// The PR step's wait stands only while its task is at verifying and hasn't
// moved since it was found: any other is dropped. Setting it, or ending
// it, finds the task's cached items again, its other items kept, and
// ending one of another reason changes nothing, but an item read back
// after a restart ends on any.
func TestPRStalled(t *testing.T) {
	t.Parallel()
	s := prItemSnap("verifying")
	is := state.Derive(s, waitSlug, Dirt{}.Has)
	other := waitItem{Issue: 1, Kind: waitError, Key: "err-1-x"}
	d := &Daemon{Step: &Step{Applier: &Applier{Owner: "o", Repo: "r"}}, digestCache: map[int][]waitItem{1: {other}}}
	d.prStallSet(s, is, stall{Why: stalledTip, Run: "rv", Head: prItemHead, Tip: prStallTip})
	if got := d.digestCache[1]; len(got) != 2 || got[0].Key != "err-1-x" || got[1].Key != "stalled-1-rv" || got[1].Lines[0].Text == "" {
		t.Fatalf("once set, the cached items = %+v; want the error, then the stalled task", got)
	}
	d.prStallEnd(s, is, stalledPR)
	if len(d.digestCache[1]) != 2 {
		t.Errorf("ending another reason left %+v; want both items", d.digestCache[1])
	}
	d.prStallEnd(s, is, stalledTip)
	if got := d.digestCache[1]; len(got) != 1 || got[0].Key != "err-1-x" {
		t.Errorf("once ended, the cached items = %+v; want the error alone", got)
	}
	d.prStalls = map[int]prStall{1: {Kept: &waitItem{Issue: 1, Kind: waitStalled}, Move: is.TransitionComment}}
	d.digestCache[1] = []waitItem{other, {Issue: 1, Kind: waitStalled}}
	d.prStallEnd(s, is, stalledPR)
	if _, ok := d.prStalls[1]; ok || len(d.digestCache[1]) != 1 {
		t.Errorf("an item read back, ended by the PR step's judgment: %+v, cached %+v; want it gone", d.prStalls, d.digestCache[1])
	}

	d.prStallSet(s, is, stall{Why: stalledPR, Run: "rv", PR: 9})
	if _, ok := d.prStalled(1, is); !ok {
		t.Errorf("the wait doesn't stand on the task as it was found")
	}
	moved := state.Derive(prItemSnap("verifying", "<!-- issuedog:transition from=verifying to=verifying event=l900 -->"), waitSlug, Dirt{}.Has)
	if _, ok := d.prStalled(1, moved); ok {
		t.Errorf("the wait stands on the task moved since; want it dropped")
	}
	if _, ok := d.prStalls[1]; ok {
		t.Errorf("a wait that doesn't stand is kept: %+v", d.prStalls)
	}
	d.prStallSet(s, is, stall{Why: stalledPR, Run: "rv", PR: 9})
	ready := is
	ready.Stage = state.Ready
	if _, ok := d.prStalled(1, ready); ok {
		t.Errorf("the wait stands on the task at ready; want it dropped")
	}
}

// A stalled task's item on its closed PR stands only while the poll lists
// that PR closed and not merged, and every other item stands.
func TestPRStallStands(t *testing.T) {
	t.Parallel()
	it := waitItem{Issue: 1, Kind: waitStalled, ClosedPR: 7}
	d := &Daemon{prs: map[int]listedPR{7: {Number: 7}}}
	if !d.prStallStands(it) || !d.prStallStands(waitItem{Kind: waitStalled}) {
		t.Errorf("with its PR closed, the item doesn't stand, or another doesn't")
	}
	d.digestCache = map[int][]waitItem{1: {it, {Issue: 1, Kind: waitError}}}
	if got := d.digestCacheOf(1); len(got) != 2 {
		t.Errorf("with its PR closed, the items = %+v; want both", got)
	}
	for name, p := range map[string]listedPR{"reopened": {Number: 7, Open: true}, "merged": {Number: 7, Merged: true}} {
		d.prs[7] = p
		if got := d.digestCacheOf(1); d.prStallStands(it) || len(got) != 1 || got[0].Kind != waitError {
			t.Errorf("with its PR %s, the item stands, or the items are %+v; want the error alone", name, got)
		}
	}
}

// After a restart, the stalled task's item read back from the digest's
// body stands for the PR step's wait only on a task at verifying: one at
// any other stage, or with none read back, keeps nothing.
func TestPRStallKept(t *testing.T) {
	t.Parallel()
	kept := waitItem{Issue: 1, Kind: waitStalled, Kept: &waitKept{}}
	d := &Daemon{digestKept: map[int][]waitItem{1: {{Issue: 1, Kind: waitError, Kept: &waitKept{}}, kept}}}
	open := state.Derive(prItemSnap("pr-open", prItemMoved("pr-open", " pr=7")), waitSlug, Dirt{}.Has)
	if _, ok := d.prStallKept(1, open); ok || len(d.prStalls) != 0 {
		t.Errorf("at pr-open, the item read back is kept: %+v", d.prStalls)
	}
	is := state.Derive(prItemSnap("verifying"), waitSlug, Dirt{}.Has)
	w, ok := d.prStallKept(1, is)
	if !ok || w.Kept == nil || w.Kept.Kind != waitStalled || w.Move != is.TransitionComment || d.prStalls[1] != w {
		t.Errorf("at verifying, kept %+v, %t, %+v; want the stalled task read back, kept as the wait", w, ok, d.prStalls)
	}
	if _, ok := d.prStallKept(2, is); ok {
		t.Errorf("with none read back, an item is kept")
	}
}
