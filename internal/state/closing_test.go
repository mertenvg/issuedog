package state

import "testing"

// The closed stages have labels, which ParseClosingLabel takes, ignoring
// case, and ParseLabel doesn't: they're no §7.1 stage, valid only on a
// closed issue (plan #87 rev 2, assumption 12).
func TestClosingStages(t *testing.T) {
	for s, want := range map[Stage]string{Completed: "stage:completed", Discarded: "stage:discarded"} {
		if l, ok := s.Label(); !ok || l != want {
			t.Errorf("%s.Label() = %q, %v; want %q", s, l, ok, want)
		}
		if !s.Closing() || s.Open() {
			t.Errorf("%s: Closing %v, Open %v; want a closed stage, not an open one", s, s.Closing(), s.Open())
		}
		if got, ok := ParseClosingLabel(want); !ok || got != s {
			t.Errorf("ParseClosingLabel(%q) = %q, %v; want %s", want, got, ok, s)
		}
		if got, ok := ParseLabel(want); ok {
			t.Errorf("ParseLabel(%q) = %q, true; want no open stage", want, got)
		}
	}
	if got, ok := ParseClosingLabel("Stage:Discarded"); !ok || got != Discarded {
		t.Errorf("ParseClosingLabel ignoring case = %q, %v", got, ok)
	}
	for _, l := range []string{"stage:ready", "stage:closed", "completed", "stage:", "stag", "", "phase:completed"} {
		if got, ok := ParseClosingLabel(l); ok {
			t.Errorf("ParseClosingLabel(%q) = %q, true; want false", l, got)
		}
	}
	for _, s := range append(append([]Stage{}, openStages...), Closed, AnyOpen) {
		if s.Closing() {
			t.Errorf("%s.Closing() = true; want false", s)
		}
	}
	if l, ok := Closed.Label(); ok {
		t.Errorf("Closed.Label() = %q, true; want none", l)
	}
}

// An accepted close is discarded when closed as not planned or as a
// duplicate, and completed otherwise, a merged pull request's always.
func TestClosingStage(t *testing.T) {
	for _, tc := range []struct {
		reason string
		merged bool
		want   Stage
	}{
		{"completed", false, Completed},
		{"", false, Completed},
		{"not_planned", false, Discarded},
		{"NOT_PLANNED", false, Discarded},
		{"duplicate", false, Discarded},
		{"not_planned", true, Completed},
		{"duplicate", true, Completed},
		{"reopened", false, Completed},
	} {
		if got := ClosingStage(tc.reason, tc.merged); got != tc.want {
			t.Errorf("ClosingStage(%q, %v) = %s; want %s", tc.reason, tc.merged, got, tc.want)
		}
	}
}
