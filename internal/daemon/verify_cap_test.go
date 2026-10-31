package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// This file is the verify cap's hand-off (SPEC §8.3, §9.3): when the head
// of a task's round's last attempt fails verify, the task moves verifying
// → needs-human on attempt-cap, keyed as the findings move would be, with
// a hand-off giving the findings, never back to in-progress. /done or a
// trusted 👍 on it starts a new round, and while it waits, the digest
// lists it and notify is assigned.

// verifyCapFields is the verify cap's hand-off on task 1 from its fields
// on, with the cc line for owner.
const verifyCapFields = "**Goal:** Find out why this task fails verify, and change what makes it fail.\n" +
	"**Why it blocks:** #1 can't proceed until what makes its head fail verify is fixed\n" +
	"**Steps:**\n" +
	"1. Read the findings above.\n" +
	"2. Fix their cause, such as an unclear criterion in this task's body, a failure on `main`, or a setting in `.issuedog/config.yaml`.\n\n" +
	"**Links:**\nNone.\n\n" +
	"**Done when:** What made the task's head fail verify is fixed.\n" +
	"**Estimated time:** It depends on the findings.\n\n" +
	"When it's done, reply `/done` on this issue or react 👍 to this comment, and the task goes back to `ready`.\n\n" +
	"cc @owner"

// verifyCapText is the verify cap's hand-off on task 1, without its
// marker, for head failing roundConfig's one command on attempt of limit,
// against main.
func verifyCapText(limit, attempt int, head, main string) string {
	return fmt.Sprintf("This task's %d attempts in this round failed, so I've stopped running the implementer here.\n\n"+
		"Verify failed on attempt %d of %d, at `%s`, checked against `main` at `%s`.\n\n"+
		"### Rule 1: verify.commands\n\nThis command failed, exit status 1:\n\n```text\nfalse\n```\n\nIt printed nothing.\n\n",
		limit, attempt, limit, head, main) + verifyCapFields
}

// verifyCapMarker is the verify cap's move's marker, keyed by move, the
// comment holding the move to verifying whose head failed, with seen, the
// newest comment when the checks started, as the findings move has it.
func verifyCapMarker(move, seen int64) string {
	return "<!-- issuedog:transition from=verifying to=needs-human event=v" + strconv.FormatInt(move, 10) +
		" seen=" + strconv.FormatInt(seen, 10) + " -->"
}

// Every attempt of a round commits a head that fails verify, with
// loop.max_attempts 2 and 3. Below the cap, the findings move the task
// back to in-progress, each carrying the attempt= of the latest move into
// verifying, not the first. On the last attempt, they move it verifying →
// needs-human on attempt-cap, keyed event=v<comment id> by the move into
// verifying, with seen= that move, the newest comment when the checks
// started, and the hand-off: never back to in-progress. No run starts
// after, and an unchanged pass costs only conditional GETs answered 304,
// one of them the hand-off's reactions. /done starts a new round: the next
// run is attempt 1, at effort low.
func TestVerifyCapHandsOff(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{2, 3} {
		t.Run("max_attempts "+strconv.Itoa(limit), func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			n := implementerTask(f)
			commits := ivResult(t, implementerOutput(t, "commits"))
			outs := slices.Repeat([]string{commits}, limit+1)
			g := roundRig(t, f, roundConfig(limit, "high"), outs...)
			key := restartKey(t, f, n)
			main := gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")

			var move int64
			for k := 1; k <= limit; k++ {
				g.pass()
				cs := implementerComments(t, f, n)
				move = cs[len(cs)-1].ID
				g.pass()
				cs = implementerComments(t, f, n)
				if k == limit {
					break
				}
				head := g.trimmed("commit." + strconv.Itoa(k))
				if got, want := cs[len(cs)-1].Body, findingsAttemptMarker(move, head, k, move); !strings.HasSuffix(got, want) || pickStage(f, n) != "stage:in-progress" {
					t.Fatalf("after attempt %d's checks, the newest comment =\n%s\nwith the task at %s; want the findings, ending %s, at in-progress",
						k, got, pickStage(f, n), want)
				}
			}
			head := g.trimmed("commit." + strconv.Itoa(limit))
			cs := implementerComments(t, f, n)
			if got, want := cs[len(cs)-1].Body, verifyCapText(limit, limit, head, main)+"\n\n"+verifyCapMarker(move, move); got != want || pickStage(f, n) != "stage:needs-human" {
				t.Fatalf("after the last attempt's checks, the newest comment =\n%s\nwith the task at %s; want\n%s\nat needs-human", got, pickStage(f, n), want)
			}
			for _, c := range cs {
				if strings.Contains(c.Body, fmt.Sprintf("to=in-progress attempt=%d ", limit)) {
					t.Errorf("comment %d moves the task back to in-progress on the last attempt:\n%s", c.ID, c.Body)
				}
			}

			g.pass()
			from, writes := len(f.Requests()), len(f.Writes())
			g.pass()
			for _, r := range f.Requests()[from:] {
				if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
					t.Errorf("an unchanged pass after the hand-off sent %+v; want only conditional GETs answered 304", r)
				}
			}
			if reads := handbackReads(f, from, cs[len(cs)-1].ID); reads != 1 {
				t.Errorf("an unchanged pass read the hand-off's reactions %d times; want once", reads)
			}
			if w := f.Writes(); len(w) != writes || g.runs() != limit {
				t.Errorf("an unchanged pass after the hand-off wrote %+v, with %d runs; want nothing, and %d", w[writes:], g.runs(), limit)
			}

			f.AddComment(n, "owner", "/done")
			g.pass()
			l := g.leaseOf(strconv.Itoa(limit + 1))
			if g.runs() != limit+1 || l.Attempt != 1 || l.AttemptKey == key || g.roundEffort(limit+1) != "low" {
				t.Errorf("after /done: %d runs, the run leased %+v with effort %q; want %d, attempt 1 of a new round, not %s, at low",
					g.runs(), l, g.roundEffort(limit+1), limit+1, key)
			}
			if got := g.roundInput(limit + 1); strings.Contains(got, "findings of verify") {
				t.Errorf("after /done, the run's input.md =\n%s\nwant no findings", got)
			}
		})
	}
}

