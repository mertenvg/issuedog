package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A child a re-plan drops while its PR is open (SPEC §6.4, §7.2, §9.4,
// §9.6): against the fakes, with step 1 wired as main wires it, on
// changedStory's story #1 with the owner's 👍 on plan revision 2, which
// drops T2 and T3 (droppedRunRevision). T2's child, #3, has the App's PR
// #6 open, from droppedPRBranch to main, named by its move into pr-open,
// and the bug filed by hand, #5, is blocked by it. T3's child, #4, has no
// PR, and closes as not planned. The owner is notify and the code owner,
// and mallory is a writer who isn't trusted.

// droppedPRBranch is T2's task branch, one commit past main.
const droppedPRBranch = "issuedog/3-refuse-an-expired-quote-at-booking"

// droppedPRTitle is T2's PR's title.
const droppedPRTitle = "feat: refuse booking an expired quote"

// droppedPROrigin is newOrigin with the owner as notify, opus allowed, as
// revision 1 gave T2, and droppedPRBranch pushed.
func droppedPROrigin(t testing.TB) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nnotify: [owner]\nmodels: {allowed: [opus]}\n")
	gitIn(t, o.work, "checkout", "--quiet", "-b", "task")
	if err := os.WriteFile(filepath.Join(o.work, "quote.txt"), []byte("expired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, o.work, "add", "quote.txt")
	gitIn(t, o.work, "commit", "--quiet", "-m", "refuse an expired quote")
	gitIn(t, o.work, "push", "--quiet", o.url, "task:refs/heads/"+droppedPRBranch)
	gitIn(t, o.work, "checkout", "--quiet", "main")
	return o
}

// droppedPROpen opens the App's PR for task n from droppedPRBranch on o
// to main, saying it closes n, and moves n into pr-open naming it, as the
// PR step does, from stage. It returns the PR's number.
func droppedPROpen(t testing.TB, f *githubtest.Fake, o *origin, n int, stage string) int {
	t.Helper()
	f.SetGitRemote(o.url)
	pr, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r",
		github.NewPullRequest{Title: droppedPRTitle, Head: droppedPRBranch, Base: "main", Body: fmt.Sprintf("Closes #%d\n\nWhat changed.", n)})
	if err != nil {
		t.Fatal(err)
	}
	head := gitIn(t, o.url, "rev-parse", "refs/heads/"+droppedPRBranch)
	f.AddComment(n, app, fmt.Sprintf("Verified.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head=%s pr=%d -->", head, pr.Number))
	f.RemoveLabel(n, app, "stage:"+stage)
	f.AddLabel(n, app, "stage:pr-open")
	return pr.Number
}

// droppedPRAt is changedStory's edit that puts T2's child at stage, with
// its PR, #6, open (droppedPROpen): at pr-open; at in-progress, sent back
// by a review since; or held by the owner's /hold at pr-open. The bug
// filed by hand is blocked by it.
func droppedPRAt(t testing.TB, o *origin, stage string) func(*githubtest.Fake, changedIDs) {
	return func(f *githubtest.Fake, r changedIDs) {
		if pr := droppedPROpen(t, f, o, r.t2, "in-progress"); pr != 6 {
			t.Fatalf("T2's PR is #%d, want #6", pr)
		}
		switch stage {
		case "in-progress":
			f.AddComment(r.t2, app, "Sent back by a review.\n\n<!-- issuedog:transition from=pr-open to=in-progress event=r5 -->")
			f.RemoveLabel(r.t2, app, "stage:pr-open")
			f.AddLabel(r.t2, app, "stage:in-progress")
		case "held":
			hold := f.AddComment(r.t2, "owner", "/hold")
			f.AddComment(r.t2, app, fmt.Sprintf("On hold.\n\n<!-- issuedog:transition from=pr-open to=held at=pr-open reason=owner cmd=%d -->", hold))
			f.AddReaction(hold, app, "+1")
			f.RemoveLabel(r.t2, app, "stage:pr-open")
			f.AddLabel(r.t2, app, "stage:held")
		}
		f.SetBlockedBy(r.other, r.t2)
	}
}

// droppedPRFake is changedStory with droppedRunRevision as plan revision
// 2, T2's child at stage with its PR open (droppedPRAt), and no write
// logged.
func droppedPRFake(t testing.TB, o *origin, stage string) (*githubtest.Fake, changedIDs) {
	t.Helper()
	f := newFake(t)
	r := changedStoryWith(t, f, false, droppedPRAt(t, o, stage), changedPlanData(t, droppedRunRevision(t)))
	return f, r
}

// droppedPRDaemon is a daemon of o/r on f built as main builds it
// (digestRunDaemon), with the digest and assignment as given.
func droppedPRDaemon(t testing.TB, f *githubtest.Fake, o *origin, digest, assign bool) *daemon.Daemon {
	t.Helper()
	d := digestRunDaemon(t, f, o, nil, nil)
	d.Digest, d.Assign = digest, assign
	return d
}

// droppedPRMarker is the transition marker of issue n's newest comment,
// without seen, which the round bound sets.
func droppedPRMarker(t *testing.T, f *githubtest.Fake, n int) map[string]string {
	t.Helper()
	fields := maps.Clone(changedLastMarker(t, f, n).Fields)
	delete(fields, "seen")
	return fields
}

// droppedPRNewest is the body of issue n's newest comment.
func droppedPRNewest(f *githubtest.Fake, n int) string {
	cs := f.State().Issues[n-1].Comments
	return cs[len(cs)-1].Body
}

// droppedPRHoldText starts the comment of T2's hold for PR #6, which links
// plan, revision 2's plan comment.
func droppedPRHoldText(plan int64) string {
	return fmt.Sprintf("On hold, not closed, since #1's plan, rev 2, drops T2, while its PR #6 is open ([the plan](https://github.com/o/r/issues/1#issuecomment-%d)).\n\n"+
		"Once the PR merges, its `Closes` closes this issue, and once it's closed without merging, I close this issue as not planned. "+
		"Or close it yourself. `/resume` doesn't end this hold.\n\n", plan)
}

// droppedPRItem is the digest's lines of T2's item, which link plan.
func droppedPRItem(plan int64) string {
	return fmt.Sprintf("- On hold, not closed, since #1's plan, rev 2, drops T2, while its PR #6 is open ([the plan](https://github.com/o/r/issues/1#issuecomment-%d)).\n"+
		"- **PR #6**: %s\n"+
		"- Merge the PR, and its `Closes` closes this task; close the PR, and I close the task as not planned; or close the task yourself. `/resume` doesn't end this hold.\n"+
		"- Answer here: https://github.com/o/r/pull/6", plan, droppedPRTitle)
}

// droppedPRPass is a pass for Replay: two passes of a fresh daemon of o
// on f each (droppedPRDaemon), as after a restart, with the digest off and
// assignment as given, so the close judge's stage on an issue a pass
// closed is set in a clean run too.
func droppedPRPass(t *testing.T, o *origin, assign bool) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		var errs []error
		for range 2 {
			errs = append(errs, droppedPRDaemon(t, f, o, false, assign).Iterate(t.Context()))
		}
		return errors.Join(errs...)
	}
}

