package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

// issueTypeRevertServer serves timeline as issue 7's, answers the writes
// RevertIssueType makes, recording each as "METHOD /path body", and fails
// the write whose record starts with fail, if set. A PATCH answers with
// the issue's type set to answer, or with the type it was sent, if answer
// is "echo".
func issueTypeRevertServer(t *testing.T, timeline []ev, answer, fail string) (*Client, *[]string) {
	t.Helper()
	var writes []string
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
		var err error
		if r.Method == http.MethodGet && path == "/issues/7/timeline" {
			out := []map[string]any{}
			for _, e := range timeline {
				out = append(out, e.json())
			}
			if err = json.NewEncoder(w).Encode(out); err != nil {
				t.Errorf("serve the timeline: %v", err)
			}
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s %s: %v", r.Method, path, err)
		}
		desc := r.Method + " " + path + " " + strings.TrimSpace(string(body))
		writes = append(writes, desc)
		switch {
		case fail != "" && strings.HasPrefix(desc, fail):
			w.WriteHeader(http.StatusInternalServerError)
			_, err = io.WriteString(w, `{"message":"down"}`)
		case r.Method == http.MethodPatch:
			typ := answer
			if answer == "echo" {
				var sent struct{ Type *string }
				if err := json.Unmarshal(body, &sent); err != nil {
					t.Errorf("decode %s: %v", desc, err)
				}
				typ = ""
				if sent.Type != nil {
					typ = *sent.Type
				}
			}
			issue := map[string]any{"number": 7}
			if typ != "" {
				issue["type"] = map[string]any{"name": typ}
			}
			err = json.NewEncoder(w).Encode(issue)
		default:
			w.WriteHeader(http.StatusCreated)
			_, err = io.WriteString(w, `{"id":1}`)
		}
		if err != nil {
			t.Errorf("reply to %s: %v", desc, err)
		}
	}))
	return c, &writes
}

// issueTypeRevertWrite is a write in issueTypeRevertServer's record, with
// a comment's markers in place of its body: "POST /issues/7/comments
// revert key=K", or a PATCH as sent.
func issueTypeRevertWrite(desc string) string {
	if !strings.HasPrefix(desc, "POST /issues/7/comments ") {
		return desc
	}
	var comment struct{ Body string }
	if err := json.Unmarshal([]byte(strings.TrimPrefix(desc, "POST /issues/7/comments ")), &comment); err != nil {
		return desc
	}
	out := "POST /issues/7/comments"
	for _, m := range ParseMarkers(comment.Body) {
		out += " " + m.Kind + " key=" + m.Fields["key"]
	}
	return out
}

