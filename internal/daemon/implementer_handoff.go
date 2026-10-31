package daemon

import (
	"fmt"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file checks and renders the implementer's hand-off (SPEC §9.3,
// §12): an action only the owner can do blocks the task, so Apply moves it
// to needs-human with this comment, which /done or a trusted 👍 on it ends
// (handback.go). The attempt cap's hand-off (capHandoff, SPEC §8.3) has
// the same form, with its own opening and fields, and so has the verify
// cap's (renderVerifyCapHandoff).

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
	if n := textLen(renderHandoff(handoffIntro, *out.Handoff, f.Issue, f.Notify)); n > maxText-textRoom {
		fail(ruleLength, "the comment is %d characters, over %d", n, maxText-textRoom)
	}
}

// The hand-off comment's own words: what it is, and how to end it.
const (
	handoffIntro  = "The implementer needs an action only you can do before it can go on with this task."
	handoffFinish = "When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`."
)

// renderHandoff is the hand-off comment of task n (SPEC §9.3), escaped:
// intro, which says why the task waits on the owner, handoffIntro for the
// implementer's own hand-off, then the template's fields as a human
// task's body has them (humanFields), then handoffFinish, then the cc line
// for notify. Its "Why it blocks" names the task, "#<n> can't proceed
// until <reason>" (humanWhyBlocks), and its "Done when" is h's text
// (inline), as a human task's plain-text one is. Nothing is checked here:
// Apply renders only output that passed checkHandoff.
func renderHandoff(intro string, h implementerHandoffFields, n int, notify []string) string {
	t := handoffTask(h)
	parts := []string{
		intro,
		humanFields(t, humanWhyBlocks(t.WhyBlocks, []int{n}, n), orNone(inline(t.DoneWhen.Text))),
		handoffFinish,
	}
	return github.Escape(strings.Join(append(parts, ccLines(notify)...), "\n\n"))
}

// capHandoffFields are the fields of the attempt cap's hand-off (SPEC
// §8.3, §9.3), in the daemon's own words, as no run gave any. It has no
// links: what to fix depends on why the runs failed, which its opening
// says (renderCapHandoff).
var capHandoffFields = implementerHandoffFields{
	Goal:      "Find out why the implementer's runs on this task fail, and fix that, so it can go on with the task.",
	WhyBlocks: "what made its implementer runs fail is fixed",
	Steps: []string{
		"Read why the last run failed, above, or in the daemon's log.",
		"Fix what made it fail, such as an unclear criterion in this task's body, a failure on `main`, or a setting in `.issuedog/config.yaml`.",
	},
	DoneWhen:      "What made the implementer's runs fail is fixed.",
	EstimatedTime: "It depends on why the runs failed.",
}

// renderCapHandoff is the attempt cap's hand-off comment on task n (SPEC
// §8.3, §9.3), escaped: renderHandoff with capHandoffFields, opening with
// the task's attempts attempts in its round failing, its implementer's
// runs and its verifies alike, then that the last was a run that failed,
// and why, failure, as the cap's state error quotes it
// (Lease.PublicFailure, publicCauseText): fenced, or for
// publicCauseLogged, a pointer to the daemon's log. With no failure, as
// when findings below the cap reached a loop.max_attempts lowered since,
// whose findings are on the task already, or from a lease written before
// it was kept, it says nothing of why, and the digest's item says nothing
// of the last attempt either (capHandoffSaysWhy). It never quotes
// findings: when the head of the round's last attempt fails verify, the
// hand-off is renderVerifyCapHandoff's, at verifying.
func renderCapHandoff(attempts int, failure string, n int, notify []string) string {
	intro := fmt.Sprintf("This task's %d attempts in this round failed, so I've stopped running the implementer here.", attempts)
	if failure != "" {
		if failure == publicCauseLogged {
			failure = ""
		}
		intro += publicCauseText(capCauseWhat, failure)
	}
	return renderHandoff(intro, capHandoffFields, n, notify)
}

// capCauseWhat is what the attempt cap's hand-off says failed last, in
// publicCauseText's paragraph, when a run did.
const capCauseWhat = "attempt, a run,"

// capHandoffSaysWhy reports whether body, the attempt cap's hand-off
// comment as renderCapHandoff writes it, says why the round's last
// attempt failed: whether the paragraph after its opening is
// publicCauseText's for a run that failed last. Both are the App's own
// text, before any failure it quotes, and the comment is the App's and
// clean, as latestHandoff finds it.
func capHandoffSaysWhy(body string) bool {
	_, rest, _ := strings.Cut(strings.ReplaceAll(body, "\r\n", "\n"), "\n\n")
	return strings.HasPrefix(rest, "The last "+capCauseWhat+" failed")
}

// capHandoffKey keys the attempt cap's move to needs-human on a task:
// event=a<attempt key>, the round of attempts that reached the cap
// (attemptKey), so each round's cap moves the task once.
func capHandoffKey(attemptKey string) state.Key {
	return state.Key{Field: "event", ID: "a" + attemptKey}
}

// capHandoffMove reports whether m is the attempt cap's move of a task to
// needs-human (capHandoff): in-progress → needs-human, keyed by
// capHandoffKey, not by a run, as the implementer's own hand-off is.
func capHandoffMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.InProgress &&
		state.Stage(m.Fields["to"]) == state.NeedsHuman && strings.HasPrefix(m.Fields["event"], "a")
}
