package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/issuedog/internal/trust"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// discNoWriters is a DiscTrust that says, as trust.Set does with no
// CODEOWNERS file, whether nobody can be a writer.
type discNoWriters struct {
	*discCounting
	none bool
}

func (n discNoWriters) DiscNoWriters() bool { return n.none }

// Only a DiscTrust, or the visit's or the current trust over one, knows
// writers, and not one that says nobody can be one.
func TestDiscKnowsWriters(t *testing.T) {
	t.Parallel()
	disc, plain := newDiscCounting(), &trustOnceCounting{}
	for _, tt := range []struct {
		tr   github.Trust
		want bool
	}{
		{disc, true}, {plain, false}, {nil, false},
		{&trustOnceVisit{trust: disc}, true}, {&trustOnceVisit{trust: plain}, false},
		{(&Step{Trust: disc}).CurrentTrust(), true}, {(&Step{Trust: plain}).CurrentTrust(), false},
		{&trustOnceVisit{trust: (&Step{Trust: disc}).CurrentTrust()}, true},
		{discNoWriters{disc, true}, false}, {discNoWriters{disc, false}, true},
		{&trustOnceVisit{trust: discNoWriters{disc, true}}, false}, {(&Step{Trust: discNoWriters{disc, true}}).CurrentTrust(), false},
	} {
		if got := discKnowsWriters(tt.tr); got != tt.want {
			t.Errorf("discKnowsWriters(%T over %v) = %v, want %v", tt.tr, tt.tr, got, tt.want)
		}
	}
}

