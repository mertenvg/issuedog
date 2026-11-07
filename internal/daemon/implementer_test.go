package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// implementerOutput is internal/agent's golden implementer output name,
// decoded, for the fake claude to print.
func implementerOutput(t testing.TB, name string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "agent", "testdata", "outputs", "implementer", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// implementerTask files a task at in-progress, as the App moved it there,
// with priority:p1 and scope:booking, and returns its number.
func implementerTask(f *githubtest.Fake) int {
	n := fileAt(f, "in-progress", "Refuse an expired quote.", "priority:p1", "scope:booking")
	f.SetType(n, "Task")
	return n
}

// newImplementerRig is newRig with the implementer at in-progress, as
// daemon.RegisterStages registers it, in place of the fakeStage, and the
// fake claude committing in its clone and printing out, a successful
// result. With clone set, the daemon's clone, and so the workspace's bare
// repo, is clone, as across a replay's restarts.
func newImplementerRig(t *testing.T, f *githubtest.Fake, o *origin, root string, clone *repo.Repo, out map[string]any) *rig {
	t.Helper()
	g := newRig(t, f, o, root)
	if clone != nil {
		g.d.Clone, g.r.Workspace.Bare = clone, clone.Dir()
	}
	g.r.Stages = map[state.Stage]daemon.Stage{state.InProgress: &daemon.Implementer{Daemon: g.d}}
	g.claude(ivResult(t, out), 0)
	g.commits(true)
	return g
}

// implementerCommitted is the App's comment on the move to verifying of
// run, which committed head as attempt attempt of its round, with seen,
// for the golden commits output.
func implementerCommitted(run, head string, attempt int, seen int64) string {
	return "The implementer committed `" + head + "`.\n\n" +
		"**PR title:** feat: refuse booking an expired quote, with a link to a new one\n\n" +
		"## What changed\n\n" +
		"- `Book` checks the quote's expiry time before it books, and returns `ErrQuoteExpired` once it has passed.\n" +
		"- The booking API answers an expired quote with `410 Gone` and a link to request a new quote.\n\n" +
		"## Why\n\n" +
		"The task's criteria: an expired quote can't be booked, and the customer is told how to get a new one. " +
		"Quotes already booked are unaffected, as the check runs only at booking.\n\n" +
		"## Tests\n\n" +
		"- added: TestBookExpiredQuote: a quote past its expiry time is refused with ErrQuoteExpired\n" +
		"- added: TestBookQuoteAtExpiry: a quote booked at its exact expiry time is refused\n" +
		"- added: TestBookingAPIExpiredQuote: the API answers 410 with the new-quote link\n" +
		"- updated: TestBook: its quotes get an expiry time in the future\n\n" +
		fmt.Sprintf("<!-- issuedog:transition from=in-progress to=verifying attempt=%d run=%s head=%s seen=%d -->", attempt, run, head, seen)
}

// A task at in-progress gets an implementer run, as a Committer: its
// commit, made with the App's identity from the run's environment, becomes
// the task's head, and the task moves to verifying, keyed by the run, with
// head= and a comment naming the head, the PR title and the summary. A
// task at verifying gets no run, and an unchanged pass after makes no
// request but conditional GETs answered 304 (SPEC §7.2, §8.1).
func TestImplementerCommitsMoveToVerifying(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, implementerOutput(t, "commits"))
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")

	g.pass()
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times, want once", g.runs())
	}
	head := g.trimmed("commit.1")
	if l := g.leaseOf("1"); l.Stage != state.InProgress || l.Start != main {
		t.Errorf("the run's lease = %+v; want it at in-progress, started at main's %s", l, main)
	}
	wantAuthor := "issuedog[bot] <issuedog[bot]@users.noreply.github.com>"
	if got := g.trimmed("author.1"); got != wantAuthor+"\n"+wantAuthor {
		t.Errorf("the run's commit has author and committer\n%s\nwant the App's, %s, as both", got, wantAuthor)
	}
	if got := g.bareRefs(); got != wantRefs(main, head) {
		t.Errorf("the bare repo's refs =\n%s\nwant\n%s", got, wantRefs(main, head))
	}
	if labels := f.State().Issues[n-1].Labels; !slices.Contains(labels, "stage:verifying") || slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v, want stage:verifying in place of stage:in-progress", labels)
	}
	cs := implementerComments(t, f, n)
	last := cs[len(cs)-1]
	if want := implementerCommitted("run1", head, 1, cs[len(cs)-2].ID); last.Author.Login != app || last.Body != want {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author.Login, last.Body, want)
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v after the run; want no lease, result, bundle or clone", left)
	}

	writes := len(f.Writes())
	g.pass()
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("the pass after wrote %+v, with %d runs in all; want nothing, and 1", w[writes:], g.runs())
	}
	from, notModified := len(f.Requests()), f.NotModified()
	g.pass()
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
	if f.NotModified() == notModified {
		t.Error("an unchanged pass got no 304")
	}
}

