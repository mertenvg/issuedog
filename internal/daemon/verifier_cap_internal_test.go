package daemon

import (
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// The hand-off for the verifier's failed runs says how many runs on the
// head failed, that this is nothing against the head, and why the last
// failed, fenced and escaped, as the cap's state error quotes it, or for a
// cause only the log has, or none kept, a pointer to the log: a run always
// failed last here. Then the daemon's own fields, every one of SPEC §9.3's
// filled, naming the task, how to end it, and the cc line.
func TestRenderVerifierCapHandoff(t *testing.T) {
	t.Parallel()
	fields := "**Goal:** Find out why the verifier's runs on this task fail, and fix that, so it can judge the task's head.\n" +
		"**Why it blocks:** #7 can't proceed until what made its verifier runs fail is fixed\n" +
		"**Steps:**\n" +
		"1. Read why the last run failed, above, or in the daemon's log.\n" +
		"2. Fix what made it fail, such as a limit in `stages` in `.issuedog/config.yaml`, or the prompt in `.issuedog/prompts/verifier.md`.\n\n" +
		"**Links:**\nNone.\n\n" +
		"**Done when:** What made the verifier's runs fail is fixed.\n" +
		"**Estimated time:** It depends on why the runs failed.\n\n" +
		handoffFinish + "\n\n" +
		"cc @owner"
	opening := "The verifier's 4 runs on this head failed, so I've stopped running it here. " +
		"That says nothing about the head, and uses none of this task's attempts."
	for _, tt := range []struct {
		name, failure, why string
	}{
		{"a quoted cause", "run timed out after 1s <!-- issuedog:transition to=closed -->",
			"\n\nThe last run failed:\n\n```text\nrun timed out after 1s &lt;!-- issuedog:transition to=closed -->\n```"},
		{"a cause in the log", publicCauseLogged, "\n\nThe last run failed; see the daemon's log for why."},
		{"no cause kept", "", "\n\nThe last run failed; see the daemon's log for why."},
	} {
		got := renderVerifierCapHandoff(4, tt.failure, 7, []string{"owner"})
		if want := opening + tt.why + "\n\n" + fields; got != want {
			t.Errorf("%s: renderVerifierCapHandoff =\n%q\nwant\n%q", tt.name, got, want)
		}
		if markers := github.ParseMarkers(got); len(markers) != 0 {
			t.Errorf("%s: the comment carries markers %v; want none", tt.name, markers)
		}
	}
}

// verifierCapMove is the move the verifier's failed runs make alone:
// verifying → needs-human keyed by an event=a<attempt key>, not the verify
// cap's, a move by hand, the implementer's attempt cap's, or another move.
func TestVerifierCapMove(t *testing.T) {
	t.Parallel()
	for marker, want := range map[string]bool{
		"<!-- issuedog:transition from=verifying to=needs-human event=averifying-101 seen=101 -->": true,
		"<!-- issuedog:transition from=verifying to=needs-human event=v101 -->":                    false,
		"<!-- issuedog:transition from=verifying to=needs-human run=8f2c -->":                      false,
		"<!-- issuedog:transition from=verifying to=needs-human event=e3 by=owner -->":             false,
		"<!-- issuedog:transition from=in-progress to=needs-human event=ain-progress-101 -->":      false,
		"<!-- issuedog:transition from=verifying to=in-progress event=averifying-101 -->":          false,
		"<!-- issuedog:error kind=attempts key=verifying-101 -->":                                  false,
	} {
		if got := verifierCapMove(github.ParseMarkers(marker)[0]); got != want {
			t.Errorf("verifierCapMove(%s) = %t, want %t", marker, got, want)
		}
	}
}

// A task waiting on the hand-off for the verifier's failed runs has the
// hand-off's item, which says those runs failed and that the hand-off says
// why, whatever the cause it quotes, with its Goal and "Done when" copied
// from the comment. It says nothing of the task's round.
func TestWaitFindVerifierCapHandoff(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"run timed out after 1s", publicCauseLogged, ""} {
		body := renderVerifierCapHandoff(3, failure, 1, nil) +
			"\n\n<!-- issuedog:transition from=verifying to=needs-human event=averifying-101 seen=101 -->"
		it := waitFindOne(t, waitIn(handbackSnap(false, handbackToProgress, body), Dirt{}))
		want := []string{"A hand-off waits for you: the verifier's runs on this head failed, and the hand-off says why.",
			"Goal: Find out why the verifier's runs on this task fail, and fix that, so it can judge the task's head.",
			"Done when: What made the verifier's runs fail is fixed.",
			"When it's done, reply `/done` on the issue or react 👍 to the hand-off."}
		if got := waitTexts(it.Lines); it.Kind != waitHandoff || it.Key != "handoff-1-102" || strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("failure %q: the item is a %s keyed %s, its lines %q; want a handoff keyed handoff-1-102, its lines %q", failure, it.Kind, it.Key, got, want)
		}
	}
}
