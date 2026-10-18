package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// Assignment (#191, plan #87's T7) against the fakes, with step 1 wired
// as main wires it and the digest off, so the items come from the cache
// that Iterate fills either way. The repo is the digest's whole run's
// (digestRunFake): mertenvg is notify, and issues 1 to 6 have items,
// 7 to 10 are at ready with none, and 11 is closed.

// assignDaemon is digestRunDaemon with the digest off and assignment on.
func assignDaemon(t testing.TB, f *githubtest.Fake, o *origin, worked *[]int) *daemon.Daemon {
	t.Helper()
	d := digestRunDaemon(t, f, o, nil, worked)
	d.Digest, d.Assign = false, true
	return d
}

// assignWrites is the assignee writes from the from'th write on, as
// "METHOD /path body", with the body's trailing newline cut.
func assignWrites(f *githubtest.Fake, from int) []string {
	var got []string
	for _, w := range f.Writes()[from:] {
		if strings.HasSuffix(w.Path, "/assignees") {
			got = append(got, w.Method+" "+w.Path+" "+strings.TrimSpace(w.Body))
		}
	}
	return got
}

// assignStep runs one Iterate of d, which must succeed, and checks that
// its assignee writes are want.
func assignStep(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, step string, want ...string) {
	t.Helper()
	from := len(f.Writes())
	iterate(t, d)
	if got := assignWrites(f, from); !slices.Equal(got, want) {
		t.Errorf("%s: assignee writes %q; want %q", step, got, want)
	}
}

// assignees is issue n's assignees in f, sorted.
func assignees(f *githubtest.Fake, n int) []string {
	return f.State().Issues[n-1].Assignees
}

// assignWant checks each issue's assignees in f against want, by number:
// an issue want leaves out has none.
func assignWant(t *testing.T, f *githubtest.Fake, step string, want map[int][]string) {
	t.Helper()
	for _, is := range f.State().Issues {
		if got := is.Assignees; !slices.Equal(got, want[is.Number]) {
			t.Errorf("%s: issue %d's assignees = %q; want %q", step, is.Number, got, want[is.Number])
		}
	}
}

// assignAdd and assignRemove are the writes that add or remove logins on
// issue n.
func assignAdd(n int, logins ...string) string {
	return fmt.Sprintf(`POST /issues/%d/assignees {"assignees":["%s"]}`, n, strings.Join(logins, `","`))
}

func assignRemove(n int, logins ...string) string {
	return fmt.Sprintf(`DELETE /issues/%d/assignees {"assignees":["%s"]}`, n, strings.Join(logins, `","`))
}

// assignIssueRead matches the path of an issue's GET, which a snapshot
// starts with.
var assignIssueRead = regexp.MustCompile(`^/issues/[0-9]+$`)

