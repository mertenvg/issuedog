package daemon

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The pull request item and the stalled task's item (SPEC §9.4), the PR's
// kept listing, and its task's PR, on their own.

// prItemHead is the head the PR step verified in these snapshots.
var prItemHead = strings.Repeat("ab", 20)

// prItemMoved is the App's comment moving a task from verifying on its
// pass, run rv, to to, with more fields after head=.
func prItemMoved(to, more string) string {
	return "<!-- issuedog:transition from=verifying to=" + to + " run=rv head=" + prItemHead + more + " -->"
}

// prItemSnap is task 1 at stage, with the App's comments: the
// implementer's move to verifying at prItemHead, the verifier's pass on
// it, run rv, and then more.
func prItemSnap(stage string, more ...string) *github.Snapshot {
	comments := append([]string{
		"<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head=" + prItemHead + " -->",
		"The verifier passed.\n\n<!-- issuedog:verify verdict=pass run=rv head=" + prItemHead + " attempt=1 -->",
	}, more...)
	s := waitSnap(1, []string{"stage:" + stage, "priority:p1"}, comments...)
	s.Type = "task"
	return s
}

// A task at pr-open whose PR is listed open has the pull request item,
// pr-<n>-<PR number>: its kind ranks as an approval's, its lines name the
// PR and its raw title, and say merging closes the task and closing it
// hands the task back, it links the PR, and assignment assigns the PR.
// With no PR listed, or one listed closed, or at any other stage, there's
// none.
func TestWaitFindPR(t *testing.T) {
	t.Parallel()
	open := &listedPR{Number: 7, Title: "feat: x <b> @eve", Open: true}
	in := waitIn(prItemSnap("pr-open", prItemMoved("pr-open", " pr=7")), Dirt{})
	in.PR = open
	it := waitFindOne(t, in)
	want := []waitLine{
		{Text: "feat: x <b> @eve", PR: 7},
		{Text: "It waits for your review. Merging it closes this task, and closing it without merging hands the task back to you."},
	}
	if it.Kind != waitPR || it.Key != "pr-1-7" || it.Link != "https://github.com/o/r/pull/7" || it.AssignPR != 7 || it.Comment != state.BodyID ||
		it.Priority != 1 || !slices.Equal(it.Lines, want) {
		t.Errorf("the item = %+v; want a pr keyed pr-1-7, linking PR 7, assigning it, p1, its lines %+v", it, want)
	}
	if waitPR.rank() != waitApproval.rank() || waitPR.String() != "pr" || waitKeyPrefix[waitPR] != "pr" {
		t.Errorf("a pull request ranks %d, an approval %d, named %s, keyed %s; want the same rank, pr and pr",
			waitPR.rank(), waitApproval.rank(), waitPR, waitKeyPrefix[waitPR])
	}
	closed := *open
	closed.Open = false
	for name, c := range map[string]struct {
		stage string
		pr    *listedPR
		moved string // a move out of pr-open since, if any
	}{
		"no PR listed":         {"pr-open", nil, ""},
		"its PR closed":        {"pr-open", &closed, ""},
		"a task held":          {"held", open, "<!-- issuedog:transition from=pr-open to=held cmd=900 -->"},
		"a task back at ready": {"ready", open, "<!-- issuedog:transition from=pr-open to=ready event=l900 -->"},
	} {
		comments := []string{prItemMoved("pr-open", " pr=7")}
		if c.moved != "" {
			comments = append(comments, c.moved)
		}
		in := waitIn(prItemSnap(c.stage, comments...), Dirt{})
		in.PR = c.pr
		if items, err := waitFind(in); err != nil || len(items) != 0 {
			t.Errorf("%s: got %+v, %v; want no item", name, items, err)
		}
	}
}

