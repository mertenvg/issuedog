package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// An epic planned into stories closes once every child is closed (#251;
// SPEC §7.2, §7.3), against the fakes, with step 1 wired as main wires
// it: the daemon creates the stories of a plan the owner approved, and
// closes the epic, as completed, with one comment, once they've all
// closed. An epic planned into tasks doesn't close so, as it waits for
// story verification as its own story (SPEC §4).

// epicCloseWritesTo is f's writes from the from'th on to issue n, as
// "METHOD /path".
func epicCloseWritesTo(f *githubtest.Fake, from, n int) []string {
	return slices.DeleteFunc(digestWrites(f, from), func(w string) bool {
		return !strings.HasSuffix(w, fmt.Sprintf(" /issues/%d", n)) && !strings.Contains(w, fmt.Sprintf(" /issues/%d/", n))
	})
}

// epicCloseStep runs one Iterate of d, which must succeed, and returns its
// writes to issue 1, the epic.
func epicCloseStep(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) []string {
	t.Helper()
	from := len(f.Writes())
	iterate(t, d)
	return epicCloseWritesTo(f, from, 1)
}

// epicCloseReason is issue n's state and state_reason, as GitHub gives
// them.
func epicCloseReason(t *testing.T, f *githubtest.Fake, n int) (string, string) {
	t.Helper()
	is, _, err := f.Client(t).Issues.Get(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	return is.GetState(), is.GetStateReason()
}

// epicCloseChecked fails t unless issue 1, the epic, is closed with state
// reason want, with comments its comments before it plus one, the App's,
// which says text and holds the move to closed, keyed event=p<key>.
func epicCloseChecked(t *testing.T, f *githubtest.Fake, comments int, want, text string, key int64) {
	t.Helper()
	if st, reason := epicCloseReason(t, f, 1); st != "closed" || reason != want {
		t.Errorf("the epic is %s, as %q; want it closed as %s", st, reason, want)
	}
	cs := f.State().Issues[0].Comments
	if len(cs) != comments+1 {
		t.Fatalf("the epic has %d comments, %d before; want one more", len(cs), comments)
	}
	if c := cs[len(cs)-1]; c.Author != app || !strings.HasPrefix(c.Body, text+"\n\n") {
		t.Errorf("the epic's newest comment is %s's\n%q\nwant the App's, starting\n%q", c.Author, c.Body, text)
	}
	fields := map[string]string{"from": "planned", "to": "closed", "reason": want, "event": fmt.Sprintf("p%d", key)}
	if m := changedLastMarker(t, f, 1); !maps.Equal(m.Fields, fields) {
		t.Errorf("the close's marker has %v, want %v", m.Fields, fields)
	}
}

// The whole of it. The first pass creates the epic's stories, #2 and #3,
// and while any is open, the epic gets no write, and a pass where nothing
// changed costs only 304s. An open issue added below the epic by hand,
// which its record doesn't name, keeps it open too, after the last story
// closes. Once that closes, the pass closes the epic as completed, with
// one comment naming the stories its record names, keyed by the move
// into planned that approved its plan, comment 1004, and the pass after
// sets stage:completed. Then a pass where nothing changed costs only 304s,
// and a restart writes nothing.
func TestEpicClose(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	doneParent(t, f, "epic", twoStories)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	if got := f.State().Issues[0].SubIssues; !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("the epic's sub-issues are %v; want its stories, 2 and 3", got)
	}
	iterate(t, d)
	doneUnchanged(t, f, d)

	f.CloseBy(2, "owner")
	for _, step := range []string{"one story closed", "the pass after"} {
		if w := epicCloseStep(t, f, d); len(w) != 0 {
			t.Errorf("%s: the epic got %q; want nothing, with #3 open", step, w)
		}
	}
	extra := f.AddIssue("owner", "One more thing", "Added by hand.")
	f.SetParent(extra, 1)
	f.CloseBy(3, "owner")
	for _, step := range []string{"the last story closed", "the pass after"} {
		if w := epicCloseStep(t, f, d); len(w) != 0 {
			t.Errorf("%s: the epic got %q; want nothing, with #%d open below it", step, w, extra)
		}
	}
	if st, _ := epicCloseReason(t, f, 1); st != "open" {
		t.Errorf("with #%d open below it, the epic is %s; want it open", extra, st)
	}

	comments := len(f.State().Issues[0].Comments)
	f.CloseBy(extra, "owner")
	w := epicCloseStep(t, f, d)
	if want := []string{"POST /issues/1/comments", "PATCH /issues/1"}; !slices.Equal(w, want) {
		t.Errorf("the pass after the last issue below it closed wrote %q to the epic; want %q", w, want)
	}
	epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
	epicCloseStep(t, f, d)
	if got := f.State().Issues[0].Labels; !slices.Contains(got, "stage:completed") || slices.Contains(got, "stage:planned") {
		t.Errorf("the closed epic's labels are %q; want stage:completed in place of stage:planned", got)
	}
	if w := epicCloseStep(t, f, d); len(w) != 0 { // it lists the judged close
		t.Errorf("the pass after stage:completed wrote %q to the epic; want nothing", w)
	}
	doneUnchanged(t, f, d)
	if w := digestPass(t, f, newDaemon(t, f, o, nil)); len(w) != 0 {
		t.Errorf("a restart wrote %q; want nothing", w)
	}
	if got := len(f.State().Issues[0].Comments); got != comments+1 {
		t.Errorf("the epic has %d comments; want %d, one close", got, comments+1)
	}
}

