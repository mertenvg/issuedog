package daemon_test

import (
	"context"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A task's PR closed without merging, or reopened by someone untrusted,
// at pr-open hands the task off, and a trusted reopen on that hand-off
// sends it back (SPEC §6.4, §7.2, §8.1 step 2), against the fakes, on
// prItemFake's task #1 at pr-open with its PR #2, and the digest #3.
// mertenvg is notify and the code owner, and mallory isn't trusted.

// prReconcileRig is prItemFake's task with a daemon of prItemDaemon's
// that has run two passes, so a pass that changes nothing makes no
// request about the PR.
func prReconcileRig(t *testing.T) (*origin, *githubtest.Fake, *daemon.Daemon) {
	t.Helper()
	o := prItemOrigin(t)
	f := prItemFake(t, o)
	d := prItemDaemon(t, f, o)
	iterate(t, d)
	iterate(t, d)
	return o, f, d
}

// prReconcileClosedKey is the key of the hand-off for PR #2's close,
// c2-<closed_at in Unix seconds>, as the client lists it (SPEC §5).
func prReconcileClosedKey(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	prs, err := f.Client(t).PullRequestsByHead(context.Background(), "o", "r", prBranch)
	if err != nil || len(prs) != 1 || prs[0].ClosedAt.IsZero() {
		t.Fatalf("PR #2 listed as %+v, %v; want it, closed", prs, err)
	}
	return "c2-" + strconv.FormatInt(prs[0].ClosedAt.Unix(), 10)
}

// prReconcileNewest is task #1's newest comment, as a snapshot reads it.
func prReconcileNewest(t *testing.T, f *githubtest.Fake) github.Comment {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	return s.Comments[len(s.Comments)-1]
}

// prReconcileHandedOff fails unless the pass just run handed task #1 off
// (prReconcileHandoff), and notify is assigned to the task, and nobody to
// the PR. It returns the hand-off comment's ID.
func prReconcileHandedOff(t *testing.T, f *githubtest.Fake, key, intro, why string) int64 {
	t.Helper()
	id := prReconcileHandoff(t, f, key, intro, why)
	if pr, task := assignees(f, 2), assignees(f, 1); len(pr) != 0 || !slices.Equal(task, []string{digestRunOwner}) {
		t.Errorf("the PR's assignees = %q, the task's %q; want nobody on the PR, and %s on the task", pr, task, digestRunOwner)
	}
	return id
}

// prReconcileHandoff fails unless the pass just run handed task #1 off:
// it's at needs-human, its newest comment holds the move pr-open →
// needs-human keyed event=key and nothing else, and starts with intro,
// links PR #2 and ends in the cc line (SPEC §9.3), and the digest lists
// the hand-off's item with why. It returns the hand-off comment's ID.
func prReconcileHandoff(t *testing.T, f *githubtest.Fake, key, intro, why string) int64 {
	t.Helper()
	if got := pickStage(f, 1); got != "stage:needs-human" {
		t.Fatalf("the task is at %s; want needs-human", got)
	}
	c := prReconcileNewest(t, f)
	marker := "\n\ncc @" + digestRunOwner + "\n\n<!-- issuedog:transition from=pr-open to=needs-human event=" + key + " -->"
	if c.Author.Login != app || !strings.HasPrefix(c.Body, intro) || !strings.HasSuffix(c.Body, marker) ||
		!strings.Contains(c.Body, "**Links:**\n- https://github.com/o/r/pull/2\n") {
		t.Errorf("the hand-off =\n%s\nwant the App's, starting\n%s\nlinking PR #2, and ending\n%s", c.Body, intro, marker)
	}
	digest := f.State().Issues[2].Body
	if !strings.Contains(digest, "**Hand-off**") || !strings.Contains(digest, "- A hand-off waits for you: "+why+"\n") || strings.Contains(digest, "Pull request") {
		t.Errorf("the digest =\n%s\nwant the hand-off's item, saying %q, and no pull request", digest, why)
	}
	return c.ID
}

// prReconcileClosedIntro and prReconcileReopenedIntro open the hand-offs.
const (
	prReconcileClosedIntro = "PR #2 for this task was closed without merging, so the task waits on you.\n\n"
	prReconcileClosedWhy   = "the task's PR was closed without merging."
	prReconcileReopenedWhy = "the task's PR was reopened after a close by someone who isn't trusted."
)

// prReconcileReopenedIntro opens the hand-off for a reopen by by.
func prReconcileReopenedIntro(by string) string {
	return "PR #2 for this task was reopened after a close by `" + by + "`, who isn't trusted, and only a trusted user's reopen keeps it, so the task waits on you.\n\n"
}

// A PR closed without merging hands its task off on the pass whose poll
// lists it closed, by the owner or by someone untrusted alike, and with
// its closed event not written yet: the listing's state and closed_at say
// enough, so the pass reads no timeline. The hand-off is keyed by the
// PR's closed_at, not its updated_at, which mallory's label after the
// close moves, links the PR and ends in the cc line, the digest lists
// it, and on the same pass notify is unassigned from the PR and assigned
// to the task. The pass after makes no request about the PR.
func TestPRReconcileClosed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		by   string
		lag  bool
	}{
		{"by the owner", digestRunOwner, false},
		{"by someone untrusted", "mallory", false},
		{"with its event late", digestRunOwner, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, f, d := prReconcileRig(t)
			if tt.lag {
				f.LagEvents(time.Hour)
			}
			f.ClosePR(2, tt.by)
			f.LagEvents(0)
			f.Advance(time.Minute)
			f.AddLabel(2, "mallory", "bug")
			key := prReconcileClosedKey(t, f)
			from, reqs := len(f.Writes()), len(f.Requests())
			iterate(t, d)
			prReconcileHandedOff(t, f, key, prReconcileClosedIntro, prReconcileClosedWhy)
			// A PR to main, back at pr-open, waits for the owner's review
			// and merge, not the App's merge into a story branch.
			if step := "1. To keep the PR, reopen it: the task goes back to `pr-open`, waiting for your review and merge.\n"; !strings.Contains(prReconcileNewest(t, f).Body, step) {
				t.Errorf("the hand-off =\n%s\nwant it to hold\n%s", prReconcileNewest(t, f).Body, step)
			}
			if got := prTimelineReads(f, reqs); got != 0 {
				t.Errorf("the pass read the PR's timeline %d times; want none: a closed PR's state says enough", got)
			}
			if got, want := assignWrites(f, from), []string{assignAdd(1, digestRunOwner), assignRemove(2, digestRunOwner)}; !slices.Equal(got, want) {
				t.Errorf("the pass wrote assignees %q; want %q", got, want)
			}
			iterate(t, d)
			prItemQuiet(t, f, d, "at the hand-off")
			if moves := prReconcileMoves(f); len(moves) != 1 {
				t.Errorf("the task's moves out of pr-open = %q; want the one hand-off", moves)
			}
		})
	}
}

