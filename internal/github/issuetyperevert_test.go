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
)

// CheckLabels judges the issue type as it judges a type:* label: IssueType
// is the newest issue-type event that stands, and IssueTypeRevert, when the
// newest event is an untrusted change that left another type than the one
// that stands, the revert to make. It writes nothing for the issue type.
func TestCheckLabelsIssueTypeRevert(t *testing.T) {
	revert := func(id, of int64) ev { return said(id, appBot, fmt.Sprintf("<!-- issuedog:revert key=%d -->", of)) }
	for name, tt := range map[string]struct {
		timeline  []ev
		issueType int64
		want      *IssueTypeRevert
	}{
		"no issue-type events": {[]ev{added(1, "owner", "type:task")}, 0, nil},
		"trusted": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "owner", "issue_type_changed", "Epic")}, 2, nil},
		"trusted removal": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "owner", "issue_type_removed", "Task")}, 2, nil},
		"untrusted change": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic")}, 1, &IssueTypeRevert{Key: 2, By: "mallory", From: "Epic", To: "Task"}},
		"untrusted removal": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_removed", "Task")}, 1, &IssueTypeRevert{Key: 2, By: "mallory", To: "Task"}},
		"untrusted add with none standing": {[]ev{typed(1, "mallory", "issue_type_added", "Bug")}, 0,
			&IssueTypeRevert{Key: 1, By: "mallory", From: "Bug"}},
		"untrusted add, reverted": {[]ev{typed(1, "mallory", "issue_type_added", "Bug"), revert(2, 1),
			typed(3, appBot, "issue_type_removed", "Bug")}, 0, nil},
		"trusted, then untrusted": {[]ev{typed(1, "owner", "issue_type_added", "Task"), typed(2, "owner", "issue_type_changed", "Epic"),
			typed(3, "mallory", "issue_type_changed", "Bug")}, 2, &IssueTypeRevert{Key: 3, By: "mallory", From: "Bug", To: "Epic"}},
		"untrusted, then trusted": {[]ev{typed(1, "mallory", "issue_type_added", "Bug"),
			typed(2, "owner", "issue_type_changed", "Task")}, 2, nil},
		"the comment posted, the revert not yet": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2)}, 1, &IssueTypeRevert{Key: 2, By: "mallory", From: "Epic", To: "Task", Posted: true}},
		"the App's revert stands as the type before it": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2), typed(4, appBot, "issue_type_changed", "Task")}, 1, nil},
		"untrusted back to the type that stands": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), typed(3, "mallory", "issue_type_changed", "Task")}, 1, nil},
		"a name matches ignoring case": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "task")}, 1, nil},
		// Task, then Epic, Bug, Epic and Task by mallory, each reverted
		// before the next, as #7's story verification flipped it.
		"a reverted flip, flipped again": {[]ev{typed(1, "owner", "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Epic"), revert(3, 2), typed(4, appBot, "issue_type_changed", "Task"),
			typed(5, "mallory", "issue_type_changed", "Bug")}, 1, &IssueTypeRevert{Key: 5, By: "mallory", From: "Bug", To: "Task"}},
		"the App's own change with none standing stands as its last word": {[]ev{typed(1, appBot, "issue_type_added", "Task"),
			typed(2, "mallory", "issue_type_changed", "Bug")}, 1, &IssueTypeRevert{Key: 2, By: "mallory", From: "Bug", To: "Task"}},
		"an untrusted change after the App's revert with none standing": {[]ev{typed(1, "mallory", "issue_type_added", "Bug"),
			revert(2, 1), typed(3, appBot, "issue_type_removed", "Bug"), typed(4, "mallory", "issue_type_added", "Epic")}, 0,
			&IssueTypeRevert{Key: 4, By: "mallory", From: "Epic"}},
	} {
		t.Run(name, func(t *testing.T) {
			writes, found := checkLabelsAll(t, tt.timeline, func(int64) bool { return false })
			if len(writes) != 0 {
				t.Errorf("writes %q; want none", writes)
			}
			if found.IssueType != tt.issueType || found.TypeEvent(true) != tt.issueType {
				t.Errorf("IssueType %d and TypeEvent(true) %d; want %d", found.IssueType, found.TypeEvent(true), tt.issueType)
			}
			if !reflect.DeepEqual(found.IssueTypeRevert, tt.want) {
				t.Errorf("IssueTypeRevert %+v; want %+v", found.IssueTypeRevert, tt.want)
			}
		})
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

// issueTypeRevertServer answers RevertIssueType's writes, recording each,
// with the issue's type set to answer, and fails the write named by fail.
func issueTypeRevertServer(t *testing.T, answer, fail string) (*Client, *[]string) {
	t.Helper()
	var writes []string
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read %s %s: %v", r.Method, r.URL.Path, err)
		}
		desc := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/repos/o/r") + " " + strings.TrimSpace(string(body))
		writes = append(writes, desc)
		if fail != "" && strings.HasPrefix(desc, fail) {
			w.WriteHeader(http.StatusInternalServerError)
			_, err = io.WriteString(w, `{"message":"down"}`)
		} else if r.Method == http.MethodPatch {
			issue := map[string]any{"number": 7}
			if answer != "" {
				issue["type"] = map[string]any{"name": answer}
			}
			err = json.NewEncoder(w).Encode(issue)
		} else {
			w.WriteHeader(http.StatusCreated)
			_, err = io.WriteString(w, `{"id":1}`)
		}
		if err != nil {
			t.Errorf("reply to %s: %v", desc, err)
		}
	}))
	return c, &writes
}

