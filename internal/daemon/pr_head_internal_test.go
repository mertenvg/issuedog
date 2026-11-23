package daemon

import (
	"testing"

	"github.com/mertenvg/issuedog/internal/agent"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// prHeadMarker is a transition marker with fields, as from=, to= and the
// rest.
func prHeadMarker(fields map[string]string) github.Marker {
	return github.Marker{Kind: "transition", Fields: fields}
}

// A task's verified head is the head= of its latest move into pr-open that
// names one: a move back there that names none, as a trusted reopen's or
// a return from a hold, is looked through, and a move out of pr-open,
// even one naming a head, never counts (SPEC §5).
func TestPRHeadOf(t *testing.T) {
	t.Parallel()
	verified := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "run": "rv", "head": "aaa", "pr": "2"})
	handoff := prHeadMarker(map[string]string{"from": "pr-open", "to": "needs-human", "event": "c2-1"})
	reopened := prHeadMarker(map[string]string{"from": "needs-human", "to": "pr-open", "event": "9", "pr": "2"})
	newHead := prHeadMarker(map[string]string{"from": "pr-open", "to": "verifying", "event": "nbbb", "head": "bbb"})
	again := prHeadMarker(map[string]string{"from": "verifying", "to": "pr-open", "run": "rw", "head": "bbb", "pr": "2"})
	for _, tt := range []struct {
		name    string
		markers []github.Marker
		want    string
	}{
		{"none", nil, ""},
		{"the PR step's", []github.Marker{verified}, "aaa"},
		{"through a trusted reopen", []github.Marker{verified, handoff, reopened}, "aaa"},
		{"not a new head's own", []github.Marker{verified, newHead}, "aaa"},
		{"the later PR step's", []github.Marker{verified, newHead, again}, "bbb"},
	} {
		if got := prHeadOf(state.Issue{Markers: tt.markers}); got != tt.want {
			t.Errorf("%s: prHeadOf = %q; want %q", tt.name, got, tt.want)
		}
	}
}

// The move's comment names the new head and the old one, or with no old
// head named, the new one alone (SPEC §7.2).
func TestRenderNewHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ old, want string }{
		{"aaa", "This task's branch is at `bbb`, not at `aaa`, the head its PR was verified at, and the daemon didn't push it. " +
			"So the task is back at `verifying` to check the new head, and its PR waits until that head is verified."},
		{"", "This task's branch is at `bbb`, a head the daemon didn't push, so the task is back at `verifying` to check it. " +
			"Its PR waits until that head is verified."},
	} {
		if got := renderNewHead(tt.old, "bbb"); got != tt.want {
			t.Errorf("renderNewHead(%q, bbb) =\n%s\nwant\n%s", tt.old, got, tt.want)
		}
	}
}

// Only a move pr-open → verifying keyed event=n<SHA> with a head is a new
// head's, whose head the task's head follows (taskHeadFollow).
func TestNewHeadMove(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"a new head's", map[string]string{"from": "pr-open", "to": "verifying", "event": "nbbb", "head": "bbb"}, true},
		{"the implementer's", map[string]string{"from": "in-progress", "to": "verifying", "run": "r1", "head": "bbb"}, false},
		{"by hand", map[string]string{"from": "pr-open", "to": "verifying", "event": "77", "head": "bbb"}, false},
		{"with no head", map[string]string{"from": "pr-open", "to": "verifying", "event": "nbbb"}, false},
		{"a bare n", map[string]string{"from": "pr-open", "to": "verifying", "event": "n", "head": "bbb"}, false},
		{"out of held", map[string]string{"from": "held", "to": "verifying", "event": "nbbb", "head": "bbb"}, false},
	} {
		if got := newHeadMove(prHeadMarker(tt.fields)); got != tt.want {
			t.Errorf("%s: newHeadMove = %t; want %t", tt.name, got, tt.want)
		}
	}
	if newHeadMove(github.Marker{Kind: "verify", Fields: map[string]string{"from": "pr-open", "to": "verifying", "event": "nbbb", "head": "bbb"}}) {
		t.Error("newHeadMove took a verify marker for a move")
	}
}

// prHeadDaemon is a daemon for o/r holding snaps, by number.
func prHeadDaemon(snaps ...*github.Snapshot) *Daemon {
	d := &Daemon{Step: &Step{Applier: &Applier{Owner: "o", Repo: "r"}}, snaps: map[int]*github.Snapshot{}}
	for _, s := range snaps {
		d.snaps[s.Number] = s
	}
	return d
}