// A task back at verifying on its pass's head, after that pass moved it
// out once, is stalled: its item, stalled-<n>-<run>, ranks as a hand-off,
// says the pass moves it no more and it holds the worker, and to set the
// stage that pass moved it to back, or push a new head, and links the
// issue. A task at verifying whose pass hasn't moved it, or whose latest
// move into verifying names another head, has none.
func TestWaitFindStalled(t *testing.T) {
	t.Parallel()
	back := "<!-- issuedog:transition from=pr-open to=verifying event=l900 -->"
	for name, c := range map[string]struct {
		moved, to string
	}{
		"from pr-open":     {prItemMoved("pr-open", " pr=7"), "pr-open"},
		"from needs-human": {prItemMoved("needs-human", ""), "needs-human"},
	} {
		it := waitFindOne(t, waitIn(prItemSnap("verifying", c.moved, back), Dirt{}))
		want := []string{
			"This task is back at `verifying`, but its pass, run `rv`, moved it to `" + c.to + "` once already, so the pass moves it no more. " +
				"It holds the worker, so no other task is picked.",
			"Set `stage:" + c.to + "` back, or push a new head.",
		}
		if it.Kind != waitStalled || it.Key != "stalled-1-rv" || it.Link != "https://github.com/o/r/issues/1" || it.AssignPR != 0 ||
			!slices.Equal(waitTexts(it.Lines), want) {
			t.Errorf("%s: the item = %+v; want a stalled task keyed stalled-1-rv, linking the issue, its lines %q", name, it, want)
		}
	}
	if waitStalled.rank() != waitHandoff.rank() || waitStalled.String() != "stalled" || waitKeyPrefix[waitStalled] != "stalled" {
		t.Errorf("a stalled task ranks %d, a hand-off %d, named %s, keyed %s; want the same rank, stalled and stalled",
			waitStalled.rank(), waitHandoff.rank(), waitStalled, waitKeyPrefix[waitStalled])
	}
	other := "<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head=" + strings.Repeat("cd", 20) + " -->"
	for name, s := range map[string]*github.Snapshot{
		"a pass not moved yet": prItemSnap("verifying"),
		"a new head":           prItemSnap("verifying", prItemMoved("pr-open", " pr=7"), back, other),
		"at pr-open":           prItemSnap("pr-open", prItemMoved("pr-open", " pr=7")),
	} {
		if items, err := waitFind(waitIn(s, Dirt{})); err != nil || slices.ContainsFunc(items, func(it waitItem) bool { return it.Kind == waitStalled }) {
			t.Errorf("%s: got %+v, %v; want no stalled task", name, items, err)
		}
	}
}

// A task's PR is the pr= of its latest move into pr-open that names one:
// a move by hand names none, so it looks through it, and a pr= that isn't
// a number counts for none.
func TestTaskPR(t *testing.T) {
	t.Parallel()
	hand := "<!-- issuedog:transition from=verifying to=pr-open event=l900 -->"
	for name, c := range map[string]struct {
		comments []string
		want     int
	}{
		"none":            {nil, 0},
		"the PR step's":   {[]string{prItemMoved("pr-open", " pr=7")}, 7},
		"a later one":     {[]string{prItemMoved("pr-open", " pr=7"), "<!-- issuedog:transition from=needs-human to=pr-open run=rw pr=9 -->"}, 9},
		"a move by hand":  {[]string{prItemMoved("pr-open", " pr=7"), hand}, 7},
		"not a number":    {[]string{prItemMoved("pr-open", " pr=x7")}, 0},
		"a move to other": {[]string{prItemMoved("needs-human", " pr=7")}, 0},
	} {
		s := prItemSnap("pr-open", c.comments...)
		if got := taskPR(state.Derive(s, waitSlug, Dirt{}.Has)); got != c.want {
			t.Errorf("%s: taskPR = %d, want %d", name, got, c.want)
		}
	}
}

