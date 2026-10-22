package daemon

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// childSpec is everything the daemon creates a child with, from one item
// of an approved plan (SPEC §11.6): its title, body and labels, its
// issue type on a repo that uses issue types, and the first stage its
// first move takes it to (SPEC §7.2). Item is the plan item's ID, such as
// S2 or T4.
type childSpec struct {
	Item, Title string
	// Body is the child's body, or "" for a human task, whose body names
	// the issues it blocks, so it's written only once they're filed
	// (bodyOf).
	Body string
	// Labels are the type label on a labels repo, then scope:<x> for a
	// task, priority:pN and the stage label, then model:<suffix> and
	// effort:<level> when the item sets them (childOverrides), in that
	// order.
	Labels []string
	// leftOff is why each override label the item asks for is left off
	// Labels, such as its model, when a reload has since dropped it from
	// models.allowed. ChildrenVisit logs each when it files the child.
	leftOff []string
	// IssueType is "Story" or "Task" on a repo that uses issue types
	// (types: issue-types), where Labels has no type label, else "".
	IssueType string
	// Stage is the first stage: planning for a story, ready for a task and
	// needs-human for a human task (SPEC §7.1), which the created row for
	// its class enters (state.TriggerCreated).
	Stage state.Stage
	// Blocks are the items of a human task's plan that depend on it, in
	// plan order, whose issues its "Why it blocks" names (SPEC §9.3).
	Blocks []string
	// human writes a human task's whole body, given the numbers of the
	// issues of Blocks, in that order. It's nil for any other child.
	human func(blocks []int) (string, error)
}

// bodyOf is c's body, given numbers, the issue number of each child filed
// so far, by item: a human task's is written now, naming the issues of
// Blocks, each of which must have a number already (childOrder).
func (c childSpec) bodyOf(numbers map[string]int) (string, error) {
	if c.human == nil {
		return c.Body, nil
	}
	nums := make([]int, len(c.Blocks))
	for i, id := range c.Blocks {
		n, ok := numbers[id]
		if !ok {
			return "", fmt.Errorf("%s, which it blocks, has no issue yet", id)
		}
		nums[i] = n
	}
	return c.human(nums)
}

// childMaxBody is the most a child's body may be, by textLen: GitHub's
// limit, less textRoom, which keeps room for a dirty note.
const childMaxBody = maxText - textRoom

// childWidestNumber stands in for the number of each issue a human task
// blocks when childSpecs checks its body's size, before they're filed: the
// widest an issue number can be, as GitHub's GraphQL API gives it, a
// 32-bit Int.
const childWidestNumber = math.MaxInt32

// childSpecs describes the child of each of items, the items of revision
// rev of parent's approved plan, in the order they're created
// (childOrder): human tasks last. A story is an item whose ID is S<n>, and
// a task one whose ID is T<n>, as the planner's schema gives them; a task
// is human when its item says so. issueTypes is set on a repo that uses
// issue types, notify is config's notify, for a human task's cc line, and
// models is models.allowed as of now, which an item's model is resolved
// against (childOverrides).
//
//   - A story's title starts with its ID, "S2. Title", unless the planner
//     already wrote it so. A task's is as planned.
//   - A story's body is its Parent and Priority line, Outcome, Acceptance
//     criteria and "Depends on", as #249's is. A task's follows SPEC §11.2
//     and a human task's §9.3, then its cc line (humanBody), written only
//     once the issues it blocks are filed (bodyOf).
//   - Every body's last line is its origin marker, plan=#N rev=R item=ID,
//     with human=1 for a human task, which is how Derive knows one
//     (state.Origin.Human), and its "Done when" check, if it has one
//     (state.CheckFields, SPEC §5). It's the body's only marker:
//     planner text is escaped as a spec's is (SPEC §11.1), with headings
//     demoted, task list boxes stripped, mentions defused and markers
//     escaped, and an empty section reads "None.".
//   - The priority is the item's, else parent's priority:pN, else p2.
//   - An item's model and effort become its child's model:* and effort:*
//     labels, which set the runs on it, and a story's planner run (SPEC
//     §3.3). One left off, as a model a reload has dropped, is in leftOff,
//     and the child is still described.
//
// Of a revision's items (SPEC §11.3), only the new ones are described
// (planFiles): a kept, changed or dropped item has its child already, and
// a kept or dropped one carries its ID and title alone. A changed child is
// described by childSpecsWhere, for its rewrite (SPEC §6.4, #296), and a
// dropped one is closed by droppedClose (#297). A new human task's "Why it
// blocks" still names a changed item that depends on it, from all of items.
//
// An item it can't describe is an error naming it: a task with no scope
// or no body of its kind, a check the plan's validation would refuse, or a
// body over childMaxBody, which a human task's is checked for with the
// widest number for each issue it blocks. Nothing is described then.
func childSpecs(parent *github.Snapshot, rev int, items []planItem, issueTypes bool, notify []string, models config.Models) ([]childSpec, error) {
	return childSpecsWhere(parent, rev, items, planFiles, issueTypes, notify, models)
}

