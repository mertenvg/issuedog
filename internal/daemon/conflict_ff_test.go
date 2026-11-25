package daemon_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A conflict bug's verified head is fast-forwarded onto its story branch
// (SPEC §7.2, §7.5's A conflict), on verdictRigs wired as main's, with
// real temporary repos: story 1, whose branch the tests cut and push to
// in the origin itself, as an admin or a person may, and main, which they
// move, so step 2's merge of main into the branch conflicts and files the
// conflict bug, which the pick takes. Its run merges the tip into the main
// commit it names, resolving the conflict, its checks and its verifier
// pass, and the PR step opens its PR into the story branch. Then, in
// place of the squash, the App pushes the head to the story branch, leased
// at the tip it was verified against, and the fake marks the PR merged by
// the App, as GitHub does a push that brings a PR's head into its base
// (SPEC §16), so the next pass closes the bug and deletes its branch. A tip
// that moved since its verify sends the bug back instead, once it passes
// rule 2, with no checks run.

// conflictFF is a conflict bug at pr-open, as conflictFFReady leaves it:
// the fake and the rig, the bug's number, its PR's, the story branch's tip
// its head was verified against, the main commit it names, and the head.
type conflictFF struct {
	f               *githubtest.Fake
	v               *verdictRig
	bug, pr         int
	tip, main, head string
}

// conflictFFSiblingTitle is the title of the sibling task conflictFFSibling
// files in story 1.
const conflictFFSiblingTitle = "Refuse a stale quote"

// conflictFFReady files story 1 on a new rig, cuts its branch, pushes a
// commit of the story's to it that changes f.txt, checks that tip as
// success, then moves main with its own change to f.txt, which conflicts,
// so the pass after files the conflict bug at ready. With sibling, it
// files a task in the story first, #3, at needs-human, where it's neither
// picked nor merged, so the bug, #4, is numbered after it
// (conflictFF.siblingOpen). The bug's first run merges the tip into the
// main commit, resolving f.txt, then the checks pass, then the verifier,
// and the PR step opens its PR into the story branch and moves it to
// pr-open, with tip= the tip, each on its own pass.
func conflictFFReady(t *testing.T, sibling bool) *conflictFF {
	t.Helper()
	f := newFake(t)
	v := newVerdictRig(t, f, 3).conflictRunWire()
	c := &conflictFF{f: f, v: v, bug: conflictRunBug}
	tipStory(t, f)
	v.passes(2)
	if sibling {
		conflictFFSibling(f)
		c.bug++
	}
	v.tipCut(tipBranch)
	c.tip = v.tipPush(tipBranch, map[string]string{"f.txt": "the story\n", "story.txt": "the story's change\n"})
	v.tipJudged(c.tip, 1, []githubtest.CheckRunState{tipCheck(c.tip, tipBranch, "success")})
	c.main = v.storyMainMoved("f.txt", "main's\n")
	v.pass()
	if n := len(f.State().Issues); n != c.bug || !strings.HasSuffix(f.State().Issues[n-1].Body, storyMainConflictOrigin(c.tip, c.main)) {
		t.Fatalf("the fake's newest issue is #%d =\n%+v\nwant the conflict bug, #%d, for main's %s and the tip %s",
			n, f.State().Issues[n-1], c.bug, c.main, c.tip)
	}
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(1, map[string]string{"f.txt": "both\n"})
	v.out(2, verifierPass(t, "The resolution keeps both."))
	v.pass()
	if v.runs() != 1 || pickStage(f, c.bug) != "stage:verifying" {
		t.Fatalf("the pick's pass ran claude %d times, and left the bug at %s; want once, and verifying", v.runs(), pickStage(f, c.bug))
	}
	c.head = v.trimmed("commit.1")
	v.passes(3)
	prs := f.State().PRs
	if v.runs() != 2 || len(prs) != 1 || prs[0].HeadSHA != c.head || prs[0].Base != tipBranch || prs[0].State != "open" || pickStage(f, c.bug) != "stage:pr-open" {
		t.Fatalf("the checks', the verifier's and the PR step's passes ran claude %d times, left the PRs %+v, and the bug at %s; want twice, one open into %s at %s, and pr-open",
			v.runs(), prs, pickStage(f, c.bug), tipBranch, c.head)
	}
	c.pr = prs[0].Number
	moves := storyFailureComments(f, c.bug, "<!-- issuedog:transition from=verifying to=pr-open ")
	if len(moves) != 1 || prMoveFields(t, moves[0])["tip"] != c.tip {
		t.Fatalf("the bug's moves to pr-open = %q; want one, with tip=%s", moves, c.tip)
	}
	return c
}

