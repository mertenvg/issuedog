package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A hand-off ended by /done or a 👍 on it (SPEC §9.3, Blocked work), and
// its item in the digest and assignment (SPEC §9.4, §9.6).

// handbackComment is a hand-off comment as renderHandoff posts it on task
// 1, with its cc line for owner, without its marker.
const handbackComment = "The implementer needs an action only you can do before it can go on with this task.\n\n" +
	"**Goal:** Add the sandbox key as a repository secret.\n" +
	"**Why it blocks:** #1 can't proceed until the repo has the secret\n" +
	"**Steps:**\n1. Add `PAYMENTS_SANDBOX_KEY` under Secrets and variables.\n\n" +
	"**Links:**\n- https://github.com/o/r/settings/secrets/actions\n\n" +
	"**Done when:** The repo's Actions secrets list PAYMENTS_SANDBOX_KEY.\n" +
	"**Estimated time:** 5 min\n\n" +
	"When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`.\n\n" +
	"cc @owner"

// handbackPosted is handbackComment on the implementer's move of run to
// needs-human.
func handbackPosted(run string) string {
	return handbackComment + "\n\n<!-- issuedog:transition from=in-progress to=needs-human run=" + run + " -->"
}

// handbackTask is newFake with issue 1 a task the owner filed, at
// needs-human after a hand-off: the App's move to in-progress, comment
// 1001, then its hand-off, comment 1002. Then it runs more.
func handbackTask(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue("owner", "Refuse an expired quote", "Refuse it.")
		f.SetType(1, "task")
		f.AddLabel(1, app, "stage:needs-human")
		f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress event=p5 -->")
		if id := f.AddComment(1, app, handbackPosted("run1")); id != 1002 {
			t.Fatalf("the hand-off's comment ID = %d, want 1002", id)
		}
		more(t, f)
		return f
	}
}

// handbackTrust trusts owner and owner2, as a code owner, and the App and
// a helper bot, so only their being bots leaves them out.
var handbackTrust = trusted{"owner": true, "owner2": true, app: true, "helper[bot]": true}

// handbackDaemon is newDaemon with the /done handler, and the hand-off's
// 👍 trigger as its only reaction trigger, trusting handbackTrust.
func handbackDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	d := newDaemon(t, f, o, nil)
	d.Step.Commands["done"] = daemon.DoneCommand()
	d.Step.Reactions = daemon.HandoffByReaction(handbackTrust)
	return d
}

// handbackPass is a fresh handbackDaemon's pass over f, as after a
// restart.
func handbackPass(t testing.TB, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error { return handbackDaemon(t, f, o).Iterate(context.Background()) }
}

// handbackReads counts the reads of comment id's reactions from the
// from'th request on.
func handbackReads(f *githubtest.Fake, from int, id int64) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/comments/%d/reactions", id) {
			n++
		}
	}
	return n
}

// handbackState checks issue 1's labels, and its comments from the third
// on, after its move to in-progress and its hand-off.
func handbackState(t *testing.T, f *githubtest.Fake, labels []string, comments []string) {
	t.Helper()
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 2); !slices.Equal(is.Labels, labels) || is.Closed || !slices.Equal(got, comments) {
		t.Errorf("labels %q, closed %t and comments\n%q\nwant %q, open and\n%q", is.Labels, is.Closed, got, labels, comments)
	}
}

// A trusted /done on a task waiting on its hand-off sends it back to
// ready, keyed by the comment, with seen the comment, and the App 👍s it.
// The pass after writes nothing.
func TestHandoffDone(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := handbackTask(doneBy("owner", 1003))
	githubtest.Replay(t, setup, handbackPass(t, o))

	f := setup(t)
	d := handbackDaemon(t, f, o)
	iterate(t, d)
	handbackState(t, f, []string{"stage:ready"}, []string{"/done",
		"owner said the hand-off is done, so the task goes back to `ready`.\n\n" +
			"<!-- issuedog:transition from=needs-human to=ready cmd=1003 seen=1003 -->"})
	if r := f.State().Issues[0].Comments[2].Reactions; !slices.Equal(r, []string{app + " +1"}) {
		t.Errorf("/done's reactions = %q, want the App's 👍", r)
	}
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the pass after wrote %+v, want nothing", got)
	}
}

