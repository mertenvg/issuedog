package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A follow-up waiting at intake for its approval in the digest and in
// assignment against the fakes (SPEC §9.4, §9.6, §10.2), through the
// digest's whole pass, with the follow-up's 👍 trigger wired as main wires
// it. Each follow-up is filed as the App files it (approvalFiled,
// storyApproval): its body ends in its cc line for owner, then its origin
// marker, naming verify #7.

// followUpDigestDaemon is approvalDaemon with the digest and assignment on.
func followUpDigestDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	d := approvalDaemon(t, f, o)
	d.Digest, d.Assign = true, true
	return d
}

// followUpDigestListed is the follow-up items in the body of the digest,
// issue n, as digestRunItems gives them.
func followUpDigestListed(f *githubtest.Fake, n int) []string {
	return slices.DeleteFunc(digestRunItems(f.State().Issues[n-1].Body), func(s string) bool { return !strings.Contains(s, " Follow-up ") })
}

// followUpDigestItem is follow-up #1's item as the digest renders it, with
// how to approve it, approve, and the link to answer at.
func followUpDigestItem(approve, link string) string {
	return "**Follow-up**, priority p3, unblocks 1 open issue (itself included)\n\n" +
		"- A follow-up the verifier found on #7 waits for your approval.\n" +
		"- " + approve + "\n" +
		"- Close the issue to drop it.\n" +
		"- Answer here: " + link + "\n"
}

// followUpDigestOnIssue is the line of an item that names only a 👍 on the
// issue, for a follow-up with no clean addendum.
const followUpDigestOnIssue = "React 👍 to the issue to approve it, and it goes to `ready`."

// followUpDigestQuiet runs a pass of d, in which nothing changed, and
// fails t unless it wrote nothing and sent want, every request counted,
// whatever its answer, each a conditional GET answered 304: the poll, the
// visit's read of the follow-up's timeline, as any visited issue's, and
// the reads for a 👍 (FollowUpByReaction), but none for the digest or for
// an issue's assignees.
func followUpDigestQuiet(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, want ...string) {
	t.Helper()
	from, writes := len(f.Requests()), len(f.Writes())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("a pass where nothing changed wrote %+v; want nothing", w)
	}
	var sent []string
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("a pass where nothing changed sent %+v; want only conditional GETs answered 304", r)
		}
		sent = append(sent, r.Method+" "+r.Path)
	}
	if !slices.Equal(sent, want) {
		t.Errorf("a pass where nothing changed sent %q; want %q", sent, want)
	}
}

// A follow-up with no parent, waiting at intake, is a digest item,
// followup-1: it names verify #7, says a 👍 on the issue approves it and
// closing drops it, and links the issue. It isn't announced, as its body's
// cc line mentioned notify, and notify is assigned to it. A pass where
// nothing changed sends only 304s, none for the item or its assignees, and
// a restart writes nothing. Then owner's 👍 approves it, and the same pass
// takes it off the digest and unassigns notify.
func TestFollowUpDigestItem(t *testing.T) {
	t.Parallel()
	f, o := approvalFiled(func(testing.TB, *githubtest.Fake) {})(t), digestNotifyOrigin(t)
	d := followUpDigestDaemon(t, f, o)
	iterate(t, d)
	digestN := doneDigest(t, f)
	if got, want := followUpDigestListed(f, digestN), []string{"#1 Follow-up p3 1"}; !slices.Equal(got, want) {
		t.Errorf("the digest lists %q; want %q", got, want)
	}
	if body, want := f.State().Issues[digestN-1].Body, followUpDigestItem(followUpDigestOnIssue, "https://github.com/o/r/issues/1"); !strings.Contains(body, want) {
		t.Errorf("the digest =\n%s\nwant the item\n%s", body, want)
	}
	if n := digestRunNews(f, digestN); n != 0 {
		t.Errorf("the digest has %d digest-new comments; want none, as the follow-up's own body mentions notify", n)
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("the follow-up's assignees = %q; want owner", got)
	}
	assignStep(t, f, d, "the pass that reads the issues written to again")
	followUpDigestQuiet(t, f, d, "GET /issues", "GET /issues/comments", "GET /issues/1/timeline", "GET /issues/1/reactions")
	if w := digestPass(t, f, followUpDigestDaemon(t, f, o)); len(w) != 0 {
		t.Errorf("a restart wrote %q; want nothing", w)
	}

	thumb := f.AddIssueReaction(1, "owner", "+1")
	from := len(f.Writes())
	iterate(t, d)
	want := []string{"POST /issues/1/comments", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:intake",
		fmt.Sprintf("PATCH /issues/%d", digestN), "DELETE /issues/1/assignees"}
	if got := writesOf(f)[from:]; !slices.Equal(got, want) {
		t.Errorf("the approving pass wrote %q; want %q", got, want)
	}
	approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{approvalMove("owner", thumb, "")})
	if got := followUpDigestListed(f, digestN); len(got) != 0 {
		t.Errorf("after the approval, the digest lists %q; want no follow-up", got)
	}
	if got := assignees(f, 1); len(got) != 0 {
		t.Errorf("after the approval, the follow-up's assignees = %q; want none", got)
	}
}

