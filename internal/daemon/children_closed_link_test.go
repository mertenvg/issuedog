package daemon_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A changed child that closes during a revision's pass gets no blocked-by
// link write, as one the listing found closed gets none (#632, from #607's
// verify; SPEC §5.2, "Applying a revision", step 2): once childChanged
// finds it closed, or its label rules close it, it's closed for the rest
// of the pass.

// closedLinkReopened has eve, who isn't trusted, reopen T2's child, #3,
// after the owner closed it, and the close judge set its closed stage, so
// the label rules close it again (SPEC §6.4).
func closedLinkReopened(f *githubtest.Fake, stage string) {
	f.CloseBy(3, "owner")
	f.RemoveLabel(3, app, stage)
	f.AddLabel(3, app, "stage:completed")
	f.ReopenBy(3, "eve")
}

// closedLinkWrites are f's writes to #3's blocked-by links.
func closedLinkWrites(f *githubtest.Fake) []githubtest.Write {
	var writes []githubtest.Write
	for _, w := range f.Writes() {
		if strings.HasPrefix(w.Path, "/issues/3/dependencies/") {
			writes = append(writes, w)
		}
	}
	return writes
}

// T2's child, #3, which revision 2 changes to depend on T1, T3 and T4, is
// closed during the pass, after the listing found it open: by the label
// rules, undoing eve's untrusted reopen, before its change, or on a
// replay after its move, keyed by the approval, stopped by a crash at its
// first link; or by the owner, between the listing and its read. It gets
// no blocked-by link write, nor a rewrite or a move but one that stands
// already, and the pass returns no error. The record stands. A crash at any write replays.
func TestChildrenChangedClosedNoLink(t *testing.T) {
	t.Parallel()
	pass := childrenPass(t, revisionPlan)
	link := func(w githubtest.Write) bool {
		return w.Method == http.MethodPost && w.Path == "/issues/3/dependencies/blocked_by"
	}
	for _, tt := range []struct {
		name   string
		setup  func(*testing.T) func(testing.TB) *githubtest.Fake
		reopen bool // eve reopened it, and the label rules close it again
		moved  bool // its move, keyed by the approval, stands
	}{
		{"an untrusted reopen undone before its change", func(*testing.T) func(testing.TB) *githubtest.Fake {
			return changedSetup(false, func(f *githubtest.Fake, _ changedIDs) { closedLinkReopened(f, "stage:in-progress") })
		}, true, false},
		{"an untrusted reopen undone after its move", func(t *testing.T) func(testing.TB) *githubtest.Fake {
			at := writeAt(t, changedSetup(false, nil), pass, 1, link)
			return func(tb testing.TB) *githubtest.Fake {
				f := childrenCrashStopped(tb, changedSetup(false, nil), pass, at)
				closedLinkReopened(f, "stage:ready")
				f.ForgetWrites()
				return f
			}
		}, true, true},
		{"the owner's close after the listing", func(*testing.T) func(testing.TB) *githubtest.Fake {
			return func(tb testing.TB) *githubtest.Fake {
				f := changedSetup(false, nil)(tb)
				f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/3" },
					func() { f.CloseBy(3, "owner") })
				return f
			}
		}, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := tt.setup(t)
			githubtest.Replay(t, setup, pass)

			f := run(t, setup, pass)
			if w := closedLinkWrites(f); len(w) != 0 {
				t.Errorf("T2's child, closed, got the link writes %+v; want none", w)
			}
			t2 := f.State().Issues[2]
			if !t2.Closed || len(t2.BlockedBy) != 0 {
				t.Errorf("T2's child is closed %t, blocked by %v; want it closed, blocked by nothing", t2.Closed, t2.BlockedBy)
			}
			var again, moves int
			for _, c := range t2.Comments {
				if strings.HasPrefix(c.Body, "Rewritten from #1's plan, rev 2") {
					moves++
				}
				if strings.HasPrefix(c.Body, "I've closed this issue again") {
					again++
				}
			}
			if want := map[bool]int{true: 1}[tt.moved]; moves != want {
				t.Errorf("T2's child has %d moves keyed by the approval, want %d", moves, want)
			}
			if want := map[bool]int{true: 1}[tt.reopen]; again != want {
				t.Errorf("T2's child has %d comments closing it again, want %d", again, want)
			}
			if recs := droppedRecords(f, 1); len(recs) != 1 || !strings.HasSuffix(recs[0], "<!-- issuedog:created rev=2 children=2,3,4,6 -->") {
				t.Errorf("issue 1's records are %q, want one listing #2, #3, #4 and #6", recs)
			}
		})
	}
}
