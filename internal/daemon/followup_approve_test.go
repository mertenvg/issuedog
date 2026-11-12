package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A follow-up waits at intake until a trusted 👍 on it approves it (SPEC
// §6.2, §10.2). Nothing files one yet, so each test files it as the App
// will: an App-authored task at stage:intake, with priority:p3, whose body
// ends in its origin marker, naming verify #7's run run3 and its first
// candidate.

// approvalOrigin is the follow-up's origin marker, as the App writes it.
const approvalOrigin = "<!-- issuedog:origin verify=#7 run=run3 item=F1 -->"

// approvalBody is the follow-up's body as the App files it, its origin last.
const approvalBody = "**Scenario:** A quote that expires mid-booking is booked at its old price.\n" +
	"**Likelihood:** rare\n**Impact:** one booking at a stale price\n\n" +
	"It waits for your approval: react 👍 to approve it, or close it to drop it.\n\ncc @owner\n\n" + approvalOrigin

// approvalFiled is newFake with issue 1 a follow-up the App filed, a task
// at stage:intake with priority:p3. Then it runs more.
func approvalFiled(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		f.AddIssue(app, "Refuse a quote that expires mid-booking", approvalBody)
		f.SetType(1, "task")
		f.AddLabel(1, app, "priority:p3")
		f.AddLabel(1, app, "stage:intake")
		more(t, f)
		return f
	}
}

// approvalDaemon is newDaemon with the /track and /ignore handlers, and
// the follow-up's 👍 trigger as its only reaction trigger, trusting
// handbackTrust, which trusts the App and a helper bot too, so only their
// being bots leaves them out.
func approvalDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	d := newDaemon(t, f, o, nil)
	for name, h := range daemon.FlagCommands() {
		d.Step.Commands[name] = h
	}
	d.Step.Reactions = daemon.FollowUpByReaction(handbackTrust)
	return d
}

// approvalPass is a fresh approvalDaemon's pass over f, as after a
// restart.
func approvalPass(t testing.TB, o *origin) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error { return approvalDaemon(t, f, o).Iterate(context.Background()) }
}

// approvalMove is the App's comment approving the follow-up on who's
// 👍 r, who as the comment quotes it, with seen, or none if it's empty.
func approvalMove(who string, r int64, seen string) string {
	if seen != "" {
		seen = " seen=" + seen
	}
	return fmt.Sprintf("`%s`'s 👍 on this follow-up approves it, so it goes to `ready`.\n\n"+
		"<!-- issuedog:transition from=intake to=ready cmd=r%d%s -->", who, r, seen)
}

// approvalState checks issue 1's labels and its comments, all of them.
func approvalState(t *testing.T, f *githubtest.Fake, labels, comments []string) {
	t.Helper()
	is := f.State().Issues[0]
	if got := closingComments(f, 1, 0); !slices.Equal(is.Labels, labels) || is.Closed || !slices.Equal(got, comments) {
		t.Errorf("labels %q, closed %t and comments\n%q\nwant %q, open and\n%q", is.Labels, is.Closed, got, labels, comments)
	}
}

// The earliest trusted 👍 on a follow-up, not a bot's, approves it: it
// moves intake → ready, keyed by the 👍, with seen the newest comment read,
// here owner's, and its comment names the 👍's user, escaped and defused.
// eve's 👍, untrusted, the helper bot's and the App's, both trusted, and
// owner's ❤️ count for nothing, nor does owner's later 👍. The App writes
// no reaction, and the pass after writes nothing.
func TestFollowUpApprovedByThumb(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		if id := f.AddComment(1, "owner", "Worth doing, after the release."); id != 1001 {
			t.Fatalf("owner's comment ID = %d, want 1001", id)
		}
		f.AddIssueReaction(1, "eve", "+1")
		f.AddIssueReaction(1, "helper[bot]", "+1")
		f.AddIssueReaction(1, app, "+1")
		f.AddIssueReaction(1, "owner", "heart")
		if id := f.AddIssueReaction(1, "owner2", "+1"); id != 1006 {
			t.Fatalf("owner2's 👍's ID = %d, want 1006", id)
		}
		f.AddIssueReaction(1, "owner", "+1")
	})
	githubtest.Replay(t, setup, approvalPass(t, o))

	f := setup(t)
	d := approvalDaemon(t, f, o)
	iterate(t, d)
	approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{"Worth doing, after the release.", approvalMove("owner2", 1006, "1001")})
	if got, want := writesOf(f), []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:intake"}; !slices.Equal(got, want) {
		t.Errorf("writes %q, want %q", got, want)
	}
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the pass after wrote %+v, want nothing", got)
	}
}