// The whole of it. A round, a spec at spec-review, a flag and a state
// error each assign mertenvg, and a pipeline issue with none has a manual
// assignee removed, while a closed issue and one outside the pipeline
// keep theirs. A pass after that, and a restart, write nothing, and a
// pass where nothing changed makes no request of assignment's: no read of
// an issue, no assignee write. Then each item goes, by an answer, a 👍 on
// the revision, a 👍 on the flag and a 👍 on the error, and its issue is
// unassigned the same pass. Work gets every open issue all along.
func TestAssignWhole(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), digestRunOrigin(t)
	f.SetAssignees(7, "bob")
	f.SetAssignees(11, "bob")
	outside := f.AddIssue(digestRunEve, "Not triaged yet", "An idea.") // 12, with no stage, marker or item
	f.SetAssignees(outside, "bob")
	var worked []int
	d := assignDaemon(t, f, o, &worked)
	assignStep(t, f, d, "the first pass", assignAdd(1, digestRunOwner), assignAdd(2, digestRunOwner), assignAdd(3, digestRunOwner),
		assignAdd(4, digestRunOwner), assignAdd(5, digestRunOwner), assignAdd(6, digestRunOwner), assignRemove(7, "bob"))
	want := map[int][]string{11: {"bob"}, outside: {"bob"}}
	for n := 1; n <= 6; n++ {
		want[n] = []string{digestRunOwner}
	}
	assignWant(t, f, "the first pass", want)
	assignStep(t, f, d, "a pass that reads the issues written to again")
	from := len(f.Requests())
	assignStep(t, f, d, "a pass where nothing changed")
	for _, r := range f.Requests()[from:] {
		if strings.HasSuffix(r.Path, "/assignees") || r.Method == http.MethodGet && assignIssueRead.MatchString(r.Path) {
			t.Errorf("a pass where nothing changed requested %s %s; want no read of an issue and no assignee write", r.Method, r.Path)
		}
	}
	assignStep(t, f, assignDaemon(t, f, o, nil), "a restart")

	answer := f.AddComment(1, digestRunOwner, "1a 2b")
	assignStep(t, f, d, "the answer", assignRemove(1, digestRunOwner))
	f.AddReaction(digestRunCommentID(t, f, 2, "<!-- issuedog:revision rev=1 "), digestRunOwner, "+1")
	assignStep(t, f, d, "the approval", assignRemove(2, digestRunOwner))
	f.AddReaction(digestRunCommentID(t, f, 3, "<!-- issuedog:triage verdict=ask "), digestRunOwner, "+1")
	assignStep(t, f, d, "the flag's answer", assignRemove(3, digestRunOwner))
	f.AddReaction(digestRunCommentID(t, f, 4, "kind=disagreement key=1 -->"), digestRunOwner, "+1")
	assignStep(t, f, d, "the error's ack", assignRemove(4, digestRunOwner))
	delete(want, 1)
	delete(want, 2)
	delete(want, 3)
	delete(want, 4)
	assignWant(t, f, "after every answer", want)
	if got := commentBodies(f, 1); len(got) == 0 || !slices.ContainsFunc(f.State().Issues[0].Comments, func(c githubtest.CommentState) bool {
		return c.Body == "1a 2b" && slices.Contains(c.Reactions, app+" +1")
	}) {
		t.Errorf("the comments on 1 = %q; want the answer %d taken, with the App's 👍", got, answer)
	}
	assignStep(t, f, d, "a pass after the answers")
	assignStep(t, f, assignDaemon(t, f, o, nil), "a restart after the answers")
	for _, n := range []int{1, 7, 10, outside} {
		if !slices.Contains(worked, n) {
			t.Errorf("Work got %v; want issue %d among them", worked, n)
		}
	}
}

// An unanswered planner round at planning assigns notify, as an
// interviewer round does, and a pass where nothing changed makes no
// request: no read of the issue, no assignee write. A trusted answer gets
// the App's 👍 and ends the item, so the issue is unassigned the same
// pass, and an untrusted comment does neither (#253). The planner isn't
// built yet, so its round is a hand-built marker.
func TestAssignPlanRound(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestRunOrigin(t)
	f.SetPermission(digestRunOwner, "admin")
	n := f.AddIssue(digestRunOwner, "Quote expiry", "Quotes should expire.", "type:epic")
	f.AddLabel(n, app, "stage:planning")
	f.AddComment(n, app, "<!-- issuedog:transition from=spec-review to=planning cmd=r7 rev=1 body=abcd1234 -->")
	f.AddComment(n, app, fmt.Sprintf("### Questions for #%d (round 3)\n\n**1. Which scope?** Blocks: #%d\n   a. daemon (recommended): it's listed\n   b. web\n\n"+
		"Reply in one comment, e.g. `1a 2c 3: free text`.\n\ncc @%s\n\n"+
		"<!-- issuedog:transition from=planning to=planning run=p1 round=3 questions=1 -->", n, n, digestRunOwner))
	d := assignDaemon(t, f, o, nil)
	assignStep(t, f, d, "the first pass", assignAdd(n, digestRunOwner))
	assignWant(t, f, "the first pass", map[int][]string{n: {digestRunOwner}})
	assignStep(t, f, d, "a pass that reads the issue written to again")
	from := len(f.Requests())
	assignStep(t, f, d, "a pass where nothing changed")
	for _, r := range f.Requests()[from:] {
		if strings.HasSuffix(r.Path, "/assignees") || r.Method == http.MethodGet && assignIssueRead.MatchString(r.Path) {
			t.Errorf("a pass where nothing changed requested %s %s; want no read of an issue and no assignee write", r.Method, r.Path)
		}
	}
	f.AddComment(n, digestRunEve, "1b")
	assignStep(t, f, d, "an untrusted comment")
	assignWant(t, f, "an untrusted comment", map[int][]string{n: {digestRunOwner}})
	f.AddComment(n, digestRunOwner, "1a")
	assignStep(t, f, d, "the answer", assignRemove(n, digestRunOwner))
	assignWant(t, f, "the answer", nil)
	var acked []string
	for _, c := range f.State().Issues[n-1].Comments {
		if slices.Contains(c.Reactions, app+" +1") {
			acked = append(acked, c.Body)
		}
	}
	if !slices.Equal(acked, []string{"1a"}) {
		t.Errorf("the App 👍'd %q; want the answer alone", acked)
	}
	assignStep(t, f, d, "a pass after the answer")
	assignStep(t, f, assignDaemon(t, f, o, nil), "a restart after the answer")
}

