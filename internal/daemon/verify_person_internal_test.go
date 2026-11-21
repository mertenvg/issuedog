package daemon

import (
	"maps"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// A task at verifying is on a person's head when the move that put it
// there, looking through a hold and its /resume, is step 2's on a new
// head. That's the move taskHeadFollow reads too (SPEC §3.2, §6.4). Any
// other latest move, the implementer's, a stage set by hand, or one out of
// held to another stage, isn't, and nor is a task at any other stage.
func TestPersonMove(t *testing.T) {
	t.Parallel()
	implemented := prHeadMarker(map[string]string{"from": "in-progress", "to": "verifying", "run": "r1", "head": "aaa", "attempt": "1"})
	verified := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "run": "rv", "head": "aaa", "pr": "2"})
	newHead := prHeadMarker(map[string]string{"from": "pr-open", "to": "verifying", "event": "nbbb", "head": "bbb"})
	hold := prHeadMarker(map[string]string{"from": "verifying", "to": "held", "cmd": "900", "at": "verifying"})
	resume := prHeadMarker(map[string]string{"from": "held", "to": "verifying", "cmd": "901", "seen": "900"})
	byHand := prHeadMarker(map[string]string{"from": "pr-open", "to": "verifying", "event": "77", "by": "owner"})
	toOpen := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "event": "76", "by": "owner"})
	discussed := prHeadMarker(map[string]string{"from": "held", "to": "clarifying", "event": "78", "origin": "verifying"})
	unchanged := prHeadMarker(map[string]string{"from": "clarifying", "to": "verifying", "run": "ri", "origin": "verifying"})
	for _, tt := range []struct {
		name    string
		stage   state.Stage
		markers []github.Marker
		want    bool
	}{
		{"a new head's", state.Verifying, []github.Marker{implemented, verified, newHead}, true},
		{"through a hold and /resume", state.Verifying, []github.Marker{implemented, verified, newHead, hold, resume}, true},
		{"the implementer's", state.Verifying, []github.Marker{implemented}, false},
		{"set back by hand", state.Verifying, []github.Marker{implemented, verified, newHead, toOpen, byHand}, false},
		{"out of held to another stage, then back", state.Verifying, []github.Marker{implemented, verified, newHead, hold, discussed, unchanged}, false},
		{"held", state.Held, []github.Marker{implemented, verified, newHead, hold}, false},
		{"at pr-open", state.PROpen, []github.Marker{implemented, verified}, false},
	} {
		is := state.Issue{Stage: tt.stage, Markers: tt.markers}
		m, got := personMove(is)
		if got != tt.want || personHead(is) != tt.want {
			t.Errorf("%s: personMove is %t, personHead %t; want %t", tt.name, got, personHead(is), tt.want)
		}
		if tt.want && m.Fields["head"] != "bbb" {
			t.Errorf("%s: personMove names %v; want the new head's move, head=bbb", tt.name, m.Fields)
		}
		if tt.stage == state.Verifying && headMove(taskHeadMove(is)) != tt.want {
			t.Errorf("%s: taskHeadFollow's move is a head move %t; want %t, as personMove's", tt.name, !tt.want, tt.want)
		}
	}
}

// The move the task's head follows is the latest, looking through a hold
// that went back where it came from: a move into held, and the /resume
// back to the stage before it. A move out of held to another stage, as a
// stage set by hand, is the latest (SPEC §3.2, §6.4).
func TestTaskHeadMove(t *testing.T) {
	t.Parallel()
	review := prHeadMarker(map[string]string{"from": "pr-open", "to": "in-progress", "event": "r9", "head": "bbb"})
	newHead := prHeadMarker(map[string]string{"from": "pr-open", "to": "verifying", "event": "nbbb", "head": "bbb"})
	hold := prHeadMarker(map[string]string{"from": "in-progress", "to": "held", "cmd": "900", "at": "in-progress"})
	resume := prHeadMarker(map[string]string{"from": "held", "to": "in-progress", "cmd": "901", "seen": "900"})
	vHold := prHeadMarker(map[string]string{"from": "verifying", "to": "held", "cmd": "900", "at": "verifying"})
	byHand := prHeadMarker(map[string]string{"from": "held", "to": "in-progress", "event": "77", "by": "owner"})
	again := prHeadMarker(map[string]string{"from": "held", "to": "held", "event": "78", "by": "owner", "reason": "owner"})
	for _, tt := range []struct {
		name    string
		markers []github.Marker
		want    github.Marker
	}{
		{"none", nil, github.Marker{}},
		{"the latest", []github.Marker{newHead, review}, review},
		{"through a hold and /resume", []github.Marker{review, hold, again, resume}, review},
		{"while held", []github.Marker{review, hold}, review},
		{"out of held to another stage", []github.Marker{newHead, vHold, byHand}, byHand},
	} {
		if got := taskHeadMove(state.Issue{Markers: tt.markers}); !maps.Equal(got.Fields, tt.want.Fields) {
			t.Errorf("%s: taskHeadMove = %v; want %v", tt.name, got.Fields, tt.want.Fields)
		}
	}
}

