package daemon

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// discCounting is trustOnceCounting with DiscAccess: the logins in writers
// are write collaborators who aren't trusted. It counts each DiscAccess
// by login as asked, apart from Trusted's count.
type discCounting struct {
	trustOnceCounting
	writers map[string]bool
	access  map[string]int
}

func (c *discCounting) DiscAccess(_ context.Context, login string) (bool, bool, error) {
	c.access[login]++
	if c.fail[login] {
		return false, false, errTrustOnce
	}
	return c.trusted[strings.ToLower(login)], c.writers[strings.ToLower(login)], nil
}

func newDiscCounting() *discCounting {
	return &discCounting{trustOnceCounting: trustOnceCounting{trusted: map[string]bool{"owner": true}, fail: map[string]bool{"bob": true},
		asked: map[string]int{}}, writers: map[string]bool{"mallory": true}, access: map[string]int{}}
}

// A visit's trust asks DiscAccess about each login once, whatever its case,
// and not at all about one it knows is trusted, keeps no error, and wraps
// it. Its Trusted answers from what DiscAccess found.
func TestDiscTrustOnceVisit(t *testing.T) {
	ctx := context.Background()
	inner := newDiscCounting()
	v := &trustOnceVisit{trust: inner, verdicts: map[string]bool{}}
	if ok, err := v.Trusted(ctx, "owner"); !ok || err != nil {
		t.Fatalf("Trusted(owner) = %v, %v", ok, err)
	}
	for _, l := range []string{"owner", "Owner", "mallory", "Mallory", "alice", "ALICE"} {
		trusted, writer, err := v.DiscAccess(ctx, l)
		if err != nil || trusted != strings.EqualFold(l, "owner") || writer != strings.EqualFold(l, "mallory") {
			t.Errorf("DiscAccess(%s) = %v, %v, %v", l, trusted, writer, err)
		}
	}
	if ok, err := v.Trusted(ctx, "MALLORY"); ok || err != nil {
		t.Errorf("Trusted(MALLORY) = %v, %v; want false, from DiscAccess", ok, err)
	}
	for range 2 {
		if _, _, err := v.DiscAccess(ctx, "bob"); !errors.Is(err, errTrustOnce) || !strings.Contains(err.Error(), "trust for this visit") {
			t.Errorf("DiscAccess(bob) = %v; want the error, wrapped", err)
		}
	}
	if want := map[string]int{"mallory": 1, "alice": 1, "bob": 2}; !reflect.DeepEqual(inner.access, want) {
		t.Errorf("DiscAccess asked %v, want %v", inner.access, want)
	}
	if want := map[string]int{"owner": 1}; !reflect.DeepEqual(inner.asked, want) {
		t.Errorf("Trusted asked %v, want %v", inner.asked, want)
	}
}

// Over a Trust that isn't a DiscTrust, a visit's DiscAccess knows no
// write collaborator, and asks Trusted, once.
func TestDiscTrustOnceVisitPlainTrust(t *testing.T) {
	ctx := context.Background()
	inner := &trustOnceCounting{trusted: map[string]bool{"owner": true}, fail: map[string]bool{"bob": true}, asked: map[string]int{}}
	v := &trustOnceVisit{trust: inner, verdicts: map[string]bool{}}
	for range 2 {
		for _, l := range []string{"owner", "mallory"} {
			if trusted, writer, err := v.DiscAccess(ctx, l); err != nil || trusted != (l == "owner") || writer {
				t.Errorf("DiscAccess(%s) = %v, %v, %v; want no writer", l, trusted, writer, err)
			}
		}
	}
	if _, _, err := v.DiscAccess(ctx, "bob"); !errors.Is(err, errTrustOnce) || !strings.Contains(err.Error(), "access of bob") {
		t.Errorf("DiscAccess(bob) = %v; want the error, wrapped", err)
	}
	if want := map[string]int{"owner": 1, "mallory": 1, "bob": 1}; !reflect.DeepEqual(inner.asked, want) {
		t.Errorf("asked %v, want %v", inner.asked, want)
	}
}

