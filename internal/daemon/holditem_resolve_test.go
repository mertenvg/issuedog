package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// Hold items (#192) once T4 (#188) resolves triggers: through Iterate with
// the digest, assignment and the reaction trigger that ends a discussion
// hold (ResolveByReaction) on, and owner to notify. #1 is the pinned
// digest, and #2 the owner's issue at ready.

// holdItemResolveDaemon is holdItemDaemon with ResolveByReaction as its
// reaction trigger, as newStep wires it.
func holdItemResolveDaemon(t testing.TB, f *githubtest.Fake, o *origin) *daemon.Daemon {
	t.Helper()
	d := holdItemDaemon(t, f, o)
	d.Step.Reactions = daemon.ResolveByReaction(d.Step.CurrentTrust())
	return d
}

// A trusted 👍 that resolves the last open trigger in step 1 takes the
// hold item away in the same pass, as holdItemFind runs after step 1, and
// the notify assignee with it. Under the owner's /hold, #2 stays held, with
// the record that the discussion ended; with no owner hold, it goes to
// clarifying. An unchanged pass, and a restart, write nothing, and the
// unchanged pass asks only conditional GETs answered 304.
func TestHoldItemGoneAfterThumbsUp(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fake  func(t testing.TB) *githubtest.Fake
		label string
	}{
		{"under an owner hold", holdItemOwnerHeld, "stage:held"},
		{"alone", holdItemFake, "stage:clarifying"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, o := tc.fake(t), digestNotifyOrigin(t)
			d := holdItemResolveDaemon(t, f, o)
			c := f.AddComment(2, "mallory", "Wait, is this right?")
			iterate(t, d)
			if body := f.State().Issues[0].Body; !strings.Contains(body, fmt.Sprintf("posted comment %d", c)) {
				t.Fatalf("digest body %q; want #2's hold listed", body)
			}
			if got := assignees(f, 2); !slices.Equal(got, []string{"owner"}) {
				t.Fatalf("#2's assignees %q; want owner while it's held for discussion", got)
			}
			f.AddReaction(c, "owner", "+1")
			iterate(t, d)
			if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{tc.label}) {
				t.Errorf("#2: labels %q, newest comment %q; want %s", l, newestComment(f, 2), tc.label)
			}
			if body := f.State().Issues[0].Body; body != digestList {
				t.Errorf("digest body %q; want nothing listed once the 👍 resolved the trigger", body)
			}
			if got := assignees(f, 2); len(got) != 0 {
				t.Errorf("#2's assignees %q; want none once nothing waits", got)
			}
			holdItemQuietWith(t, f, d, "resolved", func() *daemon.Daemon { return holdItemResolveDaemon(t, f, o) })
		})
	}
}

// A held issue's unchanged pass costs no rate limit with the digest and
// assignment on, though step 1, the reaction trigger and the hold item
// each look for its triggers (discFind), and read the reactions of the
// comments that may resolve them: #2 is held for discussion, under the
// owner's /hold that writes no marker (Q2b), with mallory's comment the
// owner 👍'd, her open comment, and her rename, with its notice. The item
// lists the open comment and the rename, and names the owner hold.
func TestHoldItemResolvedUnchangedPass(t *testing.T) {
	f, o := holdItemFake(t), digestNotifyOrigin(t)
	d := holdItemResolveDaemon(t, f, o)
	first := f.AddComment(2, "mallory", "Wait.")
	iterate(t, d)
	f.AddComment(2, "owner", "/hold")
	open := f.AddComment(2, "mallory", "And another thing.")
	f.RenameIssue(2, "mallory", "Renamed")
	iterate(t, d)
	f.AddReaction(first, "owner", "+1")
	iterate(t, d)
	if l := f.State().Issues[1].Labels; !slices.Equal(l, []string{"stage:held"}) {
		t.Fatalf("#2: labels %q; want it held", l)
	}
	body := f.State().Issues[0].Body
	if strings.Contains(body, fmt.Sprintf("posted comment %d", first)) || !strings.Contains(body, fmt.Sprintf("posted comment %d", open)) ||
		!strings.Contains(body, "`mallory` renamed this issue to “Renamed”. A code owner's 👍 on its notice accepts it.") ||
		!strings.Contains(body, "It's on an owner hold too") {
		t.Errorf("digest body %q; want the open comment and the rename listed, with the owner's /hold, and not the 👍'd comment", body)
	}
	holdItemQuietWith(t, f, d, "held", func() *daemon.Daemon { return holdItemResolveDaemon(t, f, o) })
}

// A /hold on the issue that writes no marker, as on an issue held for
// discussion already (Q2b), is named on the hold item as an owner hold;
// once the issue's own /resume ends it, while a trigger after the /resume
// keeps it held, it's no longer named.
func TestHoldItemOwnHoldEndedByResume(t *testing.T) {
	f, o := holdItemFake(t), digestNotifyOrigin(t)
	d := holdItemResolveDaemon(t, f, o)
	f.AddComment(2, "mallory", "Wait.")
	iterate(t, d)
	f.AddComment(2, "owner", "/hold")
	iterate(t, d)
	if body := f.State().Issues[0].Body; !strings.Contains(body, "It's on an owner hold too") {
		t.Fatalf("digest body %q; want the /hold named as an owner hold", body)
	}
	f.AddComment(2, "owner", "/resume")
	late := f.AddComment(2, "mallory", "One more thing.")
	iterate(t, d)
	if l, c := f.State().Issues[1].Labels, newestComment(f, 2); !slices.Equal(l, []string{"stage:held"}) || !strings.Contains(c, "<!-- issuedog:ended reason=owner key=c") {
		t.Fatalf("#2: labels %q, newest comment %q; want it held for the late comment, with the record that the /resume ended the /hold", l, c)
	}
	body := f.State().Issues[0].Body
	if strings.Contains(body, "It's on an owner hold too") || !strings.Contains(body, fmt.Sprintf("posted comment %d", late)) {
		t.Errorf("digest body %q; want the late comment listed, and no owner hold named", body)
	}
	holdItemQuietWith(t, f, d, "resumed", func() *daemon.Daemon { return holdItemResolveDaemon(t, f, o) })
}
