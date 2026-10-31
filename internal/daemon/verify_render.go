package daemon

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/verify"
)

// findingChars is how much of the end of each finding's detail and output,
// and of each failed command's output, the findings comment gives (SPEC
// §10.1): the last 1,000 characters, as SPEC §7.5 cuts a story branch's
// failures.
const findingChars = 1000

// findingCommands is how many failed commands the findings comment shows,
// in the order they ran, before it says how many more failed, and
// commandChars how much of the start of each command it shows. With
// findingChars, they keep the comment within GitHub's limit, less
// textRoom, whatever the findings and verify.commands hold.
const (
	findingCommands = 5
	commandChars    = 200
)

// renderFindings is the comment on a task's move verifying → in-progress
// on findings (SPEC §7.2, §10.1): it names attempt, the implementer run's
// number in the task's round, of limit, loop.max_attempts, unless attempt
// is 0 for none (SPEC §8.3), head, the head checked, or says there was
// none, and main, main's commit it was checked against. Then
// each failed check gets a section, in the order checked: the head's own
// finding (Rule 0) or rule 2's, then rule 1's, from cmds, then rules 3 to
// 7. A rule's section gives its detail, and its output, if any, each cut
// to its last findingChars characters and fenced. Rule 1's names each
// failed command, fenced, why it failed, its timeout or exit status
// (CommandResult.Failure), and its output, cut and fenced the same way.
// Every text the daemon didn't write is fenced, and the whole is escaped
// (SPEC §5.1).
func renderFindings(head, main string, attempt, limit int, fs []verify.Finding, cmds []verify.CommandResult) string {
	var parts []string
	on := ""
	if attempt > 0 {
		on = fmt.Sprintf(" on attempt %d of %d,", attempt, limit)
	}
	if head == "" {
		parts = append(parts, fmt.Sprintf("Verify failed%s with no head to check, against `main` at `%s`.", on, main))
	} else {
		parts = append(parts, fmt.Sprintf("Verify failed%s at `%s`, checked against `main` at `%s`.", on, head, main))
	}
	first := func(f verify.Finding) bool { return f.Rule == 0 || f.Rule == 2 }
	for _, f := range fs {
		if first(f) {
			parts = append(parts, findingSection(f))
		}
	}
	if s := commandsSection(cmds); s != "" {
		parts = append(parts, s)
	}
	for _, f := range fs {
		if !first(f) {
			parts = append(parts, findingSection(f))
		}
	}
	parts = append(parts, "So the task is back at in-progress, for the implementer to fix.")
	return github.Escape(strings.Join(parts, "\n\n"))
}

// findingSection is f's section of the findings comment: its heading, by
// its rule's number and name, or for the head's own finding (Rule 0), by
// its name, then its detail and its output, each cut and fenced.
func findingSection(f verify.Finding) string {
	heading := fmt.Sprintf("### Rule %d: %s", f.Rule, f.Name)
	if f.Rule == 0 {
		heading = "### " + upperFirst(f.Name)
	}
	s := heading + "\n\n" + ivFenced(lastFindingChars(f.Detail))
	if f.Output != "" {
		s += "\n\nIts output ends:\n\n" + ivFenced(lastFindingChars(f.Output))
	}
	return s
}

// commandsSection is rule 1's section of the findings comment, for the
// failed commands among cmds, or "" when none failed: the first
// findingCommands of them, each with its command, why it failed and its
// output, then how many more failed, which the daemon's log names.
func commandsSection(cmds []verify.CommandResult) string {
	var failed []verify.CommandResult
	for _, c := range cmds {
		if c.Failure() != "" {
			failed = append(failed, c)
		}
	}
	if len(failed) == 0 {
		return ""
	}
	parts := []string{"### Rule 1: verify.commands"}
	for i, c := range failed {
		if i == findingCommands {
			parts = append(parts, fmt.Sprintf("…and %d more failed, which the daemon's log names.", len(failed)-findingCommands))
			break
		}
		s := "This command failed, " + c.Failure() + ":\n\n" + ivFenced(firstChars(c.Command, commandChars))
		if c.Output == "" {
			s += "\n\nIt printed nothing."
		} else {
			s += "\n\nIts output ends:\n\n" + ivFenced(lastFindingChars(c.Output))
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n\n")
}

// lastFindingChars is the last findingChars characters of s, starting with
// "…" when it was longer.
func lastFindingChars(s string) string {
	if utf8.RuneCountInString(s) <= findingChars {
		return s
	}
	r := []rune(s)
	return "…" + string(r[len(r)-findingChars:])
}

// firstChars is the first n characters of s, ending with "…" when it was
// longer.
func firstChars(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// upperFirst is s with its first letter in upper case.
func upperFirst(s string) string {
	if s == "" {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}