// An epic planned into tasks acts as its own story (SPEC §4), and waits
// for story verification, so once its tasks have all closed it stays
// open, with no write: the digest lists it instead (TestDoneStoryEpic).
func TestEpicCloseIntoTasks(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	doneParent(t, f, "epic", childPlan{"Epic", "story-plan", 3, false})
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	if got := f.State().Issues[0].SubIssues; !slices.Equal(got, []int{2, 3, 4}) {
		t.Fatalf("the epic's sub-issues are %v; want its tasks, 2 to 4", got)
	}
	iterate(t, d)
	for _, c := range []int{2, 3, 4} {
		f.CloseBy(c, "owner")
	}
	for _, step := range []string{"every task closed", "the pass after", "the pass after that"} {
		if w := epicCloseStep(t, f, d); len(w) != 0 {
			t.Errorf("%s: the epic got %q; want nothing", step, w)
		}
	}
	if st, _ := epicCloseReason(t, f, 1); st != "open" {
		t.Errorf("the epic planned into tasks is %s; want it open, waiting for story verification", st)
	}
}

// epicCloseResumed is the ID of the epic's newest comment holding a move
// from held back to planned, as /resume makes it, which keys its close
// after that (SPEC §7.3).
func epicCloseResumed(t testing.TB, f *githubtest.Fake) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	for _, c := range s.Comments {
		if c.Author.Login == app && strings.Contains(c.Body, "from=held to=planned ") {
			id = c.ID
		}
	}
	if id == 0 {
		t.Fatal("the epic has no move from held back to planned")
	}
	return id
}

