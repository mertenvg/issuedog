package daemon_test

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// intakePass runs step 1, then IntakeVisit, on issue 1 with "owner" trusted,
// from a fresh client, as Iterate does.
func intakePass(t testing.TB) func(*githubtest.Fake) error {
	return intakePassWith(t, trusted{"owner": true})
}

// intakePassWith is intakePass with tr.
func intakePassWith(t testing.TB, tr github.Trust) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: tr}
		if _, err := st.Run(context.Background(), s); err != nil {
			return err
		}
		return st.IntakeVisit(context.Background(), s)
	}
}

// admitted is the comment that moves an issue from intake: why, then the
// marker keyed event=key.
func admitted(why, key string) string { return waits(why, "", key) }

// waits is admitted with the note that tells an issue of type typ, a bug or
// a task, that it waits for phase 3.
func waits(why, typ, key string) string {
	note := ""
	if typ != "" {
		note = " It's a " + typ + ": bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3)."
	}
	return withNote(why, note, key)
}

// withNote is admitted with note after the move.
func withNote(why, note, key string) string {
	return why + " I've moved it to `clarifying`." + note + "\n\n<!-- issuedog:transition from=intake to=clarifying event=" + key + " -->"
}

const storyNote = " It's a story with a parent: those wait at `clarifying` for now, as issuedog only clarifies epics, stories with no parent and untyped issues."

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
		trust trusted // nil trusts owner
	}{
		{name: "the owner's issue with stage:intake moves, untyped, keyed by its labeled event",
			setup: issueBy("owner", intake, nil),
			want:  outcome{comments: []string{admitted(byOwner, "9001")}, labels: clarifying}},
		{name: "a closed issue doesn't move",
			setup: issueBy("owner", intake, func(_ testing.TB, f *githubtest.Fake) { f.Close(1) }),
			want:  outcome{labels: intake}},
		{name: "stage:intake on the owner's issue keys the move, not approved",
			setup: issueBy("owner", []string{"stage:intake", "approved"}, nil),
			want:  outcome{comments: []string{admitted(byOwner, "9001")}, labels: []string{"approved", "stage:clarifying"}}},
		{name: "the App's own stage:intake keys the move",
			setup: issueBy("owner", nil, func(t testing.TB, f *githubtest.Fake) { labelEvent(t, f.AddLabel(1, app, "stage:intake"), 9001) }),
			want:  outcome{comments: []string{admitted(byOwner, "9001")}, labels: clarifying}},
		{name: "a stage label in other case moves",
			setup: issueBy("owner", []string{"Stage:Intake"}, nil),
			want:  outcome{comments: []string{admitted(byOwner, "9001")}, labels: clarifying}},
		{name: "the trusted approved stays the key when mallory removes and adds it again",
			setup: issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9002)
				f.RemoveLabel(1, "mallory", "approved")
				f.AddLabel(1, "mallory", "approved")
			}),
			want: outcome{comments: []string{admitted(byApproved, "9002")}, labels: []string{"approved", "stage:clarifying"}}},
		{name: "the newest trusted approved is the key",
			setup: issueBy("owner", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "approved")
				f.RemoveLabel(1, "owner", "approved")
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9003)
			}),
			want: outcome{comments: []string{admitted(byApproved, "9003")}, labels: []string{"approved", "stage:clarifying"}}},
		{name: "a bot's issue doesn't move, whatever Trust says",
			setup: issueBy("ci[bot]", nil, func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "owner", "stage:intake") }),
			trust: trusted{"owner": true, "ci[bot]": true},
			want:  outcome{labels: intake}},
		{name: "an issue with no author login doesn't move, whatever Trust says",
			setup: issueBy("", nil, func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "owner", "stage:intake") }),
			trust: trusted{"owner": true, "": true},
			want:  outcome{labels: intake}},
		{name: "a story with a parent is told it waits",
			setup: func(t testing.TB) *githubtest.Fake {
				f := issueBy("owner", []string{"stage:intake", "type:story"}, nil)(t)
				f.SetParent(1, f.AddIssue("owner", "Epic", "body"))
				return f
			},
			want: outcome{comments: []string{withNote(byOwner, storyNote, "9001")}, labels: []string{"stage:clarifying", "type:story"}}},
		{name: "an untrusted type:task, reverted, gives no note in the same pass",
			setup: issueBy("owner", intake, func(t testing.TB, f *githubtest.Fake) { labelEvent(t, f.AddLabel(1, "mallory", "type:task"), 9002) }),
			want: outcome{comments: []string{"I removed `type:task`: only trusted users can change pipeline labels, and mallory isn't one.\n\n" +
				"<!-- issuedog:revert key=9002 -->", admitted(byOwner, "9001")}, labels: clarifying}},
		{name: "a story with no parent gets no note",
			setup: issueBy("owner", []string{"stage:intake", "type:story"}, nil),
			want:  outcome{comments: []string{admitted(byOwner, "9001")}, labels: []string{"stage:clarifying", "type:story"}}},
		{name: "a task is told it waits for phase 3",
			setup: issueBy("owner", []string{"stage:intake", "type:task"}, nil),
			want:  outcome{comments: []string{waits(byOwner, "task", "9001")}, labels: []string{"stage:clarifying", "type:task"}}},
		{name: "a bug is told it waits for phase 3",
			setup: issueBy("owner", []string{"stage:intake", "type:bug"}, nil),
			want:  outcome{comments: []string{waits(byOwner, "bug", "9001")}, labels: []string{"stage:clarifying", "type:bug"}}},
		{name: "an epic gets no phase 3 note",
			setup: issueBy("owner", []string{"stage:intake", "type:epic"}, nil),
			want:  outcome{comments: []string{admitted(byOwner, "9001")}, labels: []string{"stage:clarifying", "type:epic"}}},
		{name: "an untrusted author's issue the owner labelled stage:intake waits for approved",
			setup: issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) { labelEvent(t, f.AddLabel(1, "owner", "stage:intake"), 9001) }),
			want:  outcome{labels: intake}},
		{name: "a trusted approved moves an untrusted author's issue, keyed by that event",
			setup: issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9002)
			}),
			want: outcome{comments: []string{admitted(byApproved, "9002")}, labels: []string{"approved", "stage:clarifying"}}},
		{name: "an untrusted approved is reverted, and the issue stays",
			setup: issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "stage:intake")
				labelEvent(t, f.AddLabel(1, "mallory", "approved"), 9002)
			}),
			want: outcome{comments: []string{"I removed `approved`: only trusted users can change pipeline labels, and mallory isn't one.\n\n" +
				"<!-- issuedog:revert key=9002 -->"}, labels: intake}},
		{name: "the App's restore of a trusted approved keys on the trusted add",
			setup: issueBy("mallory", nil, func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "approved"), 9001)
				f.RemoveLabel(1, "mallory", "approved")
				f.AddComment(1, app, "<!-- issuedog:revert key=9002 -->")
				f.AddLabel(1, app, "approved")
			}),
			want: outcome{comments: []string{admitted(byApproved, "9001")}, labels: []string{"approved", "stage:clarifying"}}},
		{name: "a trusted approved moves an issue with no stage label",
			setup: issueBy("owner", nil, func(t testing.TB, f *githubtest.Fake) { labelEvent(t, f.AddLabel(1, "owner", "approved"), 9001) }),
			want:  outcome{comments: []string{admitted(byApproved, "9001")}, labels: []string{"approved", "stage:clarifying"}}},
		{name: "an issue with no stage label and no approved is left to triage",
			setup: issueBy("owner", []string{"priority:p1"}, nil),
			want:  outcome{labels: []string{"priority:p1"}}},
		{name: "an override made before the visit is recorded, and the issue doesn't move",
			setup: issueBy("owner", intake, func(t testing.TB, f *githubtest.Fake) {
				f.RemoveLabel(1, "owner", "stage:intake")
				labelEvent(t, f.AddLabel(1, "owner", "stage:planning"), 9003)
			}),
			want: outcome{comments: []string{overriddenTo("stage:planning", "from=intake to=planning by=owner event=9003")},
				labels: []string{"stage:planning"}}},
		{name: "a hold set by hand at intake is recorded, and the held issue doesn't move, approved or not",
			setup: issueBy("owner", []string{"stage:intake", "approved"}, func(t testing.TB, f *githubtest.Fake) {
				labelEvent(t, f.AddLabel(1, "owner", "stage:held"), 9003)
			}),
			want: outcome{comments: []string{overriddenTo("stage:held", "from=intake to=held at=intake reason=owner by=owner event=9003")},
				labels: []string{"approved", "stage:held"}}},
		{name: "a held issue doesn't move",
			setup: issueBy("owner", nil, func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=intake to=held at=intake reason=owner cmd=5 -->")
				f.AddLabel(1, app, "stage:held")
				f.AddLabel(1, "owner", "approved")
			}),
			want: outcome{labels: []string{"approved", "stage:held"}}},
		{name: "a hand-set stage past intake is adopted, and the issue doesn't move from intake",
			setup: issueBy("owner", []string{"stage:clarifying", "approved"}, nil),
			want: outcome{comments: []string{overriddenTo("stage:clarifying", "from=intake to=clarifying by=owner event=9001")},
				labels: []string{"approved", "stage:clarifying"}}},
		{name: "an override back to intake moves again, keyed by the override's comment",
			setup: issueBy("owner", intake, func(t testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, admitted(byOwner, "9001"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:intake")
				labelEvent(t, f.AddLabel(1, "owner", "stage:intake"), 9004)
				f.RemoveLabel(1, "owner", "stage:clarifying")
			}),
			want: outcome{comments: []string{overriddenTo("stage:intake", "from=clarifying to=intake by=owner event=9004"),
				admitted(byOwner, "1002")}, labels: clarifying}},
		{name: "a type the pipeline doesn't know is reported once, and the issue stays",
			setup: issueBy("owner", []string{"stage:intake", "type:frob"}, nil),
			want: outcome{comments: []string{"I can't move this issue from `intake` to `clarifying` on trusted: its type `frob` isn't one " +
				"the pipeline knows (SPEC §4). It stays at `intake`.\n\n<!-- issuedog:error kind=transition key=9001 -->"},
				labels: []string{"stage:intake", "type:frob"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			pass := intakePass(t)
			if tt.trust != nil {
				pass = intakePassWith(t, tt.trust)
			}
			check(t, tt.setup, pass, tt.want)
		})
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

// flakyTrust trusts owner and fails on eve, counting the calls in asked if
// it's set.
type flakyTrust struct{ asked *int }

func (ft flakyTrust) Trusted(_ context.Context, login string) (bool, error) {
	if ft.asked != nil {
		*ft.asked++
	}
	if login == "eve" {
		return false, errTrust
	}
	return login == "owner", nil
}

// visitAlone runs IntakeVisit alone on issue 1 with tr, as on a snapshot
// that step 1 hasn't brought up to date. before, if set, runs between the
// read and the visit.
func visitAlone(t *testing.T, f *githubtest.Fake, tr github.Trust, before func(f *githubtest.Fake)) error {
	t.Helper()
	c := f.Client(t)
	s, err := c.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if before != nil {
		before(f)
	}
	st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: tr}
	return st.IntakeVisit(context.Background(), s)
}

// IntakeVisit on its own checks what step 1 would have put right, and
// returns every error it meets.
func TestIntakeVisitAlone(t *testing.T) {
	intake := []string{"stage:intake"}
	timeline500 := func(f *githubtest.Fake) {
		f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "GET" && strings.HasSuffix(r.Path, "/timeline") }, 500)
	}
	for _, tt := range []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		before func(f *githubtest.Fake)
		trust  github.Trust // nil is flakyTrust
		err    string       // in the error, if one is wanted
		is     error        // the error wraps, if set
		// comments are those added; labels are the issue's after.
		comments, labels []string
	}{
		{name: "a move whose labels a crash cut short is no move from intake",
			setup:  issueBy("owner", intake, func(_ testing.TB, f *githubtest.Fake) { f.AddComment(1, app, admitted(byOwner, "9001")) }),
			labels: intake},
		{name: "a second stage label at intake is left to the label rules",
			setup:  issueBy("owner", []string{"stage:intake", "stage:planning", "approved"}, nil),
			labels: []string{"approved", "stage:intake", "stage:planning"}},
		{name: "an untrusted stage:intake on the owner's issue falls back to approved",
			setup: issueBy("owner", nil, func(_ testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "mallory", "stage:intake")
				f.AddLabel(1, "owner", "approved")
			}),
			comments: []string{admitted(byApproved, "9002")}, labels: []string{"approved", "stage:clarifying"}},
		{name: "an approved removed since the read is no key",
			setup:  issueBy("owner", nil, func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "owner", "approved") }),
			before: func(f *githubtest.Fake) { f.RemoveLabel(1, "mallory", "approved") }},
		{name: "a trusted removal stands over an older add",
			setup: issueBy("mallory", intake, func(_ testing.TB, f *githubtest.Fake) {
				f.AddLabel(1, "owner", "approved")
				f.RemoveLabel(1, "owner", "approved")
				f.AddLabel(1, "mallory", "approved")
			}),
			labels: []string{"approved", "stage:intake"}},
		{name: "the App's removal stands over an older add",
			setup: issueBy("owner", []string{"stage:intake"}, func(_ testing.TB, f *githubtest.Fake) {
				f.RemoveLabel(1, app, "stage:intake")
				f.AddLabel(1, "mallory", "stage:intake")
			}),
			labels: intake},
		{name: "the fallback key skips dirty transitions and other markers",
			setup: issueBy("owner", intake, func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, admitted(byOwner, "9001"))
				f.AddLabel(1, app, "stage:clarifying")
				f.RemoveLabel(1, app, "stage:intake")
				f.AddLabel(1, "owner", "stage:intake")
				f.RemoveLabel(1, "owner", "stage:clarifying")
				f.AddComment(1, app, overriddenTo("stage:intake", "from=clarifying to=intake by=owner event=9004"))
				hold := "<!-- issuedog:transition from=intake to=held at=intake reason=owner cmd=5 -->"
				f.EditComment(f.AddComment(1, app, hold), "mallory", hold)
				f.AddComment(1, app, "<!-- issuedog:revert key=9 -->")
			}),
			comments: []string{admitted(byOwner, "1002")}, labels: []string{"stage:clarifying"}},
		{name: "an event a marker names with no transition comment that counts is an error",
			setup: issueBy("owner", intake, func(_ testing.TB, f *githubtest.Fake) {
				f.AddComment(1, app, "<!-- issuedog:transition from=intake to=bogus event=9001 -->")
			}),
			err: "event 9001 keys a transition, but no transition comment counts", labels: intake},
		{name: "a failed timeline read is returned",
			setup: issueBy("owner", intake, nil), before: timeline500, err: "intake of o/r#1: list timeline", labels: intake},
		{name: "a failed dirty read is returned",
			setup:  issueBy("owner", intake, func(_ testing.TB, f *githubtest.Fake) { f.EditBody(1, "owner", "body, edited") }),
			before: func(f *githubtest.Fake) { f.FailGraphQL("boom") }, err: "boom", labels: intake},
		{name: "a failed author trust check is returned",
			setup: issueBy("owner", intake, nil), trust: failTrust{}, is: errTrust, labels: intake},
		{name: "a failed trust check of the stage:intake event is returned",
			setup: issueBy("owner", nil, func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "eve", "stage:intake") }),
			is:    errTrust, err: "event 9001", labels: intake},
		{name: "a failed trust check of the approved event is returned",
			setup: issueBy("mallory", intake, func(_ testing.TB, f *githubtest.Fake) { f.AddLabel(1, "eve", "approved") }),
			is:    errTrust, err: "event 9002", labels: []string{"approved", "stage:intake"}},
		{name: "a failed write is returned",
			setup: issueBy("owner", intake, nil), before: func(f *githubtest.Fake) { f.FailWrite(1) },
			err: "intake of o/r#1: move o/r#1 to clarifying", labels: intake},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.setup(t)
			before := len(f.State().Issues[0].Comments)
			tr := tt.trust
			if tr == nil {
				tr = flakyTrust{}
			}
			err := visitAlone(t, f, tr, tt.before)
			switch {
			case tt.err == "" && tt.is == nil && err != nil:
				t.Errorf("IntakeVisit = %v, want no error", err)
			case (tt.err != "" || tt.is != nil) && err == nil:
				t.Errorf("IntakeVisit = nil, want an error")
			case err != nil && (!strings.Contains(err.Error(), tt.err) || tt.is != nil && !errors.Is(err, tt.is)):
				t.Errorf("IntakeVisit = %v, want an error with %q, wrapping %v", err, tt.err, tt.is)
			}
			is := f.State().Issues[0]
			var added []string
			for _, c := range is.Comments[before:] {
				added = append(added, c.Body)
			}
			if !reflect.DeepEqual(added, tt.comments) || !reflect.DeepEqual(is.Labels, tt.labels) {
				t.Errorf("comments %q, labels %v; want %q, %v", added, is.Labels, tt.comments, tt.labels)
			}
		})
	}
}