// childSpecsWhere is childSpecs for the items whose status want reports
// true, such as a revision's changed items, which are rewritten in place
// (SPEC §6.4): each is described as a new child of its kind would be, its
// origin naming rev, a human task's with human=1 and its check, so the
// rewrite renders every field as a new child's is rendered (SPEC §11.6).
func childSpecsWhere(parent *github.Snapshot, rev int, items []planItem, want func(status string) bool, issueTypes bool, notify []string,
	models config.Models) ([]childSpec, error) {
	ordered := childOrder(items)
	specs := make([]childSpec, 0, len(ordered))
	for _, it := range ordered {
		if !want(it.Status) {
			continue
		}
		c, err := childSpecOf(parent, rev, it, childDependents(items, it.ID), issueTypes, notify, models)
		if err != nil {
			return nil, fmt.Errorf("describe the child of plan #%d rev %d item %s: %w", parent.Number, rev, it.ID, err)
		}
		specs = append(specs, c)
	}
	return specs, nil
}

// childOrder is items in the order their children are created (SPEC §5.2,
// "Creating the children"): every item that isn't a human task, in plan
// order, then the human tasks, each after every human task that depends on
// it, and otherwise in
// plan order. So the issues a human task's "Why it blocks" names are filed
// before it. A cycle, which the plan's validation refuses, leaves the
// human tasks on it in plan order, last.
func childOrder(items []planItem) []planItem {
	ordered := make([]planItem, 0, len(items))
	var humans []planItem
	for _, it := range items {
		if it.Human {
			humans = append(humans, it)
		} else {
			ordered = append(ordered, it)
		}
	}
	for len(humans) > 0 {
		next := slices.IndexFunc(humans, func(h planItem) bool {
			return !slices.ContainsFunc(humans, func(d planItem) bool { return slices.Contains(d.DependsOn, h.ID) })
		})
		if next < 0 {
			return append(ordered, humans...)
		}
		ordered = append(ordered, humans[next])
		humans = slices.Delete(humans, next, next+1)
	}
	return ordered
}

// childDependents are the IDs of the items that depend on item id, in plan
// order.
func childDependents(items []planItem, id string) []string {
	var deps []string
	for _, it := range items {
		if slices.Contains(it.DependsOn, id) {
			deps = append(deps, it.ID)
		}
	}
	return deps
}

// childSpecOf is childSpecs for one item, with blocks, the items that
// depend on it.
func childSpecOf(parent *github.Snapshot, rev int, it planItem, blocks []string, issueTypes bool, notify []string, models config.Models) (childSpec, error) {
	story := strings.HasPrefix(it.ID, "S")
	pri := cmp.Or(it.Priority, "p"+strconv.Itoa(priority(parent)))
	c := childSpec{Item: it.ID, Title: childTitle(it, story)}
	var text, typ string
	var check state.Check
	switch {
	case story:
		typ, c.Stage = "story", state.Planning
		text = childStoryBody(it, parent.Number, pri)
	case it.Scope == "":
		return childSpec{}, errors.New("a task with no scope")
	case it.Human && it.HumanTask == nil:
		return childSpec{}, errors.New("a human task with no human task body")
	case it.Human:
		typ, c.Stage, c.Blocks = "task", state.NeedsHuman, blocks
		var err error
		if check, err = checkFromPlan(it.HumanTask.DoneWhen); err != nil {
			return childSpec{}, fmt.Errorf("its check: %w", err)
		}
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
		maps.Copy(origin.Fields, state.CheckFields(check))
	}
	marker, err := github.FormatMarker(origin)
	if err != nil {
		return childSpec{}, fmt.Errorf("origin marker: %w", err)
	}
	withMarker := func(text string) (string, error) {
		body := text + "\n\n" + marker
		if n := textLen(body); n > childMaxBody {
			return "", fmt.Errorf("its body is %d characters, over %d", n, childMaxBody)
		}
		return body, nil
	}
	if c.Stage == state.NeedsHuman {
		c.human = func(nums []int) (string, error) {
			text, err := humanBody(it, check, nums, parent.Number, pri, notify)
			if err != nil {
				return "", fmt.Errorf("write its human task body: %w", err)
			}
			return withMarker(text)
		}
		widest := make([]int, len(blocks))
		for i := range widest {
			widest[i] = childWidestNumber
		}
		if _, err := c.human(widest); err != nil {
			return childSpec{}, fmt.Errorf("check its body with the widest issue numbers: %w", err)
		}
	} else if c.Body, err = withMarker(text); err != nil {
		return childSpec{}, fmt.Errorf("write its body: %w", err)
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
	overrides, leftOff := childOverrides(it, models)
	c.Labels, c.leftOff = append(c.Labels, overrides...), leftOff
	return c, nil
}

// childOverrides are the override labels of it's child (SPEC §11.3,
// #126): its model as model:<suffix>, resolved against models, as /model
// resolves a full name or a label suffix (config.Models.Resolve, then
// LabelSuffix), and its effort as effort:<level>, in lower case. The
// plan's validation refused a model that didn't resolve and an effort
// config.ValidEffort refuses, but a reload since can drop a model. So one
// that's no good now isn't in labels, and leftOff says why, naming it.
func childOverrides(it planItem, models config.Models) (labels, leftOff []string) {
	if it.Model != "" {
		name, ok := models.Resolve(it.Model)
		suffix, labelled := models.LabelSuffix(name)
		if ok && labelled {
			labels = append(labels, github.ModelPrefix+suffix)
		} else {
			leftOff = append(leftOff, fmt.Sprintf("its model %q, which models.allowed doesn't have any more", it.Model))
		}
	}
	if it.Effort != "" {
		if level := strings.ToLower(it.Effort); config.ValidEffort(level) {
			labels = append(labels, github.EffortPrefix+level)
		} else {
			leftOff = append(leftOff, fmt.Sprintf("its effort %q, which isn't one of %s", it.Effort, strings.Join(config.EffortLevels(), ", ")))
		}
	}
	return labels, leftOff
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
