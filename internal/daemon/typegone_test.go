package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #409: an untrusted change of the issue type whose revert
// would set back a type the org has deleted is reported once, as a
// typegone state error, and the issue goes on with the type that stands.
// A refused read of the org's types keeps the timeline's name, so a revert
// with no rename goes through.

// typeGoneWant is the App's typegone error, keyed key, for mallory's
// change to the type typ, or with unexplained, for the type typ no event
// explains, whose revert to Task the org's deletion of Task blocks: typ is
// a code span, as checkCode fences it.
func typeGoneWant(typ string, unexplained bool, key string) string {
	why := "`mallory` changed it, and only trusted users can change the issue type"
	if unexplained {
		why = "no change by a trusted user or the App explains it, and only trusted users can change the issue type"
	}
	return fmt.Sprintf("This issue has the issue type %s, but %s. "+
		"I can't set back the type before it, `Task`: the org no longer has that type, as once it's deleted. "+
		"So I've left the type as it is, and act on `Task` as if the issue had it, until a trusted user changes the type. "+
		"A 👍 on this comment acknowledges it.\n\n<!-- issuedog:error kind=typegone key=%s -->", typ, why, key)
}

// typeGoneReady is issueTypeRevertReady's issue at ready, typed Task by the
// owner, after the org deleted Task, keeping Bug and Epic, and mallory
// changed the type to typ (event 9003), with the fake's events lag late.
func typeGoneReady(typ string, lag time.Duration) func(testing.TB) *githubtest.Fake {
	return issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
		f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Bug"}, githubtest.OrgIssueType{Name: "Epic"})
		f.LagEvents(lag)
		flagWant(t, f.SetTypeBy(1, "mallory", typ), 9003)
	})
}

// With the type that stands deleted in the org, mallory's change can't be
// reverted: step 1 posts one typegone error, keyed by the change, under
// Replay, and writes nothing else, so the type stays. The pass acts on
// Task, which fits ready, so there's no type error for mallory's type,
// which wouldn't. A pass after it writes nothing. The type's name is the
// org's text, so it's a code span that reads as it is, backticks, a marker
// and a mention included. A change whose event never lands, after
// github.UnexplainedWait, is reported the same way, keyed by its type and
// the newest event.
func TestTypeGoneReport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, typ string
		lag       time.Duration
		pass      func(testing.TB) func(*githubtest.Fake) error
		comment   string
	}{
		{"a plain name", "Epic", 0, func(t testing.TB) func(*githubtest.Fake) error { return typeErrPass(t, true) },
			typeGoneWant("`Epic`", false, "9003")},
		{"a backtick, a marker and a mention", "Ep`ic <!-- x --> @owner", 0, func(t testing.TB) func(*githubtest.Fake) error { return typeErrPass(t, true) },
			typeGoneWant("``Ep`ic &lt;!-- x --> @owner``", false, "9003")},
		{"a change whose event never lands", "Epic", time.Hour, issueTypeWaitPass, typeGoneWant("`Epic`", true, "issue-type:epic@9001")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			setup := typeGoneReady(tc.typ, tc.lag)
			pass := tc.pass(t)
			githubtest.Replay(t, setup, pass)
			f := setup(t)
			errAckRun(t, f, pass)
			want := []string{"<!-- issuedog:transition from=intake to=ready run=rready -->", tc.comment}
			if got := commentBodies(f, 1); !slices.Equal(got, want) {
				t.Errorf("comments %q; want %q", got, want)
			}
			if w := digestWrites(f, 0); !slices.Equal(w, []string{"POST /issues/1/comments"}) {
				t.Errorf("writes %q; want the error alone", w)
			}
			if got := f.State().Issues[0].Type; got != tc.typ {
				t.Errorf("type %q; want mallory's %q, which can't be set back", got, tc.typ)
			}
			typeErrQuiet(t, f, pass)
		})
	}
}