// droppedPRQuiet runs a pass of d that must write nothing, and send only
// conditional GETs that GitHub answers 304, as an unchanged poll does.
func droppedPRQuiet(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, step string) {
	t.Helper()
	from, writes := len(f.Requests()), len(f.Writes())
	iterate(t, d)
	if got := writesOf(f)[writes:]; len(got) != 0 {
		t.Errorf("%s: the pass wrote %q; want nothing", step, got)
	}
	reqs := f.Requests()[from:]
	if len(reqs) == 0 {
		t.Fatalf("%s: the pass sent no request; want its poll", step)
	}
	for _, r := range reqs {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("%s: the pass sent %+v; want only conditional GETs answered 304", step, r)
		}
	}
}

// The owner's 👍 on the revision that drops T2 while its PR is open, at
// pr-open, at in-progress after a review sent it back, or held by the
// owner at pr-open: T2's child, #3, is held, not closed, with reason=dropped,
// at the stage it was at, or for the held one, by a held → held marker
// that keeps its first hold's at, keyed as its close would be, its
// comment naming the revision and the PR and linking the plan. It keeps
// its link to the bug. T3's child, #4, with no PR, closes as not planned,
// and the record stands, naming neither. On the next pass, T2's child has
// the digest's item, dropped-3, naming the revision and PR #6, saying how
// it ends, and linking the PR, and notify is assigned to it, not to the
// PR. A pass after that, with nothing changed, sends only conditional
// GETs answered 304, and writes nothing.
func TestDroppedPRHold(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	for _, tt := range []struct {
		stage, from, at string
	}{
		{"pr-open", "pr-open", "pr-open"},
		{"in-progress", "in-progress", "in-progress"},
		{"held", "held", "pr-open"},
	} {
		t.Run(tt.stage, func(t *testing.T) {
			t.Parallel()
			f, r := droppedPRFake(t, o, tt.stage)
			d := droppedPRDaemon(t, f, o, true, true)
			iterate(t, d)
			p, plan := changedApproval(t, f)
			issues := f.State().Issues
			if t2 := issues[r.t2-1]; t2.Closed || pickStage(f, r.t2) != "stage:held" || !slices.Equal(issues[r.other-1].BlockedBy, []int{r.t2}) {
				t.Errorf("T2's child is closed %t, at %s, and the bug blocked by %v; want it open and held, still blocking the bug",
					t2.Closed, pickStage(f, r.t2), issues[r.other-1].BlockedBy)
			}
			if got := droppedPRNewest(f, r.t2); !strings.HasPrefix(got, droppedPRHoldText(plan)) {
				t.Errorf("T2's child's newest comment is\n%s\nwant it to start\n%s", got, droppedPRHoldText(plan))
			}
			want := map[string]string{"from": tt.from, "to": "held", "at": tt.at, "reason": "dropped", "event": fmt.Sprintf("p%d", p)}
			if got := droppedPRMarker(t, f, r.t2); !reflect.DeepEqual(got, want) {
				t.Errorf("T2's child's hold has %v; want %v", got, want)
			}
			if t3 := issues[r.t3-1]; !t3.Closed || droppedReason(t, f, r.t3) != "not_planned" {
				t.Errorf("T3's child is closed %t, as %q; want it closed as not planned", t3.Closed, droppedReason(t, f, r.t3))
			}
			recs := droppedRecords(f, 1)
			if len(recs) != 1 || strings.Contains(recs[0], fmt.Sprintf("#%d:", r.t2)) || strings.Contains(recs[0], fmt.Sprintf("#%d:", r.t3)) {
				t.Errorf("issue 1's records are %q; want one, naming neither dropped child", recs)
			}

			iterate(t, d)
			digest := f.State().Issues[len(f.State().Issues)-1]
			if !strings.Contains(digest.Body, "**Dropped with an open PR**") || !strings.Contains(digest.Body, droppedPRItem(plan)) {
				t.Errorf("the digest =\n%s\nwant T2's item, ending\n%s", digest.Body, droppedPRItem(plan))
			}
			if !strings.Contains(droppedPRNewest(f, digest.Number), "dropped-3") {
				t.Errorf("the digest's newest comment is\n%s\nwant it to announce dropped-3", droppedPRNewest(f, digest.Number))
			}
			if task, pr := assignees(f, r.t2), assignees(f, 6); !slices.Equal(task, []string{"owner"}) || len(pr) != 0 {
				t.Errorf("T2's child's assignees = %q, its PR's %q; want owner on the task, and nobody on the PR", task, pr)
			}
			iterate(t, d) // reads its own writes
			droppedPRQuiet(t, f, d, "a pass with nothing changed")
		})
	}
}