// branch is the conflict bug's own branch in the origin, by its prefix,
// or "" when it's gone.
func (c *conflictFF) branch() string {
	c.v.t.Helper()
	return gitIn(c.v.t, c.v.o.url, "for-each-ref", "--format=%(refname:short)", "refs/heads/issuedog/"+strconv.Itoa(c.bug)+"-")
}

// pushedNone fails, naming step, unless the story branch is at want in the
// origin, and the App sent no merge of the bug's PR, PUT /pulls/{n}/merge,
// in all of c's writes: a conflict bug is never squashed.
func (c *conflictFF) pushedNone(step, want string) {
	c.v.t.Helper()
	if got, merges := c.v.originTip(tipBranch), storyMerges(c.f, 0, c.pr); got != want || len(merges) != 0 {
		c.v.t.Errorf("%s: the story branch is at %s, with the merges %+v; want it at %s, and none", step, got, merges, want)
	}
}

// moves are the bug's moves pr-open → in-progress.
func (c *conflictFF) moves() []string {
	return storyTipMoves(c.f, c.bug)
}

// conflictFFMovedText is the comment of a conflict bug's move pr-open →
// in-progress once story 1's branch moved to tip since its PR was verified
// at head, up to its marker.
func conflictFFMovedText(head, tip string) string {
	return "The story branch `" + tipBranch + "` moved to `" + tip + "` since this conflict bug's PR was verified at `" + head + "`, which lacks it, " +
		"so the story branch can't be fast-forwarded to that head, and no checks ran on their merge. " +
		"So the bug is back at `in-progress`, for the implementer to merge the story branch `" + tipBranch + "` too, resolving any conflicts.\n\n"
}

// sentBack runs the pass that sends the bug back on tip, the story
// branch's tip that moved since its verify, and fails unless it moved it
// once, pr-open → in-progress, keyed event=b<tip> by that tip, with
// conflictFFMovedText, running no command, reading no check run of the
// head's, and posting no check, and pushed nothing, the branch still at
// tip. It returns the move's comment.
func (c *conflictFF) sentBack(tip string) string {
	c.v.t.Helper()
	seen := storyTipSeen(c.v.t, c.f, c.bug)
	onHead := func() int {
		return len(slices.DeleteFunc(c.f.State().CheckRuns, func(r githubtest.CheckRunState) bool { return r.SHA != c.head }))
	}
	from, ran, checks := len(c.f.Requests()), c.v.checksRan(), onHead()
	c.v.pass()
	storyTipMoved(c.v.t, c.f, c.bug, "stage:in-progress", conflictFFMovedText(c.head, tip), tip, seen)
	if c.v.checksRan() != ran || onHead() != checks || prMainReads(c.f, from, c.head) != 0 {
		c.v.t.Errorf("the send-back's pass ran the commands %d times, posted %d checks on the head, and read the head's checks %d times; want none of them",
			c.v.checksRan()-ran, onHead()-checks, prMainReads(c.f, from, c.head))
	}
	c.pushedNone("the send-back's pass", tip)
	return c.moves()[0]
}