// A trusted reopen of the epic the App closed holds it at planned (SPEC
// §6.4), and /resume brings it back there, a new move into planned,
// which keys a close of its own (SPEC §7.3). Here the owner reopened it
// to add a story by hand, #4, below it, so it stays open while #4 is,
// though every story its record names is still closed. Once #4 closes,
// the pass closes the epic again, keyed by the /resume's move, and a
// pass where nothing changed then costs only 304s.
func TestEpicCloseReopen(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), newOrigin(t)
	doneParent(t, f, "epic", twoStories)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	iterate(t, d)
	f.CloseBy(2, "owner")
	f.CloseBy(3, "owner")
	iterate(t, d)
	iterate(t, d)
	if st, _ := epicCloseReason(t, f, 1); st != "closed" {
		t.Fatalf("with both stories closed, the epic is %s; want it closed", st)
	}
	f.ReopenBy(1, "owner")
	iterate(t, d)
	iterate(t, d)
	if got := f.State().Issues[0].Labels; !slices.Contains(got, "stage:held") {
		t.Fatalf("after the owner's reopen, the epic's labels are %q; want it held", got)
	}
	extra := f.AddIssue("owner", "One more story", "Added by hand.")
	f.SetParent(extra, 1)
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	if got := f.State().Issues[0].Labels; !slices.Contains(got, "stage:planned") {
		t.Fatalf("after /resume, the epic's labels are %q; want it at planned", got)
	}
	closes := func() int {
		n := 0
		for _, c := range f.State().Issues[0].Comments {
			for _, m := range github.ParseMarkers(c.Body) {
				if m.Kind == "transition" && m.Fields["to"] == "closed" {
					n++
				}
			}
		}
		return n
	}
	for _, step := range []string{"the pass after /resume", "the pass after that"} {
		iterate(t, d)
		if st, _ := epicCloseReason(t, f, 1); st != "open" || closes() != 1 {
			t.Errorf("%s: the epic is %s, with %d moves to closed; want it open, with the one, with #%d open below it", step, st, closes(), extra)
		}
	}
	doneUnchanged(t, f, d)

	comments := len(f.State().Issues[0].Comments)
	f.CloseBy(extra, "owner")
	iterate(t, d)
	epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", epicCloseResumed(t, f))
	if closes() != 2 {
		t.Errorf("the epic has %d moves to closed; want 2, one for each move into planned", closes())
	}
	iterate(t, d) // stage:completed
	iterate(t, d) // it lists the judged close
	doneUnchanged(t, f, d)
}

// Children a revision drops (SPEC §6.4) count as its record says: they
// aren't the epic's children any more, and are closed as not planned.
// Here revision 2 drops S1's #2, keeps S2's #3 and adds S3's #9: the epic
// closes once #3 and #9 have, as completed, naming those two. A
// revision that drops every child leaves the epic none to wait on, so its
// first pass closes it, as not planned, as no child closed as completed,
// saying so.
func TestEpicCloseDropped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		revision func(testing.TB) map[string]any
		open     []int
		reason   string
		text     string
	}{
		{"S1 dropped", func(t testing.TB) map[string]any { return droppedEpicRevision(t) }, []int{3, 9}, "completed",
			"Every child of this epic is closed, so I've closed it as completed. Its children: #3, #9."},
		{"every child dropped", func(t testing.TB) map[string]any {
			return droppedEpicRevision(t,
				map[string]any{"id": "S1", "title": "Quotes expire after 24 hours", "status": "dropped"},
				map[string]any{"id": "S2", "title": "Customers see when a quote expires", "status": "dropped"})
		}, nil, "not_planned", "Its plan's latest revision left this epic no children, so I've closed it as not planned."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := run(t, droppedEpic(tc.revision), childrenPass(t, twoStories))
			p, _ := changedApproval(t, f)
			if !f.State().Issues[1].Closed {
				t.Fatal("S1's #2 is open; want the revision to have closed it")
			}
			comments := len(f.State().Issues[0].Comments)
			d := newDaemon(t, f, newOrigin(t), nil)
			iterate(t, d)
			iterate(t, d)
			for _, n := range tc.open {
				if st, _ := epicCloseReason(t, f, 1); st != "open" {
					t.Fatalf("with #%d open, the epic is %s; want it open", n, st)
				}
				f.CloseBy(n, "owner")
				iterate(t, d)
			}
			epicCloseChecked(t, f, comments, tc.reason, tc.text, p)
		})
	}
}

// epicCloseReplayFake is a setup for Replay: o/r with issue 1 an epic at
// planned with twoStories' plan, and its stories, #2 and #3, filed and
// linked under it, closed, each with stage:completed, as the close judge
// leaves an accepted close, with the App's record of them on issue 1, as
// comment 1005.
func epicCloseReplayFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	return epicCloseReplayFakeAs(t, "")
}

