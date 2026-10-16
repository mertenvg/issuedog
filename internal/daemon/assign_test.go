package daemon_test

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
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
// rest of the issues, and Work, carry on; the next pass writes it again.
func TestAssignFailures(t *testing.T) {
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
	if !slices.Contains(worked, 2) || !slices.Contains(worked, 3) {
		t.Errorf("Work got %v; want 2 and 3 among them", worked)
	}
	assignStep(t, f, d, "the pass after the failure", assignAdd(2, digestRunOwner))
	assignStep(t, f, d, "a pass after")
}

// A notify user GitHub can't assign is asked for once: the add leaves him
// out, and no pass asks again, so he causes no write on every pass. A
// restart asks once more, with one add that changes nothing, and so does
// a change to notify. Assigned by hand all the same, he stays where an
// item wants him.
func TestAssignDropped(t *testing.T) {
	f, o := digestRunFake(t), newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+digestRunOwner+"\n")
	o.commit(config.Path, "types: labels\nnotify: ["+digestRunOwner+", ghost]\n")
	f.Unassignable("ghost")
	d := assignDaemon(t, f, o, nil)
	assignStep(t, f, d, "the first pass", assignAdd(1, digestRunOwner, "ghost"), assignAdd(2, digestRunOwner), assignAdd(3, digestRunOwner),
		assignAdd(4, digestRunOwner), assignAdd(5, digestRunOwner), assignAdd(6, digestRunOwner))
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
// owner, or, both, the round's issue loses mallory too.
func TestAssignReplay(t *testing.T) {
	o := digestNotifyOrigin(t)
	for _, tc := range []struct {
		name         string
		round, ready []string
		want         []string
	}{
		{"an assign", nil, nil, []string{assignAdd(1, "owner")}},
		{"an unassign", []string{"owner"}, []string{"owner"}, []string{assignRemove(2, "owner")}},
		{"both", []string{"mallory"}, []string{"owner"}, []string{assignRemove(1, "mallory"), assignAdd(1, "owner"), assignRemove(2, "owner")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := assignReplayFake(tc.round, tc.ready)
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
