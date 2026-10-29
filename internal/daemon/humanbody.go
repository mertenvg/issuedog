package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// humanDoneTail follows the daemon's sentence for a "Done when" check: when
// it runs the check (SPEC §9.3, "Running the check").
const humanDoneTail = "I check this when you reply `/done` or react 👍 to this issue."

// humanBody is a human task's body (SPEC §9.3, #284), without its
// origin marker, escaped: the task's Parent line, then the §9.3 fields in
// order, each filled, then the cc line for notify. pri is the task's
// priority, it its plan item, and check its "Done when" check
// (checkFromPlan), which the origin marker records too (state.CheckFields).
//
//   - Agent text is escaped (SPEC §5.1) and made one line per field: its
//     markers escaped, mentions defused, headings demoted and task list
//     boxes stripped, and an empty field reads "None.".
//   - The steps are numbered "1.", "2." and so on, and the links are a "-"
//     list, each set apart by a blank line after it, so the field after it
//     isn't read as part of its last item (humanFields).
//   - "Why it blocks" names the issues of blocks, the children whose plan
//     items depend on this one, or the parent when there are none, then
//     the planner's reason (humanWhyBlocks, SPEC §9.3).
//   - "Done when" is what the daemon checks, in its own words, for a check,
//     or the planner's text for plain text (humanDoneWhen).
//
// A check of a kind it doesn't know is an error, as is a task with no
// human task body.
func humanBody(it planItem, check state.Check, blocks []int, parent int, pri string, notify []string) (string, error) {
	h := it.HumanTask
	if h == nil {
		return "", errors.New("a human task with no human task body")
	}
	done, err := humanDoneWhen(h.DoneWhen.Text, check)
	if err != nil {
		return "", fmt.Errorf("its done when: %w", err)
	}
	fields := humanFields(*h, humanWhyBlocks(h.WhyBlocks, blocks, parent), done)
	return github.Escape(strings.Join(append([]string{childParentLine(it, parent, pri), fields}, ccLines(notify)...), "\n\n")), nil
}

// humanFields is the SPEC §9.3 template's fields of h, in order, not yet
// escaped (github.Escape): a human task's body has them (humanBody), and
// so does a hand-off comment (renderHandoff). why and done are "Why it
// blocks" and "Done when" as the caller words them, made safe already.
// Every other field is agent text, made one line per field as humanBody
// says, and an empty one reads "None.". The steps are numbered "1.", "2."
// and so on, and the links are a "-" list, each set apart by a blank line
// after it, so the field after it isn't read as part of its last item.
func humanFields(h planHumanTask, why, done string) string {
	steps := make([]string, len(h.Steps))
	for i, s := range h.Steps {
		steps[i] = clean(strconv.Itoa(i+1) + ". " + text(s))
	}
	return strings.Join([]string{
		"**Goal:** " + orNone(inline(h.Goal)),
		"**Why it blocks:** " + why,
		"**Steps:**",
		orNone(strings.Join(steps, "\n")) + "\n",
		"**Links:**",
		bullets(h.Links) + "\n",
		"**Done when:** " + done,
		"**Estimated time:** " + orNone(inline(h.EstimatedTime)),
	}, "\n")
}

// humanWhyBlocks is "Why it blocks" (SPEC §9.3): "#12 and #13 can't proceed
// until <reason>", naming the issues of blocks in number order, or
// "#<parent> can't proceed until <reason>" when blocks is empty. reason is
// the planner's, escaped (inline), its first word lowercased if it's one
// lowerFirst knows, so it reads on after "until". An empty reason reads
// "None.".
func humanWhyBlocks(reason string, blocks []int, parent int) string {
	reason = inline(reason)
	if reason == "" {
		return "None."
	}
	nums := slices.Sorted(slices.Values(blocks))
	if len(nums) == 0 {
		nums = []int{parent}
	}
	refs := make([]string, len(nums))
	for i, n := range nums {
		refs[i] = "#" + strconv.Itoa(n)
	}
	return list(refs) + " can't proceed until " + lowerFirst(reason)
}

// humanDoneWhen is "Done when" for a human task whose "Done when" is text,
// with check c: the planner's text, escaped (inline), for plain text, the
// zero Check, or else the daemon's sentence saying exactly what humanCheck
// checks, then humanDoneTail. Each value in the sentence is inline code
// (checkCode), as in humanCheck's, so it reads as the check compares it.
// A kind it doesn't know is an error.
func humanDoneWhen(text string, c state.Check) (string, error) {
	v := checkCode(c.Value)
	var s string
	switch c.Kind {
	case "":
		return orNone(inline(text)), nil
	case state.CheckLabel:
		s = fmt.Sprintf("The label %s exists in this repo.", v)
	case state.CheckIssueType:
		s = fmt.Sprintf("This repo's owner, an organization, has the issue type %s, enabled.", v)
	case state.CheckFile:
		s = fmt.Sprintf("The file %s is on `main`.", v)
		if c.Contains != "" {
			s = fmt.Sprintf("The file %s on `main` contains %s.", v, checkCode(c.Contains))
		}
	case state.CheckRuleset:
		s = fmt.Sprintf("An active ruleset on `main` requires the check %s.", v)
	case state.CheckInstallation:
		s = fmt.Sprintf("The App is installed on %s, and not suspended.", v)
	default:
		return "", fmt.Errorf("a check of the unknown kind %q", c.Kind)
	}
	return s + " " + humanDoneTail, nil
}