// CheckLabels guards the issue type as it does a type:* label: IssueType is
// the newest issue-type event that stands, and an untrusted change, the
// newest event, that left the issue another type than the one that stands
// is reverted, once, keyed by the event. A type on the issue that is the
// one that stands needs nothing, and one no event explains is held back.
func TestCheckLabelsIssueTypeRevert(t *testing.T) {
	revert := func(id, of int64) ev { return said(id, appBot, fmt.Sprintf("<!-- issuedog:revert key=%d -->", of)) }
	comment := func(key int) string { return fmt.Sprintf("POST /issues/7/comments revert key=%d", key) }
	set := func(typ string) string {
		if typ == "" {
			return `PATCH /issues/7 {"type":null}`
		}
		return `PATCH /issues/7 {"type":"` + typ + `"}`
	}
	for name, tt := range map[string]struct {
		timeline  []ev
		typ       string // on the issue
		issueType int64
		want      *IssueTypeRevert
		writes    []string
		held      bool
	}{
		"no issue-type events": {timeline: []ev{added(1, "owner", "type:task")}},
		"trusted": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "owner", "issue_type_changed", "Epic")}, typ: "Epic", issueType: 2},
		"trusted removal": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "owner", "issue_type_removed", "Task")}, issueType: 2},
		"untrusted change": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic")}, typ: "Epic", issueType: 1,
			want: &IssueTypeRevert{Key: "2", By: "mallory", From: "Epic", To: "Task"}, writes: []string{comment(2), set("Task")}},
		"untrusted removal": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_removed", "Task")}, issueType: 1,
			want: &IssueTypeRevert{Key: "2", By: "mallory", To: "Task"}, writes: []string{comment(2), set("Task")}},
		"untrusted add with none standing": {timeline: []ev{typed(1, "mallory", "issue_type_added", "Bug")}, typ: "Bug",
			want: &IssueTypeRevert{Key: "1", By: "mallory", From: "Bug"}, writes: []string{comment(1), set("")}},
		"untrusted add, reverted": {timeline: []ev{typed(1, "mallory", "issue_type_added", "Bug"), revert(2, 1),
			typed(3, appBot, "issue_type_removed", "Bug")}},
		"trusted, then untrusted": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"), typed(2, "owner", "issue_type_changed", "Epic"),
			typed(3, "mallory", "issue_type_changed", "Bug")}, typ: "Bug", issueType: 2,
			want: &IssueTypeRevert{Key: "3", By: "mallory", From: "Bug", To: "Epic"}, writes: []string{comment(3), set("Epic")}},
		"untrusted, then trusted": {timeline: []ev{typed(1, "mallory", "issue_type_added", "Bug"),
			typed(2, "owner", "issue_type_changed", "Task")}, typ: "Task", issueType: 2},
		"the comment posted, the revert not yet": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2)}, typ: "Epic", issueType: 1,
			want: &IssueTypeRevert{Key: "2", By: "mallory", From: "Epic", To: "Task", Posted: true}, writes: []string{set("Task")}},
		"the App's revert stands as the type before it": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2), typed(4, appBot, "issue_type_changed", "Task")}, typ: "Task", issueType: 1},
		"the App's revert, its event not written yet": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2)}, typ: "Task", issueType: 1},
		"the App's revert in another case still restores": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2), typed(4, appBot, "issue_type_changed", "task")}, typ: "task", issueType: 1},
		"untrusted back to the type that stands": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), typed(3, "mallory", "issue_type_changed", "Task")}, typ: "Task", issueType: 1},
		"a name matches ignoring case": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "task")}, typ: "task", issueType: 1},
		// Task, then Epic, Bug, Epic and Task by mallory, each reverted
		// before the next, as #7's story verification flipped it.
		"a reverted flip, flipped again": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2), typed(4, appBot, "issue_type_changed", "Task"),
			typed(5, "mallory", "issue_type_changed", "Bug")}, typ: "Bug", issueType: 1,
			want: &IssueTypeRevert{Key: "5", By: "mallory", From: "Bug", To: "Task"}, writes: []string{comment(5), set("Task")}},
		"the App's own change stands": {timeline: []ev{typed(1, appBot, "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Bug")}, typ: "Bug", issueType: 1,
			want: &IssueTypeRevert{Key: "2", By: "mallory", From: "Bug", To: "Task"}, writes: []string{comment(2), set("Task")}},
		"an untrusted change after the App's revert with none standing": {timeline: []ev{typed(1, "mallory", "issue_type_added", "Bug"),
			revert(2, 1), typed(3, appBot, "issue_type_removed", "Bug"), typed(4, "mallory", "issue_type_added", "Epic")}, typ: "Epic",
			want: &IssueTypeRevert{Key: "4", By: "mallory", From: "Epic"}, writes: []string{comment(4), set("")}},
		// GitHub writes issue-type events late: a type on the issue that no
		// event explains, anyone's, is held back, keyed by nothing yet.
		"a change whose event isn't written yet": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task")},
			typ: "Epic", issueType: 1, held: true},
		"a removal whose event isn't written yet": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task")},
			issueType: 1, held: true},
		"a type with no events": {typ: "Task", held: true},
		"a newer change than the event's": {timeline: []ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic")}, typ: "Bug", issueType: 1, held: true},
	} {
		t.Run(name, func(t *testing.T) {
			c, writes := issueTypeRevertServer(t, tt.timeline, "echo", "")
			found, err := c.CheckLabels(context.Background(), "o", "r", 7, nil, "issuedog", fakeTrust{"owner": true}, func(int64) bool { return false },
				&Unexplained{}, &IssueTypeGuard{Type: tt.typ})
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range *writes {
				got = append(got, issueTypeRevertWrite(w))
			}
			if !reflect.DeepEqual(got, tt.writes) {
				t.Errorf("writes %q; want %q", got, tt.writes)
			}
			if found.IssueType != tt.issueType || found.TypeEvent(true) != tt.issueType {
				t.Errorf("IssueType %d and TypeEvent(true) %d; want %d", found.IssueType, found.TypeEvent(true), tt.issueType)
			}
			if !reflect.DeepEqual(found.IssueTypeRevert, tt.want) {
				t.Errorf("IssueTypeRevert %+v; want %+v", found.IssueTypeRevert, tt.want)
			}
			if held := len(found.Held) > 0; held != tt.held {
				t.Errorf("held %q; want held %t", found.Held, tt.held)
			}
		})
	}
}