// epicCloseReplayFakeAs is epicCloseReplayFake with the stories closed as
// the owner closed them with state reason reason, "" for none, each with
// the closed stage the close judge leaves for that (state.ClosingStage).
func epicCloseReplayFakeAs(t testing.TB, reason string) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	doneParent(t, f, "epic", twoStories)
	stage, _ := state.ClosingStage(reason, false).Label()
	for i, w := range wantChildren(twoStories) {
		n := f.AddIssue(app, w.title, fmt.Sprintf("A story.\n\n<!-- issuedog:origin plan=#1 rev=1 item=S%d -->", i+1), "type:story")
		f.SetParent(n, 1)
		f.CloseAs(n, "owner", reason)
		f.AddLabel(n, app, stage)
	}
	post(t, f, 1005, app, wantRecord(twoStories))
	f.ForgetWrites()
	return f
}

// epicCloseReplayNotPlanned is a setup for Replay: epicCloseReplayFake,
// with the owner's closes of both stories as not planned, each at
// stage:discarded.
func epicCloseReplayNotPlanned(t testing.TB) *githubtest.Fake {
	t.Helper()
	return epicCloseReplayFakeAs(t, "not_planned")
}

// epicCloseReplayReopened returns a setup for Replay: epicCloseReplayFake,
// whose epic a daemon of o's closed, then the owner reopened, which held
// it, added #4 below it by hand and closed it, and brought it back to
// planned with /resume, with the writes forgotten. So a restart closes it
// again, keyed by the /resume's move (SPEC §7.3).
func epicCloseReplayReopened(o *origin) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := epicCloseReplayFake(t)
		d := newDaemon(t, f, o, nil)
		passes := func(n int) {
			t.Helper()
			for range n {
				if err := d.Iterate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		}
		passes(2) // the close, then stage:completed
		f.ReopenBy(1, "owner")
		passes(2)
		extra := f.AddIssue("owner", "One more story", "Added by hand.")
		f.SetParent(extra, 1)
		f.AddComment(1, "owner", "/resume")
		passes(1)
		f.CloseBy(extra, "owner")
		if is := f.State().Issues[0]; is.Closed || !slices.Contains(is.Labels, "stage:planned") {
			t.Fatalf("the epic is closed %t, labelled %q; want it open at planned after /resume", is.Closed, is.Labels)
		}
		f.ForgetWrites()
		return f
	}
}

// epicCloseReplayPass is a restart's first two passes, a fresh daemon of
// o's on the fake: the close, then the close judge's closed stage.
func epicCloseReplayPass(t *testing.T, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := newDaemon(t, f, o, nil)
		return errors.Join(d.Iterate(t.Context()), d.Iterate(t.Context()))
	}
}

// Replay passes for the close of an epic whose stories have all closed,
// whatever write a crash comes at: one comment, one close, and the closed
// stage it names. It's as completed, and stage:completed, keyed by the
// move into planned that approved the plan, comment 1004; as not planned,
// and stage:discarded, when the owner closed every story as not planned;
// and keyed by the /resume's move, after a reopen.
func TestEpicCloseReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	done := "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3."
	for _, tc := range []struct {
		name          string
		setup         func(testing.TB) *githubtest.Fake
		reason, stage string
		text          string
		key           func(testing.TB, *githubtest.Fake) int64
	}{
		{"its stories closed", epicCloseReplayFake, "completed", "stage:completed", done, func(testing.TB, *githubtest.Fake) int64 { return 1004 }},
		{"its stories closed as not planned", epicCloseReplayNotPlanned, "not_planned", "stage:discarded",
			"Every child of this epic is closed, none as completed, so I've closed it as not planned. Its children: #2, #3.",
			func(testing.TB, *githubtest.Fake) int64 { return 1004 }},
		{"after a reopen and /resume", epicCloseReplayReopened(o), "completed", "stage:completed", done,
			epicCloseResumed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			githubtest.Replay(t, tc.setup, epicCloseReplayPass(t, o))
			clean := tc.setup(t)
			comments, key := len(clean.State().Issues[0].Comments), tc.key(t, clean)
			if err := epicCloseReplayPass(t, o)(clean); err != nil {
				t.Fatal(err)
			}
			if want := []string{"POST /issues/1/comments", "PATCH /issues/1", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planned"}; !slices.Equal(epicCloseWritesTo(clean, 0, 1), want) {
				t.Errorf("the clean pass's writes to the epic = %q; want %q", epicCloseWritesTo(clean, 0, 1), want)
			}
			epicCloseChecked(t, clean, comments, tc.reason, tc.text, key)
			if got := clean.State().Issues[0].Labels; !slices.Contains(got, tc.stage) || slices.Contains(got, "stage:planned") {
				t.Errorf("the closed epic's labels are %q; want %s in place of stage:planned", got, tc.stage)
			}
		})
	}
}