// The implementer runs only on a task that gets a PR: never on a human
// task, nor on an issue of another type at in-progress, such as an epic
// the owner moved there by hand.
func TestImplementerOnlyOnTasks(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		file func(f *githubtest.Fake) int
	}{
		{"a human task", func(f *githubtest.Fake) int {
			// The App files a human task, with human=1 on its origin marker.
			n := f.AddIssue(app, "Add the secret", "Add the secret.\n\n<!-- issuedog:origin plan=#9 rev=1 item=T1 human=1 -->")
			f.SetType(n, "Task")
			f.AddLabel(n, app, "stage:in-progress")
			f.AddComment(n, app, "<!-- issuedog:transition from=intake to=in-progress run=rin-progress -->")
			return n
		}},
		{"an epic", func(f *githubtest.Fake) int {
			n := fileAt(f, "in-progress", "An idea.")
			f.SetType(n, "Epic")
			return n
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			tt.file(f)
			g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, implementerOutput(t, "commits"))
			g.pass()
			if g.runs() != 0 {
				t.Errorf("claude ran %d times on %s at in-progress, want never", g.runs(), tt.name)
			}
		})
	}
}

// Each of the implementer's check rules fails the run as a failed
// attempt, with nothing written, and the lease names the rule: the
// pairing of commits with its fields, commits with no commits past the
// run's start, a PR title type not on pr.title_types, a title over
// GitHub's limit or not on one line, a comment over the length limit, and
// a hand-off with a commits outcome's field, whose other rules
// TestImplementerHandoffRules covers. Nothing moves the task's head. A
// question's rules are TestImplementerQuestionCheckRules'.
func TestImplementerCheckRules(t *testing.T) {
	t.Parallel()
	question, handoff := implementerOutput(t, "question"), implementerOutput(t, "handoff")
	for _, tt := range []struct {
		name      string
		change    func(out map[string]any)
		noCommits bool
		want      string
	}{
		{name: "no commits", noCommits: true, want: "rule commits: no commits past the run's start"},
		{name: "a type not on pr.title_types", change: func(out map[string]any) { title(out)["type"] = "docs" },
			want: `rule title-type: the PR title's type "docs" isn't one of pr.title_types, feat, fix, chore`},
		{name: "a type in another case", change: func(out map[string]any) { title(out)["type"] = "Feat" },
			want: `rule title-type: the PR title's type "Feat" isn't one of pr.title_types, feat, fix, chore`},
		{name: "a title over 256 characters", change: func(out map[string]any) { title(out)["sentence"] = strings.Repeat("é", 251) },
			want: "rule title-length: the PR title is 257 characters, over 256"},
		{name: "a blank sentence", change: func(out map[string]any) { title(out)["sentence"] = "\u00a0" }, // a no-break space
			want: `rule one-line: the PR title "feat: \u00a0"`},
		{name: "a comment over the limit", change: func(out map[string]any) { summary(out)["why"] = strings.Repeat("a", 64512) },
			want: "rule length: the comment is "},
		{name: "commits with no title", change: func(out map[string]any) { delete(out, "title") }, want: "rule outcome: commits has a title"},
		{name: "commits with no summary", change: func(out map[string]any) { delete(out, "summary") }, want: "rule outcome: commits has a summary"},
		{name: "commits with questions", change: func(out map[string]any) { out["questions"] = question["questions"] },
			want: "rule outcome: commits has no questions"},
		{name: "commits with a hand-off", change: func(out map[string]any) { out["handoff"] = handoff["handoff"] },
			want: "rule outcome: commits has no hand-off"},
		{name: "a hand-off with a title", change: func(out map[string]any) {
			out["outcome"], out["handoff"] = "handoff", handoff["handoff"]
			delete(out, "summary")
		}, want: "rule outcome: handoff has no title"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			implementerTask(f)
			out := implementerOutput(t, "commits")
			if tt.change != nil {
				tt.change(out)
			}
			g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, out)
			g.commits(!tt.noCommits)
			main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
			writes := len(f.Writes())
			g.pass()
			if g.runs() != 1 {
				t.Fatalf("claude ran %d times, want once", g.runs())
			}
			if w := f.Writes()[writes:]; len(w) != 0 {
				t.Errorf("the failed run wrote %+v; want nothing", w)
			}
			if l := g.lease(); !l.Failed || l.Attempt != 1 || !strings.Contains(l.PublicFailure, tt.want) {
				t.Errorf("the lease = %+v; want a failed attempt, 1, naming %q", l, tt.want)
			}
			if got := g.bareRefs(); got != wantRefs(main, "") {
				t.Errorf("the bare repo's refs =\n%s\nwant main's only:\n%s", got, wantRefs(main, ""))
			}
		})
	}
}

