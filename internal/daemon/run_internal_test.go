package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
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
		Started: time.Unix(100, 0).UTC(), Polled: time.Unix(90, 0).UTC(), Seen: 42, Edits: map[int64]time.Time{41: time.Unix(80, 0).UTC()},
		Start: strings.Repeat("a", 40), Target: strings.Repeat("b", 40)}
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
	if !strings.Contains(string(data), `"start":"`+want.Start+`"`) || !strings.Contains(string(data), `"target":"`+want.Target+`"`) {
		t.Errorf("lease = %s, want its start and target under start and target", data)
	}
	if none, err := json.Marshal(Lease{}); err != nil || strings.Contains(string(none), `"start"`) || strings.Contains(string(none), `"target"`) {
		t.Errorf("a lease with no start or target = %s, %v; want neither key", none, err)
	}
	if got.Issue != want.Issue || got.Stage != want.Stage || got.RunID != want.RunID || got.Clone != want.Clone ||
		!got.Started.Equal(want.Started) || !got.Polled.Equal(want.Polled) || got.Seen != want.Seen || !got.Edits[41].Equal(want.Edits[41]) ||
		got.Start != want.Start || got.Target != want.Target {
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

// sweepDirNames lists dir's entries by name.
func sweepDirNames(t *testing.T, dir string) []string {
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
		if got := sweepDirNames(t, dir); !slices.Equal(got, want) {
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
		if got := sweepDirNames(t, dir); !slices.Equal(got, want) {
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
	if got := sweepDirNames(t, r.Workspace.RunsDir); !slices.Equal(got, []string{"dead"}) {
		t.Errorf("the runs dir holds %q while the lease can't be read, want the clone kept", got)
	}
	if err := os.Remove(r.leasePath()); err != nil {
		t.Fatal(err)
	}
	if err := r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{r.Workspace.RunsDir, r.Workspace.ContextDir} {
		if got := sweepDirNames(t, dir); len(got) != 0 {
			t.Errorf("%s holds %q once the lease reads, want nothing", dir, got)
		}
	}
}

// Once a pass has read the lease and swept, later passes don't sweep: what
// looks like a crash's leftovers then may be a live run's files.
func TestWorkSweepsOnlyOnTheFirstPass(t *testing.T) {
	t.Parallel()
	r := sweepRuns(t)
	if err := r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	crashLeftovers(t, r, "late")
	if err := r.Work(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for dir, want := range map[string][]string{
		r.Runner.StateDir:                        {"lease.9.tmp", "runs"},
		filepath.Join(r.Runner.StateDir, "runs"): {"late.123.tmp"},
		r.Workspace.RunsDir:                      {"late"},
		r.Workspace.ContextDir:                   {".agent.gitconfig.4", "late"},
	} {
		if got := sweepDirNames(t, dir); !slices.Equal(got, want) {
			t.Errorf("%s holds %q after the second Work, want %q", dir, got, want)
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

// leaseCommitsGit runs git in dir, with no inherited or global config, and
// returns its output, trimmed.
func leaseCommitsGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool { return strings.HasPrefix(kv, "GIT_") }),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The lease's start and target are the commits its run's clone is made of
// (SPEC §3.2, §3.4): HEAD detached at the start, and the target, when the
// lease names one, at workspace.TargetRef, the clone's only ref. With no
// target, the clone has no ref at all.
func TestLeaseCommitsNameTheClone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	work, bare := filepath.Join(dir, "work"), filepath.Join(dir, "r.git")
	leaseCommitsGit(t, dir, "init", "--quiet", "--initial-branch=main", work)
	leaseCommitsGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "start")
	start := leaseCommitsGit(t, work, "rev-parse", "HEAD")
	leaseCommitsGit(t, work, "checkout", "--quiet", "-b", "side")
	leaseCommitsGit(t, work, "commit", "--quiet", "--allow-empty", "-m", "target")
	target := leaseCommitsGit(t, work, "rev-parse", "HEAD")
	leaseCommitsGit(t, dir, "clone", "--quiet", "--bare", work, bare)
	for name, tt := range map[string]struct {
		target, refs string
	}{
		"no target":   {"", ""},
		"with target": {target, target + " " + workspace.TargetRef},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			w := &workspace.Workspace{Bare: bare, RunsDir: filepath.Join(root, "runs"), ContextDir: filepath.Join(root, "ctx")}
			for _, d := range []string{w.RunsDir, w.ContextDir} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			l := Lease{RunID: "run1", Start: start, Target: tt.target}
			if err := w.Prepare(context.Background(), l.RunID, l.commits(), nil); err != nil {
				t.Fatal(err)
			}
			clone := w.Dir(l.RunID)
			if head := leaseCommitsGit(t, clone, "rev-parse", "HEAD"); head != start {
				t.Errorf("the clone's HEAD = %s, want the lease's start %s", head, start)
			}
			if refs := leaseCommitsGit(t, clone, "for-each-ref", "--format=%(objectname) %(refname)"); refs != tt.refs {
				t.Errorf("the clone's refs = %q, want %q", refs, tt.refs)
			}
		})
	}
}
