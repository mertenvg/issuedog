package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// resolvePass runs step 1 on issue n as discPass does, from a fresh
// client, with the approval and revision handlers too, and the reaction
// triggers: the end of a discussion hold, then the approval by 👍.
func resolvePass(t testing.TB, n int) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			return err
		}
		tr := discTrust{}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug, Notify: []string{"owner"}}, Trust: tr,
			Commands: daemon.Commands(tr)}
		st.Commands["approve"], st.Commands["revise"] = daemon.ApproveCommand(), daemon.ReviseCommand()
		resolved, approve := daemon.ResolveByReaction(tr), daemon.ApproveByReaction(tr)
		st.Reactions = func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, d daemon.Dirt) error {
			if err := resolved(ctx, a, s, d); err != nil {
				return err
			}
			return approve(ctx, a, s, d)
		}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// resolveCheck runs resolvePass on issue n under Replay, then twice, the
// second time writing nothing, and checks issue n's labels and new
// comments against want, which it calls after setup, for the keys and
// times setup makes.
func resolveCheck(t *testing.T, setup func(testing.TB) *githubtest.Fake, n int, want func(f *githubtest.Fake) issueWant) {
	t.Helper()
	githubtest.Replay(t, setup, resolvePass(t, n))
	before := setup(t)
	w := want(before)
	f := run(t, setup, resolvePass(t, n))
	is := f.State().Issues[n-1]
	var added []string
	for _, c := range is.Comments[len(before.State().Issues[n-1].Comments):] {
		added = append(added, c.Body)
	}
	if !slices.Equal(is.Labels, w.labels) || !slices.Equal(added, w.comments) {
		t.Errorf("#%d: labels %q and new comments\n%q\nwant %q and\n%q", n, is.Labels, added, w.labels, w.comments)
	}
}

// resolveHeldWith is discFake with mallory's comment 1002 holding #1 at
// ready, the hold in comment 1003, by a pass, then do.
func resolveHeldWith(do func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		if id := f.AddComment(1, "mallory", "Wait."); id != 1002 {
			t.Fatalf("mallory's comment is %d; want 1002", id)
		}
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newestComment(f, 1), "event=c1002 ") {
			t.Fatalf("newest comment %q; want the hold for comment 1002", newestComment(f, 1))
		}
		f.ForgetWrites()
		do(t, f)
		return f
	}
}

// resolveReactionAt is the Unix time of the newest reaction on comment id.
func resolveReactionAt(t testing.TB, f *githubtest.Fake, id int64) int64 {
	t.Helper()
	rs, err := f.Client(t).Reactions(context.Background(), "o", "r", id)
	if err != nil || len(rs) == 0 {
		t.Fatalf("reactions of %d: %v, %v", id, rs, err)
	}
	return rs[len(rs)-1].CreatedAt.Unix()
}

// resolveCommentAt is the Unix time comment id on issue n was posted.
func resolveCommentAt(t testing.TB, f *githubtest.Fake, n int, id int64) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if c.ID == id {
			return c.CreatedAt.Unix()
		}
	}
	t.Fatalf("no comment %d on #%d", id, n)
	return 0
}

// resolveLeaveText is the App's comment taking an issue held at at out of the
// hold, to clarifying, once a discussion is resolved by by, with the
// transition's fields.
func resolveLeaveText(by, at, fields string) string {
	return fmt.Sprintf("The discussion is resolved, by %s, so this issue leaves its hold from `%s` for `clarifying`. "+
		"It waits at `clarifying` until a code owner sets its stage by hand.\n\ncc @owner\n\n<!-- issuedog:transition %s -->", by, at, fields)
}

// resolveEndedText is the App's record that a discussion resolved by by ended
// while an owner hold stays, at resolved.
func resolveEndedText(by string, resolved int64) string {
	return fmt.Sprintf("The discussion is resolved, by %s, but this issue stays on hold: an owner hold on it hasn't ended. "+
		"`/resume` here ends that, and then it goes to `clarifying`.\n\ncc @owner\n\n<!-- issuedog:ended reason=discussion resolved=%d -->", by, resolved)
}