// What a hold or a notice says of each kind of trigger, with every login
// and title defused, and nobody GitHub doesn't name in words.
func TestDiscWhat(t *testing.T) {
	t.Parallel()
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

// Only a write collaborator's edit of their own comment after a code
// owner approved it gets the sentence that accepting the edit doesn't
// approve the new text, and how to approve it, naming its comment, in its
// notice and, shorter, in the digest: no other kind does, nor a trigger
// known from its record alone. Its text is the App's only, so a login in
// the trigger doesn't reach it.
func TestEditNoticeApproval(t *testing.T) {
	t.Parallel()
	const (
		notice = "Accepting the edit ends this edit's hold but doesn't approve the new text. " +
			"To approve it, a code owner adds a 👍 to comment 7 after the edit, first removing any 👍 they gave it before."
		digest = "Accepting it doesn't approve the new text. " +
			"To approve it, a code owner adds a 👍 to comment 7 after the edit, first removing any 👍 they gave it before."
	)
	for _, tt := range []struct {
		name           string
		tr             discTrigger
		notice, digest string
	}{
		{"an own edit", discTrigger{Kind: discOwnEdit, By: "@mallory", ID: 7}, notice, digest},
		{"an own edit known from its record", discTrigger{Kind: discOwnEdit, ID: 7, FromRecord: true}, "", ""},
		{"a comment", discTrigger{Kind: discComment, By: "mallory", ID: 7}, "", ""},
		{"an edit of a comment", discTrigger{Kind: discEdit, By: "mallory", ID: 7, Of: "owner"}, "", ""},
		{"an edit of the description", discTrigger{Kind: discEdit, By: "mallory", ID: state.BodyID, Of: "owner"}, "", ""},
		{"a deletion", discTrigger{Kind: discDeletion, By: "mallory", Of: "owner"}, "", ""},
		{"a rename", discTrigger{Kind: discRename, By: "mallory", Title: "x"}, "", ""},
		{"a close", discTrigger{Kind: discClose, By: "mallory"}, "", ""},
	} {
		if got := editNoticeApproval(tt.tr); got != tt.notice {
			t.Errorf("%s: editNoticeApproval(%+v) = %q, want %q", tt.name, tt.tr, got, tt.notice)
		}
		if got := editNoticeDigest(tt.tr); got != tt.digest {
			t.Errorf("%s: editNoticeDigest(%+v) = %q, want %q", tt.name, tt.tr, got, tt.digest)
		}
	}
}

// editNoticeAppend adds a sentence after a space, and nothing for none.
func TestEditNoticeAppend(t *testing.T) {
	t.Parallel()
	if got, want := editNoticeAppend("Text.", "More."), "Text. More."; got != want {
		t.Errorf("editNoticeAppend = %q, want %q", got, want)
	}
	if got, want := editNoticeAppend("Text.", ""), "Text."; got != want {
		t.Errorf("editNoticeAppend with no sentence = %q, want %q", got, want)
	}
}

// The origin is the latest transition's in the clarifying after a hold,
// and the hold's from stage anywhere else, or with no origin to carry.
func TestDiscOrigin(t *testing.T) {
	t.Parallel()
	tr := func(fields map[string]string) github.Marker { return github.Marker{Kind: "transition", Fields: fields} }
	for _, tt := range []struct {
		name  string
		is    state.Issue
		stage state.Stage
		want  state.Stage
	}{
		{"first hold", state.Issue{Markers: []github.Marker{tr(map[string]string{"from": "clarifying", "to": "spec-review"})}}, state.SpecReview, state.SpecReview},
		{"clarifying after a hold", state.Issue{Markers: []github.Marker{
			tr(map[string]string{"from": "held", "to": "clarifying", "origin": "spec-review"}),
			{Kind: "revision", Fields: map[string]string{"rev": "1"}}}}, state.Clarifying, state.SpecReview},
		{"origin elsewhere", state.Issue{Markers: []github.Marker{
			tr(map[string]string{"from": "clarifying", "to": "planning", "origin": "spec-review"})}}, state.Planning, state.Planning},
		{"an older origin", state.Issue{Markers: []github.Marker{
			tr(map[string]string{"from": "held", "to": "clarifying", "origin": "spec-review"}),
			tr(map[string]string{"from": "spec-review", "to": "clarifying"})}}, state.Clarifying, state.Clarifying},
		{"a bad origin", state.Issue{Markers: []github.Marker{
			tr(map[string]string{"from": "held", "to": "clarifying", "origin": "held"})}}, state.Clarifying, state.Clarifying},
		{"no marker", state.Issue{}, state.Ready, state.Ready},
	} {
		if got := discOrigin(tt.is, tt.stage); got != tt.want {
			t.Errorf("%s: discOrigin = %s, want %s", tt.name, got, tt.want)
		}
	}
}

// The floor is the first transition marker, or a later move out of held,
// read as Derive reads markers: not in dirty content, nor in a body
// someone else edited last. A hand-set move out of held is bounded by its
// label event's time, by time, once the label rules give it; with no
// marker, a stage label past intake bounds nothing.
func TestDiscFloor(t *testing.T) {
	t.Parallel()
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
	if b, ok := discFloor(s, "issuedog", none); !ok || b != (discBound{ID: 12, At: at(4)}) {
		t.Errorf("floor = %+v, %v; want the move out of held, 12, by ID", b, ok)
	}
	s.DiscLabelTimes = map[int64]time.Time{5: at(3), 4: at(0)}
	if b, ok := discFloor(s, "issuedog", none); !ok || b != (discBound{ID: 12, At: at(3), ByTime: true}) {
		t.Errorf("floor = %+v, %v; want the move out of held, 12, by its label's time", b, ok)
	}
	if b, ok := discFloor(s, "issuedog", func(id int64) bool { return id == 12 }); !ok || b != (discBound{ID: state.BodyID, At: at(1)}) {
		t.Errorf("floor with 12 dirty = %+v, %v; want the body's first marker", b, ok)
	}
	s.BodyEdit = github.Edit{By: github.Author{Login: "owner"}, At: at(9)}
	if b, ok := discFloor(s, "issuedog", func(id int64) bool { return id == 12 }); !ok || b.ID != 10 {
		t.Errorf("floor with the body edited = %+v, %v; want comment 10's marker first", b, ok)
	}
	bare := &github.Snapshot{Comments: []github.Comment{{ID: 1, Author: app, Body: "no marker"}}}
	if _, ok := discFloor(bare, "issuedog", none); ok {
		t.Error("floor found with no transition marker")
	}
	for labels, want := range map[string]bool{"stage:intake": false, "stage:ready": true, "stage:held": true, "priority:p1": false} {
		bare.Labels = []string{labels}
		if b, ok := discFloor(bare, "issuedog", none); ok != want || b != (discBound{}) {
			t.Errorf("floor with %s and no marker = %+v, %v; want %v, bounding nothing", labels, b, ok, want)
		}
	}
}

// A recorded act counts while its record is in the bound's comment or a
// later one; an unrecorded one at or after the bound's time, in its
// second too, or for a comment above its ID, unless the bound is by time.
func TestDiscOpenAfter(t *testing.T) {
	t.Parallel()
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	records := map[string]int64{"c5": 9, "c6": 10, "r": 11}
	for _, tt := range []struct {
		b    discBound
		key  string
		at   time.Time
		id   int64
		want bool
	}{
		{discBound{ID: 10, At: at(5)}, "c5", at(9), 5, false},
		{discBound{ID: 10, At: at(5)}, "c6", at(1), 6, true},
		{discBound{ID: 10, At: at(5)}, "r", at(1), 0, true},
		{discBound{ID: 10, At: at(5)}, "c9", at(9), 9, false},
		{discBound{ID: 10, At: at(5)}, "c10", at(1), 10, false},
		{discBound{ID: 10, At: at(5)}, "c11", at(1), 11, true},
		{discBound{ID: 10, At: at(5), ByTime: true}, "c9", at(5), 9, true},
		{discBound{ID: 10, At: at(5), ByTime: true}, "c11", at(4), 11, false},
		{discBound{ID: 10, At: at(5)}, "x", at(5), 0, true},
		{discBound{ID: 10, At: at(5)}, "x", at(4), 0, false},
	} {
		got, rec := discOpenAfter(tt.b, records)(tt.key, tt.at, tt.id)
		if _, want := records[tt.key]; rec != want {
			t.Errorf("open after %+v (%s): recorded %v, want %v", tt.b, tt.key, rec, want)
		}
		if got != tt.want {
			t.Errorf("open after %+v (%s at %s, %d) = %v, want %v", tt.b, tt.key, tt.at.Format(time.TimeOnly), tt.id, got, tt.want)
		}
	}
}

// The records are the first comment of each key, of a discussion hold or a
// notice, read as Derive reads markers.
func TestDiscRecords(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 1, Author: app, Body: "<!-- issuedog:transition from=ready to=held at=ready reason=discussion event=c9 -->\n<!-- issuedog:notice key=x -->"},
		{ID: 2, Author: app, Body: "<!-- issuedog:notice key=x -->\n<!-- issuedog:notice key=y -->"},
		{ID: 3, Author: app, Body: "<!-- issuedog:transition from=ready to=held at=ready reason=owner event=z -->"},
		{ID: 4, Author: app, Body: "<!-- issuedog:notice key=w -->"},
		{ID: 5, Author: github.Author{Login: "mallory"}, Body: "<!-- issuedog:notice key=v -->"},
	}}
	got := discRecords(s, "issuedog", func(id int64) bool { return id == 4 })
	if want := map[string]int64{"c9": 1, "x": 1, "y": 2}; !reflect.DeepEqual(got, want) {
		t.Errorf("records = %v, want %v", got, want)
	}
}

