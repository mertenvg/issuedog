package daemon_test

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A human task in the digest and in assignment against the fakes (#287,
// plan #250's T7), with step 1 wired as main wires it. Most file each as
// the App's issue, with the body humanBody writes (#284) and the first
// move S3 posts on it; TestHumanItemFromChildren has ChildrenVisit create
// them.

// humanItemGolden is the body humanBody writes for plan #124 rev 1's item
// T3, a human task with a file check, as TestChildSpecsGolden keeps it,
// with its cc line for mertenvg.
const humanItemGolden = "testdata/children/human-task.md"

// humanItemBody is a human task's body as the App creates it: the golden
// humanBody wrote (humanItemGolden), with a cc line for cc.
func humanItemBody(t testing.TB, cc string) string {
	t.Helper()
	b, err := os.ReadFile(humanItemGolden)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimSuffix(string(b), "\n")
	if strings.Count(body, "\ncc @mertenvg\n") != 1 {
		t.Fatalf("%s has no one cc line for mertenvg:\n%s", humanItemGolden, body)
	}
	return strings.Replace(body, "\ncc @mertenvg\n", "\ncc @"+cc+"\n", 1)
}

// humanItemGoal and humanItemDone are the Goal and "Done when" lines the
// digest copies from humanItemBody: "Done when" is the daemon's sentence
// for the file check, without the sentence on how to finish, which the
// digest's next line says in its own words.
const (
	humanItemGoal = "- Goal: Set the quote lifetime to 24 hours in production's settings.\n"
	humanItemDone = "- Done when: The file `deploy/production.env` on `main` contains `QUOTE_TTL=24h`.\n" +
		"- When it's done, reply `/done` or react 👍 to the issue.\n"
)

// humanItemFile files a human task at needs-human, as the App creates it,
// with cc in its body's cc line, blocked by each of blockers, and returns
// its number.
func humanItemFile(t testing.TB, f *githubtest.Fake, cc string, blockers ...int) int {
	t.Helper()
	n := f.AddIssue(app, "Set the quote lifetime", humanItemBody(t, cc))
	for _, l := range []string{"type:task", "scope:infra", "priority:p2", "stage:needs-human"} {
		f.AddLabel(n, app, l)
	}
	f.AddComment(n, app, "<!-- issuedog:transition from=intake to=needs-human event=p900 -->")
	for _, b := range blockers {
		f.SetBlockedBy(n, b)
	}
	return n
}

// humanItemBlocker files an issue at ready, which may block a human task.
func humanItemBlocker(f *githubtest.Fake) int {
	return fileAt(f, "ready", "Ship the service.")
}

// humanItemUnchanged runs a pass of d, in which nothing changed, and fails
// t unless it wrote nothing and made only conditional GETs answered 304,
// which cost no rate limit.
func humanItemUnchanged(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
	t.Helper()
	from, writes := len(f.Requests()), len(f.Writes())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a pass where nothing changed wrote %+v; want nothing", w)
	}
	blockers := false
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
		blockers = blockers || strings.HasSuffix(r.Path, "/dependencies/blocked_by")
	}
	if !blockers {
		t.Errorf("a pass where nothing changed read no blockers; want the human task's read, answered 304")
	}
}

// humanItemListed is the human task items in the digest's body, the
// digest being issue n, as digestRunItems gives them.
func humanItemListed(f *githubtest.Fake, n int) []string {
	return slices.DeleteFunc(digestRunItems(f.State().Issues[n-1].Body), func(s string) bool { return !strings.Contains(s, " Human task ") })
}

