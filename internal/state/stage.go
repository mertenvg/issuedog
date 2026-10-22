// Package state is the pipeline's state logic, with no I/O: stages, issue
// classes and the transition table (SPEC §7).
package state

import (
	"errors"
	"fmt"
	"strings"
)

// Stage is a pipeline stage (SPEC §7.1), or Closed.
type Stage string

// The §7.1 stages, in pipeline order, then Closed.
const (
	Intake     Stage = "intake"
	Clarifying Stage = "clarifying"
	SpecReview Stage = "spec-review"
	Planning   Stage = "planning"
	PlanReview Stage = "plan-review"
	Planned    Stage = "planned"
	Ready      Stage = "ready"
	InProgress Stage = "in-progress"
	Verifying  Stage = "verifying"
	PROpen     Stage = "pr-open"
	NeedsHuman Stage = "needs-human"
	Held       Stage = "held"
	// Closed is a closed issue. It has no label of its own: a closed
	// issue's stage label is one of the closed stages below, once its close
	// is judged (ClosingStage).
	Closed Stage = "closed"
	// Completed and Discarded are the closed stages (SPEC §6.4, §7.1): the
	// label an accepted close sets, stage:completed for
	// an issue closed as completed, a merged pull request's close included,
	// and stage:discarded for one closed as not planned or as a duplicate.
	// They're valid only on a closed issue, so they're no §7.1 stage: Open
	// is false for them, ParseLabel doesn't take their labels, and no
	// transition marker names them. A closed issue with one, that the App
	// or a trusted user set, has had its close judged, so it isn't read
	// again (SPEC §6.4).
	Completed Stage = "completed"
	Discarded Stage = "discarded"
)

// openStages are the stages that have a stage:* label.
var openStages = []Stage{Intake, Clarifying, SpecReview, Planning, PlanReview, Planned, Ready, InProgress, Verifying, PROpen, NeedsHuman, Held}

const labelPrefix = "stage:"

// Open reports whether s is one of the §7.1 stages, which an open issue has.
func (s Stage) Open() bool {
	for _, o := range openStages {
		if s == o {
			return true
		}
	}
	return false
}

// Label returns the stage:* label for s: a §7.1 stage's, or a closed
// stage's (Closing). Closed and anything else have none.
func (s Stage) Label() (string, bool) {
	if !s.Open() && !s.Closing() {
		return "", false
	}
	return labelPrefix + string(s), true
}

// Closing reports whether s is a closed stage, Completed or Discarded,
// valid only on a closed issue (SPEC §7.1).
func (s Stage) Closing() bool { return s == Completed || s == Discarded }

// ParseClosingLabel returns the closed stage a stage:* label names,
// ignoring case, as ParseLabel does: stage:completed or stage:discarded.
// It reports false for any other label.
func ParseClosingLabel(label string) (Stage, bool) {
	if len(label) < len(labelPrefix) || !strings.EqualFold(label[:len(labelPrefix)], labelPrefix) {
		return "", false
	}
	s := Stage(strings.ToLower(label[len(labelPrefix):]))
	return s, s.Closing()
}

// ClosingStage is the closed stage for an accepted close: Discarded for a
// close as not planned or as a duplicate, by its state_reason, and
// Completed for any other, a merged pull request's, merged, whatever its
// state_reason, and one with none, as from before GitHub had state
// reasons, included.
func ClosingStage(stateReason string, merged bool) Stage {
	switch strings.ToLower(stateReason) {
	case "not_planned", "duplicate":
		if !merged {
			return Discarded
		}
	}
	return Completed
}

// ParseLabel returns the stage a stage:* label names. Like GitHub, it ignores
// case. It reports false for any other label.
func ParseLabel(label string) (Stage, bool) {
	if len(label) < len(labelPrefix) || !strings.EqualFold(label[:len(labelPrefix)], labelPrefix) {
		return "", false
	}
	s := Stage(strings.ToLower(label[len(labelPrefix):]))
	return s, s.Open()
}

