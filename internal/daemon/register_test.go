package daemon_test

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/state"
)

// CheckRegistered rejects a Stage at intake while Unlabeled is set, whose
// leases record intake, and names both. Any other registration passes
// (#130's verify).
func TestRegisterCheck(t *testing.T) {
	t.Parallel()
	intake := &fakeStage{agent: "intaker"}
	for _, tt := range []struct {
		name      string
		stages    map[state.Stage]daemon.Stage
		unlabeled daemon.Stage
		fails     bool
	}{
		{name: "nothing registered"},
		{name: "a Stage at clarifying and Unlabeled",
			stages: map[state.Stage]daemon.Stage{state.Clarifying: &fakeStage{}}, unlabeled: &fakeStage{agent: "triage"}},
		{name: "a Stage at intake with no Unlabeled",
			stages: map[state.Stage]daemon.Stage{state.Intake: intake}},
		{name: "a Stage at intake and Unlabeled",
			stages:    map[state.Stage]daemon.Stage{state.Clarifying: &fakeStage{}, state.Intake: intake},
			unlabeled: &fakeStage{agent: "triage"}, fails: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := (&daemon.Runs{Stages: tt.stages, Unlabeled: tt.unlabeled}).CheckRegistered()
			want := ""
			if tt.fails {
				want = "a Stage (*daemon_test.fakeStage) is registered at intake while Unlabeled (*daemon_test.fakeStage) is set: " +
					"Unlabeled's leases record intake, so every result of the Stage at intake would be dropped"
			}
			if got := registerErrText(err); got != want {
				t.Errorf("CheckRegistered = %q, want %q", got, want)
			}
		})
	}
}

// registerErrText is err's text, or "" for nil.
func registerErrText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
