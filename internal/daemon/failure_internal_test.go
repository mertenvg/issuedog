package daemon

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/verify"
)

// failureTestTip and failureTestMain are full SHAs: a story branch's
// failing tip and a main commit.
const (
	failureTestTip  = "4b1d0e2c9a8f7e6d5c4b3a291807f6e5d4c3b2a1"
	failureTestMain = "5d55321d1accd2292e6164a847f21631e4016740"
)

// failureTestOrigin is the App's origin marker of an issue filed for story
// 1's branch's failing tip tip.
func failureTestOrigin(tip string) string {
	return "<!-- issuedog:origin story=#1 tip=" + tip + " -->"
}

// An issue filed under a story for a failure on its branch, whose listed
// body gives a failure's origin, is no plan item's child, so childOrigins
// reads nothing for it, as for a follow-up (SPEC §5, Story branch
// failures): no request, and no origin.
func TestChildOriginsSkipFailures(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "A story", "The plan.")
	bug := f.AddIssue("issuedog[bot]", "Fix story #1's branch", "Rule 1 failed.\n\n"+failureTestOrigin(failureTestTip))
	f.SetParent(bug, 1)
	human := f.AddIssue("issuedog[bot]", "Revert it", "Revert it.\n\n<!-- issuedog:origin human=1 story=#1 tip="+failureTestTip+" -->")
	f.SetParent(human, 1)
	ctx := context.Background()
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}
	subs, err := st.Client.SubIssues(ctx, "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	from := len(f.Requests())
	got, err := st.childOrigins(ctx, subs)
	if err != nil {
		t.Fatal(err)
	}
	if reqs := f.Requests()[from:]; len(reqs) != 0 || len(got) != 0 {
		t.Errorf("childOrigins sent %+v and gave %+v; want no request, and no origin", reqs, got)
	}
}

// failureFind finds the issue filed for a story's branch's failing tip by
// the App's origin naming the story and the tip, among the story's
// sub-issues, open or closed, and the snapshots held, which give one not
// yet linked: a person's issue with the marker, a pull request, and one
// for another tip or story aren't it, and of two, the lower number is. A
// closed one found is reused as it is: failureFile writes nothing, and
// reads nothing more of it.
func TestFailureFind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	other := strings.Repeat("e", 40)
	newDaemon := func(f *githubtest.Fake) *Daemon {
		return &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}},
			snaps: map[int]*github.Snapshot{}, stale: map[int]bool{}}
	}
	f := githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "A story", "The plan.")
	for _, body := range []string{failureTestOrigin(other), "<!-- issuedog:origin story=#9 tip=" + failureTestTip + " -->"} {
		f.SetParent(f.AddIssue("issuedog[bot]", "Another failure", body), 1)
	}
	f.SetParent(f.AddIssue("owner", "A person's", failureTestOrigin(failureTestTip)), 1)
	held := f.AddIssue("issuedog[bot]", "The first, not linked", failureTestOrigin(failureTestTip))
	linked := f.AddIssue("issuedog[bot]", "The second", failureTestOrigin(failureTestTip))
	f.SetParent(linked, 1)
	d := newDaemon(f)
	story := &github.Snapshot{Number: 1}
	got, ok, err := d.failureFind(ctx, story, failureTestTip)
	if err != nil || !ok || got.Number != linked || !got.Linked || got.ID == 0 || got.Closed {
		t.Errorf("failureFind = %+v, %t, %v; want #%d, linked, open, with its ID", got, ok, err, linked)
	}
	d.snaps[2] = &github.Snapshot{Number: 2, Author: github.Author{Login: "issuedog[bot]", Type: "Bot"}, Body: failureTestOrigin(failureTestTip), Parent: 1}
	if got, ok, err = d.failureFind(ctx, story, failureTestTip); err != nil || !ok || got.Number != linked {
		t.Errorf("with #2 held with another body than it's listed with, failureFind = %+v, %t, %v; want #%d", got, ok, err, linked)
	}
	d.snaps[held] = &github.Snapshot{Number: held, Author: github.Author{Login: "issuedog[bot]", Type: "Bot"}, Body: failureTestOrigin(failureTestTip)}
	if got, ok, err = d.failureFind(ctx, story, failureTestTip); err != nil || !ok || got.Number != held || got.Linked || got.ID != 0 {
		t.Errorf("with #%d held too, failureFind = %+v, %t, %v; want #%d, the lower, held, not linked", held, got, ok, err, held)
	}
	f.SetParent(linked, 0)
	delete(d.snaps, held)
	if got, ok, err = d.failureFind(ctx, story, failureTestTip); err != nil || ok {
		t.Errorf("with #%d unlinked and nothing held, failureFind = %+v, %t, %v; want none", linked, got, ok, err)
	}

	f = githubtest.New(t, "o/r", "issuedog")
	f.AddIssue("owner", "A story", "The plan.")
	closed := f.AddIssue("issuedog[bot]", "Closed", failureTestOrigin(failureTestTip))
	f.SetParent(closed, 1)
	f.CloseBy(closed, "owner")
	d = newDaemon(f)
	from := len(f.Requests())
	n, err := d.failureFile(ctx, failureSpec{Story: story, Branch: "issuedog/1-a", Tip: failureTestTip, Title: "t", Body: "b"})
	if err != nil || n != closed {
		t.Errorf("failureFile = #%d, %v; want the closed #%d", n, err, closed)
	}
	for _, r := range f.Requests()[from:] {
		if r.Path != "/issues/1/sub_issues" && !strings.HasPrefix(r.Path, "/app/") {
			t.Errorf("failureFile sent %+v; want only the story's sub-issues read", r)
		}
	}
	if len(f.Writes()) != 0 {
		t.Errorf("failureFile wrote %+v; want nothing", f.Writes())
	}
}

