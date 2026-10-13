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

// commandsPass runs step 1 on issue n of o/r with daemon.Commands, "owner"
// trusted.
func commandsPass(t testing.TB, n int) func(*githubtest.Fake) error { return passIn(t, "o", n) }

// passIn is commandsPass in the repo owner/r.
func passIn(t testing.TB, owner string, n int) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), owner, "r", n)
		if err != nil {
			return err
		}
		tr := trusted{"owner": true}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: owner, Repo: "r", AppSlug: slug}, Trust: tr, Commands: daemon.Commands(tr)}
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

// then runs more after setup.
func then(setup func(testing.TB) *githubtest.Fake, more func(f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := setup(t)
		more(f)
		return f
	}
}

// moved is the App's comment for a move by a command.
func moved(text, fields string) string {
	return text + "\n\n<!-- issuedog:transition " + fields + " -->"
}

const ready = "from=intake to=ready run=a"

// above is the marker fields of an owner hold at at that a /hold on an
// ancestor made: its cmd names comment 1001, on #1.
func above(at string) string {
	return "from=" + at + " to=held at=" + at + " reason=owner cmd=1001"
}

// madeOn is /resume's rejection when only holds made on these issues remain.
func madeOn(issues string) string {
	return "the holds below were made on " + issues + ", by `/hold` or by hand, so `/resume` there ends them"
}

// issueWant is an issue after the passes: its labels and its new comments.
type issueWant struct{ labels, comments []string }