// The whole loop, with the digest: the error is posted once and announced
// once, the owner is assigned, and stage work gets the issue with the type
// that stands. An unchanged pass after the report writes nothing. The
// owner's own change of the type ends the error: the digest drops it, and
// the issue goes on with the owner's type.
func TestTypeGoneLoop(t *testing.T) {
	t.Parallel()
	f := issueTypeRevertReady(nil)(t)
	d := issueTypeRevertDaemon(t, f)
	d.d.Assign = true
	var types []string // the issue's type, as each pass's stage work got it
	d.d.Work = func(_ context.Context, ready []daemon.Ready) error {
		for _, r := range ready {
			if r.S.Number == 1 {
				types = append(types, r.S.Type)
			}
		}
		return nil
	}
	news := digestRunNews(f, 2)
	f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Bug"}, githubtest.OrgIssueType{Name: "Epic"})
	key := f.SetTypeBy(1, "mallory", "Epic")
	d.pass(t)
	if got, want := commentBodies(f, 1)[1:], []string{typeGoneWant("`Epic`", false, fmt.Sprint(key))}; !slices.Equal(got, want) {
		t.Errorf("comments %q; want %q", got, want)
	}
	if got := digestRunNews(f, 2); got != news+1 {
		t.Errorf("%d digest announcements; want %d", got, news+1)
	}
	item := fmt.Sprintf("State error `typegone`, keyed `%d`.", key)
	if body := f.State().Issues[1].Body; !strings.Contains(body, item) {
		t.Errorf("digest body %q; want the item %q", body, item)
	}
	if got := f.State().Issues[0].Assignees; !slices.Equal(got, []string{"owner"}) {
		t.Errorf("assignees %q; want the owner", got)
	}
	for pass := range 2 {
		from := len(f.Requests())
		if w := d.pass(t); len(w) != 0 || typeRenameOrgReads(f, from) != 1 {
			t.Errorf("unchanged pass %d: writes %q, %d reads of the org's types; want none, and the one read of the open error's visit",
				pass, w, typeRenameOrgReads(f, from))
		}
	}
	if got := f.State().Issues[0].Type; got != "Epic" {
		t.Errorf("type %q; want mallory's Epic, which can't be set back", got)
	}
	if !slices.Equal(types, []string{"task", "task", "task"}) {
		t.Errorf("stage work got the type %q; want the type that stands, task, on each pass", types)
	}
	f.SetTypeBy(1, "owner", "Bug")
	d.pass(t)
	if body := f.State().Issues[1].Body; strings.Contains(body, item) {
		t.Errorf("after the owner's change, the digest body %q still has %q", body, item)
	}
	if got := f.State().Issues[0]; got.Type != "Bug" || len(got.Assignees) != 0 || len(commentBodies(f, 1)) != 2 {
		t.Errorf("after the owner's change: type %q, assignees %q, comments %q; want Bug, none, and no more comments", got.Type, got.Assignees, commentBodies(f, 1))
	}
	if w := d.pass(t); len(w) != 0 {
		t.Errorf("a pass after the owner's change wrote %q; want nothing", w)
	}
	if types[len(types)-1] != "bug" {
		t.Errorf("stage work last got the type %q; want the owner's bug", types[len(types)-1])
	}
}

// Without Issue Types read, GitHub refuses the read of the org's types,
// with a 403 on every pass. A revert with no rename involved still goes
// through, under Replay, by the timeline's name: one comment, one PATCH,
// and no state error. A pass after it writes nothing.
func TestTypeGoneRefusedRevert(t *testing.T) {
	t.Parallel()
	setup := issueTypeRevertReady(func(t testing.TB, f *githubtest.Fake) {
		f.TypeGoneRefuseList(http.StatusForbidden)
		flagWant(t, f.SetTypeBy(1, "mallory", "Epic"), 9003)
	})
	pass := typeErrPass(t, true)
	githubtest.Replay(t, setup, pass)
	f := setup(t)
	errAckRun(t, f, pass)
	issueTypeRevertCheck(t, f, "Task", issueTypeRevertWant("restored", "Task", 9003))
	if w := digestWrites(f, 0); !slices.Equal(w, []string{"POST /issues/1/comments", "PATCH /issues/1"}) {
		t.Errorf("writes %q; want the comment and one PATCH of the issue", w)
	}
	typeErrQuiet(t, f, pass)
}
