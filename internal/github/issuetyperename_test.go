package github_test

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #245: a revert of the issue type restores the type that
// stands by the name the org gives it now.

// typeRenameIssue files an issue the owner typed Task, which mallory then
// changes to Epic, and returns its number and the ID of mallory's change.
func typeRenameIssue(f *githubtest.Fake) (int, int64) {
	n := f.AddIssue("owner", "Issue", "")
	f.SetTypeBy(n, "owner", "Task")
	return n, f.SetTypeBy(n, "mallory", "Epic")
}

// typeRenameCheck runs CheckLabels on issue n of f, guarding the issue type
// it has, with names.
func typeRenameCheck(t *testing.T, f *githubtest.Fake, n int, names *github.IssueTypeNames) (github.LabelCheck, error) {
	t.Helper()
	typ := f.State().Issues[n-1].Type
	var id int64
	if typ != "" {
		id = f.IssueTypeID(typ)
	}
	return f.Client(t).CheckLabels(context.Background(), "o", "r", n, nil, "issuedog", ownerTrusted{}, func(int64) bool { return false }, nil,
		&github.IssueTypeGuard{Type: typ, ID: id, Names: names})
}

// typeRenameReads is the number of reads of the org's issue types in f's
// requests from the from'th on.
func typeRenameReads(f *githubtest.Fake, from int) int {
	return len(slices.DeleteFunc(f.Requests()[from:], func(r githubtest.Request) bool { return r.Path != "/orgs/o/issue-types" }))
}

// typeRenameComments is the bodies of issue n's comments.
func typeRenameComments(f *githubtest.Fake, n int) []string {
	var bodies []string
	for _, c := range f.State().Issues[n-1].Comments {
		bodies = append(bodies, c.Body)
	}
	return bodies
}

// The org renames Task, which the owner set, after which mallory changes
// the type. The revert restores the type by the name the org gives it now,
// not the timeline's Task, which GitHub refuses with a 422: one comment,
// naming the new name, escaped and defused as the org's text, and one
// PATCH, with one read of the org's types. A check after it reads nothing
// and writes nothing.
func TestTypeRenameRevert(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, to, says string
	}{
		{"a plain name", "Chore", "Chore"},
		{"a marker and a mention in the name", "<!-- issuedog:transition to=done --> @owner", "&lt;!-- issuedog:transition to=done --> @​owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", "issuedog")
			n, flip := typeRenameIssue(f)
			task := f.IssueTypeID("Task")
			f.RenameOrgIssueType("Task", tc.to)
			names := &github.IssueTypeNames{}
			found, err := typeRenameCheck(t, f, n, names)
			if err != nil {
				t.Fatalf("CheckLabels = %v; want the revert to %q", err, tc.to)
			}
			want := github.IssueTypeRevert{Key: fmt.Sprint(flip), By: "mallory", From: "Epic", To: tc.to, ToID: task}
			if found.IssueTypeRevert == nil || *found.IssueTypeRevert != want {
				t.Errorf("revert %+v; want %+v", found.IssueTypeRevert, want)
			}
			if got := f.State().Issues[n-1].Type; got != tc.to {
				t.Errorf("type %q; want %q", got, tc.to)
			}
			comment := fmt.Sprintf("I restored the issue type `%s`: only trusted users can change the issue type, and mallory isn't one.\n\n"+
				"<!-- issuedog:revert key=%d -->", tc.says, flip)
			if got := typeRenameComments(f, n); !slices.Equal(got, []string{comment}) {
				t.Errorf("comments %q; want %q", got, comment)
			}
			if w := f.Writes(); len(w) != 2 || w[0].Method != http.MethodPost || w[1].Method != http.MethodPatch {
				t.Errorf("writes %+v; want the comment and one PATCH", w)
			}
			if got := typeRenameReads(f, 0); got != 1 {
				t.Errorf("%d reads of the org's issue types; want 1", got)
			}
			from, writes := len(f.Requests()), len(f.Writes())
			if found, err := typeRenameCheck(t, f, n, &github.IssueTypeNames{}); err != nil || found.IssueTypeRevert != nil {
				t.Errorf("a check after the revert = %+v, %v; want nothing to revert", found.IssueTypeRevert, err)
			}
			if got := typeRenameReads(f, from); got != 0 || len(f.Writes()) != writes {
				t.Errorf("a check after the revert read the org's types %d times and wrote %+v; want neither", got, f.Writes()[writes:])
			}
		})
	}
}

