package state

import "testing"

func TestLabels(t *testing.T) {
	for _, s := range openStages {
		l, ok := s.Label()
		if !ok {
			t.Errorf("%s has no label", s)
			continue
		}
		if got, ok := ParseLabel(l); !ok || got != s {
			t.Errorf("ParseLabel(%q) = %q, %v; want %q", l, got, ok, s)
		}
	}
	if got, ok := ParseLabel("Stage:In-Progress"); !ok || got != InProgress {
		t.Errorf("ParseLabel ignoring case = %q, %v", got, ok)
	}
	for _, l := range []string{"stage:closed", "stage:any-open", "type:bug", "stage", ""} {
		if got, ok := ParseLabel(l); ok {
			t.Errorf("ParseLabel(%q) = %q, want none", l, got)
		}
	}
	for _, s := range []Stage{Closed, AnyOpen, PreviousAt, PreviousOrigin} {
		if l, ok := s.Label(); ok {
			t.Errorf("%s has label %q, want none", s, l)
		}
	}
}

func TestClassOf(t *testing.T) {
	for _, c := range []struct {
		it   Item
		want Class
	}{
		{Item{}, ClassUntyped},
		{Item{Type: "Epic"}, ClassEpic},
		{Item{Type: "story", HasParent: true, FromPlan: true}, ClassStory},
		{Item{Type: "story"}, ClassOwnerStory},
		{Item{Type: "task", HasParent: true, FromPlan: true}, ClassTask},
		{Item{Type: "task", HasParent: true, FromPlan: true, Human: true}, ClassHumanTask},
		{Item{Type: "task"}, ClassOwnerTask},
		{Item{Type: "task", Human: true}, ClassOwnerTask},
		{Item{Type: "bug", HasParent: true, FromPlan: true}, ClassBug},
		{Item{Type: "bug"}, ClassBug},
	} {
		got, err := ClassOf(c.it)
		if err != nil || got != c.want {
			t.Errorf("ClassOf(%+v) = %s, %v; want %s", c.it, got, err, c.want)
		}
	}
	if _, err := ClassOf(Item{Type: "feature"}); err == nil {
		t.Error("ClassOf(feature): want an error")
	}
}