// epicCloseUntrustedFake files the epic planned into twoStories and runs
// a daemon on it till its stories, #2 and #3, are filed and read again,
// then closes #2 as the owner, and runs a pass, which judges that close.
func epicCloseUntrustedFake(t *testing.T) (*githubtest.Fake, *daemon.Daemon) {
	t.Helper()
	f, o := newFake(t), newOrigin(t)
	doneParent(t, f, "epic", twoStories)
	d := newDaemon(t, f, o, nil)
	iterate(t, d)
	iterate(t, d)
	f.CloseBy(2, "owner")
	iterate(t, d)
	return f, d
}

// epicCloseOpen fails t unless the epic, issue 1, is open and got no
// write from the from'th on.
func epicCloseOpen(t *testing.T, f *githubtest.Fake, from int, step string) {
	t.Helper()
	if st, _ := epicCloseReason(t, f, 1); st != "open" {
		t.Errorf("%s: the epic is %s; want it open", step, st)
	}
	if w := epicCloseWritesTo(f, from, 1); len(w) != 0 {
		t.Errorf("%s: the epic got %q; want nothing", step, w)
	}
}

// epicCloseIterate runs one Iterate of d and fails t unless it fails just
// when wantErr says.
func epicCloseIterate(t *testing.T, d *daemon.Daemon, wantErr bool, step string) {
	t.Helper()
	if err := d.Iterate(context.Background()); (err != nil) != wantErr {
		t.Fatalf("%s: Iterate = %v; want an error %t", step, err, wantErr)
	}
}

