package daemon

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
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
// textRoom, whatever the findings and verify.commands hold, and so the
// verify cap's hand-off, which gives the findings with its own fields.
const (
	findingCommands = 5
	commandChars    = 200
)

// renderFindings is the comment on a task's move verifying → in-progress
// on findings (SPEC §7.2, §10.1): the findings as findingsText gives them,
// then that the task is back at in-progress, the whole escaped (SPEC
// §5.1).
func renderFindings(head, main string, attempt, limit int, fs []verify.Finding, cmds []verify.CommandResult) string {
	return github.Escape(findingsText(head, main, attempt, limit, fs, cmds) + findingsBack)
}

// findingsBack ends the comment of a move back on findings, the checks'
// and the verifier's. noVerdictBack ends it for a run with no readable
// verdict: nothing is known to be wrong, so the implementer's next run needs
// only a head of its own for the verifier to judge, and commits needs a
// commit past its start (SPEC §10.2, §12).
const (
	findingsBack  = "\n\nSo the task is back at in-progress, for the implementer to fix."
	noVerdictBack = "\n\nSo the task is back at in-progress. There's nothing to fix: make one empty commit saying so."
)

// renderVerdict is the comment on a task's move verifying → in-progress on
// the verifier's findings (SPEC §7.2, §10.2): the verdict as verdictText
// gives it, then that the task is back at in-progress, and for a run with no
// readable verdict, that there's nothing to fix, the whole escaped (SPEC
// §5.1).
func renderVerdict(head string, attempt, limit int, out verifierOutput) string {
	back := findingsBack
	if out.NoVerdict != "" {
		back = noVerdictBack
	}
	return github.Escape(verdictText(head, attempt, limit, out) + back)
}

// verdictText is the verifier's request_changes, or a run's lack of any
// verdict (SPEC §10.2), as the findings comment (renderVerdict) and the
// verify cap's hand-off (renderVerifyCapHandoff) both give it, unescaped: it
// opens by saying which it is, and names attempt, the
// implementer run's number in the task's round, of limit, loop.max_attempts,
// unless attempt is 0 for none (SPEC §8.3), and head, the head the run
// judged, which is git's. Then each finding gets a section: its criterion,
// its class on the blocking bar, and its detail. They're the agent's text
// (inline, block), which the caller escapes. Its notes aren't given, as
// they aren't findings. A run with no readable verdict (out.NoVerdict) has
// one paragraph in their place, the daemon's words alone, saying nothing is
// known to be wrong with the code, so the next run doesn't invent a fix.
func verdictText(head string, attempt, limit int, out verifierOutput) string {
	on := ""
	if attempt > 0 {
		on = fmt.Sprintf(" on attempt %d of %d,", attempt, limit)
	}
	said := "asked for changes"
	if out.NoVerdict != "" {
		said = "gave no verdict"
	}
	parts := []string{fmt.Sprintf("The verifier %s%s at `%s`.", said, on, head)}
	if out.NoVerdict != "" {
		parts = append(parts, "The verifier's run gave no readable verdict, so this counts as `request_changes`: "+out.NoVerdict+
			". Nothing is known to be wrong with the code.")
	}
	for _, f := range out.Findings {
		parts = append(parts, "### "+inline(f.Criterion)+"\n\nClass: `"+inline(f.Class)+"`\n\n"+block(f.Detail))
	}
	return strings.Join(parts, "\n\n")
}

// findingsText is the findings of a task's checks at verifying (SPEC
// §10.1), as the findings comment (renderFindings) and the verify cap's
// hand-off (renderVerifyCapHandoff) both give them, unescaped: it names
// attempt, the implementer run's number in the task's round, of limit,
// loop.max_attempts, unless attempt is 0 for none (SPEC §8.3), head, the
// head checked, or says there was none, and main, main's commit it was
// checked against. Then each failed check gets a section, in the order
// checked: the head's own finding (Rule 0) or rule 2's, then rule 1's,
// from cmds, then rules 3 to 7. A rule's section gives its detail, and its output, if any, each cut
// to its last findingChars characters and fenced. Rule 1's names each
// failed command, fenced, why it failed, its timeout or exit status
// (CommandResult.Failure), and its output, cut and fenced the same way.
// Every text the daemon didn't write is fenced, and the caller escapes the
// whole (SPEC §5.1).
func findingsText(head, main string, attempt, limit int, fs []verify.Finding, cmds []verify.CommandResult) string {
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
	return strings.Join(parts, "\n\n")
}

// verifyCapFields are the fields of the verify cap's hand-off (SPEC §8.3,
// §9.3), in the daemon's own words, as no run gave any, as
// capHandoffFields are for the failed runs'. It has no links: what to fix
// depends on the findings, which the hand-off gives above them
// (renderVerifyCapHandoff).
var verifyCapFields = implementerHandoffFields{
	Goal:      "Find out why this task fails verify, and change what makes it fail.",
	WhyBlocks: "what makes its head fail verify is fixed",
	Steps: []string{
		"Read the findings above.",
		"Fix their cause, such as an unclear criterion in this task's body, a failure on `main`, or a setting in `.issuedog/config.yaml`.",
	},
	DoneWhen:      "What made the task's head fail verify is fixed.",
	EstimatedTime: "It depends on the findings.",
}

// renderVerifyCapHandoff is the verify cap's hand-off comment on task n
// (SPEC §8.3, §9.3), for its move verifying → needs-human when the head of
// its round's last attempt fails verify, escaped: renderHandoff with
// verifyCapFields, opening with the task's attempts attempts in its round
// failing, so the implementer runs no more, then findings, the last
// attempt's findings as findingsText or, for the verifier's, verdictText
// gives them. attempts is the last
// attempt's number, loop.max_attempts or more: more when loop.max_attempts
// was lowered since the round's runs began. It quotes no run's failure, as
// the last attempt's run didn't fail: the failed runs' hand-off is
// renderCapHandoff's.
func renderVerifyCapHandoff(attempts int, findings string, n int, notify []string) string {
	intro := fmt.Sprintf("This task's %d attempts in this round failed, so I've stopped running the implementer here.\n\n%s", attempts, findings)
	return renderHandoff(intro, verifyCapFields, n, notify)
}

// verifyCapMove reports whether m is the verify cap's move of a task to
// needs-human (verifyFindings, Verifier.Apply): verifying → needs-human,
// keyed as the move back on findings would be (verifyKeyed).
func verifyCapMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.Verifying &&
		state.Stage(m.Fields["to"]) == state.NeedsHuman && verifyKeyed(m)
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