// Class is a set of issue classes, the "Applies to" column of §7.2. ClassOf
// gives an issue exactly one class; a table row may cover several.
type Class uint

// Issue classes.
const (
	// ClassEpic is an epic.
	ClassEpic Class = 1 << iota
	// ClassStory is a story with a parent, from an epic plan.
	ClassStory
	// ClassOwnerStory is a story with no parent, filed by the owner.
	ClassOwnerStory
	// ClassTask is a task the App created from a plan.
	ClassTask
	// ClassOwnerTask is a task filed by the owner.
	ClassOwnerTask
	// ClassHumanTask is a task the planner marked human (SPEC §9.3).
	ClassHumanTask
	// ClassBug is a bug, from the owner or a story verifier.
	ClassBug
	// ClassUntyped is an issue with no type yet. It follows only the rows
	// for all, so it can't leave clarifying (SPEC §4).
	ClassUntyped
	// ClassNewBug is a bug that hasn't reached ready yet. Only from ready on
	// does a bug follow the rows for task (SPEC §7.2).
	ClassNewBug
	// ClassNewOwnerTask is a task filed by the owner that hasn't reached
	// ready yet.
	ClassNewOwnerTask
)

// Class sets for the "Applies to" column.
const (
	// AnyStory is "story": with or without a parent.
	AnyStory = ClassStory | ClassOwnerStory
	// AnyTask is "task" for work that gets a PR. Every such row belongs to a
	// task's life from ready on (planner tasks are created there), so it
	// includes ClassBug: "from ready on, a bug follows the rows for task".
	// A human task is listed only on the rows whose trigger names it.
	AnyTask = ClassTask | ClassOwnerTask | ClassBug
	// NewBugOrOwnerTask is "bug, or task filed by the owner" before ready.
	NewBugOrOwnerTask = ClassNewBug | ClassNewOwnerTask
	// All is every class.
	All = ClassEpic | AnyStory | AnyTask | ClassHumanTask | ClassUntyped | NewBugOrOwnerTask
)

// Item is what decides an issue's class.
type Item struct {
	// Type is "epic", "story", "task" or "bug", from the issue type or the
	// type:* label, in any case. Empty means untyped.
	Type string
	// HasParent is true when the issue is a sub-issue.
	HasParent bool
	// FromPlan is true when the issue has the App's origin marker naming a
	// plan item (SPEC §5.2): Derive sets it exactly when Issue.Origin has a
	// plan, read from the body or else from a clean App comment (#290).
	FromPlan bool
	// Human is true for a plan item the planner marked human (SPEC §11.3).
	// Derive sets it from the origin marker's human=1 (Origin.Human).
	Human bool
	// ReachedReady is true once the issue has been at ready: any counted
	// marker names ready or a later task stage (in-progress, verifying,
	// pr-open) in its from, to, at or origin field. Using every field means
	// one ignored marker (SPEC §5.1) can't send a task back to the pre-ready
	// class. A bug or owner task before that has its own class.
	ReachedReady bool
}

// ClassOf returns the class of it. An unknown type is an error wrapping
// ErrUnknownType.
func ClassOf(it Item) (Class, error) {
	switch strings.ToLower(it.Type) {
	case "":
		return ClassUntyped, nil
	case "epic":
		return ClassEpic, nil
	case "story":
		if it.HasParent {
			return ClassStory, nil
		}
		return ClassOwnerStory, nil
	case "task":
		switch {
		case it.FromPlan && it.Human:
			return ClassHumanTask, nil
		case it.FromPlan:
			return ClassTask, nil
		case !it.ReachedReady:
			return ClassNewOwnerTask, nil
		}
		return ClassOwnerTask, nil
	case "bug":
		if !it.ReachedReady {
			return ClassNewBug, nil
		}
		return ClassBug, nil
	}
	return 0, fmt.Errorf("class of type %q: %w", it.Type, ErrUnknownType)
}

// ErrUnknownType is wrapped by ClassOf's error for a type that isn't epic,
// story, task or bug (SPEC §4).
var ErrUnknownType = errors.New("unknown type")