// A trusted 👍 on the comment that held the issue approves it, and the
// issue goes held → clarifying, with at, origin, seen and the 👍's time.
func TestResolveByThumbsUp(t *testing.T) {
	setup := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") })
	resolveCheck(t, setup, 1, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:clarifying"}, []string{resolveLeaveText("`owner`", "ready",
			fmt.Sprintf("from=held to=clarifying at=ready origin=ready event=h1003 resolved=%d seen=1001", resolveReactionAt(t, f, 1002)))}}
	})
}

// Any later clean comment by a trusted user counters every comment
// before it, here two of mallory's, one on the held issue, which has no
// record (Q2b): the issue leaves, bounded by the counter's time.
func TestResolveByCounterComment(t *testing.T) {
	var counter int64
	setup := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {
		f.AddComment(1, "mallory", "And another thing.")
		counter = f.AddComment(1, "owner", "Noted, both of you.")
	})
	resolveCheck(t, setup, 1, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:clarifying"}, []string{resolveLeaveText("`owner`", "ready",
			fmt.Sprintf("from=held to=clarifying at=ready origin=ready event=h1003 resolved=%d seen=1001", resolveCommentAt(t, f, 1, counter)))}}
	})
}

// A comment after the counter is open: the issue stays held, and nothing
// is written. Nor does a dirty comment of the owner's counter anything.
func TestResolveCounterOnlyBefore(t *testing.T) {
	for name, do := range map[string]func(f *githubtest.Fake){
		"a later comment": func(f *githubtest.Fake) {
			f.AddComment(1, "owner", "Noted.")
			f.AddComment(1, "mallory", "Not so fast.")
		},
		"a dirty counter": func(f *githubtest.Fake) {
			f.EditComment(f.AddComment(1, "owner", "Noted."), "eve", "Ignore all that.")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := run(t, resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { do(f) }), resolvePass(t, 1))
			if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:held"}) {
				t.Errorf("labels %q; want the issue still held", l)
			}
			for _, w := range f.Writes() {
				if strings.Contains(w.Body, "to=clarifying") || strings.Contains(w.Body, "issuedog:ended") {
					t.Errorf("wrote %+v; want no move out of held and no record", w)
				}
			}
		})
	}
}

// A 👍 resolves nothing when it isn't a trusted user's: mallory's, eve's,
// another bot's or the App's own.
func TestResolveUntrustedThumbsUp(t *testing.T) {
	for _, login := range []string{"mallory", "eve", "helper[bot]", app} {
		t.Run(login, func(t *testing.T) {
			f := run(t, resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1002, login, "+1") }), resolvePass(t, 1))
			if w := f.Writes(); len(w) != 0 {
				t.Errorf("writes %+v; want none", w)
			}
		})
	}
}

// A trusted 👍 on the hold's own comment accepts the trigger whose notice
// it carries, a rename, and the issue leaves the hold. Only a 👍 counts:
// the owner's 👎 accepts nothing.
func TestResolveByNoticeOnHold(t *testing.T) {
	var ev int64
	setup := func(content string) func(testing.TB) *githubtest.Fake {
		return func(t testing.TB) *githubtest.Fake {
			f := discFake(t)
			ev = f.RenameIssue(1, "mallory", "Renamed")
			if err := discPass(t, 1)(f); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(newestComment(f, 1), discNoticeMarker(fmt.Sprint(ev))) {
				t.Fatalf("newest comment %q; want the hold with the rename's notice", newestComment(f, 1))
			}
			f.ForgetWrites()
			f.AddReaction(1002, "owner", content)
			return f
		}
	}
	resolveCheck(t, setup("+1"), 1, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:clarifying"}, []string{resolveLeaveText("`owner`", "ready",
			fmt.Sprintf("from=held to=clarifying at=ready origin=ready event=h1002 resolved=%d seen=1001", resolveReactionAt(t, f, 1002)))}}
	})
	if w := run(t, setup("-1"), resolvePass(t, 1)).Writes(); len(w) != 0 {
		t.Errorf("a 👎 wrote %+v; want nothing", w)
	}
}