// discHitsApp: an edit of the App's content, or a deletion of the App's or
// an unnamed author's comment.
func TestDiscHitsApp(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		tr   discTrigger
		want bool
	}{
		{discTrigger{Kind: discEdit, Of: "issuedog[bot]"}, true},
		{discTrigger{Kind: discEdit, Of: "owner"}, false},
		{discTrigger{Kind: discEdit, Of: ""}, false},
		{discTrigger{Kind: discDeletion, Of: "issuedog[bot]"}, true},
		{discTrigger{Kind: discDeletion, Of: ""}, true},
		{discTrigger{Kind: discDeletion, Of: "owner"}, false},
		{discTrigger{Kind: discComment, Of: "issuedog[bot]"}, false},
		{discTrigger{Kind: discRename}, false},
	} {
		if got := discHitsApp(tt.tr, "issuedog"); got != tt.want {
			t.Errorf("discHitsApp(%+v) = %v, want %v", tt.tr, got, tt.want)
		}
	}
}

// A login is an outsider only when every association the snapshot gives
// says so, whatever its case: one that says COLLABORATOR, or one not
// known, is asked about.
func TestDiscWhoOf(t *testing.T) {
	t.Parallel()
	s := &github.Snapshot{Author: github.Author{Login: "Eve"}, AuthorAssociation: "NONE", Comments: []github.Comment{
		{Author: github.Author{Login: "eve"}, Association: "FIRST_TIMER"},
		{Author: github.Author{Login: "mallory"}, Association: "NONE"},
		{Author: github.Author{Login: "Mallory"}, Association: "COLLABORATOR"},
		{Author: github.Author{Login: "ann"}, Association: "COLLABORATOR"},
		{Author: github.Author{Login: "ann"}, Association: "NONE"},
		{Author: github.Author{Login: "bo"}, Association: ""},
	}}
	inner := newDiscCounting()
	who := (&Applier{}).discWhoOf(s, inner)
	for _, l := range []string{"eve", "mallory", "ann", "bo", "zed"} {
		if _, _, err := who.ask(context.Background(), github.Author{Login: l}); err != nil {
			t.Fatal(err)
		}
	}
	if want := map[string]int{"mallory": 1, "ann": 1, "bo": 1, "zed": 1}; !reflect.DeepEqual(inner.access, want) {
		t.Errorf("asked %v, want %v: all but eve, an outsider everywhere", inner.access, want)
	}
}

