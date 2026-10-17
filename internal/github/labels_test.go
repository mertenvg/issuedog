package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const labelsPath = "/repos/mertenvg/issuedog/labels"

type fakeLabel struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

// fakeLabels serves the repo's labels endpoint, pageSize labels per page,
// and hands every other request to the token fake. It records every write.
type fakeLabels struct {
	mu       sync.Mutex
	token    *fakeGitHub
	pageSize int
	labels   []fakeLabel
	pages    []int    // pages served, in order
	writes   []string // "METHOD path" of every non-GET call
	created  []fakeLabel
}

func (f *fakeLabels) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, labelsPath) {
		f.token.ServeHTTP(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		page = max(page, 1)
		f.pages = append(f.pages, page)
		lo := min((page-1)*f.pageSize, len(f.labels))
		hi := min(lo+f.pageSize, len(f.labels))
		if hi < len(f.labels) {
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=%d>; rel="next"`, r.Host, labelsPath, page+1))
		}
		_ = json.NewEncoder(w).Encode(f.labels[lo:hi])
	case http.MethodPost:
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		var l fakeLabel
		if err := json.NewDecoder(r.Body).Decode(&l); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.labels = append(f.labels, l)
		f.created = append(f.created, l)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(l)
	default:
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		http.Error(w, "unexpected write", http.StatusMethodNotAllowed)
	}
}

func newFakeLabels(pageSize int, labels ...fakeLabel) *fakeLabels {
	return &fakeLabels{token: &fakeGitHub{ttl: time.Hour}, pageSize: pageSize, labels: labels}
}

func ensure(t *testing.T, f *fakeLabels, opts LabelOptions) []string {
	t.Helper()
	created, err := EnsureLabels(context.Background(), newClient(t, f), "mertenvg", "issuedog", opts)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// labelNames lists the names of specs in order.
func labelNames(specs []labelSpec) []string {
	var names []string
	for _, s := range specs {
		names = append(names, s.name)
	}
	return names
}

func TestEnsureLabelsCreatesMissing(t *testing.T) {
	fixed := labelNames(fixedLabels)
	tests := []struct {
		name string
		opts LabelOptions
		want []string
	}{
		{"fixed set only", LabelOptions{}, fixed},
		{"scopes", LabelOptions{Scopes: []string{"daemon", "github"}},
			append(slices.Clone(fixed), "scope:daemon", "scope:github")},
		{"type labels", LabelOptions{TypeLabels: true},
			append(slices.Clone(fixed), "type:epic", "type:story", "type:task", "type:bug")},
		{"models and efforts", LabelOptions{Models: []ModelLabel{{"opus", "opus"}, {"sonnet-bedrock", "us.anthropic.claude-sonnet-4-5"}}, Efforts: []string{"low", "max"}},
			append(slices.Clone(fixed), "model:opus", "verifier-model:opus", "model:sonnet-bedrock", "verifier-model:sonnet-bedrock",
				"effort:low", "verifier-effort:low", "effort:max", "verifier-effort:max")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeLabels(100)
			created := ensure(t, f, tt.opts)
			if !slices.Equal(created, tt.want) {
				t.Errorf("created %v, want %v", created, tt.want)
			}
			var posted []string
			for _, l := range f.created {
				posted = append(posted, l.Name)
				if l.Color == "" || l.Description == "" {
					t.Errorf("label %s created with color %q, description %q", l.Name, l.Color, l.Description)
				}
			}
			if !slices.Equal(posted, tt.want) {
				t.Errorf("POSTed %v, want %v", posted, tt.want)
			}
		})
	}
}

func TestEnsureLabelsSPECNames(t *testing.T) {
	want := []string{
		"stage:intake", "stage:clarifying", "stage:spec-review", "stage:planning",
		"stage:plan-review", "stage:planned", "stage:ready", "stage:in-progress",
		"stage:verifying", "stage:pr-open", "stage:needs-human", "stage:held", "stage:completed", "stage:discarded",
		"priority:p0", "priority:p1", "priority:p2", "priority:p3",
		"approved", "review:high-risk", "scope:infra",
		"type:epic", "type:story", "type:task", "type:bug",
	}
	got := ensure(t, newFakeLabels(100), LabelOptions{Scopes: []string{"infra"}, TypeLabels: true})
	if !slices.Equal(got, want) {
		t.Errorf("created %v, want %v", got, want)
	}
}

// Existing labels keep their color and description, whatever the case of
// their name, and are never patched or deleted.
func TestEnsureLabelsLeavesExisting(t *testing.T) {
	f := newFakeLabels(100,
		fakeLabel{Name: "stage:intake", Color: "123456", Description: "custom"},
		fakeLabel{Name: "Approved", Color: "abcdef"},
		fakeLabel{Name: "scope:daemon", Color: "000000"},
		fakeLabel{Name: "unrelated", Color: "ffffff"},
	)
	created := ensure(t, f, LabelOptions{Scopes: []string{"daemon"}})
	for _, name := range []string{"stage:intake", "approved", "scope:daemon"} {
		if slices.Contains(created, name) {
			t.Errorf("recreated existing label %s", name)
		}
	}
	if len(created) != len(fixedLabels)-2 {
		t.Errorf("created %d labels, want %d", len(created), len(fixedLabels)-2)
	}
	for _, w := range f.writes {
		if w != "POST "+labelsPath {
			t.Errorf("unexpected write %s", w)
		}
	}
	if got := f.labels[0]; got.Color != "123456" || got.Description != "custom" {
		t.Errorf("existing label changed to %+v", got)
	}
}

// A model label's description names the full model, cut at its start to fit
// GitHub's 100 characters.
func TestEnsureLabelsDescribesModels(t *testing.T) {
	long := "arn:aws:bedrock:us-east-1:123456789012:application-inference-profile/" + strings.Repeat("x", 40) + "-end"
	f := newFakeLabels(100)
	ensure(t, f, LabelOptions{Models: []ModelLabel{{"sonnet", "us.anthropic.claude-sonnet-4-5"}, {"arn", long}}, Efforts: []string{"high"}})
	want := map[string]string{
		"model:sonnet":          "Runs other than the verifier's use us.anthropic.claude-sonnet-4-5",
		"verifier-model:sonnet": "The verifier's runs use us.anthropic.claude-sonnet-4-5",
		"effort:high":           "Runs other than the verifier's use high effort",
		"verifier-effort:high":  "The verifier's runs use high effort",
	}
	got := map[string]fakeLabel{}
	for _, l := range f.created {
		got[l.Name] = l
	}
	for name, desc := range want {
		if got[name].Description != desc {
			t.Errorf("%s has description %q, want %q", name, got[name].Description, desc)
		}
	}
	for name, prefix := range map[string]string{"model:arn": "Runs other than the verifier's use ", "verifier-model:arn": "The verifier's runs use "} {
		d := got[name].Description
		if tail := long[len(long)-(99-len(prefix)):]; d != prefix+"…"+tail || len([]rune(d)) != 100 {
			t.Errorf("%s has description %q, want %q", name, d, prefix+"…"+tail)
		}
	}
	colors := map[string]bool{}
	for _, name := range []string{"model:sonnet", "verifier-model:sonnet", "effort:high", "verifier-effort:high"} {
		colors[got[name].Color] = true
	}
	if len(colors) != 4 {
		t.Errorf("colors = %v, want one for each kind of label", colors)
	}
}

func TestEnsureLabelsSecondRunWritesNothing(t *testing.T) {
	f := newFakeLabels(100)
	opts := LabelOptions{Scopes: []string{"daemon", "github"}, TypeLabels: true,
		Models: []ModelLabel{{"opus", "opus"}}, Efforts: []string{"low", "max"}}
	ensure(t, f, opts)
	f.writes = nil
	if created := ensure(t, f, opts); len(created) != 0 || len(f.writes) != 0 {
		t.Errorf("second run created %v with writes %v, want none", created, f.writes)
	}
}

// A repo already set up, its labels spread over several pages, gets no
// writes: every page is read before anything is created.
func TestEnsureLabelsFollowsPagination(t *testing.T) {
	opts := LabelOptions{Scopes: []string{"daemon", "github", "agents"}, TypeLabels: true}
	var labels []fakeLabel
	for _, l := range wantedLabels(opts) {
		labels = append(labels, fakeLabel{Name: l.name, Color: l.color, Description: l.description})
	}
	f := newFakeLabels(5, labels...)
	if created := ensure(t, f, opts); len(created) != 0 || len(f.writes) != 0 {
		t.Errorf("created %v with writes %v, want none", created, f.writes)
	}
	var want []int
	for p := 1; p <= (len(labels)+4)/5; p++ {
		want = append(want, p)
	}
	if len(want) < 2 || !slices.Equal(f.pages, want) {
		t.Errorf("served pages %v, want %v", f.pages, want)
	}
}

func TestEnsureLabelsAnnotatesErrors(t *testing.T) {
	f := newFakeLabels(100)
	f.token.failMint = true
	_, err := EnsureLabels(context.Background(), newClient(t, f), "mertenvg", "issuedog", LabelOptions{})
	if err == nil || !strings.Contains(err.Error(), "list labels of mertenvg/issuedog") {
		t.Errorf("err = %v, want an annotated list error", err)
	}
}
