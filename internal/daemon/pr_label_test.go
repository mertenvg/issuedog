package daemon_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A task's PR carries exactly its task's priority label (SPEC §5, §9.6),
// on the fakes of pr_item_test.go: the task, #1, at priority:p1, and its
// PR, #2, the App's, with priority:p1.

// prLabelWrites are the label writes on the PR, #2, from the from'th
// write on.
func prLabelWrites(f *githubtest.Fake, from int) []string {
	var got []string
	for _, w := range writesOf(f)[from:] {
		if strings.HasPrefix(w, "POST /issues/2/labels") || strings.HasPrefix(w, "DELETE /issues/2/labels/") {
			got = append(got, w)
		}
	}
	return got
}

// Whoever changes the PR's priority label, mallory, untrusted, or the
// owner, by an add, a removal or a change, the next pass makes it the
// task's again, writing only the difference, and posts no comment. A
// /priority on the task moves the PR's label in the same pass. A pass
// after writes nothing, and makes no request about the PR.
func TestPRLabelFollowsTheTask(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		change func(f *githubtest.Fake)
		posts  int // the comments the change posts itself
		writes []string
		want   string // the PR's label after
	}{
		{"an untrusted add", func(f *githubtest.Fake) { f.AddLabel(2, "mallory", "priority:p0") }, 0,
			[]string{"DELETE /issues/2/labels/priority:p0"}, "priority:p1"},
		{"an untrusted removal", func(f *githubtest.Fake) { f.RemoveLabel(2, "mallory", "priority:p1") }, 0,
			[]string{"POST /issues/2/labels"}, "priority:p1"},
		{"the owner's change", func(f *githubtest.Fake) {
			f.RemoveLabel(2, digestRunOwner, "priority:p1")
			f.AddLabel(2, digestRunOwner, "priority:p3")
		}, 0, []string{"DELETE /issues/2/labels/priority:p3", "POST /issues/2/labels"}, "priority:p1"},
		{"/priority on the task", func(f *githubtest.Fake) { f.AddComment(1, digestRunOwner, "/priority p0") }, 1,
			[]string{"DELETE /issues/2/labels/priority:p1", "POST /issues/2/labels"}, "priority:p0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := prItemOrigin(t)
			f := prItemFake(t, o)
			d := prItemDaemon(t, f, o)
			iterate(t, d)
			iterate(t, d)
			comments := len(f.State().Issues[0].Comments) + len(f.State().Issues[1].Comments)

			tt.change(f)
			from := len(f.Writes())
			iterate(t, d)
			if got := prLabelWrites(f, from); !slices.Equal(got, tt.writes) {
				t.Errorf("the pass wrote %q on the PR's labels; want %q", got, tt.writes)
			}
			if got := f.State().Issues[1].Labels; !slices.Equal(got, []string{tt.want}) {
				t.Errorf("the PR's labels = %q; want %s", got, tt.want)
			}
			if got := len(f.State().Issues[0].Comments) + len(f.State().Issues[1].Comments); got != comments+tt.posts {
				t.Errorf("the task and the PR have %d comments, want %d: the label is put back with no comment", got, comments+tt.posts)
			}
			iterate(t, d)
			prItemQuiet(t, f, d, tt.name)
		})
	}
}

// A closed PR's label, and one whose task is closed, are left alone, as
// only an open PR of an open task carries its task's priority.
func TestPRLabelLeavesClosed(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name  string
		close func(f *githubtest.Fake)
	}{
		{"the PR", func(f *githubtest.Fake) { f.ClosePR(2, digestRunOwner) }},
		{"the task", func(f *githubtest.Fake) {
			f.RemoveLabel(1, digestRunOwner, "stage:pr-open")
			f.AddLabel(1, digestRunOwner, "stage:discarded")
			f.CloseBy(1, digestRunOwner)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			o := prItemOrigin(t)
			f := prItemFake(t, o)
			d := prItemDaemon(t, f, o)
			iterate(t, d)
			iterate(t, d)
			tt.close(f)
			iterate(t, d)
			f.AddLabel(2, "mallory", "priority:p0")
			from := len(f.Writes())
			iterate(t, d)
			iterate(t, d)
			if got := prLabelWrites(f, from); len(got) != 0 {
				t.Errorf("with %s closed, the passes wrote %q on the PR's labels; want nothing", tt.name, got)
			}
		})
	}
}

// Replay passes for the PR's label and assignee writes, whatever write a
// crash comes at (SPEC §5.2): mallory swapped the PR's priority:p1 for
// priority:p0 and added bug, and bob is assigned, so a pass removes
// priority:p0, adds the task's priority:p1 back, leaves bug, and swaps
// bob for notify.
func TestPRLabelAndAssigneesReplay(t *testing.T) {
	t.Parallel()
	o := prItemOrigin(t)
	setup := func(tb testing.TB) *githubtest.Fake {
		f := prItemFake(tb, o)
		f.AddLabel(2, "mallory", "priority:p0")
		f.RemoveLabel(2, "mallory", "priority:p1")
		f.AddLabel(2, "mallory", "bug")
		f.SetAssignees(2, "bob")
		return f
	}
	pass := func(f *githubtest.Fake) error {
		d := prItemDaemon(t, f, o)
		d.Digest = false
		return d.Iterate(t.Context())
	}
	clean := setup(t)
	if err := pass(clean); err != nil {
		t.Fatal(err)
	}
	want := []string{"DELETE /issues/2/labels/priority:p0", "POST /issues/2/labels", "DELETE /issues/2/assignees", "POST /issues/2/assignees"}
	if got := writesOf(clean); !slices.Equal(got, want) {
		t.Errorf("the clean pass wrote %q; want %q", got, want)
	}
	githubtest.Replay(t, setup, pass)
}
