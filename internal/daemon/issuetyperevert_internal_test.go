package daemon

import (
	"context"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// CheckLabels sets the snapshot's type to the type a revert set back: the
// issue type that stands, or with none, the type:* label's, which then
// follows the labels as for an issue read with no issue type (#182).
func TestIssueTypeRevertSnapshot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		file       func(f *githubtest.Fake)
		typ        string
		fromLabels bool
		afterLabel string // s.Type once the labels are set to none
	}{
		{"set back", func(f *githubtest.Fake) {
			f.AddIssue("owner", "Issue", "body")
			f.SetTypeBy(1, "owner", "Task")
			f.SetTypeBy(1, "mallory", "Epic")
		}, "task", false, "task"},
		{"removed, with a type label", func(f *githubtest.Fake) {
			f.AddIssue("owner", "Issue", "body", "type:story")
			f.SetTypeBy(1, "mallory", "Epic")
		}, "story", true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", "issuedog")
			tc.file(f)
			a := &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog", IssueTypes: true}
			s := a.snapshot(t, 1)
			if s.Type != "epic" || s.TypeFromLabels {
				t.Fatalf("read type %q, from labels %t; want mallory's epic", s.Type, s.TypeFromLabels)
			}
			if err := a.CheckLabels(context.Background(), s, ownerOnly{}, func(int64) bool { return false }); err != nil {
				t.Fatal(err)
			}
			if s.Type != tc.typ || s.TypeFromLabels != tc.fromLabels {
				t.Errorf("type %q, from labels %t; want %q, %t", s.Type, s.TypeFromLabels, tc.typ, tc.fromLabels)
			}
			s.SetLabels(nil)
			if s.Type != tc.afterLabel {
				t.Errorf("with no labels, type %q; want %q", s.Type, tc.afterLabel)
			}
		})
	}
}