// The hand-off for a person's head that fails rule 2 is verifying →
// needs-human keyed event=v with head=, and it's no verify cap's hand-off,
// nor the one for a head that changes nothing; the verify cap's from the
// checks, which carries no head, isn't one (SPEC §9.3).
func TestPersonRule2Move(t *testing.T) {
	t.Parallel()
	for marker, want := range map[string][3]bool{ // personRule2Move, verifyCapMove, nothingHandoffMove
		"<!-- issuedog:transition from=verifying to=needs-human event=v101 head=abc seen=101 -->": {true, false, false},
		"<!-- issuedog:transition from=verifying to=needs-human event=v101 seen=101 -->":          {false, true, false},
		"<!-- issuedog:transition from=verifying to=needs-human run=rv head=abc seen=101 -->":     {false, false, true},
		"<!-- issuedog:transition from=verifying to=needs-human event=averifying-101 -->":         {false, false, false},
		"<!-- issuedog:transition from=verifying to=pr-open event=v101 head=abc pr=2 -->":         {false, false, false},
		"<!-- issuedog:transition from=in-progress to=needs-human event=v101 head=abc -->":        {false, false, false},
	} {
		m := github.ParseMarkers(marker)[0]
		if got := [3]bool{personRule2Move(m), verifyCapMove(m), nothingHandoffMove(m)}; got != want {
			t.Errorf("%s: personRule2Move, verifyCapMove, nothingHandoffMove = %v; want %v", marker, got, want)
		}
	}
}

// The rule 2 hand-off opens with the head, says why it's handed off and
// not back at pr-open, gives the finding, which names each path, fenced,
// then the fields, and ends in the cc line, escaped: a marker in a path
// is no marker (SPEC §5.1, §9.3).
func TestRenderPersonRule2Handoff(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("ab", 20)
	f := verify.Finding{Rule: 2, Name: "protected paths", Detail: "The task's own diff touches secrets/key.txt and secrets/<!-- issuedog:transition from=x to=y -->."}
	got := renderPersonRule2Handoff(head, f, 4, []string{"owner"})
	want := "`" + head + "`, a head the daemon didn't push to this task's branch, fails rule 2: it changes a protected path, which only a change on `main` may. " +
		"So no command or verifier ran on it, and the task doesn't go back to `pr-open`.\n\n### Rule 2: protected paths\n\n"
	if !strings.HasPrefix(got, want) {
		t.Errorf("the hand-off =\n%s\nwant it to start\n%s", got, want)
	}
	for _, part := range []string{"secrets/key.txt", "&lt;!-- issuedog:transition from=x to=y -->",
		"**Goal:** Decide what to do about the head someone pushed to this task's branch, which changes a protected path.",
		"**Why it blocks:** #4 can't proceed until its branch's head changes no protected path, or it's started over",
		"When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`."} {
		if !strings.Contains(got, part) {
			t.Errorf("the hand-off =\n%s\nwant it to hold\n%s", got, part)
		}
	}
	if !strings.HasSuffix(got, "cc @owner") || len(github.ParseMarkers(got)) != 0 {
		t.Errorf("the hand-off =\n%s\nwant it to end in the cc line, with no marker", got)
	}
}