// prReconcileMoves are the App's comments on task #1 that hold step 2's
// moves: pr-open → needs-human, or back, not set by hand.
func prReconcileMoves(f *githubtest.Fake) []string {
	var moves []string
	for _, c := range f.State().Issues[0].Comments {
		for _, m := range github.ParseMarkers(c.Body) {
			from, to := m.Fields["from"], m.Fields["to"]
			if c.Author == app && m.Kind == "transition" && m.Fields["by"] == "" &&
				(from == "pr-open" && to == "needs-human" || from == "needs-human" && to == "pr-open") {
				moves = append(moves, c.Body)
			}
		}
	}
	return moves
}

// A close and a reopen in one pass: the poll lists the PR open, and
// changed, so the pass reads its timeline, once, and its newest close or
// reopen decides (SPEC §6.4). A reopen by someone untrusted, or by someone
// GitHub doesn't name, hands the task off, keyed by the reopened event,
// naming the reopener, in code, or saying GitHub names nobody, on the same
// pass as the digest and the assignees show it. The owner's reopen, or the
// App's, moves nothing, and the item stays. Whether the owner is trusted
// is asked once for the event: a pass that reads the PR again, as after
// mallory labels it, asks nothing again.
func TestPRReconcileReopenedInOnePass(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, by, intro string
	}{
		{"by someone untrusted", "mallory", prReconcileReopenedIntro("mallory")},
		{"by nobody GitHub names", "", "PR #2 for this task was reopened after a close by someone GitHub doesn't name, " +
			"and only a trusted user's reopen keeps it, so the task waits on you.\n\n"},
		{"by the owner", digestRunOwner, ""},
		{"by the App", app, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, f, d := prReconcileRig(t)
			f.ClosePR(2, digestRunOwner)
			id := f.ReopenPR(2, tt.by)
			reqs := len(f.Requests())
			iterate(t, d)
			if got := prTimelineReads(f, reqs); got != 1 {
				t.Errorf("the pass read the PR's timeline %d times; want once", got)
			}
			if tt.intro == "" {
				if got := pickStage(f, 1); got != "stage:pr-open" || len(prReconcileMoves(f)) != 0 || !strings.Contains(f.State().Issues[2].Body, prItemBody) {
					t.Errorf("after the owner's reopen, the task is at %s, its moves %q, the digest\n%s\nwant it at pr-open, no move, and the item",
						got, prReconcileMoves(f), f.State().Issues[2].Body)
				}
				iterate(t, d)
				prItemQuiet(t, f, d, "after the reopen")
				f.AddLabel(2, "mallory", "bug")
				reqs := len(f.Requests())
				iterate(t, d)
				asks := slices.ContainsFunc(f.Requests()[reqs:], func(r githubtest.Request) bool { return strings.HasPrefix(r.Path, "/collaborators/") })
				if got := prTimelineReads(f, reqs); got != 1 || asks || len(prReconcileMoves(f)) != 0 {
					t.Errorf("the pass after mallory's label: %d reads of the PR's timeline, a permission asked %t, moves %q; want one read, none asked, and no move",
						got, asks, prReconcileMoves(f))
				}
				return
			}
			prReconcileHandedOff(t, f, strconv.FormatInt(id, 10), tt.intro, prReconcileReopenedWhy)
			iterate(t, d)
			prItemQuiet(t, f, d, "at the hand-off")
		})
	}
}

