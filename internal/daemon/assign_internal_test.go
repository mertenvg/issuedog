package daemon

import (
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// What counts as a pipeline issue (assignPipeline): an item, a stage
// label, or an App transition marker in the body or a comment. Another's
// marker, or the App's of another kind, doesn't count, nor does a label
// that only looks like a stage.
func TestAssignPipeline(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	const move = "<!-- issuedog:transition from=intake to=ready run=r1 -->"
	for _, tc := range []struct {
		name  string
		s     github.Snapshot
		items bool
		want  bool
	}{
		{"nothing", github.Snapshot{Author: owner, Labels: []string{"triage:left"}}, false, false},
		{"an item", github.Snapshot{Author: owner}, true, true},
		{"a stage label", github.Snapshot{Author: owner, Labels: []string{"priority:p2", "stage:held"}}, false, true},
		{"not a stage", github.Snapshot{Author: owner, Labels: []string{"stage:nowhere"}}, false, false},
		{"the App's move in a comment", github.Snapshot{Author: owner, Comments: []github.Comment{{Author: owner, Body: "hi"}, {Author: app, Body: move}}}, false, true},
		{"the App's move in its body", github.Snapshot{Author: app, Body: move}, false, true},
		{"another's move", github.Snapshot{Author: owner, Body: move, Comments: []github.Comment{{Author: owner, Body: move}}}, false, false},
		{"the App's triage marker", github.Snapshot{Author: owner, Comments: []github.Comment{{Author: app, Body: "<!-- issuedog:triage verdict=leave run=t1 -->"}}}, false, false},
	} {
		if got := assignPipeline(&tc.s, "issuedog", tc.items); got != tc.want {
			t.Errorf("%s: assignPipeline = %t; want %t", tc.name, got, tc.want)
		}
	}
}

// assignNotify gives notify without an empty login or a repeat in any
// case, and forgets the dropped logins only when notify changes.
func TestAssignNotify(t *testing.T) {
	t.Parallel()
	d := &Daemon{Config: &config.Config{Notify: []string{"Owner", "", "bob", "owner"}}}
	if got := d.assignNotify(); !slices.Equal(got, []string{"Owner", "bob"}) {
		t.Errorf("assignNotify = %q; want Owner and bob", got)
	}
	d.assignDropped = map[string]bool{"bob": true}
	d.assignNotify()
	if !d.assignDropped["bob"] {
		t.Errorf("the same notify forgot the dropped logins, %v; want bob kept", d.assignDropped)
	}
	d.Config = &config.Config{Notify: []string{"bob"}}
	d.assignNotify()
	if d.assignDropped != nil {
		t.Errorf("another notify kept the dropped logins, %v; want none", d.assignDropped)
	}
	d.Config = nil
	if got := d.assignNotify(); got != nil {
		t.Errorf("assignNotify with no config = %q; want none", got)
	}
}

// assignMinus and assignSame compare logins in any case.
func TestAssignCompare(t *testing.T) {
	t.Parallel()
	if got := assignMinus([]string{"Amy", "bob", "Cy"}, []string{"BOB"}); !slices.Equal(got, []string{"Amy", "Cy"}) {
		t.Errorf("assignMinus = %q; want Amy and Cy", got)
	}
	if got := assignMinus([]string{"amy"}, []string{"AMY"}); got != nil {
		t.Errorf("assignMinus of the same = %q; want nil", got)
	}
	for _, tc := range []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{"Amy", "bob"}, []string{"BOB", "amy"}, true},
		{[]string{"amy"}, nil, false},
		{nil, []string{"amy"}, false},
		{[]string{"amy"}, []string{"amy", "bob"}, false},
	} {
		if got := assignSame(tc.a, tc.b); got != tc.want {
			t.Errorf("assignSame(%q, %q) = %t; want %t", tc.a, tc.b, got, tc.want)
		}
	}
}