// The hold is written once, whatever write a pass crashes at, then a
// fresh daemon's pass (SPEC §5.2): the approval, T4's child, the hold,
// T3's child's close and the record, each checked first.
func TestDroppedPRHoldReplay(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	setup := func(tb testing.TB) *githubtest.Fake {
		f, _ := droppedPRFake(tb, o, "pr-open")
		return f
	}
	githubtest.Replay(t, setup, droppedPRPass(t, o, false))
}

// droppedPRSettled is droppedPRFake at pr-open after two passes of a
// daemon with the digest and assignment off, which hold T2's child and
// judge T3's child's close, with no write logged since.
func droppedPRSettled(t testing.TB, o *origin) (*githubtest.Fake, changedIDs) {
	t.Helper()
	f, r := droppedPRFake(t, o, "pr-open")
	d := droppedPRDaemon(t, f, o, false, false)
	for range 2 {
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	f.ForgetWrites()
	return f, r
}

// notify's assignment to the held T2's child is written once, whatever
// write a pass crashes at, then a fresh daemon's pass.
func TestDroppedPRAssignReplay(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	setup := func(tb testing.TB) *githubtest.Fake {
		f, _ := droppedPRSettled(tb, o)
		return f
	}
	clean := setup(t)
	if err := droppedPRDaemon(t, clean, o, false, true).Iterate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got, want := assignWrites(clean, 0), []string{assignAdd(3, "owner")}; !slices.Equal(got, want) {
		t.Errorf("the pass's assignee writes are %q; want %q", got, want)
	}
	githubtest.Replay(t, setup, droppedPRPass(t, o, true))
}

// Once T2's PR closes without merging, the next pass removes the held
// child's link to the bug, then closes it as not planned, keyed apart from
// its hold, saying why, and it leaves the digest. A merge, by mallory,
// whom only the merge makes count, closes it by its "Closes #3", which the
// close judge accepts though it's held, and so does the owner's close:
// neither gets a comment, and each gets stage:completed. Each crash, then
// a fresh daemon, writes nothing twice.
func TestDroppedPREnd(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	for _, tt := range []struct {
		name   string
		end    func(f *githubtest.Fake)
		reason string
		stage  string
		closed bool // by the App's comment
	}{
		{"PR closed", func(f *githubtest.Fake) { f.ClosePR(6, "owner") }, "not_planned", "stage:held", true},
		{"PR merged", func(f *githubtest.Fake) { f.MergePR(6, "mallory") }, "completed", "stage:completed", false},
		{"closed by the owner", func(f *githubtest.Fake) { f.CloseBy(3, "owner") }, "completed", "stage:completed", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := func(tb testing.TB) *githubtest.Fake {
				f, _ := droppedPRSettled(tb, o)
				tt.end(f)
				f.ForgetWrites()
				return f
			}
			f := setup(t)
			p, _ := changedApproval(t, f)
			comments := len(f.State().Issues[2].Comments)
			d := droppedPRDaemon(t, f, o, true, true)
			iterate(t, d)
			t2 := f.State().Issues[2]
			if !t2.Closed || droppedReason(t, f, 3) != tt.reason || pickStage(f, 3) != tt.stage {
				t.Errorf("T2's child is closed %t, as %q, at %s; want it closed as %s, at %s", t2.Closed, droppedReason(t, f, 3), pickStage(f, 3), tt.reason, tt.stage)
			}
			if tt.closed {
				want := fmt.Sprintf("Closed as not planned: its PR #6 was closed without merging, and a plan's revision dropped it.\n\n"+
					"<!-- issuedog:transition from=held to=closed reason=not_planned event=p%d-closed -->", p)
				if got := droppedPRNewest(f, 3); got != want || len(t2.Comments) != comments+1 {
					t.Errorf("T2's child's comments since are %d, its newest\n%s\nwant one\n%s", len(t2.Comments)-comments, got, want)
				}
				if got := f.State().Issues[4].BlockedBy; len(got) != 0 {
					t.Errorf("the bug is blocked by %v; want nothing, as T2's child closed as not planned", got)
				}
			} else if len(t2.Comments) != comments {
				t.Errorf("T2's child got %d comments; want none: the close isn't the App's", len(t2.Comments)-comments)
			}
			digest := f.State().Issues[len(f.State().Issues)-1]
			if strings.Contains(digest.Body, "Dropped with an open PR") {
				t.Errorf("the digest =\n%s\nwant no dropped item", digest.Body)
			}
			githubtest.Replay(t, setup, droppedPRPass(t, o, false))
		})
	}
}