// A PR listed open whose newest close or reopen is a close was reopened,
// but GitHub hasn't written its reopened event yet: that judges nothing,
// and is no error, and the PR is read again on each pass till the event
// is there, though the poll doesn't list it changed again. Then the
// untrusted reopen hands the task off (SPEC §6.4).
func TestPRReconcileReopenEventLate(t *testing.T) {
	t.Parallel()
	_, f, d := prReconcileRig(t)
	f.LagEvents(30 * time.Second)
	f.ClosePR(2, digestRunOwner)
	f.Advance(time.Minute) // the closed event is written
	id := f.ReopenPR(2, "mallory")
	f.LagEvents(0) // the App's writes' events aren't late
	for _, step := range []string{"the pass that lists the reopen", "the pass after"} {
		from, reqs := len(f.Writes()), len(f.Requests())
		iterate(t, d)
		if got := prTimelineReads(f, reqs); got != 1 || pickStage(f, 1) != "stage:pr-open" || len(prReconcileMoves(f)) != 0 {
			t.Errorf("%s: %d reads of the PR's timeline, the task at %s, its moves %q; want one read, and the task at pr-open, unmoved",
				step, got, pickStage(f, 1), prReconcileMoves(f))
		}
		if got := writesOf(f)[from:]; slices.ContainsFunc(got, func(w string) bool { return strings.HasPrefix(w, "POST /issues/1/") }) {
			t.Errorf("%s: wrote %q; want nothing on the task", step, got)
		}
	}
	f.Advance(time.Minute) // the reopened event is written
	iterate(t, d)
	prReconcileHandedOff(t, f, strconv.FormatInt(id, 10), prReconcileReopenedIntro("mallory"), prReconcileReopenedWhy)
	iterate(t, d)
	prItemQuiet(t, f, d, "at the hand-off")
}

