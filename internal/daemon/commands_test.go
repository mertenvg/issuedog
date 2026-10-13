package daemon_test

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// commandsPass runs step 1 on issue n with daemon.Commands, "owner" trusted.
func commandsPass(t *testing.T, n int) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", n)
		if err != nil {
			return err
		}
		tr := trusted{"owner": true}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: tr, Commands: daemon.Commands(tr)}
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// file files a task by the owner under parent (none if 0), with the App's
// stage label and a transition marker with these fields, and returns its
// number. The stage is the fields' to.
func file(f *githubtest.Fake, parent int, fields string) int {
	n := f.AddIssue("owner", "Issue", "body")
	f.SetType(n, "task")
	if parent != 0 {
		f.SetParent(n, parent)
	}
	_, to, _ := strings.Cut(fields, "to=")
	to, _, _ = strings.Cut(to, " ")
	f.AddLabel(n, app, "stage:"+to)
	f.AddComment(n, app, "<!-- issuedog:transition "+fields+" -->")
	return n
}

// tree files issues by fields, each under the issue parents gives, then
// posts body as the owner on issue on, failing t unless its ID is cmd.
func tree(fields []string, parents []int, on int, cmd int64, body string, more func(f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "o/r", slug)
		for i, fl := range fields {
			file(f, parents[i], fl)
		}
		if more != nil {
			more(f)
		}
		if id := f.AddComment(on, "owner", body); id != cmd {
			t.Fatalf("command ID = %d, want %d", id, cmd)
		}
		return f
	}
}

// moved is the App's comment for a move by a command.
func moved(text, fields string) string {
	return text + "\n\n<!-- issuedog:transition " + fields + " -->"
}

// issueWant is an issue after the passes: its labels and its new comments.
type issueWant struct{ labels, comments []string }