// A drop while T2's PR is listed closed, or merged with no "Closes" line,
// closes T2's child as not planned, as any dropped child: only an open PR
// holds it.
func TestDroppedPRNotOpen(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	for name, end := range map[string]func(f *githubtest.Fake){
		"closed": func(f *githubtest.Fake) { f.ClosePR(6, "owner") },
		"merged": func(f *githubtest.Fake) {
			f.EditBody(6, app, "What changed.")
			f.MergePR(6, "owner")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, r := droppedPRFake(t, o, "pr-open")
			end(f)
			f.ForgetWrites()
			iterate(t, droppedPRDaemon(t, f, o, false, false))
			if t2 := f.State().Issues[r.t2-1]; !t2.Closed || droppedReason(t, f, r.t2) != "not_planned" ||
				!strings.HasPrefix(droppedPRNewest(f, r.t2), "Closed as not planned, since #1's plan, rev 2, drops T2 ") {
				t.Errorf("T2's child is closed %t, as %q, its newest comment\n%s\nwant it closed as not planned by the revision",
					t2.Closed, droppedReason(t, f, r.t2), droppedPRNewest(f, r.t2))
			}
		})
	}
}

// A merged PR is left for its "Closes #N" to close the task: on a pass
// whose poll lists the PR merged while the task is still open, as when
// GitHub closes the issue a moment after the merge, or never, as with no
// "Closes" line, the held task gets no write, and isn't closed as not
// planned.
func TestDroppedPRMergedOpen(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	f, r := droppedPRSettled(t, o)
	f.EditBody(6, app, "What changed.")
	f.MergePR(6, "owner")
	f.ForgetWrites()
	d := droppedPRDaemon(t, f, o, false, false)
	iterate(t, d)
	iterate(t, d)
	if t2 := f.State().Issues[r.t2-1]; t2.Closed || pickStage(f, r.t2) != "stage:held" {
		t.Errorf("T2's child is closed %t, at %s; want it held still, for its merge's close", t2.Closed, pickStage(f, r.t2))
	}
	droppedNoWrite(t, f, r.t2, "held, its PR merged")
}

