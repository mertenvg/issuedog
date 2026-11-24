package daemon

import (
	"fmt"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file writes what filing a failure on a story branch posts (SPEC
// §7.5, A failure on the story branch): the human task's and the bugs'
// titles and bodies, without their origin markers (failureChild adds
// them), and the comment on an open failure's bug. Each is escaped (SPEC
// §5.1), and each rule's detail and output, and each failed command's,
// is fenced and cut to its last findingChars characters, as findingsText
// cuts them.

// failureShort is sha's first 7 characters, for a title.
func failureShort(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// failureHumanTitle is the title of the human task filed for story's
// branch's tip tip, which fails rule 2.
func failureHumanTitle(story int, tip string) string {
	return fmt.Sprintf("Revert the protected path change at %s on story #%d's branch", failureShort(tip), story)
}

// failureBugTitle is the title of the bug filed for story's branch's tip
// tip, which passes rule 2 and fails another rule.
func failureBugTitle(story int, tip string) string {
	return fmt.Sprintf("Fix story #%d's branch, which fails its checks at %s", story, failureShort(tip))
}

// conflictBugTitle is the title of the conflict bug filed for main's
// commit main, which doesn't merge into story's branch's tip tip.
func conflictBugTitle(story int, tip, main string) string {
	return fmt.Sprintf("Merge main at %s into story #%d's branch at %s", failureShort(main), story, failureShort(tip))
}

// failureHeader is the first line of an issue filed for a failure on
// story's branch (SPEC §9.3, §11.2): the story, no scope, and pri, the
// story's priority, such as "p1".
func failureHeader(story int, pri string) string {
	return humanHeader(fmt.Sprintf("#%d", story), "", pri)
}

// renderFailureHuman is the body of the human task filed for story's
// branch, branch, in owner/repo, whose tip tip fails rule 2 (SPEC §7.5,
// Rule 2; §9.3, A story branch's failure), escaped: its header, what
// failed, rule 2's finding, rule2, cut and fenced (findingSection), then
// the template's fields in the daemon's own words: revert the change on
// the story branch with a new commit, and land it on main in a PR of its
// own if it's wanted. Its "Done when" is its tip check, that the branch's
// tip isn't tip any more (humanDoneWhen), and the cc line for notify
// closes it.
func renderFailureHuman(owner, repo string, story int, pri, branch, tip string, rule2 verify.Finding, notify []string) (string, error) {
	done, err := humanDoneWhen("", state.Check{Kind: state.CheckTip, Value: tip})
	if err != nil {
		return "", fmt.Errorf("its done when: %w", err)
	}
	h := planHumanTask{
		Goal: fmt.Sprintf("Take the change to protected paths at `%s` off the story branch `%s`, so its tip passes rule 2 again.", tip, branch),
		Steps: []string{
			"Read rule 2's finding above: it names each protected path the tip changes against the story's baseline.",
			fmt.Sprintf("Revert those changes on `%s` with a new commit, and push it to the branch.", branch),
			"If the change is wanted, land it on `main` in a PR of its own.",
		},
		Links: []string{
			fmt.Sprintf("https://github.com/%s/%s/commit/%s", owner, repo, tip),
			fmt.Sprintf("https://github.com/%s/%s/tree/%s", owner, repo, branch),
		},
		EstimatedTime: "15 minutes.",
	}
	why := humanWhyBlocks("its branch's tip passes its checks, and only a person can change a protected path", nil, story)
	parts := []string{
		failureHeader(story, pri),
		fmt.Sprintf("Story #%d's branch %s fails rule 2 at `%s`: it changes a protected path against the story's baseline, which only a person can change. "+
			"So no command ran on it, and the story's tasks wait until a later tip passes its checks.", story, checkCode(branch), tip),
		findingSection(rule2),
		humanFields(h, why, done),
	}
	return github.Escape(strings.Join(append(parts, ccLines(notify)...), "\n\n")), nil
}

// failureSections are the failed checks of a tip that passes rule 2, in
// the order checked, as findingsText gives them: rule 1's, from cmds, then
// each of fs, rules 3 and 7, each cut and fenced.
func failureSections(fs []verify.Finding, cmds []verify.CommandResult) []string {
	var parts []string
	if s := commandsSection(cmds); s != "" {
		parts = append(parts, s)
	}
	for _, f := range fs {
		parts = append(parts, findingSection(f))
	}
	return parts
}

// failureDone is the "Definition of done" of a bug filed for a failure on
// a story branch, in the daemon's own words.
var failureDone = []string{
	"verify.commands pass",
	"No new skip markers",
	"Tests added or updated for every changed behavior",
}

// renderFailureBug is the body of the bug filed for story's branch,
// branch, whose tip tip passes rule 2 and fails another rule against the
// story's baseline, base (SPEC §7.5, Any other rule), escaped: an SPEC
// §11.2 task, its header, then its Context naming the tip and each failed
// check, rule 1's commands, from cmds, then the rules of fs, each with its
// output, cut and fenced (failureSections), then its Change, Acceptance
// criteria, one for each failed check, Definition of done and Test plan,
// in the daemon's own words.
func renderFailureBug(story int, pri, branch, tip, base string, fs []verify.Finding, cmds []verify.CommandResult) string {
	facts := append([]string{fmt.Sprintf("Story #%d's branch %s fails its checks at `%s`, against the story's baseline, `main` at `%s`. "+
		"It passes rule 2, so this bug is their fix: the story's other tasks wait while it's open.", story, checkCode(branch), tip, base)},
		failureSections(fs, cmds)...)
	var criteria []string
	if commandsFailed(cmds) > 0 {
		criteria = append(criteria, "Rule 1: each of `verify.commands` passes on the story branch.")
	}
	for _, f := range fs {
		criteria = append(criteria, fmt.Sprintf("Rule %d: %s passes on the story branch.", f.Rule, f.Name))
	}
	return github.Escape(strings.Join([]string{
		failureHeader(story, pri),
		"## Context\n\n" + strings.Join(facts, "\n\n"),
		fmt.Sprintf("## Change\n\nFix what makes each check above fail, on the story branch %s, and nothing else.", checkCode(branch)),
		"## Acceptance criteria\n\n" + bullets(criteria),
		"## Definition of done\n\n" + bullets(failureDone),
		"## Test plan\n\nRun `verify.commands` on the fixed branch, and check each failed rule above again.",
	}, "\n\n"))
}

// renderConflictBug is the body of the conflict bug filed for main's
// commit main, which doesn't merge into story's branch, branch, at its
// tip tip (SPEC §7.5, A conflict), escaped: an SPEC §11.2 task, its
// header, then its Context naming the main commit, the tip and each of
// files, the conflicting files, each in a code span, then its Change, to
// merge the tip and resolve them, its Acceptance criteria, Definition of
// done and Test plan, in the daemon's own words.
func renderConflictBug(story int, pri, branch, tip, main string, files []string) string {
	names := make([]string, len(files))
	for i, f := range files {
		names[i] = "- " + checkCode(f)
	}
	list := strings.Join(names, "\n")
	if len(files) == 0 {
		list = "None."
	}
	return github.Escape(strings.Join([]string{
		failureHeader(story, pri),
		fmt.Sprintf("## Context\n\nMerging `main` at `%s` into story #%d's branch %s, at its tip `%s`, conflicts in these files:\n\n%s",
			main, story, checkCode(branch), tip, list),
		fmt.Sprintf("## Change\n\nStarting from `main` at `%s`, merge the tip `%s`, and resolve each conflict, keeping what both sides meant.", main, tip),
		"## Acceptance criteria\n\n" + bullets([]string{
			fmt.Sprintf("The head is a merge commit whose parents are `%s` and `%s`.", main, tip),
			"No conflict marker is left in the files above.",
		}),
		"## Definition of done\n\n" + bullets(failureDone),
		"## Test plan\n\nRun `verify.commands` on the merge.",
	}, "\n\n"))
}

// renderFailureComment is the comment on an open failure's bug of a story
// for its branch's later tip tip, which passes rule 2 and fails another
// rule against the story's baseline, base (SPEC §7.5, When the branch
// moves under a failure's bug), escaped, without its marker
// (failureComment): it names the branch and the tip, and gives each failed
// check, cut and fenced as the bug's own (failureSections).
func renderFailureComment(branch, tip, base string, fs []verify.Finding, cmds []verify.CommandResult) string {
	intro := fmt.Sprintf("The story branch %s moved to `%s`, which passes rule 2 but fails its checks against the story's baseline, `main` at `%s`. "+
		"This bug is open, so it's this bug's to fix too, and no second bug is filed.", checkCode(branch), tip, base)
	return github.Escape(strings.Join(append([]string{intro}, failureSections(fs, cmds)...), "\n\n"))
}