// An untrusted close of the last story never closes the epic (SPEC
// §6.4): mallory, a write collaborator, closes #3 after the owner closed
// #2, and the pass doesn't judge her close, so the epic stays open: while
// GitHub hasn't written the close's event, while the question of how it
// was made fails, or when it's judged and reverted, but the story's read
// after its reopen fails. Once it's judged, it's reverted, and the epic
// stays open; the owner's close of #3 then closes it. So it is when the
// story's timeline answers 404 once: one answer may be wrong, so the next
// pass judges her close. A close that's never judged, forgotten as gone,
// as when the story is transferred before its event shows, which two
// judgings on later passes find, keeps the epic open for good.
func TestEpicCloseUntrusted(t *testing.T) {
	t.Parallel()
	lagged := func(f *githubtest.Fake) {
		f.LagEvents(5 * time.Second)
		f.CloseBy(3, "mallory")
		f.LagEvents(0)
	}
	for _, tc := range []struct {
		name string
		// close closes #3 as mallory, with the fault, and returns how many
		// passes fail on it, and mend, which ends the fault, if any.
		close func(f *githubtest.Fake) (fails int, mend func())
		// forgotten is set when the close is forgotten as gone, and never
		// judged: the epic then stays open.
		forgotten bool
	}{
		{"its event late", func(f *githubtest.Fake) (int, func()) {
			lagged(f)
			return 0, func() { f.Advance(10 * time.Second) }
		}, false},
		{"its closer unknown", func(f *githubtest.Fake) (int, func()) {
			event := f.CloseBy(3, "mallory")
			f.FailCloser(3, event, "", closingFailure)
			return 2, func() { f.MendCloser(3, event) }
		}, false},
		{"its re-read failing", func(f *githubtest.Fake) (int, func()) {
			f.CloseBy(3, "mallory")
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPatch && r.Path == "/issues/3" }, func() {
				f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/3" }, http.StatusInternalServerError)
			})
			return 1, nil
		}, false},
		{"its story transferred", func(f *githubtest.Fake) (int, func()) {
			lagged(f)
			return 0, func() {
				f.Transfer(3, "o/other")
				f.Advance(10 * time.Second)
			}
		}, true},
		{"its timeline answering 404 once", func(f *githubtest.Fake) (int, func()) {
			lagged(f)
			return 0, func() {
				f.Advance(10 * time.Second)
				f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/3/timeline" }, http.StatusNotFound)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, d := epicCloseUntrustedFake(t)
			from := len(f.Writes())
			fails, mend := tc.close(f)
			epicCloseIterate(t, d, fails > 0, "the pass after mallory's close")
			epicCloseOpen(t, f, from, "the pass after mallory's close")
			for pass := 1; pass < fails; pass++ {
				epicCloseIterate(t, d, true, "a pass while the fault lasts")
			}
			if mend != nil {
				mend()
			}
			for _, step := range []string{"the pass that judges her close", "the pass after", "the pass after that"} {
				epicCloseIterate(t, d, false, step)
				epicCloseOpen(t, f, from, step)
			}
			if tc.forgotten {
				return
			}
			if is := f.State().Issues[2]; is.Closed {
				t.Fatalf("#3 is closed, labelled %q; want mallory's close reverted", is.Labels)
			}
			comments := len(f.State().Issues[0].Comments)
			f.CloseBy(3, "owner")
			iterate(t, d)
			epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
		})
	}
}

// An epic whose step 1 fails, in the pass its last story closes, isn't
// closed by that pass: here on mallory's comment on it, whose permission
// read fails. The next pass's step 1 holds it for her comment (SPEC
// §6.4), so it stays open.
func TestEpicCloseVisitFails(t *testing.T) {
	t.Parallel()
	f, d := epicCloseUntrustedFake(t)
	from := len(f.Writes())
	f.CloseBy(3, "owner")
	f.AddComment(1, "mallory", "Wait, S2 isn't done.")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/collaborators/mallory/permission" }, http.StatusInternalServerError)
	epicCloseIterate(t, d, true, "the pass whose visit to the epic fails")
	if st, _ := epicCloseReason(t, f, 1); st != "open" {
		t.Fatalf("the pass whose visit to the epic fails left it %s; want it open", st)
	}
	for _, step := range []string{"the pass after", "the pass after that"} {
		epicCloseIterate(t, d, false, step)
		if st, _ := epicCloseReason(t, f, 1); st != "open" {
			t.Errorf("%s: the epic is %s; want it open, held", step, st)
		}
	}
	if got := f.State().Issues[0].Labels; !slices.Contains(got, "stage:held") {
		t.Errorf("the epic's labels are %q; want it held for mallory's comment", got)
	}
	if w := epicCloseWritesTo(f, from, 1); slices.Contains(w, "PATCH /issues/1") {
		t.Errorf("the epic got %q; want no close", w)
	}
}