// /resume on the held T2's child, or on the story above it, ends no hold
// for its PR: the App 👎s it and replies that it waits for PR #6, and the
// child stays held.
func TestDroppedPRResume(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	for _, tt := range []struct {
		name string
		on   int
		who  string
	}{
		{"on the child", 3, "this issue"},
		{"on the story", 1, "#3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f, _ := droppedPRSettled(t, o)
			d := droppedPRDaemon(t, f, o, false, false)
			iterate(t, d)
			cmd := f.AddComment(tt.on, "owner", "/resume")
			iterate(t, d)
			want := fmt.Sprintf("> /resume\n\nI didn't apply this command: %s is on hold for its PR #6, as a plan's revision dropped it: "+
				"it closes once the PR merges or closes, and `/resume` doesn't end that hold.\n\n<!-- issuedog:reply reason=rejected cmd=%d -->", tt.who, cmd)
			if got := droppedPRNewest(f, tt.on); got != want {
				t.Errorf("#%d's newest comment is\n%s\nwant\n%s", tt.on, got, want)
			}
			if pickStage(f, 3) != "stage:held" || f.State().Issues[2].Closed {
				t.Errorf("T2's child is at %s; want it held still", pickStage(f, 3))
			}
		})
	}
}

// /resume on the story while T2's child is held for its PR and, after
// mallory's comment there, for discussion too: the reply names both holds
// on it, neither of which a /resume on the story ends.
func TestDroppedPRResumeDiscussed(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	f, _ := droppedPRSettled(t, o)
	d := droppedPRDaemon(t, f, o, false, false)
	f.AddComment(3, "mallory", "Keep this one, please.")
	iterate(t, d)
	iterate(t, d)
	cmd := f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	want := fmt.Sprintf("> /resume\n\nI didn't apply this command: #3 is on hold for its PR #6, as a plan's revision dropped it: "+
		"it closes once the PR merges or closes, and `/resume` doesn't end that hold; and #3 is on hold for discussion, "+
		"which only a code owner's response there, or `/resume` there, ends.\n\n<!-- issuedog:reply reason=rejected cmd=%d -->", cmd)
	if got := droppedPRNewest(f, 1); got != want {
		t.Errorf("the story's newest comment is\n%s\nwant\n%s", got, want)
	}
}