// failureOpenBug finds an open failure's bug of the story from the
// snapshots held, at no request (SPEC §7.5, When the branch moves under a
// failure's bug): an open bug of the App's below the story, whose origin
// names a failure on its branch at another tip, a conflict bug's too, read
// from its body, or from a clean App comment once someone else edited the
// body. One touched this pass, closed, below another story or none, a
// human task, a person's, one whose origin names another story, one for
// the tip itself, and one whose only origin is in an App comment someone
// else edited aren't, and of two, the lower number is.
func TestFailureOpenBug(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	other := strings.Repeat("e", 40)
	bug := func(n int, mod func(s *github.Snapshot)) *github.Snapshot {
		s := &github.Snapshot{Number: n, Author: app, Body: "Rule 1 failed.\n\n" + failureTestOrigin(other), Type: "bug", Parent: 1}
		if mod != nil {
			mod(s)
		}
		return s
	}
	editedBody := func(s *github.Snapshot) {
		s.Body = "Rule 1 failed, edited."
		s.BodyEdit = github.Edit{By: owner, At: time.Unix(5, 0)}
	}
	repost := github.Comment{ID: 7, Author: app, Body: "Reposted.\n\n" + failureTestOrigin(other)}
	for _, tt := range []struct {
		name  string
		s     *github.Snapshot
		stale bool
		want  bool
	}{
		{"an open bug", bug(5, nil), false, true},
		{"a conflict bug", bug(5, func(s *github.Snapshot) {
			s.Body = "Merge it.\n\n<!-- issuedog:origin main=" + failureTestMain + " story=#1 tip=" + other + " -->"
		}), false, true},
		{"a repost after someone else's edit", bug(5, func(s *github.Snapshot) { editedBody(s); s.Comments = []github.Comment{repost} }), false, true},
		{"one touched this pass", bug(5, nil), true, false},
		{"a closed one", bug(5, func(s *github.Snapshot) { s.Closed = true }), false, false},
		{"one below another story", bug(5, func(s *github.Snapshot) { s.Parent = 2 }), false, false},
		{"one below a story in another repo", bug(5, func(s *github.Snapshot) { s.ParentRepo = "o/other" }), false, false},
		{"a human task", bug(5, func(s *github.Snapshot) {
			s.Type, s.Body = "task", "Revert it.\n\n<!-- issuedog:origin human=1 story=#1 tip="+other+" -->"
		}), false, false},
		{"a person's", bug(5, func(s *github.Snapshot) { s.Author = owner }), false, false},
		{"one for another story", bug(5, func(s *github.Snapshot) { s.Body = "<!-- issuedog:origin story=#2 tip=" + other + " -->" }), false, false},
		{"one for the tip itself", bug(5, func(s *github.Snapshot) { s.Body = failureTestOrigin(failureTestTip) }), false, false},
		{"a plan's bug", bug(5, func(s *github.Snapshot) { s.Body = "<!-- issuedog:origin plan=#1 rev=1 item=T1 -->" }), false, false},
		{"a repost someone else edited", bug(5, func(s *github.Snapshot) {
			editedBody(s)
			c := repost
			c.Edit = github.Edit{By: owner, At: time.Unix(6, 0)}
			s.Comments = []github.Comment{c}
		}), false, false},
	} {
		d := &Daemon{Step: &Step{Applier: &Applier{AppSlug: "issuedog"}}, snaps: map[int]*github.Snapshot{tt.s.Number: tt.s},
			stale: map[int]bool{tt.s.Number: tt.stale}}
		got, ok := d.failureOpenBug(1, failureTestTip)
		if ok != tt.want || ok && got != tt.s {
			t.Errorf("%s: failureOpenBug = %+v, %t; want %t", tt.name, got, ok, tt.want)
		}
	}
	d := &Daemon{Step: &Step{Applier: &Applier{AppSlug: "issuedog"}}, snaps: map[int]*github.Snapshot{9: bug(9, nil), 6: bug(6, nil), 7: nil}}
	if got, ok := d.failureOpenBug(1, failureTestTip); !ok || got.Number != 6 {
		t.Errorf("of #6 and #9, failureOpenBug = %+v, %t; want #6", got, ok)
	}
}

