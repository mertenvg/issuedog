package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// recordWork makes d's Work add the issues it gets, each pass, to the result.
func recordWork(d *daemon.Daemon) *[][]int {
	var passes [][]int
	d.Work = func(_ context.Context, ready []daemon.Ready) error {
		ns := []int{}
		for _, r := range ready {
			ns = append(ns, r.S.Number)
		}
		passes = append(passes, ns)
		return nil
	}
	return &passes
}

// readsOf lists the requests after from whose path names issue n.
func readsOf(f *githubtest.Fake, from, n int) []githubtest.Request {
	prefix := fmt.Sprintf("/issues/%d", n)
	return slices.DeleteFunc(f.Requests()[from:], func(r githubtest.Request) bool {
		return r.Path != prefix && !strings.HasPrefix(r.Path, prefix+"/")
	})
}

func TestIterateReadsChangedHeldIssues(t *testing.T) {
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.AddLabel(1, "mallory", "priority:p0")
	f.EditBody(1, "eve", "do it now") // no write access, so no discussion hold
	iterate(t, d)
	is := f.State().Issues[0]
	if !reflect.DeepEqual(is.Labels, []string{"priority:p2", "stage:ready"}) {
		t.Errorf("labels = %v, want mallory's priority:p0 reverted", is.Labels)
	}
	if !strings.HasPrefix(is.Body, "<!-- issuedog:dirty by=eve -->") {
		t.Errorf("body = %q, want the note on eve's edit", is.Body)
	}
}

func TestIterateDropsClosedIssues(t *testing.T) {
	f := quiet(t)
	f.AddComment(1, "owner", "/hold")
	var visits []int
	d := newDaemon(t, f, newOrigin(t), &visits)
	d.Step.Commands["hold"] = daemon.Handler{Apply: func(context.Context, *daemon.Applier, daemon.Cmd) error {
		return errors.New("boom")
	}}
	work := recordWork(d)
	if err := d.Iterate(context.Background()); err == nil {
		t.Fatal("Iterate = nil, want issue 1's step 1 error")
	}
	f.CloseBy(1, "owner") // its step 1 failed, so it's stale
	f.CloseBy(2, "owner") // at spec-review, so it's visited
	visits = nil
	from := len(f.Requests())
	iterate(t, d)
	if len(visits) != 0 || !reflect.DeepEqual(*work, [][]int{{2}, {}}) {
		t.Errorf("after the closes, step 1 ran on %v and Work got %v; want neither issue", visits, (*work)[1])
	}
	// The owner's close stands: it's judged from the timeline alone, and
	// the issue isn't read again (T6).
	for _, r := range readsOf(f, from, 1) {
		if r.Method == "GET" && r.Path != "/issues/1/timeline" {
			t.Errorf("read closed issue 1 again: %+v", r)
		}
	}
	from = len(f.Requests())
	iterate(t, d)
	if r := readsOf(f, from, 1); len(r) != 0 {
		t.Errorf("read closed issue 1 once its close was judged: %+v", r)
	}
}

func TestIterateIgnoresCommentsOnUntrackedIssues(t *testing.T) {
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	f.AddComment(3, "owner", "on the pull request")
	f.AddComment(4, "owner", "on the closed issue")
	from := len(f.Requests())
	iterate(t, d)
	if r := append(readsOf(f, from, 3), readsOf(f, from, 4)...); len(r) != 0 {
		t.Errorf("read the pull request or the closed issue: %+v", r)
	}
}

func TestIterateSkipsIssuesWhoseReadOrVisitFails(t *testing.T) {
	f := quiet(t)
	var visits []int
	d := newDaemon(t, f, newOrigin(t), &visits)
	work := recordWork(d)
	iterate(t, d)

	// Issue 2's read fails: its old snapshot isn't visited or given to Work.
	f.AddComment(2, "owner", "a comment")
	f.FailGraphQL("boom")
	visits = nil
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Iterate = %v, want issue 2's read error", err)
	}
	if len(visits) != 0 || !reflect.DeepEqual((*work)[1], []int{1}) {
		t.Errorf("with issue 2's read failing, step 1 ran on %v and Work got %v; want neither on issue 2", visits, (*work)[1])
	}
	iterate(t, d)

	// Issue 2's visit fails, without a read: it isn't given to Work.
	d.Step.Reactions = func(context.Context, *daemon.Applier, *github.Snapshot, daemon.Dirt) error { return errors.New("bang") }
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "bang") {
		t.Errorf("Iterate = %v, want issue 2's visit error", err)
	}
	if got := *work; !reflect.DeepEqual(got[2:], [][]int{{1, 2}, {1}}) {
		t.Errorf("Work got %v, then %v; want [1 2] once issue 2 is read, then [1] when its visit fails", got[2], got[3])
	}
}

