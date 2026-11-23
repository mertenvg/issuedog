package daemon

import (
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

// The App's newest issuedog/verify on a head decides whether the PR step
// posts one: only a success of the App's own stands. Another App's run,
// listed as if GitHub ignored the app_id filter, never counts, nor does a
// run of another name, and the App's failure after a success needs a new
// one (SPEC §10.4). A person's head's failure is matched the same way:
// only the App's newest failure stands for it, so a success, or a
// failure older than one, needs a new one.
func TestPRChecked(t *testing.T) {
	t.Parallel()
	const app, other = 7, 9
	run := func(id, appID int64, name, conclusion string) github.CheckRun {
		return github.CheckRun{ID: id, Name: name, Status: "completed", Conclusion: conclusion, AppID: appID}
	}
	for _, tt := range []struct {
		name       string
		runs       []github.CheckRun // newest first
		conclusion string
		want       bool
	}{
		{"none", nil, "success", false},
		{"the App's success", []github.CheckRun{run(1, app, github.VerifyCheck, "success")}, "success", true},
		{"another App's success alone", []github.CheckRun{run(2, other, github.VerifyCheck, "success")}, "success", false},
		{"another App's success, newer than the App's failure", []github.CheckRun{
			run(3, other, github.VerifyCheck, "success"), run(2, app, github.VerifyCheck, "failure")}, "success", false},
		{"another App's failure, newer than the App's success", []github.CheckRun{
			run(3, other, github.VerifyCheck, "failure"), run(2, app, github.VerifyCheck, "success")}, "success", true},
		{"the App's failure, newer than its success", []github.CheckRun{
			run(3, app, github.VerifyCheck, "failure"), run(2, app, github.VerifyCheck, "success")}, "success", false},
		{"the App's success of another name", []github.CheckRun{run(1, app, "issuedog/tip", "success")}, "success", false},
		{"the App's run still queued", []github.CheckRun{{ID: 1, Name: github.VerifyCheck, Status: "queued", AppID: app}}, "success", false},
		{"no failure", nil, "failure", false},
		{"the App's failure, for a failure", []github.CheckRun{run(1, app, github.VerifyCheck, "failure")}, "failure", true},
		{"the App's success, for a failure", []github.CheckRun{run(1, app, github.VerifyCheck, "success")}, "failure", false},
		{"the App's success, newer than its failure, for a failure", []github.CheckRun{
			run(3, app, github.VerifyCheck, "success"), run(2, app, github.VerifyCheck, "failure")}, "failure", false},
		{"another App's failure alone, for a failure", []github.CheckRun{run(2, other, github.VerifyCheck, "failure")}, "failure", false},
	} {
		if got := prChecked(tt.runs, app, tt.conclusion); got != tt.want {
			t.Errorf("%s: prChecked(%s) = %t, want %t", tt.name, tt.conclusion, got, tt.want)
		}
	}
}

// The hand-off for a head that changes nothing is verifying → needs-human,
// keyed run= with head=, and neither cap's hand-off from verifying is one,
// nor is it the verify cap's (SPEC §9.3).
func TestNothingHandoffMove(t *testing.T) {
	t.Parallel()
	for marker, want := range map[string]bool{
		"<!-- issuedog:transition from=verifying to=needs-human run=rv head=abc seen=101 -->":   true,
		"<!-- issuedog:transition from=verifying to=needs-human run=rv seen=101 -->":            false,
		"<!-- issuedog:transition from=verifying to=needs-human event=v101 head=abc -->":        false,
		"<!-- issuedog:transition from=verifying to=needs-human event=averifying-101 -->":       false,
		"<!-- issuedog:transition from=verifying to=pr-open run=rv head=abc pr=3 seen=101 -->":  false,
		"<!-- issuedog:transition from=in-progress to=needs-human run=r1 head=abc seen=101 -->": false,
		"<!-- issuedog:error kind=attempts key=verifying-101 -->":                               false,
	} {
		m := github.ParseMarkers(marker)[0]
		if got := nothingHandoffMove(m); got != want {
			t.Errorf("nothingHandoffMove(%s) = %t, want %t", marker, got, want)
		}
		if want && verifyCapMove(m) {
			t.Errorf("verifyCapMove(%s) = true, want false: it's the hand-off for a head that changes nothing", marker)
		}
	}
}

// The hand-off for a head that changes nothing is in the SPEC §9.3 form:
// it names the head and why it gets no PR, then the daemon's own fields,
// how to end it, and the cc line. Its last line says /done or a 👍 is for
// a change, and closing the issue for none, so a 👍 out of habit isn't
// taken for a yes.
func TestRenderNothingHandoff(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("ab", 20)
	want := "The verifier passed `" + head + "`, but it changes nothing on `main`: its own diff from its merge base with `main` is empty, so it gets no PR.\n\n" +
		"**Goal:** Decide whether this task needs a change on `main`, as its verified head makes none.\n" +
		"**Why it blocks:** #4 can't proceed until it's decided whether it needs a change on `main`\n" +
		"**Steps:**\n" +
		"1. Read the task's acceptance criteria, and the verifier's pass above.\n" +
		"2. If nothing needs to change, close this issue.\n" +
		"3. If something does, say what in a comment, then reply `/done` for a new round.\n\n" +
		"**Links:**\nNone.\n\n" +
		"**Done when:** The issue is closed, or a new round is asked for.\n" +
		"**Estimated time:** A few minutes.\n\n" +
		"If something needs to change, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`. If not, close this issue.\n\n" +
		"cc @owner"
	if got := renderNothingHandoff(head, prBase, 4, []string{"owner"}); got != want {
		t.Errorf("the hand-off =\n%s\nwant\n%s", got, want)
	}
}

// A task waiting on the hand-off for a head that changes nothing has the
// hand-off's item, which says the verifier passed a head that changes
// nothing on main, so it gets no PR, with its Goal and "Done when" copied
// from the comment, and ends as the hand-off does: /done or a 👍 for a
// change, a close for none (SPEC §9.4).
func TestWaitFindNothingHandoff(t *testing.T) {
	t.Parallel()
	body := renderNothingHandoff(strings.Repeat("ab", 20), prBase, 1, nil) +
		"\n\n<!-- issuedog:transition from=verifying to=needs-human run=rv head=" + strings.Repeat("ab", 20) + " seen=101 -->"
	it := waitFindOne(t, waitIn(handbackSnap(false, handbackToProgress, body), Dirt{}))
	want := []string{"A hand-off waits for you: the verifier passed a head that changes nothing on `main`, so it gets no PR.",
		"Goal: Decide whether this task needs a change on `main`, as its verified head makes none.",
		"Done when: The issue is closed, or a new round is asked for.",
		"If something needs to change, reply `/done` on the issue or react 👍 to the hand-off, and the task goes back to `ready`. If not, close the issue."}
	if got := waitTexts(it.Lines); it.Kind != waitHandoff || it.Key != "handoff-1-102" || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the item is a %s keyed %s, its lines %q; want a handoff keyed handoff-1-102, its lines %q", it.Kind, it.Key, got, want)
	}
}