// assignPlanReview files an epic by author at plan-review, as the App set
// it, with plan revision 1 in the App's plan comment, as S1's planner run
// posts it: the plan, a cc line for cc and the move from planning, above
// the revision marker. It returns the epic's number and the plan's hash.
func assignPlanReview(f *githubtest.Fake, author, cc string) (int, string) {
	n := f.AddIssue(author, "Quote expiry", "Quotes should expire.", "type:epic")
	f.AddLabel(n, app, "stage:plan-review")
	f.AddComment(n, app, "<!-- issuedog:transition from=spec-review to=planning cmd=r7 rev=1 body=abcd1234 -->")
	plan := fmt.Sprintf("### Plan for #%d (rev 1)\n\nThe plan.\n\ncc @%s\n\n<!-- issuedog:transition from=planning to=plan-review run=p1 -->", n, cc)
	hash := state.BodyHash(plan)
	f.AddComment(n, app, plan+"\n<!-- issuedog:revision kind=plan rev=1 body="+hash+" -->")
	return n, hash
}

// assignPlanApproved is the App's move from plan-review to planned that
// approves plan revision 1 with hash, by reaction 9, as #268 writes it.
func assignPlanApproved(hash string) string {
	return "<!-- issuedog:transition from=plan-review to=planned rev=1 body=" + hash + " cmd=r9 -->"
}

// An epic at plan-review assigns notify while its plan waits, and a pass
// where nothing changed makes no request: no read of the issue, no
// assignee write (#270). An approval, the move to planned, ends the item,
// and so does a revision request, the move back to planning, so each
// issue is unassigned the same pass, and a restart writes nothing. The
// approval and the revision request aren't built yet (#268, #269), so
// their moves are hand-built markers.
func TestAssignPlanReview(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestRunOrigin(t)
	f.SetPermission(digestRunOwner, "admin")
	approved, hash := assignPlanReview(f, digestRunOwner, digestRunOwner)
	sentBack, _ := assignPlanReview(f, digestRunOwner, digestRunOwner)
	d := assignDaemon(t, f, o, nil)
	assignStep(t, f, d, "the first pass", assignAdd(approved, digestRunOwner), assignAdd(sentBack, digestRunOwner))
	assignWant(t, f, "the first pass", map[int][]string{approved: {digestRunOwner}, sentBack: {digestRunOwner}})
	assignStep(t, f, d, "a pass that reads the issues written to again")
	from := len(f.Requests())
	assignStep(t, f, d, "a pass where nothing changed")
	for _, r := range f.Requests()[from:] {
		if strings.HasSuffix(r.Path, "/assignees") || r.Method == http.MethodGet && assignIssueRead.MatchString(r.Path) {
			t.Errorf("a pass where nothing changed requested %s %s; want no read of an issue and no assignee write", r.Method, r.Path)
		}
	}
	assignStep(t, f, assignDaemon(t, f, o, nil), "a restart")
	f.AddComment(approved, app, assignPlanApproved(hash))
	assignStep(t, f, d, "the approval", assignRemove(approved, digestRunOwner))
	f.AddComment(sentBack, app, "<!-- issuedog:transition from=plan-review to=planning cmd=99 seen=99 -->")
	assignStep(t, f, d, "the revision request", assignRemove(sentBack, digestRunOwner))
	assignWant(t, f, "after both", nil)
	assignStep(t, f, d, "a pass after both")
	assignStep(t, f, assignDaemon(t, f, o, nil), "a restart after both")
}

// assignPlanReplayFake returns a setup for Replay: o/r with owner's epic
// 1 at plan-review, assigned to waiting, and epic 2 approved to planned
// from plan-review, assigned to approved.
func assignPlanReplayFake(waiting, approved []string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := newFake(t)
		n, _ := assignPlanReview(f, "owner", "owner")
		f.SetAssignees(n, waiting...)
		m, hash := assignPlanReview(f, "owner", "owner")
		f.AddComment(m, app, assignPlanApproved(hash))
		f.SetAssignees(m, approved...)
		return f
	}
}

