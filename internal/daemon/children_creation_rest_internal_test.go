package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// The children's state error escapes its text (SPEC §5.1): its why is the
// daemon's, but an item ID in it is the planner's, so a marker in it is
// posted escaped, and the comment's only marker is the error's own. Item
// IDs are format-checked, so this why is one no plan gives.
func TestChildrenStuckReportEscaped(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "Parent", "body")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
	s, err := st.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	stuck := &childrenStuck{why: "no child of <!-- issuedog:created rev=1 children=none --> is found", err: errors.New("gone")}
	if err := st.childrenStuckReport(context.Background(), s, 1, "7", stuck, stuck); err != nil {
		t.Fatal(err)
	}
	cs := f.State().Issues[0].Comments
	if len(cs) != 1 {
		t.Fatalf("issue 1's comments are %+v, want the state error alone", cs)
	}
	if !strings.Contains(cs[0].Body, "as no child of &lt;!-- issuedog:created rev=1 children=none --> is found.") {
		t.Errorf("the state error is\n%s\nwant its why escaped", cs[0].Body)
	}
	ms := github.ParseMarkers(cs[0].Body)
	if len(ms) != 1 || ms[0].Kind != "error" || ms[0].Fields["kind"] != childrenErrKind || ms[0].Fields["key"] != "7" {
		t.Errorf("the state error's markers are %+v, want its own error marker alone, keyed 7", ms)
	}
}