// A passed conflict bug's head becomes its story branch's tip by a
// fast-forward (SPEC §7.5's A conflict): the pass after the PR step reads
// the PR again, one request, and pushes the head to the story branch,
// leased at the tip it was verified against, with no merge of the PR's,
// so the branch's history holds both the tip and the main commit the bug
// names, and step 4 checks the head afresh as the branch's new tip,
// running the commands once. The bug stays at pr-open on that pass. The
// pass after, as the fake now lists the PR merged by the App, deletes the
// bug's branch and closes the bug as completed, keyed event=g<PR>, with no
// check carried over, and the passes after read what it wrote, then make
// no request but 304s and push nothing more.
func TestConflictFFMerges(t *testing.T) {
	t.Parallel()
	c := conflictFFReady(t, false)
	f, v := c.f, c.v
	from, writes, ran := len(f.Requests()), len(f.Writes()), v.checksRan()
	v.pass()
	if got := v.originTip(tipBranch); got != c.head {
		t.Fatalf("the fast-forward's pass left the story branch at %s; want the bug's head %s", got, c.head)
	}
	for _, sha := range []string{c.tip, c.main} {
		if in := gitIn(t, v.o.url, "merge-base", c.head, sha); in != sha {
			t.Errorf("the story branch's merge base with %s is %s; want it in the branch's history", sha, in)
		}
	}
	if got, want := writesOf(f)[writes:], []string{"POST /check-runs"}; !slices.Equal(got, want) {
		t.Errorf("the fast-forward's pass wrote %q; want %q, step 4's check of the new tip", got, want)
	}
	reads := 0
	for _, r := range f.Requests()[from:] {
		if storyPRMatch(c.pr)(r) {
			reads++
		}
	}
	if reads != 1 || pickStage(f, c.bug) != "stage:pr-open" {
		t.Errorf("the fast-forward's pass read the PR %d times, and left the bug at %s; want once, and pr-open", reads, pickStage(f, c.bug))
	}
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(c.head, tipBranch, "success")) || v.checksRan() != ran+1 {
		t.Errorf("the fast-forward's pass left the checks %+v, and ran the commands %d times; want success on the head, checked afresh, once", got, v.checksRan()-ran)
	}
	for _, chk := range prMoveChecks(t, f) {
		if chk.HeadSHA == c.head && strings.Contains(chk.Output.Summary, "carried") {
			t.Errorf("the head's check = %+v; want it checked afresh, not carried over", chk)
		}
	}

	ran = v.checksRan()
	v.pass()
	i := slices.IndexFunc(f.State().PRs, func(p githubtest.PRState) bool { return p.Number == c.pr })
	if got := f.State().PRs[i]; !got.Merged || got.MergedBy != app || got.State != "closed" {
		t.Errorf("the bug's PR = %+v; want it merged and closed by %s, the pusher", got, app)
	}
	closes := storyFailureComments(f, c.bug, "<!-- issuedog:transition from=pr-open to=closed ")
	text := "The App merged PR #" + strconv.Itoa(c.pr) + " into the story branch as `" + c.head + "`, so this task is complete.\n\n"
	want := map[string]string{"from": "pr-open", "to": "closed", "reason": "completed", "event": "g" + strconv.Itoa(c.pr)}
	if len(closes) != 1 || !strings.HasPrefix(closes[0], text) || !maps.Equal(prMoveFields(t, closes[0]), want) || !f.State().Issues[c.bug-1].Closed {
		t.Fatalf("the close's pass left the bug closed %t, with the closes %q; want closed, by one, %v, starting\n%s",
			f.State().Issues[c.bug-1].Closed, closes, want, text)
	}
	if got := c.branch(); got != "" || v.checksRan() != ran {
		t.Errorf("the close's pass left the bug's branch %q, and ran the commands %d times; want it deleted, and none", got, v.checksRan()-ran)
	}
	v.passes(2)
	v.quietPass("after the close")
	v.quietPass("again")
	c.pushedNone("after the close", c.head)
	if got := pickStage(f, c.bug); got != "stage:completed" {
		t.Errorf("the bug is at %s; want stage:completed", got)
	}
}

