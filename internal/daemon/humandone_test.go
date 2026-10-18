package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A human task closed on /done (plan #250, T5).

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

// A move to closed as not planned, as S5 closes a dropped child, closes
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
