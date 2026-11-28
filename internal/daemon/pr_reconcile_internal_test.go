package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

// prReconcileLink is PR #2's link, as the hand-off gives it.
const prReconcileLink = "https://github.com/o/r/pull/2"

// The hand-off for a task's PR closed without merging is in the SPEC §9.3
// form: it names the PR and what happened to it, then the daemon's own
// fields, with the PR as the link, then the three ways on, and the cc
// line. For an untrusted reopen, it names the reopener in code, defused,
// or says GitHub names nobody, and asks the owner to close and reopen the
// PR to keep it (SPEC §7.2).
func TestRenderPRClosedHandoff(t *testing.T) {
	t.Parallel()
	rest := func(why, step1, done, keep string) string {
		return "**Goal:** Decide what becomes of this task, whose PR " + why + ".\n" +
			"**Why it blocks:** #1 can't proceed until its PR is " + done + ", or the task is started over or closed\n" +
			"**Steps:**\n" +
			"1. To keep the PR, " + step1 + ": the task goes back to `pr-open`, waiting for your review and merge.\n" +
			"2. To start over, say what to change in a comment, then reply `/done`: a new round from the task's head, whose pass reopens this PR.\n" +
			"3. If the task isn't needed, close this issue.\n\n" +
			"**Links:**\n- " + prReconcileLink + "\n\n" +
			"**Done when:** The PR is " + done + ", a new round is asked for, or the issue is closed.\n" +
			"**Estimated time:** A few minutes.\n\n" +
			keep + " Replying `/done` on this issue or reacting 👍 to this comment sends it back to `ready` for a new round. Closing this issue ends the task."
	}
	closed := rest("was closed without merging", "reopen it", "reopened", "Reopening the PR sends the task back to `pr-open`.")
	reopened := rest("someone who isn't trusted reopened", "close it and reopen it yourself", "closed and reopened by you",
		"Closing and reopening the PR yourself sends the task back to `pr-open`.")
	for _, tt := range []struct {
		name     string
		reopened bool
		by       string
		notify   []string
		want     string
	}{
		{"closed", false, "", []string{"owner"},
			"PR #2 for this task was closed without merging, so the task waits on you.\n\n" + closed + "\n\ncc @owner"},
		{"reopened by someone untrusted", true, "mal@lory", []string{"owner", "bob"},
			"PR #2 for this task was reopened after a close by `mal@\u200blory`, who isn't trusted, and only a trusted user's reopen keeps it, so the task waits on you.\n\n" +
				reopened + "\n\ncc @owner @bob"},
		{"reopened by nobody GitHub names", true, "", nil,
			"PR #2 for this task was reopened after a close by someone GitHub doesn't name, and only a trusted user's reopen keeps it, so the task waits on you.\n\n" +
				reopened},
	} {
		if got := renderPRClosedHandoff(1, 2, prReconcileLink, tt.reopened, tt.by, false, tt.notify); got != tt.want {
			t.Errorf("%s: the hand-off =\n%s\nwant\n%s", tt.name, got, tt.want)
		}
	}
	if got, want := renderPRReopened(2, "mal@lory", false), "`mal@\u200blory` reopened PR #2, so this task is back at `pr-open`. It waits for your review and merge, and merging it closes this task."; got != want {
		t.Errorf("the reopen's move =\n%s\nwant\n%s", got, want)
	}
}

// The hand-off for a task's PR is pr-open → needs-human keyed event=, a
// close's key starting c, an untrusted reopen's the event's ID; no other
// move into needs-human is one (SPEC §5, §7.2).
func TestPRClosedMove(t *testing.T) {
	t.Parallel()
	for marker, want := range map[string][2]bool{ // prClosedMove, prClosedByReopen
		"<!-- issuedog:transition from=pr-open to=needs-human event=c2-1767225600 -->": {true, false},
		"<!-- issuedog:transition from=pr-open to=needs-human event=18734412 -->":      {true, true},
		"<!-- issuedog:transition from=pr-open to=needs-human by=owner -->":            {false, true},
		"<!-- issuedog:transition from=verifying to=needs-human event=c2-1 -->":        {false, false},
		"<!-- issuedog:transition from=pr-open to=held event=c2-1 -->":                 {false, false},
		"<!-- issuedog:revert from=pr-open to=needs-human event=c2-1 -->":              {false, false},
	} {
		m := github.ParseMarkers(marker)[0]
		if got := prClosedMove(m); got != want[0] {
			t.Errorf("prClosedMove(%s) = %t, want %t", marker, got, want[0])
		}
		if got := prClosedMove(m) && prClosedByReopen(m); got != (want[0] && want[1]) {
			t.Errorf("prClosedMove and prClosedByReopen(%s) = %t, want %t", marker, got, want[0] && want[1])
		}
	}
}

