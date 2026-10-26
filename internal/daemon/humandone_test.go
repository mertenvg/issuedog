package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
	"github.com/mertenvg/issuedog/internal/state"
	"github.com/mertenvg/logr/v2"
)

// A human task closed on /done (#285).

// doneLabelCheck is a human task's origin marker fields for a check that
// the repo has the label needs-design.
const doneLabelCheck = " check=label value=needs-design"

// humanTask is newFake with issue 1 a human task the App filed from plan
// #9, its origin marker carrying human=1 and check, if any, at needs-human
// by its first move, comment 1001. Then it runs more.
func humanTask(check string, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue(app, "Add the label", "Add it.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T2 human=1"+check+" -->")
		f.SetType(1, "task")
		f.AddLabel(1, app, "stage:needs-human")
		if id := f.AddComment(1, app, "<!-- issuedog:transition from=intake to=needs-human event=p5 -->"); id != 1001 {
			t.Fatalf("first move's comment ID = %d, want 1001", id)
		}
		more(t, f)
		return f
	}
}

// doneBy posts /done on issue 1 as login, failing t unless its ID is want.
func doneBy(login string, want int64) func(t testing.TB, f *githubtest.Fake) {
	return func(t testing.TB, f *githubtest.Fake) {
		if id := f.AddComment(1, login, "/done"); id != want {
			t.Fatalf("/done's comment ID = %d, want %d", id, want)
		}
	}
}

// doneDaemon is newDaemon with the /done handler too.
func doneDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	d := newDaemon(t, f, o, nil)
	d.Step.Commands["done"] = daemon.DoneCommand()
	return d
}

// donePass is a fresh doneDaemon's two passes over f, as after a restart:
// the second judges a close the first made (closingJudge).
func donePass(t testing.TB, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := doneDaemon(t, f, o)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		return d.Iterate(context.Background())
	}
}

// doneClosed is the App's comment closing a human task on owner's /done in
// comment id, with what the check found.
func doneClosed(found string, id int64) string {
	return fmt.Sprintf("owner said this task is done. %s So I've closed it as completed.\n\n"+
		"<!-- issuedog:transition from=needs-human to=closed reason=completed cmd=%d seen=%d -->", found, id, id)
}

// checkReads are f's requests from the from'th on that a check makes.
func checkReads(f *githubtest.Fake, from int) []githubtest.Request {
	return slices.DeleteFunc(f.Requests()[from:], func(r githubtest.Request) bool {
		return !strings.HasPrefix(r.Path, "/labels/") && !strings.Contains(r.Path, "/issue-types") &&
			!strings.HasPrefix(r.Path, "/rules/") && r.Path != "/app/installations"
	})
}

// doneState checks issue 1's labels, whether it's closed, and its comments
// from the second on, its first move's being the first.
func doneState(t *testing.T, f *githubtest.Fake, labels []string, closed bool, comments []string) {
	t.Helper()
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 1); !slices.Equal(is.Labels, labels) || is.Closed != closed || !slices.Equal(got, comments) {
		t.Errorf("labels %q, closed %t and comments\n%q\nwant %q, %t and\n%q", is.Labels, is.Closed, got, labels, closed, comments)
	}
}

// A trusted /done on a human task whose check passes, a label check or a
// plain-text "Done when", which reads no check, closes it as completed:
// one marker, the close, then the 👍. The next pass sets stage:completed,
// and the one after writes nothing.
func TestDonePasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, check, found string
		reads              int
	}{
		{"a label check", doneLabelCheck, "The label `needs-design` exists.", 1},
		{"plain text", "", `The "Done when" has no check for me to run.`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newOrigin(t)
			setup := humanTask(tc.check, func(t testing.TB, f *githubtest.Fake) {
				f.AddRepoLabel("needs-design")
				doneBy("owner", 1002)(t, f)
			})
			githubtest.Replay(t, setup, donePass(t, o))

			f := setup(t)
			d := doneDaemon(t, f, o)
			iterate(t, d)
			doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", doneClosed(tc.found, 1002)})
			if r := f.State().Issues[0].Comments[1].Reactions; !slices.Equal(r, []string{app + " +1"}) {
				t.Errorf("/done's reactions = %q, want the App's 👍", r)
			}
			if r := stateReason(t, f, 1); r != "completed" {
				t.Errorf("state_reason = %q, want completed", r)
			}
			if r := checkReads(f, 0); len(r) != tc.reads {
				t.Errorf("check reads %+v, want %d", r, tc.reads)
			}
			writes := len(f.Writes())
			iterate(t, d)
			if got, want := writesOf(f)[writes:], []string{"POST /issues/1/labels", "DELETE /issues/1/labels/stage:needs-human"}; !slices.Equal(got, want) {
				t.Errorf("the next pass wrote %q, want %q", got, want)
			}
			doneState(t, f, []string{"stage:completed"}, true, []string{"/done", doneClosed(tc.found, 1002)})
			writes = len(f.Writes())
			iterate(t, d)
			if got := f.Writes()[writes:]; len(got) != 0 {
				t.Errorf("the pass after wrote %+v, want nothing", got)
			}
		})
	}
}