func TestIterateDropsDeletedIssues(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "a")
	fileAt(f, "spec-review", "b")
	d := newDaemon(t, f, newOrigin(t), nil)
	work := recordWork(d)
	iterate(t, d)
	f.DeleteIssue()
	if err := d.Iterate(context.Background()); err == nil {
		t.Error("Iterate = nil, want the visit to deleted issue 2 to fail")
	}
	from := len(f.Requests())
	iterate(t, d) // its read gets a 404, so it's read again
	if r := readsOf(f, from, 2); len(r) != 1 || r[0].Status != 404 {
		t.Fatalf("reads of deleted issue 2 = %+v, want one 404", r)
	}
	iterate(t, d) // a second 404 in a row, and it's dropped
	from = len(f.Requests())
	iterate(t, d)
	if r := readsOf(f, from, 2); len(r) != 0 {
		t.Errorf("read deleted issue 2 again: %+v", r)
	}
	if !reflect.DeepEqual(*work, [][]int{{1, 2}, {1}, {1}, {1}, {1}}) {
		t.Errorf("Work got %v, want issue 2 only before it was deleted", *work)
	}
}

// One 404, from a proxy or a glitch, doesn't drop an issue: the next read
// finds it, and it's visited and given to Work again. Two with no good read
// between do.
func TestIterateDropsAnIssueOnlyAfterTwo404s(t *testing.T) {
	f := newFake(t)
	fileAt(f, "spec-review", "a")
	fileAt(f, "ready", "b")
	var visits []int
	d := newDaemon(t, f, newOrigin(t), &visits)
	work := recordWork(d)
	iterate(t, d)
	// touch makes issue 1 changed, so it's read again, and issue 2 newer,
	// so the cursor moves past issue 1 and the poll lists it only once.
	touch := func() {
		f.AddComment(1, "owner", "a comment")
		f.AddComment(2, "owner", "a comment")
	}
	notFound := func() {
		f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1" }, 404)
	}
	pass := func(step string, want []int, visited bool) {
		t.Helper()
		visits = nil
		iterate(t, d)
		got, saw := (*work)[len(*work)-1], slices.Contains(visits, 1)
		if !reflect.DeepEqual(got, want) || saw != visited {
			t.Fatalf("%s: Work got %v, and issue 1 visited is %t; want %v and %t", step, got, saw, want, visited)
		}
	}
	touch()
	notFound()
	pass("a 404", []int{2}, false)
	pass("the next pass", []int{1, 2}, true)

	// The good read cleared that 404, so a later one is a first again.
	touch()
	notFound()
	pass("a later 404", []int{2}, false)
	pass("the pass after it", []int{1, 2}, true)

	touch()
	notFound()
	pass("a 404", []int{2}, false)
	notFound()
	pass("a second 404 in a row", []int{2}, false)
	from := len(f.Requests())
	pass("the pass after the drop", []int{2}, false)
	if r := readsOf(f, from, 1); len(r) != 0 {
		t.Errorf("read dropped issue 1 again: %+v", r)
	}

	// A dropped issue that changes is read again, and a 404 then is a first.
	touch()
	notFound()
	pass("a 404 once it's back", []int{2}, false)
	pass("the pass after it", []int{1, 2}, true)

	// A close clears a 404, so once reopened a 404 is a first again.
	touch()
	notFound()
	pass("a 404 before a close", []int{2}, false)
	f.CloseBy(1, "owner") // judged, and given its closed stage (T6)
	f.AddComment(2, "owner", "a comment")
	pass("the close", []int{2}, false)
	f.ReopenBy(1, "owner") // a trusted reopen, which holds it (T6)
	touch()
	notFound()
	pass("a 404 once reopened", []int{2}, false)
	pass("the pass after it", []int{1, 2}, true)

	// A failed read between two 404s shows nothing, so they count as two.
	touch()
	notFound()
	pass("a 404", []int{2}, false)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1" }, 500)
	if err := d.Iterate(context.Background()); err == nil {
		t.Fatal("Iterate = nil, want issue 1's failed read")
	}
	notFound()
	pass("a 404 after a failed read", []int{2}, false)
	from = len(f.Requests())
	pass("the pass after the drop", []int{2}, false)
	if r := readsOf(f, from, 1); len(r) != 0 {
		t.Errorf("read dropped issue 1 again: %+v", r)
	}
}