// A round whose first run fails and whose second commits a head that fails
// verify, with loop.max_attempts 2: the hand-off gives the findings, and
// quotes no run's failure, as the last attempt's run didn't fail.
func TestVerifyCapAfterFailedRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	g := roundRig(t, f, roundConfig(2, "high"), failOut, ivResult(t, implementerOutput(t, "commits")))
	g.pass()
	g.pass()
	cs := implementerComments(t, f, n)
	move := cs[len(cs)-1].ID
	g.pass()
	head, main := g.trimmed("commit.2"), gitIn(t, g.d.Clone.Dir(), "rev-parse", "main")
	cs = implementerComments(t, f, n)
	got := cs[len(cs)-1].Body
	if want := verifyCapText(2, 2, head, main) + "\n\n" + verifyCapMarker(move, move); got != want || pickStage(f, n) != "stage:needs-human" {
		t.Errorf("the newest comment =\n%s\nwith the task at %s; want\n%s\nat needs-human", got, pickStage(f, n), want)
	}
	if strings.Contains(got, "error_max_turns") || strings.Contains(got, "a run, failed") {
		t.Errorf("the hand-off =\n%s\nwant no run's failure quoted", got)
	}
}

// The verify cap's move, a crash at each of its writes, then a restart:
// the replay ends with the same writes (SPEC §5.2), and the hand-off is
// the newest comment, keyed by the move into verifying.
func TestVerifyCapReplay(t *testing.T) {
	t.Parallel()
	cfg := verifyConfig("false") + "loop:\n  max_attempts: 2\nnotify: [owner]\n"
	o := verifyOrigin(t, cfg)
	head := startHead(t, o, map[string]string{"a.go": "package a\n", "a_test.go": "package a\n"}, false)
	gitIn(t, o.work, "push", "--quiet", o.url, "task")
	main := gitIn(t, o.work, "rev-parse", "main")
	parsed, err := config.Parse([]byte(cfg))
	if err != nil {
		t.Fatal(err)
	}
	moves := map[*githubtest.Fake]int64{}
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		moves[f] = roundVerifying(f, head)
		return f
	}, func(f *githubtest.Fake) error {
		g := verifyRig(t, f, o)
		g.d.Config = parsed
		return g.work(1)
	})
	for f, move := range moves {
		cs := implementerComments(t, f, 1)
		if got, want := cs[len(cs)-1].Body, verifyCapText(2, 2, head, main)+"\n\n"+verifyCapMarker(move, move); got != want || pickStage(f, 1) != "stage:needs-human" {
			t.Errorf("after the replay, the newest comment =\n%s\nwith the task at %s; want\n%s\nat needs-human", got, pickStage(f, 1), want)
		}
	}
}

// verifyCapTask is newFake with issue 1 a task the owner filed, at
// needs-human after the verify cap's hand-off, with an earlier hand-off
// before it: its move to in-progress, 1001, the implementer's hand-off,
// 1002, a 👍 on it that sent the task back to ready, 1003, its next move
// to in-progress, 1004, the implementer's move to verifying, 1005, and
// the verify cap's hand-off, 1006. Then it runs more.
func verifyCapTask(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return handbackTask(func(t testing.TB, f *githubtest.Fake) {
		f.AddComment(1, app, "<!-- issuedog:transition from=needs-human to=ready cmd=r77 seen=1002 -->")
		f.AddComment(1, app, "<!-- issuedog:transition from=ready to=in-progress event=p6 -->")
		move := f.AddComment(1, app, "The implementer committed.\n\n<!-- issuedog:transition from=in-progress to=verifying attempt=2 run=r2 head=abc -->")
		if id := f.AddComment(1, app, verifyCapText(2, 2, "abc", "def")+"\n\n"+verifyCapMarker(move, move)); id != 1006 {
			t.Fatalf("the verify cap's hand-off's comment ID = %d, want 1006", id)
		}
		more(t, f)
	})
}