// conflictFFSibling files a task in story 1, titled
// conflictFFSiblingTitle, at needs-human, where it's neither picked nor
// merged, and returns its number.
func conflictFFSibling(f *githubtest.Fake) int {
	n := f.AddIssue("owner", conflictFFSiblingTitle, "Booking a stale quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.SetParent(n, 1)
	f.AddLabel(n, app, "stage:needs-human")
	return n
}

// siblingOpen moves n, a sibling task conflictFFSibling filed, to pr-open,
// as its PR step would leave it: its head, a commit of its own on c's
// tip, stale.txt, on its branch in the origin, and made its head in the
// daemon's clone, its PR from that branch into the story branch open, the
// App's, and its moves to verifying and to pr-open naming the head, the
// PR and that tip. It returns the PR's number.
func (c *conflictFF) siblingOpen(n int) int {
	t, v := c.v.t, c.v
	t.Helper()
	branch, task := "issuedog/"+strconv.Itoa(n)+"-refuse-a-stale-quote", strconv.Itoa(n)
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "sibling", c.tip)
	if err := os.WriteFile(filepath.Join(w, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w, "add", "stale.txt")
	gitIn(t, w, "commit", "--quiet", "-m", "the sibling's run")
	head := gitIn(t, w, "rev-parse", "HEAD")
	gitIn(t, w, "push", "--quiet", v.o.url, "sibling:refs/heads/"+branch)
	gitIn(t, v.d.Clone.Dir(), "fetch", "--quiet", "--no-tags", w, "+refs/heads/sibling:"+repo.TasksRef+task)
	gitIn(t, w, "checkout", "--quiet", "main")
	pr := c.f.OpenPRAs(app, branch, tipBranch, "feat: "+conflictFFSiblingTitle, "Task #"+task+", in story #1\n\nThe change.")
	c.f.RemoveLabel(n, app, "stage:needs-human")
	c.f.AddLabel(n, app, "stage:pr-open")
	c.f.AddComment(n, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=rs head="+head+" -->")
	c.f.AddComment(n, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rsv head="+head+" pr="+strconv.Itoa(pr)+" tip="+c.tip+" -->")
	return pr
}

// A sibling's merge first: task #3 of the story, numbered before the
// conflict bug, at pr-open with its PR verified against the same tip,
// is squash-merged onto it first in the pass, so the bug, whose head
// lacks the sibling's squash commit, isn't fast-forwarded onto it, nor
// onto the old tip, which the branch is past (SPEC §7.5's The merge's
// guards, A conflict). The pass after carries the tip's check over to the
// squash commit and closes the sibling, then sends the bug back pr-open →
// in-progress, keyed event=b<SHA> by it, with no checks run on its merge
// with it, and its next run starts from its head, with that tip named as
// its target, to merge, and the move's comment in its input.md.
func TestConflictFFSiblingFirst(t *testing.T) {
	t.Parallel()
	c := conflictFFReady(t, true)
	f, v := c.f, c.v
	sibling := c.siblingOpen(3)
	writes := len(f.Writes())
	v.pass()
	if first := storyMerges(f, writes, sibling); len(first) != 1 {
		t.Fatalf("the pass merged %+v; want the sibling's PR, #%d, once", first, sibling)
	}
	squash := v.originTip(tipBranch)
	if got := gitIn(t, v.o.url, "rev-list", "--parents", "-n", "1", squash); got != squash+" "+c.tip {
		t.Fatalf("the story branch's tip and its parents are %q; want the sibling's squash commit on %s", got, c.tip)
	}
	if pickStage(f, c.bug) != "stage:pr-open" || len(c.moves()) != 0 {
		t.Fatalf("the sibling's merge's pass left the bug at %s, with the moves back %q; want pr-open, and none", pickStage(f, c.bug), c.moves())
	}
	move := c.sentBack(squash)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(squash, tipBranch, "success")) || !f.State().Issues[2].Closed {
		t.Errorf("the send-back's pass left the checks %+v, and the sibling closed %t; want the success carried over to %s, and the sibling closed on its merge",
			got, f.State().Issues[2].Closed, squash)
	}
	v.out(3, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(3, map[string]string{})
	v.pass()
	if v.runs() != 3 {
		t.Fatalf("claude ran %d times after the send-back; want three times", v.runs())
	}
	v.storyTipTarget(3, c.bug, c.head, squash, move)
}

// A person's push to the story branch between the pass's sync and the
// fast-forward makes the lease stale (SPEC §7.5's A conflict): nothing is
// pushed, the branch keeps the person's tip, and that's logged once. The
// next pass's sync shows the person's tip, which step 4 checks: it passes
// rule 2, but fails rule 7, as it lacks main, which conflicts with it, so
// that's commented on the bug, and the pass after sends the bug back on
// it, as a failure's bug takes such a tip, with no checks run on its merge
// with it, keyed event=b<SHA> by it, while its merge waits, logged once.
// Its next run starts from its head with that tip named as its target. A
// sibling task of the story at pr-open, numbered after the bug and verified
// against the same tip, isn't merged in the stale push's pass, as nothing
// names that tip as the story's tip for the rest of it. It reads logr, so
// it isn't parallel.
func TestConflictFFPersonPush(t *testing.T) {
	c := conflictFFReady(t, false)
	f, v := c.f, c.v
	sibling := c.siblingOpen(conflictFFSibling(f))
	var person string
	f.BeforeRequest(storyPRMatch(c.pr), func() {
		person = v.tipPush(tipBranch, map[string]string{"person.txt": "a person's change\n"})
	})
	stop := logtest.Capture(t, logr.I)
	writes := len(f.Writes())
	v.pass()
	if got := storyMerges(f, writes, sibling); len(got) != 0 {
		t.Errorf("the stale push's pass merged the sibling's PR %+v; want no merge onto a tip the branch is past", got)
	}
	v.pass()
	if person == "" {
		t.Fatal("the PR wasn't read again before the fast-forward, so the person didn't push")
	}
	c.pushedNone("the stale push's pass", person)
	// The person's tip lacks main, which conflicts with it, so it fails
	// rule 7 alone, and passes rule 2, so the bug takes it, and its
	// failure goes to the bug as a comment (SPEC §7.5).
	if !slices.Contains(f.State().CheckRuns, tipCheck(person, tipBranch, "failure")) || len(c.moves()) != 0 || pickStage(f, c.bug) != "stage:pr-open" {
		t.Fatalf("the pass after the stale push left the checks %+v, and the bug at %s, with the moves back %q; want failure on the person's %s, and pr-open, with none",
			f.State().CheckRuns, pickStage(f, c.bug), c.moves(), person)
	}
	if got := storyFailureComments(f, c.bug, "<!-- issuedog:failure tip="+person+" -->"); len(got) != 1 {
		t.Fatalf("the bug's comments on the person's tip = %q; want one", got)
	}
	move := c.sentBack(person)
	logs := stop()
	n, pr := strconv.Itoa(c.bug), strconv.Itoa(c.pr)
	for _, want := range []string{
		"o/r#" + n + ": its PR #" + pr + "'s head " + c.head + " isn't pushed to its story branch " + tipBranch +
			", as the branch moved from " + c.tip + " since the sync, so it waits for the new tip",
		"o/r#" + n + ": its PR #" + pr + " isn't fast-forwarded: its story branch " + tipBranch + " is at " + person + ", not at " + c.tip +
			", the tip its head " + c.head + " was verified against",
	} {
		if got := strings.Count(logs, want); got != 1 {
			t.Errorf("%q was logged %d times; want once, in\n%s", want, got, logs)
		}
	}
	v.out(3, ivResult(t, implementerOutput(t, "commits")))
	v.conflictRunMerge(3, map[string]string{})
	v.pass()
	if v.runs() != 3 {
		t.Fatalf("claude ran %d times after the send-back; want three times", v.runs())
	}
	v.storyTipTarget(3, c.bug, c.head, person, move)
}

// A tip that moved since the verify and fails rule 2 holds the bug at
// pr-open, as a failure's bug takes only a tip that passes it (SPEC §7.5's
// A checked base): it files its human task, and once that's read, the
// passes make no request but 304s, push nothing and send nothing back.
// Once a later tip passes rule 2, the bug goes back on it, keyed by it,
// though that tip fails rule 7, as it lacks main, which conflicts with
// it, as a failure's bug takes such a tip (SPEC §7.5's When the branch
// moves under a failure's bug).
func TestConflictFFWaits(t *testing.T) {
	t.Parallel()
	c := conflictFFReady(t, false)
	f, v := c.f, c.v
	f.BeforeRequest(storyPRMatch(c.pr), func() {
		v.tipPush(tipBranch, map[string]string{"secrets/key.txt": "a key\n"})
	})
	v.pass()
	bad := v.originTip(tipBranch)
	if bad == c.head || bad == c.tip {
		t.Fatalf("the story branch is at %s; want a person's push past %s", bad, c.tip)
	}
	v.passes(4)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(bad, tipBranch, "failure")) {
		t.Fatalf("the checks are %+v; want failure on %s, which fails rule 2", got, bad)
	}
	if issues := f.State().Issues; !strings.HasSuffix(issues[len(issues)-1].Body, tipFailureOrigin(bad, true)) {
		t.Fatalf("the newest issue =\n%+v\nwant the human task filed for %s", issues[len(issues)-1], bad)
	}
	v.quietPass("a conflict bug whose push waits for a tip that passes rule 2")
	v.quietPass("again")
	c.pushedNone("while the tip fails rule 2", bad)
	if len(c.moves()) != 0 || pickStage(f, c.bug) != "stage:pr-open" {
		t.Fatalf("while the tip fails rule 2, the bug is at %s, with the moves back %q; want pr-open, and none", pickStage(f, c.bug), c.moves())
	}
	fixed := v.tipPush(tipBranch, map[string]string{"secrets/key.txt": ""})
	v.pass()
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(fixed, tipBranch, "failure")) || len(c.moves()) != 0 {
		t.Fatalf("the checks are %+v, and the bug's moves back %q; want failure on %s, on rule 7 alone, and none yet", got, c.moves(), fixed)
	}
	c.sentBack(fixed)
}

// A crash just after the fast-forward's push, before step 4 posts the new
// tip's check, then a restart (SPEC §5.2): the fresh daemon's first pass
// finds the PR merged by the App, deletes the bug's branch and closes the
// bug, and checks the head as the branch's tip as any push, once, with
// nothing pushed again. The passes after are quiet.
func TestConflictFFCrash(t *testing.T) {
	t.Parallel()
	c := conflictFFReady(t, false)
	f, v := c.f, c.v
	f.FailWrite(len(f.Writes()) + 1)
	if err := v.d.Iterate(t.Context()); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Iterate = %v; want step 4's check of the new tip refused", err)
	}
	if got := v.originTip(tipBranch); got != c.head || slices.ContainsFunc(f.State().CheckRuns, func(r githubtest.CheckRunState) bool {
		return r.SHA == c.head && r.Name == github.TipCheck
	}) {
		t.Fatalf("the crashed pass left the story branch at %s, with the checks %+v; want the head %s, with no %s on it", got, f.State().CheckRuns, c.head, github.TipCheck)
	}
	v = v.restart()
	ran := v.checksRan()
	v.pass()
	if !f.State().Issues[c.bug-1].Closed || c.branch() != "" {
		t.Errorf("the restart's pass left the bug closed %t, its branch %q; want it closed, and its branch deleted", f.State().Issues[c.bug-1].Closed, c.branch())
	}
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(c.head, tipBranch, "success")) || v.checksRan() != ran+1 {
		t.Errorf("the restart's pass left the checks %+v, and ran the commands %d times; want success on the head %s, once", got, v.checksRan()-ran, c.head)
	}
	v.passes(2)
	v.quietPass("after the restart")
	c.pushedNone("after the restart", c.head)
}