// title is the golden commits output's title, to change.
func title(out map[string]any) map[string]any { return out["title"].(map[string]any) }

// summary is the golden commits output's summary, to change.
func summary(out map[string]any) map[string]any { return out["summary"].(map[string]any) }

// The move to verifying, a crash at each of its writes, then a restart:
// the replay ends with the same writes, the task's head moved once, and
// the bundle fetched once (SPEC §5.2).
func TestImplementerApplyReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	roots, clones, packs := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}, map[*githubtest.Fake]int{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		implementerTask(f)
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g := newImplementerRig(t, f, o, roots[f], clones[f], implementerOutput(t, "commits"))
		// The commit's dates are fixed, so each replay's commit, and the
		// comment naming it, is the clean run's.
		g.r.Env = append(g.r.Env, "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		if clones[f] == nil {
			clones[f], packs[f] = g.d.Clone, len(packFiles(t, g.d.Clone.Dir()))
		}
		return g.work(1)
	})
	for f, clone := range clones {
		g := &rig{t: t, f: f, root: roots[f], d: &daemon.Daemon{Clone: clone}}
		head := g.trimmed("commit.1")
		main := gitIn(t, clone.Dir(), "rev-parse", "main")
		if got := g.bareRefs(); got != wantRefs(main, head) || g.runs() != 1 {
			t.Errorf("the bare repo's refs after the replay =\n%s\nwith %d runs; want\n%s\nwith 1 run", got, g.runs(), wantRefs(main, head))
		}
		if got := len(packFiles(t, clone.Dir())); got != packs[f]+1 {
			t.Errorf("the bare repo has %d packs after the replay, want %d: one fetch past the sync's", got, packs[f]+1)
		}
		labels, cs := f.State().Issues[0].Labels, implementerComments(t, f, 1)
		last := cs[len(cs)-1]
		if want := implementerCommitted("run1", head, 1, cs[len(cs)-2].ID); last.Body != want || !slices.Contains(labels, "stage:verifying") {
			t.Errorf("after the replay, the newest comment =\n%s\nwith labels %v; want\n%s\nat verifying", last.Body, labels, want)
		}
	}
}