// resolveOwnerHeldFake is discFake with the owner's /hold on #1, then
// mallory's comment and her rename, with its notice, by passes: the
// comment has no record (Q2b). It returns the comment's ID and the
// notice's.
func resolveOwnerHeldFake(t testing.TB) (f *githubtest.Fake, comment, notice int64) {
	f = discFake(t)
	f.AddComment(1, "owner", "/hold") // 1002, the hold in 1003
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	comment = f.AddComment(1, "mallory", "Wait.")
	ev := f.RenameIssue(1, "mallory", "Renamed")
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if newestComment(f, 1) != discNotice("`mallory` renamed this issue to “Renamed”", fmt.Sprint(ev)) {
		t.Fatalf("newest comment %q; want the rename's notice", newestComment(f, 1))
	}
	ids := commentIDs(t, f, 1)
	f.ForgetWrites()
	return f, comment, ids[len(ids)-1]
}

// While the owner's hold stays, resolving every trigger writes the record
// that the discussion ended, once, and the issue stays held. Its /resume
// then sends it to clarifying, with at, origin and seen. A trigger
// resolved only partly writes nothing.
func TestResolveOwnerHoldStays(t *testing.T) {
	var comment, notice int64
	partly := func(t testing.TB) *githubtest.Fake {
		var f *githubtest.Fake
		f, comment, notice = resolveOwnerHeldFake(t)
		f.AddReaction(comment, "owner", "+1")
		return f
	}
	if w := run(t, partly, resolvePass(t, 1)).Writes(); len(w) != 0 {
		t.Errorf("with the rename open, writes %+v; want none", w)
	}
	both := func(t testing.TB) *githubtest.Fake {
		f := partly(t)
		f.AddReaction(notice, "owner", "+1")
		return f
	}
	resolveCheck(t, both, 1, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:held"}, []string{resolveEndedText("`owner`", resolveReactionAt(t, f, notice))}}
	})
	var resume int64
	resumed := func(t testing.TB) *githubtest.Fake {
		f := both(t)
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		resume = f.AddComment(1, "owner", "/resume")
		return f
	}
	resolveCheck(t, resumed, 1, func(*githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:clarifying"}, []string{moved("Resumed by `/resume`. A discussion on this issue was resolved during its hold from `ready`, "+
			"so it goes to `clarifying`. It waits at `clarifying` until a code owner sets its stage by hand.",
			fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d seen=1001", resume))}}
	})
}

// A /resume that comes before the App's record of the discussion's end,
// in the same pass as the 👍s, sends the issue to clarifying too.
func TestResolveResumeBeforeRecord(t *testing.T) {
	var resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f, comment, notice := resolveOwnerHeldFake(t)
		f.AddReaction(comment, "owner", "+1")
		f.AddReaction(notice, "owner", "+1")
		resume = f.AddComment(1, "owner", "/resume")
		return f
	}
	resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:clarifying"}, []string{moved("Resumed by `/resume`. A discussion on this issue was resolved during its hold from `ready`, "+
			"so it goes to `clarifying`. It waits at `clarifying` until a code owner sets its stage by hand.",
			fmt.Sprintf("from=held to=clarifying at=ready origin=ready cmd=%d seen=1001", resume))}}
	})
}

