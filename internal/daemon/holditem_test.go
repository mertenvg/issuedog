package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Hold items (#192, SPEC §9.4) against the fakes, through Iterate with
// the digest and assignment on, and owner to notify. #1 is the pinned
// digest, and #2 the owner's issue at ready. The daemon holdItemDaemon
// makes has no reaction triggers, so only a move out of held resolves a
// trigger there: a stage label the owner sets by hand. #188's
// resolutions are tested with them on in holditem_resolve_test.go.

// holdItemDaemon is digestDaemon with assignment on too.
func holdItemDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	t.Helper()
	d := digestDaemon(t, f, o, nil)
	d.Assign = true
	return d
}

// holdItemFake is digestPinned with #2 at ready, the owner's.
func holdItemFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := digestPinned(t)
	fileAt(f, "ready", "do it")
	return f
}

// holdItemOwnerHeld is holdItemFake with #2 held by the owner's /hold, as
// the App moved it.
func holdItemOwnerHeld(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := holdItemFake(t)
	f.RemoveLabel(2, app, "stage:ready")
	f.AddLabel(2, app, "stage:held")
	f.AddComment(2, app, "On hold.\n\n<!-- issuedog:transition from=ready to=held at=ready reason=owner cmd=9 seen=1001 -->")
	return f
}

// holdItemQuiet runs two passes of d, then checks that a third writes
// nothing and asks only conditional GETs that GitHub answers 304, with no
// login's permission asked twice: the hold item's look for triggers shares
// the visit's trust with step 1's. A new daemon's pass writes nothing
// either.
func holdItemQuiet(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, o *origin, step string) {
	t.Helper()
	holdItemQuietWith(t, f, d, step, func() *daemon.Daemon { return holdItemDaemon(t, f, o) })
}

// holdItemQuietWith is holdItemQuiet, with restart the new daemon.
func holdItemQuietWith(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, step string, restart func() *daemon.Daemon) {
	t.Helper()
	iterate(t, d)
	iterate(t, d)
	writes, from := len(f.Writes()), len(f.Requests())
	iterate(t, d)
	asked := map[string]int{}
	for _, r := range f.Requests()[from:] {
		if r.Method != "GET" || !r.Conditional || r.Status != 304 {
			t.Errorf("%s: the unchanged pass sent %+v; want only conditional GETs answered 304", step, r)
		}
		if strings.HasSuffix(r.Path, "/permission") {
			if asked[r.Path]++; asked[r.Path] > 1 {
				t.Errorf("%s: the unchanged pass asked %s %d times; want once at most", step, r.Path, asked[r.Path])
			}
		}
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("%s: the unchanged pass wrote %+v; want nothing", step, w)
	}
	iterate(t, restart())
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("%s: a restart wrote %+v; want nothing", step, w)
	}
}

// A write collaborator's comment holds #2, which the digest lists with its
// trigger, and whose notify users are assigned. The hold's comment
// mentions them, so the digest doesn't (SPEC §9.4). An unchanged pass, and
// a restart, write nothing. Once the owner sets a stage by hand, which
// resolves every trigger, the item goes, and so does the assignee.
func TestHoldItemListedAndAssigned(t *testing.T) {
	t.Parallel()
	f, o := holdItemFake(t), digestNotifyOrigin(t)
	c := f.AddComment(2, "mallory", "Wait, is this right?")
	d := holdItemDaemon(t, f, o)
	iterate(t, d)
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("labels %q; want #2 held for discussion", l)
	}
	body := f.State().Issues[0].Body
	if !strings.Contains(body, "### 1. #2: Issue\n\n**Discussion hold**, priority p2") ||
		!strings.Contains(body, fmt.Sprintf("- **Trigger:** `mallory` posted comment %d. A code owner's 👍 on the comment approves it", c)) {
		t.Errorf("digest body %q; want #2's hold listed with mallory's comment", body)
	}
	if cs := digestComments(f, 1); len(cs) != 0 {
		t.Errorf("digest comments %q; want no announcement, as the hold's comment mentions the owner", cs)
	}
	if got := assignees(f, 2); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("#2's assignees %q; want owner while it's held for discussion", got)
	}
	holdItemQuiet(t, f, d, o, "held")
	if got := assignees(f, 2); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("#2's assignees %q; want owner still, as the trigger is open", got)
	}

	f.RemoveLabel(2, "owner", "stage:held")
	f.AddLabel(2, "owner", "stage:ready")
	iterate(t, d)
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Fatalf("labels %q; want the owner's ready to stand", l)
	}
	if body := f.State().Issues[0].Body; body != digestList {
		t.Errorf("digest body %q; want nothing listed once the trigger is resolved", body)
	}
	if got := assignees(f, 2); len(got) != 0 {
		t.Errorf("#2's assignees %q; want none once nothing waits", got)
	}
	holdItemQuiet(t, f, d, o, "resolved")
}

