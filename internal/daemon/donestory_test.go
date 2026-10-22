package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// A finished story in the digest and in assignment against the fakes
// (#278, SPEC §9.4), with step 1 wired as main wires it: the daemon
// creates the children of a plan the owner approved, and the story waits
// on the owner once they've all closed.

// doneParent files issue 1, an owner's issue of type typ, by its type:*
// label, at planning, and comment 1001, its move there; then posts k's
// plan, revision 1, as comment 1002, with the planner's move into
// plan-review, the owner's 👍 on it, reaction 1003, and comment 1004, the App's move
// into planned that approves it, as ApproveByReaction posts it. So the
// daemon's first pass creates its children.
func doneParent(t testing.TB, f *githubtest.Fake, typ string, k childPlan) {
	t.Helper()
	if n := f.AddIssue("owner", "Quotes expire", "The story."); n != 1 {
		t.Fatalf("the parent is #%d; want #1", n)
	}
	f.AddLabel(1, "owner", "type:"+typ)
	f.AddLabel(1, app, "stage:planning")
	post(t, f, 1001, app, "<!-- issuedog:transition from=intake to=planning run=a -->")
	planOfK(t, f, k, state.PlanReview)
	hash, ok := state.PlanHash(f.State().Issues[0].Comments[1].Body)
	if !ok {
		t.Fatalf("comment 1002 is no plan revision: %q", f.State().Issues[0].Comments[1].Body)
	}
	r := f.AddReaction(1002, "owner", "+1")
	moveOn(t, f, 1, daemon.Move{To: state.Planned, Trigger: state.TriggerPlanApproved, Key: state.Key{Field: "cmd", ID: fmt.Sprintf("r%d", r)},
		Fields: map[string]string{"rev": "1", "body": hash, "seen": "1002"}}, 1004)
}

// storyTasks is a story planned into three tasks, T1 to T3, the last a
// human task, on a repo that uses type:* labels.
var storyTasks = childPlan{"Story", "story-plan", 3, false}

// doneStoryDaemon is a daemon of o/r on f that keeps the digest and the
// assignees, with notify [owner].
func doneStoryDaemon(t *testing.T, f *githubtest.Fake, o *origin) *daemon.Daemon {
	t.Helper()
	d := digestDaemon(t, f, o, nil)
	d.Assign = true
	return d
}

// doneListed is the finished story items in the body of the digest,
// issue n, as digestRunItems gives them.
func doneListed(f *githubtest.Fake, n int) []string {
	return slices.DeleteFunc(digestRunItems(f.State().Issues[n-1].Body), func(s string) bool { return !strings.Contains(s, " Finished story ") })
}

// doneUnchanged runs a pass of d, in which nothing changed, and fails t
// unless it wrote nothing and made only conditional GETs answered 304,
// which cost no rate limit.
func doneUnchanged(t *testing.T, f *githubtest.Fake, d *daemon.Daemon) {
	t.Helper()
	from, writes := len(f.Requests()), len(f.Writes())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a pass where nothing changed wrote %+v; want nothing", w)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
	}
}

// doneStep runs one Iterate of d, which must succeed, and checks that its
// assignee writes to issue 1, the story, are want. The human task the
// story's plan files, #4, is assigned and unassigned as #287 says, which
// isn't this test's to check.
func doneStep(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, step string, want ...string) {
	t.Helper()
	from := len(f.Writes())
	iterate(t, d)
	got := slices.DeleteFunc(assignWrites(f, from), func(w string) bool { return !strings.Contains(w, " /issues/1/") })
	if !slices.Equal(got, want) {
		t.Errorf("%s: the story's assignee writes %q; want %q", step, got, want)
	}
}

// doneDigest returns the digest's number, the last issue, and fails t if
// it isn't the digest.
func doneDigest(t *testing.T, f *githubtest.Fake) int {
	t.Helper()
	n := len(f.State().Issues)
	if !strings.Contains(f.State().Issues[n-1].Body, "<!-- issuedog:digest -->") {
		t.Fatalf("issue %d isn't the digest:\n%s", n, f.State().Issues[n-1].Body)
	}
	return n
}

