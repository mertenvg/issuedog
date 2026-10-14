package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/logr/v2"

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

// CheckLabels' settings is the newest change to a model or effort label
// that stands: a trusted user's or the App's, but no untrusted change, and
// no App revert of one.
func TestCheckLabelsSettings(t *testing.T) {
	revert := func(id, of int64) ev { return said(id, appBot, fmt.Sprintf("<!-- issuedog:revert key=%d -->", of)) }
	for name, tt := range map[string]struct {
		timeline []ev
		want     int64
	}{
		"none":                        {[]ev{added(1, "owner", "priority:p1"), added(2, appBot, "stage:ready")}, 0},
		"trusted adds":                {[]ev{added(1, "owner", "model:opus"), added(2, "owner", "Model:Sonnet")}, 2},
		"trusted removal":             {[]ev{added(1, "owner", "model:opus"), added(2, "owner", "model:sonnet"), removed(3, "owner", "model:sonnet")}, 3},
		"verifier-effort":             {[]ev{added(1, "owner", "model:opus"), added(2, "owner", "verifier-effort:max")}, 2},
		"the App's own change":        {[]ev{added(1, "owner", "model:opus"), added(2, appBot, "model:sonnet"), removed(3, appBot, "model:opus")}, 3},
		"other labels after":          {[]ev{added(1, "owner", "model:opus"), added(2, "owner", "priority:p1"), removed(3, "owner", "priority:p1")}, 1},
		"untrusted add, reverted":     {[]ev{added(1, "owner", "model:opus"), added(2, "mallory", "effort:low"), revert(3, 2), removed(4, appBot, "effort:low")}, 1},
		"untrusted removal, reverted": {[]ev{added(1, "owner", "model:opus"), removed(2, "mallory", "model:opus"), revert(3, 2), added(4, appBot, "model:opus")}, 1},
		"untrusted add, not yet":      {[]ev{added(1, "owner", "model:opus"), added(2, "mallory", "effort:low")}, 1},
		"untrusted only":              {[]ev{added(1, "mallory", "model:opus"), revert(2, 1), removed(3, appBot, "model:opus")}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, got := checkLabelsAll(t, tt.timeline, func(int64) bool { return false }); got != tt.want {
				t.Errorf("settings = %d, want %d", got, tt.want)
			}
		})
	}
}

// CheckLabels gives settings with an override or a disagreement too.
func TestCheckLabelsSettingsWithEvents(t *testing.T) {
	marker := said(2, appBot, "<!-- issuedog:transition from=x to=ready -->")
	for kind, timeline := range map[LabelEventKind][]ev{
		LabelOverride:     {added(1, appBot, "stage:ready"), marker, added(3, "owner", "model:opus"), removed(4, "owner", "stage:ready"), added(5, "owner", "stage:held")},
		LabelDisagreement: {added(1, appBot, "stage:ready"), marker, added(3, "owner", "model:opus"), removed(4, "owner", "stage:ready")},
	} {
		t.Run(string(kind), func(t *testing.T) {
			_, events, settings := checkLabelsAll(t, timeline, func(int64) bool { return false })
			if len(events) != 1 || events[0].Kind != kind || settings != 3 {
				t.Errorf("events %+v and settings %d; want one %s, and settings 3, the model label's", events, settings, kind)
			}
		})
	}
}

func TestOverrideLabel(t *testing.T) {
	for name, want := range map[string]bool{"model:opus": true, "Verifier-Model:opus": true, "effort:max": true, "VERIFIER-EFFORT:LOW": true,
		"models:opus": false, "stage:ready": false, "priority:p1": false, "approved": false} {
		if got := OverrideLabel(name); got != want {
			t.Errorf("OverrideLabel(%q) = %t, want %t", name, got, want)
		}
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
	writes, events, _ := checkLabelsAll(t, timeline, dirty)
	return writes, events
}

// checkLabelsAll is checkLabels, also returning CheckLabels' settings.
func checkLabelsAll(t *testing.T, timeline []ev, dirty func(int64) bool) ([]string, []LabelEvent, int64) {
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
	_, got, settings, _, err := c.CheckLabels(context.Background(), "o", "r", 7, nil, "issuedog", fakeTrust{"owner": true}, dirty, nil)
	if err != nil {
		t.Fatal(err)
	}
	return writes, got, settings
}

func readAll(r io.Reader) []byte {
	b, _ := io.ReadAll(r)
	return b
}

// With no Date on the second read of the issue, the local clock times an
// unexplained label, which is logged, and a later Date counts from there.
func TestCheckLabelsDateFallback(t *testing.T) {
	var date string // the Date of each read of the issue, or none
	var writes []string
	c := newClientWith(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/repos/o/r")
		var err error
		switch {
		case r.Method == http.MethodGet && path == "/issues/7/timeline":
			err = json.NewEncoder(w).Encode([]map[string]any{added(1, "mallory", "foo").json()})
		case r.Method == http.MethodGet && path == "/issues/7":
			if date == "" {
				w.Header()["Date"] = nil // so the server sends none
			} else {
				w.Header().Set("Date", date)
			}
			err = json.NewEncoder(w).Encode(map[string]any{"number": 7, "labels": []map[string]any{{"name": "priority:p0"}}})
		default:
			writes = append(writes, r.Method+" "+path)
			_, err = io.WriteString(w, "{}")
		}
		if err != nil {
			t.Errorf("reply to %s %s: %v", r.Method, path, err)
		}
	}))
	seen := &Unexplained{}
	start := time.Now()
	for i, tt := range []struct {
		date   string
		writes int
	}{
		{"", 0},
		{start.Add(4 * time.Minute).UTC().Format(http.TimeFormat), 0},
		{start.Add(6 * time.Minute).UTC().Format(http.TimeFormat), 2},
	} {
		date = tt.date
		var logs bytes.Buffer
		stop := logr.AddWriter(&logs, logr.WithFilter(logr.D))
		_, _, _, held, err := c.CheckLabels(context.Background(), "o", "r", 7, []string{"priority:p0"}, "issuedog", fakeTrust{},
			func(int64) bool { return false }, seen)
		logr.Wait()
		stop()
		if err != nil {
			t.Fatal(err)
		}
		if len(writes) != tt.writes || (tt.writes == 0) != (len(held) == 1) {
			t.Errorf("pass %d: writes %q, held %q; want %d writes, and the label held only with none", i, writes, held, tt.writes)
		}
		if fell := strings.Contains(logs.String(), "the local clock times"); fell != (tt.date == "") {
			t.Errorf("pass %d: logs = %q, want the fallback logged only with no Date", i, logs.String())
		}
	}
}