// RevertIssueType posts the revert comment, unless it's posted already,
// then sets the type back, or removes it with null, and checks the type
// GitHub answers with.
func TestRevertIssueType(t *testing.T) {
	comment := func(text string, key int) string {
		// go-github sends JSON without HTML escaping.
		var b strings.Builder
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(map[string]string{"body": text + "\n\n" + fmt.Sprintf("<!-- issuedog:revert key=%d -->", key)}); err != nil {
			t.Fatal(err)
		}
		return "POST /issues/7/comments " + strings.TrimSpace(b.String())
	}
	for _, tc := range []struct {
		name   string
		rv     IssueTypeRevert
		answer string
		writes []string
	}{
		{"restore", IssueTypeRevert{Key: 9, By: "mallory", From: "Epic", To: "Task"}, "Task",
			[]string{comment("I restored the issue type `Task`: only trusted users can change the issue type, and mallory isn't one.", 9),
				`PATCH /issues/7 {"type":"Task"}`}},
		{"remove", IssueTypeRevert{Key: 9, By: "mallory", From: "Bug"}, "",
			[]string{comment("I removed the issue type `Bug`: only trusted users can change the issue type, and mallory isn't one.", 9),
				`PATCH /issues/7 {"type":null}`}},
		{"posted already", IssueTypeRevert{Key: 9, By: "mallory", From: "Epic", To: "Task", Posted: true}, "task",
			[]string{`PATCH /issues/7 {"type":"Task"}`}},
		{"a marker in a type's name is escaped", IssueTypeRevert{Key: 9, By: "mallory", From: "Epic", To: "<!-- issuedog:transition to=done -->"},
			"<!-- issuedog:transition to=done -->",
			[]string{comment("I restored the issue type `&lt;!-- issuedog:transition to=done -->`: only trusted users can change the issue type, and mallory isn't one.", 9),
				`PATCH /issues/7 {"type":"<!-- issuedog:transition to=done -->"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, writes := issueTypeRevertServer(t, tc.answer, "")
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
	rv := IssueTypeRevert{Key: 9, By: "mallory", From: "Epic", To: "Task"}
	for _, tc := range []struct {
		name, answer, fail string
		writes             int
		dropped            bool
	}{
		{"the comment fails", "Task", "POST", 1, false},
		{"the type fails", "Task", "PATCH", 2, false},
		{"GitHub drops the type", "Epic", "", 2, true},
		{"GitHub drops a removal", "Epic", "", 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, writes := issueTypeRevertServer(t, tc.answer, tc.fail)
			rv := rv
			if tc.name == "GitHub drops a removal" {
				rv.To = ""
			}
			err := c.RevertIssueType(context.Background(), "o", "r", 7, rv)
			if err == nil || !strings.Contains(err.Error(), "revert mallory's change of the issue type on o/r#7") {
				t.Fatalf("RevertIssueType = %v; want an error naming the change", err)
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