// The whole of it. The first pass creates the story's children, 2 to 4,
// and the story isn't listed or assigned while any is open, the last
// closing included. The pass after the last child closes lists it, after
// every other item, announces it once, with its key, and assigns notify.
// A pass where nothing changed then costs only 304s, and a restart writes
// nothing. A child reopened takes it off and unassigns notify, and its
// close puts it back, with no second announcement. An open issue added
// below the story by hand, which its record doesn't name, takes it off
// too, till it closes. The story's close removes the item, and, as for
// every closed issue (SPEC §9.6), notify stays assigned on the closed
// story, out of the "is:open assignee:@me" view.
func TestDoneStory(t *testing.T) {
	t.Parallel()
	f, o := newFake(t), digestNotifyOrigin(t)
	doneParent(t, f, "story", storyTasks)
	d := doneStoryDaemon(t, f, o)
	doneStep(t, f, d, "the pass that creates the children")
	if got := f.State().Issues[0].SubIssues; !slices.Equal(got, []int{2, 3, 4}) {
		t.Fatalf("issue 1's sub-issues are %v; want its children, 2 to 4", got)
	}
	digestN := doneDigest(t, f)
	doneStep(t, f, d, "the pass that reads the issues written to again")
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("with every child open, the digest lists %q; want no finished story", got)
	}
	news := digestRunNews(f, digestN)

	f.CloseBy(2, "owner")
	f.CloseBy(3, "owner")
	doneStep(t, f, d, "two children closed")
	doneStep(t, f, d, "the pass after")
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("with #4 open, the digest lists %q; want no finished story", got)
	}
	f.CloseBy(4, "owner")
	doneStep(t, f, d, "the last child closed", assignAdd(1, "owner"))
	want := []string{"#1 Finished story p2 1"}
	if got := doneListed(f, digestN); !slices.Equal(got, want) {
		t.Errorf("after the last child closed, the digest lists %q; want %q", got, want)
	}
	body := f.State().Issues[digestN-1].Body
	for _, line := range []string{"- Its children: #2, #3, #4.", "- Answer here: https://github.com/o/r/issues/1\n"} {
		if !strings.Contains(body, line) {
			t.Errorf("the digest has no line %q:\n%s", line, body)
		}
	}
	if got := digestRunNews(f, digestN); got != news+1 {
		t.Errorf("the digest has %d digest-new comments, %d before; want one more", got, news)
	}
	if c := digestComments(f, digestN); !strings.Contains(c[len(c)-1], "- **Finished story** on #1: Quotes expire. Answer here: https://github.com/o/r/issues/1") ||
		!strings.Contains(c[len(c)-1], "<!-- issuedog:digest-new items=done-1 -->") {
		t.Errorf("the announcement is\n%s\nwant the finished story, keyed done-1", c[len(c)-1])
	}
	doneStep(t, f, d, "a pass that reads the closed children's judged closes")
	doneUnchanged(t, f, d)
	if w := digestPass(t, f, doneStoryDaemon(t, f, o)); len(w) != 0 {
		t.Errorf("a restart wrote %q; want nothing", w)
	}

	f.ReopenBy(4, "owner")
	doneStep(t, f, d, "a child reopened", assignRemove(1, "owner"))
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("after #4 reopened, the digest lists %q; want no finished story", got)
	}
	f.CloseBy(4, "owner")
	doneStep(t, f, d, "the child closed again", assignAdd(1, "owner"))
	if got := doneListed(f, digestN); !slices.Equal(got, want) {
		t.Errorf("after #4 closed again, the digest lists %q; want %q", got, want)
	}
	if got := digestRunNews(f, digestN); got != news+1 {
		t.Errorf("the digest has %d digest-new comments after it came back; want %d, no second announcement", got, news+1)
	}

	extra := f.AddIssue("owner", "One more thing", "Added by hand.")
	f.SetParent(extra, 1)
	doneStep(t, f, d, "an open issue added below the story", assignRemove(1, "owner"))
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("with #%d open below it, the digest lists %q; want no finished story", extra, got)
	}
	f.CloseBy(extra, "owner")
	doneStep(t, f, d, "the added issue closed", assignAdd(1, "owner"))
	if got := doneListed(f, digestN); !slices.Equal(got, want) {
		t.Errorf("after #%d closed, the digest lists %q; want %q", extra, got, want)
	}

	f.CloseBy(1, "owner")
	doneStep(t, f, d, "the story closed")
	if got := doneListed(f, digestN); len(got) != 0 {
		t.Errorf("after the story closed, the digest lists %q; want nothing", got)
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("the closed story's assignees = %q; want owner left, as on any closed issue", got)
	}
}

