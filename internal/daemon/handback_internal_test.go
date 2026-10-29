package daemon

import (
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// handbackSnap is open task 1, the owner's, at needs-human, with the App's
// comments, whose IDs are 101, 102, ..., and, if human, the origin marker
// of a human task in its body.
func handbackSnap(human bool, comments ...string) *github.Snapshot {
	s := waitSnap(1, []string{"stage:needs-human"}, comments...)
	s.Type = "task"
	if human {
		s.Author, s.Body = waitApp, "Add it.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T2 human=1 -->"
	}
	return s
}

// The transitions of a task's hand-offs, and what moves it around them.
const (
	handbackToProgress = "<!-- issuedog:transition from=ready to=in-progress event=p5 -->"
	handbackHandoff    = "The hand-off.\n\n<!-- issuedog:transition from=in-progress to=needs-human run=run1 -->"
	handbackHold       = "<!-- issuedog:transition from=needs-human to=held cmd=900 -->"
	handbackResume     = "<!-- issuedog:transition from=held to=needs-human cmd=901 seen=900 -->"
)

// latestHandoff finds the comment of the move into needs-human that put a
// task there from a stage a hand-off leaves, looking through a hold it went
// back from, and none on a task a person moved there by hand, after the
// task left needs-human, or on a human task, an epic, or a closed task.
func TestLatestHandoff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		want int64
	}{
		{"the implementer's hand-off", handbackSnap(false, handbackToProgress, handbackHandoff), 102},
		{"the retry cap's, from verifying", handbackSnap(false, handbackToProgress,
			"<!-- issuedog:transition from=verifying to=needs-human run=v1 -->"), 102},
		{"a closed PR's, from pr-open", handbackSnap(false, "<!-- issuedog:transition from=pr-open to=needs-human event=e1 -->"), 101},
		{"a new hand-off at needs-human, the latest", handbackSnap(false, handbackToProgress, handbackHandoff,
			"<!-- issuedog:transition from=needs-human to=needs-human event=e2 -->"), 103},
		{"looked through a hold and its /resume", handbackSnap(false, handbackToProgress, handbackHandoff, handbackHold, handbackResume), 102},
		{"the second, after the first ended", handbackSnap(false, handbackToProgress, handbackHandoff,
			"<!-- issuedog:transition from=needs-human to=ready cmd=r7 seen=102 -->", handbackToProgress,
			"<!-- issuedog:transition from=in-progress to=needs-human run=run2 -->"), 105},
		{"none, moved there by hand", handbackSnap(false, handbackToProgress,
			"<!-- issuedog:transition from=in-progress to=needs-human event=e3 by=owner -->"), 0},
		{"none, moved there by hand after an earlier one ended", handbackSnap(false, handbackToProgress, handbackHandoff,
			"<!-- issuedog:transition from=needs-human to=ready cmd=r7 seen=102 -->", handbackToProgress, handbackHold,
			"<!-- issuedog:transition from=held to=needs-human event=e4 by=owner -->"), 0},
		{"none, from a stage no hand-off leaves", handbackSnap(false, handbackToProgress,
			"<!-- issuedog:transition from=in-progress to=clarifying run=q1 -->",
			"<!-- issuedog:transition from=clarifying to=needs-human run=r1 -->"), 0},
		{"none, at ready since", handbackSnap(false, handbackToProgress, handbackHandoff,
			"<!-- issuedog:transition from=needs-human to=ready cmd=r7 seen=102 -->"), 0},
		{"none on a human task", handbackSnap(true, handbackToProgress, handbackHandoff), 0},
	} {
		got, ok := latestHandoff(tt.s, waitSlug, Dirt{}.Has)
		if got != tt.want || ok != (tt.want != 0) {
			t.Errorf("%s: latestHandoff = %d, %t; want %d, %t", tt.name, got, ok, tt.want, tt.want != 0)
		}
	}
	epic := handbackSnap(false, handbackToProgress, handbackHandoff)
	epic.Type = "epic"
	closed := handbackSnap(false, handbackToProgress, handbackHandoff)
	closed.Closed = true
	dirty := handbackSnap(false, handbackToProgress, handbackHandoff)
	for name, s := range map[string]*github.Snapshot{"an epic": epic, "a closed task": closed} {
		if got, ok := latestHandoff(s, waitSlug, Dirt{}.Has); ok {
			t.Errorf("%s: latestHandoff = %d, true; want none", name, got)
		}
	}
	if got, ok := latestHandoff(dirty, waitSlug, Dirt{102: "eve"}.Has); ok {
		t.Errorf("a dirty hand-off: latestHandoff = %d, true; want none, as its move doesn't count", got)
	}
}