// /hold on the story, then /resume there, while T2's child is held for its
// PR: the story leaves held, and so does T4's child, but T2's child stays
// held, as /resume ends no hold for a PR. The story's move says so, and
// T2's child gets the record of the owner hold that ended on it, saying it
// stays on hold for its PR.
func TestDroppedPRResumeAbove(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	f, _ := droppedPRSettled(t, o)
	d := droppedPRDaemon(t, f, o, false, false)
	f.AddComment(1, "owner", "/hold")
	iterate(t, d)
	iterate(t, d)
	if pickStage(f, 1) != "stage:held" || pickStage(f, 7) != "stage:held" {
		t.Fatalf("after /hold, the story is at %s, T4's child at %s; want both held", pickStage(f, 1), pickStage(f, 7))
	}
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	if pickStage(f, 1) != "stage:planned" || pickStage(f, 7) != "stage:ready" || pickStage(f, 3) != "stage:held" {
		t.Errorf("after /resume, the story is at %s, T4's child at %s, T2's at %s; want planned, ready and held",
			pickStage(f, 1), pickStage(f, 7), pickStage(f, 3))
	}
	if got, want := droppedPRNewest(f, 1), " Still on hold for a PR, as a plan's revision dropped it: #3. `/resume` doesn't end that hold.\n\n"; !strings.Contains(got, want) ||
		strings.Contains(got, "Still on hold: ") {
		t.Errorf("the story's newest comment is\n%s\nwant it to say\n%s\nand name no owner hold left", got, want)
	}
	if got, want := droppedPRNewest(f, 3), "`/resume` on #1 ended the owner hold on this issue, but it stays on hold for a PR, as a plan's revision dropped it, "+
		"which `/resume` doesn't end.\n\n"; !strings.HasPrefix(got, want) {
		t.Errorf("T2's child's newest comment is\n%s\nwant it to start\n%s", got, want)
	}
}

// droppedPREpic is droppedEpic's epic, whose revision 2 drops the story
// S1, #2, with S1's T1, #4, at pr-open, its PR, #9, open
// (droppedPROpen), and no write logged.
func droppedPREpic(t testing.TB, o *origin) *githubtest.Fake {
	t.Helper()
	f := droppedEpic(func(t testing.TB) map[string]any { return droppedEpicRevision(t) })(t)
	if pr := droppedPROpen(t, f, o, 4, "ready"); pr != 9 {
		t.Fatalf("T1's PR is #%d, want #9", pr)
	}
	f.ForgetWrites()
	return f
}

