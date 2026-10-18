package daemon_test

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A human task in the digest and in assignment against the fakes (#287,
// plan #250's T7), with step 1 wired as main wires it. S3 doesn't create
// human tasks yet, so each is filed as the App's issue, with the body
// childHumanTaskBody writes and the first move S3 posts on it.

// humanItemBody is a human task's body as the App creates it, from plan
// #9 rev 1's item T3, with a cc line for cc.
func humanItemBody(cc string) string {
	return "**Parent:** #9   **Scope:** scope:infra   **Priority:** p2\n\n" +
		"**Goal:** Set the quote lifetime in production.\n" +
		"**Why it blocks:** T2 can't be deployed until production has a lifetime.\n" +
		"**Steps:**\n1. Open `deploy/production.env`.\n2. Add `QUOTE_TTL=24h`.\n\n" +
		"**Links:**\n- https://github.com/o/r/blob/main/deploy/production.env\n\n" +
		"**Done when:** Production's settings on `main` set `QUOTE_TTL=24h`.\n" +
		"**Estimated time:** 5 min\n\n" +
		"cc @" + cc + "\n\n" +
		"<!-- issuedog:origin plan=#9 rev=1 item=T3 human=1 -->"
}

// humanItemFile files a human task at needs-human, as the App creates it,
// with cc in its body's cc line, blocked by each of blockers, and returns
// its number.
func humanItemFile(f *githubtest.Fake, cc string, blockers ...int) int {
	n := f.AddIssue(app, "Set the quote lifetime", humanItemBody(cc))
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
	task := humanItemFile(f, "owner", blocker)
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
	for _, line := range []string{"- Goal: Set the quote lifetime in production.", "- Done when: Production's settings on `main` set `QUOTE_TTL=24h`.",
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
	free := humanItemFile(f, "owner")
	cleared := humanItemFile(f, "owner", blocker)
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
	task := humanItemFile(f, "owner")
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
	task := humanItemFile(f, "owner", blocker)
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
	task := humanItemFile(f, "owner", first, second)
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
		f.SetAssignees(humanItemFile(f, "owner", blocker), assigned...)
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
