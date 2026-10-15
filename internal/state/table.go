package state

import (
	"errors"
	"fmt"
	"strings"
)

// Placeholders in a row's From or To. They never name an issue's stage, and
// Lookup never matches them as input.
const (
	// AnyOpen as From matches every open stage, held included.
	AnyOpen Stage = "any-open"
	// PreviousAt as To is the stage in the hold marker's at field (SPEC §6.4).
	PreviousAt Stage = "previous-at"
	// PreviousOrigin as To is the stage in the marker's origin field
	// (SPEC §6.4).
	PreviousOrigin Stage = "previous-origin"
)

// Trigger names what caused a transition. Each §7.2 row has its own.
type Trigger string

// The §7.2 triggers, one per row and in table order.
const (
	TriggerTrusted                Trigger = "trusted"
	TriggerMoreQuestions          Trigger = "more-questions"
	TriggerSpecWritten            Trigger = "spec-written"
	TriggerTaskWritten            Trigger = "task-written"
	TriggerAnswered               Trigger = "answered"
	TriggerSpecRevise             Trigger = "spec-revise"
	TriggerSpecApproved           Trigger = "spec-approved"
	TriggerPlanValid              Trigger = "plan-valid"
	TriggerPlanNoReview           Trigger = "plan-no-review"
	TriggerPlanRevise             Trigger = "plan-revise"
	TriggerPlanApproved           Trigger = "plan-approved"
	TriggerParentChangedTask      Trigger = "parent-changed-task"
	TriggerParentChangedHumanTask Trigger = "parent-changed-human-task"
	TriggerParentChangedStory     Trigger = "parent-changed-story"
	TriggerDropped                Trigger = "dropped"
	TriggerStoryVerified          Trigger = "story-verified"
	TriggerChildrenClosed         Trigger = "children-closed"
	TriggerScheduled              Trigger = "scheduled"
	TriggerImplementerQuestion    Trigger = "implementer-question"
	TriggerImplemented            Trigger = "implemented"
	TriggerImplementerBlocked     Trigger = "implementer-blocked"
	TriggerFindings               Trigger = "findings"
	TriggerAttemptCap             Trigger = "attempt-cap"
	TriggerVerifierQuestion       Trigger = "verifier-question"
	TriggerVerified               Trigger = "verified"
	TriggerPersonPushed           Trigger = "person-pushed"
	TriggerReviewChanges          Trigger = "review-changes"
	TriggerNewHead                Trigger = "new-head"
	TriggerMainMoved              Trigger = "main-moved"
	TriggerMerged                 Trigger = "merged"
	TriggerPRClosed               Trigger = "pr-closed"
	TriggerHandoffDone            Trigger = "handoff-done"
	TriggerHumanTaskDone          Trigger = "human-task-done"
	TriggerHold                   Trigger = "hold"
	TriggerDiscussionHold         Trigger = "discussion-hold"
	TriggerDiscussionResolved     Trigger = "discussion-resolved"
	TriggerDiscussionNoChange     Trigger = "discussion-no-change"
	TriggerSpecRewritten          Trigger = "spec-rewritten"
	TriggerPlanChanged            Trigger = "plan-changed"
	TriggerTaskRewritten          Trigger = "task-rewritten"
	TriggerHumanTaskRewritten     Trigger = "human-task-rewritten"
	TriggerResume                 Trigger = "resume"
	TriggerResumeAfterDiscussion  Trigger = "resume-after-discussion"
	TriggerClosed                 Trigger = "closed"
	TriggerReopened               Trigger = "reopened"
)

// Row is one row of the §7.2 transition table. Its fields are unexported, so
// the only rows that exist are the table's, reached through Lookup.
type Row struct {
	from, to Stage
	// classes are the classes the row applies to. They follow the "Applies
	// to" column, narrowed where the trigger names a narrower class (a human
	// task, a story with a parent).
	classes Class
	trigger Trigger
}

// From is the row's from stage: a stage, or AnyOpen.
func (r Row) From() Stage { return r.from }

// To is the row's to stage: a stage, PreviousAt or PreviousOrigin.
func (r Row) To() Stage { return r.to }

// Classes are the classes the row applies to.
func (r Row) Classes() Class { return r.classes }

// Trigger is the row's trigger.
func (r Row) Trigger() Trigger { return r.trigger }

// ErrNoTransition means the table has no row for a transition (SPEC §7.2).
var ErrNoTransition = errors.New("no such transition")

// epicOrOwnerStory is "epic, or story with no parent".
const epicOrOwnerStory = ClassEpic | ClassOwnerStory