// A task held at pr-open isn't judged: its PR's close, or a close and an
// untrusted reopen, made while it's held hands nothing off, and costs no
// timeline read. /resume brings it back to pr-open, and that pass judges
// its PR, from the window it had before the hold, and hands it off.
// A return with no change to the PR reads its timeline once all the same,
// with assignment off, so no poll lists the PR changed for the App's
// writes, and moves nothing (SPEC §6.4, §8.1).
func TestPRReconcileHeld(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		// act changes the PR while the task is held, and returns the key,
		// the opening and the digest's why of the hand-off it makes, or
		// none.
		act func(t *testing.T, f *githubtest.Fake) (key, intro, why string)
		// reads is how often the /resume's pass reads the PR's timeline:
		// only an open PR is read.
		reads int
	}{
		{"a close", func(t *testing.T, f *githubtest.Fake) (string, string, string) {
			f.ClosePR(2, digestRunOwner)
			return prReconcileClosedKey(t, f), prReconcileClosedIntro, prReconcileClosedWhy
		}, 0},
		{"a close and an untrusted reopen", func(_ *testing.T, f *githubtest.Fake) (string, string, string) {
			f.ClosePR(2, digestRunOwner)
			return strconv.FormatInt(f.ReopenPR(2, "mallory"), 10), prReconcileReopenedIntro("mallory"), prReconcileReopenedWhy
		}, 1},
		{"no change", func(*testing.T, *githubtest.Fake) (string, string, string) { return "", "", "" }, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, f, d := prReconcileRig(t)
			d.Assign = tt.name != "no change"
			f.AddComment(1, digestRunOwner, "/hold")
			iterate(t, d)
			if got := pickStage(f, 1); got != "stage:held" {
				t.Fatalf("after /hold, the task is at %s; want held", got)
			}
			key, intro, why := tt.act(t, f)
			reqs := len(f.Requests())
			iterate(t, d)
			iterate(t, d)
			if got := prTimelineReads(f, reqs); got != 0 || pickStage(f, 1) != "stage:held" || len(prReconcileMoves(f)) != 0 {
				t.Errorf("while held: %d reads of the PR's timeline, the task at %s, its moves %q; want no read, and the task held, unmoved",
					got, pickStage(f, 1), prReconcileMoves(f))
			}
			f.AddComment(1, digestRunOwner, "/resume")
			reqs = len(f.Requests())
			iterate(t, d)
			if reads := prTimelineReads(f, reqs); reads != tt.reads {
				t.Errorf("the /resume's pass read the PR's timeline %d times; want %d", reads, tt.reads)
			}
			if intro == "" {
				if got := pickStage(f, 1); got != "stage:pr-open" || len(prReconcileMoves(f)) != 0 {
					t.Errorf("after /resume, the task is at %s, its moves %q; want pr-open, and no move", got, prReconcileMoves(f))
				}
				iterate(t, d)
				prItemQuiet(t, f, d, "after /resume")
				return
			}
			prReconcileHandedOff(t, f, key, intro, why)
			iterate(t, d)
			prItemQuiet(t, f, d, "at the hand-off")
		})
	}
}

// At needs-human on the hand-off for its PR's close, a PR listed open
// whose newest close or reopen is the close was reopened, but GitHub
// hasn't written the owner's reopened event yet: that moves nothing, and
// the PR is read again next pass, which moves the task back once the
// event is there, keyed by it (SPEC §6.4, §7.2).
func TestPRReconcileReopenAtHandoffLate(t *testing.T) {
	t.Parallel()
	_, f, d := prReconcileRig(t)
	prReconcileHandOff(t, f, d)
	f.LagEvents(30 * time.Second)
	id := f.ReopenPR(2, digestRunOwner)
	f.LagEvents(0)
	reqs := len(f.Requests())
	iterate(t, d)
	if got := pickStage(f, 1); got != "stage:needs-human" || len(prReconcileMoves(f)) != 1 || prTimelineReads(f, reqs) != 1 {
		t.Errorf("with the reopened event late, the task is at %s, its moves %q, %d reads of the PR's timeline; want needs-human, the hand-off alone, and one read",
			got, prReconcileMoves(f), prTimelineReads(f, reqs))
	}
	f.Advance(time.Minute)
	iterate(t, d)
	if got, want := prReconcileNewest(t, f).Body, "<!-- issuedog:transition from=needs-human to=pr-open event="+strconv.FormatInt(id, 10)+" pr=2 -->"; pickStage(f, 1) != "stage:pr-open" || !strings.HasSuffix(got, want) {
		t.Errorf("once the event is there, the task is at %s, its newest comment\n%s\nwant pr-open, and the move ending %s", pickStage(f, 1), got, want)
	}
}

// A task that comes back to pr-open by a trusted stage:pr-open set by
// hand is judged on that pass, as one back from a hold is: here it was
// set to needs-human by hand, which has no hand-off, so the PR's close
// meanwhile hands nothing off, and once it's set back to pr-open, the
// PR's close hands it off on that pass (SPEC §6.4).
func TestPRReconcileStageSetByHand(t *testing.T) {
	t.Parallel()
	_, f, d := prReconcileRig(t)
	f.RemoveLabel(1, digestRunOwner, "stage:pr-open")
	f.AddLabel(1, digestRunOwner, "stage:needs-human")
	iterate(t, d)
	f.ClosePR(2, digestRunOwner)
	key := prReconcileClosedKey(t, f)
	iterate(t, d)
	iterate(t, d)
	if got := pickStage(f, 1); got != "stage:needs-human" || len(prReconcileMoves(f)) != 0 {
		t.Fatalf("set to needs-human by hand, with its PR closed, the task is at %s, its moves %q; want needs-human, and no hand-off", got, prReconcileMoves(f))
	}
	f.RemoveLabel(1, digestRunOwner, "stage:needs-human")
	f.AddLabel(1, digestRunOwner, "stage:pr-open")
	iterate(t, d)
	prReconcileHandedOff(t, f, key, prReconcileClosedIntro, prReconcileClosedWhy)
	iterate(t, d)
	prItemQuiet(t, f, d, "at the hand-off")

	// Set back to pr-open by hand once more, with the PR still closed: its
	// close was handed off already, so it moves the task no more, and the
	// task isn't read again on each pass for it.
	f.RemoveLabel(1, digestRunOwner, "stage:needs-human")
	f.AddLabel(1, digestRunOwner, "stage:pr-open")
	iterate(t, d)
	iterate(t, d)
	reqs := len(f.Requests())
	iterate(t, d)
	iterate(t, d)
	reads := slices.ContainsFunc(f.Requests()[reqs:], func(r githubtest.Request) bool { return r.Path == "/issues/1" })
	if got := pickStage(f, 1); got != "stage:pr-open" || len(prReconcileMoves(f)) != 1 || reads {
		t.Errorf("set back to pr-open by hand with the close handed off: the task at %s, its moves %q, read again %t; want pr-open, the one hand-off, and no read",
			got, prReconcileMoves(f), reads)
	}
}

