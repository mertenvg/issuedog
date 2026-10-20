package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

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
