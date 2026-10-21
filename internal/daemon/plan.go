package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// outcomePlan is the planner's outcome for a plan. Its other outcome is a
// question round, outcomeQuestions, as the interviewer's.
const outcomePlan = "plan"

// A plan's kinds: what its items are (SPEC §11.3).
const (
	planStories = "stories"
	planTasks   = "tasks"
)

// The kinds of a human task's "Done when" check (SPEC §11.3), one set of
// names everywhere: the planner's schema, the plan data, and the checks
// the daemon runs on a human task (SPEC §9.3).
const (
	planCheckLabel        = "label"        // a label exists
	planCheckIssueType    = "issue-type"   // an issue type exists
	planCheckFile         = "file"         // a file is on main, optionally containing some text
	planCheckRuleset      = "ruleset"      // a ruleset on main requires a check
	planCheckInstallation = "installation" // the App is installed on an owner
)

// planOutput is the planner's structured output, as
// internal/agent/schemas/planner.json gives it: a plan, or a question round
// in the interviewer's question shape.
type planOutput struct {
	Outcome   string              `json:"outcome"`
	Questions []interviewQuestion `json:"questions,omitempty"`
	Plan      *planBody           `json:"plan,omitempty"`
}

// planBody is a plan: what the plan comment renders (renderPlan), and the
// plan's data its plan-data marker keeps (planDataMarker). Its slices have
// no omitempty, so the data reads back exactly: an empty list stays empty,
// and a missing one stays nil.
type planBody struct {
	Kind        string     `json:"kind"` // planStories or planTasks
	Why         string     `json:"why"`
	Summary     string     `json:"summary"`
	Message     string     `json:"message,omitempty"`
	Assumptions []string   `json:"assumptions"`
	Order       string     `json:"order"`
	Items       []planItem `json:"items"`
}

// planItem is one story or task of a plan. Scope, Human and one of Task
// and HumanTask are a task's only. Priority, Model and Effort are "" when
// the item leaves them to the issue's and the defaults. Status is a
// revision's only (plan_revision.go): planKept, planChanged, planNew or
// planDropped, and a kept or dropped item has its ID and title alone, its
// other fields empty or nil, as its data keeps them. A first plan's items
// have none, so their data is as it was before revisions (omitempty).
type planItem struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Status    string         `json:"status,omitempty"`
	Outcome   string         `json:"outcome"`
	Criteria  []string       `json:"criteria"`
	DependsOn []string       `json:"depends_on"`
	Priority  string         `json:"priority,omitempty"`
	Model     string         `json:"model,omitempty"`
	Effort    string         `json:"effort,omitempty"`
	Scope     string         `json:"scope,omitempty"`
	Human     bool           `json:"human,omitempty"`
	Task      *planTaskBody  `json:"task,omitempty"`
	HumanTask *planHumanTask `json:"human_task,omitempty"`
}

// planTaskBody is a task's issue body (SPEC §11.2), which names its files.
// The plan comment never shows it.
type planTaskBody struct {
	Context          string   `json:"context"`
	Change           string   `json:"change"`
	DefinitionOfDone []string `json:"definition_of_done"`
	TestPlan         string   `json:"test_plan"`
}

// planHumanTask is a human task's issue body (SPEC §9.3). The plan comment
// shows only its "Done when" text, and its check's kind.
type planHumanTask struct {
	Goal          string       `json:"goal"`
	WhyBlocks     string       `json:"why_blocks"`
	Steps         []string     `json:"steps"`
	Links         []string     `json:"links"`
	EstimatedTime string       `json:"estimated_time"`
	DoneWhen      planDoneWhen `json:"done_when"`
}

// planDoneWhen is a human task's "Done when": text the owner can check,
// and the check the daemon runs first, if any.
type planDoneWhen struct {
	Text  string     `json:"text"`
	Check *planCheck `json:"check,omitempty"`
}

// planCheck is a "Done when" check: a kind (planCheckLabel and the rest)
// and exactly that kind's fields. Its values may name files, so the plan
// comment shows only its kind.
type planCheck struct {
	Kind      string `json:"kind"`
	Label     string `json:"label,omitempty"`      // label
	IssueType string `json:"issue_type,omitempty"` // issue-type
	Path      string `json:"path,omitempty"`       // file
	Contains  string `json:"contains,omitempty"`   // file, optional
	Check     string `json:"check,omitempty"`      // ruleset
	Owner     string `json:"owner,omitempty"`      // installation
}

// decodePlan decodes a planner run's output. A field the types don't have
// is an error.
func decodePlan(raw json.RawMessage) (planOutput, error) {
	var out planOutput
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return planOutput{}, fmt.Errorf("decode planner output: %w", err)
	}
	return out, nil
}