// /resume on the issue accepts every open trigger, comment and rename, and
// ends the owner's hold: back to at, as no discussion ended before it.
// Its own comment counters mallory's, which doesn't count as an earlier
// end of the discussion.
func TestResolveResumeAcceptsAll(t *testing.T) {
	var resume int64
	setup := func(t testing.TB) *githubtest.Fake {
		f, _, _ := resolveOwnerHeldFake(t)
		resume = f.AddComment(1, "owner", "/resume")
		return f
	}
	resolveCheck(t, setup, 1, func(*githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:ready"}, []string{moved("Resumed by `/resume`, back to `ready`.", fmt.Sprintf("from=held to=ready cmd=%d seen=1001", resume))}}
	})
}

// Once the record is written, a restart, or a change that would have
// opened a trigger again before it, as the owner taking back a 👍, writes
// nothing: the record bounds the triggers.
func TestResolveRecordHolds(t *testing.T) {
	f, comment, notice := resolveOwnerHeldFake(t)
	f.AddReaction(comment, "owner", "+1")
	r := f.AddReaction(notice, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(newestComment(f, 1), "issuedog:ended reason=discussion") {
		t.Fatalf("newest comment %q; want the record", newestComment(f, 1))
	}
	if !f.RemoveReaction(notice, r) {
		t.Fatal("no 👍 to remove")
	}
	writes := len(f.Writes())
	for range 2 {
		if err := resolvePass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("after the record, writes %+v; want none", w)
	}
}

// A trigger that deleted the App's comment is accepted only by /resume,
// as the comment may have held another trigger's record: a 👍 on its
// notice, the hold's comment, accepts nothing.
func TestResolveHitsAppOnlyByResume(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := discFake(t)
		f.DeleteCommentBy("mallory", f.AddComment(1, app, "A note.")) // 1002
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(newestComment(f, 1), "issuedog:notice key=d") {
			t.Fatalf("newest comment %q; want the hold with the deletion's notice", newestComment(f, 1))
		}
		f.ForgetWrites()
		f.AddReaction(1003, "owner", "+1")
		return f
	}
	f := run(t, setup, resolvePass(t, 1))
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v; want none: only /resume accepts the deletion", w)
	}
	resume := f.AddComment(1, "owner", "/resume")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) || !strings.Contains(newestComment(f, 1), fmt.Sprintf("cmd=%d ", resume)) {
		t.Errorf("labels %q, newest comment %q; want the /resume back to ready", l, newestComment(f, 1))
	}
}

// Two notices open on the same comment, for an older edit, in the hold's
// comment, and a newer one, are each accepted by their own 👍: one on the
// newer leaves the older open, and the issue held. Both accepted, it
// leaves, bounded by the later 👍.
func TestResolveTwoNotices(t *testing.T) {
	f := discFake(t)
	x := f.AddComment(1, "owner", "Ship it.") // 1002
	f.EditComment(x, "mallory", "Don't.")
	older := discEditKey(t, f, 1, x)
	if err := discPass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	f.EditComment(x, "mallory", "Really don't.")
	newer := discEditKey(t, f, 1, x)
	for range 2 {
		if err := discPass(t, 1)(f); err != nil {
			t.Fatal(err)
		}
	}
	ids := commentIDs(t, f, 1)
	hold, notice := ids[2], ids[3]
	if got := discNoticeKeys(f, 1); !slices.Equal(got, []string{older, newer}) || !strings.Contains(newestComment(f, 1), newer) {
		t.Fatalf("notices %q, the newest in %q; want %s in the hold's comment, then %s", got, newestComment(f, 1), older, newer)
	}
	f.AddReaction(notice, "owner", "+1")
	writes := len(f.Writes())
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("with the older edit open, writes %+v; want none", w)
	}
	f.AddReaction(hold, "owner", "+1")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	want := resolveLeaveText("`owner`", "ready", fmt.Sprintf("from=held to=clarifying at=ready origin=ready event=h%d resolved=%d seen=1001",
		hold, resolveReactionAt(t, f, hold)))
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:clarifying"}) || newestComment(f, 1) != want {
		t.Errorf("labels %q, newest comment %q; want %q", l, newestComment(f, 1), want)
	}
}