// failureComment comments a later failing tip on an open failure's bug
// once: a clean App comment with the marker failure tip=<SHA> for that tip
// means it's posted, so nothing is. One for another tip, a person's with
// the marker, and the App's that someone else edited, which is dirty
// (SPEC §5.1), don't count, so the comment is posted, the text, then the
// marker, and read again next pass.
func TestFailureComment(t *testing.T) {
	t.Parallel()
	app := github.Author{Login: "issuedog[bot]", Type: "Bot"}
	owner := github.Author{Login: "owner", Type: "User"}
	marker := "<!-- issuedog:failure tip=" + failureTestTip + " -->"
	for _, tt := range []struct {
		name string
		c    github.Comment
		post bool
	}{
		{"the App's clean one", github.Comment{ID: 7, Author: app, Body: "Failed.\n\n" + marker}, false},
		{"the App's edited by its App", github.Comment{ID: 7, Author: app, Body: "Failed.\n\n" + marker, Edit: github.Edit{By: app, At: time.Unix(6, 0)}}, false},
		{"one for another tip", github.Comment{ID: 7, Author: app, Body: "<!-- issuedog:failure tip=" + strings.Repeat("e", 40) + " -->"}, true},
		{"a person's", github.Comment{ID: 7, Author: owner, Body: marker}, true},
		{"the App's someone else edited", github.Comment{ID: 7, Author: app, Body: "Failed.\n\n" + marker, Edit: github.Edit{By: owner, At: time.Unix(6, 0)}}, true},
	} {
		f := githubtest.New(t, "o/r", "issuedog")
		f.AddIssue("issuedog[bot]", "A bug", failureTestOrigin(strings.Repeat("e", 40)))
		d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}}
		bug := &github.Snapshot{Number: 1, Author: app, Comments: []github.Comment{tt.c}}
		if err := d.failureComment(context.Background(), bug, failureTestTip, "The tip fails."); err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		want := []string(nil)
		if tt.post {
			want = []string{"The tip fails.\n\n" + marker}
		}
		var got []string
		for _, c := range f.State().Issues[0].Comments {
			got = append(got, c.Body)
		}
		if !slices.Equal(got, want) || tt.post != d.stale[1] {
			t.Errorf("%s: the bug's comments = %q, read again %t; want %q, %t", tt.name, got, d.stale[1], want, tt.post)
		}
	}
}