// A discWho asks about each login once per call, whatever its case, and
// never about a bot, nobody, or anyone with no Trust: none is a writer.
func TestDiscWhoAsksOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := newDiscCounting()
	inner.writers["helper[bot]"], inner.writers["robo"] = true, true
	who := (&Applier{}).discWhoOf(&github.Snapshot{}, inner)
	for _, a := range []github.Author{{Login: "mallory"}, {Login: "MALLORY"}, {Login: "helper[bot]", Type: "Bot"},
		{Login: "robo", Type: "Bot"}, {}, {Login: "unknown"}} {
		w, err := who.writer(ctx, a)
		if want := strings.EqualFold(a.Login, "mallory"); err != nil || w != want {
			t.Errorf("writer(%+v) = %v, %v; want %v", a, w, err, want)
		}
	}
	if want := map[string]int{"mallory": 1}; !reflect.DeepEqual(inner.access, want) {
		t.Errorf("asked %v, want %v", inner.access, want)
	}
	if w, err := (discWho{access: map[string][2]bool{}}).writer(ctx, github.Author{Login: "mallory"}); w || err != nil {
		t.Errorf("writer with no Trust = %v, %v", w, err)
	}
	if _, err := who.trusted(ctx, github.Author{Login: "bob"}); !errors.Is(err, errTrustOnce) || !strings.Contains(err.Error(), "trust of bob") {
		t.Errorf("trusted(bob) = %v; want the error, wrapped", err)
	}
}

// A question of trust alone asks Trusted, never DiscAccess, once per call
// and login, whatever its case, and uses DiscAccess's answer when writer
// asked already. A bot, nobody, an outsider and no Trust ask nothing.
func TestDiscWhoTrustedAsksTrusted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := newDiscCounting()
	who := (&Applier{}).discWhoOf(&github.Snapshot{Comments: []github.Comment{{Author: github.Author{Login: "eve"}, Association: "NONE"}}}, inner)
	if _, err := who.writer(ctx, github.Author{Login: "mallory"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []github.Author{{Login: "owner"}, {Login: "Owner"}, {Login: "ann"}, {Login: "mallory"}, {Login: "eve"},
		{Login: "robo", Type: "Bot"}, {}, {Login: "unknown"}} {
		got, err := who.trusted(ctx, a)
		if want := strings.EqualFold(a.Login, "owner"); err != nil || got != want {
			t.Errorf("trusted(%+v) = %v, %v; want %v", a, got, err, want)
		}
	}
	if want := map[string]int{"mallory": 1}; !reflect.DeepEqual(inner.access, want) {
		t.Errorf("DiscAccess asked %v, want %v: writer's question only", inner.access, want)
	}
	if want := map[string]int{"owner": 1, "ann": 1}; !reflect.DeepEqual(inner.asked, want) {
		t.Errorf("Trusted asked %v, want %v", inner.asked, want)
	}
	if ok, err := (discWho{verdicts: map[string]bool{}}).trusted(ctx, github.Author{Login: "owner"}); ok || err != nil {
		t.Errorf("trusted with no Trust = %v, %v", ok, err)
	}
}

// discNoRecordTrust is discCounting, but GitHub has no permission record
// of ghost.
type discNoRecordTrust struct{ *discCounting }

func (c discNoRecordTrust) DiscAccess(ctx context.Context, login string) (bool, bool, error) {
	if strings.EqualFold(login, "ghost") {
		c.access["ghost"]++
		return false, false, fmt.Errorf("permission of ghost: %w", trust.ErrNoRecord)
	}
	return c.discCounting.DiscAccess(ctx, login)
}

// A "no permission record" answer is remembered across calls, for each
// issue asked about, until its wait is up, when Due gives those issues to
// read again; a snapshot that shows the login as a collaborator forgets it.
func TestDiscNoRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := discNoRecordTrust{newDiscCounting()}
	a := &Applier{}
	ask := func(s *github.Snapshot) {
		t.Helper()
		if w, err := a.discWhoOf(s, inner).writer(ctx, github.Author{Login: "Ghost"}); w || err != nil {
			t.Fatalf("writer(ghost) = %v, %v; want no writer, no error", w, err)
		}
	}
	ask(&github.Snapshot{Number: 1})
	ask(&github.Snapshot{Number: 1})
	ask(&github.Snapshot{Number: 2})
	if inner.access["ghost"] != 1 {
		t.Fatalf("asked about ghost %d times; want once, then remembered", inner.access["ghost"])
	}
	if due := a.discNoRecordDue(time.Now()); len(due) != 0 {
		t.Errorf("due %v at once; want none", due)
	}
	if due := a.discNoRecordDue(time.Now().Add(discNoRecordWait + time.Second)); !reflect.DeepEqual(due, []int{1, 2}) {
		t.Errorf("due %v after the wait; want the issues asked for, [1 2]", due)
	}
	ask(&github.Snapshot{Number: 3})
	if inner.access["ghost"] != 2 {
		t.Errorf("asked about ghost %d times after the wait; want twice", inner.access["ghost"])
	}
	ask(&github.Snapshot{Number: 3, Comments: []github.Comment{{Author: github.Author{Login: "ghost"}, Association: "COLLABORATOR"}}})
	if inner.access["ghost"] != 3 || a.discNoRecords["ghost"] == nil {
		t.Errorf("asked about ghost %d times when shown as a collaborator; want three, and the record noted again", inner.access["ghost"])
	}
	a.discNoRecords["ghost"].at = time.Now().Add(-discNoRecordWait)
	ask(&github.Snapshot{Number: 4})
	ask(&github.Snapshot{Number: 5})
	if inner.access["ghost"] != 4 {
		t.Errorf("asked about ghost %d times once the wait was up; want four, the wait started again", inner.access["ghost"])
	}
	if r := a.discNoRecords["ghost"]; !reflect.DeepEqual(r.issues, map[int]bool{3: true, 4: true, 5: true}) {
		t.Errorf("issues %v; want 3, 4 and 5", r.issues)
	}
	a.discNoRecordForget("ghost")
	if _, ok := a.discNoRecords["ghost"]; ok {
		t.Error("ghost still remembered after discNoRecordForget")
	}
}