// A task's PR window begins at its latest move into pr-open from another
// stage (SPEC §6.4): a pr-open → pr-open move doesn't narrow it, nor does
// a return from a hold that began at pr-open, by /resume or the
// interviewer's unchanged after a discussion hold, but a return from a
// hold that began elsewhere, by a stage set by hand, begins it at that
// hold, and a stage set by hand from another stage, or a trusted reopen's
// move back from needs-human, begins it at that move.
func TestPRWindow(t *testing.T) {
	t.Parallel()
	const (
		toPROpen  = "<!-- issuedog:transition from=verifying to=pr-open run=rv head=abc pr=2 -->"
		mainMoved = "<!-- issuedog:transition from=pr-open to=pr-open event=m1 -->"
		hold      = "<!-- issuedog:transition from=pr-open to=held cmd=900 -->"
		resume    = "<!-- issuedog:transition from=held to=pr-open cmd=901 seen=900 -->"
		discHold  = "<!-- issuedog:transition from=pr-open to=held at=pr-open origin=pr-open reason=discussion event=c1 seen=102 -->"
		discEnd   = "<!-- issuedog:transition from=held to=clarifying at=pr-open origin=pr-open event=h103 seen=102 -->"
		unchanged = "<!-- issuedog:transition from=clarifying to=pr-open origin=pr-open run=iv1 seen=104 -->"
		backHand  = "<!-- issuedog:transition from=pr-open to=verifying by=owner -->"
		holdThere = "<!-- issuedog:transition from=verifying to=held cmd=905 -->"
		handSet   = "<!-- issuedog:transition from=held to=pr-open by=owner -->"
		fromHand  = "<!-- issuedog:transition from=verifying to=pr-open by=owner -->"
		handoff   = "<!-- issuedog:transition from=pr-open to=needs-human event=c2-1 -->"
		reopened  = "<!-- issuedog:transition from=needs-human to=pr-open event=18734412 pr=2 -->"
	)
	for _, tt := range []struct {
		name     string
		comments []string
		want     int // the comment the window begins at, by index, or -1 for none
	}{
		{"none", []string{"<!-- issuedog:transition from=ready to=in-progress event=s1 -->"}, -1},
		{"the PR step's move", []string{toPROpen}, 0},
		{"main moved", []string{toPROpen, mainMoved}, 0},
		{"a /resume", []string{toPROpen, hold, resume}, 0},
		{"the interviewer's unchanged", []string{toPROpen, discHold, discEnd, unchanged}, 0},
		{"a stage set by hand from a hold begun elsewhere", []string{toPROpen, backHand, holdThere, handSet}, 2},
		{"a stage set by hand from another stage", []string{toPROpen, backHand, fromHand}, 2},
		{"a trusted reopen's move back", []string{toPROpen, handoff, reopened}, 2},
		{"a hand-off", []string{toPROpen, handoff}, 0},
	} {
		s := waitSnap(1, []string{"stage:pr-open"})
		s.Type = "task"
		for i, c := range tt.comments {
			s.Comments = append(s.Comments, github.Comment{ID: int64(101 + i), Author: waitApp, Body: c, CreatedAt: waitT0.Add(time.Duration(i+1) * time.Minute)})
		}
		want := time.Time{}
		if tt.want >= 0 {
			want = s.Comments[tt.want].CreatedAt
		}
		if got := prWindow(s, waitSlug, Dirt{}.Has); !got.Equal(want) {
			t.Errorf("%s: the window begins at %s, want %s", tt.name, got, want)
		}
	}
}

// A task waiting on the hand-off for its PR's close has the hand-off's
// item, which says the PR was closed without merging, or reopened by
// someone untrusted, with its Goal and "Done when" copied from the
// comment, and ends as the hand-off does: reopen the PR, or close and
// reopen it, /done or a 👍 for a new round, or close the issue (SPEC §9.4).
func TestWaitFindPRClosedHandoff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		reopened bool
		key      string
		want     []string
	}{
		{"closed", false, "c2-1767225600", []string{"A hand-off waits for you: the task's PR was closed without merging.",
			"Goal: Decide what becomes of this task, whose PR was closed without merging.",
			"Done when: The PR is reopened, a new round is asked for, or the issue is closed.",
			"Reopen the PR to send the task back to `pr-open`, or reply `/done` on the issue or react 👍 to the hand-off for a new round, or close the issue."}},
		{"reopened", true, "18734412", []string{"A hand-off waits for you: the task's PR was reopened after a close by someone who isn't trusted.",
			"Goal: Decide what becomes of this task, whose PR someone who isn't trusted reopened.",
			"Done when: The PR is closed and reopened by you, a new round is asked for, or the issue is closed.",
			"Close and reopen the PR yourself to send the task back to `pr-open`, or reply `/done` on the issue or react 👍 to the hand-off for a new round, or close the issue."}},
	} {
		body := renderPRClosedHandoff(1, 2, prReconcileLink, tt.reopened, "mallory", false, nil) +
			"\n\n<!-- issuedog:transition from=pr-open to=needs-human event=" + tt.key + " -->"
		it := waitFindOne(t, waitIn(handbackSnap(false, "<!-- issuedog:transition from=verifying to=pr-open run=rv head=abc pr=2 -->", body), Dirt{}))
		if got := waitTexts(it.Lines); it.Kind != waitHandoff || it.Key != "handoff-1-102" || strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
			t.Errorf("%s: the item is a %s keyed %s, its lines %q; want a handoff keyed handoff-1-102, its lines %q", tt.name, it.Kind, it.Key, got, tt.want)
		}
	}
}