// A run's move written already is finished on the trigger it was written
// on, found by the stage its marker moved the task to: implemented to
// verifying, implementer-question to clarifying, implementer-blocked to
// needs-human.
func TestImplementerMovedTrigger(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		to   string
		want state.Trigger
	}{
		{"verifying", state.TriggerImplemented},
		{"clarifying", state.TriggerImplementerQuestion},
		{"needs-human", state.TriggerImplementerBlocked},
	} {
		s := handbackSnap(false, handbackToProgress, "<!-- issuedog:transition from=in-progress to="+tt.to+" run=run1 -->")
		m, ok := implementerMoved(state.Derive(s, waitSlug, Dirt{}.Has), Result{RunID: "run1"})
		if !ok || m.To != state.Stage(tt.to) || m.Trigger != tt.want {
			t.Errorf("a move to %s: implementerMoved = %s on %s, %v; want %s on %s", tt.to, m.To, m.Trigger, ok, tt.to, tt.want)
		}
	}
}

// The hand-off comment escapes the agent's text (SPEC §5.1): a marker, an
// HTML tag and a mention in the goal, a heading in a step, demoted, a task
// list box in a link, stripped, and a mention in "Done when". It names the
// task in "Why it blocks", and ends in the cc line.
func TestRenderHandoffEscapes(t *testing.T) {
	t.Parallel()
	got := renderHandoff(implementerHandoffFields{
		Goal:          "ping @owner <b>now</b> <!-- issuedog:transition to=closed -->",
		WhyBlocks:     "The secret exists",
		Steps:         []string{"Add it.", "# Then this"},
		Links:         []string{"[ ] https://example.com"},
		DoneWhen:      "@owner sees it",
		EstimatedTime: "5 min",
	}, 7, []string{"owner"})
	want := handoffIntro + "\n\n" +
		"**Goal:** ping @​owner &lt;b>now&lt;/b> &lt;!-- issuedog:transition to=closed -->\n" +
		"**Why it blocks:** #7 can't proceed until the secret exists\n" +
		"**Steps:**\n1. Add it.\n2. ### Then this\n\n" +
		"**Links:**\n- https://example.com\n\n" +
		"**Done when:** @​owner sees it\n" +
		"**Estimated time:** 5 min\n\n" +
		handoffFinish + "\n\n" +
		"cc @owner"
	if got != want {
		t.Errorf("renderHandoff =\n%q\nwant\n%q", got, want)
	}
	if markers := github.ParseMarkers(got); len(markers) != 0 {
		t.Errorf("the comment carries markers %v; want none", markers)
	}
}