// Replay passes for the assign of a plan waiting at plan-review and the
// unassign of one approved, alone and together, whatever write a crash
// comes at (#270).
func TestAssignPlanReviewReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	for _, tc := range []struct {
		name              string
		waiting, approved []string
		want              []string
	}{
		{"an assign", nil, nil, []string{assignAdd(1, "owner")}},
		{"an unassign", []string{"owner"}, []string{"owner"}, []string{assignRemove(2, "owner")}},
		{"both", nil, []string{"owner"}, []string{assignAdd(1, "owner"), assignRemove(2, "owner")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := assignPlanReplayFake(tc.waiting, tc.approved)
			clean := setup(t)
			if err := assignReplayPass(t, o)(clean); err != nil {
				t.Fatal(err)
			}
			if got := assignWrites(clean, 0); !slices.Equal(got, tc.want) {
				t.Errorf("the clean pass's assignee writes = %q; want %q", got, tc.want)
			}
			githubtest.Replay(t, setup, assignReplayPass(t, o))
		})
	}
}

// assignNoNotifyOrigin is digestRunOrigin with notify empty.
func assignNoNotifyOrigin(t testing.TB) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+digestRunOwner+"\n")
	o.commit(config.Path, "types: labels\n")
	return o
}

// With notify empty, nobody is assigned: an issue with an item that has
// mertenvg assigned by hand loses him, and so does one without.
func TestAssignNotifyEmpty(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), assignNoNotifyOrigin(t)
	f.SetAssignees(1, digestRunOwner)
	f.SetAssignees(8, digestRunOwner)
	d := assignDaemon(t, f, o, nil)
	assignStep(t, f, d, "the first pass", assignRemove(1, digestRunOwner), assignRemove(8, digestRunOwner))
	assignWant(t, f, "the first pass", nil)
	assignStep(t, f, d, "a second pass")
}

// A manual change on a pipeline issue is put right on the next poll: an
// assignee added beside mertenvg goes, and mertenvg, removed, comes back.
// Logins compare in any case, so MERTENVG for mertenvg is no change.
func TestAssignRevertsManual(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), digestRunOrigin(t)
	d := assignDaemon(t, f, o, nil)
	iterate(t, d)
	f.SetAssignees(1, digestRunOwner, "bob")
	f.SetAssignees(2)
	f.SetAssignees(3, "MERTENVG")
	assignStep(t, f, d, "the next poll", assignRemove(1, "bob"), assignAdd(2, digestRunOwner))
	if got := assignees(f, 3); !slices.Equal(got, []string{"MERTENVG"}) {
		t.Errorf("issue 3's assignees = %q; want MERTENVG as set", got)
	}
	assignStep(t, f, d, "a pass after")
}

// An issue whose read fails keeps its assignees that pass, and is put
// right on the next. A failed assignee write is the pass's error, and the
// rest of the issues, and Work, carry on without its issue; the next pass
// reads it again, and writes it again (#199).
func TestAssignFailures(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), digestRunOrigin(t)
	var worked []int
	d := assignDaemon(t, f, o, &worked)
	iterate(t, d)
	f.SetAssignees(1, digestRunOwner, "bob")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/1" }, http.StatusInternalServerError)
	from := len(f.Writes())
	if err := d.Iterate(t.Context()); err == nil {
		t.Errorf("Iterate with a failed read = nil; want its error")
	}
	if got := assignWrites(f, from); got != nil {
		t.Errorf("assignee writes with issue 1's read failed = %q; want none", got)
	}
	assignStep(t, f, d, "the next pass", assignRemove(1, "bob"))

	f.SetAssignees(2)
	f.SetAssignees(3)
	f.FailWrite(len(f.Writes()) + 1)
	worked = nil
	from = len(f.Writes())
	err := d.Iterate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "the assignees of o/r#2") {
		t.Errorf("Iterate with a failed assignee write = %v; want an error naming issue 2's assignees", err)
	}
	if got, want := assignWrites(f, from), []string{assignAdd(2, digestRunOwner), assignAdd(3, digestRunOwner)}; !slices.Equal(got, want) {
		t.Errorf("assignee writes = %q; want %q, the first failed", got, want)
	}
	if slices.Contains(worked, 2) || !slices.Contains(worked, 3) {
		t.Errorf("Work got %v; want 3 among them, and not 2, whose write failed", worked)
	}
	from = len(f.Requests())
	assignStep(t, f, d, "the pass after the failure", assignAdd(2, digestRunOwner))
	if !slices.ContainsFunc(f.Requests()[from:], func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/2" }) {
		t.Errorf("the pass after the failure didn't read 2 again: %+v", f.Requests()[from:])
	}
	assignStep(t, f, d, "a pass after")
}