// droppedPREpicSettled is droppedPREpic after two passes of a daemon with
// the digest and assignment off, which apply the revision and judge its
// closes, with no write logged since.
func droppedPREpicSettled(t testing.TB, o *origin) *githubtest.Fake {
	t.Helper()
	f := droppedPREpic(t, o)
	d := droppedPRDaemon(t, f, o, false, false)
	for range 2 {
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	f.ForgetWrites()
	return f
}

// An epic's revision drops the story S1 while its T1's PR is open: T1's
// #4 is held for its PR, and S1's #2, above it, is held for it too, with
// a comment saying an issue below it waits for its PR, T1's hold written
// first. The rest of S1's subtree closes as not planned, the owner's hold
// on its T2 included. /resume on S1 ends neither hold, and its reply names
// both. Once T1's PR closes without merging, one pass closes T1, then S1,
// each as not planned, keyed apart from its hold, and a crash at any write
// of those, then a fresh daemon, writes nothing twice. The holds are
// written once whatever write a pass crashes at, too.
func TestDroppedPRStory(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake { return droppedPREpic(tb, o) }, droppedPRPass(t, o, false))

	f := droppedPREpic(t, o)
	d := droppedPRDaemon(t, f, o, false, false)
	iterate(t, d)
	p, plan := changedApproval(t, f)
	link := fmt.Sprintf("([the plan](https://github.com/o/r/issues/1#issuecomment-%d))", plan)
	for _, tt := range []struct {
		n        int
		from, at string
		text     string
	}{
		{4, "pr-open", "pr-open", "On hold, not closed, since #1's plan, rev 2, drops S1, #2, which this issue is below, while its PR #9 is open " + link + ".\n\n"},
		{2, "planned", "planned", "On hold, not closed, since #1's plan, rev 2, drops S1, while an issue below it waits for its PR " + link + ".\n\n" +
			"Once nothing below it is open, I close this issue as not planned. `/resume` doesn't end this hold.\n\n"},
	} {
		want := map[string]string{"from": tt.from, "to": "held", "at": tt.at, "reason": "dropped", "event": fmt.Sprintf("p%d", p)}
		if got := droppedPRMarker(t, f, tt.n); !reflect.DeepEqual(got, want) || pickStage(f, tt.n) != "stage:held" {
			t.Errorf("#%d is at %s, its newest marker %v; want it held, by %v", tt.n, pickStage(f, tt.n), got, want)
		}
		if got := droppedPRNewest(f, tt.n); !strings.HasPrefix(got, tt.text) {
			t.Errorf("#%d's newest comment is\n%s\nwant it to start\n%s", tt.n, got, tt.text)
		}
	}
	writes := writesOf(f)
	if t1, s1 := slices.Index(writes, "POST /issues/4/comments"), slices.Index(writes, "POST /issues/2/comments"); t1 < 0 || s1 < t1 {
		t.Errorf("the pass wrote %q; want T1's hold, then S1's", writes)
	}
	for _, n := range []int{5, 7} {
		if is := f.State().Issues[n-1]; !is.Closed || droppedReason(t, f, n) != "not_planned" {
			t.Errorf("#%d is closed %t, as %q; want it closed as not planned", n, is.Closed, droppedReason(t, f, n))
		}
	}

	iterate(t, d)
	cmd := f.AddComment(2, "owner", "/resume")
	iterate(t, d)
	want := fmt.Sprintf("> /resume\n\nI didn't apply this command: #4 is on hold for its PR #9, as a plan's revision dropped it: "+
		"it closes once the PR merges or closes, and `/resume` doesn't end that hold; and this issue is on hold for the issues below it, "+
		"as a plan's revision dropped it: it closes once nothing below it is open, and `/resume` doesn't end that hold.\n\n"+
		"<!-- issuedog:reply reason=rejected cmd=%d -->", cmd)
	if got := droppedPRNewest(f, 2); got != want {
		t.Errorf("S1's newest comment is\n%s\nwant\n%s", got, want)
	}

	f.ClosePR(9, "owner")
	iterate(t, d)
	for _, tt := range []struct {
		n    int
		text string
	}{
		{4, "Closed as not planned: its PR #9 was closed without merging, and a plan's revision dropped it."},
		{2, "Closed as not planned: nothing below this issue is open now, and a plan's revision dropped it."},
	} {
		want := fmt.Sprintf("%s\n\n<!-- issuedog:transition from=held to=closed reason=not_planned event=p%d-closed -->", tt.text, p)
		if is := f.State().Issues[tt.n-1]; !is.Closed || droppedReason(t, f, tt.n) != "not_planned" || droppedPRNewest(f, tt.n) != want {
			t.Errorf("#%d is closed %t, as %q, its newest comment\n%s\nwant it closed as not planned, by\n%s",
				tt.n, is.Closed, droppedReason(t, f, tt.n), droppedPRNewest(f, tt.n), want)
		}
	}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := droppedPREpicSettled(tb, o)
		f.ClosePR(9, "owner")
		f.ForgetWrites()
		return f
	}, droppedPRPass(t, o, false))
}