// conflictFFSeed is what a conflict bug set up by hand starts from, in a
// shared origin, base's: story 1's branch cut from main and moved to tip,
// which changes f.txt, main moved past it to main, with its own change to
// f.txt, and the bug's head, the merge of the two resolving f.txt, which
// a fake puts on the bug's branch, branch.
type conflictFFSeed struct {
	base                    *verdictRig
	branch, tip, main, head string
}

// newConflictFFSeed builds a conflictFFSeed in a new rig's origin.
func newConflictFFSeed(t *testing.T) *conflictFFSeed {
	t.Helper()
	v := newVerdictRig(t, newFake(t), 3)
	s := &conflictFFSeed{base: v, branch: "issuedog/2-merge-main"}
	v.tipCut(tipBranch)
	s.tip = v.tipPush(tipBranch, map[string]string{"f.txt": "the story\n"})
	s.main = v.storyMainMoved("f.txt", "main's\n")
	w := v.o.work
	gitIn(t, w, "checkout", "--quiet", "-B", "bugfix", s.main)
	gitIn(t, w, "fetch", "--quiet", v.o.url, "refs/heads/"+tipBranch)
	// The merge keeps main's f.txt, then the amend resolves it, as the
	// bug's implementer would resolve the conflict.
	gitIn(t, w, "merge", "--quiet", "--no-edit", "-s", "ours", "FETCH_HEAD")
	s.head = s.commit(t, "f.txt", "both\n", true)
	return s
}