// An untrusted /done on a task waiting on its hand-off writes nothing.
func TestHandoffDoneUntrusted(t *testing.T) {
	t.Parallel()
	f := handbackTask(doneBy("eve", 1003))(t)
	iterate(t, handbackDaemon(t, f, newOrigin(t)))
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
}

// A trusted 👍 on the latest hand-off comment sends the task back to
// ready, keyed by the 👍, with seen the newest comment read, here the
// hand-off's own: the earliest trusted 👍 by someone who isn't a bot, its
// login escaped and defused. The App writes no reaction, and the pass
// after writes nothing.
func TestHandoffByReaction(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := handbackTask(func(t testing.TB, f *githubtest.Fake) {
		f.AddReaction(1002, "eve", "+1")
		f.AddReaction(1002, "helper[bot]", "+1")
		f.AddReaction(1002, "owner", "heart")
		if id := f.AddReaction(1002, "owner2", "+1"); id != 1006 {
			t.Fatalf("owner2's 👍's ID = %d, want 1006", id)
		}
		f.AddReaction(1002, "owner", "+1")
	})
	githubtest.Replay(t, setup, handbackPass(t, o))

	f := setup(t)
	d := handbackDaemon(t, f, o)
	iterate(t, d)
	handbackState(t, f, []string{"stage:ready"}, []string{
		"`owner2`'s 👍 on the hand-off says it's done, so the task goes back to `ready`.\n\n" +
			"<!-- issuedog:transition from=needs-human to=ready cmd=r1006 seen=1002 -->"})
	if got, want := writesOf(f), []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:needs-human"}; !slices.Equal(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the pass after wrote %+v, want nothing", got)
	}
}

// A trusted 👍 on the hand-off after comments that followed it sends the
// task back to ready with seen the newest comment read, the newest of
// them, not the hand-off's own (SPEC §5, seen on an approval by 👍), as
// seen bounds the next round's input: owner's comment, 1003, then eve's,
// 1004, then owner's 👍, 1005.
func TestHandoffByReactionSeenNewest(t *testing.T) {
	t.Parallel()
	f := handbackTask(func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "owner", "I've added the secret, checking the list now.")
		if id := f.AddComment(1, "eve", "Is it the sandbox key or the live one?"); id != 1004 {
			t.Fatalf("eve's comment ID = %d, want 1004", id)
		}
		if id := f.AddReaction(1002, "owner", "+1"); id != 1005 {
			t.Fatalf("owner's 👍's ID = %d, want 1005", id)
		}
	})(t)
	iterate(t, handbackDaemon(t, f, newOrigin(t)))
	handbackState(t, f, []string{"stage:ready"}, []string{
		"I've added the secret, checking the list now.",
		"Is it the sandbox key or the live one?",
		"`owner`'s 👍 on the hand-off says it's done, so the task goes back to `ready`.\n\n" +
			"<!-- issuedog:transition from=needs-human to=ready cmd=r1005 seen=1004 -->"})
}

// handbackTwice is newFake with issue 1 a task at needs-human after its
// second hand-off: its move to in-progress, 1001, its first hand-off,
// 1002, a 👍 on it that sent the task back to ready, 1003, its next move
// to in-progress, 1004, and its second hand-off, 1005.
func handbackTwice(t testing.TB) *githubtest.Fake {
	return handbackTask(func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, app, "<!-- issuedog:transition from=needs-human to=ready cmd=r77 seen=1002 -->")
		f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress event=p6 -->")
		if id := f.AddComment(1, app, handbackPosted("run2")); id != 1005 {
			t.Fatalf("the second hand-off's comment ID = %d, want 1005", id)
		}
	})(t)
}