// A /done whose check fails gets the App's 👎 and one reply quoting what
// it found, and the issue stays open at needs-human; the next pass writes
// nothing. A second /done checks again, and closes it once it passes.
func TestDoneFails(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := humanTask(doneLabelCheck, doneBy("owner", 1002))
	githubtest.Replay(t, setup, donePass(t, o))

	f := setup(t)
	d := doneDaemon(t, f, o)
	iterate(t, d)
	reply := rejected("/done", "its \"Done when\" check didn't pass. This repo has no label `needs-design`", "1002")
	doneState(t, f, []string{"stage:needs-human"}, false, []string{"/done", reply})
	if r := f.State().Issues[0].Comments[1].Reactions; !slices.Equal(r, []string{app + " -1"}) {
		t.Errorf("/done's reactions = %q, want the App's 👎", r)
	}
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the next pass wrote %+v, want nothing", got)
	}

	f.AddRepoLabel("needs-design")
	doneBy("owner", 1005)(t, f) // after the reply, 1003, and the 👎, 1004
	iterate(t, d)
	doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", reply, "/done", doneClosed("The label `needs-design` exists.", 1005)})
}

// A /done on a task that isn't a human task, at needs-human, is rejected as
// not handled yet, with no check read; no new comment would do better.
func TestDoneNotHumanTask(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		fileAt(f, "needs-human", "do it")
		f.SetType(1, "task")
		doneBy("owner", 1002)(t, f)
		return f
	}
	githubtest.Replay(t, setup, donePass(t, o))
	f := setup(t)
	iterate(t, doneDaemon(t, f, o))
	doneState(t, f, []string{"stage:needs-human"}, false, []string{"/done",
		"> /done\n\nI didn't apply this command: `/done` on a blocked task isn't handled yet (build phase 3).\n\n" +
			"<!-- issuedog:reply reason=rejected cmd=1002 -->"})
	if r := checkReads(f, 0); len(r) != 0 {
		t.Errorf("check reads %+v, want none", r)
	}
}

// An untrusted /done writes nothing, and reads no check.
func TestDoneUntrusted(t *testing.T) {
	t.Parallel()
	f := humanTask(doneLabelCheck, doneBy("eve", 1002))(t)
	iterate(t, doneDaemon(t, f, newOrigin(t)))
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if r := checkReads(f, 0); len(r) != 0 {
		t.Errorf("check reads %+v, want none", r)
	}
}

// A /done posted while the task is held waits: nothing reads its check,
// and it gets no reaction. Once /resume ends the hold, it applies in the
// same pass.
func TestDoneWhileHeld(t *testing.T) {
	t.Parallel()
	f, o := humanTask("", func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "owner", "/hold")
		doneBy("owner", 1003)(t, f)
	})(t), newOrigin(t)
	d := doneDaemon(t, f, o)
	iterate(t, d)
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:held"}) || is.Closed || len(is.Comments[2].Reactions) != 0 {
		t.Fatalf("labels %q, closed %t, /done's reactions %q; want held, open, and none", is.Labels, is.Closed, is.Comments[2].Reactions)
	}
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	is = f.State().Issues[0]
	last := is.Comments[len(is.Comments)-1].Body
	if !is.Closed || !strings.Contains(last, "from=needs-human to=closed reason=completed cmd=1003") {
		t.Errorf("closed %t, last comment %q; want it closed on the /done that waited", is.Closed, last)
	}
}

