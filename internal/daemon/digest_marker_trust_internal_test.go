package daemon

import (
	"context"
	"fmt"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// An App issue with an App comment that has an origin marker, as a
// reposted one, is no digest, whatever its body: digestTake judges it with
// no edit history read, though the App's own original post has the
// identity marker. Without the comment, it's the digest.
func TestDigestMarkedReposted(t *testing.T) {
	t.Parallel()
	for _, reposted := range []bool{false, true} {
		t.Run(fmt.Sprintf("reposted %v", reposted), func(t *testing.T) {
			t.Parallel()
			f := githubtest.New(t, "o/r", "issuedog")
			f.AddIssue("issuedog[bot]", "A task", "Do it.\n\n<!-- issuedog:digest -->")
			if reposted {
				f.AddComment(1, "issuedog[bot]", "Reposted.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T1 -->")
			}
			d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
			if err := d.poll(context.Background()); err != nil {
				t.Fatal(err)
			}
			if d.digest.has(1) == reposted || d.stale[1] != reposted || len(d.digest.titled) != 0 {
				t.Errorf("digests %v, stale %v, held %v; want #1 the digest %t", d.digest.issues, d.stale, d.digest.titled, !reposted)
			}
			want := 1
			if reposted {
				want = 0
			}
			if got := f.HistoryQueries(); got != want {
				t.Errorf("history queries = %d; want %d", got, want)
			}
		})
	}
}

// digestMarkedHistory takes the identity marker only from a version of the
// body the App wrote (SPEC §5.1, #498): the body as listed with no edit,
// else the original post, whoever GitHub names, or an edit of the App's.
// Mallory's edit, a deleted original, and the App's dirty note, which
// escapes the marker, give none.
func TestDigestMarkedHistory(t *testing.T) {
	t.Parallel()
	const marked, plain = "Nothing is waiting on you.\n\n<!-- issuedog:digest -->", "A task."
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	mallory := github.Author{Login: "mallory", Type: "User"}
	ghost := github.Author{Login: "ghost", Type: "User"}
	for _, tt := range []struct {
		name string
		h    []github.Edit
		body string
		want bool
	}{
		{"never edited, marked", nil, marked, true},
		{"never edited, plain", nil, plain, false},
		{"the original post marked", []github.Edit{{By: ghost, Text: marked}, {By: mallory, Text: plain}}, plain, true},
		{"the App's edit marked", []github.Edit{{By: app, Text: plain}, {By: app, Text: marked}, {By: mallory, Text: "gone"}}, "gone", true},
		{"mallory's edit marked", []github.Edit{{By: app, Text: plain}, {By: mallory, Text: marked}}, marked, false},
		{"the original deleted", []github.Edit{{By: app, Text: ""}, {By: mallory, Text: marked}}, marked, false},
		{"the App's dirty note", []github.Edit{{By: app, Text: plain}, {By: mallory, Text: marked}, {By: app, Text: github.Escape(marked)}}, github.Escape(marked), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := digestMarkedHistory(tt.h, tt.body, "issuedog"); got != tt.want {
				t.Errorf("digestMarkedHistory = %t; want %t", got, tt.want)
			}
		})
	}
}