// Task work comes after the other stages and triage (SPEC §8.1, step 4):
// with an epic at clarifying, an issue for triage and three tasks at
// in-progress, the clarifying stage's run is set up first, even below the
// tasks' priority, then triage's, and only then the implementer's, on the
// task first by priority, then number, whatever order Work gets them in:
// here the newest first. The fake stages fail at their context, as a
// setup failure, uncounted, so each pass stops at the run it set up.
func TestImplementerRunsAfterOtherStages(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	low := implementerTask(f) // #1, p1
	f.RemoveLabel(low, app, "priority:p1")
	f.AddLabel(low, app, "priority:p2")
	interview(f)                                             // #2, an epic at clarifying, p2
	untriaged := f.AddIssue("owner", "Something", "an idea") // #3, no stage
	high := implementerTask(f)                               // #4, p1
	f.RemoveLabel(high, app, "priority:p1")
	f.AddLabel(high, app, "priority:p0")
	later := implementerTask(f) // #5, p1
	f.RemoveLabel(later, app, "priority:p1")
	f.AddLabel(later, app, "priority:p0")
	g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, implementerOutput(t, "commits"))
	var set []string
	setup := errors.New("no context")
	clarify := &fakeStage{contextErr: setup, during: func() { set = append(set, "clarifying") }}
	triage := &fakeStage{agent: "triage", contextErr: setup, pending: func(in daemon.Input) bool { return in.S.Number == untriaged },
		during: func() { set = append(set, "triage") }}
	g.r.Stages[state.Clarifying], g.r.Unlabeled = clarify, triage

	work := func() error {
		ctx := context.Background()
		var ready []daemon.Ready
		for n := later; n >= 1; n-- {
			s, err := g.d.Step.Client.Snapshot(ctx, "o", "r", n)
			if err != nil {
				t.Fatal(err)
			}
			in, err := g.d.Step.Run(ctx, s)
			if err != nil {
				t.Fatal(err)
			}
			ready = append(ready, daemon.Ready{S: s, Inputs: in})
		}
		return g.r.Work(ctx, ready)
	}
	if err := work(); !errors.Is(err, setup) || !slices.Equal(set, []string{"clarifying"}) || g.runs() != 0 {
		t.Fatalf("the first Work = %v, setting up %v, with %d claude runs; want the clarifying stage's setup failure, and no run", err, set, g.runs())
	}
	clarify.pending = func(daemon.Input) bool { return false }
	if err := work(); !errors.Is(err, setup) || !slices.Equal(set, []string{"clarifying", "triage"}) || g.runs() != 0 {
		t.Fatalf("the second Work = %v, setting up %v, with %d claude runs; want triage's setup failure, and no run", err, set, g.runs())
	}
	triage.pending = func(daemon.Input) bool { return false }
	if err := work(); err != nil || g.runs() != 1 {
		t.Fatalf("the third Work = %v, with %d claude runs; want the implementer's run", err, g.runs())
	}
	if l := g.leaseOf("1"); l.Issue != high {
		t.Errorf("the implementer ran on #%d, want #%d, the first task at p0, before #%d, also at p0, and #%d at p2", l.Issue, high, later, low)
	}
}

// A run whose move to verifying is written, but whose labels a crash cut
// short, only finishes on the next pass: it's not checked again, so a
// change to pr.title_types since, which its title type is no longer on,
// doesn't hold it back. The task's head moves, and the run's files go
// (SPEC §8.2, Checks).
func TestImplementerMovedRunFinishes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, implementerOutput(t, "commits"))
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	f.FailWrite(len(f.Writes()) + 2) // the move's comment, then its label
	if err := g.work(n); err == nil {
		t.Fatal("the run's apply wrote every label; want it cut short after the move's comment")
	}
	f.FailWrite(0)
	g.d.Config.PR.TitleTypes = []string{"fix"}
	if err := g.work(n); err != nil {
		t.Fatalf("the next pass: %v", err)
	}
	head := g.trimmed("commit.1")
	if got := g.bareRefs(); got != wantRefs(main, head) || g.runs() != 1 {
		t.Errorf("the bare repo's refs =\n%s\nwith %d runs; want\n%s\nwith 1 run", got, g.runs(), wantRefs(main, head))
	}
	if labels := f.State().Issues[n-1].Labels; !slices.Contains(labels, "stage:verifying") || slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v, want stage:verifying in place of stage:in-progress", labels)
	}
	if cs := implementerComments(t, f, n); len(cs) != 2 {
		t.Errorf("the task has %d comments, want 2: its move to in-progress, and the one move to verifying", len(cs))
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v; want no lease, result, bundle or clone", left)
	}
}

// Apply renders nothing from output that fails the check, even when called
// without Check: a commits result with no head writes nothing.
func TestImplementerApplyChecks(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := newImplementerRig(t, f, newOrigin(t), t.TempDir(), nil, implementerOutput(t, "commits"))
	s, err := g.d.Step.Client.Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(implementerOutput(t, "commits"))
	if err != nil {
		t.Fatal(err)
	}
	im := &daemon.Implementer{Daemon: g.d}
	writes := len(f.Writes())
	err = im.Apply(context.Background(), g.d.Step.Applier, daemon.Result{RunID: "run1", S: s, Dirt: daemon.Dirt{}, Output: raw})
	if err == nil || !strings.Contains(err.Error(), "rule commits: no commits past the run's start") {
		t.Errorf("Apply = %v, want the commits rule broken", err)
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("Apply wrote %+v; want nothing", w)
	}
}

// implementerComments are issue n's comments, with their IDs, as a fresh
// client reads them.
func implementerComments(t testing.TB, f *githubtest.Fake, n int) []github.Comment {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	return s.Comments
}