// A finished story (#278) shares the judgement the epic's close takes
// (digestDoneStands), so an untrusted close of its last child, still to
// judge, doesn't list it in the digest or assign notify either: here
// mallory's close of #4, whose event is late. Once it's judged it's
// reverted, and the story still isn't listed; the owner's close of #4
// lists it and assigns notify.
func TestEpicCloseDigestUntrusted(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	doneParent(t, f, "story", storyTasks)
	d := doneStoryDaemon(t, f, o)
	doneStep(t, f, d, "the pass that creates the children")
	digestN := doneDigest(t, f)
	doneStep(t, f, d, "the pass that reads the issues written to again")
	f.CloseBy(2, "owner")
	f.CloseBy(3, "owner")
	doneStep(t, f, d, "two children closed")
	f.LagEvents(5 * time.Second)
	f.CloseBy(4, "mallory")
	f.LagEvents(0)
	doneStep(t, f, d, "mallory's close, its event late")
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("with mallory's close of #4 to judge, the digest lists %q; want no finished story", got)
	}
	f.Advance(10 * time.Second)
	for _, step := range []string{"the pass that judges her close", "the pass after"} {
		doneStep(t, f, d, step)
		if got := doneListed(f, digestN); len(got) != 0 {
			t.Errorf("%s: the digest lists %q; want no finished story", step, got)
		}
	}
	if f.State().Issues[3].Closed {
		t.Fatal("#4 is closed; want mallory's close reverted")
	}
	f.CloseBy(4, "owner")
	doneStep(t, f, d, "the owner's close of #4", assignAdd(1, "owner"))
	if got, want := doneListed(f, digestN), []string{"#1 Finished story p2 1"}; !slices.Equal(got, want) {
		t.Errorf("after the owner's close of #4, the digest lists %q; want %q", got, want)
	}
}

// epicCloseUnlabelled takes each stage label off issues ns as login, then
// closes each as login, as between the pass that files them and the next
// poll, so no visit has read them yet.
func epicCloseUnlabelled(f *githubtest.Fake, login string, ns ...int) {
	for _, n := range ns {
		for _, l := range slices.Clone(f.State().Issues[n-1].Labels) {
			if strings.HasPrefix(l, "stage:") {
				f.RemoveLabel(n, login, l)
			}
		}
		f.CloseBy(n, login)
	}
}

// A close of a story with no stage label, which no visit read with one,
// isn't judged (SPEC §16), so it never closes the epic (SPEC §7.3): here
// mallory, a write collaborator, takes the stage label off each story the
// first pass filed, #2 and #3, before the next poll, and closes it. The
// epic stays open, with no write. The owner's close made so keeps it open
// too, till the owner adds stage:completed to each story: that has the
// close judge judge its close, and the pass closes the epic. On
// mallory's, the close judge reopens each story instead, and the epic
// stays open.
func TestEpicCloseUnlabelled(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"mallory", "owner"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), newOrigin(t)
			doneParent(t, f, "epic", twoStories)
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			if got := f.State().Issues[0].SubIssues; !slices.Equal(got, []int{2, 3}) {
				t.Fatalf("the epic's sub-issues are %v; want its stories, 2 and 3", got)
			}
			from := len(f.Writes())
			epicCloseUnlabelled(f, by, 2, 3)
			for _, step := range []string{"the pass after the stories closed", "the pass after", "the pass after that"} {
				epicCloseIterate(t, d, false, step)
				epicCloseOpen(t, f, from, step)
			}
			comments := len(f.State().Issues[0].Comments)
			f.AddLabel(2, "owner", "stage:completed")
			f.AddLabel(3, "owner", "stage:completed")
			iterate(t, d)
			if by == "owner" {
				epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
				return
			}
			epicCloseOpen(t, f, from, "the pass after the owner's stage:completed")
			if f.State().Issues[1].Closed || f.State().Issues[2].Closed {
				t.Errorf("#2 closed %t, #3 closed %t; want mallory's closes reverted", f.State().Issues[1].Closed, f.State().Issues[2].Closed)
			}
		})
	}
}

