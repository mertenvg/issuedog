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

// epicCloseChecked fails t unless issue 1, the epic, is closed as
// completed, with comments its comments before it plus one, the App's,
// which says text and holds the move to closed, keyed event=p<key>.
func epicCloseChecked(t *testing.T, f *githubtest.Fake, comments int, text string, key int64) {
	t.Helper()
	if st, reason := epicCloseReason(t, f, 1); st != "closed" || reason != "completed" {
		t.Errorf("the epic is %s, as %q; want it closed as completed", st, reason)
	}
	cs := f.State().Issues[0].Comments
	if len(cs) != comments+1 {
		t.Fatalf("the epic has %d comments, %d before; want one more", len(cs), comments)
	}
	if c := cs[len(cs)-1]; c.Author != app || !strings.HasPrefix(c.Body, text+"\n\n") {
		t.Errorf("the epic's newest comment is %s's\n%q\nwant the App's, starting\n%q", c.Author, c.Body, text)
	}
	want := map[string]string{"from": "planned", "to": "closed", "reason": "completed", "event": fmt.Sprintf("p%d", key)}
	if m := changedLastMarker(t, f, 1); !maps.Equal(m.Fields, want) {
		t.Errorf("the close's marker has %v, want %v", m.Fields, want)
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
	epicCloseChecked(t, f, comments, "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
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

// A trusted reopen of the epic the App closed holds it at planned (SPEC
// §6.4), and once /resume brings it back there, it stays open, though
// every story is still closed: it closes so once for each approved plan,
// and the owner reopened it.
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
			t.Errorf("%s: the epic is %s, with %d moves to closed; want it open, with the one", step, st, closes())
		}
	}
	doneUnchanged(t, f, d)
}

// Children a revision drops (SPEC §6.4) count as its record says: they
// aren't the epic's children any more, and are closed as not planned.
// Here revision 2 drops S1's #2, keeps S2's #3 and adds S3's #9: the epic
// closes once #3 and #9 have, naming those two. A revision that drops
// every child leaves the epic none to wait on, so its first pass closes
// it, saying so.
func TestEpicCloseDropped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		revision func(testing.TB) map[string]any
		open     []int
		text     string
	}{
		{"S1 dropped", func(t testing.TB) map[string]any { return droppedEpicRevision(t) }, []int{3, 9},
			"Every child of this epic is closed, so I've closed it as completed. Its children: #3, #9."},
		{"every child dropped", func(t testing.TB) map[string]any {
			return droppedEpicRevision(t,
				map[string]any{"id": "S1", "title": "Quotes expire after 24 hours", "status": "dropped"},
				map[string]any{"id": "S2", "title": "Customers see when a quote expires", "status": "dropped"})
		}, nil, "Its plan's latest revision left this epic no children, so I've closed it as completed."},
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
			epicCloseChecked(t, f, comments, tc.text, p)
		})
	}
}

// epicCloseReplayFake is a setup for Replay: o/r with issue 1 an epic at
// planned with twoStories' plan, and its stories, #2 and #3, filed and
// linked under it, closed, with the App's record of them on issue 1, as
// comment 1005.
func epicCloseReplayFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	doneParent(t, f, "epic", twoStories)
	for i, w := range wantChildren(twoStories) {
		n := f.AddIssue(app, w.title, fmt.Sprintf("A story.\n\n<!-- issuedog:origin plan=#1 rev=1 item=S%d -->", i+1), "type:story")
		f.SetParent(n, 1)
		f.CloseBy(n, "owner")
	}
	post(t, f, 1005, app, wantRecord(twoStories))
	f.ForgetWrites()
	return f
}

// epicCloseReplayPass is a restart's first two passes, a fresh daemon of
// o's on the fake: the close, then the close judge's stage:completed.
func epicCloseReplayPass(t *testing.T, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := newDaemon(t, f, o, nil)
		return errors.Join(d.Iterate(t.Context()), d.Iterate(t.Context()))
	}
}

// Replay passes for the close of an epic whose stories have all closed,
// whatever write a crash comes at: one comment, one close as completed,
// and stage:completed.
func TestEpicCloseReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, epicCloseReplayFake, epicCloseReplayPass(t, o))
	clean := epicCloseReplayFake(t)
	if err := epicCloseReplayPass(t, o)(clean); err != nil {
		t.Fatal(err)
	}
	if want := []string{"POST /issues/1/comments", "PATCH /issues/1", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:planned"}; !slices.Equal(epicCloseWritesTo(clean, 0, 1), want) {
		t.Errorf("the clean pass's writes to the epic = %q; want %q", epicCloseWritesTo(clean, 0, 1), want)
	}
	epicCloseChecked(t, clean, 4, "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
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
// stays open; the owner's close of #3 then closes it. A close that's
// never judged, forgotten as gone, as when the story is transferred
// before its event shows, or its timeline answers 404 once, keeps the
// epic open for good.
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
		}, true},
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
			epicCloseChecked(t, f, comments, "Every child of this epic is closed, so I've closed it as completed. Its children: #2, #3.", 1004)
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