// A human task with an open blocker isn't on the digest. The pass after
// its blocker closes lists it, the close changing nothing on the task
// itself, with no announcement, as its body's cc line mentioned notify. A
// pass where nothing changed then costs only 304s, the blockers' read
// among them, and a restart writes nothing. A blocker reopened takes the
// item off again, and so does the task's own close.
func TestHumanItemDigest(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	blocker := humanItemBlocker(f)
	task := humanItemFile(t, f, "owner", blocker)
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	digestN := len(f.State().Issues)
	if !strings.Contains(f.State().Issues[digestN-1].Body, "<!-- issuedog:digest -->") {
		t.Fatalf("issue %d isn't the digest:\n%s", digestN, f.State().Issues[digestN-1].Body)
	}
	if got := humanItemListed(f, digestN); len(got) != 0 {
		t.Errorf("with its blocker open, the digest lists %q; want no human task", got)
	}
	iterate(t, d) // the pass that reads the issues written to again
	humanItemUnchanged(t, f, d)

	f.CloseBy(blocker, "owner")
	news := digestRunNews(f, digestN)
	iterate(t, d)
	want := []string{fmt.Sprintf("#%d Human task p2 1", task)}
	if got := humanItemListed(f, digestN); !slices.Equal(got, want) {
		t.Errorf("after its blocker closed, the digest lists %q; want %q", got, want)
	}
	body := f.State().Issues[digestN-1].Body
	for _, line := range []string{humanItemGoal, humanItemDone,
		fmt.Sprintf("- Answer here: https://github.com/o/r/issues/%d\n", task)} {
		if !strings.Contains(body, line) {
			t.Errorf("the digest has no line %q:\n%s", line, body)
		}
	}
	if got := digestRunNews(f, digestN); got != news {
		t.Errorf("the digest has %d digest-new comments, %d before; want no announcement for a human task", got, news)
	}
	iterate(t, d)
	humanItemUnchanged(t, f, d)
	if w := digestPass(t, f, digestDaemon(t, f, o, nil)); len(w) != 0 {
		t.Errorf("a restart wrote %q; want nothing", w)
	}

	f.ReopenBy(blocker, "owner")
	iterate(t, d)
	if got := humanItemListed(f, digestN); len(got) != 0 {
		t.Errorf("after its blocker reopened, the digest lists %q; want no human task", got)
	}
	f.CloseBy(blocker, "owner")
	iterate(t, d)
	if got := humanItemListed(f, digestN); !slices.Equal(got, want) {
		t.Errorf("after its blocker closed again, the digest lists %q; want %q", got, want)
	}
	f.CloseBy(task, "owner")
	iterate(t, d)
	if got := humanItemListed(f, digestN); len(got) != 0 {
		t.Errorf("after the task closed, the digest lists %q; want no human task", got)
	}
}

// A human task with no blockers is listed on the first pass, and one with
// only closed blockers too.
func TestHumanItemUnblocked(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	blocker := humanItemBlocker(f)
	f.CloseBy(blocker, "owner")
	free := humanItemFile(t, f, "owner")
	cleared := humanItemFile(t, f, "owner", blocker)
	d := digestDaemon(t, f, o, nil)
	iterate(t, d)
	digestN := len(f.State().Issues)
	want := []string{fmt.Sprintf("#%d Human task p2 1", free), fmt.Sprintf("#%d Human task p2 1", cleared)}
	if got := humanItemListed(f, digestN); !slices.Equal(got, want) {
		t.Errorf("the digest lists %q; want %q", got, want)
	}
}

// Every issue at needs-human is visited on each pass, but only a human
// task's blockers are read: not those of a task at needs-human with no
// human=1, nor of any other issue a pass visits.
func TestHumanItemReadsOnlyHumanTasks(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	plain := fileAt(f, "needs-human", "Do it by hand.", "type:task")
	ready := humanItemBlocker(f)
	task := humanItemFile(t, f, "owner")
	var visits []int
	d := newDaemon(t, f, o, &visits)
	for range 3 {
		iterate(t, d)
	}
	if n := slices.Index(visits, plain); n < 0 || slices.Index(visits[n+1:], plain) < 0 || !slices.Contains(visits, ready) {
		t.Errorf("visits %v; want %d on more than one pass, and %d", visits, plain, ready)
	}
	read := map[string]bool{}
	for _, r := range f.Requests() {
		if strings.HasSuffix(r.Path, "/dependencies/blocked_by") {
			read[r.Path] = true
		}
	}
	if want := fmt.Sprintf("/issues/%d/dependencies/blocked_by", task); len(read) != 1 || !read[want] {
		t.Errorf("blockers read at %v; want %s alone", slices.Sorted(maps.Keys(read)), want)
	}
}

