package daemon

import (
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// The merger's login isn't the App's text (SPEC §5.1, §9): the hand-off
// for an untrusted merge and the close on a trusted one name it in code,
// with its mentions defused and its marker openers escaped, and a merge by
// nobody GitHub names says so.
func TestRenderMergedMerger(t *testing.T) {
	t.Parallel()
	handoff := renderUntrustedMergeHandoff(1, 5, "https://github.com/o/r/pull/5", "eve@x<!--", []string{"owner"})
	intro := "PR #5 for this task was merged into the story branch by `eve@​x&lt;!--`, who isn't trusted, and only the App's or a trusted user's merge closes the task, so it waits on you.\n\n"
	if !strings.HasPrefix(handoff, intro) || strings.Contains(handoff, "<!--") || !strings.HasSuffix(handoff, "cc @owner") {
		t.Errorf("the hand-off =\n%s\nwant it to start\n%s\nhold no marker opener, and end in the cc line", handoff, intro)
	}
	nobody := renderUntrustedMergeHandoff(1, 5, "https://github.com/o/r/pull/5", "", nil)
	if !strings.HasPrefix(nobody, "PR #5 for this task was merged into the story branch by someone GitHub doesn't name, ") {
		t.Errorf("the hand-off for nobody named =\n%s\nwant it to say GitHub names nobody", nobody)
	}
	if got, want := renderMergedClose(5, "abc", "eve@x"), "`eve@​x` merged PR #5 into the story branch as `abc`, so this task is complete."; got != want {
		t.Errorf("renderMergedClose = %q; want %q", got, want)
	}
	if got, want := renderMergedClose(5, "abc", ""), "The App merged PR #5 into the story branch as `abc`, so this task is complete."; got != want {
		t.Errorf("renderMergedClose by the App = %q; want %q", got, want)
	}
}

// The hand-off for an untrusted merge is keyed event=g<PR> (mergedKey), so
// it's no hand-off for a PR's close or untrusted reopen (prClosedMove),
// whose keys are c<PR>-<seconds> and a reopened event's ID, though it may
// move from pr-open too.
func TestMergedHandoffMove(t *testing.T) {
	t.Parallel()
	move := func(from, key string) github.Marker {
		return github.Marker{Kind: "transition", Fields: map[string]string{"from": from, "to": "needs-human", "event": key}}
	}
	for _, tt := range []struct {
		m              github.Marker
		merged, closed bool
	}{
		{move("pr-open", mergedKey(5).ID), true, false},
		{move("needs-human", "g5"), true, false},
		{move("in-progress", "g5"), true, false},
		{move("pr-open", "c5-1700000000"), false, true},
		{move("pr-open", "18734412"), false, true},
		{github.Marker{Kind: "transition", Fields: map[string]string{"from": "pr-open", "to": "closed", "event": "g5"}}, false, false},
	} {
		if got := mergedHandoffMove(tt.m); got != tt.merged {
			t.Errorf("mergedHandoffMove(%v) = %t; want %t", tt.m.Fields, got, tt.merged)
		}
		if got := prClosedMove(tt.m); got != tt.closed {
			t.Errorf("prClosedMove(%v) = %t; want %t", tt.m.Fields, got, tt.closed)
		}
	}
}

// Step 2 keeps a task at pr-open or needs-human, and one at in-progress
// or verifying with a parent in this repo and a PR, for its PR's merge
// (prDirtSet), and none at another stage, nor one with no parent, or a
// parent in another repo, or no PR. A task it keeps at in-progress whose
// PR isn't merged moves nothing, and its move out of pr-open, left
// waiting for the worker, waits no more (prPass), with no request.
func TestPRDirtSetMerge(t *testing.T) {
	t.Parallel()
	snap := func(stage string, parent int, repo string, more ...string) *github.Snapshot {
		s := prItemSnap(stage, more...)
		s.Parent, s.ParentRepo = parent, repo
		return s
	}
	opened := prItemMoved("pr-open", " pr=2")
	back := "<!-- issuedog:transition from=pr-open to=in-progress event=r5 -->"
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		want bool
	}{
		{"at pr-open", snap("pr-open", 0, "", opened), true},
		{"at in-progress in a story with a PR", snap("in-progress", 9, "", opened, back), true},
		{"at verifying in a story with a PR", snap("verifying", 9, "", opened, "<!-- issuedog:transition from=pr-open to=verifying event=nabc head=abc -->"), true},
		{"at in-progress with no parent", snap("in-progress", 0, "", opened, back), false},
		{"at in-progress with a parent in another repo", snap("in-progress", 9, "o/other", opened, back), false},
		{"at in-progress with no PR", snap("in-progress", 9, ""), false},
		{"at ready", snap("ready", 9, "", opened, "<!-- issuedog:transition from=pr-open to=ready event=x -->"), false},
	} {
		d := prHeadDaemon(tt.s)
		d.Step.AppSlug = waitSlug
		d.prDirtSet(tt.s, Dirt{})
		if _, kept := d.prDirt[1]; kept != tt.want {
			t.Errorf("%s: kept %t; want %t", tt.name, kept, tt.want)
		}
	}

	s := snap("in-progress", 9, "", opened, back)
	d := prHeadDaemon(s)
	d.Step.AppSlug = waitSlug
	d.prs = map[int]listedPR{2: {Number: 2, Open: true}}
	d.prDirtSet(s, Dirt{})
	d.prWait(1, Move{To: state.Verifying, Trigger: state.TriggerNewHead, Key: newHeadKey("bbb")}, "#3 holds it")
	moved, err := d.prPass(t.Context())
	if err != nil || len(moved) != 0 {
		t.Fatalf("prPass = %v, %v; want nothing moved, and no error", moved, err)
	}
	if _, waits := d.prWaiting[1]; waits {
		t.Error("the task's move out of pr-open still waits; want it dropped, as the task is at in-progress")
	}
}

// A task's verified tip is the tip= of its latest move into pr-open that
// names a head, so a person's head that failed verify, with no tip=, names
// none, and a move into pr-open with no head, as a stage set by hand,
// looks through to the one before.
func TestPRMoveTip(t *testing.T) {
	t.Parallel()
	to := func(fields string) github.Marker {
		m := github.Marker{Kind: "transition", Fields: map[string]string{"to": "pr-open"}}
		for f := range strings.FieldsSeq(fields) {
			k, v, _ := strings.Cut(f, "=")
			m.Fields[k] = v
		}
		return m
	}
	for _, tt := range []struct {
		name    string
		markers []github.Marker
		want    string
	}{
		{"none", nil, ""},
		{"the PR step's", []github.Marker{to("head=h1 tip=t1")}, "t1"},
		{"a person's failed head after", []github.Marker{to("head=h1 tip=t1"), to("head=h2")}, ""},
		{"a stage set by hand after", []github.Marker{to("head=h1 tip=t1"), to("by=owner")}, "t1"},
	} {
		if got := prMoveTip(state.Issue{Markers: tt.markers}); got != tt.want {
			t.Errorf("%s: prMoveTip = %q; want %q", tt.name, got, tt.want)
		}
	}
}
