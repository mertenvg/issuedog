package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file tests #366: a changed child the App filed, whose origin the
// App reposted in a comment after an edit of its body (SPEC §6.4), gets
// the rewritten origin in its move's comment too, so a later edit of the
// body falls back to the revision's origin, not the reposted one's
// earlier rev. And a changed child held from the clarifying after a hold
// goes, on /resume, to the revision's stage with no origin, not the old
// one.

// changedRepostText is the line a changed child's move comment carries,
// after the revision's, when an earlier App comment carries an older
// origin (childRepostMoveText in internal/daemon).
const changedRepostText = "An earlier comment of mine carries this issue's `origin` marker from before, " +
	"so this comment carries the new one, which still counts once the description is edited again."

// changedRepostOrigin is T2's origin marker as revision 2's rewrite
// writes it.
const changedRepostOrigin = "<!-- issuedog:origin plan=#1 rev=2 item=T2 -->"

// changedRepostSetup is changedStory, held if held, with T2's child's body
// edited by owner, so its markers aren't read, and the App's repost of its
// revision 1 origin (#291).
func changedRepostSetup(held bool) func(testing.TB) *githubtest.Fake {
	return changedSetup(held, func(f *githubtest.Fake, r changedIDs) {
		f.EditBody(r.t2, "owner", "The task, put better by owner.")
		f.AddComment(r.t2, app, originRepost("<!-- issuedog:origin plan=#1 rev=1 item=T2 -->"))
	})
}

// A changed child whose origin the App reposted, at revision 1, is
// rewritten, and its move's comment carries the rewritten origin, naming
// revision 2, and says why, held or not. A crash at any write, before or
// after it applies, writes nothing twice. When eve then edits the body,
// which makes its markers unread, the child's step reposts nothing, and
// its origin is the move comment's, revision 2, not the repost's.
func TestChildrenChangedRepostedOrigin(t *testing.T) {
	t.Parallel()
	for name, held := range map[string]bool{"not held": false, "held": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			setup := changedRepostSetup(held)
			githubtest.Replay(t, setup, childrenPass(t, revisionPlan))
			f := run(t, setup, childrenPass(t, revisionPlan))
			p, plan := changedApproval(t, f)
			t2 := f.State().Issues[2]
			if !strings.HasSuffix(t2.Body, "\n\n"+changedRepostOrigin) || t2.BodyEditedBy != app {
				t.Errorf("T2's child, last edited by %q, has body\n%s\nwant revision 2's rewrite", t2.BodyEditedBy, t2.Body)
			}
			st := childrenStep(t, f, revisionPlan)
			s, err := st.Client.Snapshot(context.Background(), "o", "r", 3)
			if err != nil {
				t.Fatal(err)
			}
			move := s.Comments[len(s.Comments)-1]
			want := changedMoveText("T2", plan, changedRepostText+" It goes back to `stage:ready`.") +
				fmt.Sprintf("\n\n<!-- issuedog:transition from=in-progress to=ready event=p%d -->\n%s", p, changedRepostOrigin)
			if held {
				if !strings.HasPrefix(move.Body, changedMoveText("T2", plan, changedRepostText+" It stays on hold, and `/resume` takes it to `stage:ready`.")) ||
					!strings.Contains(move.Body, "<!-- issuedog:transition from=held to=held ") || !strings.HasSuffix(move.Body, "\n"+changedRepostOrigin) {
					t.Errorf("T2's child's newest comment is\n%q\nwant its held → held move, saying why it carries the origin naming revision 2, and carrying it", move.Body)
				}
			} else if move.Body != want {
				t.Errorf("T2's child's newest comment is\n%q\nwant\n%q", move.Body, want)
			}

			f.EditBody(3, "eve", "The task, rewritten by eve.")
			f.ForgetWrites()
			s, err = st.Client.Snapshot(context.Background(), "o", "r", 3)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Run(context.Background(), s); err != nil {
				t.Fatal(err)
			}
			s, err = st.Client.Snapshot(context.Background(), "o", "r", 3)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range s.Comments {
				if c.ID > move.ID && strings.Contains(c.Body, "<!-- issuedog:origin ") {
					t.Errorf("after eve's edit, T2's child got the comment %q; want no repost", c.Body)
				}
			}
			a := &daemon.Applier{Client: st.Client, Owner: "o", Repo: "r", AppSlug: slug}
			d, err := a.Dirty(context.Background(), s, trusted{"owner": true})
			if err != nil {
				t.Fatal(err)
			}
			if o := state.Derive(s, slug, d.Has).Origin; !d.Has(state.BodyID) || o.Plan != 1 || o.Rev != 2 || o.Item != "T2" || o.Comment != move.ID {
				t.Errorf("after eve's edit, T2's child is dirty %t, with origin %+v; want a dirty body, and the origin of item T2, revision 2, from its move's comment %d",
					d.Has(state.BodyID), o, move.ID)
			}
		})
	}
}