// commit writes path with content in s's origin's work tree, on its
// bugfix branch, as a new commit, or with amend into its last one, sends
// it to the origin, goes back to main, and returns the commit.
func (s *conflictFFSeed) commit(t *testing.T, path, content string, amend bool) string {
	t.Helper()
	w := s.base.o.work
	gitIn(t, w, "checkout", "--quiet", "bugfix")
	if err := os.WriteFile(filepath.Join(w, path), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, w, "add", path)
	if amend {
		gitIn(t, w, "commit", "--quiet", "--amend", "--no-edit")
	} else {
		gitIn(t, w, "commit", "--quiet", "-m", "a commit of the bug's")
	}
	sha := gitIn(t, w, "rev-parse", "HEAD")
	// The origin keeps it outside refs/heads, so a fake can put a branch
	// there.
	gitIn(t, w, "push", "--quiet", s.base.o.url, sha+":refs/seed/"+sha)
	gitIn(t, w, "checkout", "--quiet", "main")
	return sha
}

// fake puts s's origin's story branch at at and the bug's branch at bugAt,
// then builds a fake on it: story #1, and bug #2, its conflict bug for s's
// tip and main commit, below it, at pr-open with its PR from its branch
// into the story branch, its moves naming s's head, verified against s's
// tip, whose issuedog/tip is success, as is bugAt's issuedog/verify.
// more, unless nil, then adds to it, given the PR's number. Its writes so
// far are forgotten.
func (s *conflictFFSeed) fake(tb testing.TB, at, bugAt string, more func(f *githubtest.Fake, pr int)) *githubtest.Fake {
	tb.Helper()
	gitIn(tb, s.base.o.url, "update-ref", "refs/heads/"+s.branch, bugAt)
	gitIn(tb, s.base.o.url, "update-ref", "refs/heads/"+tipBranch, at)
	f := newFake(tb)
	f.SetPermission(wholeRunOwner, "admin")
	f.SetGitRemote(s.base.o.url)
	tipStory(tb, f)
	bug := f.AddIssue(app, "Merge main at "+s.main[:7]+" into story #1's branch", "Resolve the conflicts.\n\n"+storyMainConflictOrigin(s.tip, s.main),
		"type:bug", "priority:p1")
	if bug != 2 {
		tb.Fatalf("the bug is #%d; want #2", bug)
	}
	f.SetParent(2, 1)
	c := f.Client(tb)
	pr, err := c.CreatePullRequest(context.Background(), "o", "r", github.NewPullRequest{Title: "fix: Merge main into the story branch", Head: s.branch,
		Base: tipBranch, Body: "Task #2, in story #1\n\nThe resolution."})
	if err != nil {
		tb.Fatal(err)
	}
	f.AddLabel(pr.Number, app, "priority:p1")
	f.AddLabel(2, app, "stage:pr-open")
	f.AddComment(2, app, "Filed.\n\n<!-- issuedog:transition from=intake to=ready event=t"+s.tip+" -->")
	f.AddComment(2, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head="+s.head+" -->")
	f.AddComment(2, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv head="+s.head+" pr="+strconv.Itoa(pr.Number)+" tip="+s.tip+" -->")
	for _, run := range []struct{ name, sha, id string }{
		{github.TipCheck, s.tip, "branch=" + tipBranch},
		{github.VerifyCheck, bugAt, "tip=" + s.tip},
	} {
		if _, err := c.PostCheck(context.Background(), "o", "r", run.name, github.NewCheckRun{HeadSHA: run.sha, Conclusion: "success",
			Title: "t", Summary: "s", ExternalID: run.id}); err != nil {
			tb.Fatal(err)
		}
	}
	if more != nil {
		more(f, pr.Number)
	}
	f.ForgetWrites()
	return f
}

// rig is a fresh daemon on f, in root, as a restart leaves one, on clone
// as its bare clone, or a new one for nil, that picks nothing.
func (s *conflictFFSeed) rig(t *testing.T, f *githubtest.Fake, root string, clone *repo.Repo) *verdictRig {
	t.Helper()
	g := newRig(t, f, s.base.o, root)
	if clone != nil {
		g.d.Clone, g.r.Workspace.Bare = clone, clone.Dir()
	}
	v := &verdictRig{rig: g, o: s.base.o, cfg: s.base.cfg, ran: s.base.ran, noPick: true}
	v.wire()
	return v
}

// replay checks with githubtest.Replay that a conflict bug's pass replays
// safely after a crash at any write (SPEC §5.2), on s's fake with the
// story branch at at and the bug's branch at s's head. Each pass is a
// fresh daemon on the fake's own bare clone (rig), and runs five
// iterations. The origin is shared, so each setup puts the bug's branch
// and the story branch back. It returns each run's fake.
func (s *conflictFFSeed) replay(t *testing.T, at string) []*githubtest.Fake {
	t.Helper()
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	var fakes []*githubtest.Fake
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := s.fake(tb, at, s.head, nil)
		roots[f] = t.TempDir()
		fakes = append(fakes, f)
		return f
	}, func(f *githubtest.Fake) error {
		v := s.rig(t, f, roots[f], clones[f])
		clones[f] = v.d.Clone
		for range 5 {
			if err := v.d.Iterate(context.Background()); err != nil {
				return err
			}
		}
		return nil
	})
	return fakes
}