// A follow-up in a story, #1 under story #2, links its clean addendum on
// the story, and says a 👍 there or on the issue approves it. A pass where
// nothing changed reads the addendum's reactions too, a 304, and nothing
// for the item or its assignees. Owner's 👍 on the addendum approves it,
// and the same pass takes it off the digest and unassigns notify.
func TestFollowUpDigestStory(t *testing.T) {
	t.Parallel()
	f, o := storyApproval(func(testing.TB, *githubtest.Fake) {})(t), digestNotifyOrigin(t)
	d := followUpDigestDaemon(t, f, o)
	iterate(t, d)
	digestN := doneDigest(t, f)
	if got, want := followUpDigestListed(f, digestN), []string{"#1 Follow-up p3 1"}; !slices.Equal(got, want) {
		t.Errorf("the digest lists %q; want %q", got, want)
	}
	item := followUpDigestItem("React 👍 to its addendum on #2, or to the issue, to approve it, and it goes to `ready`.",
		"https://github.com/o/r/issues/2#issuecomment-1001")
	if body := f.State().Issues[digestN-1].Body; !strings.Contains(body, item) {
		t.Errorf("the digest =\n%s\nwant the item\n%s", body, item)
	}
	if n := digestRunNews(f, digestN); n != 0 {
		t.Errorf("the digest has %d digest-new comments; want none", n)
	}
	assignStep(t, f, d, "the pass that reads the issues written to again")
	followUpDigestQuiet(t, f, d, "GET /issues", "GET /issues/comments", "GET /issues/1/timeline", "GET /issues/1/reactions",
		"GET /issues/comments/1001/reactions")

	thumb := f.AddReaction(1001, "owner", "+1")
	from := len(f.Writes())
	iterate(t, d)
	approvalState(t, f, []string{"priority:p3", "stage:ready"}, []string{storyApproved("owner", thumb)})
	if got, want := assignWrites(f, from), []string{assignRemove(1, "owner")}; !slices.Equal(got, want) {
		t.Errorf("the approving pass's assignee writes %q; want %q", got, want)
	}
	if got := followUpDigestListed(f, digestN); len(got) != 0 {
		t.Errorf("after the approval, the digest lists %q; want no follow-up", got)
	}
}

// A follow-up in a story whose addendum someone else edited, which counts
// for nothing (SPEC §5.1), links the issue, and names only a 👍 there.
func TestFollowUpDigestEditedAddendum(t *testing.T) {
	t.Parallel()
	f := storyApproval(func(t testing.TB, f *githubtest.Fake) {
		f.EditComment(1001, "owner", "Edited.\n\n<!-- issuedog:addendum child=1 -->")
	})(t)
	iterate(t, followUpDigestDaemon(t, f, digestNotifyOrigin(t)))
	digestN := doneDigest(t, f)
	if body, want := f.State().Issues[digestN-1].Body, followUpDigestItem(followUpDigestOnIssue, "https://github.com/o/r/issues/1"); !strings.Contains(body, want) {
		t.Errorf("the digest =\n%s\nwant the item\n%s", body, want)
	}
}

// A follow-up ranks after an approval and before a question round (SPEC
// §9.4, The order), whatever their priorities: each unblocks only itself.
func TestFollowUpDigestRank(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(t testing.TB, f *githubtest.Fake) {
		f.AddLabel(digestRound(f), app, "priority:p0")
		fileAt(f, "spec-review", "The spec.", "priority:p3")
	})(t)
	iterate(t, followUpDigestDaemon(t, f, digestNotifyOrigin(t)))
	digestN := doneDigest(t, f)
	want := []string{"#3 Approval p3 1", "#1 Follow-up p3 1", "#2 Question round p0 1"}
	if got := digestRunItems(f.State().Issues[digestN-1].Body); !slices.Equal(got, want) {
		t.Errorf("the digest lists %q; want %q", got, want)
	}
}