func TestHoldResumePriority(t *testing.T) {
	held := func(at, reason string) string {
		return "from=" + at + " to=held at=" + at + " reason=" + reason + " run=h"
	}
	untyped := func(n int) func(f *githubtest.Fake) { return func(f *githubtest.Fake) { f.SetType(n, "") } }
	tests := []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		on     int // the issue the command is on, the owner's first comment there
		want   map[int]issueWant
		react  string // the App's reaction to the command
		posts  []string
		writes []string // all of them, if set
	}{
		{name: "/hold holds the issue last and its open descendants depth first, below a closed one too; one held already keeps its at",
			// 1 > 2 > 3, 2 > 4 (held), 1 > 5, 1 > 6 (closed) > 7
			setup: tree([]string{ready, "from=ready to=in-progress run=a", "from=in-progress to=verifying run=a",
				held("planning", "owner"), ready, ready, ready}, []int{0, 1, 2, 2, 1, 1, 6},
				1, 1008, "/hold", func(f *githubtest.Fake) { f.Close(6) }),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:held"}, []string{moved("On hold by `/hold`, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1008")}},
				2: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `in-progress`.", "from=in-progress to=held at=in-progress reason=owner cmd=1008")}},
				3: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `verifying`.", "from=verifying to=held at=verifying reason=owner cmd=1008")}},
				5: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1008")}},
				7: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1008")}},
			},
			react: "+1", posts: []string{"/issues/2/comments", "/issues/3/comments", "/issues/5/comments", "/issues/7/comments", "/issues/1/comments"}},
		{name: "/hold skips a sub-issue in another repo and all below it, comparing the repo in any case",
			// 1 > o/other#2, 1 > O/R#3, 1 > o/rx#4, o/r#2 > 4
			setup: tree([]string{ready, ready, ready, ready}, []int{0, 0, 0, 2}, 1, 1005, "/hold", func(f *githubtest.Fake) {
				f.LinkSubIssue(1, "o/other", 2)
				f.LinkSubIssue(1, "O/R", 3)
				f.LinkSubIssue(1, "o/rx", 4)
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:held"}, []string{moved("On hold by `/hold`, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1005")}},
				3: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1005")}},
			},
			react: "+1"},
		{name: "/hold reports a sub-issue of unknown type once, escaped, and holds the rest",
			setup: tree([]string{ready, ready, ready}, []int{0, 1, 1}, 1, 1004, "/hold", func(f *githubtest.Fake) { f.SetType(2, "Feature<!--x") }),
			on:    1,
			want: map[int]issueWant{
				1: {[]string{"stage:held"}, []string{moved("On hold by `/hold`, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1004")}},
				2: {[]string{"stage:ready"}, []string{"I can't move this issue from `ready` to `held` on hold: its type `feature&lt;!--x` isn't one the pipeline knows (SPEC §4). " +
					"It stays at `ready`.\n\n<!-- issuedog:error kind=transition key=1004 -->"}},
				3: {[]string{"stage:held"}, []string{moved("On hold by `/hold` on #1, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1004")}},
			},
			react: "+1"},
		{name: "/hold on an issue of unknown type is rejected",
			setup: tree([]string{ready, ready}, []int{0, 1}, 1, 1003, "/hold", func(f *githubtest.Fake) { f.SetType(1, "Feature") }),
			on:    1,
			want: map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{rejected("/hold", "this issue can't go on hold: its type `feature` isn't one the pipeline knows (SPEC §4)", "1003")}},
			},
			react: "-1"},
		{name: "/hold that began isn't rejected when its issue's type turns unknown",
			setup: then(tree([]string{ready, ready}, []int{0, 1}, 1, 1003, "/hold", nil), func(f *githubtest.Fake) {
				f.AddComment(2, app, moved("On hold by `/hold` on #1, from `ready`.", "from=ready to=held at=ready reason=owner cmd=1003"))
				f.SetType(1, "Feature")
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{"I can't move this issue from `ready` to `held` on hold: its type `feature` isn't one the pipeline knows (SPEC §4). " +
					"It stays at `ready`.\n\n<!-- issuedog:error kind=transition key=1003 -->"}},
				2: {[]string{"stage:held"}, nil},
			},
			react: "+1"},
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
			setup: tree([]string{held("ready", "owner"), held("in-progress", "owner"), above("verifying"), held("ready", "discussion")},
				[]int{0, 1, 2, 2}, 2, 1005, "/resume", nil),
			on: 2,
			want: map[int]issueWant{
				2: {[]string{"stage:in-progress"}, []string{moved("Resumed by `/resume`, back to `in-progress`.", "from=held to=in-progress cmd=1005")}},
				3: {[]string{"stage:verifying"}, []string{moved("Resumed by `/resume` on #2, back to `verifying`.", "from=held to=verifying cmd=1005")}},
			},
			react: "+1", posts: []string{"/issues/3/comments", "/issues/2/comments"}},
		{name: "/resume on an issue that isn't held resumes its descendants, three levels down",
			setup: tree([]string{"from=intake to=ready run=a", above("ready"), above("planning")}, []int{0, 1, 2}, 1, 1004, "/resume", nil),
			on:    1,
			want: map[int]issueWant{
				2: {[]string{"stage:ready"}, []string{moved("Resumed by `/resume` on #1, back to `ready`.", "from=held to=ready cmd=1004")}},
				3: {[]string{"stage:planning"}, []string{moved("Resumed by `/resume` on #1, back to `planning`.", "from=held to=planning cmd=1004")}},
			},
			react: "+1"},
		{name: "/resume with nothing open on an owner hold is rejected",
			setup: tree([]string{ready, held("ready", "discussion"), held("ready", "owner")}, []int{0, 1, 1}, 1, 1004, "/resume",
				func(f *githubtest.Fake) { f.Close(3) }),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{rejected("/resume", "nothing here or in its open sub-issues is on an owner hold", "1004")}},
			},
			react: "-1"},
		{name: "/resume doesn't read a dirty hold marker, and its stage:held label is the child's own",
			setup: tree([]string{ready, above("ready")}, []int{0, 1}, 1, 1003, "/resume", func(f *githubtest.Fake) {
				f.EditComment(1002, "mallory", "<!-- issuedog:transition "+above("ready")+" -->")
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{rejected("/resume", madeOn("#2"), "1003")}},
			},
			react: "-1"},
		{name: "/resume on a parent leaves a child labelled held by hand, in any case, with no marker yet",
			setup: tree([]string{ready, ready}, []int{0, 1}, 1, 1003, "/resume", func(f *githubtest.Fake) {
				f.RemoveLabel(2, "owner", "stage:ready")
				f.AddLabel(2, "owner", "Stage:Held")
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{rejected("/resume", madeOn("#2"), "1003")}},
			},
			react: "-1"},
		{name: "/resume isn't rejected once it resumed a child that has since closed",
			setup: then(tree([]string{ready, held("ready", "owner")}, []int{0, 1}, 1, 1003, "/resume", nil), func(f *githubtest.Fake) {
				f.AddComment(2, app, moved("Resumed by `/resume` on #1, back to `ready`.", "from=held to=ready cmd=1003"))
				f.Close(2)
			}),
			on: 1, react: "+1"},
		{name: "/resume doesn't count a hold whose at isn't a stage to go back to",
			setup: tree([]string{ready, "from=held to=held at=held reason=owner cmd=1001", "from=ready to=held at=closed reason=owner cmd=1001"},
				[]int{0, 1, 1}, 1, 1004, "/resume", nil),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:ready"}, []string{rejected("/resume", "nothing here or in its open sub-issues is on an owner hold", "1004")}},
			},
			react: "-1"},
		{name: "/resume goes back to the latest hold's at",
			setup: tree([]string{ready}, []int{0}, 1, 1006, "/resume", func(f *githubtest.Fake) {
				for _, m := range []string{held("ready", "owner"), "from=held to=ready run=r", "from=ready to=in-progress run=b", held("in-progress", "owner")} {
					f.AddComment(1, app, "<!-- issuedog:transition "+m+" -->")
				}
				f.RemoveLabel(1, app, "stage:ready")
				f.AddLabel(1, app, "stage:held")
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"stage:in-progress"}, []string{moved("Resumed by `/resume`, back to `in-progress`.", "from=held to=in-progress cmd=1006")}},
			},
			react: "+1"},
		{name: "/resume reports a child it can't move once, resumes the rest, and a later comment is still processed",
			setup: then(tree([]string{ready, above("ready"), above("ready")}, []int{0, 1, 1}, 1, 1004, "/resume", untyped(2)),
				func(f *githubtest.Fake) { f.AddComment(1, "owner", "/priority p1") }),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"priority:p1", "stage:ready"}, nil},
				2: {[]string{"stage:held"}, []string{"I can't move this issue from `held` to `ready` on resume: the transition table (SPEC §7.2) " +
					"has no such row for a untyped issue. It stays at `held`.\n\n<!-- issuedog:error kind=transition key=1004 -->"}},
				3: {[]string{"stage:ready"}, []string{moved("Resumed by `/resume` on #1, back to `ready`.", "from=held to=ready cmd=1004")}},
			},
			react: "+1", writes: []string{"POST /issues/2/comments", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:held",
				"POST /issues/comments/1004/reactions", "POST /issues/1/labels", "POST /issues/comments/1005/reactions"}},
		{name: "/resume is rejected if the issue's own hold has no row back",
			setup: tree([]string{held("ready", "owner"), held("ready", "owner")}, []int{0, 1}, 1, 1003, "/resume", untyped(1)),
			on:    1,
			want: map[int]issueWant{
				1: {[]string{"stage:held"}, []string{rejected("/resume", "this issue can't go back to `ready`: "+
					"the transition table (SPEC §7.2) has no such row for a untyped issue", "1003")}},
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
		{name: "/priority keeps its label in any case",
			setup: tree([]string{ready}, []int{0}, 1, 1002, "/priority p1", func(f *githubtest.Fake) {
				f.AddLabel(1, "owner", "Priority:P1")
				f.AddLabel(1, app, "priority:p3")
			}),
			on: 1,
			want: map[int]issueWant{
				1: {[]string{"Priority:P1", "stage:ready"}, nil},
			},
			react: "+1", writes: []string{"DELETE /issues/1/labels/priority:p3", "POST /issues/comments/1002/reactions"}},
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
			comments := got.Issues[tt.on-1].Comments
			cmd := comments[slices.IndexFunc(comments, func(c githubtest.CommentState) bool { return c.Author == "owner" })]
			if want := []string{app + " " + tt.react}; !reflect.DeepEqual(cmd.Reactions, want) {
				t.Errorf("the command's reactions = %q, want %q", cmd.Reactions, want)
			}
			var posts, all []string
			writes := f.Writes()
			for _, w := range writes {
				all = append(all, w.Method+" "+w.Path)
				if w.Method == "POST" && strings.HasSuffix(w.Path, "/comments") {
					posts = append(posts, w.Path)
				}
			}
			if tt.posts != nil && !reflect.DeepEqual(posts, tt.posts) {
				t.Errorf("comments posted on %q, want %q", posts, tt.posts)
			}
			if tt.writes != nil {
				if !reflect.DeepEqual(all, tt.writes) {
					t.Errorf("writes = %q, want %q", all, tt.writes)
				}
				return
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

// A /resume ends only the holds below that a /hold on its issue or above
// made: a child's own hold, by its /hold or by hand, and the holds its /hold
// made below it, stay until /resume on the child.
func TestOwnHolds(t *testing.T) {
	type step struct {
		on   int
		body string
	}
	// #1 at ready, its child #2 at in-progress, maybe held by the owner's
	// hand, and #2's child #3 at verifying, then each step but the last
	// applied.
	setup := func(hand bool, steps []step) func(testing.TB) *githubtest.Fake {
		return func(t testing.TB) *githubtest.Fake {
			f := githubtest.New(t, "o/r", slug)
			file(f, 0, ready)
			file(f, 1, "from=ready to=in-progress run=a")
			file(f, 2, "from=in-progress to=verifying run=a")
			if hand {
				f.RemoveLabel(2, "owner", "stage:in-progress")
				f.AddLabel(2, "owner", "stage:held")
			}
			for i, s := range steps {
				f.AddComment(s.on, "owner", s.body)
				if i < len(steps)-1 {
					if err := commandsPass(t, s.on)(f); err != nil {
						t.Fatal(err)
					}
				}
			}
			f.ForgetWrites()
			return f
		}
	}
	tests := []struct {
		name   string
		hand   bool
		steps  []step
		labels [3]string // #1's, #2's and #3's stage
		react  string    // the App's reaction to the last command
		reply  string    // the reason of its rejection, if any
	}{
		{name: "a child's own /hold, and the hold it made below, stay when its parent resumes",
			steps:  []step{{2, "/hold"}, {1, "/hold"}, {1, "/resume"}},
			labels: [3]string{"stage:ready", "stage:held", "stage:held"}, react: "+1"},
		{name: "a child held by hand stays when its parent resumes",
			hand:   true,
			steps:  []step{{1, "/hold"}, {1, "/resume"}},
			labels: [3]string{"stage:ready", "stage:held", "stage:verifying"}, react: "+1"},
		{name: "a parent's /resume ends the holds its /hold made",
			steps:  []step{{1, "/hold"}, {1, "/resume"}},
			labels: [3]string{"stage:ready", "stage:in-progress", "stage:verifying"}, react: "+1"},
		{name: "a child's /resume ends its own hold and the one it made below",
			steps:  []step{{2, "/hold"}, {1, "/hold"}, {2, "/resume"}},
			labels: [3]string{"stage:held", "stage:in-progress", "stage:verifying"}, react: "+1"},
		{name: "a grandparent's /resume with only holds made below it is rejected",
			steps:  []step{{2, "/hold"}, {1, "/resume"}},
			labels: [3]string{"stage:ready", "stage:held", "stage:held"}, react: "-1", reply: madeOn("#2")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			last := tt.steps[len(tt.steps)-1]
			githubtest.Replay(t, setup(tt.hand, tt.steps), commandsPass(t, last.on))

			got := run(t, setup(tt.hand, tt.steps), commandsPass(t, last.on)).State()
			for i, want := range tt.labels {
				if l := got.Issues[i].Labels; !reflect.DeepEqual(l, []string{want}) {
					t.Errorf("#%d: labels %q, want %q", i+1, l, want)
				}
			}
			// The last command is the owner's last comment on its issue.
			comments := got.Issues[last.on-1].Comments
			i := len(comments) - 1
			for comments[i].Author != "owner" {
				i--
			}
			if want := []string{app + " " + tt.react}; !reflect.DeepEqual(comments[i].Reactions, want) {
				t.Errorf("the last command's reactions = %q, want %q", comments[i].Reactions, want)
			}
			var replies, want []string
			for _, c := range comments[i+1:] {
				if strings.Contains(c.Body, "issuedog:reply") {
					replies = append(replies, c.Body)
				}
			}
			if tt.reply != "" {
				// Up to its cmd's value, the command's ID.
				want = []string{strings.TrimSuffix(rejected(last.body, tt.reply, ""), " -->")}
			}
			if len(replies) != len(want) || (len(want) > 0 && !strings.HasPrefix(replies[0], want[0])) {
				t.Errorf("replies %q, want %q", replies, want)
			}
		})
	}
}

// The Applier's repo, in any case, matches its sub-issues' repository_url.
func TestHoldMixedCaseRepo(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := githubtest.New(t, "O/r", slug)
		file(f, 0, ready)
		file(f, 1, ready)
		f.AddComment(1, "owner", "/hold")
		return f
	}
	githubtest.Replay(t, setup, passIn(t, "O", 1))
	if l := run(t, setup, passIn(t, "O", 1)).State().Issues[1].Labels; !reflect.DeepEqual(l, []string{"stage:held"}) {
		t.Errorf("#2: labels %q, want it held", l)
	}
}