// A first marker that records a stage label set by hand, as when the owner
// brings an issue in by label, is bounded by the label's time, by time.
func TestDiscFloorFirstOverride(t *testing.T) {
	t.Parallel()
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 10, Author: app, Body: "<!-- issuedog:transition from=intake to=ready by=owner event=5 -->", CreatedAt: at(8)},
	}, DiscLabelTimes: map[int64]time.Time{5: at(3)}}
	if b, ok := discFloor(s, "issuedog", func(int64) bool { return false }); !ok || b != (discBound{ID: 10, At: at(3), ByTime: true}) {
		t.Errorf("floor = %+v, %v; want the label's time, by time", b, ok)
	}
	// A move into the pipeline keyed by its label event, with no by, is the
	// App's admission, which comes later: nothing before it counts.
	s.Comments[0].Body = "<!-- issuedog:transition from=intake to=clarifying event=5 -->"
	if b, ok := discFloor(s, "issuedog", func(int64) bool { return false }); !ok || b != (discBound{ID: 10, At: at(8)}) {
		t.Errorf("floor = %+v, %v; want the admission's comment, by ID", b, ok)
	}
}

// At a floor by ID, a comment in the floor's second counts only above the
// floor's comment: one posted just before the marker, in its second,
// doesn't.
func TestDiscFindByID(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 0, 0, 5, 0, time.UTC)
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	mallory := github.Author{Login: "mallory", Type: "User"}
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 9, Author: mallory, Body: "before", CreatedAt: at},
		{ID: 10, Author: app, Body: "<!-- issuedog:transition from=intake to=ready run=a -->", CreatedAt: at},
		{ID: 11, Author: mallory, Body: "after", CreatedAt: at},
	}}
	a := &Applier{Client: noReactions(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	found, err := a.discFind(context.Background(), s, Dirt{}, newDiscCounting())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Key != "c11" {
		t.Errorf("found %+v; want only comment 11", found)
	}
}

// A notice may be lost only to App content posted, or a deletion made, at
// or after its trigger: a dirty App comment or a deletion from before it
// isn't its notice, nor is the trigger's own content.
func TestDiscLostTimes(t *testing.T) {
	t.Parallel()
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	tr := discTrigger{Kind: discRename, Key: "77", At: at(5)}
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		d    Dirt
		want bool
	}{
		{"dirty before", &github.Snapshot{Comments: []github.Comment{{ID: 1, Author: app, CreatedAt: at(4)}}}, Dirt{1: "eve"}, false},
		{"dirty at", &github.Snapshot{Comments: []github.Comment{{ID: 1, Author: app, CreatedAt: at(5)}}}, Dirt{1: "eve"}, true},
		{"clean after", &github.Snapshot{Comments: []github.Comment{{ID: 1, Author: app, CreatedAt: at(6)}}}, Dirt{}, false},
		{"dirty owner's after", &github.Snapshot{Comments: []github.Comment{{ID: 1, Author: github.Author{Login: "owner"}, CreatedAt: at(6)}}}, Dirt{1: "eve"}, false},
		{"deleted before", &github.Snapshot{Deleted: []github.DeletedComment{{ID: "x", Author: app, At: at(4)}}}, Dirt{}, false},
		{"deleted at", &github.Snapshot{Deleted: []github.DeletedComment{{ID: "x", Author: app, At: at(5)}}}, Dirt{}, true},
		{"unnamed deleted after", &github.Snapshot{Deleted: []github.DeletedComment{{ID: "x", At: at(6)}}}, Dirt{}, true},
		{"owner's deleted after", &github.Snapshot{Deleted: []github.DeletedComment{{ID: "x", Author: github.Author{Login: "owner"}, At: at(6)}}}, Dirt{}, false},
	} {
		if got := discLost(tt.s, tt.d, tr, "issuedog"); got != tt.want {
			t.Errorf("%s: discLost = %v, want %v", tt.name, got, tt.want)
		}
	}
	own := discTrigger{Kind: discEdit, Key: "e1-5", ID: 1, At: at(5)}
	if discLost(&github.Snapshot{Comments: []github.Comment{{ID: 1, Author: app, CreatedAt: at(5)}}}, Dirt{1: "mallory"}, own, "issuedog") {
		t.Error("discLost for an edit's own comment = true")
	}
	del := discTrigger{Kind: discDeletion, Key: "dx", At: at(5)}
	if discLost(&github.Snapshot{Deleted: []github.DeletedComment{{ID: "x", Author: app, At: at(5)}}}, Dirt{}, del, "issuedog") {
		t.Error("discLost for a deletion's own event = true")
	}
}

