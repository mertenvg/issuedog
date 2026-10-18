package daemon

import (
	"fmt"

	"github.com/mertenvg/issuedog/internal/github"
)

// PlanRunData is for the planner's whole run (planner_run_test.go), which
// is in package daemon_test: the plan readPlanData reads back from comment
// c, and the plan in output, a planner run's structured output, decoded as
// Check decodes it, so the two compare with reflect.DeepEqual. It fails if
// c has no plan data, or if output holds no plan.
func PlanRunData(c github.Comment, appSlug string, output []byte) (got, want any, err error) {
	p, ok, err := readPlanData(c, appSlug)
	if err != nil {
		return nil, nil, fmt.Errorf("plan data of comment %d: %w", c.ID, err)
	}
	if !ok {
		return nil, nil, fmt.Errorf("comment %d has no plan data", c.ID)
	}
	out, err := decodePlan(output)
	if err != nil {
		return nil, nil, fmt.Errorf("the plan the run returned: %w", err)
	}
	if out.Plan == nil {
		return nil, nil, fmt.Errorf("the run returned a %q outcome, not a plan", out.Outcome)
	}
	return p, *out.Plan, nil
}