// A notify user GitHub can't assign is asked for once: the add leaves him
// out, and no pass asks again, so he causes no write on every pass. A
// restart asks once more, with one add that changes nothing, and so does
// a change to notify. Assigned by hand all the same, he stays where an
// item wants him. The first pass warns of him once, and of no other add.
func TestAssignDropped(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logr.AddWriter).
	f, o := digestRunFake(t), newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+digestRunOwner+"\n")
	o.commit(config.Path, "types: labels\nnotify: ["+digestRunOwner+", ghost]\n")
	f.Unassignable("ghost")
	d := assignDaemon(t, f, o, nil)
	var warnings bytes.Buffer
	stop := logr.AddWriter(&warnings, logr.WithFilter(logr.W))
	assignStep(t, f, d, "the first pass", assignAdd(1, digestRunOwner, "ghost"), assignAdd(2, digestRunOwner), assignAdd(3, digestRunOwner),
		assignAdd(4, digestRunOwner), assignAdd(5, digestRunOwner), assignAdd(6, digestRunOwner))
	logr.Wait()
	stop()
	if got := warnings.String(); strings.Count(got, "GitHub left") != 1 || !strings.Contains(got, "GitHub left ghost out when assigning o/r#1") {
		t.Errorf("the first pass's warnings = %q; want one, of ghost on issue 1", got)
	}
	assignStep(t, f, d, "a second pass")
	assignStep(t, f, d, "a third pass")
	r := assignDaemon(t, f, o, nil)
	assignStep(t, f, r, "a restart", assignAdd(1, "ghost"))
	assignStep(t, f, r, "a pass after the restart")
	o.commit(config.Path, "types: labels\nnotify: [ghost, "+digestRunOwner+"]\n")
	assignStep(t, f, r, "a change to notify", assignAdd(1, "ghost"))
	assignStep(t, f, r, "a pass after the change")
	f.SetAssignees(2, digestRunOwner, "ghost")
	assignStep(t, f, r, "ghost assigned by hand")
	if got := assignees(f, 2); !slices.Equal(got, []string{"ghost", digestRunOwner}) {
		t.Errorf("issue 2's assignees = %q; want ghost kept beside mertenvg", got)
	}
}

// An issue that leaves the pipeline, as a flag /ignore answers, loses the
// notify users it was assigned, once: an assignee added by hand stays,
// and a later change is left alone.
func TestAssignLeavesPipeline(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), digestRunOrigin(t)
	d := assignDaemon(t, f, o, nil)
	iterate(t, d)
	f.AddComment(3, digestRunOwner, "/ignore")
	f.SetAssignees(3, digestRunOwner, "bob")
	assignStep(t, f, d, "the /ignore", assignRemove(3, digestRunOwner))
	if got := assignees(f, 3); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("issue 3's assignees = %q; want bob left", got)
	}
	f.SetAssignees(3, "bob", digestRunOwner)
	assignStep(t, f, d, "mertenvg assigned by hand after")
	assignStep(t, f, assignDaemon(t, f, o, nil), "a restart")
}

// assignReplayFake returns a setup for Replay: o/r with owner's issue 1 at
// clarifying with an unanswered round, assigned to round, and issue 2 at
// ready, assigned to ready.
func assignReplayFake(round, ready []string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := newFake(t)
		f.SetAssignees(digestRound(f), round...)
		f.SetAssignees(fileAt(f, "ready", "do it"), ready...)
		return f
	}
}

// assignReplayPass is a new daemon's pass over f, with notify owner.
func assignReplayPass(t *testing.T, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		d := newDaemon(t, f, o, nil)
		d.Assign = true
		return d.Iterate(t.Context())
	}
}

