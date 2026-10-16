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