// IntakeVisit asks Trust about each login once, and reads no timeline for
// an issue that can't move.
func TestIntakeVisitReads(t *testing.T) {
	asked := 0
	if err := visitAlone(t, issueBy("owner", []string{"stage:intake"}, nil)(t), flakyTrust{&asked}, nil); err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("Trust was asked %d times, want once, for owner", asked)
	}
	for name, setup := range map[string]func(testing.TB) *githubtest.Fake{
		"no stage label and no approved": issueBy("owner", nil, nil),
		"an untrusted author's, at intake": issueBy("mallory", nil, func(_ testing.TB, f *githubtest.Fake) {
			f.AddLabel(1, "owner", "stage:intake")
		}),
	} {
		f := setup(t)
		if err := visitAlone(t, f, flakyTrust{}, nil); err != nil {
			t.Fatal(err)
		}
		for _, r := range f.Requests() {
			if strings.HasSuffix(r.Path, "/timeline") {
				t.Errorf("%s: read the timeline, want no read: %+v", name, r)
			}
		}
	}
}

// A stage:intake whose event is later than the hold bound is held back,
// then left out, so the issue doesn't move on a label nothing explains.
// Once its event shows, the issue is read again when the label falls due,
// with no change to it, and moves, keyed by that event.
func TestIntakeMovesOnceALateEventShows(t *testing.T) {
	f := newFake(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.LagEvents(2 * time.Minute)
	n := f.AddIssue("owner", "Idea", "body")
	ev := f.AddLabel(n, "owner", "stage:intake")
	f.LagEvents(0)
	iterate(t, d) // held back
	f.Advance(github.HoldBound + time.Second)
	iterate(t, d) // left out
	if got := len(f.State().Issues[0].Comments); got != 0 {
		t.Fatalf("%d comments with stage:intake left out, want none", got)
	}
	var moves []string
	for range 6 {
		f.Advance(time.Minute)
		iterate(t, d)
	}
	is := f.State().Issues[0]
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "issuedog:revert") || strings.Contains(c.Body, "issuedog:error") {
			t.Errorf("a revert or state error: %q", c.Body)
		}
		if strings.Contains(c.Body, "issuedog:transition") {
			moves = append(moves, c.Body)
		}
	}
	if want := []string{admitted(byOwner, strconv.FormatInt(ev, 10))}; !reflect.DeepEqual(moves, want) || !reflect.DeepEqual(is.Labels, []string{"stage:clarifying"}) {
		t.Errorf("transitions %q, labels %v; want %q, at stage:clarifying", moves, is.Labels, want)
	}
}