// With no comment read, as on a follow-up nobody has commented on, the
// approval's move has no seen: the next round's input is what follows its
// own comment (SPEC §5, RoundBound).
func TestFollowUpApprovedNoComment(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		if id := f.AddIssueReaction(1, "owner", "+1"); id != 1001 {
			t.Fatalf("owner's 👍's ID = %d, want 1001", id)
		}
	})(t)
	iterate(t, approvalDaemon(t, f, newOrigin(t)))
	approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{approvalMove("owner", 1001, "")})
}

// 👍s that approve nothing write nothing: an untrusted user's, the helper
// bot's and the App's, though trust trusts both, a trusted ❤️, a trusted
// 👍 on owner's comment rather than on the issue, and a trusted 👍 that a
// marker of the App's names already. The issue's reactions are read once
// a pass. A trusted 👍 on the issue then approves it.
func TestFollowUpThumbsIgnored(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		c := f.AddComment(1, "owner", "Looks rare enough.")
		f.AddReaction(c, "owner", "+1")
		for _, login := range []string{"eve", "helper[bot]", app} {
			f.AddIssueReaction(1, login, "+1")
		}
		f.AddIssueReaction(1, "owner", "heart")
		named := f.AddIssueReaction(1, "owner2", "+1")
		f.AddComment(1, app, fmt.Sprintf("<!-- issuedog:reply reason=rejected cmd=r%d -->", named))
	})(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if n := thumbReads(f, 0); n != 2 {
		t.Errorf("read the follow-up's reactions %d times, want once a pass, 2", n)
	}
	r := f.AddIssueReaction(1, "owner", "+1")
	iterate(t, d)
	if last := f.State().Issues[0].Comments; !strings.Contains(last[len(last)-1].Body, fmt.Sprintf("from=intake to=ready cmd=r%d ", r)) {
		t.Errorf("the newest comment = %q; want the move to ready on owner's 👍", last[len(last)-1].Body)
	}
}

// A 👍 given while the follow-up is held waits: nothing reads it. Once
// /resume returns it to intake, the 👍 approves it in the same pass.
func TestFollowUpThumbWhileHeld(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) { f.AddComment(1, "owner", "/hold") })(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	r := f.AddIssueReaction(1, "owner", "+1")
	iterate(t, d)
	if is := f.State().Issues[0]; !slices.Equal(is.Labels, []string{"priority:p3", "stage:held"}) {
		t.Fatalf("labels %q; want held", is.Labels)
	}
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the held follow-up's reactions %d times, want none", n)
	}
	f.AddComment(1, "owner", "/resume")
	iterate(t, d)
	is := f.State().Issues[0]
	if last := is.Comments[len(is.Comments)-1].Body; !slices.Equal(is.Labels, []string{"priority:p3", "stage:ready"}) ||
		!strings.Contains(last, fmt.Sprintf("from=intake to=ready cmd=r%d ", r)) {
		t.Errorf("labels %q, newest comment %q; want ready, on the 👍 that waited", is.Labels, last)
	}
}

// approved, added by a trusted user, admits no follow-up: it stays at
// intake, with nothing written, pass after pass, and the "issue is
// trusted" row doesn't move it, as its author is the App.
func TestFollowUpApprovedLabel(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) { f.AddLabel(1, "owner", "approved") })(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	approvalState(t, f, []string{"approved", "priority:p3", "stage:intake"}, nil)
}

