package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// childSpec is everything the daemon creates a child with, from one item
// of an approved plan (plan #249, T2): its title, body and labels, its
// issue type on a repo that uses issue types, and the first stage its
// first move takes it to (Q1a). Item is the plan item's ID, such as S2 or
// T4.
type childSpec struct {
	Item, Title, Body string
	// Labels are the type label on a labels repo, then scope:<x> for a
	// task, priority:pN and the stage label, in that order.
	Labels []string
	// IssueType is "Story" or "Task" on a repo that uses issue types
	// (types: issue-types), where Labels has no type label, else "".
	IssueType string
	// Stage is the first stage: planning for a story, ready for a task and
	// needs-human for a human task (SPEC §7.1), which the created row for
	// its class enters (state.TriggerCreated).
	Stage state.Stage
}

// childMaxBody is the most a child's body may be, by textLen: GitHub's
// limit, less textRoom, which keeps room for a dirty note.
const childMaxBody = maxText - textRoom

// childSpecs describes the child of each of items, in plan order: the
// items of revision rev of parent's approved plan. A story is an item
// whose ID is S<n>, and a task one whose ID is T<n>, as the planner's
// schema gives them; a task is human when its item says so. issueTypes is
// set on a repo that uses issue types, and notify is config's notify, for
// a human task's cc line.
//
//   - A story's title starts with its ID, "S2. Title", unless the planner
//     already wrote it so. A task's is as planned.
//   - A story's body is its Parent and Priority line, Outcome, Acceptance
//     criteria and "Depends on", as #249's is. A task's follows SPEC §11.2
//     and a human task's §9.3, then its cc line.
//   - Every body's last line is its origin marker, plan=#N rev=R item=ID,
//     with human=1 for a human task, which is how Derive knows one
//     (state.Origin.Human). It's the body's only marker: planner text is
//     escaped as a spec's is (SPEC §11.1), with headings demoted, task
//     list boxes stripped, mentions defused and markers escaped, and an
//     empty section reads "None.".
//   - The priority is the item's, else parent's priority:pN, else p2.
//
// An item it can't describe is an error naming it: a task with no scope
// or no body of its kind, or a body over childMaxBody. Nothing is
// described then.
func childSpecs(parent *github.Snapshot, rev int, items []planItem, issueTypes bool, notify []string) ([]childSpec, error) {
	specs := make([]childSpec, 0, len(items))
	for _, it := range items {
		c, err := childSpecOf(parent, rev, it, issueTypes, notify)
		if err != nil {
			return nil, fmt.Errorf("describe the child of plan #%d rev %d item %s: %w", parent.Number, rev, it.ID, err)
		}
		specs = append(specs, c)
	}
	return specs, nil
}

// childSpecOf is childSpecs for one item.
func childSpecOf(parent *github.Snapshot, rev int, it planItem, issueTypes bool, notify []string) (childSpec, error) {
	story := strings.HasPrefix(it.ID, "S")
	pri := cmp.Or(it.Priority, "p"+strconv.Itoa(priority(parent)))
	c := childSpec{Item: it.ID, Title: childTitle(it, story)}
	var text, typ string
	switch {
	case story:
		typ, c.Stage = "story", state.Planning
		text = childStoryBody(it, parent.Number, pri)
	case it.Scope == "":
		return childSpec{}, errors.New("a task with no scope")
	case it.Human && it.HumanTask == nil:
		return childSpec{}, errors.New("a human task with no human task body")
	case it.Human:
		typ, c.Stage = "task", state.NeedsHuman
		text = childHumanTaskBody(it, parent.Number, pri, notify)
	case it.Task == nil:
		return childSpec{}, errors.New("a task with no task body")
	default:
		typ, c.Stage = "task", state.Ready
		text = childTaskBody(it, parent.Number, pri)
	}
	origin := github.Marker{Kind: "origin", Fields: map[string]string{
		"plan": "#" + strconv.Itoa(parent.Number), "rev": strconv.Itoa(rev), "item": it.ID}}
	if c.Stage == state.NeedsHuman {
		origin.Fields["human"] = "1"
	}
	marker, err := github.FormatMarker(origin)
	if err != nil {
		return childSpec{}, fmt.Errorf("origin marker: %w", err)
	}
	c.Body = text + "\n\n" + marker
	if n := textLen(c.Body); n > childMaxBody {
		return childSpec{}, fmt.Errorf("its body is %d characters, over %d", n, childMaxBody)
	}
	if issueTypes {
		c.IssueType = strings.ToUpper(typ[:1]) + typ[1:]
	} else {
		c.Labels = append(c.Labels, "type:"+typ)
	}
	if !story {
		c.Labels = append(c.Labels, "scope:"+it.Scope)
	}
	stage, _ := c.Stage.Label()
	c.Labels = append(c.Labels, "priority:"+pri, stage)
	return c, nil
}

