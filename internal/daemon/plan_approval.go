package daemon

import (
	"fmt"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// approvedPlanData returns the plan revision the latest move into planned
// approved (state.ApprovedPlan), with the plan's data read back from its
// comment (readPlanData), to create the children from (SPEC §5.2). It reports
// false, with no error, when ApprovedPlan finds none, or when the comment
// has no plan-data marker the App wrote. Data that doesn't read back is an
// error. dirty is as for state.Derive.
func approvedPlanData(s *github.Snapshot, appSlug string, dirty func(int64) bool) (state.Revision, planBody, bool, error) {
	rev, c, ok := state.ApprovedPlan(s, appSlug, dirty)
	if !ok {
		return state.Revision{}, planBody{}, false, nil
	}
	p, ok, err := readPlanData(c, appSlug)
	if err != nil {
		return state.Revision{}, planBody{}, false, fmt.Errorf("approved plan revision %d of #%d: %w", rev.Rev, s.Number, err)
	}
	if !ok {
		return state.Revision{}, planBody{}, false, nil
	}
	return rev, p, true, nil
}
