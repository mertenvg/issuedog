package state

import (
	"reflect"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

const slug = "issuedog"

var (
	appBot  = github.Author{Login: slug + "[bot]", Type: "Bot"}
	owner   = github.Author{Login: "owner", Type: "User"}
	mallory = github.Author{Login: "mallory", Type: "User"}
)

func clean(int64) bool { return false }

func dirtyIDs(ids ...int64) func(int64) bool {
	return func(id int64) bool {
		for _, d := range ids {
			if d == id {
				return true
			}
		}
		return false
	}
}

// snap is an issue whose comments, by is[i] with body bodies[i], have IDs 1, 2, ...
func snap(body string, by []github.Author, bodies ...string) *github.Snapshot {
	s := &github.Snapshot{Number: 7, Author: appBot, Body: body}
	for i, b := range bodies {
		s.Comments = append(s.Comments, github.Comment{ID: int64(i + 1), Author: by[i], Body: b})
	}
	return s
}

func TestDeriveStage(t *testing.T) {
	const (
		toClarifying = "<!-- issuedog:transition from=intake to=clarifying cmd=10 -->"
		toSpecReview = "<!-- issuedog:transition from=clarifying to=spec-review run=ab -->"
	)
	edited := snap("", []github.Author{appBot, appBot}, toClarifying, toSpecReview)
	// Edits don't decide: dirty does (plan #5, the amendment to assumption 7).
	edited.Comments[1].Edit = github.Edit{By: mallory, At: time.Unix(5, 0)}
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  Stage
	}{
		{"no marker is intake", snap("", nil), clean, Intake},
		{"the latest marker wins", snap("", []github.Author{appBot, appBot}, toClarifying, toSpecReview), clean, SpecReview},
		{"a dirty marker falls back to the one before", snap("", []github.Author{appBot, appBot}, toClarifying, toSpecReview), dirtyIDs(2), Clarifying},
		{"clean App content counts, whoever edited it", edited, clean, SpecReview},
		{"a trusted user's marker is ignored", snap("", []github.Author{appBot, owner}, toClarifying, toSpecReview), clean, Clarifying},
		{"an escaped marker is ignored", snap("", []github.Author{appBot, appBot}, toClarifying, github.Escape(toSpecReview)), clean, Clarifying},
		{"a marker to no stage is ignored", snap("", []github.Author{appBot, appBot}, toClarifying, "<!-- issuedog:transition to=nowhere cmd=3 -->"), clean, Clarifying},
		{"a closing marker counts", snap("", []github.Author{appBot, appBot}, toClarifying, "<!-- issuedog:transition from=clarifying to=closed cmd=4 -->"), clean, Closed},
		{"only a transition marker sets the stage", snap("", []github.Author{appBot, appBot}, toClarifying, "<!-- issuedog:reply cmd=4 reason=rejected to=planning -->"), clean, Clarifying},
		{"a body marker counts", snap(toClarifying, nil), clean, Clarifying},
		{"a dirty body is skipped", snap(toClarifying, nil), dirtyIDs(BodyID), Intake},
	} {
		if got := Derive(tt.s, slug, tt.dirty).Stage; got != tt.want {
			t.Errorf("%s: stage = %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestDeriveKeysAndErrors(t *testing.T) {
	s := snap("", []github.Author{appBot, appBot, appBot, owner, appBot},
		"<!-- issuedog:transition from=intake to=clarifying cmd=10 --><!-- issuedog:transition from=clarifying to=clarifying cmd=r11 run=ab -->",
		"<!-- issuedog:transition from=held to=clarifying by=owner event=12 -->",
		"<!-- issuedog:reply cmd=13 reason=rejected --><!-- issuedog:error kind=transition key=14 -->"+
			"<!-- issuedog:error kind=disagreement key=18 --><!-- issuedog:error key=19 ack=r20 -->",
		"<!-- issuedog:transition from=clarifying to=ready cmd=15 --><!-- issuedog:error kind=transition key=16 -->",
		"<!-- issuedog:transition from=clarifying to=ready cmd=17 -->")
	is := Derive(s, slug, dirtyIDs(5))
	wantKeys := map[Key]bool{CmdKey(10): true, ReactionKey(11): true, {"run", "ab"}: true, {"event", "12"}: true, CmdKey(13): true}
	if !reflect.DeepEqual(is.Keys, wantKeys) {
		t.Errorf("keys = %v, want %v", is.Keys, wantKeys)
	}
	for _, tt := range []struct {
		kind, key string
		want      bool
	}{
		{"transition", "14", true},
		{"disagreement", "14", false}, // the same key, another kind
		{"disagreement", "18", true},
		{"transition", "18", false},
		{"transition", "19", true}, // an ack covers its key for every kind
		{"disagreement", "19", true},
		{"transition", "16", false}, // a trusted user's marker
	} {
		if got := is.Reported(tt.kind, tt.key); got != tt.want {
			t.Errorf("Reported(%s, %s) = %v, want %v", tt.kind, tt.key, got, tt.want)
		}
	}
	for _, tt := range []struct {
		k    Key
		want bool
	}{
		{CmdKey(10), true}, {ReactionKey(11), true}, {Key{"run", "ab"}, true}, {Key{"event", "12"}, true},
		{CmdKey(13), false}, // a reply, not a transition
		{CmdKey(15), false}, // a trusted user's marker
		{CmdKey(17), false}, // dirty
		{Key{"cmd", ""}, false},
	} {
		if got := is.Moved(tt.k); got != tt.want {
			t.Errorf("Moved(%+v) = %v, want %v", tt.k, got, tt.want)
		}
	}
	if is.Stage != Clarifying {
		t.Errorf("stage = %s, want clarifying", is.Stage)
	}
}

func TestDeriveItem(t *testing.T) {
	const origin = "<!-- issuedog:origin plan=#5 rev=1 item=T4 -->"
	held := "<!-- issuedog:transition from=in-progress to=held at=in-progress cmd=3 -->"
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  Item
	}{
		{"origin in the body", snap(origin, nil), clean, Item{FromPlan: true}},
		{"origin in a dirty body", snap(origin, nil), dirtyIDs(BodyID), Item{}},
		{"origin in a comment", snap("", []github.Author{appBot}, origin), clean, Item{}},
		{"a held task reached ready", snap("", []github.Author{appBot}, held), clean, Item{ReachedReady: true}},
		{"a dirty marker doesn't reach ready", snap("", []github.Author{appBot}, held), dirtyIDs(1), Item{}},
		{"leaving a hold carries at", snap("", []github.Author{appBot}, "<!-- issuedog:transition from=held to=clarifying at=in-progress cmd=4 -->"), clean, Item{ReachedReady: true}},
	} {
		tt.s.Type, tt.s.Parent = "task", 3
		tt.want.Type, tt.want.HasParent = "task", true
		if got := Derive(tt.s, slug, tt.dirty).Item; got != tt.want {
			t.Errorf("%s: item = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

// Derive reads Origin from the App's whole origin marker in a clean body
// (#270): plan, rev, item and the body as its comment, whatever other
// fields the marker has. A body someone else edited last, a dirty body, a
// marker that isn't the App's, one in a comment or an escaped one, and one
// that lacks a field or has a bad one, give the zero Origin.
func TestDeriveOrigin(t *testing.T) {
	t.Parallel()
	const origin = "<!-- issuedog:origin plan=#248 rev=1 item=T4 -->"
	want := Origin{Plan: 248, Rev: 1, Item: "T4", Comment: BodyID}
	byApp := snap("The task.\n\n"+origin, nil)
	byApp.BodyEdit = github.Edit{By: appBot, At: time.Unix(5, 0)}
	byOwner := snap("The task.\n\n"+origin, nil)
	byOwner.BodyEdit = github.Edit{By: owner, At: time.Unix(5, 0)}
	byMallory := snap("The task.\n\n"+origin, nil)
	byMallory.Author = mallory
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  Origin
	}{
		{"a clean App marker", snap("The task.\n\n"+origin, nil), clean, want},
		{"other fields", snap("<!-- issuedog:origin check=label plan=#248 rev=1 item=T4 value=a%20b -->", nil), clean, want},
		{"a body the App edited last", byApp, clean, want},
		{"the first whole marker", snap("<!-- issuedog:origin plan=#248 rev=0 item=T1 -->"+origin+"<!-- issuedog:origin plan=#9 rev=2 item=S1 -->", nil), clean, want},
		{"a body someone else edited last", byOwner, clean, Origin{}},
		{"a dirty body", snap(origin, nil), dirtyIDs(BodyID), Origin{}},
		{"a marker that isn't the App's", byMallory, clean, Origin{}},
		{"an escaped marker", snap(github.Escape(origin), nil), clean, Origin{}},
		{"a marker in a comment", snap("", []github.Author{appBot}, origin), clean, Origin{}},
		{"no marker", snap("The task.", nil), clean, Origin{}},
		{"no #", snap("<!-- issuedog:origin plan=248 rev=1 item=T4 -->", nil), clean, Origin{}},
		{"plan 0", snap("<!-- issuedog:origin plan=#0 rev=1 item=T4 -->", nil), clean, Origin{}},
		{"a plan that isn't a number", snap("<!-- issuedog:origin plan=#x rev=1 item=T4 -->", nil), clean, Origin{}},
		{"no plan", snap("<!-- issuedog:origin rev=1 item=T4 -->", nil), clean, Origin{}},
		{"rev 0", snap("<!-- issuedog:origin plan=#248 rev=0 item=T4 -->", nil), clean, Origin{}},
		{"a rev that isn't a number", snap("<!-- issuedog:origin plan=#248 rev=r1 item=T4 -->", nil), clean, Origin{}},
		{"no item", snap("<!-- issuedog:origin plan=#248 rev=1 -->", nil), clean, Origin{}},
		{"another kind", snap("<!-- issuedog:revision plan=#248 rev=1 item=T4 -->", nil), clean, Origin{}},
	} {
		if got := Derive(tt.s, slug, tt.dirty).Origin; got != tt.want {
			t.Errorf("%s: origin = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}

// Derive reads Origin.Human, and so Item.Human, from human=1 on the App's
// whole origin marker in a clean body (#275), and a task with a parent
// that has it is a human task. Any other value, or none, isn't human; nor
// is human=1 on a marker that isn't read: in a dirty body, a body someone
// else edited last, someone else's marker, one in a comment, or one that
// isn't whole.
func TestDeriveOriginHuman(t *testing.T) {
	t.Parallel()
	const human = "<!-- issuedog:origin plan=#249 rev=1 item=T3 human=1 -->"
	byOwner := snap("The task.\n\n"+human, nil)
	byOwner.BodyEdit = github.Edit{By: owner, At: time.Unix(5, 0)}
	byMallory := snap("The task.\n\n"+human, nil)
	byMallory.Author = mallory
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  bool
	}{
		{"human=1", snap("The task.\n\n"+human, nil), clean, true},
		{"human=1 among check fields", snap("<!-- issuedog:origin plan=#249 rev=1 item=T3 human=1 check=label value=a%20b -->", nil), clean, true},
		{"no human", snap("<!-- issuedog:origin plan=#249 rev=1 item=T3 -->", nil), clean, false},
		{"human=0", snap("<!-- issuedog:origin plan=#249 rev=1 item=T3 human=0 -->", nil), clean, false},
		{"human=true", snap("<!-- issuedog:origin plan=#249 rev=1 item=T3 human=true -->", nil), clean, false},
		{"a dirty body", snap(human, nil), dirtyIDs(BodyID), false},
		{"a body someone else edited last", byOwner, clean, false},
		{"a marker that isn't the App's", byMallory, clean, false},
		{"a marker in a comment", snap("", []github.Author{appBot}, human), clean, false},
		{"an escaped marker", snap(github.Escape(human), nil), clean, false},
		{"a marker with no item", snap("<!-- issuedog:origin plan=#249 rev=1 human=1 -->", nil), clean, false},
		{"a later marker's human=1", snap("<!-- issuedog:origin plan=#249 rev=1 item=T3 -->"+human, nil), clean, false},
	} {
		tt.s.Type, tt.s.Parent = "task", 249
		is := Derive(tt.s, slug, tt.dirty)
		if is.Origin.Human != tt.want || is.Item.Human != tt.want {
			t.Errorf("%s: Origin.Human = %v, Item.Human = %v, want %v", tt.name, is.Origin.Human, is.Item.Human, tt.want)
		}
		want := ClassTask
		if tt.want {
			want = ClassHumanTask
		}
		if c, err := ClassOf(is.Item); is.Item.FromPlan && (err != nil || c != want) {
			t.Errorf("%s: class = %s, %v; want %s", tt.name, c, err, want)
		}
	}
	is := Derive(snap("The task.\n\n"+human, nil), slug, clean)
	if want := (Origin{Plan: 249, Rev: 1, Item: "T3", Comment: BodyID, Human: true}); is.Origin != want {
		t.Errorf("origin = %+v, want %+v", is.Origin, want)
	}
}

// Derive reads the latest triage marker that counts, and its comment: not
// a dirty one, nor one in the body or by anyone but the App.
func TestDeriveTriage(t *testing.T) {
	const (
		flag = "<!-- issuedog:triage verdict=ask run=a recommend=handle -->"
		left = "<!-- issuedog:triage verdict=leave cmd=r9 -->"
	)
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  TriageVerdict
	}{
		{"none", snap("", []github.Author{appBot}, "hello"), clean, TriageVerdict{}},
		{"a flag", snap("", []github.Author{appBot}, flag), clean, TriageVerdict{Verdict: "ask", Recommend: "handle", Comment: 1}},
		{"the latest wins", snap("", []github.Author{appBot, appBot}, flag, left), clean, TriageVerdict{Verdict: "leave", Comment: 2}},
		{"a dirty one falls back to the one before", snap("", []github.Author{appBot, appBot}, flag, left), dirtyIDs(2),
			TriageVerdict{Verdict: "ask", Recommend: "handle", Comment: 1}},
		{"a trusted user's is ignored", snap("", []github.Author{appBot, owner}, flag, left), clean,
			TriageVerdict{Verdict: "ask", Recommend: "handle", Comment: 1}},
		{"one in the body is ignored", snap(left, nil), clean, TriageVerdict{}},
		{"a transition after it leaves it the latest", snap("", []github.Author{appBot, appBot}, flag,
			"<!-- issuedog:transition from=intake to=clarifying cmd=r5 -->"), clean, TriageVerdict{Verdict: "ask", Recommend: "handle", Comment: 1}},
	} {
		if got := Derive(tt.s, slug, tt.dirty).Triage; got != tt.want {
			t.Errorf("%s: triage = %+v, want %+v", tt.name, got, tt.want)
		}
	}
	if keys := Derive(snap("", []github.Author{appBot}, left), slug, clean).Keys; !keys[ReactionKey(9)] {
		t.Errorf("keys = %v, want the triage marker's cmd=r9", keys)
	}
}

// An ack names its error's kind and covers only that kind and key. One with
// no kind, from before acks named one, covers its key for every kind (plan
// #7, Q5a). An ack reply isn't a posted error, and one that doesn't count
// covers nothing.
func TestDeriveAcksByKind(t *testing.T) {
	s := snap("", []github.Author{appBot, appBot, owner, appBot},
		"<!-- issuedog:error kind=attempts key=5 --><!-- issuedog:error kind=transition key=5 --><!-- issuedog:error kind=labels key=6 -->"+
			// An error with no kind, and markers of other kinds, are neither.
			"<!-- issuedog:error key=24 --><!-- issuedog:revert kind=labels key=25 --><!-- issuedog:reply kind=type key=26 ack=r27 -->",
		"<!-- issuedog:error kind=attempts key=5 ack=r9 --><!-- issuedog:error key=6 ack=r10 --><!-- issuedog:error kind=type ack=r13 -->",
		"<!-- issuedog:error kind=disagreement key=7 ack=r11 -->",
		"<!-- issuedog:error kind=disagreement key=8 ack=r12 -->")
	is := Derive(s, slug, dirtyIDs(4))
	wantAcks := map[ErrorKey]bool{{"attempts", "5"}: true, {"", "6"}: true}
	if !reflect.DeepEqual(is.Acks, wantAcks) {
		t.Errorf("acks = %v, want %v", is.Acks, wantAcks)
	}
	wantErrors := map[ErrorKey]int64{{"attempts", "5"}: 1, {"transition", "5"}: 1, {"labels", "6"}: 1}
	if !reflect.DeepEqual(is.Errors, wantErrors) {
		t.Errorf("errors = %v, want %v; an ack reply isn't a posted error", is.Errors, wantErrors)
	}
	for _, tt := range []struct {
		kind, key       string
		acked, reported bool
	}{
		{"attempts", "5", true, true},
		{"transition", "5", false, true}, // posted, but the ack is of another kind
		{"type", "5", false, false},      // neither posted nor acknowledged
		{"labels", "6", true, true},      // a kindless ack covers every kind
		{"type", "6", true, true},
		{"", "6", true, true},
		{"attempts", "60", false, false},
		{"disagreement", "7", false, false}, // a trusted user's marker
		{"disagreement", "8", false, false}, // dirty
		{"type", "", false, false},          // an ack with no key
	} {
		if got := is.Acked(tt.kind, tt.key); got != tt.acked {
			t.Errorf("Acked(%s, %s) = %v, want %v", tt.kind, tt.key, got, tt.acked)
		}
		if got := is.Reported(tt.kind, tt.key); got != tt.reported {
			t.Errorf("Reported(%s, %s) = %v, want %v", tt.kind, tt.key, got, tt.reported)
		}
	}
}

// Derive records each state error's comment, the earliest that carries it
// and counts, and the comment holding the latest transition that counts,
// so a later reader can tell an error posted before a transition from one
// posted after it (plan #7, assumption 7).
func TestDeriveErrorAndTransitionComments(t *testing.T) {
	const (
		toClarifying = "<!-- issuedog:transition from=intake to=clarifying cmd=10 -->"
		toSpecReview = "<!-- issuedog:transition from=clarifying to=spec-review run=a -->"
		toPlanning   = "<!-- issuedog:transition from=spec-review to=planning cmd=r30 -->"
		before       = "<!-- issuedog:error kind=transition key=20 -->"
		after        = "<!-- issuedog:error kind=disagreement key=21 -->"
		attempts     = "<!-- issuedog:error kind=attempts key=22 -->"
		owners       = "<!-- issuedog:error kind=labels key=23 --><!-- issuedog:transition from=spec-review to=planning cmd=31 -->"
		reply        = "<!-- issuedog:reply cmd=32 reason=rejected to=planning -->"
		nowhere      = "<!-- issuedog:transition to=nowhere cmd=33 -->"
	)
	s := snap("", []github.Author{appBot, appBot, appBot, appBot, appBot, appBot, owner, appBot, appBot, appBot},
		toClarifying, // 1
		before,       // 2
		toSpecReview, // 3
		after+before, // 4: the same error again keeps comment 2
		toPlanning,   // 5: dirty, so not the latest transition
		attempts,     // 6: dirty
		owners,       // 7: a trusted user's markers
		attempts,     // 8: posted again, after the dirty one
		reply,        // 9: not a transition
		nowhere)      // 10: a transition to no stage
	is := Derive(s, slug, dirtyIDs(5, 6))
	if is.Stage != SpecReview || is.TransitionComment != 3 {
		t.Errorf("stage %s from comment %d, want spec-review from comment 3", is.Stage, is.TransitionComment)
	}
	want := map[ErrorKey]int64{{"transition", "20"}: 2, {"disagreement", "21"}: 4, {"attempts", "22"}: 8}
	if !reflect.DeepEqual(is.Errors, want) {
		t.Errorf("errors = %v, want %v", is.Errors, want)
	}
	if b, a := is.Errors[ErrorKey{"transition", "20"}], is.Errors[ErrorKey{"disagreement", "21"}]; b >= is.TransitionComment || a <= is.TransitionComment {
		t.Errorf("errors in comments %d and %d, transition in %d; want the first before it and the second after", b, a, is.TransitionComment)
	}
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		dirty func(int64) bool
		want  int64
		stage Stage
	}{
		{"no transition", snap("", []github.Author{appBot}, before), clean, 0, Intake},
		{"a transition in the body", snap(toClarifying, []github.Author{appBot}, before), clean, BodyID, Clarifying},
		{"a comment's after the body's", snap(toClarifying, []github.Author{appBot, appBot}, before, toSpecReview), clean, 2, SpecReview},
		{"a dirty latest falls back to the one before", snap("", []github.Author{appBot, appBot}, toClarifying, toSpecReview), dirtyIDs(2), 1, Clarifying},
		{"a hold counts", snap("", []github.Author{appBot, appBot}, toClarifying,
			"<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=5 -->"), clean, 2, Held},
		{"a close counts", snap("", []github.Author{appBot, appBot}, toClarifying,
			"<!-- issuedog:transition from=clarifying to=closed cmd=5 -->"), clean, 2, Closed},
		{"its comment, not its seen", snap("", []github.Author{appBot, appBot, appBot}, toClarifying, "a question",
			"<!-- issuedog:transition from=clarifying to=spec-review run=b seen=2 -->"), clean, 3, SpecReview},
	} {
		if is := Derive(tt.s, slug, tt.dirty); is.TransitionComment != tt.want || is.Stage != tt.stage {
			t.Errorf("%s: stage %s from comment %d, want %s from comment %d", tt.name, is.Stage, is.TransitionComment, tt.stage, tt.want)
		}
	}
	// An error in the body, which the App doesn't post there, is still read.
	if errs := Derive(snap(before, nil), slug, clean).Errors; !reflect.DeepEqual(errs, map[ErrorKey]int64{{"transition", "20"}: BodyID}) {
		t.Errorf("errors = %v, want the body's", errs)
	}
	if errs := Derive(snap(before, nil), slug, dirtyIDs(BodyID)).Errors; len(errs) != 0 {
		t.Errorf("errors = %v, want none from a dirty body", errs)
	}
}

// An ack that names its kind makes Reported true with no clean error
// posted, as when the error comment went dirty after the ack: the error
// is never posted again (plan #7, assumption 8). It covers no other kind.
func TestErrAckReported(t *testing.T) {
	s := snap("", []github.Author{appBot, appBot},
		"<!-- issuedog:error kind=transition key=5 -->",
		"<!-- issuedog:error kind=transition key=5 ack=r9 -->")
	is := Derive(s, slug, dirtyIDs(1))
	if len(is.Errors) != 0 {
		t.Fatalf("errors = %v, want none: the error comment is dirty", is.Errors)
	}
	for _, tt := range []struct {
		kind, key string
		want      bool
	}{
		{"transition", "5", true},
		{"disagreement", "5", false},
		{"transition", "6", false},
	} {
		if got := is.Reported(tt.kind, tt.key); got != tt.want {
			t.Errorf("Reported(%s, %s) = %v, want %v", tt.kind, tt.key, got, tt.want)
		}
	}
}