// The item's line naming the PR escapes its raw title as an item's Title
// is, so it makes no mention, marker, link or image, and the body is read
// back after a restart with its kind, priority and link.
func TestDigestRenderPR(t *testing.T) {
	t.Parallel()
	hostile := "feat: [x](https://evil.example) ![i](https://evil.example/i.png) @eve <!-- issuedog:digest -->"
	it := waitItem{Issue: 1, Kind: waitPR, Key: "pr-1-7", Title: "Refuse an expired quote", Priority: 1, Unblocks: 1,
		Lines: waitPRLines(listedPR{Number: 7, Title: hostile, Open: true}), Link: prLink("o", "r", 7), AssignPR: 7, Comment: state.BodyID}
	body := digestRenderBody("", []waitItem{it})
	digestRenderCheck(t, body)
	want := "- **PR #7**: " + digestRenderTitle(hostile) + "\n"
	if !strings.Contains(body, want) || strings.Contains(body, " [x](") || strings.Contains(body, "![i]") {
		t.Errorf("the body =\n%s\nwant the PR's line\n%s", body, want)
	}
	if got := digestRenderPR(7, ""); got != "- **PR #7**" {
		t.Errorf("a PR with no title renders %q, want - **PR #7**", got)
	}
	if !digestRenderMentions(it) {
		t.Errorf("a pull request gets no mention; want one, as the move to pr-open has no cc line")
	}
	kept := digestKeptParse(body)[1]
	if len(kept) != 1 || kept[0].Kind != waitPR || kept[0].Priority != 1 || kept[0].Link != "https://github.com/o/r/pull/7" ||
		kept[0].Comment != state.BodyID || kept[0].AssignPR != 0 {
		t.Errorf("read back %+v; want the pull request, p1, linking the PR, assigning nothing", kept)
	}
}

// A cached pull request item stands only while its PR is listed open, and
// names the PR's title as last listed (digestCacheOf), and assignment
// takes the PR from it, never the task.
func TestDigestCacheOfPR(t *testing.T) {
	t.Parallel()
	it := waitItem{Issue: 1, Kind: waitPR, Key: "pr-1-7", AssignPR: 7, Lines: waitPRLines(listedPR{Number: 7, Title: "old"})}
	d := &Daemon{digestCache: map[int][]waitItem{1: {it}}, prs: map[int]listedPR{7: {Number: 7, Title: "new", Open: true}}}
	got := d.digestCacheOf(1)
	if len(got) != 1 || got[0].Lines[0].Text != "new" || d.digestCache[1][0].Lines[0].Text != "old" {
		t.Errorf("with the PR open, the items = %+v; want the one, naming its new title, and the cache left as it was", got)
	}
	for name, p := range map[string]listedPR{"closed": {Number: 7, Title: "new"}, "merged": {Number: 7, Merged: true}} {
		d.prs[7] = p
		if got := d.digestCacheOf(1); len(got) != 0 {
			t.Errorf("with the PR %s, the items = %+v; want none", name, got)
		}
	}
	delete(d.prs, 7)
	if got := d.digestCacheOf(1); len(got) != 0 {
		t.Errorf("with the PR not listed, the items = %+v; want none", got)
	}
}

// The poll keeps each pull request the App opened, as listed, with its
// state, merge, labels and assignees, and none someone else opened (SPEC
// §7.5), and keeps none of them in the pipeline.
func TestPollKeepsAppPRs(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", waitSlug)
	task := f.AddIssue("owner", "Task", "")
	mine := f.AddIssue(waitSlug+"[bot]", "feat: mine", "Closes #1", "priority:p1")
	f.SetPullRequest(mine)
	f.SetAssignees(mine, "owner")
	theirs := f.AddIssue("mallory", "feat: theirs", "")
	f.SetPullRequest(theirs)
	closed := f.AddIssue(waitSlug+"[bot]", "feat: closed", "")
	f.SetPullRequest(closed)
	f.Close(closed)
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: waitSlug}}, stale: map[int]bool{}}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := slices.Sorted(maps.Keys(d.prs)); !slices.Equal(got, []int{mine, closed}) {
		t.Fatalf("the poll kept PRs %v; want the App's, %d and %d", got, mine, closed)
	}
	if p := d.prs[mine]; p.Number != mine || p.Title != "feat: mine" || !p.Open || p.Merged || !slices.Equal(p.Labels, []string{"priority:p1"}) ||
		!slices.Equal(p.Assignees, []string{"owner"}) || p.UpdatedAt.IsZero() {
		t.Errorf("the App's open PR is kept as %+v; want it open, unmerged, with priority:p1, assigned owner", p)
	}
	if p := d.prs[closed]; p.Open || p.ClosedAt.IsZero() {
		t.Errorf("the App's closed PR is kept as %+v; want it closed, with its closed_at", p)
	}
	if !d.stale[task] || d.stale[mine] || d.stale[theirs] || d.stale[closed] {
		t.Errorf("the poll marked %v stale; want the task alone", d.stale)
	}
}