// A finished story's item (#278) takes the same judgement
// (digestDoneStands): mallory's closes of a story's children, #2 to #4,
// with their stage labels taken off before the next poll, neither list
// the story in the digest nor assign notify.
func TestEpicCloseDigestUnlabelled(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	doneParent(t, f, "story", storyTasks)
	d := doneStoryDaemon(t, f, o)
	doneStep(t, f, d, "the pass that creates the children")
	digestN := doneDigest(t, f)
	epicCloseUnlabelled(f, "mallory", 2, 3, 4)
	for _, step := range []string{"the pass after mallory's closes", "the pass after", "the pass after that"} {
		doneStep(t, f, d, step)
		if got := doneListed(f, digestN); len(got) != 0 {
			t.Errorf("%s: the digest lists %q; want no finished story", step, got)
		}
	}
}

// A story closed again after its close was judged and accepted is judged
// again before it counts (SPEC §6.4): here the owner's close of #3 is
// judged, then, before the next poll, mallory reopens #3 and closes it
// again, and the owner closes #2, the last story open. The pass that
// lists them doesn't close the epic: while GitHub hasn't written
// mallory's close's event, nor when it's judged and reverted, but the
// story's read after its reopen fails.
func TestEpicCloseClosedAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// close closes #3 again as mallory, with the fault, and reports
		// whether the pass fails on it.
		close func(f *githubtest.Fake) bool
	}{
		{"its event late", func(f *githubtest.Fake) bool {
			f.LagEvents(5 * time.Second)
			f.CloseBy(3, "mallory")
			f.LagEvents(0)
			return false
		}},
		{"its re-read failing", func(f *githubtest.Fake) bool {
			f.CloseBy(3, "mallory")
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPatch && r.Path == "/issues/3" }, func() {
				f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/3" }, http.StatusInternalServerError)
			})
			return true
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), newOrigin(t)
			doneParent(t, f, "epic", twoStories)
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			iterate(t, d)
			f.CloseBy(3, "owner")
			iterate(t, d)
			if got := f.State().Issues[2].Labels; !slices.Contains(got, "stage:completed") {
				t.Fatalf("#3's labels are %q; want the owner's close judged, at stage:completed", got)
			}
			from := len(f.Writes())
			f.ReopenBy(3, "mallory")
			fails := tc.close(f)
			f.CloseBy(2, "owner")
			epicCloseIterate(t, d, fails, "the pass after mallory's close")
			epicCloseOpen(t, f, from, "the pass after mallory's close")
		})
	}
}

// An open issue the owner added below the epic by hand holds it open
// (SPEC §7.3), and so does one whose link below it someone untrusted
// removed: here mallory, a write collaborator, takes #4 from below the
// epic once both stories closed. The epic stays open, with no write, and
// a pass where nothing changed then costs only 304s; once the owner
// closes #4, the pass closes the epic. The owner's own unlink of #4 lets
// it go, so the pass after it closes the epic.
func TestEpicCloseUnlinked(t *testing.T) {
	t.Parallel()
	for _, by := range []string{"mallory", "owner"} {
		t.Run(by, func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), newOrigin(t)
			doneParent(t, f, "epic", twoStories)
			d := newDaemon(t, f, o, nil)
			iterate(t, d)
			extra := f.AddIssue("owner", "One more thing", "Added by hand.")
			f.SetParent(extra, 1)
			iterate(t, d)
			f.CloseBy(2, "owner")
			f.CloseBy(3, "owner")
			from := len(f.Writes())
			for _, step := range []string{"both stories closed", "the pass after"} {
				epicCloseIterate(t, d, false, step)
				epicCloseOpen(t, f, from, step)
			}
			comments := len(f.State().Issues[0].Comments)
			f.UnlinkBy(extra, by)
			if by == "owner" {
				iterate(t, d)
				epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
				return
			}
			for _, step := range []string{"the pass after mallory's unlink", "the pass after", "the pass after that"} {
				epicCloseIterate(t, d, false, step)
				epicCloseOpen(t, f, from, step)
			}
			doneUnchanged(t, f, d)
			f.CloseBy(extra, "owner")
			iterate(t, d)
			epicCloseChecked(t, f, comments, "completed", "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
		})
	}
}
