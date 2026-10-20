package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Which issue closes once its children have (#251): an epic planned into
// stories, at planned, with a record of its approved plan revision's
// children, read as a finished story's are (epicCloseChildren), and not
// closed so already for that plan.

// epicCloseFindOn is epicCloseFind on s, with dirty content d.
func epicCloseFindOn(t *testing.T, s *github.Snapshot, d Dirt) ([]int, state.Key, bool, error) {
	t.Helper()
	return epicCloseFind(s, waitSlug, d.Has, state.Derive(s, waitSlug, d.Has))
}

// An epic planned into stories waits on the children its record names,
// in its order, and closes keyed event=p<id>, by its comment holding the
// move into planned that approved the plan. A record of none, as after a
// revision that drops every child, waits on none.
func TestEpicCloseFind(t *testing.T) {
	t.Parallel()
	children, key, ok, err := epicCloseFindOn(t, doneSnap(t, 60, "epic", "epic-plan", 13, 12), Dirt{})
	if err != nil || !ok || !slices.Equal(children, []int{13, 12}) || key != (state.Key{Field: "event", ID: "p6003"}) {
		t.Errorf("an epic planned into stories gave %v, %+v, %t, %v; want [13 12], event=p6003", children, key, ok, err)
	}
	none := doneSnap(t, 61, "epic", "epic-plan")
	none.Comments = append(none.Comments, github.Comment{ID: 6104, Author: waitApp, Body: "None left.\n\n<!-- issuedog:created rev=1 children=none -->"})
	children, key, ok, err = epicCloseFindOn(t, none, Dirt{})
	if err != nil || !ok || children == nil || len(children) != 0 || key.ID != "p6103" {
		t.Errorf("an epic whose record names none gave %v, %+v, %t, %v; want no children, non-nil, keyed p6103", children, key, ok, err)
	}
}

// Nothing else waits so: an epic planned into tasks, which waits for
// story verification as its own story (SPEC §4), a story, a task, an
// epic with no record yet, a dirty one, no approved revision, or no plan
// data to tell what it was planned into, one held or closed, and one
// whose close for this plan is written already, as one a trusted reopen
// held and /resume brought back to planned.
func TestEpicCloseFindNothing(t *testing.T) {
	t.Parallel()
	norecord := doneSnap(t, 70, "epic", "epic-plan")
	dirty := doneSnap(t, 71, "epic", "epic-plan", 12)
	unapproved := doneSnap(t, 72, "epic", "epic-plan", 12)
	unapproved.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned cmd=r5 seen=2 -->"
	nodata := doneSnap(t, 73, "epic", "epic-plan", 12)
	nodata.Comments[1].Body = waitPlanComment(t, 1)
	nodata.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + waitPlanHash(t, 1) + " cmd=r5 seen=2 -->"
	held := doneSnap(t, 74, "epic", "epic-plan", 12)
	held.Comments = append(held.Comments, github.Comment{ID: 7405, Author: waitApp, Body: "<!-- issuedog:transition from=planned to=held at=planned reason=owner cmd=9 -->"})
	closed := doneSnap(t, 75, "epic", "epic-plan", 12)
	closed.Closed = true
	again := doneSnap(t, 76, "epic", "epic-plan", 12)
	for i, body := range []string{
		"Closed.\n\n<!-- issuedog:transition from=planned to=closed reason=completed event=p7603 -->",
		"<!-- issuedog:transition from=closed to=held at=planned reason=owner event=31 -->",
		"<!-- issuedog:transition from=held to=planned cmd=7608 -->",
	} {
		again.Comments = append(again.Comments, github.Comment{ID: int64(7605 + i), Author: waitApp, Body: body})
	}
	for name, tc := range map[string]struct {
		s *github.Snapshot
		d Dirt
	}{
		"an epic planned into tasks": {doneSnap(t, 77, "epic", "story-plan", 12), Dirt{}},
		"a story":                    {doneSnap(t, 78, "story", "epic-plan", 12), Dirt{}},
		"a task":                     {doneSnap(t, 79, "task", "epic-plan", 12), Dirt{}},
		"no record":                  {norecord, Dirt{}},
		"a dirty record":             {dirty, Dirt{7104: "mallory"}},
		"no approved revision":       {unapproved, Dirt{}},
		"no plan data":               {nodata, Dirt{}},
		"held":                       {held, Dirt{}},
		"closed":                     {closed, Dirt{}},
		"closed so before":           {again, Dirt{}},
	} {
		if children, key, ok, err := epicCloseFindOn(t, tc.s, tc.d); err != nil || ok || children != nil {
			t.Errorf("%s: got %v, %+v, %t, %v; want nothing", name, children, key, ok, err)
		}
	}
}

// An epic's plan data that doesn't read back is an error, which
// epicCloseSet logs; a finished story's item is then waitFind's error too
// (TestWaitFindDoneBadData), as the reading is one.
func TestEpicCloseFindBadData(t *testing.T) {
	t.Parallel()
	s := doneSnap(t, 80, "epic", "epic-plan", 12)
	m := github.Marker{Kind: "plan-data", Fields: map[string]string{"v": "1", "data": "not-base64!"}}
	data, err := github.FormatMarker(m)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planRevision("### Plan\n\n<!-- issuedog:transition from=planning to=plan-review rev=1 run=p1 -->\n"+data, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, ok := state.PlanHash(plan)
	if !ok {
		t.Fatal("no plan hash")
	}
	s.Comments[1].Body = plan
	s.Comments[2].Body = "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + hash + " cmd=r5 seen=2 -->"
	children, _, ok, err := epicCloseFindOn(t, s, Dirt{})
	if err == nil || ok || children != nil || !strings.Contains(err.Error(), "an epic's plan") {
		t.Errorf("got %v, %t, %v; want an error naming the epic's plan", children, ok, err)
	}
}

// epicClosePassDaemon files issue 1 on a fake, an epic at planned, as the
// App leaves it once it has created the stories of its plan, #2 and #3,
// closed, with the comments doneSnap gives it, and returns a daemon on the
// fake whose snapshot of it and the poll's listing are as of then, and
// whose cache waits on children for it.
func epicClosePassDaemon(t *testing.T, children ...int) (*githubtest.Fake, *Daemon) {
	t.Helper()
	f := githubtest.New(t, "o/r", waitSlug)
	n := f.AddIssue(waitOwner.Login, "Quotes", "The spec.")
	f.AddLabel(n, waitOwner.Login, "type:epic")
	f.AddLabel(n, waitApp.Login, "stage:planned")
	for _, c := range doneSnap(t, 1, "epic", "epic-plan", 2, 3).Comments {
		f.AddComment(n, waitApp.Login, c.Body)
	}
	for i := range 2 {
		c := f.AddIssue(waitApp.Login, fmt.Sprintf("S%d. Story", i+1), fmt.Sprintf("The story.\n\n<!-- issuedog:origin plan=#1 rev=1 item=S%d -->", i+1))
		f.SetParent(c, n)
		f.CloseBy(c, waitOwner.Login)
	}
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: waitSlug}, Trust: ownerOnly{}}
	s, err := st.Client.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{Step: st, snaps: map[int]*github.Snapshot{n: s}, inputs: map[int][]github.Comment{n: nil}, stale: map[int]bool{},
		closedListed: map[int]bool{2: true, 3: true}, epicCloseDue: map[int][]int{n: children}}
	f.ForgetWrites()
	return f, d
}