// /track on a follow-up is rejected, with the App's 👎 and one reply
// saying how to approve it or drop it, and it stays at intake.
func TestFollowUpTrack(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	setup := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		if id := f.AddComment(1, "owner", "/track"); id != 1001 {
			t.Fatalf("/track's comment ID = %d, want 1001", id)
		}
	})
	githubtest.Replay(t, setup, approvalPass(t, o))

	f := run(t, setup, approvalPass(t, o))
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, []string{"/track",
		"> /track\n\nI didn't apply this command: this is a follow-up, which `/track` doesn't admit. " +
			"React 👍 to it to approve it, or close it to drop it.\n\n<!-- issuedog:reply reason=rejected cmd=1001 -->"})
	if r := f.State().Issues[0].Comments[0].Reactions; !slices.Equal(r, []string{app + " -1"}) {
		t.Errorf("/track's reactions = %q, want the App's 👎", r)
	}
}

// An edit by someone else that hides the follow-up's origin, here the
// owner's, which is trusted, so there's no hold, gets the marker reposted
// in an App comment, worded for a follow-up, once (SPEC §6.4), its fields
// in the order the App writes a marker's (section 5). The issue
// stays a follow-up: a trusted 👍 still approves it.
func TestFollowUpOriginReposted(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		f.EditBody(1, "owner", "A quote that expires mid-booking is booked at its old price.")
	})(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	repost := "This issue's description was edited, so I've copied its `origin` marker here to keep tracking it as a follow-up.\n\n" +
		"<!-- issuedog:origin item=F1 run=run3 verify=#7 -->"
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, []string{repost})
	writes := len(f.Writes())
	iterate(t, d)
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the pass after wrote %+v, want nothing", got)
	}
	r := f.AddIssueReaction(1, "owner", "+1")
	iterate(t, d)
	approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{repost, approvalMove("owner", r, "")})
}

// A person's issue with a follow-up's origin marker is no follow-up: its
// marker isn't the App's. It's a task the owner filed, which the "issue is
// trusted" row moves to clarifying, and its reactions are never read.
func TestFollowUpPersonsIssue(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue("owner", "Refuse a quote that expires mid-booking", approvalBody)
	f.SetType(1, "task")
	f.AddLabel(1, "owner", "stage:intake")
	f.AddIssueReaction(1, "owner", "+1")
	iterate(t, approvalDaemon(t, f, newOrigin(t)))
	is := f.State().Issues[0]
	if len(is.Comments) != 1 || !strings.Contains(is.Comments[0].Body, "<!-- issuedog:transition from=intake to=clarifying event=") ||
		!slices.Equal(is.Labels, []string{"stage:clarifying"}) {
		t.Errorf("labels %q, comments %q; want the trusted row's move to clarifying", is.Labels, closingComments(f, 1, 0))
	}
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the person's issue's reactions %d times, want none", n)
	}
}

// A read of the follow-up's reactions that fails fails the pass, with
// nothing written, and the next pass tries again.
func TestFollowUpReactionsReadFails(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) { f.AddIssueReaction(1, "owner", "+1") })(t)
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/issues/1/reactions" }, http.StatusBadGateway)
	d := approvalDaemon(t, f, newOrigin(t))
	if err := d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), "follow-up approval by 👍 on o/r#1: ") {
		t.Fatalf("Iterate: %v, want the failed read's error, from the 👍 trigger", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Fatalf("writes %+v, want none", w)
	}
	iterate(t, d)
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"priority:p3", "stage:ready"}) {
		t.Errorf("labels %q after the next pass; want ready", l)
	}
}

// With step 1 as NewStep builds it, every 👍 trigger wired, a trusted 👍
// on a follow-up approves it.
func TestFollowUpNewStep(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) { f.AddIssueReaction(1, "owner", "+1") })(t)
	iterate(t, newStepDaemon(t, f))
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"priority:p3", "stage:ready"}) {
		t.Errorf("the follow-up's labels are %q after a 👍 on it; want ready", l)
	}
}

