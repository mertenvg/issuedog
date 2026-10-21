package daemon_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// An issue gone in the pass its assignee write fails, with an answer gone
// doesn't take as the issue gone, and a removal that fails before an add
// (#407, from #238's and #240's verify).

// assignGoneRefusedRig is newRig on f with assignment on and the
// interviewer's run asking more questions, whose Work records in worked
// each issue it gets before it runs the stage work.
func assignGoneRefusedRig(t *testing.T, f *githubtest.Fake, worked *[]int) *rig {
	t.Helper()
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.d.Assign = true
	g.stage.moreQuestions = true
	g.claude(okOut, 0)
	g.d.Work = func(ctx context.Context, ready []daemon.Ready) error {
		for _, r := range ready {
			*worked = append(*worked, r.S.Number)
		}
		return g.r.Work(ctx, ready)
	}
	return g
}

// An issue transferred just before its assignee write, which GitHub
// refuses with a 422, as it might answer a login it can't assign, reaches
// Work in that pass (#238), since the 422 says nothing of the snapshot.
// The read before the stage run finds it gone, so it gets no run, and
// nothing is written to it, by Work or anything else. The next pass's
// read finds it gone again, which drops it, and no later pass asks about
// it.
func TestAssignGoneRefused(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	interview(f)
	f.SetAssignees(1, "bob")
	var worked []int
	g := assignGoneRefusedRig(t, f, &worked)
	write := func(r githubtest.Request) bool {
		return r.Method == http.MethodDelete && r.Path == "/issues/1/assignees"
	}
	f.BeforeRequest(write, func() { f.Transfer(1, "o/elsewhere") })
	f.AnswerOnce(write, http.StatusUnprocessableEntity)
	from, writes := len(f.Requests()), len(f.Writes())
	err := g.d.Iterate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the assignees of o/r#1, read again next pass") || assignFailOnly(err, 1) != nil {
		t.Errorf("the pass = %v; want the refused write's error alone", err)
	}
	if !slices.Contains(worked, 1) {
		t.Errorf("Work got %v; want 1, whose write was refused", worked)
	}
	about := assignAbout(f, from, 1)
	at := slices.Index(about, "DELETE /issues/1/assignees 422")
	if at < 0 {
		t.Fatalf("requests about 1 = %q; want the refused removal", about)
	}
	if got, want := about[at+1:], []string{"GET /issues/1 301", "GET /repositories/2/issues/1 200"}; !slices.Equal(got, want) {
		t.Errorf("requests about 1 after the refused removal = %q; want %q, the read before the run, which finds it gone", got, want)
	}
	if n := g.runs(); n != 0 {
		t.Errorf("claude ran %d times; want none, as the issue is gone", n)
	}
	for _, w := range f.Writes()[writes:] {
		if strings.HasPrefix(w.Path, "/issues/1") {
			t.Errorf("wrote %s %s to the transferred issue; want nothing", w.Method, w.Path)
		}
	}
	for i, want := range [][]string{{"GET /issues/1 301", "GET /repositories/2/issues/1 304"}, nil, nil} {
		worked, from, writes = nil, len(f.Requests()), len(f.Writes())
		iterate(t, g.d)
		if got := assignAbout(f, from, 1); !slices.Equal(got, want) {
			t.Errorf("pass %d after: requests about 1 = %q; want %q", i+1, got, want)
		}
		if slices.Contains(worked, 1) {
			t.Errorf("pass %d after: Work got %v; want 1 left out", i+1, worked)
		}
		for _, w := range f.Writes()[writes:] {
			if strings.HasPrefix(w.Path, "/issues/1") {
				t.Errorf("pass %d after: wrote %s %s; want nothing", i+1, w.Method, w.Path)
			}
		}
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("issue 1's assignees = %q; want bob, whose removal was refused", got)
	}
}

// A removal GitHub refuses ends the issue's assignee writes for that
// pass, so the add it would make waits for the next pass, which makes
// both: bob, assigned by hand to an issue with an unanswered round, goes,
// and mertenvg, unassigned by hand, comes back.
func TestAssignGoneRefusedAddWaits(t *testing.T) {
	t.Parallel()
	f, d := assignAddFake(t)
	f.SetAssignees(12, "bob")
	assignFailRefuse(f, 12, http.MethodDelete)
	from := len(f.Requests())
	err := d.Iterate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the assignees of o/r#12, read again next pass") || assignFailOnly(err, 12) != nil {
		t.Errorf("the pass = %v; want the refused removal's error alone", err)
	}
	if got, want := assignFailCount(assignAbout(f, from, 12), "DELETE /issues/12/assignees 422"), 1; got != want {
		t.Errorf("the refused removal was made %d times: %q; want once", got, assignAbout(f, from, 12))
	}
	if got := assignFailCount(assignAbout(f, from, 12), "POST /issues/12/assignees "); got != 0 {
		t.Errorf("the pass whose removal was refused added %d times: %q; want no add", got, assignAbout(f, from, 12))
	}
	if got := assignees(f, 12); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("issue 12's assignees after the refused removal = %q; want bob alone", got)
	}
	assignStep(t, f, d, "the next pass", assignRemove(12, "bob"), assignAdd(12, digestRunOwner))
	if got := assignees(f, 12); !slices.Equal(got, []string{digestRunOwner}) {
		t.Errorf("issue 12's assignees after the next pass = %q; want mertenvg alone", got)
	}
	assignStep(t, f, d, "a pass after")
}
