package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// What tells an open failure on a story branch and its bug (SPEC §3.4's A
// story's waits, §7.5), from what the daemon holds, at no request:
// failureBugOf, storyFailure, failureWait, failureTip's answers from
// memory, and failureMovedPast. story_failure_test.go runs them end to end.

// storyFailureBranch is story 1's branch in these tests.
const storyFailureBranch = "issuedog/1-quotes"

// storyFailureBug is bug n, below story 1, filed by the App for its
// branch's failing tip failureTestTip, then changed by mod.
func storyFailureBug(n int, mod func(s *github.Snapshot)) *github.Snapshot {
	s := &github.Snapshot{Number: n, Author: github.Author{Login: "issuedog[bot]", Type: "Bot"}, Body: "Rule 1 failed.\n\n" + failureTestOrigin(failureTestTip),
		Type: "bug", Parent: 1}
	if mod != nil {
		mod(s)
	}
	return s
}

// storyFailureDaemon is a Daemon holding snaps, by number, and story 1's
// branch's tip judged as tip with conclusion, or none for "".
func storyFailureDaemon(conclusion, tip string, snaps ...*github.Snapshot) *Daemon {
	d := &Daemon{Step: &Step{Applier: &Applier{AppSlug: "issuedog"}}, snaps: map[int]*github.Snapshot{1: {Number: 1, Type: "story"}}}
	for _, s := range snaps {
		d.snaps[s.Number] = s
	}
	if conclusion != "" {
		d.tipJudge(storyFailureBranch, tip, conclusion)
	}
	return d
}

// A failure's bug of a story is a bug whose App origin names a failure on
// that story's branch, a conflict bug's too, whatever its type's case. A
// task filed for one, the human task, one of another story's, a plan's bug
// and a person's bug aren't.
func TestFailureBugOf(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		s    *github.Snapshot
		want bool
	}{
		{"a bug", storyFailureBug(5, nil), true},
		{"a Bug", storyFailureBug(5, func(s *github.Snapshot) { s.Type = "Bug" }), true},
		{"a conflict bug", storyFailureBug(5, func(s *github.Snapshot) {
			s.Body = "Merge it.\n\n<!-- issuedog:origin main=" + failureTestMain + " story=#1 tip=" + failureTestTip + " -->"
		}), true},
		{"a task", storyFailureBug(5, func(s *github.Snapshot) { s.Type = "task" }), false},
		{"the human task", storyFailureBug(5, func(s *github.Snapshot) {
			s.Type, s.Body = "task", "Revert it.\n\n<!-- issuedog:origin human=1 story=#1 tip="+failureTestTip+" -->"
		}), false},
		{"another story's", storyFailureBug(5, func(s *github.Snapshot) {
			s.Body = "<!-- issuedog:origin story=#2 tip=" + failureTestTip + " -->"
		}), false},
		{"a plan's bug", storyFailureBug(5, func(s *github.Snapshot) { s.Body = "<!-- issuedog:origin plan=#1 rev=1 item=T1 -->" }), false},
		{"a person's", storyFailureBug(5, func(s *github.Snapshot) { s.Author = github.Author{Login: "owner", Type: "User"} }), false},
	} {
		if got := failureBugOf(state.Derive(tt.s, "issuedog", func(int64) bool { return false }), 1); got != tt.want {
			t.Errorf("%s: failureBugOf = %t; want %t", tt.name, got, tt.want)
		}
	}
	if failureBugOf(state.Issue{Item: state.Item{Type: "bug"}}, 0) {
		t.Error("failureBugOf an issue with no origin, for no story, = true; want false")
	}
}

