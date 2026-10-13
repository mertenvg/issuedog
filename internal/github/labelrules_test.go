package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/trust"
)

var _ Trust = (*trust.Set)(nil)

type fakeTrust map[string]bool

func (f fakeTrust) Trusted(_ context.Context, login string) (bool, error) { return f[login], nil }

// ev is one timeline event: text is the label of a label change, or a comment body.
type ev struct {
	id               int64
	event, who, text string
}

func (e ev) json() map[string]any {
	user := map[string]any{"login": e.who, "type": "User"}
	if strings.HasSuffix(e.who, "[bot]") {
		user["type"] = "Bot"
	}
	m := map[string]any{"id": e.id, "event": e.event, "created_at": fmt.Sprintf("2026-10-01T00:00:%02dZ", e.id)}
	if e.event == "commented" {
		m["user"], m["body"] = user, e.text
	} else {
		m["actor"], m["label"] = user, map[string]any{"name": e.text}
	}
	return m
}

const appBot = "issuedog[bot]"

func added(id int64, who, label string) ev   { return ev{id, "labeled", who, label} }
func removed(id int64, who, label string) ev { return ev{id, "unlabeled", who, label} }
func said(id int64, who, body string) ev     { return ev{id, "commented", who, body} }

func TestCheckLabels(t *testing.T) {
	marker := func(to string) string { return "<!-- issuedog:transition from=x to=" + to + " -->" }
	tests := []struct {
		name       string
		timeline   []ev
		wantWrites []string
		wantEvents []LabelEvent
	}{
		{"untrusted add is reverted with a comment",
			[]ev{added(1, "mallory", "approved")},
			[]string{"POST /issues/7/comments revert key=1", "DELETE /issues/7/labels/approved"}, nil},
		{"replayed revert does not comment twice",
			[]ev{added(1, "mallory", "approved"), said(2, appBot, "<!-- issuedog:revert key=1 -->")},
			[]string{"DELETE /issues/7/labels/approved"}, nil},
		{"untrusted remove is restored",
			[]ev{added(1, appBot, "type:task"), removed(2, "mallory", "type:task")},
			[]string{"POST /issues/7/comments revert key=2", `POST /issues/7/labels ["type:task"]`}, nil},
		{"trusted manual stage change is an override",
			[]ev{added(1, appBot, "stage:ready"), said(2, appBot, marker("ready")),
				removed(3, "owner", "stage:ready"), added(4, "owner", "stage:held")},
			nil, []LabelEvent{{Kind: LabelOverride, Stages: []string{"stage:held"}, Marker: "ready", By: "owner", Label: "stage:held", Key: 4}}},
		{"untrusted removal after a trusted override keeps the override",
			[]ev{added(1, appBot, "stage:ready"), said(2, appBot, marker("ready")),
				removed(3, "owner", "stage:ready"), added(4, "owner", "stage:held"), removed(5, "mallory", "stage:held")},
			[]string{"POST /issues/7/comments revert key=5", `POST /issues/7/labels ["stage:held"]`},
			[]LabelEvent{{Kind: LabelOverride, Stages: []string{"stage:held"}, Marker: "ready", By: "owner", Label: "stage:held", Key: 4}}},
		{"the App's own revert doesn't hide an override that S4 hasn't recorded yet",
			[]ev{added(1, appBot, "stage:ready"), said(2, appBot, marker("ready")),
				removed(3, "owner", "stage:ready"), added(4, "owner", "stage:held"), removed(5, "mallory", "stage:held"),
				said(6, appBot, "<!-- issuedog:revert key=5 -->"), added(7, appBot, "stage:held")},
			nil, []LabelEvent{{Kind: LabelOverride, Stages: []string{"stage:held"}, Marker: "ready", By: "owner", Label: "stage:held", Key: 4}}},
		{"a marker in a label name is escaped in the revert comment",
			[]ev{added(1, "mallory", "scope:<!--issuedog:transition to=done-->")},
			[]string{"POST /issues/7/comments revert key=1", "DELETE /issues/7/labels/scope:<!--issuedog:transition to=done-->"}, nil},
		{"untrusted add of a model label is reverted",
			[]ev{added(1, "mallory", "model:opus")},
			[]string{"POST /issues/7/comments revert key=1", "DELETE /issues/7/labels/model:opus"}, nil},
		{"untrusted removal of a model label is restored",
			[]ev{added(1, appBot, "model:opus"), removed(2, "mallory", "model:opus")},
			[]string{"POST /issues/7/comments revert key=2", `POST /issues/7/labels ["model:opus"]`}, nil},
		{"untrusted add of a verifier model label is reverted",
			[]ev{added(1, "mallory", "Verifier-Model:opus")},
			[]string{"POST /issues/7/comments revert key=1", "DELETE /issues/7/labels/Verifier-Model:opus"}, nil},
		{"untrusted add of an effort label is reverted",
			[]ev{added(1, "mallory", "effort:max")},
			[]string{"POST /issues/7/comments revert key=1", "DELETE /issues/7/labels/effort:max"}, nil},
		{"untrusted removal of a verifier effort label is restored",
			[]ev{added(1, "owner", "verifier-effort:low"), removed(2, "mallory", "verifier-effort:low")},
			[]string{"POST /issues/7/comments revert key=2", `POST /issues/7/labels ["verifier-effort:low"]`}, nil},
		{"trusted model and effort changes stand",
			[]ev{added(1, appBot, "model:opus"), removed(2, "owner", "model:opus"), added(3, "owner", "model:sonnet"),
				added(4, "owner", "verifier-model:opus"), added(5, "owner", "effort:max"), added(6, "owner", "verifier-effort:low")},
			nil, nil},
		{"recorded override is not reported again",
			[]ev{added(1, "owner", "stage:held"), said(2, appBot, marker("held"))}, nil, nil},
		{"change by the App is accepted",
			[]ev{added(1, appBot, "stage:ready"), added(2, appBot, "priority:p1"), said(3, appBot, marker("ready"))},
			nil, nil},
		{"stage that disagrees with the marker",
			[]ev{added(1, appBot, "stage:ready"), said(2, appBot, marker("ready")), said(3, "owner", marker("held")),
				removed(4, appBot, "stage:ready"), added(5, appBot, "stage:verifying")},
			nil, []LabelEvent{{Kind: LabelDisagreement, Stages: []string{"stage:verifying"}, Marker: "ready", Key: 5}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writes, got := checkLabels(t, tt.timeline, func(int64) bool { return false })
			if !reflect.DeepEqual(writes, tt.wantWrites) {
				t.Errorf("writes = %q, want %q", writes, tt.wantWrites)
			}
			if !reflect.DeepEqual(got, tt.wantEvents) {
				t.Errorf("events = %+v, want %+v", got, tt.wantEvents)
			}
		})
	}
}