// The current trust asks Step.Trust's DiscAccess as it is on each call,
// and with a Trust that isn't a DiscTrust knows no write collaborator.
// Both wrap an error.
func TestDiscCurrentTrust(t *testing.T) {
	ctx := context.Background()
	st := &Step{Trust: newDiscCounting()}
	cur, ok := st.CurrentTrust().(github.DiscTrust)
	if !ok {
		t.Fatal("CurrentTrust isn't a github.DiscTrust")
	}
	if trusted, writer, err := cur.DiscAccess(ctx, "mallory"); trusted || !writer || err != nil {
		t.Errorf("DiscAccess(mallory) = %v, %v, %v; want a writer", trusted, writer, err)
	}
	if _, _, err := cur.DiscAccess(ctx, "bob"); !errors.Is(err, errTrustOnce) || !strings.Contains(err.Error(), "current access") {
		t.Errorf("DiscAccess(bob) = %v; want the error, wrapped", err)
	}
	st.Trust = &trustOnceCounting{trusted: map[string]bool{"owner": true}, fail: map[string]bool{"bob": true}, asked: map[string]int{}}
	if trusted, writer, err := cur.DiscAccess(ctx, "owner"); !trusted || writer || err != nil {
		t.Errorf("DiscAccess(owner) = %v, %v, %v over a plain Trust; want trusted, no writer", trusted, writer, err)
	}
	if _, _, err := cur.DiscAccess(ctx, "bob"); !errors.Is(err, errTrustOnce) || !strings.Contains(err.Error(), "current access") {
		t.Errorf("DiscAccess(bob) = %v over a plain Trust; want the error, wrapped", err)
	}
}

// Only a DiscTrust, or the visit's or the current trust over one, knows
// writers.
func TestDiscKnowsWriters(t *testing.T) {
	disc, plain := newDiscCounting(), &trustOnceCounting{}
	for _, tt := range []struct {
		tr   github.Trust
		want bool
	}{
		{disc, true}, {plain, false}, {nil, false},
		{&trustOnceVisit{trust: disc}, true}, {&trustOnceVisit{trust: plain}, false},
		{(&Step{Trust: disc}).CurrentTrust(), true}, {(&Step{Trust: plain}).CurrentTrust(), false},
		{&trustOnceVisit{trust: (&Step{Trust: disc}).CurrentTrust()}, true},
	} {
		if got := discKnowsWriters(tt.tr); got != tt.want {
			t.Errorf("discKnowsWriters(%T over %v) = %v, want %v", tt.tr, tt.tr, got, tt.want)
		}
	}
}

// What a hold or a notice says of each kind of trigger, with every login
// and title defused, and nobody GitHub doesn't name in words.
func TestDiscWhat(t *testing.T) {
	for _, tt := range []struct {
		tr   discTrigger
		want string
	}{
		{discTrigger{Kind: discComment, By: "mallory", ID: 7}, "`mallory` posted comment 7"},
		{discTrigger{Kind: discEdit, By: "mallory", ID: state.BodyID, Of: "owner"}, "`mallory` edited the issue description, written by `owner`"},
		{discTrigger{Kind: discEdit, By: "mallory", ID: 7, Of: "issuedog[bot]"}, "`mallory` edited comment 7 by `issuedog[bot]`"},
		{discTrigger{Kind: discOwnEdit, By: "mallory", ID: 7}, "`mallory` edited their comment 7 after a code owner approved it"},
		{discTrigger{Kind: discDeletion, By: "mallory", Of: ""}, "`mallory` deleted a comment by someone GitHub doesn't name"},
		{discTrigger{Kind: discDeletion, By: "unknown", Of: "owner"}, "someone GitHub doesn't name deleted a comment by `owner`"},
		{discTrigger{Kind: discRename, By: "mallory", Title: "Hi @owner <b>x</b>"}, "`mallory` renamed this issue to “Hi @​owner &lt;b>x&lt;/b>”"},
	} {
		if got := discWhat(tt.tr); got != tt.want {
			t.Errorf("discWhat(%+v) = %q, want %q", tt.tr, got, tt.want)
		}
	}
}

// The origin is the latest transition's in the clarifying after a hold,
// and the issue's stage anywhere else, or with no origin to carry.
func TestDiscOrigin(t *testing.T) {
	tr := func(fields map[string]string) github.Marker { return github.Marker{Kind: "transition", Fields: fields} }
	for _, tt := range []struct {
		name string
		is   state.Issue
		want state.Stage
	}{
		{"first hold", state.Issue{Stage: state.SpecReview, Markers: []github.Marker{tr(map[string]string{"from": "clarifying", "to": "spec-review"})}}, state.SpecReview},
		{"clarifying after a hold", state.Issue{Stage: state.Clarifying, Markers: []github.Marker{
			tr(map[string]string{"from": "held", "to": "clarifying", "origin": "spec-review"}),
			{Kind: "revision", Fields: map[string]string{"rev": "1"}}}}, state.SpecReview},
		{"origin elsewhere", state.Issue{Stage: state.Planning, Markers: []github.Marker{
			tr(map[string]string{"from": "clarifying", "to": "planning", "origin": "spec-review"})}}, state.Planning},
		{"an older origin", state.Issue{Stage: state.Clarifying, Markers: []github.Marker{
			tr(map[string]string{"from": "held", "to": "clarifying", "origin": "spec-review"}),
			tr(map[string]string{"from": "spec-review", "to": "clarifying"})}}, state.Clarifying},
		{"a bad origin", state.Issue{Stage: state.Clarifying, Markers: []github.Marker{
			tr(map[string]string{"from": "held", "to": "clarifying", "origin": "held"})}}, state.Clarifying},
		{"no marker", state.Issue{Stage: state.Intake}, state.Intake},
	} {
		if got := discOrigin(tt.is); got != tt.want {
			t.Errorf("%s: discOrigin = %s, want %s", tt.name, got, tt.want)
		}
	}
}