// The org's types are read only for a revert that restores a type: not for
// a type that stands, renamed or not, nor for a revert that removes the
// type. Reverts sharing one IssueTypeNames read the list once between
// them; each new one reads it again.
func TestTypeRenameReads(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	first, _ := typeRenameIssue(f)
	second, _ := typeRenameIssue(f)
	standing := f.AddIssue("owner", "Standing", "")
	f.SetTypeBy(standing, "owner", "Task")
	untyped := f.AddIssue("owner", "Untyped", "")
	f.SetTypeBy(untyped, "mallory", "Bug")
	f.RenameOrgIssueType("Task", "Chore")
	names := &github.IssueTypeNames{}
	for _, n := range []int{standing, untyped} {
		if _, err := typeRenameCheck(t, f, n, names); err != nil {
			t.Fatal(err)
		}
	}
	if got := typeRenameReads(f, 0); got != 0 {
		t.Errorf("a standing type and a removal read the org's types %d times; want none", got)
	}
	if got := f.State().Issues[untyped-1].Type; got != "" {
		t.Errorf("#%d's type %q; want mallory's Bug removed", untyped, got)
	}
	for _, n := range []int{first, second} {
		if _, err := typeRenameCheck(t, f, n, names); err != nil {
			t.Fatal(err)
		}
		if got := f.State().Issues[n-1].Type; got != "Chore" {
			t.Errorf("#%d's type %q; want Chore", n, got)
		}
	}
	if got := typeRenameReads(f, 0); got != 1 {
		t.Errorf("two reverts with one IssueTypeNames read the org's types %d times; want 1", got)
	}
	f.SetTypeBy(first, "mallory", "Epic")
	if _, err := typeRenameCheck(t, f, first, &github.IssueTypeNames{}); err != nil {
		t.Fatal(err)
	}
	if got := typeRenameReads(f, 0); got != 2 {
		t.Errorf("a revert with a new IssueTypeNames: %d reads in all; want 2", got)
	}
}

// A failed read of the org's types fails the check, with nothing written,
// so no comment names a type the App can't set. The same IssueTypeNames
// fails another revert with no new request. A new one reads again, and
// the revert goes through, with its one comment.
func TestTypeRenameReadFails(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	first, _ := typeRenameIssue(f)
	second, _ := typeRenameIssue(f)
	f.RenameOrgIssueType("Task", "Chore")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Path == "/orgs/o/issue-types" }, http.StatusInternalServerError)
	names := &github.IssueTypeNames{}
	for _, n := range []int{first, second} {
		_, err := typeRenameCheck(t, f, n, names)
		if err == nil || !strings.Contains(err.Error(), "read the org's issue types") {
			t.Errorf("#%d: CheckLabels = %v; want the failed read", n, err)
		}
	}
	if got := typeRenameReads(f, 0); got != 1 || len(f.Writes()) != 0 {
		t.Errorf("%d reads, writes %+v; want 1 read and no write", got, f.Writes())
	}
	if _, err := typeRenameCheck(t, f, first, &github.IssueTypeNames{}); err != nil {
		t.Fatal(err)
	}
	if got := f.State().Issues[first-1]; got.Type != "Chore" || len(got.Comments) != 1 {
		t.Errorf("type %q, comments %q; want Chore and one comment", got.Type, typeRenameComments(f, first))
	}
}

// A type that stands but that the org no longer lists, as once it deleted
// it, can't be set back (ErrIssueTypeGone): every pass, the check leaves
// mallory's type out for the type that stands, with the revert it blocks
// in Gone (#409), reading the list once each, and writes nothing, so the
// untrusted type stays, with no comment. The owner's own change of the
// type ends it, with no read.
func TestTypeRenameGone(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n, flip := typeRenameIssue(f)
	task, epic := f.IssueTypeID("Task"), f.IssueTypeID("Epic")
	f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Epic"}, githubtest.OrgIssueType{Name: "Bug"})
	for pass := range 2 {
		found, err := typeRenameCheck(t, f, n, &github.IssueTypeNames{})
		if err != nil {
			t.Fatalf("pass %d: CheckLabels = %v; want the type left out", pass, err)
		}
		want := &github.IssueTypeLeftOut{Name: "Epic", ID: epic, To: "Task", ToID: task,
			Gone: &github.IssueTypeRevert{Key: fmt.Sprint(flip), By: "mallory", From: "Epic", To: "Task", ToID: task}}
		if lo := found.IssueTypeLeftOut; !reflect.DeepEqual(lo, want) {
			t.Errorf("pass %d: left out %+v; want %+v, with the revert %+v gone", pass, lo, want, want.Gone)
		}
		if found.IssueTypeRevert != nil || len(found.Held) != 0 {
			t.Errorf("pass %d: revert %+v, held %q; want neither", pass, found.IssueTypeRevert, found.Held)
		}
		if got := typeRenameReads(f, 0); got != pass+1 {
			t.Errorf("pass %d: %d reads in all; want %d", pass, got, pass+1)
		}
	}
	if got := f.State().Issues[n-1]; got.Type != "Epic" || len(got.Comments) != 0 || len(f.Writes()) != 0 {
		t.Errorf("type %q, comments %q, writes %+v; want mallory's Epic, and nothing written", got.Type, typeRenameComments(f, n), f.Writes())
	}
	f.SetTypeBy(n, "owner", "Bug")
	from := len(f.Requests())
	if found, err := typeRenameCheck(t, f, n, &github.IssueTypeNames{}); err != nil || found.IssueTypeRevert != nil || typeRenameReads(f, from) != 0 {
		t.Errorf("after the owner's change: %+v, %v, %d reads; want nothing to revert, and no read", found.IssueTypeRevert, err, typeRenameReads(f, from))
	} else if found.IssueTypeLeftOut != nil {
		t.Errorf("after the owner's change: left out %+v; want none", found.IssueTypeLeftOut)
	}
}