// table is every §7.2 row, phases 2 and 3 included, in SPEC order.
var table = []Row{
	{Intake, Clarifying, All, TriggerTrusted},
	{Clarifying, Clarifying, All, TriggerMoreQuestions},
	{Clarifying, SpecReview, epicOrOwnerStory, TriggerSpecWritten},
	{Clarifying, Ready, NewBugOrOwnerTask, TriggerTaskWritten},
	{Clarifying, Ready, AnyTask, TriggerAnswered},
	{SpecReview, Clarifying, epicOrOwnerStory, TriggerSpecRevise},
	{SpecReview, Planning, epicOrOwnerStory, TriggerSpecApproved},
	{Planning, PlanReview, ClassEpic | AnyStory, TriggerPlanValid},
	{Planning, Planned, AnyStory, TriggerPlanNoReview},
	{PlanReview, Planning, ClassEpic | AnyStory, TriggerPlanRevise},
	{PlanReview, Planned, ClassEpic | AnyStory, TriggerPlanApproved},
	{AnyOpen, Ready, AnyTask, TriggerParentChangedTask},
	{AnyOpen, NeedsHuman, ClassHumanTask, TriggerParentChangedHumanTask},
	{AnyOpen, Planning, ClassStory, TriggerParentChangedStory},
	{AnyOpen, Held, All, TriggerDropped},
	{Planned, Closed, AnyStory | ClassEpic, TriggerStoryVerified},
	{Planned, Closed, ClassEpic, TriggerChildrenClosed},
	{Ready, InProgress, AnyTask, TriggerScheduled},
	{InProgress, Clarifying, AnyTask, TriggerImplementerQuestion},
	{InProgress, Verifying, AnyTask, TriggerImplemented},
	{InProgress, NeedsHuman, AnyTask, TriggerImplementerBlocked},
	{Verifying, InProgress, AnyTask, TriggerFindings},
	{Verifying, NeedsHuman, AnyTask, TriggerAttemptCap},
	{Verifying, Clarifying, AnyTask, TriggerVerifierQuestion},
	{Verifying, PROpen, AnyTask, TriggerVerified},
	{Verifying, PROpen, AnyTask, TriggerPersonPushed},
	{PROpen, InProgress, AnyTask, TriggerReviewChanges},
	{PROpen, Verifying, AnyTask, TriggerNewHead},
	{PROpen, PROpen, AnyTask, TriggerMainMoved},
	{PROpen, Closed, AnyTask, TriggerMerged},
	{PROpen, NeedsHuman, AnyTask, TriggerPRClosed},
	{NeedsHuman, Ready, AnyTask, TriggerHandoffDone},
	{NeedsHuman, Closed, ClassHumanTask, TriggerHumanTaskDone},
	{AnyOpen, Held, All, TriggerHold},
	{AnyOpen, Held, All, TriggerDiscussionHold},
	{Held, Clarifying, All, TriggerDiscussionResolved},
	{Clarifying, PreviousOrigin, All, TriggerDiscussionNoChange},
	{Clarifying, SpecReview, epicOrOwnerStory, TriggerSpecRewritten},
	{Clarifying, Planning, ClassEpic | AnyStory, TriggerPlanChanged},
	{Clarifying, Ready, AnyTask | NewBugOrOwnerTask, TriggerTaskRewritten},
	{Clarifying, NeedsHuman, ClassHumanTask, TriggerHumanTaskRewritten},
	{Held, PreviousAt, All, TriggerResume},
	{Held, Clarifying, All, TriggerResumeAfterDiscussion},
	{AnyOpen, Closed, All, TriggerClosed},
	{Closed, Held, All, TriggerReopened},
}

// Lookup returns the row for moving an issue of class from one stage to
// another on trigger. from and to are actual stages: a row from AnyOpen
// matches any open stage, and a row to PreviousAt or PreviousOrigin matches
// any open stage but held. The caller passes the stage it read from the
// marker; Lookup can't check it. class must be a single class, as ClassOf
// returns. A missing row is an error wrapping ErrNoTransition.
func Lookup(from, to Stage, class Class, trigger Trigger) (Row, error) {
	if class == 0 || class&(class-1) != 0 || class&^All != 0 {
		return Row{}, fmt.Errorf("look up %s → %s on %s: class %s is not a single class", from, to, trigger, class)
	}
	for _, r := range table {
		if r.trigger == trigger && r.classes&class != 0 && r.matchFrom(from) && r.matchTo(to, class) {
			return r, nil
		}
	}
	return Row{}, fmt.Errorf("look up %s → %s for %s on %s: %w", from, to, class, trigger, ErrNoTransition)
}

func (r Row) matchFrom(s Stage) bool {
	if r.from == AnyOpen {
		return s.Open()
	}
	return r.from == s
}

// matchTo also keeps an untyped issue from leaving clarifying through a
// previous stage, since it can't have been past clarifying (SPEC §4).
func (r Row) matchTo(s Stage, class Class) bool {
	if r.to == PreviousAt || r.to == PreviousOrigin {
		if class == ClassUntyped {
			return s == Intake || s == Clarifying
		}
		return s.Open() && s != Held
	}
	return r.to == s
}

// Fits reports whether an issue like it can be at stage s (SPEC §4, plan
// #7, assumption 9). If not, a type change left the issue where it doesn't
// belong, and that is a kind=type state error (SPEC §5.3).
//   - An unknown type, which ClassOf rejects, fits only intake: the
//     pipeline never takes it in.
//   - Intake, clarifying and held fit every other type, untyped included:
//     any issue can be at them, and the interviewer types it at clarifying.
//   - Any other stage, closed included, fits the issue's class when a §7.2
//     row for that class has the stage as its from or to. The rows from
//     "any open" don't count, as their from would let their classes be
//     at every stage. Their to doesn't count either, and needn't: other rows
//     have each of their classes at that stage.
//
// Anything that isn't a §7.1 stage or closed, such as AnyOpen, fits nothing.
func Fits(s Stage, it Item) bool {
	if !s.Open() && s != Closed {
		return false
	}
	c, err := ClassOf(it)
	if err != nil {
		// ClassOf's only error is an unknown type.
		return s == Intake
	}
	switch s {
	case Intake, Clarifying, Held:
		return true
	}
	for _, r := range table {
		if r.from != AnyOpen && r.classes&c != 0 && (r.from == s || r.to == s) {
			return true
		}
	}
	return false
}

var classNames = []string{"epic", "story", "owner-story", "task", "owner-task", "human-task", "bug", "untyped", "new-bug", "new-owner-task"}

// String names the classes in c, joined by "|".
func (c Class) String() string {
	var names []string
	for i, n := range classNames {
		if c&(1<<i) != 0 {
			names = append(names, n)
		}
	}
	if c&^All != 0 || len(names) == 0 {
		names = append(names, fmt.Sprintf("%#x", uint(c&^All)))
	}
	return strings.Join(names, "|")
}