// changedAfterHoldSetup is changedStory with T2's child, picked up at
// in-progress, held for discussion there, then at the clarifying after
// that hold, whose origin is in-progress, and held there by owner's
// /hold. Then plan revision 2 changes it, approved by owner's 👍, and a
// pass applies it. Owner's /resume follows, whose comment's ID it sets in
// resume.
func changedAfterHoldSetup(resume *int64) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		tt, ok := t.(*testing.T)
		if !ok {
			t.Fatal("changedAfterHoldSetup needs a *testing.T")
		}
		f := newFake(t)
		changedStory(t, f, false, func(f *githubtest.Fake, r changedIDs) {
			f.AddComment(r.t2, app, "On hold for discussion.\n\n"+
				"<!-- issuedog:transition from=in-progress to=held at=in-progress origin=in-progress reason=discussion event=e9-1 -->")
			f.AddComment(r.t2, app, "Discussion resolved.\n\n<!-- issuedog:transition from=held to=clarifying at=in-progress origin=in-progress event=h9 -->")
			hold := f.AddComment(r.t2, "owner", "/hold")
			f.AddComment(r.t2, app, fmt.Sprintf("On hold.\n\n<!-- issuedog:transition from=clarifying to=held at=clarifying origin=in-progress reason=owner cmd=%d -->", hold))
			f.AddReaction(hold, app, "+1")
			f.RemoveLabel(r.t2, app, "stage:in-progress")
			f.AddLabel(r.t2, app, "stage:held")
		})
		if err := childrenPass(tt, revisionPlan)(f); err != nil {
			t.Fatal(err)
		}
		f.ForgetWrites()
		*resume = f.AddComment(3, "owner", "/resume")
		return f
	}
}

// A changed child held from the clarifying after a hold keeps its hold,
// whose at and origin become ready, the revision's (SPEC §6.4). Owner's
// /resume then takes it to ready, not to that clarifying, and carries no
// origin: only the interviewer's move out of the clarifying after a hold
// carries one (SPEC §7.2, Skipping plan review), so in-progress, the old
// origin, isn't carried onto it. The write is safe to replay.
func TestChildrenChangedAfterHoldResume(t *testing.T) {
	t.Parallel()
	var resume int64
	setup := changedAfterHoldSetup(&resume)
	pass := func(f *githubtest.Fake) error {
		st := childrenStep(t, f, revisionPlan)
		s, err := st.Client.Snapshot(context.Background(), "o", "r", 3)
		if err != nil {
			return err
		}
		_, err = st.Run(context.Background(), s)
		return err
	}
	f := setup(t)
	cs := f.State().Issues[2].Comments
	changed := ivHoldMove(t, cs[len(cs)-2].Body)
	ivHoldFields(t, changed, map[string]string{"from": "held", "to": "held", "at": "ready", "origin": "ready", "reason": ""})
	githubtest.Replay(t, setup, pass)
	f = run(t, setup, pass)
	m := ivHoldMove(t, newestComment(f, 3))
	ivHoldFields(t, m, map[string]string{"from": "held", "to": "ready", "cmd": fmt.Sprintf("%d", resume)})
	if o, ok := m.Fields["origin"]; ok {
		t.Errorf("the /resume to ready carries origin=%s; want none", o)
	}
	if l := f.State().Issues[2].Labels; !slices.Contains(l, "stage:ready") || slices.Contains(l, "stage:held") {
		t.Errorf("after /resume, T2's child is labelled %v, want stage:ready", l)
	}
}