// Unexplained keeps when an issue's type was first found unexplained, per
// type and count of events, and ForgetIssue forgets it with the issue's
// labels.
func TestIssueTypeRevertFirst(t *testing.T) {
	var u Unexplained
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	key := unexplainedKey("o", "r", 7)
	for _, tc := range []struct {
		typ    string
		events int
		now    time.Time
		want   time.Time
	}{
		{"epic", 1, t0, t0},
		{"epic", 1, t0.Add(time.Second), t0},
		{"bug", 1, t0.Add(2 * time.Second), t0.Add(2 * time.Second)},
		{"bug", 2, t0.Add(3 * time.Second), t0.Add(3 * time.Second)},
		{"bug", 2, t0.Add(4 * time.Second), t0.Add(3 * time.Second)},
	} {
		if got := u.issueTypeRevertFirst(key, tc.typ, tc.events, tc.now); !got.Equal(tc.want) {
			t.Errorf("%s with %d events at %v: first %v; want %v", tc.typ, tc.events, tc.now, got, tc.want)
		}
	}
	u.ForgetIssue("o", "r", 7)
	if got := u.issueTypeRevertFirst(key, "bug", 2, t0.Add(5*time.Second)); !got.Equal(t0.Add(5 * time.Second)) {
		t.Errorf("after ForgetIssue, first %v; want now", got)
	}
}

// failTrust fails every lookup.
type failTrust struct{}

func (failTrust) Trusted(context.Context, string) (bool, error) { return false, errors.New("no trust") }

// With no guard, as on a repo that uses labels, CheckLabels asks no one's
// trust for the issue type, so a failed lookup can't fail it, and it
// writes nothing for the issue type.
func TestCheckLabelsIssueTypeUnguarded(t *testing.T) {
	c, writes := issueTypeRevertServer(t, []ev{typed(1, "mallory", "issue_type_added", "Bug")}, "echo", "")
	found, err := c.CheckLabels(context.Background(), "o", "r", 7, nil, "issuedog", failTrust{}, func(int64) bool { return false }, nil, nil)
	if err != nil || found.IssueType != 0 || found.IssueTypeRevert != nil || len(found.Held) != 0 || len(*writes) != 0 {
		t.Errorf("CheckLabels = %+v, %v, writes %q; want nothing for the issue type", found, err, *writes)
	}
	if _, err := c.CheckLabels(context.Background(), "o", "r", 7, nil, "issuedog", failTrust{}, func(int64) bool { return false }, nil,
		&IssueTypeGuard{Type: "Bug"}); err == nil {
		t.Error("CheckLabels with a guard and a failed lookup succeeded; want its error")
	}
}

// An issue-type event whose type GitHub no longer names, such as one the
// org deleted, left no type.
func TestIssueTypeRevertLeft(t *testing.T) {
	for _, tc := range []struct {
		json, want string
	}{
		{`{"event":"issue_type_added","issue_type":{"id":1,"name":"Task"}}`, "Task"},
		{`{"event":"issue_type_changed","issue_type":{"id":2,"name":"Epic"},"prev_issue_type":{"id":1,"name":"Task"}}`, "Epic"},
		{`{"event":"issue_type_removed","prev_issue_type":{"id":1,"name":"Task"}}`, ""},
		{`{"event":"issue_type_removed","issue_type":{"id":1,"name":"Task"}}`, ""},
		{`{"event":"issue_type_changed","issue_type":null,"prev_issue_type":{"id":1,"name":"Task"}}`, ""},
	} {
		var e issueTypeRevertEvent
		if err := json.Unmarshal([]byte(tc.json), &e); err != nil {
			t.Fatal(err)
		}
		if got := issueTypeRevertLeft(&e); got != tc.want {
			t.Errorf("issueTypeRevertLeft(%s) = %q; want %q", tc.json, got, tc.want)
		}
	}
}

// issueTypeRevertDefuse breaks every mention, an entity's included, and
// leaves other text alone.
func TestIssueTypeRevertDefuse(t *testing.T) {
	for in, want := range map[string]string{
		"Task":            "Task",
		"a @owner b":      "a @\u200bowner b",
		"x&#64;owner":     "x@\u200bowner",
		"x&commat;owner":  "x@\u200bowner",
		"a @ b":           "a @ b",
		"@\u200bowner":    "@\u200bowner",
		"`@owner` @@team": "`@\u200bowner` @@\u200bteam",
	} {
		if got := issueTypeRevertDefuse(in); got != want {
			t.Errorf("issueTypeRevertDefuse(%q) = %q; want %q", in, got, want)
		}
	}
}