// A failed read of a human task's blockers is the pass's error, and the
// task keeps its last items, as for a failed step 1: listed, and assigned,
// whatever the blockers are meanwhile. The next good read decides.
func TestHumanItemBlockersFail(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	blocker := humanItemBlocker(f)
	f.CloseBy(blocker, "owner")
	task := humanItemFile(t, f, "owner", blocker)
	d := digestDaemon(t, f, o, nil)
	d.Assign = true
	iterate(t, d)
	digestN := len(f.State().Issues)
	want := []string{fmt.Sprintf("#%d Human task p2 1", task)}
	if got := humanItemListed(f, digestN); !slices.Equal(got, want) {
		t.Fatalf("the digest lists %q; want %q", got, want)
	}
	f.ReopenBy(blocker, "owner")
	path := fmt.Sprintf("/issues/%d/dependencies/blocked_by", task)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == path }, http.StatusInternalServerError)
	if err := d.Iterate(t.Context()); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("the blockers of human task o/r#%d", task)) {
		t.Fatalf("Iterate = %v; want the failed read of the blockers", err)
	}
	if got := humanItemListed(f, digestN); !slices.Equal(got, want) {
		t.Errorf("after a failed read, the digest lists %q; want %q kept", got, want)
	}
	if got := assignees(f, task); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("after a failed read, the task's assignees = %q; want owner kept", got)
	}
	iterate(t, d)
	if got := humanItemListed(f, digestN); len(got) != 0 {
		t.Errorf("after a good read, the digest lists %q; want no human task, its blocker open", got)
	}
	if got := assignees(f, task); len(got) != 0 {
		t.Errorf("after a good read, the task's assignees = %q; want none", got)
	}
}

// Assignment: a human task with an open blocker isn't assigned. The pass
// after its last blocker closes assigns notify, and a pass where nothing
// changed costs only 304s. A new open blocker unassigns it the pass it's
// read, as the blockers' read changes, and that blocker's close assigns
// it again. A restart writes nothing.
func TestHumanItemAssign(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	first, second := humanItemBlocker(f), humanItemBlocker(f)
	task := humanItemFile(t, f, "owner", first, second)
	d := newDaemon(t, f, o, nil)
	d.Assign = true
	assignStep(t, f, d, "the first pass")
	assignStep(t, f, d, "a pass that reads the issues written to again")
	f.CloseBy(first, "owner")
	assignStep(t, f, d, "one blocker closed")
	f.CloseBy(second, "owner")
	assignStep(t, f, d, "the last blocker closed", assignAdd(task, "owner"))
	assignStep(t, f, d, "a pass that reads the task again")
	humanItemUnchanged(t, f, d)
	assignStep(t, f, assignRestart(t, f, o), "a restart")

	third := humanItemBlocker(f)
	f.SetBlockedBy(task, third)
	assignStep(t, f, d, "a new open blocker", assignRemove(task, "owner"))
	f.CloseBy(third, "owner")
	assignStep(t, f, d, "the new blocker closed", assignAdd(task, "owner"))
	assignStep(t, f, assignRestart(t, f, o), "a restart after")

	// The task's close ends its item, but, as for every closed issue
	// (SPEC §9.6), assignment writes nothing to it: notify stays assigned,
	// out of the "is:open assignee:@me" view.
	f.CloseBy(task, "owner")
	assignStep(t, f, d, "the task closed")
	if got := assignees(f, task); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("the closed task's assignees = %q; want owner left, as on any closed issue", got)
	}
}

// assignRestart is a new daemon of o/r on f, which keeps the assignees.
func assignRestart(t *testing.T, f *githubtest.Fake, o *origin) *daemon.Daemon {
	t.Helper()
	d := newDaemon(t, f, o, nil)
	d.Assign = true
	return d
}