// A trusted close drops a follow-up (SPEC §10.2), whatever its reason: the
// pass that judges it takes notify off it, before the closed stage, and
// takes it off the digest. Nobody else assigned is taken off.
func TestFollowUpDigestClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reason, stage string
	}{
		{"not_planned", "stage:discarded"},
		{"completed", "stage:completed"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			t.Parallel()
			f := approvalFiled(func(testing.TB, *githubtest.Fake) {})(t)
			d := followUpDigestDaemon(t, f, digestNotifyOrigin(t))
			iterate(t, d)
			digestN := doneDigest(t, f)
			iterate(t, d)
			// mallory, assigned by hand just before the close, which no
			// pass judges while the follow-up is open.
			f.SetAssignees(1, "owner", "mallory")
			f.CloseAs(1, "owner", tc.reason)
			from := len(f.Writes())
			iterate(t, d)
			want := []string{"DELETE /issues/1/assignees", "POST /issues/1/labels", "DELETE /issues/1/labels/stage:intake", fmt.Sprintf("PATCH /issues/%d", digestN)}
			if got := writesOf(f)[from:]; !slices.Equal(got, want) {
				t.Errorf("the pass that judges the close wrote %q; want %q", got, want)
			}
			if got, want := assignWrites(f, from), []string{assignRemove(1, "owner")}; !slices.Equal(got, want) {
				t.Errorf("the assignee writes %q; want %q", got, want)
			}
			is := f.State().Issues[0]
			if !is.Closed || !slices.Equal(is.Labels, []string{"priority:p3", tc.stage}) || !slices.Equal(is.Assignees, []string{"mallory"}) {
				t.Errorf("the follow-up is closed %t, with labels %q and assignees %q; want closed, at %s, with mallory alone", is.Closed, is.Labels, is.Assignees, tc.stage)
			}
			if got := followUpDigestListed(f, digestN); len(got) != 0 {
				t.Errorf("after the close, the digest lists %q; want no follow-up", got)
			}
			assignStep(t, f, d, "the pass after the close")
		})
	}
}

// An untrusted close is reverted, as for any issue (SPEC §6.4): the
// follow-up is reopened, with the App's comment, still listed and still
// assigned, with no assignee write.
func TestFollowUpDigestUntrustedClose(t *testing.T) {
	t.Parallel()
	f := approvalFiled(func(testing.TB, *githubtest.Fake) {})(t)
	d := followUpDigestDaemon(t, f, digestNotifyOrigin(t))
	iterate(t, d)
	digestN := doneDigest(t, f)
	iterate(t, d)
	before := f.State().Issues[digestN-1].Body
	event := f.CloseBy(1, "eve")
	from := len(f.Writes())
	iterate(t, d)
	iterate(t, d)
	if got := assignWrites(f, from); len(got) != 0 {
		t.Errorf("after eve's close, the assignee writes %q; want none", got)
	}
	approvalState(t, f, []string{"priority:p3", "stage:intake"}, []string{closingReverted("`eve`", event)})
	if got := assignees(f, 1); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("after eve's close, the follow-up's assignees = %q; want owner", got)
	}
	if got := f.State().Issues[digestN-1].Body; got != before {
		t.Errorf("after eve's close, the digest =\n%s\nwant it as before\n%s", got, before)
	}
}

// followUpDigestReplayFake returns a setup for Replay: o/r with the pinned
// digest, #1, listing nothing, then follow-up #2, as the App files it,
// assigned to assigned, then more.
func followUpDigestReplayFake(assigned []string, more func(f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		t.Helper()
		f := digestPinned(t)
		n := f.AddIssue(app, "Refuse a quote that expires mid-booking", approvalBody)
		f.SetType(n, "task")
		f.AddLabel(n, app, "priority:p3")
		f.AddLabel(n, app, "stage:intake")
		f.SetAssignees(n, assigned...)
		more(f)
		return f
	}
}

// Replay passes for the digest's and the assignees' writes, whatever write
// a crash comes at: a follow-up listed and assigned, one approved, which
// leaves the digest and is unassigned, and one closed, which is unassigned
// before its closed stage is set.
func TestFollowUpDigestReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	for _, tc := range []struct {
		name     string
		setup    func(testing.TB) *githubtest.Fake
		want     []string
		assigned []string
	}{
		{"listed", followUpDigestReplayFake(nil, func(*githubtest.Fake) {}),
			[]string{"PATCH /issues/1", "POST /issues/2/assignees"}, []string{"owner"}},
		{"approved", followUpDigestReplayFake([]string{"owner"}, func(f *githubtest.Fake) { f.AddIssueReaction(2, "owner", "+1") }),
			[]string{"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:intake", "DELETE /issues/2/assignees"}, nil},
		{"closed", followUpDigestReplayFake([]string{"owner"}, func(f *githubtest.Fake) { f.CloseAs(2, "owner", "not_planned") }),
			[]string{"DELETE /issues/2/assignees", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:intake"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pass := func(f *githubtest.Fake) error { return followUpDigestDaemon(t, f, o).Iterate(t.Context()) }
			clean := tc.setup(t)
			if err := pass(clean); err != nil {
				t.Fatal(err)
			}
			if got := writesOf(clean); !slices.Equal(got, tc.want) {
				t.Errorf("the clean pass wrote %q; want %q", got, tc.want)
			}
			if got := assignees(clean, 2); !slices.Equal(got, tc.assigned) {
				t.Errorf("after the clean pass, the follow-up's assignees = %q; want %q", got, tc.assigned)
			}
			githubtest.Replay(t, tc.setup, pass)
		})
	}
}

