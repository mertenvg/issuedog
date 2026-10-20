package daemon_test

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// An assignee write that keeps failing, in a way gone doesn't take as the
// issue gone, doesn't keep its issue from Work (#238). GitHub might answer
// a login it can't assign with a 422 on every pass (SPEC §16).

// assignFailRefuse makes f answer the next assignee write by method to
// issue n with a 422, as GitHub might for a login it can't assign.
func assignFailRefuse(f *githubtest.Fake, n int, method string) {
	path := fmt.Sprintf("/issues/%d/assignees", n)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == method && r.Path == path }, http.StatusUnprocessableEntity)
}

// assignFailCount is how many of about, as assignAbout gives them, start
// with prefix.
func assignFailCount(about []string, prefix string) int {
	n := 0
	for _, r := range about {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

// assignFailOnly returns err less the errors of issue n's assignee writes
// that GitHub refused with a 422, which every pass that refuses them has,
// so what's left is the pass's other errors.
func assignFailOnly(err error, n int) error {
	refused := fmt.Sprintf("the assignees of o/r#%d, read again next pass: ", n)
	var rest []error
	var walk func(error)
	walk = func(e error) {
		var joined interface{ Unwrap() []error }
		if errors.As(e, &joined) {
			for _, e := range joined.Unwrap() {
				walk(e)
			}
			return
		}
		if !strings.HasPrefix(e.Error(), refused) || !strings.Contains(e.Error(), "422") {
			rest = append(rest, e)
		}
	}
	if err != nil {
		walk(err)
	}
	return errors.Join(rest...)
}

// An assignee write that GitHub refuses with a 422 on every pass, a remove
// or an add, is the pass's error each time, and Work gets its issue in
// every one of those passes. Each reads the issue once and makes the
// write once, which is all the failure costs. Once GitHub takes the
// write, the pass after reads the issue written to again, and a pass
// where nothing changed makes no request about it.
func TestAssignFailWorks(t *testing.T) {
	t.Parallel()
	for _, write := range []struct {
		name   string
		fake   func(t *testing.T) (*githubtest.Fake, *daemon.Daemon)
		method string
		want   string
	}{
		{"remove", assignGoneFake, http.MethodDelete, assignRemove(12, "bob")},
		{"add", assignAddFake, http.MethodPost, assignAdd(12, digestRunOwner)},
	} {
		t.Run(write.name, func(t *testing.T) {
			t.Parallel()
			f, d := write.fake(t)
			var worked []int
			assignWorked(d, &worked)
			for pass := 1; pass <= 3; pass++ {
				assignFailRefuse(f, 12, write.method)
				worked = nil
				from := len(f.Requests())
				err := d.Iterate(t.Context())
				if err == nil || !strings.Contains(err.Error(), "the assignees of o/r#12, read again next pass") || assignFailOnly(err, 12) != nil {
					t.Errorf("pass %d: Iterate = %v; want the refused write's error alone", pass, err)
				}
				if !slices.Contains(worked, 12) {
					t.Errorf("pass %d: Work got %v; want 12 among them, though its write was refused", pass, worked)
				}
				about := assignAbout(f, from, 12)
				if got := assignFailCount(about, write.method+" /issues/12/assignees 422"); got != 1 {
					t.Errorf("pass %d: the refused write was made %d times: %q; want once", pass, got, about)
				}
				if got := assignFailCount(about, "GET /issues/12 "); got != 1 {
					t.Errorf("pass %d: read 12 %d times: %q; want once", pass, got, about)
				}
			}
			worked = nil
			assignStep(t, f, d, "the pass GitHub takes the write", write.want)
			assignStep(t, f, d, "a pass that reads 12 written to again")
			from := len(f.Requests())
			assignStep(t, f, d, "a pass where nothing changed")
			for _, r := range f.Requests()[from:] {
				if strings.HasSuffix(r.Path, "/assignees") || r.Method == http.MethodGet && assignIssueRead.MatchString(r.Path) {
					t.Errorf("a pass where nothing changed requested %s %s; want no read of an issue and no assignee write", r.Method, r.Path)
				}
			}
			if got := len(slices.DeleteFunc(slices.Clone(worked), func(n int) bool { return n != 12 })); got != 3 {
				t.Errorf("Work got %v after GitHub took the write; want 12 in each of the 3 passes", worked)
			}
		})
	}
}

// assignFailPass is a pass of a new rig on f, with root its state, as a
// restart keeps it: assignment on, the interviewer's run asking more
// questions, so the issue stays at clarifying and the pass after has
// nothing to write, and GitHub refusing to remove issue 1's assignees
// with a 422.
func assignFailPass(t *testing.T, f *githubtest.Fake, o *origin, root string) error {
	t.Helper()
	g := newRig(t, f, o, root)
	g.d.Assign = true
	g.stage.moreQuestions = true
	g.claude(okOut, 0)
	assignFailRefuse(f, 1, http.MethodDelete)
	return g.d.Iterate(t.Context())
}

// The stage runs in the pass whose assignee write GitHub refuses: bob,
// assigned by hand to an epic at clarifying with an answer, isn't
// removed, and the pass's error says why, but the interviewer's run is
// applied all the same.
func TestAssignFailRuns(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	answer := interview(f)
	f.SetAssignees(1, "bob")
	o, root := newOrigin(t), t.TempDir()
	err := assignFailPass(t, f, o, root)
	if err == nil || !strings.Contains(err.Error(), "the assignees of o/r#1, read again next pass") || assignFailOnly(err, 1) != nil {
		t.Errorf("the pass = %v; want the refused write's error alone", err)
	}
	is := f.State().Issues[0]
	if last := is.Comments[len(is.Comments)-1].Body; last != applied("clarifying", "run1", answer) || !slices.Equal(is.Labels, []string{"stage:clarifying"}) {
		t.Errorf("newest comment %q and labels %v; want run1's questions, at clarifying", last, is.Labels)
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("issue 1's assignees = %q; want bob, whose removal was refused", got)
	}
}

// Replay passes for the stage's writes in a pass whose assignee write
// GitHub refuses, whatever write a crash comes at. Each pass is a restart
// that keeps the state, and its refused write is no error of Replay's.
func TestAssignFailRunsReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots := map[*githubtest.Fake]string{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		roots[f] = t.TempDir()
		interview(f)
		f.SetAssignees(1, "bob")
		return f
	}, func(f *githubtest.Fake) error {
		return assignFailOnly(assignFailPass(t, f, o, roots[f]), 1)
	})
}
