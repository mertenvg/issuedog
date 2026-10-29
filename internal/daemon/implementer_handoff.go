package daemon

import (
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
)

// This file checks and renders the implementer's hand-off (SPEC §9.3,
// §12): an action only the owner can do blocks the task, so Apply moves it
// to needs-human with this comment, which /done or a trusted 👍 on it ends
// (handback.go).

// handoffTask is h as a human task's fields, so a hand-off is checked
// (checkHumanTask) and rendered (humanFields) as a human task is. Its
// "Done when" is the agent's text, with no check: a hand-off has no check
// the daemon runs (SPEC §9.3).
func handoffTask(h implementerHandoffFields) planHumanTask {
	return planHumanTask{Goal: h.Goal, WhyBlocks: h.WhyBlocks, Steps: h.Steps, Links: h.Links,
		EstimatedTime: h.EstimatedTime, DoneWhen: planDoneWhen{Text: h.DoneWhen}}
}

// checkHandoff is checkImplementer's rules for a handoff outcome: it has a
// hand-off, and no title, summary or questions (rule outcome); every field
// of SPEC §9.3's template is filled, at least one step and one link
// included, and each is one line (checkHumanTask, rule human-task or
// one-line); and its comment, as renderHandoff posts it for f, is within
// maxText less textRoom (rule length).
func checkHandoff(out implementerOutput, f implementerFacts, fail func(string, string, ...any)) {
	if out.Handoff == nil {
		fail(ruleOutcome, "handoff has a hand-off")
	}
	if out.Title != nil {
		fail(ruleOutcome, "handoff has no title")
	}
	if out.Summary != nil {
		fail(ruleOutcome, "handoff has no summary")
	}
	if out.Questions != nil {
		fail(ruleOutcome, "handoff has no questions")
	}
	if out.Handoff == nil {
		return
	}
	checkHumanTask("the hand-off", handoffTask(*out.Handoff), fail)
	if n := textLen(renderHandoff(*out.Handoff, f.Issue, f.Notify)); n > maxText-textRoom {
		fail(ruleLength, "the comment is %d characters, over %d", n, maxText-textRoom)
	}
}

// The hand-off comment's own words: what it is, and how to end it.
const (
	handoffIntro  = "The implementer needs an action only you can do before it can go on with this task."
	handoffFinish = "When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`."
)

// renderHandoff is the hand-off comment of task n (SPEC §9.3), escaped:
// handoffIntro, then the template's fields as a human task's body has
// them (humanFields), then handoffFinish, then the cc line for notify. Its
// "Why it blocks" names the task, "#<n> can't proceed until <reason>"
// (humanWhyBlocks), and its "Done when" is the agent's text (inline), as a
// human task's plain-text one is. Nothing is checked here: Apply renders
// only output that passed checkHandoff.
func renderHandoff(h implementerHandoffFields, n int, notify []string) string {
	t := handoffTask(h)
	parts := []string{
		handoffIntro,
		humanFields(t, humanWhyBlocks(t.WhyBlocks, []int{n}, n), orNone(inline(t.DoneWhen.Text))),
		handoffFinish,
	}
	return github.Escape(strings.Join(append(parts, ccLines(notify)...), "\n\n"))
}
