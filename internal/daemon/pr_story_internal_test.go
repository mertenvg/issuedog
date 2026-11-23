package daemon

import (
	"strings"
	"testing"
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