// epicClosePassClosed reports whether issue 1 of f is closed.
func epicClosePassClosed(f *githubtest.Fake) bool { return f.State().Issues[0].Closed }

// The pass closes an epic whose children the poll listed closed: it reads
// the epic fresh, closes it with one comment, and leaves it to be read
// again, with no Work on this pass and nothing left to wait on.
func TestEpicClosePass(t *testing.T) {
	t.Parallel()
	f, d := epicClosePassDaemon(t, 2, 3)
	if err := d.epicClosePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, w := range f.Writes() {
		writes = append(writes, w.Method+" "+w.Path)
	}
	if want := []string{"POST /issues/1/comments", "PATCH /issues/1"}; !slices.Equal(writes, want) || !epicClosePassClosed(f) {
		t.Errorf("writes %q, closed %t; want %q, closed", writes, epicClosePassClosed(f), want)
	}
	if _, ok := d.inputs[1]; ok || !d.stale[1] || len(d.epicCloseDue) != 0 {
		t.Errorf("inputs %v, stale %v, waiting %v; want the epic read again, with no inputs, waiting on nothing", d.inputs, d.stale, d.epicCloseDue)
	}
}

// What the fresh read shows wins over the snapshot held, as when the
// epic changed after the poll: a stage the owner set by hand meanwhile,
// which the label rules record first, or a record that names a child the
// poll didn't list closed. Either way the epic stays open, and is read
// again on the next pass.
func TestEpicClosePassFresh(t *testing.T) {
	t.Parallel()
	f, d := epicClosePassDaemon(t, 2, 3)
	f.RemoveLabel(1, waitOwner.Login, "stage:planned")
	f.AddLabel(1, waitOwner.Login, "stage:held")
	if err := d.epicClosePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cs := f.State().Issues[0].Comments; epicClosePassClosed(f) || !strings.Contains(cs[len(cs)-1].Body, "to=held") || !d.stale[1] {
		t.Errorf("closed %t, stale %t, with the newest comment %q; want it open, held by the owner's label, to be read again",
			epicClosePassClosed(f), d.stale[1], cs[len(cs)-1].Body)
	}

	f, d = epicClosePassDaemon(t, 2)
	delete(d.closedListed, 3)
	if err := d.epicClosePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.Writes()) != 0 || !d.stale[1] {
		t.Errorf("writes %+v, stale %t; want none, with #3 not listed closed, and the epic read again", f.Writes(), d.stale[1])
	}
}