// childTitle is it's title on one line: a story's starts with its ID,
// "S2. Title", once.
func childTitle(it planItem, story bool) string {
	title := strings.TrimSpace(normalize(it.Title, ""))
	if story && !strings.HasPrefix(title, it.ID+". ") {
		title = it.ID + ". " + title
	}
	return title
}

// childStoryBody is a story's body, without its origin marker, escaped:
// its Parent and Priority line, Outcome, Acceptance criteria, then what it
// depends on, by item ID.
func childStoryBody(it planItem, parent int, pri string) string {
	deps := "nothing"
	if len(it.DependsOn) > 0 {
		ids := make([]string, len(it.DependsOn))
		for i, d := range it.DependsOn {
			ids[i] = inline(d)
		}
		deps = strings.Join(ids, ", ")
	}
	return github.Escape(strings.Join([]string{
		fmt.Sprintf("**Parent:** #%d   **Priority:** %s", parent, pri),
		"## Outcome\n\n" + block(it.Outcome),
		"## Acceptance criteria\n\n" + bullets(it.Criteria),
		"Depends on: " + deps + ".",
	}, "\n\n"))
}

// childParentLine is a task's first line (SPEC §11.2): its parent, scope
// and priority.
func childParentLine(it planItem, parent int, pri string) string {
	return fmt.Sprintf("**Parent:** #%d   **Scope:** scope:%s   **Priority:** %s", parent, inline(it.Scope), pri)
}

// childTaskBody is a task's body, without its origin marker, escaped: the
// SPEC §11.2 template, with the files the planner named in its Context.
func childTaskBody(it planItem, parent int, pri string) string {
	t := it.Task
	return github.Escape(strings.Join([]string{
		childParentLine(it, parent, pri),
		"## Context\n\n" + block(t.Context),
		"## Change\n\n" + block(t.Change),
		"## Acceptance criteria\n\n" + bullets(it.Criteria),
		"## Definition of done\n\n" + bullets(t.DefinitionOfDone),
		"## Test plan\n\n" + block(t.TestPlan),
	}, "\n\n"))
}

// childHumanTaskBody is a human task's body, without its origin marker,
// escaped: the task's Parent line, then the SPEC §9.3 fields in order,
// each filled, then the cc line for notify. "Why it blocks" is the
// planner's, which names items by ID: the children aren't numbered yet
// when it's written (#224's reconcile decision 7). "Done when" is its
// text: the check the daemon runs is S4's. The steps and links are lists,
// set apart by a blank line after them, so the field after them isn't read
// as part of their last item.
func childHumanTaskBody(it planItem, parent int, pri string, notify []string) string {
	h := it.HumanTask
	steps := make([]string, len(h.Steps))
	for i, s := range h.Steps {
		steps[i] = clean(strconv.Itoa(i+1) + ". " + text(s))
	}
	fields := strings.Join([]string{
		"**Goal:** " + orNone(inline(h.Goal)),
		"**Why it blocks:** " + orNone(inline(h.WhyBlocks)),
		"**Steps:**",
		orNone(strings.Join(steps, "\n")) + "\n",
		"**Links:**",
		bullets(h.Links) + "\n",
		"**Done when:** " + orNone(inline(h.DoneWhen.Text)),
		"**Estimated time:** " + orNone(inline(h.EstimatedTime)),
	}, "\n")
	return github.Escape(strings.Join(append([]string{childParentLine(it, parent, pri), fields}, ccLines(notify)...), "\n\n"))
}
