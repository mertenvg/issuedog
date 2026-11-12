package daemon

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file writes what filing a pass's candidates posts (SPEC §10.2): each
// follow-up's title, body and labels, and the filed record on the task.

// followUpPriority is every follow-up's priority (SPEC §10.2).
const followUpPriority = "p3"

// followUpTitle is c's title as the follow-up's issue title: on one line,
// with each control character a space and no U+FEFF (normalize), trimmed,
// as a plan item's child's is (childTitle). A title isn't Markdown, so
// nothing else in it needs escaping. It's "" for a title that's only
// spaces and control characters, which no issue can have.
func followUpTitle(c verifierFollowUp) string {
	return strings.TrimSpace(normalize(c.Title, ""))
}

// followUpBody is the body of the follow-up for candidate item of task's
// pass, c, whose comment is at link (SPEC §10.2): which verify found it and
// where, its scope, scenario, likelihood and impact, that it waits at intake
// for approval and how to give it, then the cc line for notify, and origin,
// its origin marker as written, last. c's fields are the verifier's text, so
// each is cleaned and defused (bullets), and the whole text is escaped (SPEC
// §5.1), so origin is the body's only marker.
func followUpBody(task int, link string, item string, c verifierFollowUp, notify []string, origin string) string {
	parts := []string{
		fmt.Sprintf("The verifier found this as it passed #%d: it's %s of [that pass](%s), rarer than the blocking bar, so it didn't hold #%d back.",
			task, item, link, task),
		bullets([]string{"Scope: " + c.Scope, "Scenario: " + c.Scenario, "Likelihood: " + c.Likelihood, "Impact: " + c.Impact}),
		"It waits at `intake` for your approval. React 👍 to this issue to approve it, and it goes to `ready`, or close it to drop it.",
	}
	return github.Escape(strings.Join(append(parts, ccLines(notify)...), "\n\n")) + "\n\n" + origin
}

// followUpLabels are the follow-up's labels for c (SPEC §10.2): type:task,
// but on a repo that uses issue types, where typ is "Task" instead, then
// scope:<scope> if scopes, config's, still lists c's scope, then
// priority:p3 and stage:intake. A scope a reload has since dropped from the
// list is left off, and leftOff is true, so the caller logs it.
func followUpLabels(c verifierFollowUp, issueTypes bool, scopes []string) (labels []string, typ string, leftOff bool) {
	if issueTypes {
		typ = "Task"
	} else {
		labels = append(labels, "type:task")
	}
	if slices.Contains(scopes, c.Scope) {
		labels = append(labels, "scope:"+c.Scope)
	} else {
		leftOff = true
	}
	stage, _ := state.Intake.Label()
	return append(labels, "priority:"+followUpPriority, stage), typ, leftOff
}

// filedChild is one candidate of a pass and its follow-up, found or filed:
// its place in the pass, F1, F2 and so on, the follow-up's number, and the
// candidate's title, the verifier's text.
type filedChild struct {
	Item   string
	Number int
	Title  string
}

// filedText is the filed record's text (SPEC §5, filed): that the
// follow-ups of the pass at link are filed, then each of filed, in the
// pass's order, as "- F1 #812: title". A title is the verifier's text, so
// it's escaped (inline), and so is the whole text (SPEC §5.1).
func filedText(link string, filed []filedChild) string {
	lines := []string{fmt.Sprintf("I've filed the follow-ups of [this pass](%s), each waiting at `intake` for its approval:", link)}
	for _, f := range filed {
		lines = append(lines, fmt.Sprintf("- %s #%d: %s", f.Item, f.Number, inline(f.Title)))
	}
	return github.Escape(strings.Join(lines, "\n"))
}

// filedChildren is the filed record's children=: each of filed's numbers,
// in the pass's order, joined by commas.
func filedChildren(filed []filedChild) string {
	nums := make([]string, len(filed))
	for i, f := range filed {
		nums[i] = strconv.Itoa(f.Number)
	}
	return strings.Join(nums, ",")
}