func TestHoldResumePriority(t *testing.T) {
	held := func(at, reason string) string {
		return "from=" + at + " to=held at=" + at + " reason=" + reason + " run=h"
	}
	tests := []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		on    int // the issue the command is on
		want  map[int]issueWant
		react string // the App's reaction to the command
		posts []string
	}{
		{name: "/hold holds the issue last and its open descendants depth first; one held already keeps its at",
			// 1 > 2 > 3, 2 > 4 (held), 1 > 5, 1 > 6 (closed)
			setup: tree([]string{"from=intake to=ready run=a", "from=ready to=in-progress run=a", "from=in-progress to=verifying run=a",
				held("planning", "owner"), "from=intake to=ready run=a", "from=intake to=ready run=a"}, []int{0, 1, 2, 2, 1, 1},
				1, 1007, "/hold", func(f *githubtest.Fake) { f.Close(6) }),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:held"}, []string{moved("On hold by `/hold`, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1007")}},
				2: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `in-progress`.", "from=in-progress to=held at=in-progress reason=owner cmd=1007")}},
				3: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `verifying`.", "from=verifying to=held at=verifying reason=owner cmd=1007")}},
				5: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1007")}},
			},
			react: "+1", posts: []string{"/issues/2/comments", "/issues/3/comments", "/issues/5/comments", "/issues/1/comments"}},
		{name: "/hold on a held issue writes it no marker, but holds its descendants",
			setup: tree([]string{held("planning", "discussion"), "from=intake to=ready run=a"}, []int{0, 1}, 1, 1003, "/hold", nil),
			on:    1,
			want: map[int]issueWant{
				2: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1003")}},
			},
			react: "+1"},
		{name: "/hold records a descendant's hand-set stage first",
			setup: tree([]string{"from=intake to=ready run=a", "from=intake to=ready run=a"}, []int{0, 1}, 1, 1003, "/hold", func(f *githubtest.Fake) {
				f.RemoveLabel(2, "owner", "stage:ready")
				f.AddLabel(2, "owner", "stage:planned")
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:held"}, []string{moved("On hold by `/hold`, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1003")}},
				2: {[]string{"stage:held"}, []string{
					moved("owner set `stage:planned` by hand, so I've recorded it as an owner override.", "from=ready to=planned by=owner event=9004"),
					moved("On hold by `/hold` on #1, from `planned`.", "from=planned to=held at=planned reason=owner cmd=1003")}},
			},
			react: "+1"},
		{name: "/resume ends owner holds on the issue and below, but not above, nor a discussion hold",
			// 1 > 2 > 3, 2 > 4 (discussion)
			setup: tree([]string{held("ready", "owner"), held("in-progress", "owner"), held("verifying", "owner"), held("ready", "discussion")},
				[]int{0, 1, 2, 2}, 2, 1005, "/resume", nil),
			on: 2,
			want: map[int]issueWant{
				2: {[]string{"stage:in-progress"}, []string{moved("Resumed by `/resume`, back to `in-progress`.", "from=held to=in-progress cmd=1005")}},
				3: {[]string{"stage:verifying"}, []string{moved("Resumed by `/resume` on #2, back to `verifying`.", "from=held to=verifying cmd=1005")}},
			},
			react: "+1", posts: []string{"/issues/3/comments", "/issues/2/comments"}},
		{name: "/resume on an issue that isn't held resumes its descendants, three levels down",
			setup: tree([]string{"from=intake to=ready run=a", held("ready", "owner"), held("planning", "owner")}, []int{0, 1, 2}, 1, 1004, "/resume", nil),
			on:    1,
			want: map[int]issueWant{
				2: {[]string{"stage:ready"}, []string{moved("Resumed by `/resume` on #1, back to `ready`.", "from=held to=ready cmd=1004")}},
				3: {[]string{"stage:planning"}, []string{moved("Resumed by `/resume` on #1, back to `planning`.", "from=held to=planning cmd=1004")}},
			},
			react: "+1"},
		{name: "/resume with nothing open on an owner hold is rejected; a dirty hold marker doesn't count",
			setup: tree([]string{"from=intake to=ready run=a", held("ready", "discussion"), held("ready", "owner"), held("ready", "owner")},
				[]int{0, 1, 1, 1}, 1, 1005, "/resume", func(f *githubtest.Fake) {
					f.Close(3)
					f.EditComment(1004, "mallory", "<!-- issuedog:transition "+held("ready", "owner")+" -->")
				}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{rejected("/resume", "nothing here or in its open sub-issues is on an owner hold", "1005")}},
			},
			react: "-1"},
		{name: "/priority p1 replaces the other priorities",
			setup: tree([]string{"from=intake to=ready run=a"}, []int{0}, 1, 1002, "/priority p1", func(f *githubtest.Fake) {
				f.AddLabel(1, app, "priority:p2")
				f.AddLabel(1, "owner", "Priority:P3")
				f.AddLabel(1, "owner", "scope:daemon")
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"priority:p1", "scope:daemon", "stage:ready"}, nil},
			},
			react: "+1"},
		{name: "/priority p9 is rejected",
			setup: tree([]string{"from=intake to=ready run=a"}, []int{0}, 1, 1002, "/priority p9", func(f *githubtest.Fake) { f.AddLabel(1, app, "priority:p2") }),
			on:    1,
			want: map[int]issueWant{
				1: {[]string{"priority:p2", "stage:ready"}, []string{rejected("/priority p9", "bad argument: `/priority` takes p0, p1, p2 or p3", "1002")}},
			},
			react: "-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			githubtest.Replay(t, tt.setup, commandsPass(t, tt.on))

			before := tt.setup(t).State()
			f := run(t, tt.setup, commandsPass(t, tt.on))
			got := f.State()
			if len(got.Issues) != len(before.Issues) {
				t.Fatalf("%d issues, want %d", len(got.Issues), len(before.Issues))
			}
			for i, is := range got.Issues {
				was := before.Issues[i]
				w, ok := tt.want[is.Number]
				if !ok {
					w.labels = was.Labels
				}
				var added []string
				for _, c := range is.Comments[len(was.Comments):] {
					added = append(added, c.Body)
				}
				if !reflect.DeepEqual(is.Labels, w.labels) || !reflect.DeepEqual(added, w.comments) {
					t.Errorf("#%d: labels %q and new comments %q, want %q and %q", is.Number, is.Labels, added, w.labels, w.comments)
				}
			}
			cmd := got.Issues[tt.on-1].Comments[len(before.Issues[tt.on-1].Comments)-1]
			if want := []string{app + " " + tt.react}; !reflect.DeepEqual(cmd.Reactions, want) {
				t.Errorf("the command's reactions = %q, want %q", cmd.Reactions, want)
			}
			var posts []string
			writes := f.Writes()
			for _, w := range writes {
				if w.Method == "POST" && strings.HasSuffix(w.Path, "/comments") {
					posts = append(posts, w.Path)
				}
			}
			if tt.posts != nil && !reflect.DeepEqual(posts, tt.posts) {
				t.Errorf("comments posted on %q, want %q", posts, tt.posts)
			}
			// The only reaction written is the 👍, last, or the 👎, first.
			for i, w := range writes {
				want := (tt.react == "+1" && i == len(writes)-1) || (tt.react == "-1" && i == 0)
				if strings.HasSuffix(w.Path, "/reactions") != want {
					t.Errorf("write %d is %s %s", i+1, w.Method, w.Path)
				}
			}
		})
	}
}

// A crash after a descendant's marker doesn't leave /hold 👍'd with the
// issue not held: at every crash point, a 👍'd /hold has its marker on #1.
func TestHoldAcksLast(t *testing.T) {
	setup := tree([]string{"from=intake to=ready run=a", "from=intake to=ready run=a", "from=intake to=ready run=a"}, []int{0, 1, 2}, 1, 1004, "/hold", nil)
	w := len(run(t, setup, commandsPass(t, 1)).Writes())
	for n := 1; n <= w; n++ {
		f := setup(t)
		f.FailWrite(n)
		if err := commandsPass(t, 1)(f); err == nil {
			t.Fatalf("crash at write %d: the pass didn't fail", n)
		}
		is := f.State().Issues[0]
		acked := slices.Contains(is.Comments[1].Reactions, app+" +1")
		if marked := slices.ContainsFunc(is.Comments, func(c githubtest.CommentState) bool { return strings.Contains(c.Body, "to=held") }); acked && !marked {
			t.Errorf("crash at write %d: /hold is 👍'd, but #1 has no hold marker", n)
		}
	}
}