// The snapshot takes each write's answer, GitHub's assignees after it: a
// login GitHub left out of the add isn't there, and a removal leaves
// nobody, so a pass that GitHub's updated_at doesn't make read the issue
// again still compares against what GitHub has.
func TestAssignTakesAnswer(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	item := f.AddIssue("owner", "Waits", "")
	f.SetAssignees(item, "bob")
	done := f.AddIssue("owner", "Ready", "", "stage:ready")
	f.SetAssignees(done, "bob")
	f.Unassignable("ghost")
	c := f.Client(t)
	d := &Daemon{Step: &Step{Applier: &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}},
		Config: &config.Config{Notify: []string{"owner", "Ghost"}}, Assign: true,
		digestCache: map[int][]waitItem{item: {{Issue: item, Kind: waitFlag}}},
		snaps:       map[int]*github.Snapshot{}, inputs: map[int][]github.Comment{}}
	for _, n := range []int{item, done} {
		s, err := c.Snapshot(t.Context(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		d.snaps[n], d.inputs[n] = s, nil
	}
	if err := d.assignPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := d.snaps[item].Assignees; !slices.Equal(got, []string{"owner"}) {
		t.Errorf("the snapshot's assignees with an item = %q; want owner alone, as GitHub answered", got)
	}
	if got := d.snaps[done].Assignees; got != nil {
		t.Errorf("the snapshot's assignees without one = %q; want none, as GitHub answered", got)
	}
	if !d.assignDropped["ghost"] || len(d.assignDropped) != 1 {
		t.Errorf("the dropped logins = %v; want ghost alone, in lower case", d.assignDropped)
	}
}

// A failed assignee write that isn't the issue gone leaves the issue's
// inputs for Work and marks it to be read again, not as gone, on a daemon
// with no issue marked to be read again before (#238).
func TestAssignFailKeepsInputs(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Waits", "")
	c := f.Client(t)
	d := &Daemon{Step: &Step{Applier: &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}},
		Config: &config.Config{Notify: []string{"owner"}}, Assign: true,
		digestCache: map[int][]waitItem{n: {{Issue: n, Kind: waitFlag}}},
		snaps:       map[int]*github.Snapshot{}, inputs: map[int][]github.Comment{}}
	s, err := c.Snapshot(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	d.snaps[n], d.inputs[n] = s, []github.Comment{{Body: "an input"}}
	f.FailWrite(1)
	if err := d.assignPass(t.Context()); err == nil {
		t.Errorf("assignPass with a failed write = nil; want its error")
	}
	if in, ok := d.inputs[n]; !ok || len(in) != 1 || !d.stale[n] || d.missing[n] {
		t.Errorf("after the failed write: inputs %v (held %t), stale %t, missing %t; want the input kept, and read again, not as gone",
			in, ok, d.stale[n], d.missing[n])
	}
}

// What a run's result leaves on the issue it applied to decides whether
// the unassign put off until after Work is made (assignApplied, SPEC
// §9.6): a question round with no answer since calls it off, and so it
// does unless the result was re-applied, when step 1's Inputs, the
// answers since the round, count, as apply checks for none then. A
// failure to find what waits on the issue, as an epic at planned whose
// plan data doesn't read back gives, leaves the unassign to be made, for
// the next pass to judge again.
func TestAssignAppliedUnassign(t *testing.T) {
	t.Parallel()
	answer := []github.Comment{{ID: 199, Author: waitOwner, Body: "1a 2a"}}
	round := func(*testing.T) *github.Snapshot {
		return waitSnap(1, []string{"stage:clarifying"}, waitRoundMarker(1, 2))
	}
	badData := func(t *testing.T) *github.Snapshot {
		s := doneSnap(t, 1, "epic", "story-plan", 12)
		doneBadData(t, s)
		return s
	}
	for _, tc := range []struct {
		name     string
		applied  func(*testing.T) *github.Snapshot
		moved    bool
		inputs   []github.Comment
		unassign bool
	}{
		{"a new round", round, false, nil, false},
		{"a new round, after the answer to the one before", round, false, answer, false},
		{"a re-applied round", round, true, nil, false},
		{"a re-applied round answered since", round, true, answer, true},
		{"plan data that doesn't read back", badData, false, nil, true},
	} {
		f := githubtest.New(t, "o/r", waitSlug)
		n := f.AddIssue("owner", "Waits", "")
		f.SetAssignees(n, "owner")
		c := f.Client(t)
		s, err := c.Snapshot(t.Context(), "o", "r", n)
		if err != nil {
			t.Fatal(err)
		}
		d := &Daemon{Step: &Step{Applier: &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: waitSlug}},
			snaps: map[int]*github.Snapshot{n: s}, inputs: map[int][]github.Comment{n: tc.inputs},
			assignLater: map[int][]string{n: {"owner"}}}
		d.assignApplied(tc.applied(t), Dirt{}, tc.moved)
		if err := d.assignLaterPass(t.Context()); err != nil {
			t.Fatalf("%s: assignLaterPass: %v", tc.name, err)
		}
		want := []string{"owner"}
		if tc.unassign {
			want = nil
		}
		if got := f.State().Issues[0].Assignees; !slices.Equal(got, want) {
			t.Errorf("%s: assignees %q; want %q", tc.name, got, want)
		}
	}
}
