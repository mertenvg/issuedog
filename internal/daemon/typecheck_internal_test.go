package daemon

import (
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// typeErrSnap is openErrSnap with type typ, set by the type change event.
func typeErrSnap(typ string, event int64, body string, comments ...string) *github.Snapshot {
	s := openErrSnap(body, comments...)
	s.Type, s.TypeEvent = typ, event
	return s
}

// The type is a type error's cause where no issue of it can be (state.Fits),
// whatever its parent, origin or past, and an unknown type anywhere.
func TestTypeErrCause(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		stage state.Stage
		typ   string
		want  bool
	}{
		{state.SpecReview, "epic", false},
		{state.SpecReview, "story", false}, // an owner's story, though a sub-issue story doesn't fit
		{state.SpecReview, "task", true},
		{state.SpecReview, "bug", true},
		{state.SpecReview, "", true},
		{state.Planning, "story", false},
		{state.Planning, "task", true},
		{state.Ready, "task", false},
		{state.Ready, "bug", false},
		{state.Ready, "epic", true},
		{state.Ready, "story", true},
		{state.NeedsHuman, "task", false},
		{state.InProgress, "bug", false}, // only once it has reached ready
		{state.Clarifying, "task", false},
		{state.Clarifying, "", false},
		{state.Held, "bug", false},
		{state.Intake, "", false},
		{state.Intake, "feature", true},
		{state.Clarifying, "feature", true},
		{state.Held, "feature", true},
		{state.Ready, "Task", false}, // in any case, as ClassOf reads it
	} {
		if got := typeErrCause(tc.stage, tc.typ); got != tc.want {
			t.Errorf("typeErrCause(%s, %q) = %t, want %t", tc.stage, tc.typ, got, tc.want)
		}
	}
}

// waitFind lists an open type error as err-<n>-type-<key>, beside the
// approval at spec-review, while the type change that keys it stands and
// the type doesn't fit; a newer type change, or a type that fits, ends it.
func TestTypeErrWaitFind(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		typ   string
		event int64
		want  []string
	}{
		{"open", "task", 70, []string{"err-14-type-70", "rev-14-1"}},
		{"a newer type change", "task", 71, []string{"rev-14-1"}},
		{"a type that fits", "epic", 70, []string{"rev-14-1"}},
	} {
		s := waitSnap(14, nil, waitRevision(1, "The spec."), "Doesn't fit.\n\n<!-- issuedog:error kind=type key=70 -->")
		s.Type, s.TypeEvent = tc.typ, tc.event
		items, err := waitFind(waitIn(s, Dirt{}))
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, it := range items {
			keys = append(keys, it.Key)
		}
		if !slices.Equal(keys, tc.want) {
			t.Errorf("%s: keys = %v, want %v", tc.name, keys, tc.want)
		}
	}
}

// typeErrStands needs a type change that stands, which keys the error, as
// well as a cause.
func TestTypeErrStands(t *testing.T) {
	t.Parallel()
	task := state.Issue{Stage: state.SpecReview, Item: state.Item{Type: "task"}}
	for _, tc := range []struct {
		event int64
		is    state.Issue
		want  bool
	}{
		{70, task, true},
		{0, task, false},
		{70, state.Issue{Stage: state.SpecReview, Item: state.Item{Type: "epic"}}, false},
	} {
		if got := typeErrStands(&github.Snapshot{TypeEvent: tc.event}, tc.is); got != tc.want {
			t.Errorf("typeErrStands(event %d, %+v) = %t, want %t", tc.event, tc.is, got, tc.want)
		}
	}
}