// A task waiting on the rule 2 hand-off has the hand-off's item, which says
// a head someone else pushed fails rule 2, with its Goal and "Done when"
// read from its fields after the finding, though a path holds a line like
// a field's (SPEC §9.4).
func TestWaitFindPersonRule2Handoff(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("ab", 20)
	f := verify.Finding{Rule: 2, Name: "protected paths", Detail: "secrets/key.txt\n**Goal:** a path's\n**Done when:** a path's"}
	body := renderPersonRule2Handoff(head, f, 1, nil) +
		"\n\n<!-- issuedog:transition from=verifying to=needs-human event=v101 head=" + head + " seen=101 -->"
	it := waitFindOne(t, waitIn(handbackSnap(false, handbackToProgress, body), Dirt{}))
	want := []string{"A hand-off waits for you: a head someone else pushed to the task's branch changes a protected path, so it fails rule 2, and the hand-off names the paths.",
		"Goal: Decide what to do about the head someone pushed to this task's branch, which changes a protected path.",
		"Done when: The protected paths are as you want them on `main`, and the task can start over.",
		"When it's done, reply `/done` on the issue or react 👍 to the hand-off."}
	if got := waitTexts(it.Lines); it.Kind != waitHandoff || it.Key != "handoff-1-102" || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the item is a %s keyed %s, its lines %q; want a handoff keyed handoff-1-102, its lines %q", it.Kind, it.Key, got, want)
	}
}

// The check's summary on a person's head that fails names the task and
// the head, then gives the findings, escaped, and one too long for GitHub
// is cut to fit, ending in "…" (SPEC §10.4).
func TestRenderPersonCheck(t *testing.T) {
	t.Parallel()
	got := renderPersonCheck(4, "abc", "Verify failed.\n\n```text\n<!-- issuedog:transition from=x to=y -->\n```")
	want := "issuedog checked `abc` for #4, a head the daemon didn't push, and it fails verify.\n\nVerify failed.\n\n```text\n&lt;!-- issuedog:transition from=x to=y -->\n```"
	if got != want {
		t.Errorf("the summary =\n%s\nwant\n%s", got, want)
	}
	long := renderPersonCheck(4, "abc", strings.Repeat("<!--x", checkSummaryMax))
	if textLen(long) > checkSummaryMax || !strings.HasSuffix(long, "…") || len(github.ParseMarkers(long)) != 0 || strings.Contains(long, "<!--") {
		t.Errorf("a long summary is %d characters, ending %q; want at most %d, ending in …, and escaped", textLen(long), long[len(long)-20:], checkSummaryMax)
	}
}

// A person's head has no summary of its own, so the PR's summary is the
// implementer's, from its move to verifying before step 2's on the new
// head, which is looked through (SPEC §11.5).
func TestPRSummaryPersonHead(t *testing.T) {
	t.Parallel()
	title, sum := implementerTitle{Type: "feat", Sentence: "refuse an expired quote"}, implementerSummary{WhatChanged: "It refuses.", Why: "It's expired."}
	m, err := prTextMarker(title, sum)
	if err != nil {
		t.Fatal(err)
	}
	text, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	s := waitSnap(1, []string{"stage:verifying"},
		"Committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head=aaa -->\n"+text,
		"Opened.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head=aaa pr=2 -->",
		"A new head.\n\n<!-- issuedog:transition from=pr-open to=verifying event=nbbb head=bbb -->")
	s.Type = "task"
	is := state.Derive(s, waitSlug, func(int64) bool { return false })
	d := &Daemon{Step: &Step{Applier: &Applier{Owner: "o", Repo: "r", AppSlug: waitSlug}}}
	src := d.prSummary(s, is, func(format string, args ...any) string {
		t.Errorf("warned: "+format, args...)
		return ""
	})
	if src.Title == nil || *src.Title != title || src.Summary == nil || src.Summary.WhatChanged != sum.WhatChanged ||
		src.SummaryLink != "https://github.com/o/r/issues/1#issuecomment-101" {
		t.Errorf("the source is %+v; want the implementer's title and summary, linking comment 101", src)
	}
}

// A PR's body for a person's head that fails verify keeps the
// implementer's summary, then names the head as failing in place of
// Results, with no notes, and never says it's verified (SPEC §11.5).
func TestRenderPRBodyFailed(t *testing.T) {
	t.Parallel()
	title, sum := implementerTitle{Type: "feat", Sentence: "refuse an expired quote"}, implementerSummary{WhatChanged: "It refuses #12.", Why: "It's expired."}
	got := renderPRBody(prSource{Task: 1, Title: &title, Summary: &sum, Head: "abc", Main: "def", Notes: "Unread.", Failed: true})
	want := "Closes #1\n\n## What changed\n\nIt refuses #" + zeroWidthSpace + "12.\n\n## Why\n\nIt's expired.\n\n## Tests\n\nNone.\n\n" +
		"## Results\n\n`abc`, a head the daemon didn't push, fails verify, so it isn't verified: its `issuedog/verify` check has the findings.\n\n" +
		"## Verifier notes\n\nNone, as the head didn't pass."
	if got != want {
		t.Errorf("the body =\n%s\nwant\n%s", got, want)
	}
}