// humanItemReplayFake returns a setup for Replay: o/r with issue 1 at
// ready, closed if blockerClosed, blocking human task 2, assigned to
// assigned.
func humanItemReplayFake(blockerClosed bool, assigned []string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := newFake(t)
		blocker := humanItemBlocker(f)
		if blockerClosed {
			f.CloseBy(blocker, "owner")
		}
		f.SetAssignees(humanItemFile(t, f, "owner", blocker), assigned...)
		return f
	}
}

// Replay passes for the assign of a human task whose blocker closed, and
// the unassign of one whose blocker is open, whatever write a crash comes
// at.
func TestHumanItemAssignReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	for _, tc := range []struct {
		name     string
		closed   bool
		assigned []string
		want     []string
	}{
		{"an assign", true, nil, []string{assignAdd(2, "owner")}},
		{"an unassign", false, []string{"owner"}, []string{assignRemove(2, "owner")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := humanItemReplayFake(tc.closed, tc.assigned)
			clean := setup(t)
			if err := assignReplayPass(t, o)(clean); err != nil {
				t.Fatal(err)
			}
			if got := assignWrites(clean, 0); !slices.Equal(got, tc.want) {
				t.Errorf("the clean pass's assignee writes = %q; want %q", got, tc.want)
			}
			githubtest.Replay(t, setup, assignReplayPass(t, o))
		})
	}
}

// End to end (#284 and #287): the human tasks ChildrenVisit creates from
// an approved plan, T3 #4 and T2 #5 of humanPlan, at needs-human with no
// blockers, are each listed on the digest's first pass, linking the issue,
// with the Goal and "Done when" copied from the body humanBody wrote:
// "Done when" the daemon's sentence for its check, then how to finish.
func TestHumanItemFromChildren(t *testing.T) {
	t.Parallel()
	f := run(t, humanAtReview, childrenPass(t, humanKind))
	for _, n := range []int{4, 5} {
		if got := f.State().Issues[n-1].Labels; !slices.Contains(got, "stage:needs-human") {
			t.Fatalf("#%d's labels are %q; want it at needs-human", n, got)
		}
	}
	d := digestDaemon(t, f, digestNotifyOrigin(t), nil)
	iterate(t, d)
	digestN := len(f.State().Issues)
	body := f.State().Issues[digestN-1].Body
	if !strings.Contains(body, "<!-- issuedog:digest -->") {
		t.Fatalf("issue %d isn't the digest:\n%s", digestN, body)
	}
	got := humanItemListed(f, digestN)
	if len(got) != 2 || !strings.HasPrefix(got[0], "#4 Human task ") || !strings.HasPrefix(got[1], "#5 Human task ") {
		t.Errorf("the digest lists %q; want #4 and #5, each a human task", got)
	}
	for _, item := range []string{
		"- A human task waits for you.\n" +
			"- Goal: Name the owner as the code owner of everything.\n" +
			// The digest defuses a copied mention, in code or not.
			"- Done when: The file `.github/CODEOWNERS` on `main` contains `* @​owner`.\n" +
			"- When it's done, reply `/done` or react 👍 to the issue.\n" +
			"- Answer here: https://github.com/o/r/issues/4\n",
		"- A human task waits for you.\n" +
			"- Goal: Require the verify check on main.\n" +
			"- Done when: An active ruleset on `main` requires the check `issuedog/verify`.\n" +
			"- When it's done, reply `/done` or react 👍 to the issue.\n" +
			"- Answer here: https://github.com/o/r/issues/5\n",
	} {
		if !strings.Contains(body, item) {
			t.Errorf("the digest has no item\n%s\nin\n%s", item, body)
		}
	}
	// The body's sentence on how to finish says to react on "this issue",
	// which in the digest is the digest.
	if strings.Contains(body, "See the issue for what to do.") || strings.Contains(body, "I check this when you reply") {
		t.Errorf("the digest falls back, or keeps the body's sentence on how to finish:\n%s", body)
	}
}
