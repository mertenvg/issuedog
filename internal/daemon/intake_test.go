package daemon_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// intakePass runs step 1, then IntakeVisit, on issue 1 with "owner" trusted,
// from a fresh client, as Iterate does.
func intakePass(t testing.TB) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: trusted{"owner": true}}
		if _, err := st.Run(context.Background(), s); err != nil {
			return err
		}
		return st.IntakeVisit(context.Background(), s)
	}
}

// admitted is the comment that moves an issue from intake: why, then the
// marker keyed event=key.
func admitted(why, key string) string { return waits(why, "", key) }

// waits is admitted for an issue of type typ, which the comment tells it
// waits for phase 3 if it's a bug or a task.
func waits(why, typ, key string) string {
	text := why + " I've moved it to `clarifying`."
	if typ != "" {
		text += " It's a " + typ + ": bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3)."
	}
	return text + "\n\n<!-- issuedog:transition from=intake to=clarifying event=" + key + " -->"
}

const (
	byOwner    = "owner opened this issue, and it has `stage:intake`, so it's in the pipeline (SPEC §6.2)."
	byApproved = "owner added `approved`, so this issue is in the pipeline (SPEC §6.2)."
)

// issueBy files issue 1 by author with these labels, whose labeled events
// are 9001 on, then runs more, if set.
func issueBy(author string, labels []string, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.AddIssue(author, "Title", "body", labels...)
		if more != nil {
			more(t, f)
		}
		return f
	}
}

func overriddenTo(label, marker string) string {
	return "owner set `" + label + "` by hand, so I've recorded it as an owner override.\n\n<!-- issuedog:transition " + marker + " -->"
}

func TestIntakeVisit(t *testing.T) {
	intake := []string{"stage:intake"}
	clarifying := []string{"stage:clarifying"}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"the owner's issue with stage:intake moves, untyped, keyed by its labeled event",
			issueBy("owner", intake, nil),
			outcome{comments: []string{admitted(byOwner, "9001")}, labels: clarifying}},
		{"a task is told it waits for phase 3",
			issueBy("owner", []string{"stage:intake", "type:task"}, nil),
			outcome{comments: []string{waits(byOwner, "task", "9001")}, labels: []string{"stage:clarifying", "type:task"}}},
		{"a bug is told it waits for phase 3",
			issueBy("owner", []string{"stage:intake", "type:bug"}, nil),
			outcome{comments: []string{waits(byOwner, "bug", "9001")}, labels: []string{"stage:clarifying", "type:bug"}}},
		{"an epic gets no phase 3 note",
			issueBy("owner", []string{"stage:intake", "type:epic"}, nil),
			outcome{comments: []string{admitted(byOwner, "9001")}, labels: []string{"stage:clarifying", "type:epic"}}},
		{"an untrusted author's issue the owner labelled stage:intake waits for approved",
			issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) { labelEvent(t, f.AddLabel(1, "owner", "stage:intake"), 9001) }),
			outcome{labels: intake}},
		{"a trusted approved moves an untrusted author's issue, keyed by that event",
			issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9002)
			}),
			outcome{comments: []string{admitted(byApproved, "9002")}, labels: []string{"approved", "stage:clarifying"}}},
		{"an untrusted approved is reverted, and the issue stays",
			issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				labelEvent(t, f.AddLabel(1, "mallory", "approved"), 9002)
			}),
			outcome{comments: []string{"I removed `approved`: only trusted users can change pipeline labels, and mallory isn't one.\n\n" +
				"<!-- issuedog:revert key=9002 -->"}, labels: intake}},
		{"the App's restore of a trusted approved keys on the trusted add",
			issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9001)
				f.RemoveLabel(1, "mallory", "approved")
				f.AddComment(1, app, "<!-- issuedog:revert key=9002 -->")
				f.AddLabel(1, app, "approved")
			}),
			outcome{comments: []string{admitted(byApproved, "9001")}, labels: []string{"approved", "stage:clarifying"}}},
		{"a trusted approved moves an issue with no stage label",
			issueBy("owner", nil, func(t testing.TB, f *githubtest.Fake) { labelEvent(t, f.AddLabel(1, "owner", "approved"), 9001) }),
			outcome{comments: []string{admitted(byApproved, "9001")}, labels: []string{"approved", "stage:clarifying"}}},
		{"an issue with no stage label and no approved is left to triage",
			issueBy("owner", []string{"priority:p1"}, nil),
			outcome{labels: []string{"priority:p1"}}},
		{"an override made before the visit is recorded, and the issue doesn't move",
			issueBy("owner", intake, func(t testing.TB, f *githubtest.Fake) {
				f.RemoveLabel(1, "owner", "stage:intake")
				labelEvent(t, f.AddLabel(1, "owner", "stage:planning"), 9003)
			}),
			outcome{comments: []string{overriddenTo("stage:planning", "from=intake to=planning by=owner event=9003")},
				labels: []string{"stage:planning"}}},
		{"a hold set by hand at intake is recorded, and the held issue doesn't move, approved or not",
			issueBy("owner", []string{"stage:intake", "approved"}, func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9003)
			}),
			outcome{comments: []string{overriddenTo("stage:held", "from=intake to=held at=intake reason=owner by=owner event=9003")},
				labels: []string{"approved", "stage:held"}}},
		{"a held issue doesn't move",
			issueBy("owner", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=intake to=held at=intake reason=owner cmd=5 -->")
				f.AddLabel(1, app, "stage:held")
				f.AddLabel(1, "owner", "approved")
			}),
			outcome{labels: []string{"approved", "stage:held"}}},
		{"a hand-set stage past intake is adopted, and the issue doesn't move from intake",
			issueBy("owner", []string{"stage:clarifying", "approved"}, nil),
			outcome{comments: []string{overriddenTo("stage:clarifying", "from=intake to=clarifying by=owner event=9001")},
				labels: []string{"approved", "stage:clarifying"}}},
		{"an override back to intake moves again, keyed by the override's comment",
			issueBy("owner", intake, func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, admitted(byOwner, "9001"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:intake")
				labelEvent(t, f.AddLabel(1, "owner", "stage:intake"), 9004)
				f.RemoveLabel(1, "owner", "stage:clarifying")
			}),
			outcome{comments: []string{overriddenTo("stage:intake", "from=clarifying to=intake by=owner event=9004"),
				admitted(byOwner, "1002")}, labels: clarifying}},
		{"a type the pipeline doesn't know is reported once, and the issue stays",
			issueBy("owner", []string{"stage:intake", "type:frob"}, nil),
			outcome{comments: []string{"I can't move this issue from `intake` to `clarifying` on trusted: its type `frob` isn't one " +
				"the pipeline knows (SPEC §4). It stays at `intake`.\n\n<!-- issuedog:error kind=transition key=9001 -->"},
				labels: []string{"stage:intake", "type:frob"}}},
	} {
		t.Run(tt.name, func(t *testing.T) { check(t, tt.setup, intakePass(t), tt.want) })
	}
}