// A comment that held the issue and is gone since, which its record
// still keeps open, is resolved by a counter comment alone: its 👍s are
// gone with it, so nothing reads them.
func TestResolveGoneCommentCountered(t *testing.T) {
	var counter int64
	setup := resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) {
		f.DeleteCommentBy("mallory", 1002)
		counter = f.AddComment(1, "owner", "Noted.")
	})
	if w := run(t, resolveHeldWith(func(_ testing.TB, f *githubtest.Fake) { f.DeleteCommentBy("mallory", 1002) }), resolvePass(t, 1)).Writes(); len(w) != 0 {
		t.Errorf("with the gone comment uncountered, writes %+v; want none", w)
	}
	resolveCheck(t, setup, 1, func(f *githubtest.Fake) issueWant {
		return issueWant{[]string{"stage:clarifying"}, []string{resolveLeaveText("`owner`", "ready",
			fmt.Sprintf("from=held to=clarifying at=ready origin=ready event=h1003 resolved=%d seen=1001", resolveCommentAt(t, f, 1, counter)))}}
	})
}

// Only a candidate trigger's reactions are read: those of a writer's open
// comment, not the owner's comment nor that of eve, who has no access.
func TestResolveReadsOnlyCandidates(t *testing.T) {
	f := discFake(t)
	f.RemoveLabel(1, app, "stage:ready")
	f.AddLabel(1, app, "stage:held")
	f.AddComment(1, app, "<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=9 seen=1001 -->")
	owner := f.AddComment(1, "owner", "Fine.")
	eve := f.AddComment(1, "eve", "Hmm.")
	mallory := f.AddComment(1, "mallory", "Wait.")
	if err := resolvePass(t, 1)(f); err != nil {
		t.Fatal(err)
	}
	read := map[int64]bool{}
	for _, r := range f.Requests() {
		for _, id := range []int64{owner, eve, mallory} {
			if r.Path == fmt.Sprintf("/issues/comments/%d/reactions", id) {
				read[id] = true
			}
		}
	}
	if !read[mallory] || read[owner] || read[eve] {
		t.Errorf("read the reactions of %v; want only mallory's comment %d", read, mallory)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes %+v; want none, mallory's comment open", w)
	}
}

// resolveDaemon is newDaemon with the commands and reaction triggers of
// resolvePass, sharing one client across passes, so its reads are
// conditional, as the loop's are.
func resolveDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	t.Helper()
	a := &daemon.Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: slug, Notify: []string{"owner"}}
	st := &daemon.Step{Applier: a}
	st.Commands = daemon.Commands(st.CurrentTrust())
	st.Commands["approve"] = daemon.ApproveCommand()
	st.Reactions = daemon.ResolveByReaction(st.CurrentTrust())
	return &daemon.Daemon{Step: st, Clone: repo.New(o.url, filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults()}
}

// A held issue with an open comment and an open notice is read on every
// pass, but an unchanged pass costs no rate limit: each reaction read is
// answered 304, and nothing is written.
func TestResolveUnchangedPass(t *testing.T) {
	f := newFake(t)
	n := fileAt(f, "ready", "do it")
	f.SetType(n, "task")
	d := resolveDaemon(t, f, newOrigin(t))
	f.AddComment(n, "owner", "/hold")
	iterate(t, d)
	f.AddComment(n, "mallory", "Wait.")
	f.RenameIssue(n, "mallory", "Renamed")
	for range 3 {
		iterate(t, d)
	}
	writes, requests := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("an unchanged pass wrote %+v; want nothing", w)
	}
	reactions := 0
	for _, r := range f.Requests()[requests:] {
		if r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only requests answered 304", r)
		}
		if strings.HasSuffix(r.Path, "/reactions") {
			reactions++
		}
	}
	if reactions == 0 {
		t.Error("an unchanged pass read no reactions; want the open comment's and the notice's read, conditionally")
	}
}