// At needs-human on a hand-off that isn't for its PR, as the implementer's
// after a review sent the task back (SPEC §7.2), a trusted reopen of the
// task's PR moves nothing, and its PR isn't read for it.
func TestPRReconcileOtherHandoff(t *testing.T) {
	t.Parallel()
	_, f, d := prReconcileRig(t)
	f.ClosePR(2, digestRunOwner)
	f.RemoveLabel(1, app, "stage:pr-open")
	f.AddLabel(1, app, "stage:needs-human")
	f.AddComment(1, app, "Review changes.\n\n<!-- issuedog:transition from=pr-open to=in-progress event=r1 -->")
	f.AddComment(1, app, "The hand-off.\n\n<!-- issuedog:transition from=in-progress to=needs-human run=r2 -->")
	iterate(t, d)
	f.ReopenPR(2, digestRunOwner)
	reqs := len(f.Requests())
	iterate(t, d)
	iterate(t, d)
	if got := pickStage(f, 1); got != "stage:needs-human" || len(prReconcileMoves(f)) != 0 || prTimelineReads(f, reqs) != 0 {
		t.Errorf("the task is at %s, its moves %q, %d reads of the PR's timeline; want needs-human, no move, and no read",
			got, prReconcileMoves(f), prTimelineReads(f, reqs))
	}
}

// A close and an untrusted reopen before the task's move into pr-open, as
// at verifying, where a close hands nothing off (SPEC §7.5), are outside
// its PR's window (SPEC §6.4): the first pass at pr-open reads the PR,
// and moves nothing.
func TestPRReconcileBeforeTheWindow(t *testing.T) {
	t.Parallel()
	o := prItemOrigin(t)
	f := newFake(t)
	f.SetPermission(digestRunOwner, "admin")
	f.SetGitRemote(o.url)
	n := f.AddIssue(digestRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1")
	f.AddLabel(n, app, "stage:pr-open")
	pr, err := f.Client(t).CreatePullRequest(context.Background(), "o", "r",
		github.NewPullRequest{Title: prItemTitle, Head: prBranch, Base: "main", Body: "Closes #1\n\nWhat changed."})
	if err != nil {
		t.Fatal(err)
	}
	f.ClosePR(pr.Number, digestRunOwner)
	f.ReopenPR(pr.Number, "mallory")
	head := gitIn(t, o.url, "rev-parse", "refs/heads/"+prBranch)
	f.AddComment(n, app, prMoveText(pr.Number, head)+"<!-- issuedog:transition from=verifying to=pr-open run=rv head="+head+" pr=2 -->")
	d := prItemDaemon(t, f, o)
	reqs := len(f.Requests())
	iterate(t, d)
	if got := pickStage(f, 1); got != "stage:pr-open" || len(prReconcileMoves(f)) != 0 || prTimelineReads(f, reqs) != 1 {
		t.Errorf("the task is at %s, its moves %q, %d reads of the PR's timeline; want pr-open, no move, and one read",
			got, prReconcileMoves(f), prTimelineReads(f, reqs))
	}
}

// A failed read of the PR's timeline judges nothing: it's the pass's
// error, and the task stays at pr-open, and the next pass reads it again,
// though its poll doesn't list the PR changed, and hands the task off for
// mallory's reopen (SPEC §6.4).
func TestPRReconcileReadFails(t *testing.T) {
	t.Parallel()
	_, f, d := prReconcileRig(t)
	f.ClosePR(2, digestRunOwner)
	id := f.ReopenPR(2, "mallory")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/issues/2/timeline" }, http.StatusInternalServerError)
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "judge PR o/r#2 of #1") {
		t.Errorf("the pass whose read failed gave %v; want its error", err)
	}
	if got := pickStage(f, 1); got != "stage:pr-open" || len(prReconcileMoves(f)) != 0 {
		t.Errorf("after the failed read, the task is at %s, its moves %q; want pr-open, no move", got, prReconcileMoves(f))
	}
	iterate(t, d)
	prReconcileHandedOff(t, f, strconv.FormatInt(id, 10), prReconcileReopenedIntro("mallory"), prReconcileReopenedWhy)
}