// Under an owner hold alone, nothing is listed or assigned. A write
// collaborator's comment then holds #2 with no App comment of its own
// (SPEC §6.4), so the digest announces the hold, mentioning the owner, keyed
// by that comment. A rename, whose notice mentions the owner, is listed but
// not announced, and a later comment announces it again, mentioning the owner
// once more: its marker names every trigger no App comment named.
func TestHoldItemAnnouncesUnmentioned(t *testing.T) {
	t.Parallel()
	f, o := holdItemOwnerHeld(t), digestNotifyOrigin(t)
	d := holdItemDaemon(t, f, o)
	iterate(t, d)
	if body := f.State().Issues[0].Body; body != digestList || len(assignees(f, 2)) != 0 {
		t.Errorf("digest body %q, #2's assignees %q; want nothing listed or assigned for an owner hold alone", body, assignees(f, 2))
	}
	c := f.AddComment(2, "mallory", "Wait.")
	if got := digestPass(t, f, d); !slices.Equal(got, []string{"POST /issues/1/comments", "PATCH /issues/1", "POST /issues/2/assignees"}) {
		t.Errorf("writes %q; want the announcement, the body edit and the assignee", got)
	}
	cs := digestComments(f, 1)
	key := fmt.Sprintf("hold-2-c%d", c)
	if len(cs) != 1 || !strings.Contains(cs[0], "- **Discussion hold** on #2: Issue. Answer here: https://github.com/o/r/issues/2\n\ncc @owner\n\n") ||
		!strings.HasSuffix(cs[0], "<!-- issuedog:digest-new items="+key+" -->") {
		t.Errorf("digest comments %q; want one announcement of %s, with the cc line", cs, key)
	}
	if body := f.State().Issues[0].Body; !strings.Contains(body, "It's on an owner hold too") || !strings.Contains(body, fmt.Sprintf("posted comment %d", c)) {
		t.Errorf("digest body %q; want the hold, its comment and the owner hold", body)
	}
	holdItemQuiet(t, f, d, o, "announced")

	ev := f.RenameIssue(2, "mallory", "Wait")
	if got := digestPass(t, f, d); !slices.Equal(got, []string{"POST /issues/2/comments", "PATCH /issues/1"}) {
		t.Errorf("writes %q; want the rename's notice and the body edit, and no announcement", got)
	}
	if body := f.State().Issues[0].Body; !strings.Contains(body, "`mallory` renamed this issue to “Wait”. A code owner's 👍 on its notice accepts it.") {
		t.Errorf("digest body %q; want the rename %d listed", body, ev)
	}
	holdItemQuiet(t, f, d, o, "renamed")

	c2 := f.AddComment(2, "mallory", "And another thing.")
	if got := digestPass(t, f, d); !slices.Equal(got, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Errorf("writes %q; want the announcement and the body edit", got)
	}
	cs = digestComments(f, 1)
	if want := fmt.Sprintf("<!-- issuedog:digest-new items=hold-2-c%d,hold-2-c%d -->", c, c2); len(cs) != 2 || !strings.HasSuffix(cs[1], want) {
		t.Errorf("digest comments %q; want a second announcement ending %s", cs, want)
	}
	holdItemQuiet(t, f, d, o, "announced again")
}

// A trigger whose notice was lost, deleted after it was posted, gets no
// second notice (discLost), so the digest names it, says only /resume
// accepts it, and announces it.
func TestHoldItemLostNotice(t *testing.T) {
	t.Parallel()
	f, o := holdItemOwnerHeld(t), digestNotifyOrigin(t)
	ev := f.RenameIssue(2, "mallory", "Wait")
	d := holdItemDaemon(t, f, o)
	iterate(t, d)
	cs := f.State().Issues[1].Comments
	if !strings.Contains(cs[len(cs)-1].Body, fmt.Sprintf("<!-- issuedog:notice key=%d -->", ev)) || len(digestComments(f, 1)) != 0 {
		t.Fatalf("newest comment on #2 %q, digest comments %q; want the rename's notice, and no announcement", cs[len(cs)-1].Body, digestComments(f, 1))
	}
	ids := commentIDs(t, f, 2)
	f.DeleteCommentBy("owner", ids[len(ids)-1])
	iterate(t, d)
	body := f.State().Issues[0].Body
	if !strings.Contains(body, "`mallory` renamed this issue to “Wait”. It has no clean notice, as one may have been deleted or edited, so only `/resume` here accepts it.") {
		t.Errorf("digest body %q; want the rename listed, accepted only by /resume", body)
	}
	if got := digestComments(f, 1); len(got) != 1 || !strings.HasSuffix(got[0], fmt.Sprintf("<!-- issuedog:digest-new items=hold-2-%d -->", ev)) {
		t.Errorf("digest comments %q; want the rename's hold announced", got)
	}
	if got := assignees(f, 2); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("#2's assignees %q; want owner", got)
	}
	holdItemQuiet(t, f, d, o, "lost notice")
}

// Replay passes for a hold's announcement, its body edit and its assignee,
// whatever write a crash comes at.
func TestHoldItemReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	setup := func(t testing.TB) *githubtest.Fake {
		f := holdItemOwnerHeld(t)
		f.AddComment(2, "mallory", "Wait.")
		return f
	}
	clean := setup(t)
	if err := holdItemDaemon(t, clean, o).Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := digestWrites(clean, 0); !slices.Equal(got, []string{"POST /issues/1/comments", "PATCH /issues/1", "POST /issues/2/assignees"}) {
		t.Errorf("the clean pass wrote %q; want the announcement, the body edit and the assignee", got)
	}
	githubtest.Replay(t, setup, func(f *githubtest.Fake) error { return holdItemDaemon(t, f, o).Iterate(context.Background()) })
}