// Replay passes for an assign, an unassign and both, whatever write a
// crash comes at: the round's issue gets owner, and the ready issue loses
// owner, or, both, the round's issue loses mallory too. In the last, the
// round's issue may be transferred just as owner is added to it: the add
// gets a 301, which writes nothing, and is no error, and the issue isn't
// listed again (#199).
func TestAssignReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	for _, tc := range []struct {
		name         string
		round, ready []string
		moved        bool // the round's issue is transferred at owner's add
		want         []string
	}{
		{"an assign", nil, nil, false, []string{assignAdd(1, "owner")}},
		{"an unassign", []string{"owner"}, []string{"owner"}, false, []string{assignRemove(2, "owner")}},
		{"both", []string{"mallory"}, []string{"owner"}, false, []string{assignRemove(1, "mallory"), assignAdd(1, "owner"), assignRemove(2, "owner")}},
		{"both, transferred at the add", []string{"mallory"}, []string{"owner"}, true,
			[]string{assignRemove(1, "mallory"), assignAdd(1, "owner"), assignRemove(2, "owner")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := assignReplayFake(tc.round, tc.ready)
			if tc.moved {
				fill := setup
				setup = func(t testing.TB) *githubtest.Fake {
					f := fill(t)
					f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodPost && r.Path == "/issues/1/assignees" },
						func() { f.Transfer(1, "o/elsewhere") })
					return f
				}
			}
			clean := setup(t)
			if err := assignReplayPass(t, o)(clean); err != nil {
				t.Fatal(err)
			}
			if got := assignWrites(clean, 0); !slices.Equal(got, tc.want) {
				t.Errorf("the clean pass's assignee writes = %q; want %q", got, tc.want)
			}
			if a := clean.State().Issues[0].Assignees; tc.moved && (a != nil || !slices.Contains(assignAbout(clean, 0, 1), "POST /issues/1/assignees 301")) {
				t.Errorf("the clean pass left the transferred issue with %q, requests %q; want none assigned, and the add redirected", a, assignAbout(clean, 0, 1))
			}
			githubtest.Replay(t, setup, assignReplayPass(t, o))
		})
	}
}

// An issue read as closed, closed between the poll and its read, is left
// alone that pass, though it's at a stage, and the next poll drops it.
func TestAssignClosedInThePass(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), digestRunOrigin(t)
	d := assignDaemon(t, f, o, nil)
	iterate(t, d)
	f.SetAssignees(7, "bob")
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/7" },
		func() { f.CloseBy(7, digestRunOwner) })
	assignStep(t, f, d, "the pass that reads 7 closed")
	assignStep(t, f, d, "the next pass")
	if got := assignees(f, 7); !slices.Equal(got, []string{"bob"}) || !f.State().Issues[6].Closed {
		t.Errorf("issue 7's assignees = %q; want bob kept on the closed issue", got)
	}
}

// assignGoneFake is digestRunFake with issue 12, at ready, after the
// first pass, then bob assigned to it by hand.
func assignGoneFake(t *testing.T) (*githubtest.Fake, *daemon.Daemon) {
	t.Helper()
	f, o := digestRunFake(t), digestRunOrigin(t)
	d := assignDaemon(t, f, o, nil)
	if n := fileAt(f, "ready", "do it"); n != 12 {
		t.Fatalf("the issue at ready is %d; want 12", n)
	}
	iterate(t, d)
	f.SetAssignees(12, "bob")
	return f, d
}

// assignAbout is the requests from the from'th on that name issue n, at
// its own path or at the new home of a transfer, as "METHOD /path status".
func assignAbout(f *githubtest.Fake, from, n int) []string {
	about := regexp.MustCompile(fmt.Sprintf(`^(/repositories/[0-9]+)?/issues/%d(/|$)`, n))
	var got []string
	for _, r := range f.Requests()[from:] {
		if about.MatchString(r.Path) {
			got = append(got, fmt.Sprintf("%s %s %d", r.Method, r.Path, r.Status))
		}
	}
	return got
}

// assignGoneRead is the requests of a snapshot read that finds issue 12
// gone: deleted, a 404; transferred, a 301 and the GET of the new home,
// where the issue names another repo.
var assignGoneRead = map[string][]string{
	"deleted":     {"GET /issues/12 404"},
	"transferred": {"GET /issues/12 301", "GET /repositories/2/issues/12 200"},
}

// assignGoneReread is assignGoneRead's second read: the new home's
// answer hasn't changed, so the ETag saves it.
var assignGoneReread = map[string][]string{
	"deleted":     {"GET /issues/12 404"},
	"transferred": {"GET /issues/12 301", "GET /repositories/2/issues/12 304"},
}