// A pipeline label on the epic that nothing on the timeline explains yet,
// as while its event is late, holds the label rules back (ErrLabelsHeld):
// the epic isn't closed on that read, with no error, and is read again on
// the next pass.
func TestEpicClosePassLabelsHeld(t *testing.T) {
	t.Parallel()
	f, d := epicClosePassDaemon(t, 2, 3)
	f.LagEvents(5 * time.Second)
	f.AddLabel(1, waitOwner.Login, "priority:p1")
	if err := d.epicClosePass(context.Background()); err != nil {
		t.Fatalf("epicClosePass = %v; want no error while the label is held back", err)
	}
	if epicClosePassClosed(f) || len(f.Writes()) != 0 || !d.stale[1] {
		t.Errorf("closed %t, writes %+v, stale %t; want it open, with no write, to be read again", epicClosePassClosed(f), f.Writes(), d.stale[1])
	}
}

// An epic that left the pipeline, as when it closed, is forgotten, with
// no request.
func TestEpicClosePassLeft(t *testing.T) {
	t.Parallel()
	f, d := epicClosePassDaemon(t, 2, 3)
	delete(d.snaps, 1)
	from := len(f.Requests())
	if err := d.epicClosePass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r := f.Requests()[from:]; len(r) != 0 || len(d.epicCloseDue) != 0 {
		t.Errorf("requests %+v, waiting %v; want none, and nothing left to wait on", r, d.epicCloseDue)
	}
}

// The close's comment names the children its record names, in its order,
// or says its record names none.
func TestEpicCloseText(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		children []int
		want     string
	}{
		{[]int{3, 2}, "Every child of this epic is closed, so I've closed it as completed. Its children: #3, #2."},
		{[]int{}, "Its plan's latest revision left this epic no children, so I've closed it as completed."},
	} {
		if got := epicCloseText(tc.children); got != tc.want {
			t.Errorf("epicCloseText(%v) = %q, want %q", tc.children, got, tc.want)
		}
	}
}
