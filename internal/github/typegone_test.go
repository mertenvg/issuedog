package github_test

import (
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is #409: a refused read of the org's issue types keeps the
// timeline's name, and a type's name is fenced as code that reads as it
// is. A type the org no longer lists is TestTypeRenameGone's.

// typeGoneComment is the App's comment on restoring the type span, as a
// code span, after mallory's change keyed key.
func typeGoneComment(span string, key int64) string {
	return fmt.Sprintf("I restored the issue type %s: only trusted users can change the issue type, and mallory isn't one.\n\n"+
		"<!-- issuedog:revert key=%d -->", span, key)
}

// GitHub refuses the read of the org's issue types: a 403, as without
// Issue Types read, or a 404, as for an org the App can't see, which is no
// empty list. The revert restores the type by the timeline's name, as
// before #245, under Replay: one comment and one PATCH. That holds even
// though the org no longer lists Task, as a 404 can't say so, so no type
// is left out as gone. A second revert in the pass reads nothing more.
//
// A personal account answers the read with the same 404
// (TestOrgIssueTypes), but has no issue types to revert: GitHub refuses
// any type there, and since #302 so does the fake, so it's no case here.
func TestTypeGoneRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		refuse func(f *githubtest.Fake)
	}{
		{"a 403", func(f *githubtest.Fake) { f.TypeGoneRefuseList(http.StatusForbidden) }},
		{"a 404", func(f *githubtest.Fake) { f.TypeGoneRefuseList(http.StatusNotFound) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var flips []int64 // mallory's changes, on the fake setup made last
			setup := func(tb testing.TB) *githubtest.Fake {
				f := githubtest.New(tb, "o/r", "issuedog")
				flips = nil
				for range 2 {
					_, flip := typeRenameIssue(f)
					flips = append(flips, flip)
				}
				f.SetOrgIssueTypes(githubtest.OrgIssueType{Name: "Epic"})
				tc.refuse(f)
				return f
			}
			pass := func(f *githubtest.Fake) error {
				names := &github.IssueTypeNames{}
				for n := 1; n <= 2; n++ {
					found, err := typeRenameCheck(t, f, n, names)
					if err != nil {
						return err
					}
					if found.IssueTypeLeftOut != nil {
						return fmt.Errorf("#%d: left out %+v; want the revert", n, found.IssueTypeLeftOut)
					}
				}
				return nil
			}
			githubtest.Replay(t, setup, pass)
			f := setup(t)
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			for i, flip := range flips {
				n := i + 1
				if got := f.State().Issues[n-1].Type; got != "Task" {
					t.Errorf("#%d's type %q; want Task", n, got)
				}
				if got, want := typeRenameComments(f, n), typeGoneComment("`Task`", flip); !slices.Equal(got, []string{want}) {
					t.Errorf("#%d's comments %q; want %q", n, got, want)
				}
			}
			if w := f.Writes(); len(w) != 4 {
				t.Errorf("writes %+v; want a comment and a PATCH for each", w)
			}
			if got := typeRenameReads(f, 0); got != 1 {
				t.Errorf("%d reads of the org's issue types; want 1", got)
			}
		})
	}
}

// TypeGoneCodeSpan fences text as inline code that reads as it is: one more
// backtick than its longest run, padded when it starts or ends with a
// backtick or a space.
func TestTypeGoneCodeSpan(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Task":   "`Task`",
		"Ta`sk":  "``Ta`sk``",
		"a``b`c": "```a``b`c```",
		"`Epic":  "`` `Epic ``",
		"Epic`":  "`` Epic` ``",
		" Bug":   "`  Bug `",
		"":       "` `",
	} {
		if got := github.TypeGoneCodeSpan(in); got != want {
			t.Errorf("TypeGoneCodeSpan(%q) = %q, want %q", in, got, want)
		}
	}
}

// A type's name with backticks in it, the org's text, stays inside its code
// span in the revert's comment: the name the org renamed Task to, which
// the revert restores, and a type mallory set on an issue with no type,
// which it removes.
func TestTypeGoneBackticks(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n, flip := typeRenameIssue(f)
	f.RenameOrgIssueType("Task", "Ta`sk")
	if _, err := typeRenameCheck(t, f, n, &github.IssueTypeNames{}); err != nil {
		t.Fatal(err)
	}
	if got, want := typeRenameComments(f, n), typeGoneComment("``Ta`sk``", flip); !slices.Equal(got, []string{want}) {
		t.Errorf("comments %q; want %q", got, want)
	}
	untyped := f.AddIssue("owner", "Untyped", "")
	removed := f.SetTypeBy(untyped, "mallory", "`Epic")
	if _, err := typeRenameCheck(t, f, untyped, &github.IssueTypeNames{}); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("I removed the issue type `` `Epic ``: only trusted users can change the issue type, and mallory isn't one.\n\n"+
		"<!-- issuedog:revert key=%d -->", removed)
	if got := typeRenameComments(f, untyped); !slices.Equal(got, []string{want}) {
		t.Errorf("the removal's comments %q; want %q", got, want)
	}
}