// A trigger whose key no marker can hold is left out, as it would fail
// every pass.
func TestDiscFindSkipsBadKeys(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	at := time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC)
	s := &github.Snapshot{Comments: []github.Comment{
		{ID: 1, Author: app, Body: "<!-- issuedog:transition from=intake to=ready run=a -->", CreatedAt: at.Add(-time.Hour)},
	}, Deleted: []github.DeletedComment{
		{ID: "CDE bad", By: github.Author{Login: "mallory", Type: "User"}, Author: app, At: at},
		{ID: "CDE_<x>", By: github.Author{Login: "mallory", Type: "User"}, Author: app, At: at},
		{ID: "CDE_ok", By: github.Author{Login: "mallory", Type: "User"}, Author: app, At: at},
	}}
	a := &Applier{Owner: "o", Repo: "r", AppSlug: "issuedog"}
	found, err := a.discFind(context.Background(), s, Dirt{}, newDiscCounting())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Key != "dCDE_ok" {
		t.Errorf("found %+v; want only the deletion whose key a marker holds", found)
	}
}

// An edit's key names its content and time in Unix seconds.
func TestDiscEditKey(t *testing.T) {
	t.Parallel()
	if got := discEditKey(42, time.Unix(1767225606, 0)); got != "e42-1767225606" {
		t.Errorf("discEditKey = %q", got)
	}
}

// noReactions is a client on which every comment has no reactions, for the
// resolver's reads (resolve.go, #188) in a test of a snapshot made by hand.
// It counts the requests it gets in *reads, if reads is given.
func noReactions(t testing.TB, reads ...*int) *github.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for _, n := range reads {
			*n++
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte("[]")); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	// The server's own client, not http.DefaultTransport, which another
	// test's server would close the idle connections of.
	gh, err := gogithub.NewClient(gogithub.WithHTTPClient(srv.Client()), gogithub.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return &github.Client{Client: gh}
}