// A stage label a repo rename put on, left out, neither stops the move
// from intake nor is removed by it: the label rules remove it once it's
// due, with no override recorded for it.
func TestIntakeMoveLeavesALeftOutStageLabel(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Idea", "body", "stage:intake")
	f.AddLabel(1, "mallory", "foo")
	f.Advance(time.Hour)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.RenameLabel("foo", "stage:held")
	f.AddComment(1, "owner", "poke")
	iterate(t, d) // held back
	f.Advance(github.HoldBound + time.Second)
	iterate(t, d) // left out, and the move
	is := f.State().Issues[0]
	if want := []string{"stage:clarifying", "stage:held"}; !reflect.DeepEqual(is.Labels, want) {
		t.Errorf("labels after the move %v, want %v: the left-out label kept", is.Labels, want)
	}
	for range 6 {
		f.Advance(time.Minute)
		iterate(t, d)
	}
	is = f.State().Issues[0]
	var moves []string
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "issuedog:transition") {
			moves = append(moves, c.Body)
		}
	}
	if want := []string{admitted(byOwner, "9001")}; !reflect.DeepEqual(moves, want) || !reflect.DeepEqual(is.Labels, []string{"stage:clarifying"}) {
		t.Errorf("transitions %q, labels %v; want %q, at stage:clarifying alone", moves, is.Labels, want)
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