// A failure on a story branch is open while a failure's bug of the story
// is open among the snapshots held, the lowest number named, one written
// to this pass included, or while the branch's tip is judged failure. A
// closed bug, one below another story or in another repo, a human task,
// and a tip judged success, or a failure judged on another tip, as before
// the branch moved, leave none open.
func TestStoryFailure(t *testing.T) {
	t.Parallel()
	other := strings.Repeat("e", 40)
	human := storyFailureBug(6, func(s *github.Snapshot) {
		s.Type, s.Body = "task", "Revert it.\n\n<!-- issuedog:origin human=1 story=#1 tip="+failureTestTip+" -->"
	})
	for _, tt := range []struct {
		name       string
		conclusion string // story 1's branch's tip's, at tip
		tip        string
		stale      bool // every snapshot was written to this pass
		snaps      []*github.Snapshot
		bug        int
		open       bool
	}{
		{"an open bug", "", "", false, []*github.Snapshot{storyFailureBug(5, nil)}, 5, true},
		{"an open bug, on a passing tip", checkSuccess, failureTestTip, false, []*github.Snapshot{storyFailureBug(5, nil)}, 5, true},
		{"two open bugs", "", "", false, []*github.Snapshot{storyFailureBug(9, nil), storyFailureBug(7, nil)}, 7, true},
		{"one written to this pass", "", "", true, []*github.Snapshot{storyFailureBug(5, nil)}, 5, true},
		{"a closed bug", "", "", false, []*github.Snapshot{storyFailureBug(5, func(s *github.Snapshot) { s.Closed = true })}, 0, false},
		{"a bug below another story", "", "", false, []*github.Snapshot{storyFailureBug(5, func(s *github.Snapshot) { s.Parent = 2 })}, 0, false},
		{"a bug below a story in another repo", "", "", false,
			[]*github.Snapshot{storyFailureBug(5, func(s *github.Snapshot) { s.ParentRepo = "o/other" })}, 0, false},
		{"an open human task", checkSuccess, failureTestTip, false, []*github.Snapshot{human}, 0, false},
		{"a failing tip", checkFailure, failureTestTip, false, nil, 0, true},
		{"a failing tip and its human task", checkFailure, failureTestTip, false, []*github.Snapshot{human}, 0, true},
		{"a passing tip", checkSuccess, failureTestTip, false, nil, 0, false},
		{"a failure on a tip the branch moved past", checkFailure, other, false, nil, 0, false},
		{"nothing judged", "", "", false, nil, 0, false},
	} {
		tip := tt.tip
		if tip == "" {
			tip = failureTestTip
		}
		d := storyFailureDaemon(tt.conclusion, tip, tt.snaps...)
		if tt.stale {
			d.stale = map[int]bool{}
			for _, s := range tt.snaps {
				d.stale[s.Number] = true
			}
		}
		bug, open := d.storyFailure(d.snaps[1], storyFailureBranch, failureTestTip)
		if bug != tt.bug || open != tt.open {
			t.Errorf("%s: storyFailure = #%d, %t; want #%d, %t", tt.name, bug, open, tt.bug, tt.open)
		}
	}
}

// A task in a story waits on an open failure on its branch, saying why,
// naming the bug, or the tip's failure, while the failure's bug itself
// doesn't, nor a task with no parent, or one whose story the daemon
// doesn't hold.
func TestFailureWait(t *testing.T) {
	t.Parallel()
	sibling := &github.Snapshot{Number: 3, Type: "task", Parent: 1}
	bug := storyFailureBug(5, nil)
	d := storyFailureDaemon(checkSuccess, failureTestTip, sibling, bug)
	if got, want := d.failureWait(sibling, storyFailureBranch, failureTestTip),
		"a failure on its story branch "+storyFailureBranch+" is open, as its bug #5 is, which goes first"; got != want {
		t.Errorf("the sibling's wait = %q; want %q", got, want)
	}
	if got := d.failureWait(bug, storyFailureBranch, failureTestTip); got != "" {
		t.Errorf("the failure's bug's wait = %q; want none", got)
	}
	for name, s := range map[string]*github.Snapshot{
		"no parent":               {Number: 3, Type: "task"},
		"a story it doesn't hold": {Number: 3, Type: "task", Parent: 2},
	} {
		if got := d.failureWait(s, storyFailureBranch, failureTestTip); got != "" {
			t.Errorf("%s: the wait = %q; want none", name, got)
		}
	}
	d = storyFailureDaemon(checkFailure, failureTestTip, sibling)
	if got, want := d.failureWait(sibling, storyFailureBranch, failureTestTip),
		"its story branch "+storyFailureBranch+"'s tip "+failureTestTip+" has issuedog/tip as failure, so a failure on the branch is open"; got != want {
		t.Errorf("the sibling's wait on a failing tip = %q; want %q", got, want)
	}
	d = storyFailureDaemon(checkSuccess, failureTestTip, sibling)
	if got := d.failureWait(sibling, storyFailureBranch, failureTestTip); got != "" {
		t.Errorf("the sibling's wait with no failure open = %q; want none", got)
	}
}

