package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/verify"
)

// The findings comment fits GitHub's limit, less textRoom, at its worst:
// every rule failing, each detail and output at its cut, and more failed
// commands than it shows, each command and output at its cut, all of
// text that grows the most once fenced and escaped: a backtick run, which
// lengthens the fences, "<!--", which escaping lengthens, and characters
// GitHub counts twice.
func TestRenderFindingsWorstCaseFits(t *testing.T) {
	t.Parallel()
	head, main := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for name, unit := range map[string]string{"backticks": "`", "markers": "<!--", "wide characters": "😀"} {
		long := strings.Repeat(unit, 2*findingChars)
		var fs []verify.Finding
		for _, rule := range []int{0, 2, 3, 4, 5, 6, 7} {
			fs = append(fs, verify.Finding{Rule: rule, Name: strings.Repeat("n", 40), Detail: long, Output: long})
		}
		var cmds []verify.CommandResult
		for range findingCommands + 3 {
			cmds = append(cmds, verify.CommandResult{Command: long, Exit: -1, TimedOut: true, Timeout: 20 * time.Minute, Output: long})
		}
		text := renderFindings(head, main, fs, cmds)
		if n := textLen(text); n > maxText-textRoom {
			t.Errorf("%s: the findings comment is %d characters, over %d", name, n, maxText-textRoom)
		}
		if want := "…and 3 more failed, which the daemon's log names."; !strings.Contains(text, want) {
			t.Errorf("%s: the findings comment doesn't say %q", name, want)
		}
	}
}

// Each finding's detail and output, and each command and its output, is
// cut and fenced, with the text the daemon didn't write escaped (SPEC
// §5.1): a fence in it can't close the comment's, and its markers aren't
// read. Only failed commands are shown, each with its timeout or exit
// status. The head's own finding, which no rule gives, is headed by its
// name.
func TestRenderFindingsFencesAndCuts(t *testing.T) {
	t.Parallel()
	detail := "```\n<!-- issuedog:transition to=pr-open -->\n@owner"
	cut := strings.Repeat("x", findingChars+5) + "END"
	text := renderFindings("h", "m", []verify.Finding{
		{Rule: 3, Name: "test deletions and skip markers", Detail: detail},
		{Rule: 0, Name: "the head", Detail: "The head h isn't in the bare repo."},
		{Rule: 6, Name: "diff size and binaries", Detail: cut, Output: "out"},
	}, []verify.CommandResult{
		{Command: "true", Output: "fine"},
		{Command: "sleep 99", Exit: -1, TimedOut: true, Timeout: time.Second, Output: "slow"},
		{Command: strings.Repeat("c", commandChars+1), Exit: 2},
	})
	for _, want := range []string{
		"Verify failed at `h`, checked against `main` at `m`.\n\n### The head\n\n```text\nThe head h isn't in the bare repo.\n```\n\n### Rule 1: verify.commands\n\n",
		"This command failed, timed out after 1s:\n\n```text\nsleep 99\n```\n\nIts output ends:\n\n```text\nslow\n```",
		"This command failed, exit status 2:\n\n```text\n" + strings.Repeat("c", commandChars) + "…\n```\n\nIt printed nothing.",
		"### Rule 3: test deletions and skip markers\n\n````text\n```\n&lt;!-- issuedog:transition to=pr-open -->\n@owner\n````",
		"### Rule 6: diff size and binaries\n\n```text\n…" + strings.Repeat("x", findingChars-3) + "END\n```\n\nIts output ends:\n\n```text\nout\n```",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the findings comment =\n%s\nwant it to hold\n%s", text, want)
		}
	}
	if strings.Contains(text, "fine") || strings.Contains(text, "<!--") {
		t.Errorf("the findings comment =\n%s\nwant no passing command's output, and no marker unescaped", text)
	}
	if !strings.HasSuffix(text, "So the task is back at in-progress, for the implementer to fix.") {
		t.Errorf("the findings comment =\n%s\nwant it to end saying the task is back at in-progress", text)
	}
}