// prReconcileHandOff runs the pass that hands task #1 off for its PR's
// close by the owner, and the one after, and returns the hand-off
// comment's ID.
func prReconcileHandOff(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) int64 {
	t.Helper()
	f.ClosePR(2, digestRunOwner)
	key := prReconcileClosedKey(t, f)
	iterate(t, d)
	id := prReconcileHandedOff(t, f, key, prReconcileClosedIntro, prReconcileClosedWhy)
	iterate(t, d)
	return id
}

// At needs-human on the hand-off for its PR's close, a trusted user's
// reopen moves the task back, needs-human → pr-open on pr-reopened, keyed
// by the reopened event, with pr=, naming the reopener (SPEC §7.2). On
// that pass the pull request item comes back, with its key, so it isn't
// announced again, notify is assigned to the PR again and unassigned from
// the task. An untrusted reopen moves nothing, and the hand-off stands,
// with one read; the owner's close and reopen after it moves the task
// back.
func TestPRReconcileReopenAtHandoff(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		first string // who reopens first
	}{
		{"by the owner", digestRunOwner},
		{"by someone untrusted, then the owner", "mallory"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, f, d := prReconcileRig(t)
			prReconcileHandOff(t, f, d)
			news := len(f.State().Issues[2].Comments)
			id := f.ReopenPR(2, tt.first)
			if tt.first != digestRunOwner {
				from, reqs := len(f.Writes()), len(f.Requests())
				iterate(t, d)
				if got := prTimelineReads(f, reqs); got != 1 || pickStage(f, 1) != "stage:needs-human" || len(prReconcileMoves(f)) != 1 || len(assignWrites(f, from)) != 0 {
					t.Errorf("after mallory's reopen: %d reads of the PR's timeline, the task at %s, its moves %q, assignee writes %q; want one read, and the hand-off standing",
						got, pickStage(f, 1), prReconcileMoves(f), assignWrites(f, from))
				}
				iterate(t, d)
				prItemQuiet(t, f, d, "after mallory's reopen")
				f.ClosePR(2, digestRunOwner)
				id = f.ReopenPR(2, digestRunOwner)
			}
			from := len(f.Writes())
			iterate(t, d)
			if got := pickStage(f, 1); got != "stage:pr-open" {
				t.Fatalf("after the owner's reopen, the task is at %s; want pr-open", got)
			}
			c := prReconcileNewest(t, f)
			want := "`" + digestRunOwner + "` reopened PR #2, so this task is back at `pr-open`. It waits for your review and merge, and merging it closes this task.\n\n" +
				"<!-- issuedog:transition from=needs-human to=pr-open event=" + strconv.FormatInt(id, 10) + " pr=2 -->"
			if c.Author.Login != app || c.Body != want {
				t.Errorf("the move back =\n%s\nwant\n%s", c.Body, want)
			}
			digest := f.State().Issues[2]
			if !strings.Contains(digest.Body, prItemBody) || strings.Contains(digest.Body, "**Hand-off**") || len(digest.Comments) != news {
				t.Errorf("the digest =\n%s\nwith %d comments; want the pull request item back, no hand-off, and no new announcement", digest.Body, len(digest.Comments))
			}
			if got, want := assignWrites(f, from), []string{assignRemove(1, digestRunOwner), assignAdd(2, digestRunOwner)}; !slices.Equal(got, want) {
				t.Errorf("the pass wrote assignees %q; want %q", got, want)
			}
			iterate(t, d)
			prItemQuiet(t, f, d, "back at pr-open")
		})
	}
}