// A check read that fails fails the pass, with no reply and no reaction,
// and the next pass checks again.
func TestDoneReadFails(t *testing.T) {
	t.Parallel()
	f, o := humanTask(doneLabelCheck, func(t testing.TB, f *githubtest.Fake) {
		f.AddRepoLabel("needs-design")
		doneBy("owner", 1002)(t, f)
	})(t), newOrigin(t)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/labels/needs-design" }, http.StatusBadGateway)
	d := doneDaemon(t, f, o)
	if err := d.Iterate(context.Background()); err == nil {
		t.Fatal("Iterate: no error, want the failed check read's")
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("writes %+v, want none", w)
	}
	iterate(t, d)
	doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", doneClosed("The label `needs-design` exists.", 1002)})
}

// A crash after the close's marker: the next pass closes the issue with
// the marker's reason, posts no disagreement and no second marker, and
// 👍s the /done.
func TestDoneCrashAfterMarker(t *testing.T) {
	t.Parallel()
	f := humanTask(doneLabelCheck, func(t testing.TB, f *githubtest.Fake) {
		doneBy("owner", 1002)(t, f)
		f.AddComment(1, app, doneClosed("The label `needs-design` exists.", 1002))
	})(t)
	iterate(t, doneDaemon(t, f, newOrigin(t)))
	if got, want := writesOf(f), []string{"PATCH /issues/1", "POST /issues/comments/1002/reactions"}; !slices.Equal(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", doneClosed("The label `needs-design` exists.", 1002)})
	if r := checkReads(f, 0); len(r) != 0 {
		t.Errorf("check reads %+v, want none: the command is applied", r)
	}
}

// Apply without Check just before it, for the same /done, runs the check
// itself: one that fails is an error, with nothing written, and one that
// passes closes the task.
func TestDoneApplyChecksAgain(t *testing.T) {
	t.Parallel()
	setup := humanTask(doneLabelCheck, doneBy("owner", 1002))
	apply := func(f *githubtest.Fake) error {
		a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
		s, err := a.Client.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		cmd, ok := state.ParseCommand("/done")
		if !ok {
			t.Fatal("/done doesn't parse")
		}
		return daemon.DoneCommand().Apply(context.Background(), a, daemon.Cmd{Command: cmd, Comment: s.Comments[1], S: s})
	}
	f := setup(t)
	if err := apply(f); err == nil || !strings.Contains(err.Error(), "no longer passes") {
		t.Errorf("Apply on a failing check: %v, want an error saying it no longer passes", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	f.AddRepoLabel("needs-design")
	if err := apply(f); err != nil {
		t.Fatal(err)
	}
	doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", doneClosed("The label `needs-design` exists.", 1002)})
}

// A move to closed as not planned, as #195 closes a dropped child, closes
// the issue as not planned, and the next pass sets stage:discarded.
func TestCloseNotPlannedDiscarded(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	fileAt(f, "ready", "do it")
	f.SetType(1, "task")
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
	s, err := a.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	m := daemon.Move{To: state.Closed, StateReason: "not_planned", Trigger: state.TriggerClosed, Key: state.Key{Field: "run", ID: "drop"}}
	if err := a.Transition(context.Background(), s, clean, m); err != nil {
		t.Fatal(err)
	}
	if r := stateReason(t, f, 1); r != "not_planned" {
		t.Errorf("state_reason = %q, want not_planned", r)
	}
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	if is := f.State().Issues[0]; !is.Closed || !slices.Equal(is.Labels, []string{"stage:discarded"}) {
		t.Errorf("closed %t, labels %q; want closed at stage:discarded", is.Closed, is.Labels)
	}
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the pass after wrote %+v, want nothing", got)
	}
}

// A human task closed on a 👍 on the issue itself (#286).

// thumbTrust trusts owner and owner2, a login no user can have, to show
// it's escaped and defused, and, to show they're ignored whatever trust
// says, the App and a helper bot.
var thumbTrust = trusted{"owner": true, "owner2": true, errAckHostile: true, app: true, "helper[bot]": true}

// thumbBy reacts 👍 to issue 1 itself as login, failing t unless the
// reaction's ID is want.
func thumbBy(login string, want int64) func(t testing.TB, f *githubtest.Fake) {
	return func(t testing.TB, f *githubtest.Fake) {
		if id := f.AddIssueReaction(1, login, "+1"); id != want {
			t.Fatalf("%s's 👍's ID = %d, want %d", login, id, want)
		}
	}
}

// thumbDaemon is doneDaemon with the 👍 trigger as its only reaction
// trigger, trusting thumbTrust.
func thumbDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	d := doneDaemon(t, f, o)
	d.Step.Reactions = daemon.DoneByReaction(thumbTrust)
	return d
}

// thumbPass is a fresh thumbDaemon's two passes over f, as after a
// restart: the second judges a close the first made (closingJudge).
func thumbPass(t testing.TB, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := thumbDaemon(t, f, o)
		if err := d.Iterate(context.Background()); err != nil {
			return err
		}
		return d.Iterate(context.Background())
	}
}

