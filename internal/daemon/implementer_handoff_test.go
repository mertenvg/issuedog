package daemon_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// The implementer's hand-off (SPEC §9.3, §12): a handoff result moves the
// task to needs-human with the hand-off comment. Its origin is
// digestNotifyOrigin, with notify: [owner], so the comment ends in its cc
// line.

// handoffPosted is the App's hand-off comment on task 1 for the golden
// handoff output, on the move of run with seen.
func handoffPosted(run string, seen int64) string {
	return "The implementer needs an action only you can do before it can go on with this task.\n\n" +
		"**Goal:** Give the booking tests the payment provider's sandbox key, as a repository secret.\n" +
		"**Why it blocks:** #1 can't proceed until the repo has the secret PAYMENTS_SANDBOX_KEY, " +
		"which the expired-quote refund test calls the sandbox with, and only the owner can add a secret\n" +
		"**Steps:**\n" +
		"1. Open the payment provider's dashboard, switch to Sandbox, and copy the secret key that starts with `sk_test_`.\n" +
		"2. In the repo's settings, under Secrets and variables, Actions, add a repository secret named `PAYMENTS_SANDBOX_KEY` with that key.\n" +
		"3. Reply `/done` on this issue.\n\n" +
		"**Links:**\n" +
		"- https://dashboard.payments.example.com/test/apikeys\n" +
		"- https://github.com/acme/booking/settings/secrets/actions\n\n" +
		"**Done when:** The repo's Actions secrets list PAYMENTS_SANDBOX_KEY.\n" +
		"**Estimated time:** 5 min\n\n" +
		"When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`.\n\n" +
		"cc @owner\n\n" +
		fmt.Sprintf("<!-- issuedog:transition from=in-progress to=needs-human run=%s seen=%d -->", run, seen)
}

// A handoff result moves the task in-progress → needs-human, keyed by the
// run, with the hand-off comment: the SPEC §9.3 template's fields, the
// steps numbered and the links bulleted, how to finish it, and the cc line
// for notify. The run's commit is the task's head, and the next pass runs
// nothing and writes nothing.
func TestImplementerHandoffMovesToNeedsHuman(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := newImplementerRig(t, f, digestNotifyOrigin(t), t.TempDir(), nil, implementerOutput(t, "handoff"))
	main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")

	g.pass()
	if g.runs() != 1 {
		t.Fatalf("claude ran %d times, want once", g.runs())
	}
	if labels := f.State().Issues[n-1].Labels; !slices.Contains(labels, "stage:needs-human") || slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v, want stage:needs-human in place of stage:in-progress", labels)
	}
	cs := implementerComments(t, f, n)
	last := cs[len(cs)-1]
	if want := handoffPosted("run1", cs[len(cs)-2].ID); last.Author.Login != app || last.Body != want {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author.Login, last.Body, want)
	}
	if head := g.trimmed("commit.1"); g.bareRefs() != wantRefs(main, head) {
		t.Errorf("the bare repo's refs =\n%s\nwant the run's commit as the task's head:\n%s", g.bareRefs(), wantRefs(main, head))
	}
	if left := g.leftovers(); len(left) != 0 {
		t.Errorf("left %v after the run; want no lease, result, bundle or clone", left)
	}
	writes := len(f.Writes())
	g.pass()
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("the pass after wrote %+v, with %d runs in all; want nothing, and 1", w[writes:], g.runs())
	}
}

// handoffFields is the golden handoff output's hand-off, to change.
func handoffFields(out map[string]any) map[string]any { return out["handoff"].(map[string]any) }