// assignGoneHow makes issue 12 of f gone, by name, as assignGoneRead
// names them.
var assignGoneHow = map[string]func(f *githubtest.Fake){
	"deleted":     func(f *githubtest.Fake) { f.DeleteIssue() },
	"transferred": func(f *githubtest.Fake) { f.Transfer(12, "o/elsewhere") },
}

// assignWorked makes d's Work record the issues it gets in worked.
func assignWorked(d *daemon.Daemon, worked *[]int) {
	d.Work = func(_ context.Context, ready []daemon.Ready) error {
		for _, r := range ready {
			*worked = append(*worked, r.S.Number)
		}
		return nil
	}
}

// assignGonePasses runs the passes after issue 12 of f is found gone, or
// may be: each pass's requests about 12 must be the matching want, Work
// must never get 12, and nothing may be written about it, by assignment
// or anything else.
func assignGonePasses(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, want ...[]string) {
	t.Helper()
	var worked []int
	assignWorked(d, &worked)
	writes := len(f.Writes())
	for i, w := range want {
		from := len(f.Requests())
		iterate(t, d)
		if got := assignAbout(f, from, 12); !slices.Equal(got, w) {
			t.Errorf("pass %d after: requests about 12 = %q; want %q", i+1, got, w)
		}
	}
	if slices.Contains(worked, 12) {
		t.Errorf("Work got %v; want 12 left out", worked)
	}
	for _, w := range f.Writes()[writes:] {
		if strings.HasPrefix(w.Path, "/issues/12") {
			t.Errorf("wrote %s %s after 12 was found gone; want nothing", w.Method, w.Path)
		}
	}
}

// The snapshot read's own path: an issue gone, deleted or transferred,
// just before the poll's read of it, is read once more, then dropped.
// Work never gets it, nothing is written to it, its manual assignee
// included, and no later pass reads it (#199). A transferred issue's GET
// gets a 301, which Go follows to the new home, where the issue names
// another repo.
func TestAssignGoneRead(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"deleted", "transferred"} {
		t.Run(name, func(t *testing.T) {
			f, d := assignGoneFake(t) // bob's assignment makes the poll list 12
			f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/12" },
				func() { assignGoneHow[name](f) })
			assignGonePasses(t, f, d, assignGoneRead[name], assignGoneReread[name], nil, nil)
		})
	}
}

// A failed write, with an answer gone doesn't take as gone (a 500), makes
// the next pass read the issue again rather than write again (#199). If
// the issue is gone by then, deleted or transferred, that read finds it
// so, and the read after drops it: Work never gets it, and the write
// isn't made again, on that pass or later ones. A deleted issue's number
// filed again is an issue like any other, which the dropped one's memory
// doesn't follow: outside the pipeline, it keeps mertenvg, assigned by
// hand.
func TestAssignGone(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"deleted", "transferred"} {
		t.Run(name, func(t *testing.T) {
			f, d := assignGoneFake(t)
			var worked []int
			assignWorked(d, &worked)
			f.FailWrite(len(f.Writes()) + 1)
			from := len(f.Writes())
			if err := d.Iterate(t.Context()); err == nil || !strings.Contains(err.Error(), "the assignees of o/r#12, read again next pass") {
				t.Errorf("Iterate with a failed write = %v; want its error", err)
			}
			if slices.Contains(worked, 12) {
				t.Errorf("Work got %v in the pass whose write failed; want 12 left out", worked)
			}
			assignGoneHow[name](f)
			assignGonePasses(t, f, d, assignGoneRead[name], assignGoneReread[name], nil, nil)
			if got, want := assignWrites(f, from), []string{assignRemove(12, "bob")}; !slices.Equal(got, want) {
				t.Errorf("assignee writes = %q; want %q, the failed one alone", got, want)
			}
			if name != "deleted" {
				return
			}
			if n := f.AddIssue(digestRunEve, "Filed again", "An idea."); n != 12 {
				t.Fatalf("the new issue is %d; want 12", n)
			}
			f.SetAssignees(12, digestRunOwner)
			assignStep(t, f, d, "a new issue 12, outside the pipeline")
		})
	}
}

// A failed write to an issue that isn't gone is read again next pass,
// once, and written again; the read, which its ETag may save, is all it
// costs.
func TestAssignFailedRead(t *testing.T) {
	t.Parallel()
	f, d := assignGoneFake(t)
	f.FailWrite(len(f.Writes()) + 1)
	if err := d.Iterate(t.Context()); err == nil {
		t.Errorf("Iterate with a failed write = nil; want its error")
	}
	from := len(f.Requests())
	assignStep(t, f, d, "the pass after the failure", assignRemove(12, "bob"))
	reads := 0
	for _, r := range assignAbout(f, from, 12) {
		if strings.HasPrefix(r, "GET /issues/12 ") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the pass after the failure read 12 %d times: %q; want once", reads, assignAbout(f, from, 12))
	}
	assignStep(t, f, d, "a pass after")
}