// thumbClosed is the App's comment closing a human task on who's 👍 r,
// who as the comment quotes it, with what the check found, seen being
// the first move, comment 1001.
func thumbClosed(who, found string, r int64) string {
	return fmt.Sprintf("`%s`'s 👍 says this task is done. %s So I've closed it as completed.\n\n"+
		"<!-- issuedog:transition from=needs-human to=closed reason=completed cmd=r%d seen=1001 -->", who, found, r)
}

// thumbRejected is the App's one reply to who's 👍 r, who as the reply
// quotes it, whose check found found.
func thumbRejected(who, found string, r int64) string {
	return fmt.Sprintf("> 👍 by `%s`\n\nI didn't close this task: its \"Done when\" check didn't pass. %s "+
		"To check again, remove the 👍 from this issue and add it again, or post `/done`.\n\n"+
		"<!-- issuedog:reply reason=rejected cmd=r%d -->", who, found, r)
}

// thumbReads counts the reads of issue 1's own reactions from the from'th
// request on.
func thumbReads(f *githubtest.Fake, from int) int {
	n := 0
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == "/issues/1/reactions" {
			n++
		}
	}
	return n
}

// A trusted 👍 on a human task whose check passes, a label check or a
// plain-text "Done when", which reads no check, closes it as completed,
// once: one marker keyed by the 👍, then the close, with no reaction of
// the App's. The newest trusted 👍 is the one taken, and its login is
// escaped and defused. The next pass sets stage:completed, and the one
// after writes nothing.
func TestDoneByReactionPasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, check string
		thumbs      func(t testing.TB, f *githubtest.Fake)
		closed      string
		reads       int
	}{
		{"a label check", doneLabelCheck, thumbBy("owner", 1002),
			thumbClosed("owner", "The label `needs-design` exists.", 1002), 1},
		{"plain text", "", thumbBy("owner", 1002),
			thumbClosed("owner", `The "Done when" has no check for me to run.`, 1002), 0},
		{"the newest trusted 👍, escaped and defused", "", func(t testing.TB, f *githubtest.Fake) {
			thumbBy("owner", 1002)(t, f)
			thumbBy(errAckHostile, 1003)(t, f)
			thumbBy("mallory", 1004)(t, f)
		}, thumbClosed("eve&lt;!--@​owner", `The "Done when" has no check for me to run.`, 1003), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			o := newOrigin(t)
			setup := humanTask(tc.check, func(t testing.TB, f *githubtest.Fake) {
				f.AddRepoLabel("needs-design")
				tc.thumbs(t, f)
			})
			githubtest.Replay(t, setup, thumbPass(t, o))

			f := setup(t)
			d := thumbDaemon(t, f, o)
			iterate(t, d)
			doneState(t, f, []string{"stage:needs-human"}, true, []string{tc.closed})
			if got, want := writesOf(f), []string{"POST /issues/1/comments", "PATCH /issues/1"}; !slices.Equal(got, want) {
				t.Errorf("writes %q, want %q", got, want)
			}
			if r := stateReason(t, f, 1); r != "completed" {
				t.Errorf("state_reason = %q, want completed", r)
			}
			if r := checkReads(f, 0); len(r) != tc.reads {
				t.Errorf("check reads %+v, want %d", r, tc.reads)
			}
			writes := len(f.Writes())
			iterate(t, d)
			if got, want := writesOf(f)[writes:], []string{"POST /issues/1/labels", "DELETE /issues/1/labels/stage:needs-human"}; !slices.Equal(got, want) {
				t.Errorf("the next pass wrote %q, want %q", got, want)
			}
			doneState(t, f, []string{"stage:completed"}, true, []string{tc.closed})
			writes = len(f.Writes())
			iterate(t, d)
			if got := f.Writes()[writes:]; len(got) != 0 {
				t.Errorf("the pass after wrote %+v, want nothing", got)
			}
		})
	}
}

