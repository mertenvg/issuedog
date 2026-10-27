package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file tests the rarer edges of where a changed child's origin lives
// (#549, from #366's and #374's verifies, SPEC §5 and §6.4): a repost
// made dirty before the revision, one posted between the rewrite and the
// move, and an adopted child's move comment deleted, or edited along with
// a type change.

// repostEdgeOrigin is T2's origin marker as revision 1 filed it.
const repostEdgeOrigin = "<!-- issuedog:origin plan=#1 rev=1 item=T2 -->"

// repostEdgeDirtySetup is changedStory with T2's child's body edited by
// owner, so its markers aren't read, and the App's repost of its revision
// 1 origin edited by mallory, so it's dirty, as it is in the pass after
// that edit when the story's visit comes before the child's step 1.
func repostEdgeDirtySetup(t testing.TB) *githubtest.Fake {
	return changedSetup(false, func(f *githubtest.Fake, r changedIDs) {
		f.EditBody(r.t2, "owner", "The task, put better by owner.")
		repost := f.AddComment(r.t2, app, originRepost(repostEdgeOrigin))
		f.EditComment(repost, "mallory", originRepost(repostEdgeOrigin)+" Edited.")
	})(t)
}

// repostEdgeMove is the comment of T2's child's move back to ready for
// revision 2, with no origin marker and no word on one.
func repostEdgeMove(t *testing.T, f *githubtest.Fake) string {
	t.Helper()
	p, plan := changedApproval(t, f)
	return changedMoveText("T2", plan, "It goes back to `stage:ready`.") +
		fmt.Sprintf("\n\n<!-- issuedog:transition from=in-progress to=ready event=p%d -->", p)
}

// A repost of a changed child's origin that's dirty, as mallory edited
// it, gives no origin (SPEC §5), so the move's comment carries no marker
// and doesn't say why: no clean App comment carries an older origin for a
// later edit to fall back to. Once eve edits the body, the child's step 1
// reposts the rewrite's marker, naming revision 2, from the body's
// history. A crash at any write, before or after it applies, writes
// nothing twice.
func TestChildrenChangedDirtyRepost(t *testing.T) {
	t.Parallel()
	githubtest.Replay(t, repostEdgeDirtySetup, childrenPass(t, revisionPlan))
	f := run(t, repostEdgeDirtySetup, childrenPass(t, revisionPlan))
	if got, want := newestComment(f, 3), repostEdgeMove(t, f); got != want {
		t.Errorf("T2's child's newest comment is\n%q\nwant\n%q", got, want)
	}
	f.EditBody(3, "eve", "The task, rewritten by eve.")
	had := len(f.State().Issues[2].Comments)
	if err := adoptedChildStep(t, 3)(f); err != nil {
		t.Fatal(err)
	}
	if added := originAdded(f, 3, had); !slices.Equal(added, []string{originRepost(changedRepostOrigin)}) {
		t.Errorf("after eve's edit, T2's child's new comments are %q; want the repost of %s", added, changedRepostOrigin)
	}
}

// Should the child's step 1 repost its origin between the rewrite and the
// move, as when the story's pass failed at the move's comment, then owner
// edited the rewritten body, and the child was visited before the story,
// the repost names revision 2 already, from the rewrite in the body's
// history. The story's next pass then moves the child with no marker of
// its own and no word on one, and doesn't rewrite it again, rather than
// fail for a body with no marker to carry. A crash at any write, before
// or after it applies, writes nothing twice.
func TestChildrenChangedRepostBeforeMove(t *testing.T) {
	t.Parallel()
	// at is the write of the move's comment on T2's child, #3, in the
	// revision's pass.
	clean := repostEdgeDirtySetup(t)
	if err := childrenPass(t, revisionPlan)(clean); err != nil {
		t.Fatal(err)
	}
	at := slices.IndexFunc(clean.Writes(), func(w githubtest.Write) bool { return w.Method == "POST" && w.Path == "/issues/3/comments" }) + 1
	if at == 0 {
		t.Fatalf("the revision's pass wrote %+v; want a comment on #3", clean.Writes())
	}
	setup := func(tb testing.TB) *githubtest.Fake {
		f := repostEdgeDirtySetup(tb)
		f.FailWrite(at)
		if err := childrenPass(t, revisionPlan)(f); err == nil {
			tb.Fatal("the revision's pass, failing at the move's comment, returned no error")
		}
		f.FailWrite(0)
		f.EditBody(3, "owner", "The revised task, put better by owner.")
		if err := adoptedChildStep(t, 3)(f); err != nil {
			tb.Fatal(err)
		}
		if newest := newestComment(f, 3); newest != originRepost(changedRepostOrigin) {
			tb.Fatalf("T2's child's newest comment is %q; want the repost of %s", newest, changedRepostOrigin)
		}
		f.ForgetWrites()
		return f
	}
	before := setup(t)
	had := len(before.State().Issues[2].Comments)
	githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
	f := run(t, setup, childrenPass(t, revisionPlan))
	if added, want := originAdded(f, 3, had), []string{repostEdgeMove(t, f)}; !slices.Equal(added, want) {
		t.Errorf("T2's child's new comments are\n%q\nwant\n%q", added, want)
	}
	if o := adoptedOrigin(t, f, 3); o.Plan != 1 || o.Rev != 2 || o.Item != "T2" || o.Comment == state.BodyID {
		t.Errorf("T2's child's origin is %+v; want item T2 of #1's plan, revision 2, from the repost", o)
	}
	if updates := slices.DeleteFunc(f.Writes(), func(w githubtest.Write) bool { return w.Method != "PATCH" || w.Path != "/issues/3" }); len(updates) != 0 {
		t.Errorf("T2's child got the edits %+v; want none, as its rewrite stands", updates)
	}
}