func TestPipelineLabel(t *testing.T) {
	for _, name := range []string{"stage:ready", "priority:p1", "scope:x", "type:task", "approved",
		"model:opus", "verifier-model:opus", "effort:max", "verifier-effort:low", "Model:Opus", "VERIFIER-EFFORT:LOW"} {
		if !pipelineLabel(name) {
			t.Errorf("pipelineLabel(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"review:high-risk", "models:opus", "efforts:max", "verifier:opus", "bug"} {
		if pipelineLabel(name) {
			t.Errorf("pipelineLabel(%q) = true, want false", name)
		}
	}
}

func TestCheckLabelsDisagreement(t *testing.T) {
	marker := func(to string) string { return "<!-- issuedog:transition from=x to=" + to + " -->" }
	// then appends evs to a stage:ready label and a marker that agree.
	then := func(evs ...ev) []ev {
		return append([]ev{added(1, appBot, "stage:ready"), said(2, appBot, marker("ready"))}, evs...)
	}
	tests := []struct {
		name     string
		timeline []ev
		dirty    int64 // a comment that dirty reports
		want     LabelEvent
	}{
		{name: "a marker with no label change since is unfinished",
			timeline: then(said(3, appBot, marker("verifying"))),
			want:     LabelEvent{Kind: LabelDisagreement, Stages: []string{"stage:ready"}, Marker: "verifying", Key: 1, Unfinished: true}},
		{name: "the App's own add toward the marker leaves it unfinished",
			timeline: then(said(3, appBot, marker("verifying")), added(4, appBot, "stage:verifying")),
			want:     LabelEvent{Kind: LabelDisagreement, Stages: []string{"stage:ready", "stage:verifying"}, Marker: "verifying", Key: 1, Unfinished: true}},
		{name: "a trusted removal after the marker is reported",
			timeline: then(removed(3, "owner", "stage:ready")),
			want:     LabelEvent{Kind: LabelDisagreement, Marker: "ready", Key: 3}},
		{name: "a marker in a dirty comment is skipped",
			timeline: then(said(3, appBot, marker("verifying")), added(4, appBot, "stage:verifying"), removed(5, appBot, "stage:ready")),
			dirty:    3,
			want:     LabelEvent{Kind: LabelDisagreement, Stages: []string{"stage:verifying"}, Marker: "ready", Key: 5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writes, got := checkLabels(t, tt.timeline, func(id int64) bool { return id == tt.dirty })
			if len(writes) != 0 {
				t.Errorf("writes = %q, want none", writes)
			}
			if want := []LabelEvent{tt.want}; !reflect.DeepEqual(got, want) {
				t.Errorf("events = %+v, want %+v", got, want)
			}
		})
	}
}

// checkLabels runs CheckLabels on issue 7 with this timeline and "owner"
// trusted. It returns the writes made, each with its body or marker keys,
// and the events.
func checkLabels(t *testing.T, timeline []ev, dirty func(int64) bool) ([]string, []LabelEvent) {
	t.Helper()
	var writes []string
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
		if r.Method == http.MethodGet && path == "/issues/7/timeline" {
			var out []map[string]any
			for _, e := range timeline {
				out = append(out, e.json())
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		desc, body := r.Method+" "+path, readAll(r.Body)
		var comment struct{ Body string }
		if json.Unmarshal(body, &comment) == nil && comment.Body != "" {
			for _, m := range ParseMarkers(comment.Body) {
				desc += " " + m.Kind + " key=" + m.Fields["key"]
			}
		} else if len(body) > 0 {
			desc += " " + strings.TrimSpace(string(body))
		}
		writes = append(writes, desc)
		_, _ = io.WriteString(w, map[bool]string{true: "[]", false: "{}"}[strings.HasSuffix(path, "/labels")])
	}))
	_, got, err := c.CheckLabels(context.Background(), "o", "r", 7, nil, "issuedog", fakeTrust{"owner": true}, dirty)
	if err != nil {
		t.Fatal(err)
	}
	return writes, got
}

func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}