// A 👍 whose check fails gets one reply saying what it found, keyed by
// the 👍, and the issue stays open at needs-human. Later passes write
// nothing and run no check for it, even once the check would pass. The 👍
// removed and added again checks again, and closes the task.
func TestDoneByReactionFails(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := humanTask(doneLabelCheck, thumbBy("owner", 1002))
	githubtest.Replay(t, setup, thumbPass(t, o))

	f := setup(t)
	d := thumbDaemon(t, f, o)
	iterate(t, d)
	reply := thumbRejected("owner", "This repo has no label `needs-design`.", 1002)
	doneState(t, f, []string{"stage:needs-human"}, false, []string{reply})
	if got, want := writesOf(f), []string{"POST /issues/1/comments"}; !slices.Equal(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	f.AddRepoLabel("needs-design")
	for range 2 { // the pass that reads the reply again, and one more
		from, writes := len(f.Requests()), len(f.Writes())
		iterate(t, d)
		if got := f.Writes()[writes:]; len(got) != 0 {
			t.Errorf("a later pass wrote %+v, want nothing", got)
		}
		if r := checkReads(f, from); len(r) != 0 {
			t.Errorf("a later pass read %+v, want no check: the 👍 has its reply", r)
		}
	}

	if !f.DeleteIssueReaction(1, 1002) {
		t.Fatal("owner's 👍 wasn't there to remove")
	}
	again := f.AddIssueReaction(1, "owner", "+1")
	iterate(t, d)
	doneState(t, f, []string{"stage:needs-human"}, true, []string{reply,
		fmt.Sprintf("`owner`'s 👍 says this task is done. The label `needs-design` exists. So I've closed it as completed.\n\n"+
			"<!-- issuedog:transition from=needs-human to=closed reason=completed cmd=r%d seen=1001 -->", again)})
}

// The reply to a 👍 whose check fails quotes its login escaped and
// defused, so it holds no marker and mentions no one.
func TestDoneByReactionFailsEscaped(t *testing.T) {
	t.Parallel()
	f := humanTask(doneLabelCheck, thumbBy(errAckHostile, 1002))(t)
	iterate(t, thumbDaemon(t, f, newOrigin(t)))
	doneState(t, f, []string{"stage:needs-human"}, false, []string{
		thumbRejected("eve&lt;!--@​owner", "This repo has no label `needs-design`.", 1002)})
}

// unreadableFound is what the check finds on an origin marker whose check
// fields couldn't be read (state.CheckUnreadable).
const unreadableFound = "I couldn't read the check on this task's `origin` marker, so it can't pass."

// unreadableDone is the App's reply to /done in comment 1002 on a human
// task whose check couldn't be read: it asks for no new comment, which
// would fail the same way, and says to close the issue by hand instead.
const unreadableDone = "> /done\n\nI didn't apply this command: its \"Done when\" check didn't pass. " + unreadableFound +
	" Close this issue by hand once the task is done.\n\n<!-- issuedog:reply reason=rejected cmd=1002 -->"

// unreadableThumb is the App's one reply to owner's 👍 1002 on a human task
// whose check couldn't be read: it doesn't say to check again, which would
// fail the same way, and says to close the issue by hand instead.
const unreadableThumb = "> 👍 by `owner`\n\nI didn't close this task: its \"Done when\" check didn't pass. " + unreadableFound +
	" Close this issue by hand once the task is done.\n\n<!-- issuedog:reply reason=rejected cmd=r1002 -->"

// A /done or a 👍 on a human task whose origin marker has check fields that
// don't record a whole check, an unknown kind or a value that doesn't
// decode, fails without a read: the /done gets the App's 👎 and the
// rejection reply, and the 👍 its one reply, each saying the check
// couldn't be read and quoting none of the fields. Neither reply suggests
// checking again, which would fail too: each says to close the issue by
// hand. The issue stays open at needs-human, and a pass where nothing
// changed after makes only conditional GETs, answered 304, and writes
// nothing.
func TestDoneUnreadableCheck(t *testing.T) {
	t.Parallel()
	for _, marker := range []struct{ name, check string }{
		{"an unknown kind", " check=app-installed value=%3C!--%20@owner"},
		{"a value that doesn't decode", " check=label value=@owner%zz"},
	} {
		for _, tc := range []struct {
			name    string
			trigger func(t testing.TB, f *githubtest.Fake)
			daemon  func(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon
			pass    func(t testing.TB, o *origin) func(*githubtest.Fake) error
			want    []string
			thumb   []string // /done's reactions
		}{
			{"/done", doneBy("owner", 1002), doneDaemon, donePass, []string{"/done", unreadableDone}, []string{app + " -1"}},
			{"a 👍", thumbBy("owner", 1002), thumbDaemon, thumbPass, []string{unreadableThumb}, nil},
		} {
			t.Run(marker.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()
				o := newOrigin(t)
				setup := humanTask(marker.check, tc.trigger)
				githubtest.Replay(t, setup, tc.pass(t, o))

				f := setup(t)
				d := tc.daemon(t, f, o)
				iterate(t, d) // the reply
				doneState(t, f, []string{"stage:needs-human"}, false, tc.want)
				if tc.thumb != nil {
					if r := f.State().Issues[0].Comments[1].Reactions; !slices.Equal(r, tc.thumb) {
						t.Errorf("/done's reactions = %q, want %q", r, tc.thumb)
					}
				}
				iterate(t, d) // the pass that reads the issue written to again
				if r := checkReads(f, 0); len(r) != 0 {
					t.Errorf("check reads %+v, want none", r)
				}
				from, writes := len(f.Requests()), len(f.Writes())
				iterate(t, d)
				if w := f.Writes()[writes:]; len(w) != 0 {
					t.Errorf("a pass where nothing changed wrote %+v; want nothing", w)
				}
				for _, r := range f.Requests()[from:] {
					if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
						t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
					}
				}
				doneState(t, f, []string{"stage:needs-human"}, false, tc.want)
			})
		}
	}
}