// A trusted reopen and the end of the hand-off in one pass: /done, or a
// 👍 on the hand-off, handled in step 1 first, sends the task to ready, a
// new round, and the reopen moves it nowhere, on that pass or after
// (SPEC §7.2).
func TestPRReconcileReopenAndDone(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		end  func(f *githubtest.Fake, handoff int64) string // ends the hand-off, and returns the key of the move it makes
	}{
		{"/done", func(f *githubtest.Fake, _ int64) string {
			return "cmd=" + strconv.FormatInt(f.AddComment(1, digestRunOwner, "/done"), 10)
		}},
		{"👍", func(f *githubtest.Fake, handoff int64) string {
			return "cmd=r" + strconv.FormatInt(f.AddReaction(handoff, digestRunOwner, "+1"), 10)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, f, d := prReconcileRig(t)
			handoff := prReconcileHandOff(t, f, d)
			f.ReopenPR(2, digestRunOwner)
			key := tt.end(f, handoff)
			iterate(t, d)
			iterate(t, d)
			if got := pickStage(f, 1); got != "stage:ready" || len(prReconcileMoves(f)) != 1 {
				t.Errorf("the task is at %s, its moves %q; want ready, and no move back to pr-open", got, prReconcileMoves(f))
			}
			back := "<!-- issuedog:transition from=needs-human to=ready " + key + " "
			if !slices.ContainsFunc(f.State().Issues[0].Comments, func(c githubtest.CommentState) bool {
				return c.Author == app && strings.Contains(c.Body, back)
			}) {
				t.Errorf("the task's comments = %+v; want a move back to ready starting %s", f.State().Issues[0].Comments, back)
			}
		})
	}
}

// A merge into main is no close here: its "Closes #N" closes the task,
// which the close judge accepts as completed (SPEC §6.4), with no
// hand-off, and the pull request item ends, with notify unassigned from
// the PR, and nobody assigned to the task.
func TestPRReconcileMerge(t *testing.T) {
	t.Parallel()
	_, f, d := prReconcileRig(t)
	f.MergePR(2, digestRunOwner)
	iterate(t, d)
	iterate(t, d)
	task := f.State().Issues[0]
	if !task.Closed || !slices.Contains(task.Labels, "stage:completed") || len(prReconcileMoves(f)) != 0 {
		t.Errorf("after the merge, the task is closed %t, labelled %q, its moves %q; want closed, stage:completed, and no hand-off",
			task.Closed, task.Labels, prReconcileMoves(f))
	}
	if pr := assignees(f, 2); len(pr) != 0 || len(task.Assignees) != 0 || strings.Contains(f.State().Issues[2].Body, "#1:") {
		t.Errorf("after the merge, the PR's assignees = %q, the task's %q, the digest\n%s\nwant nobody, and nothing on the task", pr, task.Assignees, f.State().Issues[2].Body)
	}
}

// A merge of a PR whose "Closes #N" someone edited out closes nothing, and
// is no close without merging either: the task stays at pr-open with no
// hand-off, and its item ends, as the PR isn't open (SPEC §7.2).
func TestPRReconcileMergeClosingNothing(t *testing.T) {
	t.Parallel()
	_, f, d := prReconcileRig(t)
	if _, err := f.Client(t).UpdatePullRequest(context.Background(), "o", "r", 2, nil, new("What changed."), nil); err != nil {
		t.Fatal(err)
	}
	f.MergePR(2, digestRunOwner)
	iterate(t, d)
	iterate(t, d)
	task := f.State().Issues[0]
	if task.Closed || pickStage(f, 1) != "stage:pr-open" || len(prReconcileMoves(f)) != 0 || strings.Contains(f.State().Issues[2].Body, "#1:") {
		t.Errorf("after the merge, the task is closed %t, at %s, its moves %q, the digest\n%s\nwant it open at pr-open, no hand-off, and no item",
			task.Closed, pickStage(f, 1), prReconcileMoves(f), f.State().Issues[2].Body)
	}
}

// A task a command on another issue touched, as its story's /hold, waits
// for its next pass, read again, as its snapshot may be out of date: its
// PR's close in the same pass hands nothing off then, and once the task
// is read, held, nothing still (SPEC §6.4).
func TestPRReconcileTouched(t *testing.T) {
	t.Parallel()
	o := prItemOrigin(t)
	f := prItemFake(t, o)
	story := f.AddIssue(digestRunOwner, "A story", "The spec.", "type:story")
	f.AddLabel(story, app, "stage:planning")
	f.AddComment(story, app, "<!-- issuedog:transition from=intake to=planning event=p1 -->")
	f.SetParent(1, story)
	d := prItemDaemon(t, f, o)
	iterate(t, d)
	iterate(t, d)
	f.AddComment(story, digestRunOwner, "/hold")
	f.ClosePR(2, digestRunOwner)
	iterate(t, d)
	iterate(t, d)
	if got := pickStage(f, 1); got != "stage:held" || len(prReconcileMoves(f)) != 0 {
		t.Errorf("after the story's /hold and the PR's close, the task is at %s, its moves %q; want held, and no hand-off", got, prReconcileMoves(f))
	}
}