// A whole run on the daemon main builds (rig.wireAsMain), with triage as
// its Unlabeled stage and every stage registered: a follow-up waiting at
// intake starts no run of any stage, triage's included, and nothing moves
// it. A pass where nothing changed sends only conditional GETs answered
// 304: the poll, the visit's own read of its timeline, as any visited
// issue's, and one read for a 👍, its reactions. Neither mallory's issue
// at intake, which waits for approved, nor a follow-up owner closed as not
// planned is visited for it. Then owner adds approved and posts
// /track: /track gets its one reply, still no run starts, and nothing
// moves it.
func TestFollowUpWaitsRun(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		f.AddLabel(f.AddIssue("mallory", "Cache quotes", "Cache them."), "owner", "stage:intake")
		closed := f.AddIssue(app, "Refuse a quote with no price", "Refuse it.\n\n<!-- issuedog:origin verify=#7 run=run3 item=F2 -->")
		f.SetType(closed, "task")
		f.AddLabel(closed, app, "stage:intake")
		f.CloseAs(closed, "owner", "not_planned")
	})(t)
	g := newRig(t, f, newOrigin(t), t.TempDir())
	if err := os.WriteFile(g.path("claude", "claude"), []byte(wholeRunClaude), 0o755); err != nil {
		t.Fatal(err)
	}
	g.wireAsMain(g.d.Config)
	for range 2 {
		iterate(t, g.d)
	}
	if n := g.runs(); n != 0 {
		t.Errorf("%d claude runs; want none, of triage or any stage", n)
	}
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, nil)
	from := len(f.Requests())
	iterate(t, g.d)
	// Every request, whatever its status, is counted.
	var sent []string
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
		sent = append(sent, r.Method+" "+r.Path)
	}
	if want := []string{"GET /issues", "GET /issues/comments", "GET /issues/1/timeline", "GET /issues/1/reactions"}; !slices.Equal(sent, want) {
		t.Errorf("a pass where nothing changed sent %q; want %q", sent, want)
	}
	f.AddLabel(1, "owner", "approved")
	if id := f.AddComment(1, "owner", "/track"); id != 1001 {
		t.Fatalf("/track's comment ID = %d, want 1001", id)
	}
	for range 2 {
		iterate(t, g.d)
	}
	if n := g.runs(); n != 0 {
		t.Errorf("%d claude runs after approved and /track; want none", n)
	}
	approvalState(t, f, []string{"approved", "priority:p3", "stage:intake"}, []string{"/track",
		"> /track\n\nI didn't apply this command: this is a follow-up, which `/track` doesn't admit. " +
			"React 👍 to it to approve it, or close it to drop it.\n\n<!-- issuedog:reply reason=rejected cmd=1001 -->"})
}

// A plan's task the App filed, at intake before its first move, as after a
// crash between filing it and its move (SPEC §5.2), is no follow-up, though
// it's in the same class and visited on every pass: a 👍 on it approves
// nothing, and its reactions aren't read. Nothing is written.
func TestFollowUpPlanTaskAtIntake(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	f.AddIssue(app, "Refuse an expired quote", "Refuse it.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T1 -->")
	f.SetType(1, "task")
	f.AddLabel(1, app, "stage:intake")
	f.AddIssueReaction(1, "owner", "+1")
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the plan task's reactions %d times, want none", n)
	}
}

// A follow-up someone retyped as a bug isn't a task the daemon filed any
// more, so a 👍 on it approves nothing, and its reactions aren't read: the
// type check (SPEC §5.3) is what speaks to a type change. Nothing is
// written, and it stays at intake.
func TestFollowUpRetyped(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "bug")
		f.AddIssueReaction(1, "owner", "+1")
	})(t)
	d := approvalDaemon(t, f, newOrigin(t))
	iterate(t, d)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v, want none", w)
	}
	if n := thumbReads(f, 0); n != 0 {
		t.Errorf("read the retyped follow-up's reactions %d times, want none", n)
	}
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, nil)
}