// Through the loop, as TestChildrenDroppedRunResult, but with T2's PR open
// while a review has sent it back to in-progress: its run's result fails
// to apply, its move written or not, so the lease waits. Then the owner's
// 👍 on the revision that drops T2 is handled, by the same daemon or a new
// one, as after a restart: T2's child is held for its PR, not closed, and
// its waiting result is dropped, uncounted, with nothing of it written:
// not its move, if it wasn't written, nor its follow-up. The lease and the
// run's files go, and no run starts again.
func TestDroppedPRRunResult(t *testing.T) {
	t.Parallel()
	for name, tt := range map[string]struct{ before, restart bool }{
		"unmoved":                  {before: true},
		"moved":                    {},
		"unmoved, after a restart": {before: true, restart: true},
		"moved, after a restart":   {restart: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o, root := droppedPROrigin(t), t.TempDir()
			f, r := droppedPRFake(t, o, "in-progress")
			newDroppedRig := func(st *droppedRunStage) *rig {
				g := newRig(t, f, o, root)
				g.claude(okOut, 0)
				st.fakeStage = &fakeStage{agent: "implementer", pending: func(in daemon.Input) bool { return in.S.Number == r.t2 }}
				g.r.Stages = map[state.Stage]daemon.Stage{state.InProgress: st}
				return g
			}
			g := newDroppedRig(&droppedRunStage{before: tt.before})

			if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "applying failed") {
				t.Fatalf("the run's pass returned %v, want its failed apply", err)
			}
			if l := g.lease(); g.runs() != 1 || l.RunID != "run1" || l.Failed || l.ApplyFailures != 1 {
				t.Fatalf("after the run's pass: %d runs, lease %+v; want run1's lease waiting, with one failed apply", g.runs(), l)
			}
			moved := marked(f, r.t2, "run1")
			if moved == tt.before {
				t.Fatalf("run1's move on T2's child written %t, want %t", moved, !tt.before)
			}
			comments := len(f.State().Issues[r.t2-1].Comments)

			if tt.restart {
				g = newDroppedRig(&droppedRunStage{before: tt.before, failed: true})
			}
			g.d.Step.Reactions = daemon.ApproveByReaction(g.d.Step.CurrentTrust())
			g.d.Step.Models = g.d.CurrentModels
			g.pass()
			t2 := f.State().Issues[r.t2-1]
			if t2.Closed || pickStage(f, r.t2) != "stage:held" {
				t.Fatalf("T2's child is closed %t, at %s; want it held for its PR", t2.Closed, pickStage(f, r.t2))
			}
			if got := len(t2.Comments); got != comments+1 || !strings.HasPrefix(t2.Comments[got-1].Body, "On hold, not closed, since #1's plan, rev 2, drops T2, while its PR #6 is open ") {
				t.Errorf("T2's child's comments since the run's pass are %+v; want only the hold", t2.Comments[comments:])
			}
			if got := marked(f, r.t2, "run1"); got != moved || slices.ContainsFunc(t2.Comments, func(c githubtest.CommentState) bool { return c.Body == followUp }) {
				t.Errorf("T2's child has run1's move %t, and its follow-up or not; want the move %t, as before the drop, and no follow-up", got, moved)
			}
			if l := g.lease(); l.RunID != "" || l.Failed || g.runs() != 1 {
				t.Errorf("lease %+v, %d runs; want the result dropped, uncounted, and no run again", l, g.runs())
			}
			if left := g.leftovers(); len(left) != 0 {
				t.Errorf("left %v, want nothing of the dropped run", left)
			}
		})
	}
}
