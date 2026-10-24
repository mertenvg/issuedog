package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// ChildrenVisit stops a revision whose kept or changed child isn't found
// before any write (childrenGone), and a dependency never names a dropped
// item (SPEC §11.3), so by the time a revision's writes come, every
// changed item and every dependency has a child. Should one not, as
// through a caller that skips childrenGone, the write that needs it is an
// error, as a first plan's dependency with no child is, not a skip that
// would let the record stand: nothing is written for it.
func TestChildrenNoChildIsAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	plan := github.Comment{ID: 9, Author: planTestApp, Body: "Plan.\n\n<!-- issuedog:transition from=planning to=plan-review rev=2 run=p2 -->"}
	key := state.Key{Field: "event", ID: "p10"}
	for _, tt := range []struct {
		name  string
		write func(*Step, map[string]childFound) error
		want  string
	}{
		{"a changed item's rewrite", func(st *Step, children map[string]childFound) error {
			return st.childrenChanged(ctx, &github.Snapshot{Number: 1}, 2, plan, key, []childSpec{{Item: "T2"}}, children, map[string]int{})
		}, "item T2: revision 2 changes it, but it has no child"},
		{"a new item's dependency", func(st *Step, children map[string]childFound) error {
			return st.childrenLink(ctx, []planItem{{ID: "T4", Status: planNew, DependsOn: []string{"T1"}}}, children)
		}, "item T4: it depends on T1, which has no child"},
		{"a changed item's links", func(st *Step, children map[string]childFound) error {
			return st.revisedLink(ctx, []planItem{{ID: "T2", Status: planChanged}}, children)
		}, "item T2: the revision changes it, but it has no child"},
		{"a changed item's dependency", func(st *Step, children map[string]childFound) error {
			return st.revisedLink(ctx, []planItem{{ID: "T3", Status: planChanged, DependsOn: []string{"T1"}}}, children)
		}, "item T3: it depends on T1, which has no child"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("owner", "The story", "")
			t4 := f.AddIssue("issuedog[bot]", "T4's child", "")
			t3 := f.AddIssue("issuedog[bot]", "T3's child", "")
			// The IDs aren't read: each write stops before it needs one.
			children := map[string]childFound{"T4": {Number: t4, ID: 1004, Linked: true}, "T3": {Number: t3, ID: 1003, Linked: true}}
			st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
			err := tt.write(st, children)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %v; want one saying %q", err, tt.want)
			}
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes %+v; want none", w)
			}
		})
	}
}