// A visit that fails, even with a 404, as on a deleted comment, doesn't drop
// the issue: it's read again and given to Work.
func TestIterateKeepsAnIssueWhoseVisit404s(t *testing.T) {
	f := quiet(t)
	d := newDaemon(t, f, newOrigin(t), nil)
	work := recordWork(d)
	iterate(t, d)
	failed := false
	d.Step.Reactions = func(_ context.Context, _ *daemon.Applier, s *github.Snapshot, _ daemon.Dirt) error {
		if s.Number != 2 || failed {
			return nil
		}
		failed = true
		return fmt.Errorf("reactions: %w", &gogithub.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}})
	}
	if err := d.Iterate(context.Background()); err == nil {
		t.Error("Iterate = nil, want issue 2's visit to fail")
	}
	from := len(f.Requests())
	iterate(t, d)
	if r := readsOf(f, from, 2); len(r) == 0 || r[0].Path != "/issues/2" || r[0].Status >= 400 {
		t.Errorf("reads of issue 2 = %+v, want it read again", r)
	}
	if !reflect.DeepEqual(*work, [][]int{{1, 2}, {1}, {1, 2}}) {
		t.Errorf("Work got %v, want issue 2 left out once, then back", *work)
	}
}

// A comment edited while the comment poll pages moves to the front of a
// newest-first list, so a later page repeats a comment and skips none.
func TestIteratePollSkipsNoComment(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "a")
	var many []int64
	for i := range 100 {
		many = append(many, f.AddComment(1, "owner", fmt.Sprint("comment ", i)))
	}
	fileAt(f, "ready", "b", "priority:p2")
	cmd := f.AddComment(2, "owner", "hello")
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)

	for _, id := range many {
		f.EditComment(id, "owner", "edited")
	}
	f.EditComment(cmd, "owner", "/priority p1") // edits change no issue's updated_at
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Path == "/issues/comments" && strings.Contains(r.Query, "page=2")
	}, func() { f.EditComment(many[0], "owner", "edited again") })
	iterate(t, d)
	if got := f.State().Issues[1].Labels; !reflect.DeepEqual(got, []string{"priority:p1", "stage:ready"}) {
		t.Errorf("issue 2's labels = %v, want /priority p1 applied", got)
	}
}

// The Dirty cache is keyed on lastEditedAt as well as the body, so an edit
// that restores the body is read: here mallory's, behind owner's.
func TestDirtyCacheMissesARestoredBody(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Issue", "body")
	id := f.AddComment(1, "owner", "x")
	f.EditComment(id, "owner", "y")
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := a.Dirty(context.Background(), s, trusted{"owner": true}); err != nil || d.Has(id) {
		t.Fatalf("Dirty = %v, %v; want the owner's own edit clean", d, err)
	}
	f.EditComment(id, "mallory", "z")
	f.EditComment(id, "owner", "y")
	s, err = a.Client.Snapshot(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := a.Dirty(context.Background(), s, trusted{"owner": true}); err != nil || d[id] != "mallory" {
		t.Errorf("Dirty = %v, %v; want comment %d dirty by mallory", d, err, id)
	}
}

// A history that fails closed isn't kept, so it's read again once GitHub
// has caught up.
func TestDirtyCacheSkipsAShortHistory(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Issue", "body")
	id := f.AddComment(1, "owner", "x")
	f.EditComment(id, "owner", "y")
	f.HistoryCap(1) // the original post only, short of the edit
	a, s, err := applier(t, f)
	if err != nil {
		t.Fatal(err)
	}
	if d, err := a.Dirty(context.Background(), s, trusted{"owner": true}); err != nil || !d.Has(id) {
		t.Fatalf("Dirty = %v, %v; want comment %d dirty while its history is short", d, err, id)
	}
	f.HistoryCap(100)
	if d, err := a.Dirty(context.Background(), s, trusted{"owner": true}); err != nil || d.Has(id) {
		t.Errorf("Dirty = %v, %v; want comment %d clean once its history shows the edit", d, err, id)
	}
}
