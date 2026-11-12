package daemon

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
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