// An override made after the label rules ran, but before IntakeVisit read
// the timeline, holds the move until the next visit records it.
func TestIntakeWaitsForAnOverrideAfterTheLabelRules(t *testing.T) {
	f := issueBy("owner", []string{"stage:intake"}, nil)(t)
	timelines := 0
	f.BeforeRequest(func(r githubtest.Request) bool {
		if r.Method == "GET" && strings.HasSuffix(r.Path, "/issues/1/timeline") {
			timelines++
		}
		return timelines == 2 // IntakeVisit's, after CheckLabels'
	}, func() { f.AddLabel(1, "owner", "stage:planning") })
	pass := intakePass(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("the first pass wrote %+v, want nothing", w)
	}
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	want := []string{overriddenTo("stage:planning", "from=intake to=planning by=owner event=9002")}
	var got []string
	for _, c := range is.Comments {
		got = append(got, c.Body)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(is.Labels, []string{"stage:planning"}) {
		t.Errorf("comments %q, labels %v; want only the override, at stage:planning", got, is.Labels)
	}
}

// An override made after IntakeVisit read the timeline, but before its
// marker went out, is recorded by the next visit: the marker records the
// labeled event of stage:intake, and the override came after it.
func TestIntakeOverrideDuringTheMove(t *testing.T) {
	f := issueBy("owner", []string{"stage:intake"}, nil)(t)
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Method == "POST" && strings.HasSuffix(r.Path, "/issues/1/comments")
	}, func() { f.AddLabel(1, "owner", "stage:planning") })
	pass := intakePass(t)
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	if err := pass(f); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	want := []string{admitted(byOwner, "9001"), overriddenTo("stage:planning", "from=clarifying to=planning by=owner event=9002")}
	var got []string
	for _, c := range is.Comments {
		got = append(got, c.Body)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(is.Labels, []string{"stage:planning"}) {
		t.Errorf("comments %q, labels %v; want the move, then the override, at stage:planning", got, is.Labels)
	}
}

func TestIntakeVisitTrustError(t *testing.T) {
	f := issueBy("owner", []string{"stage:intake"}, nil)(t)
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: failTrust{}}
	if err := st.IntakeVisit(context.Background(), s); !errors.Is(err, errTrust) {
		t.Errorf("IntakeVisit = %v, want the trust error", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("wrote %+v, want nothing", w)
	}
}

// Iterate moves the owner's issue at intake, and leaves an untrusted
// author's and an unlabelled one.
func TestIterateMovesIntake(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Idea", "body", "stage:intake")
	f.AddIssue("mallory", "Idea", "body")
	f.AddLabel(2, "owner", "stage:intake")
	f.AddIssue("owner", "Unsorted", "body")
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	n := len(f.Writes())
	iterate(t, d)
	if w := f.Writes(); len(w) != n {
		t.Errorf("a second Iterate wrote %+v, want nothing", w[n:])
	}
	st := f.State()
	for i, want := range [][]string{{"stage:clarifying"}, {"stage:intake"}, nil} {
		if got := st.Issues[i].Labels; !reflect.DeepEqual(got, want) {
			t.Errorf("#%d labels = %v, want %v", i+1, got, want)
		}
	}
	if got := len(st.Issues[0].Comments); got != 1 {
		t.Errorf("#1 has %d comments, want the move's", got)
	}
}