// unreadableWarning is the warning logged for running issue 1's check,
// on trigger, when its origin marker's check fields couldn't be read.
func unreadableWarning(trigger string) string {
	return "o/r#1: " + trigger + " ran the human task's check, but its origin marker's check fields couldn't be read, " +
		"so it fails: only a bug or a change to the marker's format does that"
}

// lostReplyDoneBy posts /done on issue 1 as login, with the App's 👎 on it
// but no reply, as when the pass that 👎'd it stopped before replying: the
// next pass posts the reply again (reword), wording it through the check.
func lostReplyDoneBy(login string) func(t testing.TB, f *githubtest.Fake) {
	return func(t testing.TB, f *githubtest.Fake) {
		f.AddReaction(f.AddComment(1, login, "/done"), app, "-1")
	}
}

// Running a check that couldn't be read logs a warning each time, on a
// /done, on a 👍, and on a 👎'd /done whose reply was lost, whose reply
// runs the check again: once for the first, none on the passes after that
// read the marker but run no check, and once more for a second /done, 👍
// or lost reply. A human task with such a marker but no /done or 👍 logs
// none, and a check that could be read, passing or failing, or a
// plain-text "Done when", logs none on a /done or a 👍.
func TestDoneUnreadableCheckWarns(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.CaptureTo).
	const marker = " check=app-installed value=go.mod"
	for _, tc := range []struct {
		name    string
		trigger func(t testing.TB, f *githubtest.Fake)
		again   func(t testing.TB, f *githubtest.Fake)
		daemon  func(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon
		warning string
	}{
		{"/done", doneBy("owner", 1002), doneBy("owner", 1005), doneDaemon, unreadableWarning("/done")}, // after the reply, 1003, and the 👎, 1004
		{"a 👍", thumbBy("owner", 1002), func(t testing.TB, f *githubtest.Fake) {
			if !f.DeleteIssueReaction(1, 1002) {
				t.Fatal("owner's 👍 wasn't there to remove")
			}
			f.AddIssueReaction(1, "owner", "+1")
		}, thumbDaemon, unreadableWarning("a 👍")},
		{"/done whose reply was lost", lostReplyDoneBy("owner"), lostReplyDoneBy("owner"), doneDaemon, unreadableWarning("/done")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := humanTask(marker, tc.trigger)(t)
			d := tc.daemon(t, f, newOrigin(t))
			var logs bytes.Buffer
			stop := logtest.CaptureTo(t, &logs, logr.W)
			defer stop()
			for range 3 { // the reply, the pass that reads it, and one where nothing changed
				iterate(t, d)
			}
			logr.Wait()
			if got := strings.Count(logs.String(), tc.warning); got != 1 || strings.Count(logs.String(), "couldn't be read") != 1 {
				t.Errorf("after the first %s, logged %q %d times, want once and no other such warning:\n%s", tc.name, tc.warning, got, logs.String())
			}
			tc.again(t, f)
			iterate(t, d)
			logr.Wait()
			if got := strings.Count(logs.String(), tc.warning); got != 2 {
				t.Errorf("after a second %s, logged %q %d times, want twice:\n%s", tc.name, tc.warning, got, logs.String())
			}
		})
	}
	t.Run("no /done or 👍", func(t *testing.T) {
		f := humanTask(marker, func(testing.TB, *githubtest.Fake) {})(t)
		d := thumbDaemon(t, f, newOrigin(t))
		var logs bytes.Buffer
		stop := logtest.CaptureTo(t, &logs, logr.W)
		defer stop()
		iterate(t, d)
		iterate(t, d)
		logr.Wait()
		if strings.Contains(logs.String(), "couldn't be read") {
			t.Errorf("logged %q, want no warning for a check that isn't run", logs.String())
		}
	})
	for _, check := range []struct {
		name, marker string
		label        bool // whether the repo has the label needs-design
		reads        int  // the check's reads
	}{
		{"a label check that passes", doneLabelCheck, true, 1},
		{"a label check that fails", doneLabelCheck, false, 1},
		{"plain text", "", false, 0},
	} {
		for _, trig := range []struct {
			name   string
			by     func(t testing.TB, f *githubtest.Fake)
			daemon func(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon
		}{
			{"/done", doneBy("owner", 1002), doneDaemon},
			{"a 👍", thumbBy("owner", 1002), thumbDaemon},
		} {
			t.Run(check.name+"/"+trig.name, func(t *testing.T) {
				f := humanTask(check.marker, func(t testing.TB, f *githubtest.Fake) {
					if check.label {
						f.AddRepoLabel("needs-design")
					}
					trig.by(t, f)
				})(t)
				d := trig.daemon(t, f, newOrigin(t))
				var logs bytes.Buffer
				stop := logtest.CaptureTo(t, &logs, logr.W)
				defer stop()
				iterate(t, d)
				iterate(t, d)
				logr.Wait()
				if r := checkReads(f, 0); len(r) != check.reads {
					t.Errorf("check reads %+v, want %d", r, check.reads)
				}
				if strings.Contains(logs.String(), "couldn't be read") {
					t.Errorf("logged %q, want no warning for a check that could be read", logs.String())
				}
			})
		}
	}
}