// A failure's bug takes no tip step 4 hasn't judged, as before a restart's
// first read of its check, nor one the App merged a task's PR onto, and
// takes one judged success at once, as it passed rule 2 among its checks,
// with no read of the bare repo, which this Daemon hasn't.
// story_failure_test.go judges a failing tip's rule 2 in a real one.
func TestFailureTipFromMemory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	other := strings.Repeat("e", 40)
	for _, tt := range []struct {
		name       string
		conclusion string
		judged     string // the tip judged
		merged     bool   // the App merged a task's PR onto it
		want       bool
	}{
		{"nothing judged", "", "", false, false},
		{"another tip judged", checkFailure, other, false, false},
		{"a tip judged success", checkSuccess, failureTestTip, false, true},
		{"a tip a PR merged onto", checkSuccess, failureTestTip, true, false},
	} {
		d := storyFailureDaemon(tt.conclusion, tt.judged)
		if tt.merged {
			d.tipMerged(storyFailureBranch, failureTestTip)
		}
		got, err := d.failureTip(ctx, storyFailureBranch, failureTestTip, failureTestMain)
		if err != nil || got != tt.want {
			t.Errorf("%s: failureTip = %t, %v; want %t", tt.name, got, err, tt.want)
		}
	}
}

// A failure's bug's run names its story branch's tip as its target to
// merge once that tip isn't the one it was filed for, and never on that
// tip, nor with no tip named. No other issue's run names it for that.
func TestFailureMovedPast(t *testing.T) {
	t.Parallel()
	other := strings.Repeat("e", 40)
	derive := func(s *github.Snapshot) state.Issue {
		return state.Derive(s, "issuedog", func(int64) bool { return false })
	}
	bug := derive(storyFailureBug(5, nil))
	task := derive(storyFailureBug(5, func(s *github.Snapshot) { s.Type = "task" }))
	for _, tt := range []struct {
		name string
		is   state.Issue
		tip  string
		want bool
	}{
		{"the bug on a newer tip", bug, other, true},
		{"the bug on its own tip", bug, failureTestTip, false},
		{"the bug with no tip", bug, "", false},
		{"a task on a newer tip", task, other, false},
	} {
		if got := failureMovedPast(tt.is, tt.tip); got != tt.want {
			t.Errorf("%s: failureMovedPast = %t; want %t", tt.name, got, tt.want)
		}
	}
}

