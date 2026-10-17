package daemon_test

import (
	"context"
	"reflect"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// After the App's full rewrite the App is the body's author, so the issue
// author's own edit after it is sanctioned only if they are trusted (#87,
// Q4a). Before any rewrite it still is. An App edit whose text someone
// deleted may have been a rewrite, so it counts as one. Trust is asked
// about the author only where it decides, once.
func TestAuthorEditAfterRewrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		author string
		edits  [][2]string // editor, text
		del    int         // a history entry whose text someone deleted, or 0
		want   daemon.Dirt
		checks map[string]int
	}{
		{"an untrusted author's edit after the rewrite", "filer",
			[][2]string{{app, rewriteSpec}, {"filer", "evil"}}, 0, daemon.Dirt{state.BodyID: "filer"}, map[string]int{"filer": 1}},
		{"and edited back to the rewrite's text", "filer",
			[][2]string{{app, rewriteSpec}, {"filer", "evil"}, {"filer", rewriteSpec}}, 0, daemon.Dirt{state.BodyID: "filer"}, map[string]int{"filer": 1}},
		{"an untrusted author's edit with no rewrite", "filer",
			[][2]string{{"filer", "the idea, put better"}}, 0, daemon.Dirt{}, map[string]int{}},
		{"an untrusted author's edit before the rewrite", "filer",
			[][2]string{{"filer", "the idea, put better"}, {app, rewriteSpec}}, 0, daemon.Dirt{}, map[string]int{}},
		{"a trusted author's edit after the rewrite", "owner",
			[][2]string{{app, rewriteSpec}, {"owner", "spec v2"}}, 0, daemon.Dirt{}, map[string]int{"owner": 1}},
		{"an untrusted author's edit after a rewrite whose text was deleted", "filer",
			[][2]string{{app, rewriteSpec}, {"filer", "evil"}}, 1, daemon.Dirt{state.BodyID: "filer"}, map[string]int{"filer": 1}},
		{"their edit before a rewrite whose text was deleted is still theirs", "filer",
			[][2]string{{"filer", "the idea, put better"}, {app, rewriteSpec}, {"owner", "spec v2"}}, 2, daemon.Dirt{},
			map[string]int{"owner": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := githubtest.New(t, "o/r", slug)
			n := f.AddIssue(tc.author, "Title", "an idea")
			for _, e := range tc.edits {
				f.EditBody(n, e[0], e[1])
			}
			if tc.del > 0 {
				f.DeleteRevision(n, tc.del)
			}
			a, s, err := applier(t, f)
			if err != nil {
				t.Fatal(err)
			}
			trust := countTrust{trusted{"owner": true}, map[string]int{}}
			d, err := a.Dirty(context.Background(), s, trust)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(d, tc.want) {
				t.Errorf("dirt = %v, want %v", d, tc.want)
			}
			if !reflect.DeepEqual(trust.checks, tc.checks) {
				t.Errorf("trust checks = %v, want %v", trust.checks, tc.checks)
			}
		})
	}
}

// authorEditReview files issue 1, a story, by filer, whom nobody trusts,
// at spec-review: event 9001. If rewrite is set, the App first rewrites
// the body whole with the spec. Comment 1001 is revision 1 of the body,
// with the owner's 👍 (1002). Then filer edits the body and back, so its
// hash matches revision 1 again, and more runs.
func authorEditReview(rewrite bool, more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		f.AddIssue("filer", "Title", "body")
		f.SetType(1, "Story")
		body := "body"
		if rewrite {
			body = rewriteSpec
			f.EditBody(1, app, body)
		}
		labelEvent(t, f.AddLabel(1, app, "stage:spec-review"), 9001)
		post(t, f, 1001, app, revision(1, body))
		f.AddReaction(1001, "owner", "+1")
		f.EditBody(1, "filer", "evil")
		f.EditBody(1, "filer", body)
		more(t, f)
		return f
	}
}

// After a spec rewrite, the untrusted author's edit makes the body dirty:
// it gets a note, with the text below escaped, no revision is posted, and
// neither the 👍 nor an /approve approves it, though the body has
// revision 1's text again. Before any rewrite their edit is still
// sanctioned: the body stays clean and the 👍 approves it.
func TestAuthorEditApproval(t *testing.T) {
	t.Parallel()
	atReview := []string{"stage:spec-review"}
	byFiler := "the issue description has an edit by filer that wasn't approved, so I can't approve it"
	noted := "<!-- issuedog:dirty by=filer -->\n> ⚠️ issuedog ignores this issue description: " +
		"**filer** edited it without approval.\n\n" + github.Escape(rewriteSpec)
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
		body  string
	}{
		{"after a rewrite the 👍 doesn't approve",
			authorEditReview(true, func(testing.TB, *githubtest.Fake) {}),
			outcome{labels: atReview}, noted},
		{"after a rewrite an /approve is rejected",
			authorEditReview(true, func(t testing.TB, f *githubtest.Fake) { post(t, f, 1003, "owner", "/approve") }),
			outcome{comments: []string{rejected("/approve", byFiler, "1003")}, labels: atReview, downs: []string{"/approve"}}, noted},
		{"before any rewrite the 👍 approves",
			authorEditReview(false, func(testing.TB, *githubtest.Fake) {}),
			outcome{comments: []string{approvedRev(1, "owner's 👍", "r1002", "1001")}, labels: []string{"stage:planning"}}, "body"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			check(t, tt.setup, visitPass(t), tt.want)
			f := run(t, tt.setup, visitPass(t))
			if got := f.State().Issues[0].Body; got != tt.body {
				t.Errorf("body = %q, want %q", got, tt.body)
			}
		})
	}
}