// A trusted /done, or a trusted 👍 on the verify cap's hand-off, sends the
// task back to ready, a new round, as for any hand-off (SPEC §9.3), since
// a move into needs-human from verifying is one. An untrusted /done or 👍
// does nothing, and nor does a trusted 👍 on the earlier hand-off.
func TestVerifyCapEnds(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		more func(t testing.TB, f *githubtest.Fake)
		want string // the move to ready's marker, or "" for none
	}{
		{"a trusted /done", doneBy("owner", 1007), "<!-- issuedog:transition from=needs-human to=ready cmd=1007 seen=1007 -->"},
		{"an untrusted /done", doneBy("eve", 1007), ""},
		{"a trusted 👍", func(t testing.TB, f *githubtest.Fake) {
			if id := f.AddReaction(1006, "owner", "+1"); id != 1007 {
				t.Fatalf("owner's 👍's ID = %d, want 1007", id)
			}
		}, "<!-- issuedog:transition from=needs-human to=ready cmd=r1007 seen=1006 -->"},
		{"an untrusted 👍", func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1006, "eve", "+1") }, ""},
		{"a trusted 👍 on the earlier hand-off", func(_ testing.TB, f *githubtest.Fake) { f.AddReaction(1002, "owner", "+1") }, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := verifyCapTask(tt.more)(t)
			iterate(t, handbackDaemon(t, f, newOrigin(t)))
			is := f.State().Issues[0]
			if tt.want == "" {
				if w := f.Writes(); len(w) != 0 || !slices.Equal(is.Labels, []string{"stage:needs-human"}) {
					t.Errorf("writes %+v, labels %q; want none, at needs-human", w, is.Labels)
				}
				return
			}
			if last := is.Comments[len(is.Comments)-1].Body; !slices.Equal(is.Labels, []string{"stage:ready"}) || !strings.HasSuffix(last, tt.want) {
				t.Errorf("labels %q, newest comment\n%s\nwant ready, on a move ending %s", is.Labels, last, tt.want)
			}
		})
	}
}

// While a task waits on the verify cap's hand-off, the digest lists it as
// a hand-off for failed verifies, with its Goal and "Done when" copied
// from the comment, how to finish it and a link to the comment, and notify
// is assigned. Both end on the pass whose 👍 sends the task back to ready.
func TestVerifyCapItem(t *testing.T) {
	t.Parallel()
	f, o := verifyCapTask(func(testing.TB, *githubtest.Fake) {})(t), digestNotifyOrigin(t)
	d := digestDaemon(t, f, o, nil)
	d.Assign = true
	d.Step.Reactions = daemon.HandoffByReaction(handbackTrust)
	iterate(t, d)
	digestN := len(f.State().Issues)
	if want := []string{"#1 Hand-off p2 1"}; !slices.Equal(handbackListed(f, digestN), want) {
		t.Errorf("the digest lists %q; want %q", handbackListed(f, digestN), want)
	}
	body := f.State().Issues[digestN-1].Body
	for _, line := range []string{
		"- A hand-off waits for you: the task's `loop.max_attempts` attempts in its round failed, the last at verify, and the hand-off has the findings.\n",
		"- Goal: Find out why this task fails verify, and change what makes it fail.\n",
		"- Done when: What made the task's head fail verify is fixed.\n",
		"- When it's done, reply `/done` on the issue or react 👍 to the hand-off.\n",
		"- Answer here: https://github.com/o/r/issues/1#issuecomment-1006\n",
	} {
		if !strings.Contains(body, line) {
			t.Errorf("the digest has no line %q:\n%s", line, body)
		}
	}
	if got := assignees(f, 1); !slices.Equal(got, []string{"owner"}) {
		t.Errorf("the task's assignees = %q; want owner", got)
	}
	f.AddReaction(1006, "owner", "+1")
	iterate(t, d)
	if got := handbackListed(f, digestN); len(got) != 0 {
		t.Errorf("after the 👍, the digest lists %q; want no hand-off", got)
	}
	if got := assignees(f, 1); len(got) != 0 {
		t.Errorf("after the 👍, the task's assignees = %q; want none", got)
	}
	if l := f.State().Issues[0].Labels; !slices.Equal(l, []string{"stage:ready"}) {
		t.Errorf("after the 👍, the task's labels = %q; want ready", l)
	}
}