// 👍s from an untrusted writer, an unknown user, a bot and the App, all
// but the writer trusted by thumbTrust, write nothing and run no check,
// and so do a trusted ❤️ on the issue and a trusted 👍 on a comment.
func TestDoneByReactionIgnored(t *testing.T) {
	t.Parallel()
	f := humanTask(doneLabelCheck, func(t testing.TB, f *githubtest.Fake) {
		f.AddRepoLabel("needs-design")
		for _, login := range []string{"mallory", "eve", "helper[bot]", app} {
			f.AddIssueReaction(1, login, "+1")
		}
		f.AddIssueReaction(1, "owner", "heart")
		f.AddReaction(1001, "owner", "+1")
	})(t)
	d := thumbDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if r := checkReads(f, 0); len(r) != 0 {
		t.Errorf("check reads %+v, want none", r)
	}
	if n := thumbReads(f, 0); n != 2 {
		t.Errorf("read the issue's reactions %d times, want once a pass, 2", n)
	}
}

// A 👍 on a task at needs-human that isn't a human task writes nothing,
// and its reactions aren't read.
func TestDoneByReactionNotHumanTask(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	fileAt(f, "needs-human", "do it")
	f.SetType(1, "task")
	f.AddIssueReaction(1, "owner", "+1")
	iterate(t, thumbDaemon(t, f, newOrigin(t)))
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the issue's reactions %d times, want none", n)
	}
}

