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
	} {
		tt.s.Type, tt.s.Parent = "task", 3
		tt.want.Type, tt.want.HasParent = "task", true
		if got := Derive(tt.s, slug, tt.dirty).Item; got != tt.want {
			t.Errorf("%s: item = %+v, want %+v", tt.name, got, tt.want)
		}
	}
}
