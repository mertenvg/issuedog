package state

import (
	"errors"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
)

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
		{Item{Type: "task", ReachedReady: true}, ClassOwnerTask},
		{Item{Type: "task", Human: true, ReachedReady: true}, ClassOwnerTask},
		{Item{Type: "task"}, ClassNewOwnerTask},
		{Item{Type: "bug", HasParent: true, FromPlan: true, ReachedReady: true}, ClassBug},
		{Item{Type: "bug", ReachedReady: true}, ClassBug},
		{Item{Type: "bug"}, ClassNewBug},
		// A follow-up is a task the daemon filed, as a plan's task is, at
		// intake and after ready, with a parent or not, and never a human
		// task (SPEC §7.2, §10.2).
		{Item{Type: "task", FollowUp: true}, ClassTask},
		{Item{Type: "Task", FollowUp: true, ReachedReady: true}, ClassTask},
		{Item{Type: "task", FollowUp: true, HasParent: true}, ClassTask},
		{Item{Type: "task", FollowUp: true, Human: true}, ClassTask},
	} {
		got, err := ClassOf(c.it)
		if err != nil || got != c.want {
			t.Errorf("ClassOf(%+v) = %s, %v; want %s", c.it, got, err, c.want)
		}
	}
	if _, err := ClassOf(Item{Type: "feature"}); !errors.Is(err, ErrUnknownType) {
		t.Errorf("ClassOf(feature) = %v, want ErrUnknownType", err)
	}
}

// A follow-up the App filed is a task from the daemon (SPEC §7.2), by its
// origin marker as Derive reads it: at intake, with no move, and after its
// approval's move to ready. The same body in a person's issue is a task the
// owner filed, whose class is its own before ready.
func TestFollowUpClass(t *testing.T) {
	t.Parallel()
	const body = "Refuse it.\n\n<!-- issuedog:origin verify=#736 run=run7 item=F1 -->"
	waiting := &github.Snapshot{Number: 9, Author: appBot, Type: "task", Body: body}
	approved := &github.Snapshot{Number: 9, Author: appBot, Type: "task", Body: body, Comments: []github.Comment{
		{ID: 1, Author: appBot, Body: "<!-- issuedog:transition from=intake to=ready cmd=r5 -->"}}}
	person := &github.Snapshot{Number: 9, Author: owner, Type: "task", Body: body}
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		stage Stage
		want  Class
	}{
		{"waiting at intake", waiting, Intake, ClassTask},
		{"approved, at ready", approved, Ready, ClassTask},
		{"a person's issue with the marker", person, Intake, ClassNewOwnerTask},
	} {
		is := Derive(tt.s, slug, clean)
		got, err := ClassOf(is.Item)
		if err != nil || got != tt.want || is.Stage != tt.stage {
			t.Errorf("%s: class %s, %v, stage %s; want %s at %s", tt.name, got, err, is.Stage, tt.want, tt.stage)
		}
	}
}