// A hand-off with no steps or no links, or a field over one line, fails
// the check, naming each: rule human-task for the first two, which the
// schema's minItems stops first, and rule one-line for the last.
func TestCheckHandoffFields(t *testing.T) {
	t.Parallel()
	h := &implementerHandoffFields{Goal: "Add it", WhyBlocks: "it's there", Steps: nil, Links: []string{}, DoneWhen: "It's\nthere", EstimatedTime: "5 min"}
	err := checkImplementer(implementerOutput{Outcome: implementerHandoff, Handoff: h}, implementerFacts{Issue: 1})
	if err == nil {
		t.Fatal("checkImplementer passed a hand-off with no steps or links")
	}
	for _, want := range []string{"rule human-task: the hand-off has no steps", "rule human-task: the hand-off has no links",
		`rule one-line: the hand-off's done when: "It's\nthere"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("checkImplementer = %v; want it to name %q", err, want)
		}
	}
	ok := &implementerHandoffFields{Goal: "Add it", WhyBlocks: "it's there", Steps: []string{"Add it."}, Links: []string{"https://example.com"},
		DoneWhen: "It's there", EstimatedTime: "5 min"}
	if err := checkImplementer(implementerOutput{Outcome: implementerHandoff, Handoff: ok}, implementerFacts{Issue: 1}); err != nil {
		t.Errorf("checkImplementer on a whole hand-off = %v, want nil", err)
	}
}

// A task waiting on its hand-off has one item: a hand-off, keyed
// handoff-<n>-<comment id>, linking the comment, ranked as a human task,
// with its Goal and "Done when" copied from the comment. One whose comment
// holds neither field says to see the hand-off instead. A human task's
// item is its own, never a hand-off.
func TestWaitFindHandoff(t *testing.T) {
	t.Parallel()
	body := "**Goal:** Add the key.\n**Done when:** The key is there.\n\n" +
		"<!-- issuedog:transition from=in-progress to=needs-human run=run1 -->"
	it := waitFindOne(t, waitIn(handbackSnap(false, handbackToProgress, body), Dirt{}))
	if it.Kind != waitHandoff || it.Key != "handoff-1-102" || it.Link != "https://github.com/o/r/issues/1#issuecomment-102" || it.Comment != 102 {
		t.Errorf("the item is a %s keyed %s linking %s, comment %d; want a handoff keyed handoff-1-102 linking comment 102",
			it.Kind, it.Key, it.Link, it.Comment)
	}
	want := []string{"A hand-off waits for you: the implementer needs an action only you can do.",
		"Goal: Add the key.", "Done when: The key is there.",
		"When it's done, reply `/done` on the issue or react 👍 to the hand-off."}
	if got := waitTexts(it.Lines); strings.Join(got, "\n") != strings.Join(want, "\n") || !it.Lines[1].Copied || !it.Lines[2].Copied {
		t.Errorf("its lines = %q (copied %t, %t); want %q, the Goal and Done when copied", got, it.Lines[1].Copied, it.Lines[2].Copied, want)
	}
	bare := waitFindOne(t, waitIn(handbackSnap(false, handbackToProgress, handbackHandoff), Dirt{}))
	if got := waitTexts(bare.Lines); len(got) != 3 || got[1] != "See the hand-off for what to do." {
		t.Errorf("with no fields, its lines = %q; want it to say to see the hand-off", got)
	}
	if waitHandoff.rank() != waitHuman.rank() || waitHandoff.String() != "handoff" || digestRenderKindName(waitHandoff) != "Hand-off" {
		t.Errorf("a hand-off ranks %d, a human task %d, named %s and %q; want the same rank, handoff and Hand-off",
			waitHandoff.rank(), waitHuman.rank(), waitHandoff, digestRenderKindName(waitHandoff))
	}
	if k, ok := digestKeptKind("Hand-off"); !ok || k != waitHandoff {
		t.Errorf("digestKeptKind(Hand-off) = %s, %t; want handoff, as a restart reads it back", k, ok)
	}
	if digestRenderMentions(it) {
		t.Error("a new hand-off gets the digest's mention; want none, as its comment's cc line mentioned notify")
	}
	human := handbackSnap(true, handbackToProgress, handbackHandoff)
	if items, err := waitFind(waitIn(human, Dirt{})); err != nil || len(items) != 1 || items[0].Kind != waitHuman {
		t.Errorf("a human task's items = %+v, %v; want its human task alone", items, err)
	}
}