// Each of the hand-off's check rules fails the run as a failed attempt,
// with nothing written, and the lease names the rule: each SPEC §9.3 field
// left blank, here with a no-break space, which the schema's pattern lets
// through, a step or link left blank, a hand-off missing, a commits
// outcome's field or questions beside it, and a comment over the length
// limit. Nothing moves the task's head.
func TestImplementerHandoffRules(t *testing.T) {
	t.Parallel()
	commits, question := implementerOutput(t, "commits"), implementerOutput(t, "question")
	blank := " "
	for _, tt := range []struct {
		name   string
		change func(out map[string]any)
		want   string
	}{
		{"a blank goal", func(out map[string]any) { handoffFields(out)["goal"] = blank }, "rule human-task: the hand-off's goal is empty"},
		{"a blank why it blocks", func(out map[string]any) { handoffFields(out)["why_blocks"] = blank },
			"rule human-task: the hand-off's why it blocks is empty"},
		{"a blank step", func(out map[string]any) { handoffFields(out)["steps"] = []any{"Open it.", blank} },
			"rule human-task: the hand-off's step 2 is empty"},
		{"a blank link", func(out map[string]any) { handoffFields(out)["links"] = []any{blank} },
			"rule human-task: the hand-off's link 1 is empty"},
		{"a blank done when", func(out map[string]any) { handoffFields(out)["done_when"] = blank },
			"rule human-task: the hand-off's done when is empty"},
		{"a blank estimated time", func(out map[string]any) { handoffFields(out)["estimated_time"] = blank },
			"rule human-task: the hand-off's estimated time is empty"},
		{"no hand-off", func(out map[string]any) { delete(out, "handoff") }, "rule outcome: handoff has a hand-off"},
		{"a summary beside it", func(out map[string]any) { out["summary"] = commits["summary"] }, "rule outcome: handoff has no summary"},
		{"questions beside it", func(out map[string]any) { out["questions"] = question["questions"] }, "rule outcome: handoff has no questions"},
		{"a comment over the limit", func(out map[string]any) { handoffFields(out)["steps"] = []any{strings.Repeat("a", 64512)} },
			"rule length: the comment is "},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			implementerTask(f)
			out := implementerOutput(t, "handoff")
			tt.change(out)
			g := newImplementerRig(t, f, digestNotifyOrigin(t), t.TempDir(), nil, out)
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

// The move to needs-human, a crash at each of its writes, then a restart:
// the replay ends with the same writes, the hand-off posted once, and the
// task at needs-human (SPEC §5.2).
func TestImplementerHandoffReplay(t *testing.T) {
	t.Parallel()
	o := digestNotifyOrigin(t)
	roots, clones := map[*githubtest.Fake]string{}, map[*githubtest.Fake]*repo.Repo{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		implementerTask(f)
		roots[f] = t.TempDir()
		return f
	}, func(f *githubtest.Fake) error {
		g := newImplementerRig(t, f, o, roots[f], clones[f], implementerOutput(t, "handoff"))
		// work doesn't reload the config from main, as a pass does.
		g.d.Config.Notify = []string{"owner"}
		g.r.Env = append(g.r.Env, "GIT_AUTHOR_DATE=2026-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2026-01-01T00:00:00Z")
		if clones[f] == nil {
			clones[f] = g.d.Clone
		}
		return g.work(1)
	})
	for f := range clones {
		labels, cs := f.State().Issues[0].Labels, implementerComments(t, f, 1)
		last := cs[len(cs)-1]
		if want := handoffPosted("run1", cs[len(cs)-2].ID); last.Body != want || !slices.Contains(labels, "stage:needs-human") || len(cs) != 2 {
			t.Errorf("after the replay, %d comments, the newest =\n%s\nwith labels %v; want 2, the newest\n%s\nat needs-human", len(cs), last.Body, labels, want)
		}
	}
}

// A run whose move to needs-human is written, but whose labels a crash cut
// short, only finishes on the next pass: it isn't checked again, and the
// hand-off isn't posted again.
func TestImplementerHandoffMovedRunFinishes(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := newImplementerRig(t, f, digestNotifyOrigin(t), t.TempDir(), nil, implementerOutput(t, "handoff"))
	f.FailWrite(len(f.Writes()) + 2) // the move's comment, then its label
	if err := g.work(n); err == nil {
		t.Fatal("the run's apply wrote every label; want it cut short after the move's comment")
	}
	f.FailWrite(0)
	if err := g.work(n); err != nil {
		t.Fatalf("the next pass: %v", err)
	}
	if labels := f.State().Issues[n-1].Labels; !slices.Contains(labels, "stage:needs-human") || slices.Contains(labels, "stage:in-progress") {
		t.Errorf("the task's labels are %v, want stage:needs-human in place of stage:in-progress", labels)
	}
	if cs := implementerComments(t, f, n); len(cs) != 2 || g.runs() != 1 {
		t.Errorf("the task has %d comments after %d runs, want 2, its move to in-progress and the one hand-off, after 1", len(cs), g.runs())
	}
}