// 👍s that end no hand-off write nothing: a trusted 👍 on an earlier
// hand-off and on the issue itself, a trusted ❤️ on the latest, and 👍s
// on it by someone untrusted, a bot and the App, and a trusted one that a
// marker names already. Only the latest hand-off's reactions are read,
// once a pass. A trusted 👍 on it then ends it.
func TestHandoffByReactionIgnored(t *testing.T) {
	t.Parallel()
	f := handbackTwice(t)
	f.AddReaction(1002, "owner", "+1")
	f.AddIssueReaction(1, "owner", "+1")
	f.AddReaction(1005, "owner", "heart")
	for _, login := range []string{"eve", "helper[bot]", app} {
		f.AddReaction(1005, login, "+1")
	}
	// A trusted 👍 that a marker of the App's names counts once, at most.
	named := f.AddReaction(1005, "owner2", "+1")
	f.AddComment(1, app, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=r%d -->", named))
	d := handbackDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if n := handbackReads(f, 0, 1005); n != 2 {
		t.Errorf("read the latest hand-off's reactions %d times, want once a pass, 2", n)
	}
	if n, issue := handbackReads(f, 0, 1002), thumbReads(f, 0); n != 0 || issue != 0 {
		t.Errorf("read the earlier hand-off's reactions %d times, and the issue's %d; want neither", n, issue)
	}
	r := f.AddReaction(1005, "owner", "+1")
	iterate(t, d)
	if last := f.State().Issues[0].Comments; !strings.Contains(last[len(last)-1].Body, fmt.Sprintf("from=needs-human to=ready cmd=r%d ", r)) {
		t.Errorf("the newest comment = %q; want the move to ready on owner's 👍 on the latest hand-off", last[len(last)-1].Body)
	}
}

// A 👍 given while the task is held waits: nothing reads it. Once /resume
// returns the task to needs-human, the hand-off from before the hold still
// waits, and the 👍 on it applies in the same pass.
func TestHandoffByReactionWhileHeld(t *testing.T) {
	t.Parallel()
	f, o := handbackTask(func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "owner", "/hold")
	})(t), newOrigin(t)
	d := handbackDaemon(t, f, o)
	iterate(t, d)
	r := f.AddReaction(1002, "owner", "+1")
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"stage:held"}) {
		t.Fatalf("labels %q; want held", is.Labels)
	}
	if n := handbackReads(f, 0, 1002); n != 0 {
		t.Errorf("read the held task's hand-off's reactions %d times, want none", n)
	}
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	is := f.State().Issues[0]
	if last := is.Comments[len(is.Comments)-1].Body; !slices.Equal(is.Labels, []string{"stage:ready"}) ||
		!strings.Contains(last, fmt.Sprintf("from=needs-human to=ready cmd=r%d ", r)) {
		t.Errorf("labels %q, newest comment %q; want ready, on the 👍 that waited", is.Labels, last)
	}
}

// A pass where nothing changed on a task waiting on its hand-off makes
// one read for it beyond the poll and the issue's own: its hand-off's
// reactions, a conditional GET answered 304. Every request is one, and
// nothing reads the issue's own reactions or its blockers.
func TestHandoffUnchanged(t *testing.T) {
	t.Parallel()
	f := handbackTask(func(testing.TB, *githubtest.Fake) {})(t)
	d := handbackDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	from := len(f.Requests())
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
		if r.Path == "/issues/1/reactions" || strings.HasSuffix(r.Path, "/dependencies/blocked_by") {
			t.Errorf("a pass where nothing changed read %s; want no read of the issue's reactions or blockers", r.Path)
		}
	}
	if n := handbackReads(f, from, 1002); n != 1 {
		t.Errorf("a pass where nothing changed read the hand-off's reactions %d times; want once, answered 304", n)
	}
}

// A read of the hand-off's reactions that fails fails the pass, with
// nothing written, and the next pass tries again.
func TestHandoffByReactionReadFails(t *testing.T) {
	t.Parallel()
	f := handbackTask(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") })(t)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/issues/comments/1002/reactions" }, http.StatusBadGateway)
	d := handbackDaemon(t, f, newOrigin(t))
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "hand-off done by 👍 on o/r#1: ") {
		t.Fatalf("Iterate: %v, want the failed read's error, from the 👍 trigger", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("writes %+v, want none", w)
	}
	iterate(t, d)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("labels %q after the next pass; want ready", l)
	}
}

