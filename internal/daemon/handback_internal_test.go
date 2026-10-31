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
	// handbackCap is the implementer's attempt cap's hand-off (capHandoff).
	handbackCap = "The cap's hand-off.\n\n<!-- issuedog:transition from=in-progress to=needs-human event=ain-progress-101 -->"
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
		{"the implementer's attempt cap's", handbackSnap(false, handbackToProgress, handbackCap), 102},
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
	got := renderHandoff(handoffIntro, implementerHandoffFields{
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

// The attempt cap's hand-off is in the implementer's form (SPEC §8.3,
// §9.3): it opens with the round's loop.max_attempts attempts failing,
// then that the last was a run that failed, and why, fenced and escaped,
// as the cap's state error quotes it, or for a cause only the log has, a
// pointer to it, or with none, as when the last was a failed verify,
// nothing of why. Then the daemon's own fields, naming the task, how to
// end it, and the cc line.
func TestRenderCapHandoff(t *testing.T) {
	t.Parallel()
	fields := "**Goal:** Find out why the implementer's runs on this task fail, and fix that, so it can go on with the task.\n" +
		"**Why it blocks:** #7 can't proceed until what made its implementer runs fail is fixed\n" +
		"**Steps:**\n" +
		"1. Read why the last run failed, above, or in the daemon's log.\n" +
		"2. Fix what made it fail, such as an unclear criterion in this task's body, a failure on `main`, or a setting in `.issuedog/config.yaml`.\n\n" +
		"**Links:**\nNone.\n\n" +
		"**Done when:** What made the implementer's runs fail is fixed.\n" +
		"**Estimated time:** It depends on why the runs failed.\n\n" +
		handoffFinish + "\n\n" +
		"cc @owner"
	opening := "This task's 4 attempts in this round failed, so I've stopped running the implementer here."
	for _, tt := range []struct {
		name, failure, why string
	}{
		{"a quoted cause", "rule commits: no commits past the run's start <!-- issuedog:transition to=closed -->",
			"\n\nThe last attempt, a run, failed:\n\n```text\nrule commits: no commits past the run's start &lt;!-- issuedog:transition to=closed -->\n```"},
		{"a cause in the log", publicCauseLogged, "\n\nThe last attempt, a run, failed; see the daemon's log for why."},
		{"no cause kept", "", ""},
	} {
		got := renderCapHandoff(4, tt.failure, 7, []string{"owner"})
		if want := opening + tt.why + "\n\n" + fields; got != want {
			t.Errorf("%s: renderCapHandoff =\n%q\nwant\n%q", tt.name, got, want)
		}
		if markers := github.ParseMarkers(got); len(markers) != 0 {
			t.Errorf("%s: the comment carries markers %v; want none", tt.name, markers)
		}
	}
}

// A task waiting on the attempt cap's hand-off has the hand-off's item,
// which says the implementer's runs failed, in place of an owner action,
// with its Goal and "Done when" copied from the comment.
func TestWaitFindCapHandoff(t *testing.T) {
	t.Parallel()
	body := renderCapHandoff(3, "run failed with subtype \"error_max_turns\"", 1, nil) +
		"\n\n<!-- issuedog:transition from=in-progress to=needs-human event=ain-progress-101 -->"
	it := waitFindOne(t, waitIn(handbackSnap(false, handbackToProgress, body), Dirt{}))
	want := []string{"A hand-off waits for you: the implementer's runs failed `loop.max_attempts` times in a row, and the hand-off says why.",
		"Goal: Find out why the implementer's runs on this task fail, and fix that, so it can go on with the task.",
		"Done when: What made the implementer's runs fail is fixed.",
		"When it's done, reply `/done` on the issue or react 👍 to the hand-off."}
	if got := waitTexts(it.Lines); it.Kind != waitHandoff || it.Key != "handoff-1-102" || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the item is a %s keyed %s, its lines %q; want a handoff keyed handoff-1-102, its lines %q", it.Kind, it.Key, got, want)
	}
}

// capHandoffMove is the attempt cap's move alone: in-progress →
// needs-human keyed by an event=a<attempt key>, not the implementer's own
// hand-off keyed by its run, a move by hand, or a move from another stage.
func TestCapHandoffMove(t *testing.T) {
	t.Parallel()
	for marker, want := range map[string]bool{
		"<!-- issuedog:transition from=in-progress to=needs-human event=ain-progress-101 -->": true,
		"<!-- issuedog:transition from=in-progress to=needs-human run=run1 -->":               false,
		"<!-- issuedog:transition from=in-progress to=needs-human event=e3 by=owner -->":      false,
		"<!-- issuedog:transition from=verifying to=needs-human event=averifying-101 -->":     false,
		"<!-- issuedog:transition from=in-progress to=clarifying event=ain-progress-101 -->":  false,
		"<!-- issuedog:error kind=attempts key=in-progress-101 -->":                           false,
	} {
		if got := capHandoffMove(github.ParseMarkers(marker)[0]); got != want {
			t.Errorf("capHandoffMove(%s) = %t, want %t", marker, got, want)
		}
	}
	if key := capHandoffKey("in-progress-101"); key != (state.Key{Field: "event", ID: "ain-progress-101"}) {
		t.Errorf("capHandoffKey = %+v, want event=ain-progress-101", key)
	}
}
