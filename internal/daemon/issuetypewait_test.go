package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #183: an issue type no issue-type event explains gets the
// labels' 5 minutes, held back for github.HoldBound, then left out for the
// type that stands until it's explained, or reverted after
// github.UnexplainedWait; and issue types are compared by ID.

// issueTypeWaitReads counts the reads of issue 1 so far: its GET and its
// timeline's.
func issueTypeWaitReads(f *githubtest.Fake) int {
	n := 0
	for _, r := range f.Requests() {
		if r.Method == "GET" && (r.Path == "/issues/1" || strings.HasPrefix(r.Path, "/issues/1/timeline")) {
			n++
		}
	}
	return n
}

// issueTypeWaitFallback is the App's comment on reverting an issue type
// no event explains, keyed key: from is the type on the issue, as GitHub
// shows it, or "" for none; to is the type that stands.
func issueTypeWaitFallback(from, to, key string) string {
	why := fmt.Sprintf("no change by a trusted user or the App explains `%s`", from)
	if from == "" {
		why = "no change by a trusted user or the App explains the issue having no type"
	}
	return fmt.Sprintf("I restored the issue type `%s`: %s, and only trusted users can change the issue type.\n\n"+
		"<!-- issuedog:revert key=%s -->", to, why, key)
}

// A trusted change whose event lands later than github.HoldBound, but
// before github.UnexplainedWait, isn't reverted. It holds the issue back,
// then is left out for the type that stands, Task, which fits ready, so
// no type error is posted while it waits, and the issue isn't read again
// until it's due. Then its event explains it, so it stands, and gives the
// one type error, keyed by its event, with one mention.
func TestIssueTypeWaitLateTrusted(t *testing.T) {
	t.Parallel()
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	news := digestRunNews(f, 2)
	f.LagEvents(2 * time.Minute)
	key := f.SetTypeBy(1, "owner", "Epic")
	f.LagEvents(0)
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("held back: writes %q; want none", w)
	}
	f.Advance(github.HoldBound + time.Second)
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("left out: writes %q; want none", w)
	}
	issueTypeRevertCheck(t, f, "Epic")
	before := issueTypeWaitReads(f)
	for _, wait := range []time.Duration{time.Minute, time.Minute, time.Minute} { // its event lands in the second
		f.Advance(wait)
		if w := d.pass(t); len(w) != 0 {
			t.Errorf("left out, %v on: writes %q; want none", wait, w)
		}
	}
	if n := issueTypeWaitReads(f) - before; n != 0 {
		t.Errorf("issue 1 read %d times while its type was left out and not due; want none", n)
	}
	issueTypeRevertCheck(t, f, "Epic")
	f.Advance(2 * time.Minute) // due: five minutes since it was first found
	// The type error, then the digest's announcement of it and its body: no
	// PATCH of issue 1, which would be a revert.
	if w := d.pass(t); !slices.Equal(w, []string{"POST /issues/1/comments", "POST /issues/2/comments", "PATCH /issues/2"}) {
		t.Errorf("once due: writes %q; want the type error and the digest's", w)
	}
	if errs := errorMarkers(f, 1); !slices.Equal(errs, []string{fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", key)}) {
		t.Errorf("state errors %q; want the one type error, keyed %d", errs, key)
	}
	if got := f.State().Issues[0].Type; got != "Epic" {
		t.Errorf("type %q; want the owner's Epic", got)
	}
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("after the type error: writes %q; want none", w)
	}
	if got := digestRunNews(f, 2); got != news+1 {
		t.Errorf("%d digest announcements; want %d", got, news+1)
	}
}

// A type left out is read again sooner when the issue changes, as by a
// comment, and then once its event has landed, it stands, with its type
// error, though it isn't due yet.
func TestIssueTypeWaitLateTrustedPoked(t *testing.T) {
	t.Parallel()
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	f.LagEvents(time.Minute)
	key := f.SetTypeBy(1, "owner", "Epic")
	f.LagEvents(0)
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("held back: writes %q; want none", w)
	}
	f.Advance(github.HoldBound + time.Second)
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("left out: writes %q; want none", w)
	}
	if errs := errorMarkers(f, 1); len(errs) != 0 {
		t.Fatalf("left out: state errors %q; want none", errs)
	}
	f.Advance(time.Minute) // its event lands
	f.AddComment(1, "owner", "poke")
	if w := d.pass(t); !slices.Equal(w, []string{"POST /issues/1/comments", "POST /issues/2/comments", "PATCH /issues/2"}) {
		t.Errorf("poked: writes %q; want the type error and the digest's", w)
	}
	if errs := errorMarkers(f, 1); !slices.Equal(errs, []string{fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", key)}) {
		t.Errorf("state errors %q; want the one type error, keyed %d", errs, key)
	}
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("after the type error: writes %q; want none", w)
	}
}