// assignAddFake is digestRunFake with issue 12 at clarifying with an
// unanswered round, after the first pass assigned mertenvg to it, then
// unassigned by hand, so the next pass adds him again.
func assignAddFake(t *testing.T) (*githubtest.Fake, *daemon.Daemon) {
	t.Helper()
	f, o := digestRunFake(t), digestRunOrigin(t)
	d := assignDaemon(t, f, o, nil)
	if n := digestRound(f); n != 12 {
		t.Fatalf("the issue with a round is %d; want 12", n)
	}
	iterate(t, d)
	if got := assignees(f, 12); !slices.Equal(got, []string{digestRunOwner}) {
		t.Fatalf("issue 12's assignees after the first pass = %q; want mertenvg", got)
	}
	f.SetAssignees(12)
	return f, d
}

// An assignee write that finds the issue gone, deleted or transferred
// between its read and the write, is no error, whether it's an add or a
// remove: the write is made once, the issue isn't given to Work, the next
// pass reads it once more, which drops it, and no later pass reads it or
// writes to it again (#199). A transferred issue's write gets a 301,
// which Go follows with a GET of the new home's assignee path, a 404.
func TestAssignGoneWrite(t *testing.T) {
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
		for _, how := range []string{"deleted", "transferred"} {
			t.Run(write.name+" "+how, func(t *testing.T) {
				f, d := write.fake(t)
				var worked []int
				assignWorked(d, &worked)
				at := -1
				f.BeforeRequest(func(r githubtest.Request) bool {
					return r.Method == write.method && r.Path == "/issues/12/assignees"
				}, func() {
					at = len(f.Requests())
					assignGoneHow[how](f)
				})
				assignStep(t, f, d, "the pass that finds 12 gone", write.want)
				if slices.Contains(worked, 12) {
					t.Errorf("Work got %v in the pass that found 12 gone; want it left out", worked)
				}
				answer := map[string][]string{
					"deleted":     {write.method + " /issues/12/assignees 404"},
					"transferred": {write.method + " /issues/12/assignees 301", "GET /repositories/2/issues/12/assignees 404"},
				}[how]
				if at < 0 {
					t.Fatalf("the write never came")
				}
				if got := assignAbout(f, at, 12); !slices.Equal(got, answer) {
					t.Errorf("requests about 12 from the write on = %q; want %q", got, answer)
				}
				assignGonePasses(t, f, d, assignGoneRead[how], nil, nil)
			})
		}
	}
}

// A delete between an issue's read and its assignee write, in one pass,
// is no error either, and the next pass drops the issue with no write.
func TestAssignGoneInThePass(t *testing.T) {
	t.Parallel()
	f, d := assignGoneFake(t)
	f.BeforeRequest(func(r githubtest.Request) bool {
		return r.Method == http.MethodDelete && r.Path == "/issues/12/assignees"
	}, f.DeleteIssue)
	assignStep(t, f, d, "the pass that finds 12 gone", assignRemove(12, "bob"))
	assignStep(t, f, d, "the next pass")
	assignStep(t, f, d, "a pass after")
}

// An issue closed, then reopened outside the pipeline, is no longer one
// assignment kept: the close forgets that, so it keeps what was assigned
// by hand meanwhile, mertenvg too.
func TestAssignClosedForgets(t *testing.T) {
	t.Parallel()
	f, o := digestRunFake(t), digestRunOrigin(t)
	d := assignDaemon(t, f, o, nil)
	iterate(t, d)
	f.CloseBy(3, digestRunOwner)
	assignStep(t, f, d, "the close")
	f.DeleteComment(digestRunCommentID(t, f, 3, "<!-- issuedog:triage verdict=ask "))
	f.SetAssignees(3, digestRunOwner, "bob")
	f.ReopenBy(3, digestRunOwner)
	assignStep(t, f, d, "the reopen, with no flag left")
	if got := assignees(f, 3); !slices.Equal(got, []string{"bob", digestRunOwner}) {
		t.Errorf("issue 3's assignees = %q; want bob and mertenvg kept", got)
	}
}