// A story with an open follow-up below it isn't finished (SPEC §7.3,
// §9.4), whether the follow-up waits for its approval or was approved: it
// isn't listed or assigned while every child its record names is closed.
// Once the owner closes the follow-up as not planned, which drops it, it
// holds nothing: the pass that judges that close lists the story and
// assigns notify to it.
func TestFollowUpDigestHoldsStory(t *testing.T) {
	t.Parallel()
	for _, approved := range []bool{false, true} {
		t.Run("approved "+strconv.FormatBool(approved), func(t *testing.T) {
			t.Parallel()
			f, o := newFake(t), digestNotifyOrigin(t)
			doneParent(t, f, "story", storyTasks)
			d := doneStoryDaemon(t, f, o)
			d.Step.Reactions = daemon.FollowUpByReaction(handbackTrust, d.Step.HeldSnap)
			doneStep(t, f, d, "the pass that creates the children")
			digestN := doneDigest(t, f)
			fu := f.AddIssue(app, "Refuse a quote that expires mid-booking",
				"Refuse it.\n\ncc @owner\n\n<!-- issuedog:origin verify=#2 run=run3 item=F1 -->")
			f.SetType(fu, "task")
			f.SetParent(fu, 1)
			f.AddLabel(fu, app, "priority:p3")
			f.AddLabel(fu, app, "stage:intake")
			if approved {
				f.AddIssueReaction(fu, "owner", "+1")
			}
			for _, c := range []int{2, 3, 4} {
				f.CloseBy(c, "owner")
			}
			doneStep(t, f, d, "every child closed")
			doneStep(t, f, d, "the pass after")
			if got := doneListed(f, digestN); len(got) != 0 {
				t.Errorf("with the follow-up open, the digest lists %q; want no finished story", got)
			}
			if want, got := "stage:intake", pickStage(f, fu); approved && got != "stage:ready" || !approved && got != want {
				t.Errorf("the follow-up is at %s; want ready if approved, and else intake", got)
			}
			f.CloseAs(fu, "owner", "not_planned")
			doneStep(t, f, d, "the follow-up closed as not planned", assignAdd(1, "owner"))
			if got, want := doneListed(f, digestN), []string{"#1 Finished story p2 1"}; !slices.Equal(got, want) {
				t.Errorf("after the follow-up's close, the digest lists %q; want %q", got, want)
			}
		})
	}
}

// A restart reads the follow-up's item back from the digest's body (SPEC
// §9.4, Keeping the list): when the follow-up's read, or its step 1, at
// the read of its reactions, fails on the restart's first pass, the
// digest is written as it was, with no write at all, and notify stays
// assigned. The next pass, whose visit works, writes nothing either.
func TestFollowUpDigestRestart(t *testing.T) {
	t.Parallel()
	for what, path := range map[string]string{"read": "/issues/1", "step 1": "/issues/1/reactions"} {
		t.Run(what, func(t *testing.T) {
			t.Parallel()
			f, o := approvalFiled(func(testing.TB, *githubtest.Fake) {})(t), digestNotifyOrigin(t)
			iterate(t, followUpDigestDaemon(t, f, o))
			iterate(t, followUpDigestDaemon(t, f, o))
			digestN := doneDigest(t, f)
			before := f.State().Issues[digestN-1].Body
			if got := followUpDigestListed(f, digestN); !slices.Equal(got, []string{"#1 Follow-up p3 1"}) {
				t.Fatalf("the digest lists %q; want the follow-up", got)
			}
			d := followUpDigestDaemon(t, f, o)
			f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == path }, http.StatusInternalServerError)
			from := len(f.Writes())
			if err := d.Iterate(t.Context()); err == nil || !strings.Contains(err.Error(), "o/r#1") {
				t.Fatalf("the restart's first pass: Iterate = %v; want #1's failure", err)
			}
			if got := digestWrites(f, from); len(got) != 0 {
				t.Errorf("the restart's first pass wrote %q; want nothing", got)
			}
			if got := f.State().Issues[digestN-1].Body; got != before {
				t.Errorf("digest\n%s\nwant it as before the restart\n%s", got, before)
			}
			if got := assignees(f, 1); !slices.Equal(got, []string{"owner"}) {
				t.Errorf("the follow-up's assignees = %q; want owner kept", got)
			}
			from = len(f.Writes())
			iterate(t, d)
			if got := digestWrites(f, from); len(got) != 0 {
				t.Errorf("the restart's second pass wrote %q; want nothing", got)
			}
		})
	}
}