// A fresh daemon at each step: one started on a task at pr-open whose PR
// closed hands it off on its first pass, one started at that hand-off with
// the PR reopened by the owner moves it back, and one started back at
// pr-open reads the PR's timeline once, for the start, and moves nothing.
func TestPRReconcileRestart(t *testing.T) {
	t.Parallel()
	o, f, _ := prReconcileRig(t)
	f.ClosePR(2, digestRunOwner)
	key := prReconcileClosedKey(t, f)
	iterate(t, prItemDaemon(t, f, o))
	// The fresh daemon never saw the pull request item, so the PR keeps
	// notify, as one whose item ended while no daemon ran does (SPEC §9.6).
	prReconcileHandoff(t, f, key, prReconcileClosedIntro, prReconcileClosedWhy)

	id := f.ReopenPR(2, digestRunOwner)
	iterate(t, prItemDaemon(t, f, o))
	if got, want := prReconcileNewest(t, f).Body, "<!-- issuedog:transition from=needs-human to=pr-open event="+strconv.FormatInt(id, 10)+" pr=2 -->"; pickStage(f, 1) != "stage:pr-open" || !strings.HasSuffix(got, want) {
		t.Errorf("the restart at the hand-off left the task at %s, its newest comment\n%s\nwant pr-open, and the move ending %s", pickStage(f, 1), got, want)
	}

	d := prItemDaemon(t, f, o)
	from, reqs := len(prReconcileMoves(f)), len(f.Requests())
	iterate(t, d)
	if got := prTimelineReads(f, reqs); got != 1 || pickStage(f, 1) != "stage:pr-open" || len(prReconcileMoves(f)) != from {
		t.Errorf("the restart at pr-open: %d reads of the PR's timeline, the task at %s, %d moves; want one read, at pr-open, and none new", got, pickStage(f, 1), len(prReconcileMoves(f))-from)
	}
	iterate(t, d)
	prItemQuiet(t, f, d, "after the restart at pr-open")
}

// Replay passes for both hand-offs and the trusted reopen's move, whatever
// write a crash comes at (SPEC §5.2): each pass is a fresh daemon's, with
// the digest and assignment off, so its writes are the move's alone. A
// fresh daemon never unassigns notify from a PR it didn't assign
// (SPEC §9.6), so a crash before the hand-off, which leaves the pull
// request item standing for that pass, would leave the PR assigned after
// a restart: TestPRLabelAndAssigneesReplay replays the assignees' writes.
func TestPRReconcileReplay(t *testing.T) {
	t.Parallel()
	o := prItemOrigin(t)
	for _, tt := range []struct {
		name  string
		setup func(f *githubtest.Fake)
		want  []string
	}{
		{"closed", func(f *githubtest.Fake) { f.ClosePR(2, digestRunOwner) },
			[]string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:pr-open"}},
		{"an untrusted reopen", func(f *githubtest.Fake) {
			f.ClosePR(2, digestRunOwner)
			f.ReopenPR(2, "mallory")
		}, []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:pr-open"}},
		{"a trusted reopen at the hand-off", func(f *githubtest.Fake) {
			f.ClosePR(2, digestRunOwner)
			f.RemoveLabel(1, app, "stage:pr-open")
			f.AddLabel(1, app, "stage:needs-human")
			f.AddComment(1, app, prReconcileClosedIntro+"Reopen it.\n\n<!-- issuedog:transition from=pr-open to=needs-human event=c2-1 -->")
			f.ReopenPR(2, digestRunOwner)
		}, []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:needs-human"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := func(tb testing.TB) *githubtest.Fake {
				f := prItemFake(tb, o)
				tt.setup(f)
				f.ForgetWrites()
				return f
			}
			pass := func(f *githubtest.Fake) error {
				d := prItemDaemon(t, f, o)
				d.Digest, d.Assign = false, false
				return d.Iterate(t.Context())
			}
			clean := setup(t)
			if err := pass(clean); err != nil {
				t.Fatal(err)
			}
			if got := writesOf(clean); !slices.Equal(got, tt.want) {
				t.Errorf("the clean pass wrote %q; want %q", got, tt.want)
			}
			githubtest.Replay(t, setup, pass)
		})
	}
}