// An untrusted change, or removal, whose event never lands is held back
// for github.HoldBound, then left out for github.UnexplainedWait, with no
// write, no type error, no mention and no read of the issue, then reverted
// once, keyed by the type and the newest event. The comment names the type
// as GitHub shows it, or says the issue had no type.
func TestIssueTypeWaitNeverLands(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, typ, comment string
	}{
		{"a change", "Epic", issueTypeWaitFallback("Epic", "Task", "issue-type:epic@9001")},
		{"a removal", "", issueTypeWaitFallback("", "Task", "issue-type:@9001")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := issueTypeRevertReady(nil)(t)
			d := issueTypeRevertDaemon(t, f)
			news := digestRunNews(f, 2)
			f.LagEvents(time.Hour)
			f.SetTypeBy(1, "mallory", tc.typ)
			for _, wait := range []time.Duration{0, github.HoldBound / 2, github.HoldBound/2 + time.Second} {
				f.Advance(wait)
				if w := d.pass(t); len(w) != 0 {
					t.Errorf("after %v more: writes %q; want none", wait, w)
				}
			}
			issueTypeRevertCheck(t, f, tc.typ)
			before := issueTypeWaitReads(f)
			for range 4 {
				f.Advance(time.Minute)
				if w := d.pass(t); len(w) != 0 {
					t.Errorf("left out: writes %q; want none", w)
				}
			}
			if n := issueTypeWaitReads(f) - before; n != 0 {
				t.Errorf("issue 1 read %d times while its type was left out and not due; want none", n)
			}
			issueTypeRevertCheck(t, f, tc.typ)
			f.Advance(time.Minute) // past the wait
			if w := d.pass(t); !slices.Equal(w, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
				t.Errorf("at the wait: writes %q; want the revert alone", w)
			}
			issueTypeRevertCheck(t, f, "Task", tc.comment)
			if w := d.pass(t); len(w) != 0 {
				t.Errorf("after the revert: writes %q; want none", w)
			}
			if got := digestRunNews(f, 2); got != news {
				t.Errorf("%d digest announcements; want %d", got, news)
			}
		})
	}
}

// issueTypeWaitPass is typeErrPass, on a repo that uses issue types, but
// with the memory of unexplained labels and types held for the pass: one
// that finds issue 1 held back runs again github.UnexplainedWait later, by
// the fake's clock, as the loop would, from a fresh read. A pass builds its
// memory afresh, as the daemon does on restart.
func issueTypeWaitPass(t testing.TB) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		approve, ack := daemon.ApproveByReaction(typeErrTrust), daemon.ErrorByReaction(typeErrTrust)
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug, IssueTypes: true, Unexplained: &github.Unexplained{}},
			Trust: typeErrTrust,
			Reactions: func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, d daemon.Dirt) error {
				if err := approve(ctx, a, s, d); err != nil {
					return err
				}
				return ack(ctx, a, s, d)
			}}
		st.Commands = daemon.Commands(typeErrTrust)
		st.Commands["approve"] = daemon.ApproveCommand()
		for i := 0; ; i++ {
			s, err := c.Snapshot(context.Background(), "o", "r", 1)
			if err != nil {
				return err
			}
			if _, err = st.Run(context.Background(), s); i > 0 || !errors.Is(err, daemon.ErrLabelsHeld) {
				return err
			}
			f.Advance(github.UnexplainedWait + time.Second)
		}
	}
}

// The revert of a type no event explains, after github.UnexplainedWait, is
// safe to replay: the comment, then the type set back. A pass after it
// writes nothing.
func TestIssueTypeWaitReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, typ, comment string
	}{
		{"a change", "Epic", issueTypeWaitFallback("Epic", "Task", "issue-type:epic@9001")},
		{"a removal", "", issueTypeWaitFallback("", "Task", "issue-type:@9001")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			setup := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
				f.LagEvents(time.Hour)
				f.SetTypeBy(1, "mallory", tc.typ)
			})
			pass := issueTypeWaitPass(t)
			githubtest.Replay(t, setup, pass)
			f := setup(t)
			errAckRun(t, f, pass)
			issueTypeRevertCheck(t, f, "Task", tc.comment)
			if w := digestWrites(f, 0); !slices.Equal(w, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
				t.Errorf("writes %q; want the comment and one PATCH of the issue", w)
			}
			typeErrQuiet(t, f, pass)
		})
	}
}

// An org renames the type Task, here to Bug, a name the pipeline knows
// that can be at ready, so the issue gets no type error. The issue shows
// Bug, with Task's ID, while the owner's issue-type event keeps the name
// Task. The types are compared by ID, so the type that stands is the
// issue's: it's neither held back, left out nor reverted, then or later.
// By name, it would be reverted to Task after github.UnexplainedWait, which
// GitHub refuses, as the org has no Task any more.
func TestIssueTypeWaitRenamedInOrg(t *testing.T) {
	t.Parallel()
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	f.RenameOrgIssueType("Task", "Bug")
	f.AddComment(1, "owner", "poke")
	for _, wait := range []time.Duration{0, github.HoldBound + time.Second, github.UnexplainedWait} {
		f.Advance(wait)
		if w := d.pass(t); len(w) != 0 {
			t.Errorf("%v on: writes %q; want none", wait, w)
		}
	}
	issueTypeRevertCheck(t, f, "Bug", "poke")
}