// newStepDaemon is newDaemon with step 1 as NewStep builds it, trusting
// handbackTrust.
func newStepDaemon(t testing.TB, f *githubtest.Fake) *daemon.Daemon {
	d := newDaemon(t, f, newOrigin(t), nil)
	d.Step = daemon.NewStep(d.Step.Applier, handbackTrust, func() config.Models { return d.Config.Models })
	return d
}

// With step 1 as NewStep builds it, both 👍 triggers wired, a human task
// works as before: a trusted 👍 on the issue closes it, and a 👍 on its
// first move's comment isn't read, as it has no hand-off. A trusted 👍 on
// a task's hand-off sends the task back to ready.
func TestHandoffNewStep(t *testing.T) {
	t.Parallel()
	f := humanTask("", func(t testing.TB, f *githubtest.Fake) {
		f.AddReaction(1001, "owner", "+1")
		thumbBy("owner", 1003)(t, f)
	})(t)
	iterate(t, newStepDaemon(t, f))
	if is := f.State().Issues[0]; !is.Closed {
		t.Errorf("the human task is open; want it closed on the 👍 on the issue")
	}
	if n := handbackReads(f, 0, 1001); n != 0 {
		t.Errorf("read the human task's first move's reactions %d times, want none", n)
	}
	f = handbackTask(func(t testing.TB, f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") })(t)
	iterate(t, newStepDaemon(t, f))
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("the task's labels are %q after a 👍 on its hand-off; want ready", l)
	}
}

// handbackListed is the hand-off items in the digest's body, the digest
// being issue n, as digestRunItems gives them.
func handbackListed(f *githubtest.Fake, n int) []string {
	return slices.DeleteFunc(digestRunItems(f.State().Issues[n-1].Body), func(s string) bool { return !strings.Contains(s, " Hand-off ") })
}

// While a task waits on its hand-off, the digest lists it, with its Goal
// and "Done when" copied from the hand-off comment, how to finish it and a
// link to the comment, with no announcement, as the comment's cc line
// mentioned notify, and notify is assigned. Both end on the pass whose 👍
// sends the task back to ready.
func TestHandoffItem(t *testing.T) {
	t.Parallel()
	f, o := handbackTask(func(testing.TB, *githubtest.Fake) {})(t), digestNotifyOrigin(t)
	d := digestDaemon(t, f, o, nil)
	d.Assign = true
	d.Step.Reactions = daemon.HandoffByReaction(handbackTrust)
	iterate(t, d)
	digestN := len(f.State().Issues)
	if want := []string{"#1 Hand-off p2 1"}; !slices.Equal(handbackListed(f, digestN), want) {
		t.Errorf("the digest lists %q; want %q", handbackListed(f, digestN), want)
	}
	body := f.State().Issues[digestN-1].Body
	for _, line := range []string{
		"- A hand-off waits for you: the implementer needs an action only you can do.\n",
		"- Goal: Add the sandbox key as a repository secret.\n",
		"- Done when: The repo's Actions secrets list PAYMENTS_SANDBOX_KEY.\n",
		"- When it's done, reply `/done` on the issue or react 👍 to the hand-off.\n",
		"- Answer here: https://github.com/o/r/issues/1#issuecomment-1002\n",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("the digest has no line %q:\n%s", line, body)
		}
	}
	if n := digestRunNews(f, digestN); n != 0 {
		t.Errorf("the digest has %d digest-new comments; want no announcement for a hand-off", n)
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("the task's assignees = %q; want owner", got)
	}
	f.AddReaction(1002, "owner", "+1")
	iterate(t, d)
	if got := handbackListed(f, digestN); len(got) != 0 {
		t.Errorf("after the 👍, the digest lists %q; want no hand-off", got)
	}
	if got := assignees(f, 1); len(got) != 0 {
		t.Errorf("after the 👍, the task's assignees = %q; want none", got)
	}
}
