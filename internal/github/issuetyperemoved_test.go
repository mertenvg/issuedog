package github_test

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #246's second part: a removal of the issue type leaves no
// type, so an untrusted change after one, or an untrusted removal, is
// reverted. The fake gives a removal only a prev_issue_type, as GitHub's
// REST description does. TestTypeRemovedLeftID checks that an issue_type
// on a removal, if GitHub ever gives one, isn't taken for the type it left.

// typeRemovedCheck runs CheckLabels on issue n of f, guarding the type it
// has now, with its ID, as the daemon passes them.
func typeRemovedCheck(t testing.TB, f *githubtest.Fake, n int) (github.LabelCheck, error) {
	t.Helper()
	typ := f.State().Issues[n-1].Type
	var id int64
	if typ != "" {
		id = f.IssueTypeID(typ)
	}
	found, err := f.Client(t).CheckLabels(context.Background(), "o", "r", n, nil, "issuedog", ownerTrusted{}, func(int64) bool { return false },
		&github.Unexplained{}, &github.IssueTypeGuard{Type: typ, ID: id})
	if err != nil {
		return found, fmt.Errorf("check #%d: %w", n, err)
	}
	return found, nil
}

// A removal stands for no type: after the owner removes Task, mallory's Task added back is an untrusted
// change, reverted by removing it, and mallory's removal of the owner's
// Task is one too, reverted by setting Task back. Each is one comment and
// one PATCH, safe to replay, with no read of the org's types for the
// removal.
func TestTypeRemovedRevert(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// more sets the types after the owner's Task, and returns the ID of
		// mallory's change.
		more     func(f *githubtest.Fake, n int) int64
		did, typ string
		toID     bool // the revert's ToID is Task's
	}{
		{"the removed type added back", func(f *githubtest.Fake, n int) int64 {
			f.SetTypeBy(n, "owner", "")
			return f.SetTypeBy(n, "mallory", "Task")
		}, "I removed the issue type `Task`", "", false},
		{"an untrusted removal", func(f *githubtest.Fake, n int) int64 {
			return f.SetTypeBy(n, "mallory", "")
		}, "I restored the issue type `Task`", "Task", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var flip int64
			setup := func(t testing.TB) *githubtest.Fake {
				f := githubtest.New(t, "o/r", "issuedog")
				n := f.AddIssue("owner", "Issue", "")
				f.SetTypeBy(n, "owner", "Task")
				flip = tc.more(f, n)
				return f
			}
			pass := func(f *githubtest.Fake) error {
				_, err := typeRemovedCheck(t, f, 1)
				return err
			}
			githubtest.Replay(t, setup, pass)
			f := setup(t)
			found, err := typeRemovedCheck(t, f, 1)
			if err != nil {
				t.Fatal(err)
			}
			want := github.IssueTypeRevert{Key: fmt.Sprint(flip), By: "mallory", From: "Task"}
			if tc.toID {
				want = github.IssueTypeRevert{Key: fmt.Sprint(flip), By: "mallory", To: "Task", ToID: f.IssueTypeID("Task")}
			}
			if found.IssueTypeRevert == nil || *found.IssueTypeRevert != want || len(found.Held) != 0 {
				t.Fatalf("revert %+v, held %q; want %+v, and nothing held", found.IssueTypeRevert, found.Held, want)
			}
			if got := f.State().Issues[0].Type; got != tc.typ {
				t.Errorf("type %q; want %q", got, tc.typ)
			}
			comment := fmt.Sprintf("%s: only trusted users can change the issue type, and mallory isn't one.\n\n<!-- issuedog:revert key=%d -->", tc.did, flip)
			if got := typeRenameComments(f, 1); !slices.Equal(got, []string{comment}) {
				t.Errorf("comments %q; want %q", got, comment)
			}
			if w := f.Writes(); len(w) != 2 || w[0].Path != "/issues/1/comments" || w[1].Method != "PATCH" || w[1].Path != "/issues/1" {
				t.Errorf("writes %+v; want the comment and one PATCH", w)
			}
			if !tc.toID && typeRenameReads(f, 0) != 0 {
				t.Errorf("a revert by removal read the org's types %d times; want none", typeRenameReads(f, 0))
			}
			writes := len(f.Writes())
			if found, err := typeRemovedCheck(t, f, 1); err != nil || found.IssueTypeRevert != nil || len(f.Writes()) != writes {
				t.Errorf("a check after the revert: %+v, %v, writes %+v; want nothing", found.IssueTypeRevert, err, f.Writes()[writes:])
			}
		})
	}
}