// A task of a story holding the worker steps aside for an open failure's
// bug of that story, and for nothing else: not for a closed bug, a task,
// another story's bug, nor the bug itself, and a failure's bug holding it,
// or a task in another story, or in a story that's closed or not held,
// doesn't step aside (SPEC §8.1's One task at a time).
func TestFailureAside(t *testing.T) {
	t.Parallel()
	sibling := &github.Snapshot{Number: 2, Type: "task", Parent: 1, Labels: []string{"stage:in-progress"}}
	for _, tt := range []struct {
		name          string
		holder, taker *github.Snapshot
		story         *github.Snapshot // story 1, or nil for none held
		want          bool
	}{
		{"a sibling, for the bug", sibling, storyFailureBug(5, nil), &github.Snapshot{Number: 1, Type: "story"}, true},
		{"a sibling, for a conflict bug", sibling, storyFailureBug(5, func(s *github.Snapshot) {
			s.Body = "Merge it.\n\n<!-- issuedog:origin main=" + failureTestMain + " story=#1 tip=" + failureTestTip + " -->"
		}), &github.Snapshot{Number: 1, Type: "story"}, true},
		{"a sibling, for a closed bug", sibling, storyFailureBug(5, func(s *github.Snapshot) { s.Closed = true }), &github.Snapshot{Number: 1, Type: "story"}, false},
		{"a sibling, for a task", sibling, &github.Snapshot{Number: 5, Type: "task", Parent: 1}, &github.Snapshot{Number: 1, Type: "story"}, false},
		{"a sibling, for another story's bug", sibling, storyFailureBug(5, func(s *github.Snapshot) {
			s.Parent, s.Body = 3, "Rule 1 failed.\n\n<!-- issuedog:origin story=#3 tip="+failureTestTip+" -->"
		}), &github.Snapshot{Number: 1, Type: "story"}, false},
		{"another failure's bug, for the bug", storyFailureBug(4, nil), storyFailureBug(5, nil), &github.Snapshot{Number: 1, Type: "story"}, false},
		{"a task in another story", &github.Snapshot{Number: 2, Type: "task", Parent: 3}, storyFailureBug(5, nil), &github.Snapshot{Number: 1, Type: "story"}, false},
		{"a task with a parent in another repo", &github.Snapshot{Number: 2, Type: "task", Parent: 1, ParentRepo: "o/other"}, storyFailureBug(5, nil),
			&github.Snapshot{Number: 1, Type: "story"}, false},
		{"a sibling, in a closed story", sibling, storyFailureBug(5, nil), &github.Snapshot{Number: 1, Type: "story", Closed: true}, false},
		{"a sibling, in a story not held", sibling, storyFailureBug(5, nil), nil, false},
		{"the bug, for itself", storyFailureBug(5, nil), storyFailureBug(5, nil), &github.Snapshot{Number: 1, Type: "story"}, false},
	} {
		d := storyFailureDaemon("", "")
		delete(d.snaps, 1)
		if tt.story != nil {
			d.snaps[1] = tt.story
		}
		if got := d.failureAside(tt.holder, tt.taker); got != tt.want {
			t.Errorf("%s: failureAside = %t; want %t", tt.name, got, tt.want)
		}
	}
}

// The worker is free for an open failure's bug of a story while only a
// task of that story holds it, at in-progress or verifying, and for no
// other task, nor while a lease holds it, or step 1 didn't finish (SPEC
// §8.1's One task at a time). The pick takes only that bug past the
// holder (pickAside), and any task with no holder.
func TestWorkerFreeForFailureBug(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"stage:in-progress", "stage:verifying"} {
		sibling := &github.Snapshot{Number: 2, Type: "task", Parent: 1, Labels: []string{stage}}
		bug := storyFailureBug(5, func(s *github.Snapshot) { s.Labels = []string{"stage:pr-open"} })
		other := &github.Snapshot{Number: 6, Type: "task", Parent: 1, Labels: []string{"stage:pr-open"}}
		d := storyFailureDaemon("", "", sibling, bug, other)
		if free, why := d.workerFree(5); !free || why != "" {
			t.Errorf("%s: workerFree for the bug = %t, %q; want free", stage, free, why)
		}
		if free, why := d.workerFree(6); free || why != "#2, at "+stage+", holds it" {
			t.Errorf("%s: workerFree for another task = %t, %q; want #2 holding it", stage, free, why)
		}
		d.workerLeased = true
		if free, why := d.workerFree(5); free || why != "a run's lease holds it" {
			t.Errorf("%s: workerFree for the bug with a lease = %t, %q; want the lease holding it", stage, free, why)
		}
		d.workerLeased, d.step1Gaps = false, map[int]step1Gap{7: {why: "its read failed"}}
		if free, _ := d.workerFree(5); free {
			t.Errorf("%s: workerFree for the bug with a gap in step 1 = true; want false", stage)
		}
		r := &Runs{Daemon: d}
		if !r.pickAside([]*github.Snapshot{sibling}, bug) || r.pickAside([]*github.Snapshot{sibling}, other) || !r.pickAside(nil, other) {
			t.Errorf("%s: pickAside past #2 = %t for the bug, %t for another task, and %t for it with no holder; want true, false, true",
				stage, r.pickAside([]*github.Snapshot{sibling}, bug), r.pickAside([]*github.Snapshot{sibling}, other), r.pickAside(nil, other))
		}
	}
}