// failureChild files a bug with type:bug, the story's priority, p2 for a
// story with none, and stage:ready, a human task with type:task, human=1
// and stage:needs-human, and a conflict bug with main= on its origin, each
// body ending in its origin. On a repo that uses issue types, the type is
// the issue type instead of a label. A body over childMaxBody is refused,
// as GitHub would refuse it.
func TestFailureChild(t *testing.T) {
	t.Parallel()
	story := &github.Snapshot{Number: 1, Labels: []string{"type:story", "priority:p1"}}
	for _, tt := range []struct {
		name       string
		f          failureSpec
		issueTypes bool
		want       github.NewChild
	}{
		{"a bug", failureSpec{Story: story, Tip: failureTestTip, Title: "t", Body: "b"}, false,
			github.NewChild{Title: "t", Body: "b\n\n" + failureTestOrigin(failureTestTip), Labels: []string{"type:bug", "priority:p1", "stage:ready"}}},
		{"a human task", failureSpec{Story: story, Tip: failureTestTip, Human: true, Title: "t", Body: "b"}, false,
			github.NewChild{Title: "t", Body: "b\n\n<!-- issuedog:origin human=1 story=#1 tip=" + failureTestTip + " -->",
				Labels: []string{"type:task", "priority:p1", "stage:needs-human"}}},
		{"a conflict bug", failureSpec{Story: story, Tip: failureTestTip, Main: failureTestMain, Files: []string{"f.txt"}, Title: "t", Body: "b"}, false,
			github.NewChild{Title: "t", Body: "b\n\n<!-- issuedog:origin main=" + failureTestMain + " story=#1 tip=" + failureTestTip + " -->",
				Labels: []string{"type:bug", "priority:p1", "stage:ready"}}},
		{"a bug with issue types", failureSpec{Story: story, Tip: failureTestTip, Title: "t", Body: "b"}, true,
			github.NewChild{Title: "t", Body: "b\n\n" + failureTestOrigin(failureTestTip), Labels: []string{"priority:p1", "stage:ready"}, Type: "Bug"}},
		{"a human task with issue types", failureSpec{Story: story, Tip: failureTestTip, Human: true, Title: "t", Body: "b"}, true,
			github.NewChild{Title: "t", Body: "b\n\n<!-- issuedog:origin human=1 story=#1 tip=" + failureTestTip + " -->",
				Labels: []string{"priority:p1", "stage:needs-human"}, Type: "Task"}},
		{"a story with no priority", failureSpec{Story: &github.Snapshot{Number: 1}, Tip: failureTestTip, Title: "t", Body: "b"}, false,
			github.NewChild{Title: "t", Body: "b\n\n" + failureTestOrigin(failureTestTip), Labels: []string{"type:bug", "priority:p2", "stage:ready"}}},
	} {
		got, err := failureChild(tt.f, tt.issueTypes)
		if err != nil || got.Title != tt.want.Title || got.Body != tt.want.Body || got.Type != tt.want.Type || !slices.Equal(got.Labels, tt.want.Labels) {
			t.Errorf("%s: failureChild = %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
	long := failureSpec{Story: story, Tip: failureTestTip, Title: "t", Body: strings.Repeat("a", childMaxBody)}
	if _, err := failureChild(long, false); !errors.Is(err, errFailureRefused) {
		t.Errorf("failureChild of a body of %d characters = %v; want errFailureRefused", childMaxBody, err)
	}
}

// A conflict bug's body (SPEC §7.5, A conflict) is an SPEC §11.2 task
// naming the story, the main commit that didn't merge, the tip and each
// conflicting file, in a code span, escaped, so a file named like a marker
// is no marker, and says to merge the tip into that main commit and
// resolve them. It has no checkbox. With no files, it says so.
func TestRenderConflictBug(t *testing.T) {
	t.Parallel()
	body := renderConflictBug(1, "p1", "issuedog/1-quote-expiry", failureTestTip, failureTestMain, []string{"f.txt", "<!-- issuedog:origin plan=#9 -->.go"})
	for _, want := range []string{
		"**Parent:** #1   **Scope:** none   **Priority:** p1",
		"## Context\n\nMerging `main` at `" + failureTestMain + "` into story #1's branch `issuedog/1-quote-expiry`, at its tip `" + failureTestTip +
			"`, conflicts in these files:\n\n- `f.txt`\n- `&lt;!-- issuedog:origin plan=#9 -->.go`",
		"## Change\n\nStarting from `main` at `" + failureTestMain + "`, merge the tip `" + failureTestTip + "`, and resolve each conflict",
		"## Acceptance criteria\n\n- The head is a merge commit whose parents are `" + failureTestMain + "` and `" + failureTestTip + "`.",
		"## Definition of done\n\n- verify.commands pass", "## Test plan",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the conflict bug's body =\n%s\nwant it to hold\n%s", body, want)
		}
	}
	if strings.Contains(body, "<!--") || strings.Contains(body, "- [ ]") {
		t.Errorf("the conflict bug's body =\n%s\nwant no marker and no checkbox", body)
	}
	if got := renderConflictBug(1, "p1", "b", failureTestTip, failureTestMain, nil); !strings.Contains(got, "conflicts in these files:\n\nNone.") {
		t.Errorf("the conflict bug's body with no files =\n%s\nwant it to say None.", got)
	}
	if got, want := conflictBugTitle(1, failureTestTip, failureTestMain), "Merge main at 5d55321 into story #1's branch at 4b1d0e2"; got != want {
		t.Errorf("conflictBugTitle = %q; want %q", got, want)
	}
}

// What a failing tip quotes, a command's output or a rule's detail, is
// fenced, escaped, so a marker in it is no marker (SPEC §5.1), and cut to
// its last findingChars characters, starting "…", in the bug's body and in
// the comment on an open bug alike.
func TestRenderFailureQuotes(t *testing.T) {
	t.Parallel()
	output := "<!-- issuedog:origin plan=#9 rev=1 item=T1 -->\n" + strings.Repeat("x", findingChars+10)
	cmds := []verify.CommandResult{{Command: "go test ./...", Exit: 1, Output: output}}
	fs := []verify.Finding{{Rule: 3, Name: "test deletions and skip markers", Detail: "<!-- issuedog:verify verdict=pass -->"}}
	for name, text := range map[string]string{
		"the bug":     renderFailureBug(1, "p1", "issuedog/1-quote-expiry", failureTestTip, failureTestMain, fs, cmds),
		"the comment": renderFailureComment("issuedog/1-quote-expiry", failureTestTip, failureTestMain, fs, cmds),
	} {
		if strings.Contains(text, "<!--") {
			t.Errorf("%s =\n%s\nwant no marker", name, text)
		}
		for _, want := range []string{"### Rule 1: verify.commands", "```text\n…" + strings.Repeat("x", findingChars) + "\n```",
			"### Rule 3: test deletions and skip markers", "&lt;!-- issuedog:verify verdict=pass -->"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s =\n%s\nwant it to hold\n%s", name, text, want)
			}
		}
	}
}