// The fast-forward, then the close, replay safely (SPEC §5.2): every run
// ends with the story branch at the bug's head, pushed once, the PR merged
// by the App, the bug's branch deleted, the bug closed as completed, and
// the head checked as the branch's tip, never carried over.
func TestConflictFFReplay(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	for _, f := range s.replay(t, s.tip) {
		st := f.State()
		if len(st.PRs) != 1 || !st.PRs[0].Merged || st.PRs[0].MergedBy != app || !st.Issues[1].Closed {
			t.Fatalf("after the replay, the PRs = %+v, the bug closed %t; want one, merged by %s, and the bug closed", st.PRs, st.Issues[1].Closed, app)
		}
		if !slices.Contains(st.CheckRuns, tipCheck(s.head, tipBranch, "success")) || !slices.Equal(st.Branches, []githubtest.BranchState{{Name: tipBranch, SHA: s.head}}) {
			t.Errorf("after the replay, the checks = %+v, the branches %+v; want success on the head %s, the story branch's tip, and the bug's branch gone",
				st.CheckRuns, st.Branches, s.head)
		}
	}
}

// The send-back on a tip that moved since the verify replays safely (SPEC
// §5.2): every run ends with the story branch at the person's tip,
// checked as failure, as it lacks main, which conflicts with it, though
// it passes rule 2, nothing pushed, the PR open, and the bug at
// in-progress, moved once, keyed by that tip.
func TestConflictFFMovedReplay(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	person := s.base.tipPush(tipBranch, map[string]string{"person.txt": "a person's change\n"})
	for _, f := range s.replay(t, person) {
		st := f.State()
		var moves []string
		for _, cm := range st.Issues[1].Comments {
			if strings.Contains(cm.Body, "<!-- issuedog:transition from=pr-open to=in-progress event=b"+person+" ") {
				moves = append(moves, cm.Body)
			}
		}
		if len(moves) != 1 || pickStage(f, 2) != "stage:in-progress" || st.PRs[0].Merged || st.PRs[0].State != "open" {
			t.Fatalf("after the replay, the bug is at %s, with the moves %q, and the PR %+v; want in-progress, after one, and the PR open",
				pickStage(f, 2), moves, st.PRs[0])
		}
		if !slices.Contains(st.CheckRuns, tipCheck(person, tipBranch, "failure")) ||
			!slices.Contains(st.Branches, githubtest.BranchState{Name: tipBranch, SHA: person}) {
			t.Errorf("after the replay, the checks = %+v, the branches %+v; want failure on the person's %s, and the story branch at it",
				st.CheckRuns, st.Branches, person)
		}
	}
}