// A closed human task, its close not yet judged, gets nothing for a 👍,
// and its reactions aren't read.
func TestDoneByReactionClosed(t *testing.T) {
	t.Parallel()
	f := humanTask("", thumbBy("owner", 1002))(t)
	f.CloseBy(1, "owner")
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug}
	s, err := a.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := daemon.DoneByReaction(thumbTrust)(context.Background(), a, s, daemon.Dirt{}); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the issue's reactions %d times, want none", n)
	}
}

// A 👍 given while the task is held waits: its reactions aren't read, and
// nothing is written for it. Once /resume returns the task to needs-human,
// it applies in the same pass.
func TestDoneByReactionWhileHeld(t *testing.T) {
	t.Parallel()
	f, o := humanTask("", func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "owner", "/hold")
		thumbBy("owner", 1003)(t, f)
	})(t), newOrigin(t)
	d := thumbDaemon(t, f, o)
	iterate(t, d)
	iterate(t, d)
	is := f.State().Issues[0]
	if !slices.Equal(is.Labels, []string{"stage:held"}) || is.Closed {
		t.Fatalf("labels %q, closed %t; want held and open", is.Labels, is.Closed)
	}
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the held issue's reactions %d times, want none", n)
	}
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	is = f.State().Issues[0]
	last := is.Comments[len(is.Comments)-1].Body
	if !is.Closed || !strings.Contains(last, "from=needs-human to=closed reason=completed cmd=r1003 ") {
		t.Errorf("closed %t, last comment %q; want it closed on the 👍 that waited", is.Closed, last)
	}
}

// A /done and a 👍 in the same pass close the task once, on the /done:
// the 👍 finds it closed, and its reactions aren't read.
func TestDoneByReactionAfterDone(t *testing.T) {
	t.Parallel()
	f := humanTask("", func(t testing.TB, f *githubtest.Fake) {
		doneBy("owner", 1002)(t, f)
		thumbBy("owner", 1003)(t, f)
	})(t)
	iterate(t, thumbDaemon(t, f, newOrigin(t)))
	doneState(t, f, []string{"stage:needs-human"}, true, []string{"/done", doneClosed(`The "Done when" has no check for me to run.`, 1002)})
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the issue's reactions %d times, want none", n)
	}
}

// A pass where nothing changed on an open human task, one whose 👍 has
// its reply, makes only conditional GETs, answered 304, its reactions'
// read among them, and writes nothing.
func TestDoneByReactionUnchanged(t *testing.T) {
	t.Parallel()
	f := humanTask(doneLabelCheck, thumbBy("owner", 1002))(t)
	d := thumbDaemon(t, f, newOrigin(t))
	iterate(t, d) // the reply
	iterate(t, d) // the pass that reads the issue written to again
	from, writes := len(f.Requests()), len(f.Writes())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a pass where nothing changed wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if n := thumbReads(f, from); n != 1 {
		t.Errorf("a pass where nothing changed read the issue's reactions %d times; want once, answered 304", n)
	}
}

// A read that fails, of the issue's reactions or for the check, fails the
// pass, with no reply and no close, and the next pass checks again.
func TestDoneByReactionReadFails(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/issues/1/reactions", "/labels/needs-design"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			f, o := humanTask(doneLabelCheck, func(t testing.TB, f *githubtest.Fake) {
				f.AddRepoLabel("needs-design")
				thumbBy("owner", 1002)(t, f)
			})(t), newOrigin(t)
			f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == path }, http.StatusBadGateway)
			d := thumbDaemon(t, f, o)
			if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "done by 👍 on o/r#1: ") {
				t.Fatalf("Iterate: %v, want the failed read's error, from the 👍 trigger", err)
			}
			if w := f.Writes(); len(w) != 0 {
				t.Fatalf("writes %+v, want none", w)
			}
			iterate(t, d)
			doneState(t, f, []string{"stage:needs-human"}, true, []string{thumbClosed("owner", "The label `needs-design` exists.", 1002)})
		})
	}
}
