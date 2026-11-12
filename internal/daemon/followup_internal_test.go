package daemon

import (
	"context"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// followUpWaits finds the issues Iterate visits on every pass for a
// follow-up's 👍 (SPEC §8.1) from the snapshot alone: an open issue the App
// filed with stage:intake, in any case. A person's issue at intake, an App
// issue at any other stage or with no stage label, and a closed one aren't.
func TestFollowUpWaits(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	for _, tt := range []struct {
		name string
		s    github.Snapshot
		want bool
	}{
		{"the App's, at intake", github.Snapshot{Author: app, Labels: []string{"priority:p3", "stage:intake"}}, true},
		{"the App's, at intake in another case", github.Snapshot{Author: app, Labels: []string{"Stage:Intake"}}, true},
		{"a person's, at intake", github.Snapshot{Author: owner, Labels: []string{"stage:intake"}}, false},
		{"the App's, at ready", github.Snapshot{Author: app, Labels: []string{"stage:ready"}}, false},
		{"the App's, with no stage label", github.Snapshot{Author: app, Labels: []string{"priority:p3"}}, false},
		{"the App's, closed at intake", github.Snapshot{Author: app, Labels: []string{"stage:intake"}, Closed: true}, false},
	} {
		if got := followUpWaits(&tt.s, "issuedog"); got != tt.want {
			t.Errorf("%s: followUpWaits = %t, want %t", tt.name, got, tt.want)
		}
	}
}

// A follow-up filed under a story, whose listed body gives a follow-up's
// origin, is no plan item's child, so childOrigins reads nothing for it: a
// story's plan revision costs no snapshot for each follow-up of each
// verify of its tasks. A sub-issue the App filed whose listed body gives
// no origin is still read.
func TestChildOriginsSkipFollowUps(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "A story", "The plan.")
	fu := f.AddIssue("issuedog[bot]", "F1", "A rare scenario.\n\n<!-- issuedog:origin verify=#3 run=v1 item=F1 -->")
	f.SetParent(fu, 1)
	ctx := context.Background()
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
	subs, err := st.Client.SubIssues(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	from := len(f.Requests())
	got, err := st.childOrigins(ctx, subs)
	if err != nil {
		t.Fatal(err)
	}
	if reqs := f.Requests()[from:]; len(reqs) != 0 || len(got) != 0 {
		t.Errorf("childOrigins sent %+v and gave %+v; want no request, and no origin", reqs, got)
	}
	bare := f.AddIssue("issuedog[bot]", "T1", "No marker.")
	f.SetParent(bare, 1)
	if subs, err = st.Client.SubIssues(ctx, "o", "r", 1); err != nil {
		t.Fatal(err)
	}
	from = len(f.Requests())
	if _, err := st.childOrigins(ctx, subs); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(f.Requests()[from:], func(r githubtest.Request) bool { return r.Path == "/issues/3" }) {
		t.Errorf("childOrigins sent %+v; want #3's snapshot read", f.Requests()[from:])
	}
}