// repostEdgeAdoptedMove is the comment of the adopted T2's move for
// revision 2, which carries its origin, as adoptedChangedSetup leaves it.
func repostEdgeAdoptedMove(tb testing.TB, f *githubtest.Fake) int64 {
	tb.Helper()
	s, err := f.Client(tb).Snapshot(context.Background(), "o", "r", 3)
	if err != nil {
		tb.Fatal(err)
	}
	move := s.Comments[len(s.Comments)-1]
	if !strings.HasSuffix(move.Body, changedRepostOrigin) {
		tb.Fatalf("T2's child's newest comment is %q; want its move, carrying its origin", move.Body)
	}
	return move.ID
}

// Once someone deletes the comment of the adopted T2's move, which carried
// its origin naming revision 2, Derive falls back to the App's comment
// that adopted it, naming revision 1, as for an edited one. Step 1 on the
// child posts the marker of its rewritten body again, so it reads
// revision 2. A crash at any write, before or after it applies, writes
// nothing twice.
func TestAdoptedRepostDeletedMove(t *testing.T) {
	t.Parallel()
	setup := adoptedChangedSetup(t, func(tb testing.TB, f *githubtest.Fake) {
		f.DeleteComment(repostEdgeAdoptedMove(tb, f))
	})
	before := setup(t)
	if o := adoptedOrigin(t, before, 3); o.Rev != 1 {
		t.Errorf("before step 1, T2's child's origin is %+v; want revision 1's, from the comment that adopted it", o)
	}
	had := len(before.State().Issues[2].Comments)
	githubtest.Replay(t, setup, adoptedChildStep(t, 3))
	f := run(t, setup, adoptedChildStep(t, 3))
	if added := originAdded(f, 3, had); !slices.Equal(added, []string{adoptedRepostLine + "\n\n" + changedRepostOrigin}) {
		t.Errorf("T2's child's new comments are %q; want only the repost of %s", added, changedRepostOrigin)
	}
	if o := adoptedOrigin(t, f, 3); o.Plan != 1 || o.Rev != 2 || o.Item != "T2" || o.Comment == state.BodyID {
		t.Errorf("T2's child's origin is %+v; want item T2 of #1's plan, revision 2, from the repost", o)
	}
}

// Step 1 reposts an adopted child's origin before it checks the type
// (SPEC §8.1): with owner's edit of the adopted T2's move comment and
// owner's retype of it as a feature, a type the pipeline doesn't know,
// the repost comes first, then the type error. A crash at any write,
// before or after it applies, writes nothing twice.
func TestAdoptedRepostBeforeTypeCheck(t *testing.T) {
	t.Parallel()
	var retype int64
	setup := adoptedChangedSetup(t, func(tb testing.TB, f *githubtest.Fake) {
		move := repostEdgeAdoptedMove(tb, f)
		f.EditComment(move, "owner", newestComment(f, 3))
		f.RemoveLabel(3, "owner", "type:task")
		retype = f.AddLabel(3, "owner", "type:feature")
	})
	before := setup(t)
	had := len(before.State().Issues[2].Comments)
	githubtest.Replay(t, setup, adoptedChildStep(t, 3))
	f := run(t, setup, adoptedChildStep(t, 3))
	want := []string{adoptedRepostLine + "\n\n" + changedRepostOrigin, typeErrWant("feature", "ready", retype, true)}
	if added := originAdded(f, 3, had); !slices.Equal(added, want) {
		t.Errorf("T2's child's new comments are\n%q\nwant\n%q", added, want)
	}
}