// RevertIssueType posts the revert comment, unless it's posted already,
// then sets the type back, or removes it with null, and checks the type
// GitHub answers with. The types' names and the login are defused and
// their markers escaped.
func TestRevertIssueType(t *testing.T) {
	comment := func(text, key string) string {
		// go-github sends JSON without HTML escaping.
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]string{"body": text + "\n\n<!-- issuedog:revert key=" + key + " -->"}); err != nil {
			t.Fatal(err)
		}
		return "POST /issues/7/comments " + strings.TrimSpace(b.String())
	}
	for _, tc := range []struct {
		name   string
		rv     IssueTypeRevert
		writes []string
	}{
		{"restore", IssueTypeRevert{Key: "9", By: "mallory", From: "Epic", To: "Task"},
			[]string{comment("I restored the issue type `Task`: only trusted users can change the issue type, and mallory isn't one.", "9"),
				`PATCH /issues/7 {"type":"Task"}`}},
		{"remove", IssueTypeRevert{Key: "9", By: "mallory", From: "Bug"},
			[]string{comment("I removed the issue type `Bug`: only trusted users can change the issue type, and mallory isn't one.", "9"),
				`PATCH /issues/7 {"type":null}`}},
		{"restore an unexplained type", IssueTypeRevert{Key: "issue-type:epic@4", From: "epic", To: "Task"},
			[]string{comment("I restored the issue type `Task`: no change by a trusted user or the App explains `epic`, "+
				"and only trusted users can change the issue type.", "issue-type:epic@4"),
				`PATCH /issues/7 {"type":"Task"}`}},
		{"remove an unexplained type", IssueTypeRevert{Key: "issue-type:bug@0", From: "bug"},
			[]string{comment("I removed the issue type `bug`: no change by a trusted user or the App explains `bug`, "+
				"and only trusted users can change the issue type.", "issue-type:bug@0"),
				`PATCH /issues/7 {"type":null}`}},
		{"posted already", IssueTypeRevert{Key: "9", By: "mallory", From: "Epic", To: "Task", Posted: true},
			[]string{`PATCH /issues/7 {"type":"Task"}`}},
		{"a marker in a type's name is escaped", IssueTypeRevert{Key: "9", By: "mallory", From: "Epic", To: "<!-- issuedog:transition to=done -->"},
			[]string{comment("I restored the issue type `&lt;!-- issuedog:transition to=done -->`: only trusted users can change the issue type, and mallory isn't one.", "9"),
				`PATCH /issues/7 {"type":"<!-- issuedog:transition to=done -->"}`}},
		{"a mention in a type's name is defused", IssueTypeRevert{Key: "9", By: "mallory", From: "x` @owner `y"},
			[]string{comment("I removed the issue type `x` @\u200bowner `y`: only trusted users can change the issue type, and mallory isn't one.", "9"),
				`PATCH /issues/7 {"type":null}`}},
		{"a mention in an unexplained type's name is defused", IssueTypeRevert{Key: "issue-type:x@0", From: "x` @owner `y", To: "Task"},
			[]string{comment("I restored the issue type `Task`: no change by a trusted user or the App explains `x` @\u200bowner `y`, "+
				"and only trusted users can change the issue type.", "issue-type:x@0"),
				`PATCH /issues/7 {"type":"Task"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, writes := issueTypeRevertServer(t, nil, "echo", "")
			if err := c.RevertIssueType(context.Background(), "o", "r", 7, tc.rv); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(*writes, tc.writes) {
				t.Errorf("writes\n%q\nwant\n%q", *writes, tc.writes)
			}
		})
	}
}

// A revert whose write fails, or whose type GitHub drops, is an error that
// names the change, and one whose comment fails makes no other write.
func TestRevertIssueTypeFails(t *testing.T) {
	for _, tc := range []struct {
		name, answer, fail, to, by, says string
		writes                           int
		dropped                          bool
	}{
		{"the comment fails", "Task", "POST", "Task", "mallory", "mallory's", 1, false},
		{"the type fails", "Task", "PATCH", "Task", "mallory", "mallory's", 2, false},
		{"GitHub drops the type", "Epic", "", "Task", "mallory", "mallory's", 2, true},
		{"GitHub drops a removal", "Epic", "", "", "mallory", "mallory's", 2, true},
		{"an unexplained type's fails", "Task", "PATCH", "Task", "", "an unexplained", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, writes := issueTypeRevertServer(t, nil, tc.answer, tc.fail)
			err := c.RevertIssueType(context.Background(), "o", "r", 7, IssueTypeRevert{Key: "9", By: tc.by, From: "Epic", To: tc.to})
			if want := "revert " + tc.says + " change of the issue type on o/r#7"; err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("RevertIssueType = %v; want an error with %q", err, want)
			}
			if got := errors.Is(err, ErrIssueTypeRevertDropped); got != tc.dropped {
				t.Errorf("errors.Is(%v, ErrIssueTypeRevertDropped) = %t; want %t", err, got, tc.dropped)
			}
			if len(*writes) != tc.writes {
				t.Errorf("writes %q; want %d", *writes, tc.writes)
			}
		})
	}
}