// An epic planned into tasks acts as its own story (SPEC §4): once its
// tasks have all closed it's listed and assigned the same way. An epic
// planned into stories isn't, once its stories have closed: each story
// is verified on its own.
func TestDoneStoryEpic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		k        childPlan
		children []int
		want     []string
	}{
		{"planned into tasks", childPlan{"Epic", "story-plan", 3, false}, []int{2, 3, 4}, []string{"#1 Finished story p2 1"}},
		{"planned into stories", twoStories, []int{2, 3}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), digestNotifyOrigin(t)
			doneParent(t, f, "epic", tc.k)
			d := doneStoryDaemon(t, f, o)
			iterate(t, d)
			if got := f.State().Issues[0].SubIssues; !slices.Equal(got, tc.children) {
				t.Fatalf("the epic's sub-issues are %v; want its children, %v", got, tc.children)
			}
			digestN := doneDigest(t, f)
			iterate(t, d)
			for _, c := range tc.children {
				f.CloseBy(c, "owner")
			}
			iterate(t, d)
			if got := doneListed(f, digestN); !slices.Equal(got, tc.want) {
				t.Errorf("with every child closed, the digest lists %q; want %q", got, tc.want)
			}
			wantAssigned := []string(nil)
			if tc.want != nil {
				wantAssigned = []string{"owner"}
			}
			if got := assignees(f, 1); !slices.Equal(got, wantAssigned) {
				t.Errorf("the epic's assignees = %q; want %q", got, wantAssigned)
			}
		})
	}
}

// doneReplayFake returns a setup for Replay: o/r with issue 1 a story at
// planned with storyTasks' plan, assigned to assigned, and children 2 to
// 4, filed and linked under it, closed but for open, each closed one with
// stage:completed, as the close judge leaves an accepted close, with the
// App's record of them on issue 1, as comment 1005, which a visit then
// takes as every child written.
func doneReplayFake(open int, assigned []string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := newFake(t)
		doneParent(t, f, "story", storyTasks)
		for i := range 3 {
			n := f.AddIssue(app, fmt.Sprintf("Task %d", i+1), fmt.Sprintf("A task.\n\n<!-- issuedog:origin plan=#1 rev=1 item=T%d -->", i+1))
			f.SetParent(n, 1)
			if n != open {
				f.CloseBy(n, "owner")
				f.AddLabel(n, app, "stage:completed")
			}
		}
		post(t, f, 1005, app, wantRecord(storyTasks))
		f.SetAssignees(1, assigned...)
		f.ForgetWrites()
		return f
	}
}

// Replay passes for the assign of a story whose children have all closed,
// and the unassign of one whose child is open again, whatever write a
// crash comes at.
func TestDoneStoryAssignReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	for _, tc := range []struct {
		name     string
		open     int
		assigned []string
		want     []string
	}{
		{"an assign", 0, nil, []string{assignAdd(1, "owner")}},
		{"an unassign", 3, []string{"owner"}, []string{assignRemove(1, "owner")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := doneReplayFake(tc.open, tc.assigned)
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
