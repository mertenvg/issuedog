package daemon

import (
	"strings"
	"testing"
)

// The digest's item for the attempt cap's hand-off says the last attempt
// was a failed run, and that the hand-off says why, only when the
// hand-off does (SPEC §9.4): with a quoted cause, or a pointer to the
// daemon's log. One that says nothing of why, as after findings below a
// loop.max_attempts lowered since, or from a lease written before the
// cause was kept, gets a line saying only that the round's attempts
// failed.
func TestWaitFindCapHandoffCause(t *testing.T) {
	t.Parallel()
	why := "A hand-off waits for you: the task's `loop.max_attempts` attempts in its round failed, the last a failed implementer run, and the hand-off says why."
	for _, tt := range []struct {
		name, failure, line string
	}{
		{"a quoted cause", "run failed with subtype \"error_max_turns\"", why},
		{"a cause in the log", publicCauseLogged, why},
		{"no cause kept", "", "A hand-off waits for you: the task's `loop.max_attempts` attempts in its round failed."},
	} {
		body := renderCapHandoff(3, tt.failure, 1, []string{"owner"}) +
			"\n\n<!-- issuedog:transition from=in-progress to=needs-human event=ain-progress-101 seen=101 -->"
		it := waitFindOne(t, waitIn(handbackSnap(false, handbackToProgress, body), Dirt{}))
		want := []string{tt.line,
			"Goal: Find out why the implementer's runs on this task fail, and fix that, so it can go on with the task.",
			"Done when: What made the implementer's runs fail is fixed.",
			"When it's done, reply `/done` on the issue or react 👍 to the hand-off."}
		if got := waitTexts(it.Lines); it.Kind != waitHandoff || strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("%s: the item is a %s, its lines %q; want a handoff, its lines %q", tt.name, it.Kind, got, want)
		}
		if got, want := capHandoffSaysWhy(body), tt.failure != ""; got != want {
			t.Errorf("%s: capHandoffSaysWhy = %t, want %t", tt.name, got, want)
		}
	}
}

// capHandoffSaysWhy reads only the paragraph after the hand-off's
// opening, the App's own text: a failure the hand-off quotes, fenced, or
// a field's text, can't make one that says nothing of why say it, and
// CRLF line ends, which waitGoalDone allows too, read the same.
func TestCapHandoffSaysWhy(t *testing.T) {
	t.Parallel()
	opening := "This task's 3 attempts in this round failed, so I've stopped running the implementer here."
	for body, want := range map[string]bool{
		opening + "\n\nThe last attempt, a run, failed:\n\n```text\nboom\n```":             true,
		opening + "\r\n\r\nThe last attempt, a run, failed; see the daemon's log for why.": true,
		opening + "\n\n**Goal:** The last attempt, a run, failed":                          false,
		opening + "\n\n```text\nThe last attempt, a run, failed\n```":                      false,
		opening: false,
	} {
		if got := capHandoffSaysWhy(body); got != want {
			t.Errorf("capHandoffSaysWhy(%q) = %t, want %t", body, got, want)
		}
	}
}