// Task 1 may take the worker only while no other open issue the daemon
// holds is at in-progress or verifying, step 1 finished on every issue,
// and no lease held it when Work last ended (SPEC §8.1). Task 1's own
// stage, and a closed issue's, don't count.
func TestWorkerFree(t *testing.T) {
	t.Parallel()
	task := &github.Snapshot{Number: 1, Labels: []string{"stage:pr-open"}}
	for _, tt := range []struct {
		name string
		d    func() *Daemon
		want bool
		why  string
	}{
		{"free", func() *Daemon {
			return prHeadDaemon(task, &github.Snapshot{Number: 2, Labels: []string{"stage:ready"}})
		}, true, ""},
		{"another at in-progress", func() *Daemon {
			return prHeadDaemon(task, &github.Snapshot{Number: 2, Labels: []string{"stage:in-progress"}})
		}, false, "#2, at stage:in-progress, holds it"},
		{"another at verifying", func() *Daemon {
			return prHeadDaemon(task, &github.Snapshot{Number: 3, Labels: []string{"priority:p1", "stage:verifying"}})
		}, false, "#3, at stage:verifying, holds it"},
		{"another at verifying, closed", func() *Daemon {
			return prHeadDaemon(task, &github.Snapshot{Number: 3, Labels: []string{"stage:verifying"}, Closed: true})
		}, true, ""},
		{"the task itself at verifying", func() *Daemon {
			return prHeadDaemon(&github.Snapshot{Number: 1, Labels: []string{"stage:verifying"}})
		}, true, ""},
		{"a gap in step 1", func() *Daemon {
			d := prHeadDaemon(task)
			d.step1Gaps = map[int]step1Gap{4: {why: "its read failed"}}
			return d
		}, false, "step 1 didn't finish on every issue this pass"},
		{"a lease", func() *Daemon {
			d := prHeadDaemon(task)
			d.workerLeased = true
			return d
		}, false, "a run's lease holds it"},
	} {
		if got, why := tt.d().workerFree(1); got != tt.want || why != tt.why {
			t.Errorf("%s: workerFree = %t, %q; want %t, %q", tt.name, got, why, tt.want, tt.why)
		}
	}
}

// Work notes whether a lease holds the worker as it ends: one standing
// does, one marked ended doesn't, and none doesn't.
func TestWorkerLeaseNote(t *testing.T) {
	t.Parallel()
	r := &Runs{Daemon: prHeadDaemon(), Runner: &agent.Runner{StateDir: t.TempDir()}}
	ctx := t.Context()
	for _, tt := range []struct {
		name  string
		lease *Lease
		want  bool
	}{
		{"none", nil, false},
		{"standing", &Lease{Issue: 5, Stage: state.InProgress, RunID: "run1"}, true},
		{"ended", &Lease{Issue: 5, Stage: state.InProgress, RunID: "run1", Failed: true}, false},
	} {
		if tt.lease != nil {
			if err := r.writeLease(*tt.lease); err != nil {
				t.Fatal(err)
			}
		}
		r.Daemon.workerLeased = !tt.want
		if err := r.workerLeaseNote(ctx); err != nil {
			t.Fatalf("%s: workerLeaseNote: %v", tt.name, err)
		}
		if r.Daemon.workerLeased != tt.want {
			t.Errorf("%s: workerLeased = %t; want %t", tt.name, r.Daemon.workerLeased, tt.want)
		}
	}
}

// Work notes the lease as it ends, whatever it ran: with none left, the
// worker isn't leased for the next pass's step 2, though it was before.
func TestWorkNotesTheWorkersLease(t *testing.T) {
	t.Parallel()
	r := sweepRuns(t)
	r.Daemon = prHeadDaemon()
	r.Daemon.workerLeased = true
	if err := r.Work(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if r.Daemon.workerLeased {
		t.Error("after a Work that left no lease, workerLeased = true; want false")
	}
}

// A move waiting for the worker stands while the task hasn't moved since
// its PR was judged and the poll hasn't listed the PR changed, and is
// dropped otherwise, so the PR is judged again (SPEC §8.1).
func TestPRWaitingFor(t *testing.T) {
	t.Parallel()
	m := Move{To: state.Verifying, Trigger: state.TriggerNewHead, Key: newHeadKey("bbb")}
	p := listedPR{Number: 2, Open: true}
	for _, tt := range []struct {
		name    string
		moved   int64 // the task's latest transition's comment
		changed bool  // the poll listed the PR changed
		want    bool
	}{
		{"standing", 10, false, true},
		{"the task moved", 11, false, false},
		{"the PR changed", 10, true, false},
	} {
		d := prHeadDaemon()
		d.prJudged = map[int]int64{1: 10}
		d.prWait(1, m, "#3 holds it")
		if tt.changed {
			d.prReadAgain(2)
		}
		got, ok := d.prWaitingFor(1, state.Issue{TransitionComment: tt.moved}, p)
		if ok != tt.want || ok && got.Key != m.Key {
			t.Errorf("%s: prWaitingFor = %+v, %t; want the move, %t", tt.name, got, ok, tt.want)
		}
		if _, kept := d.prWaiting[1]; kept != tt.want {
			t.Errorf("%s: the move kept %t; want %t", tt.name, kept, tt.want)
		}
	}
}
