package daemon

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// A task in a story's move to pr-open names its story branch, which an
// admin may have chosen, as a code span with its markers escaped
// (checkCode, SPEC §5.1): a backtick in the name widens the fence, a
// marker opener is escaped, and Markdown markers stay inside the span. A
// PR to main's comment is as it was.
func TestRenderPROpenStoryBranch(t *testing.T) {
	t.Parallel()
	head, tip := strings.Repeat("ab", 20), strings.Repeat("cd", 20)
	got := renderPROpen(4, head, "issuedog/2-a`b<!--c*d_[e]", tip)
	want := "PR #4 into the story branch ``issuedog/2-a`b&lt;!--c*d_[e]`` is open for this task, verified at `" + head +
		"` against the branch's tip `" + tip + "`. The App merges it once it's checked, then closes this task."
	if got != want {
		t.Errorf("renderPROpen =\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(got, "<!--") {
		t.Errorf("renderPROpen =\n%s\nwant no marker opener", got)
	}
	main := "PR #4 to `main` is open for this task, verified at `" + head + "`. It waits for your review and merge, and merging it closes this task."
	if got := renderPROpen(4, head, prBase, ""); got != main {
		t.Errorf("renderPROpen to main =\n%s\nwant\n%s", got, main)
	}
}

// A trusted reopen of a task in a story's PR says the App merges it into
// the story branch once it's checked, as the owner reviews only the
// story's PR (SPEC §7.5).
func TestRenderPRReopenedStory(t *testing.T) {
	t.Parallel()
	want := "`mal@​lory` reopened PR #2, so this task is back at `pr-open`. The App merges it into the story branch once it's checked, then closes this task."
	if got := renderPRReopened(2, "mal@lory", true); got != want {
		t.Errorf("renderPRReopened =\n%s\nwant\n%s", got, want)
	}
}

// The hand-off for a task in a story's PR, closed or reopened by someone
// untrusted, says in its first step that, back at pr-open, the App merges
// the PR into the story branch once it's checked, and never that it waits
// for the owner's review and merge.
func TestRenderPRClosedHandoffStory(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		reopened bool
		step     string
	}{
		{"a close", false, "1. To keep the PR, reopen it: the task goes back to `pr-open`, where the App merges it into the story branch once it's checked.\n"},
		{"an untrusted reopen", true, "1. To keep the PR, close it and reopen it yourself: the task goes back to `pr-open`, where the App merges it into the story branch once it's checked.\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := renderPRClosedHandoff(1, 2, "https://github.com/o/r/pull/2", tt.reopened, "mallory", true, nil)
			if !strings.Contains(got, tt.step) || strings.Contains(got, "your review and merge") {
				t.Errorf("the hand-off =\n%s\nwant it to hold\n%s\nand never the owner's review and merge", got, tt.step)
			}
		})
	}
}

// A task in a story at pr-open whose PR into its story branch is listed
// closed, with that close handed off already, is stalled as any task's
// is, but its item says a reopened PR is the App's to merge into the
// story branch, not one that waits on the owner's review (SPEC §7.5,
// §9.4).
func TestWaitFindStalledClosedStory(t *testing.T) {
	t.Parallel()
	closed := listedPR{Number: 7, Title: "feat: x", ClosedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	handoff := "<!-- issuedog:transition from=pr-open to=needs-human event=" + prClosedKey(closed).ID + " -->"
	hand := "<!-- issuedog:transition from=needs-human to=pr-open event=l900 -->"
	s := prItemSnap("pr-open", prItemMoved("pr-open", " pr=7"), handoff, hand)
	s.Parent = 2
	in := waitIn(s, Dirt{})
	in.PR = &closed
	it := waitFindOne(t, in)
	want := []string{
		"This task is at `pr-open`, but its PR #7 is closed, and its close was handed off already, so the close moves it no more.",
		"Reopen PR #7 for the App to merge it into the story branch once it's checked, or set `stage:ready` for a new round, or close this issue.",
	}
	if it.Kind != waitStalled || it.Key != "stalled-1-pr7" || it.ClosedPR != 7 || !slices.Equal(waitTexts(it.Lines), want) {
		t.Errorf("the item = %+v; want a stalled task keyed stalled-1-pr7, carrying PR 7, its lines %q", it, want)
	}
}