// The floor is the first transition marker, or a later move out of held,
// read as Derive reads markers: not in dirty content, nor in a body
// someone else edited last.
func TestDiscFloor(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	mark := func(fields string) string { return "<!-- issuedog:transition " + fields + " -->" }
	s := &github.Snapshot{Author: app, Body: mark("from=intake to=ready run=a"), CreatedAt: at(1), Comments: []github.Comment{
		{ID: 10, Author: app, Body: mark("from=ready to=held at=ready reason=owner cmd=3"), CreatedAt: at(2)},
		{ID: 11, Author: app, Body: mark("from=held to=held at=ready reason=owner event=4 by=owner"), CreatedAt: at(3)},
		{ID: 12, Author: app, Body: mark("from=held to=ready event=5 by=owner"), CreatedAt: at(4)},
		{ID: 13, Author: github.Author{Login: "mallory"}, Body: mark("from=held to=ready event=6 by=owner"), CreatedAt: at(5)},
		{ID: 14, Author: app, Body: mark("from=ready to=held at=ready reason=owner cmd=7"), CreatedAt: at(6)},
	}}
	none := func(int64) bool { return false }
	if id, when, ok := discFloor(s, "issuedog", none); !ok || id != 12 || !when.Equal(at(4)) {
		t.Errorf("floor = %d at %s, %v; want the move out of held, 12", id, when, ok)
	}
	if id, _, ok := discFloor(s, "issuedog", func(id int64) bool { return id == 12 }); !ok || id != state.BodyID {
		t.Errorf("floor with 12 dirty = %d, %v; want the body's first marker", id, ok)
	}
	s.BodyEdit = github.Edit{By: github.Author{Login: "owner"}, At: at(9)}
	if id, _, ok := discFloor(s, "issuedog", func(id int64) bool { return id == 12 }); !ok || id != 10 {
		t.Errorf("floor with the body edited = %d, %v; want comment 10's marker first", id, ok)
	}
	if _, _, ok := discFloor(&github.Snapshot{Comments: []github.Comment{{ID: 1, Author: app, Body: "no marker"}}}, "issuedog", none); ok {
		t.Error("floor found with no transition marker")
	}
}

// A login is an outsider only when every association the snapshot gives
// says so, whatever its case: one that says COLLABORATOR, or one not
// known, is asked about.
func TestDiscWhoOf(t *testing.T) {
	s := &github.Snapshot{Author: github.Author{Login: "Eve"}, AuthorAssociation: "NONE", Comments: []github.Comment{
		{Author: github.Author{Login: "eve"}, Association: "FIRST_TIMER"},
		{Author: github.Author{Login: "mallory"}, Association: "NONE"},
		{Author: github.Author{Login: "Mallory"}, Association: "COLLABORATOR"},
		{Author: github.Author{Login: "ann"}, Association: "COLLABORATOR"},
		{Author: github.Author{Login: "ann"}, Association: "NONE"},
		{Author: github.Author{Login: "bo"}, Association: ""},
	}}
	inner := newDiscCounting()
	who := discWhoOf(s, inner)
	for _, l := range []string{"eve", "mallory", "ann", "bo", "zed"} {
		if _, _, err := who.ask(context.Background(), github.Author{Login: l}); err != nil {
			t.Fatal(err)
		}
	}
	if want := map[string]int{"mallory": 1, "ann": 1, "bo": 1, "zed": 1}; !reflect.DeepEqual(inner.access, want) {
		t.Errorf("asked %v, want %v: all but eve, an outsider everywhere", inner.access, want)
	}
}

// An edit's key names its content and time in Unix seconds.
func TestDiscEditKey(t *testing.T) {
	if got := discEditKey(42, time.Unix(1767225606, 0)); got != "e42-1767225606" {
		t.Errorf("discEditKey = %q", got)
	}
}
