package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/workspace"
)

// The lease is replaced by a rename, never written in place: a link to the
// old one keeps the old content, and no temp file is left.
func TestWriteLeaseReplaces(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := &Runs{Runner: &agent.Runner{StateDir: dir}}
	path := filepath.Join(dir, "lease.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	want := Lease{Issue: 7, Stage: "clarifying", RunID: "ab12", Clone: "/work/runs/ab12",
		Started: time.Unix(100, 0).UTC(), Polled: time.Unix(90, 0).UTC(), Seen: 42, Edits: map[int64]time.Time{41: time.Unix(80, 0).UTC()}}
	if err := r.writeLease(want); err != nil {
		t.Fatal(err)
	}
	if old, err := os.ReadFile(filepath.Join(dir, "old")); err != nil || string(old) != "old" {
		t.Errorf("the old lease's other link reads %q, %v; want it untouched by a rename", old, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Lease
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Issue != want.Issue || got.Stage != want.Stage || got.RunID != want.RunID || got.Clone != want.Clone ||
		!got.Started.Equal(want.Started) || !got.Polled.Equal(want.Polled) || got.Seen != want.Seen || !got.Edits[41].Equal(want.Edits[41]) {
		t.Errorf("lease = %+v, want %+v", got, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("lease mode = %v, %v; want 0600", fi, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Errorf("state dir has %v, %v; want only the lease and the link", entries, err)
	}
}

// A lease that can't be read is removed, as nothing can resume it, with
// the clone it names if that's in the workspace.
func TestReadLeaseRemovesBadOnes(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		data      string
		cloneGoes bool
	}{
		"malformed":             {data: "{"},
		"no run ID":             {data: `{"issue":1}`},
		"path":                  {data: `{"issue":1,"run_id":"../x","clone":"/elsewhere/x"}`},
		"clone in the runs dir": {data: `{"issue":1,"run_id":"","clone":"RUNS/x"}`, cloneGoes: true},
	} {
		t.Run(name, func(t *testing.T) {
			runs := t.TempDir()
			r := &Runs{Runner: &agent.Runner{StateDir: t.TempDir()},
				Workspace: &workspace.Workspace{RunsDir: runs, ContextDir: t.TempDir()}}
			if err := os.Mkdir(filepath.Join(runs, "x"), 0o755); err != nil {
				t.Fatal(err)
			}
			data := strings.ReplaceAll(tt.data, "RUNS", runs)
			if err := os.WriteFile(r.leasePath(), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := r.readLease(context.Background()); ok || err != nil {
				t.Errorf("readLease = %t, %v; want no lease and no error", ok, err)
			}
			if _, err := os.Stat(r.leasePath()); !os.IsNotExist(err) {
				t.Errorf("the lease is still there: %v", err)
			}
			if _, err := os.Stat(filepath.Join(runs, "x")); os.IsNotExist(err) != tt.cloneGoes {
				t.Errorf("the clone is gone: %t, want %t", os.IsNotExist(err), tt.cloneGoes)
			}
		})
	}
}

// The first Work removes the temp leases a crash left, and nothing else.
func TestWorkSweepsTempLeases(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"lease.123.tmp", "lease.4.tmp", "other.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	gate, err := agent.NewGate("x")
	if err != nil {
		t.Fatal(err)
	}
	r := &Runs{Runner: &agent.Runner{StateDir: dir}, Gate: gate,
		Workspace: &workspace.Workspace{RunsDir: t.TempDir(), ContextDir: t.TempDir()}}
	if err := r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "other.tmp" {
		t.Errorf("the state dir has %v, %v; want only other.tmp", entries, err)
	}
}

// crashLeftovers makes what a crash leaves for runs: a checkout, a context
// dir and a temp result for each, a temp lease, and a temp git config.
func crashLeftovers(t *testing.T, r *Runs, runs ...string) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(r.Runner.StateDir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range runs {
		for _, d := range []string{r.Workspace.Dir(id), r.Workspace.Context(id)} {
			if err := os.Mkdir(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(r.Runner.StateDir, "runs", id+".123.tmp"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{filepath.Join(r.Runner.StateDir, "lease.9.tmp"), filepath.Join(r.Workspace.ContextDir, ".agent.gitconfig.4")} {
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// names lists dir's entries by name.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	return got
}

// sweepRuns is a Runs for the sweep tests, its dirs all empty.
func sweepRuns(t *testing.T) *Runs {
	t.Helper()
	gate, err := agent.NewGate("x")
	if err != nil {
		t.Fatal(err)
	}
	return &Runs{Runner: &agent.Runner{StateDir: t.TempDir()}, Gate: gate,
		Workspace: &workspace.Workspace{RunsDir: t.TempDir(), ContextDir: t.TempDir()}}
}

// After a crash with no lease, the next start's first Work removes every
// run's checkout and context dir, the temp results, the temp lease and the
// temp git config, and leaves a result, which isn't a temp file.
func TestWorkSweepsLeftoversAfterACrash(t *testing.T) {
	t.Parallel()
	r := sweepRuns(t)
	crashLeftovers(t, r, "ab12", "cd34")
	if err := os.WriteFile(r.resultPath("ab12"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string][]string{
		r.Runner.StateDir:                        {"runs"},
		filepath.Join(r.Runner.StateDir, "runs"): {"ab12.json"},
		r.Workspace.RunsDir:                      nil,
		r.Workspace.ContextDir:                   nil,
	} {
		if got := names(t, dir); !slices.Equal(got, want) {
			t.Errorf("%s holds %q after the first Work, want %q", dir, got, want)
		}
	}
}

// The sweep keeps the lease's run's checkout and context dir, for resume,
// and removes every other run's, with the temp files.
func TestSweepKeepsTheLeasesRun(t *testing.T) {
	t.Parallel()
	r := sweepRuns(t)
	crashLeftovers(t, r, "live", "dead")
	l := Lease{Issue: 7, Stage: "clarifying", RunID: "live", Clone: r.Workspace.Dir("live"), Attempt: 1}
	if err := r.writeLease(l); err != nil {
		t.Fatal(err)
	}
	if !r.sweep(context.Background()) {
		t.Fatal("sweep reports the lease unread")
	}
	for dir, want := range map[string][]string{
		r.Runner.StateDir:                        {"lease.json", "runs"},
		filepath.Join(r.Runner.StateDir, "runs"): nil,
		r.Workspace.RunsDir:                      {"live"},
		r.Workspace.ContextDir:                   {"live"},
	} {
		if got := names(t, dir); !slices.Equal(got, want) {
			t.Errorf("%s holds %q after the sweep, want %q", dir, got, want)
		}
	}
}

// While the lease can't be read, Work sweeps no run's files, as it can't
// tell which is the lease's, and sweeps them on a later pass that reads it.
func TestWorkSweepsRunsOnceTheLeaseReads(t *testing.T) {
	t.Parallel()
	r := sweepRuns(t)
	crashLeftovers(t, r, "dead")
	// A dir where the lease goes can't be read as one.
	if err := os.Mkdir(r.leasePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := r.Work(context.Background(), nil); err == nil {
		t.Fatal("Work succeeded with a lease it can't read")
	}
	if got := names(t, r.Workspace.RunsDir); !slices.Equal(got, []string{"dead"}) {
		t.Errorf("the runs dir holds %q while the lease can't be read, want the clone kept", got)
	}
	if err := os.Remove(r.leasePath()); err != nil {
		t.Fatal(err)
	}
	if err := r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{r.Workspace.RunsDir, r.Workspace.ContextDir} {
		if got := names(t, dir); len(got) != 0 {
			t.Errorf("%s holds %q once the lease reads, want nothing", dir, got)
		}
	}
}

// pick finds each agent's setting, the interviewer's for triage, and none
// for an unknown agent.
func TestPick(t *testing.T) {
	t.Parallel()
	p := config.PerStage[int]{Interviewer: 1, Planner: 2, Implementer: 3, Verifier: 4}
	for name, want := range map[string]int{"interviewer": 1, "planner": 2, "implementer": 3, "verifier": 4, "triage": 1} {
		if got, ok := pick(p, name); got != want || !ok {
			t.Errorf("pick(%s) = %d, %t; want %d", name, got, ok, want)
		}
	}
	if got, ok := pick(p, "librarian"); got != 0 || ok {
		t.Errorf("pick(librarian) = %d, %t; want none", got, ok)
	}
}

// lastFailure keeps a cause of up to 1,000 characters whole, counting
// runes, not bytes, and cuts a longer one to 1,000, its last an ellipsis.
func TestLastFailure(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, cause, want string
	}{
		{"short", "rule scope: T3", "rule scope: T3"},
		{"exactly 1000", strings.Repeat("é", 1000), strings.Repeat("é", 1000)},
		{"1001", strings.Repeat("é", 1001), strings.Repeat("é", 999) + "…"},
		{"long", strings.Repeat("ab", 5000), strings.Repeat("ab", 499) + "a…"},
	} {
		if got := lastFailure(errors.New(tt.cause)); got != tt.want {
			t.Errorf("%s: lastFailure = %q (%d runes), want %q", tt.name, got, utf8.RuneCountInString(got), tt.want)
		}
	}
}

// runFields leaves out what's unset or can't go in a marker, never failing:
// an unset model or effort, a model name with a space, a modelUsage key
// with a comma or '>', and output that names no models, such as a crashed
// run's.
func TestRunFields(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct {
		l    Lease
		raw  string
		want map[string]string
	}{
		"all":        {Lease{Model: "opus", Effort: "max"}, `{"modelUsage":{"b":{},"a":{}}}`, map[string]string{"model": "opus", "effort": "max", "ran": "a,b"}},
		"unset":      {Lease{}, `{"modelUsage":{}}`, map[string]string{}},
		"odd model":  {Lease{Model: "o pus", Effort: "low"}, `{}`, map[string]string{"effort": "low"}},
		"odd keys":   {Lease{}, `{"modelUsage":{"a,b":{},"c-->":{},"":{},"d":{}}}`, map[string]string{"ran": "d"}},
		"empty":      {Lease{Model: "opus"}, ``, map[string]string{"model": "opus"}},
		"not JSON":   {Lease{Model: "opus"}, `Killed`, map[string]string{"model": "opus"}},
		"usage null": {Lease{}, `{"modelUsage":null}`, map[string]string{}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := runFields(tt.l, []byte(tt.raw)); !maps.Equal(got, tt.want) {
				t.Errorf("runFields = %v, want %v", got, tt.want)
			}
		})
	}
}

// overrideLabels keeps the model and effort labels of all four kinds, in
// lower case and sorted, and nothing else.
func TestOverrideLabels(t *testing.T) {
	t.Parallel()
	got := overrideLabels([]string{"stage:clarifying", "Model:Sonnet", "verifier-effort:max", "priority:p1", "effort:low", "Verifier-Model:opus", "modelx"})
	want := []string{"effort:low", "model:sonnet", "verifier-effort:max", "verifier-model:opus"}
	if !slices.Equal(got, want) {
		t.Errorf("overrideLabels = %q, want %q", got, want)
	}
}
