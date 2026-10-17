package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// discRecordedSnapshot is an issue whose first marker, in comment 10, is a
// discussion hold keyed by an edit of comment 5 at at(1), before it, and
// whose comment 11 is a notice of each of more. Comment 5 is gone since,
// and nothing else names the acts: no deletion, no rename.
func discRecordedSnapshot(at func(int) time.Time, more ...string) *github.Snapshot {
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	notices := ""
	for _, key := range more {
		notices += "<!-- issuedog:notice key=" + key + " -->"
	}
	return &github.Snapshot{Number: 1, CreatedAt: at(0), Labels: []string{"stage:held"}, Comments: []github.Comment{
		{ID: 10, Author: app, CreatedAt: at(10),
			Body: "<!-- issuedog:transition from=ready to=held at=ready origin=ready reason=discussion event=" + discEditKey(5, at(1)) + " -->"},
		{ID: 11, Author: app, Body: notices, CreatedAt: at(20)},
	}}
}

// Every key a marker records from the floor on is a trigger from its
// record alone, with no question, and no read but the reactions of the
// notice in comment 11, which may accept it (resolver.notice, T4):
// the edit the floor's own marker records, which is from before the floor,
// of content gone since; and a recorded comment, deletion and rename the
// snapshot no longer shows. A move out of held after them
// resolves them all.
func TestDiscFindRecordedFromTheFloor(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	s := discRecordedSnapshot(at, "c7", "dCDE_x", "77")
	d := Dirt{}
	reads := 0
	a := &Applier{Client: noReactions(t, &reads), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	tr := newDiscCounting()
	found, err := a.discFind(context.Background(), s, d, tr)
	if err != nil {
		t.Fatal(err)
	}
	want := []discTrigger{
		{Kind: discEdit, Key: "e5-1767225601", ID: 5, At: at(1), FromRecord: true},
		{Kind: discRename, Key: "77", At: at(20), FromRecord: true},
		{Kind: discComment, Key: "c7", ID: 7, At: at(20), FromRecord: true},
		{Kind: discDeletion, Key: "dCDE_x", At: at(20), FromRecord: true},
	}
	if !slices.Equal(found, want) {
		t.Errorf("found %+v;\nwant %+v", found, want)
	}
	if len(tr.asked) != 0 || len(tr.access) != 0 || reads != 1 {
		t.Errorf("asked %v and %v, with %d reads; want nobody asked about a recorded act, and one read of the notice's reactions",
			tr.asked, tr.access, reads)
	}
	s.Comments = append(s.Comments, github.Comment{ID: 12, Author: s.Comments[0].Author, CreatedAt: at(40),
		Body: "<!-- issuedog:transition from=held to=ready by=owner event=99 -->"})
	if found, err := a.discFind(context.Background(), s, d, tr); err != nil || len(found) != 0 {
		t.Errorf("after a move out of held, found %+v, %v; want none", found, err)
	}
}

// A recorded trigger the snapshot still shows as it was is found by its
// own pass, with what the snapshot says of it, and discRecorded adds no
// second one of it.
func TestDiscRecordedOnce(t *testing.T) {
	at := func(s int) time.Time { return time.Date(2026, 1, 1, 0, 0, s, 0, time.UTC) }
	s := discRecordedSnapshot(at, "c12")
	s.Comments = append(s.Comments, github.Comment{ID: 12, Author: github.Author{Login: "mallory", Type: "User"}, Body: "Wait.", CreatedAt: at(25)})
	a := &Applier{Client: noReactions(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}
	found, err := a.discFind(context.Background(), s, Dirt{}, newDiscCounting())
	if err != nil {
		t.Fatal(err)
	}
	want := []discTrigger{
		{Kind: discEdit, Key: "e5-1767225601", ID: 5, At: at(1), FromRecord: true},
		{Kind: discComment, Key: "c12", By: "mallory", ID: 12, At: at(25)},
	}
	if !slices.Equal(found, want) {
		t.Errorf("found %+v;\nwant %+v", found, want)
	}
}

// A key tells its trigger's kind, content and an edit's time; any other
// key is a trigger of no kind, at its record's time.
func TestDiscFromKey(t *testing.T) {
	rec := time.Date(2026, 1, 1, 0, 0, 9, 0, time.UTC)
	for _, tt := range []struct {
		key  string
		want discTrigger
	}{
		{"c12", discTrigger{Kind: discComment, ID: 12, At: rec}},
		{"e42-1767225606", discTrigger{Kind: discEdit, ID: 42, At: time.Unix(1767225606, 0).UTC()}},
		{"e0-1767225606", discTrigger{Kind: discEdit, ID: state.BodyID, At: time.Unix(1767225606, 0).UTC()}},
		{"dCDE_1", discTrigger{Kind: discDeletion, At: rec}},
		{"77", discTrigger{Kind: discRename, At: rec}},
		{"cx", discTrigger{At: rec}},
		{"e42", discTrigger{At: rec}},
		{"e42-x", discTrigger{At: rec}},
		{"ex-5", discTrigger{At: rec}},
		{"d", discTrigger{At: rec}},
		{"x9", discTrigger{At: rec}},
	} {
		tt.want.Key, tt.want.FromRecord = tt.key, true
		if got := discFromKey(tt.key, rec); got != tt.want {
			t.Errorf("discFromKey(%q) = %+v, want %+v", tt.key, got, tt.want)
		}
	}
}

// A trigger from its record says what its key tells, and that it was
// recorded earlier.
func TestDiscWhatRecorded(t *testing.T) {
	at := time.Unix(1767225606, 0)
	for _, tt := range []struct {
		tr   discTrigger
		want string
	}{
		{discTrigger{Kind: discComment, ID: 12}, "comment 12, which is gone now, recorded earlier"},
		{discTrigger{Kind: discEdit, ID: 42, At: at}, "an edit of comment 42 at 2026-01-01T00:00:06Z, recorded earlier"},
		{discTrigger{Kind: discEdit, ID: state.BodyID, At: at}, "an edit of the issue description at 2026-01-01T00:00:06Z, recorded earlier"},
		{discTrigger{Kind: discDeletion}, "a deletion of a comment, recorded earlier"},
		{discTrigger{Kind: discRename}, "a rename of this issue, recorded earlier"},
		{discTrigger{}, "an act on this issue, recorded earlier"},
	} {
		tt.tr.FromRecord = true
		if got := discWhat(tt.tr); got != tt.want {
			t.Errorf("discWhat(%+v) = %q, want %q", tt.tr, got, tt.want)
		}
	}
}