// still fails, naming step, unless bug #2 on f is at pr-open,
// with no move back, its PR open, and the story branch at at in s's
// origin.
func (s *conflictFFSeed) still(t *testing.T, f *githubtest.Fake, step, at string) {
	t.Helper()
	pr := f.State().PRs[0]
	if got := s.base.originTip(tipBranch); got != at || pickStage(f, 2) != "stage:pr-open" || len(storyTipMoves(f, 2)) != 0 || pr.State != "open" {
		t.Errorf("%s: the story branch is at %s, the bug at %s, with the moves back %q, and its PR %+v; want %s, pr-open, none, and the PR open",
			step, got, pickStage(f, 2), storyTipMoves(f, 2), pr, at)
	}
}

// A conflict bug's head that lacks the tip its move to pr-open names, as
// no head rule 7 passed can, is never pushed to the story branch: the
// lease alone would let that push over the tip, so the App checks that
// it's a fast-forward first, and warns once (SPEC §7.5's A conflict).
// The story branch keeps its tip, and the bug waits at pr-open.
func TestConflictFFNotForward(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	gitIn(t, s.base.o.work, "branch", "--quiet", "-f", "bugfix", s.main)
	lacks := s.commit(t, "f.txt", "both, by hand\n", false)
	f := s.fake(t, s.tip, lacks, func(f *githubtest.Fake, pr int) {
		f.AddComment(2, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head="+lacks+" -->")
		f.AddComment(2, app, "PR open.\n\n<!-- issuedog:transition from=verifying to=pr-open run=rv2 head="+lacks+" pr="+strconv.Itoa(pr)+" tip="+s.tip+" -->")
	})
	v := s.rig(t, f, t.TempDir(), nil)
	v.passes(4)
	s.still(t, f, "a head that lacks the tip", s.tip)
}

// A conflict bug whose head is its story branch's tip already, as its
// fast-forward left it, while GitHub hasn't marked its PR merged, isn't
// sent back on that tip, though it isn't the tip its head was verified
// against, as the head holds it, and nothing is pushed: the bug waits at
// pr-open for its PR to read merged, and once the head's check as the
// tip is posted and read, the passes make no request but 304s (SPEC
// §7.5's A conflict, §16). The fake marks no PR merged whose head was in
// its base when it was opened, as here.
func TestConflictFFNotMarked(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	f := s.fake(t, s.head, s.head, nil)
	v := s.rig(t, f, t.TempDir(), nil)
	v.passes(3)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(s.head, tipBranch, "success")) {
		t.Fatalf("the checks are %+v; want success on the head %s as the story branch's tip", got, s.head)
	}
	v.quietPass("a conflict bug whose PR isn't marked merged yet")
	s.still(t, f, "a head that's the tip already", s.head)
}

// A head a person pushed to a conflict bug's branch that failed verify,
// whose move back to pr-open names no tip it was verified against, isn't
// sent back when the story branch moves, and isn't pushed: its failure
// stands until its next push or the owner's review moves it on (SPEC
// §7.2). Once the moved tip's check is read, the passes make no request
// but 304s.
func TestConflictFFPersonHead(t *testing.T) {
	t.Parallel()
	s := newConflictFFSeed(t)
	gitIn(t, s.base.o.work, "branch", "--quiet", "-f", "bugfix", s.head)
	pushed := s.commit(t, "person.txt", "a person's head\n", false)
	person := s.base.tipPush(tipBranch, map[string]string{"story-person.txt": "a person's change\n"})
	f := s.fake(t, person, pushed, func(f *githubtest.Fake, pr int) {
		f.AddComment(2, app, "A new head.\n\n<!-- issuedog:transition from=pr-open to=verifying event=n"+pushed+" head="+pushed+" -->")
		f.AddComment(2, app, "It fails verify.\n\n<!-- issuedog:transition from=verifying to=pr-open event=v1 head="+pushed+" pr="+strconv.Itoa(pr)+" -->")
		if _, err := f.Client(t).PostCheck(context.Background(), "o", "r", github.VerifyCheck, github.NewCheckRun{HeadSHA: pushed, Conclusion: "failure",
			Title: "t", Summary: "s", ExternalID: "tip=" + s.tip}); err != nil {
			t.Fatal(err)
		}
	})
	v := s.rig(t, f, t.TempDir(), nil)
	v.passes(4)
	if got := f.State().CheckRuns; !slices.Contains(got, tipCheck(person, tipBranch, "failure")) {
		t.Fatalf("the checks are %+v; want failure on the person's tip %s, which passes rule 2", got, person)
	}
	v.quietPass("a person's failed head on a conflict bug")
	s.still(t, f, "a person's failed head", person)
}
