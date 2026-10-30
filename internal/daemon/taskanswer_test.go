package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// taskAnswerRound is the implementer's round 1 on a task, as its move to
// clarifying posts it.
const taskAnswerRound = "### Questions for #1 (round 1)\n\n" +
	"**1. Refuse or accept a quote at its expiry second?** Blocks: #1\n" +
	"   a. Refuse (recommended): it has expired\n" +
	"   b. Accept\n\n" +
	"Reply in one comment, e.g. `1a 2c 3: free text`.\n\n" +
	"<!-- issuedog:transition from=in-progress to=clarifying round=1 run=run1 questions=1 -->"

// taskAnswerTask files a task at in-progress, as the App moved it there,
// then the implementer's move to clarifying, which asks round 1, as the
// App writes it: its comment, then the labels.
func taskAnswerTask(f *githubtest.Fake) int {
	n := implementerTask(f)
	f.AddComment(n, app, taskAnswerRound)
	f.AddLabel(n, app, "stage:clarifying")
	f.RemoveLabel(n, app, "stage:in-progress")
	return n
}

// taskAnswered is the App's comment on a task's move back to ready on
// answers to round 1, keyed by the first, with seen the newest.
func taskAnswered(first, newest int64) string {
	return fmt.Sprintf("Round 1 has answers, so the task goes back to `ready`. The implementer's next run gets them.\n\n"+
		"<!-- issuedog:transition from=clarifying to=ready cmd=%d seen=%d -->", first, newest)
}

// A trusted answer to the implementer's round moves the task clarifying →
// ready in step 1 (SPEC §7.2, §9.1), keyed by the first answer, with seen
// the newest, so both answers are its. Each gets the App's 👍, as any
// answer does. The pass after writes nothing, and an unchanged pass makes
// no request but conditional GETs answered 304.
func TestTaskAnswerMovesToReady(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := taskAnswerTask(f)
	first := f.AddComment(n, "owner", "1a")
	newest := f.AddComment(n, "owner", "And keep the error text short.")
	d := newDaemon(t, f, newOrigin(t), nil)
	from := len(f.Requests())
	iterate(t, d)
	// Step 1 reads each answer's reactions once, to 👍 it, and the move
	// reads them no more.
	reads := 0
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/comments/%d/reactions", first) {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the pass read the first answer's reactions %d times, want once", reads)
	}

	is := f.State().Issues[n-1]
	if !slices.Contains(is.Labels, "stage:ready") || slices.Contains(is.Labels, "stage:clarifying") {
		t.Errorf("the task's labels are %v, want stage:ready in place of stage:clarifying", is.Labels)
	}
	if last := is.Comments[len(is.Comments)-1]; last.Author != app || last.Body != taskAnswered(first, newest) {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author, last.Body, taskAnswered(first, newest))
	}
	for _, c := range is.Comments[len(is.Comments)-3 : len(is.Comments)-1] {
		if !slices.Equal(c.Reactions, []string{app + " +1"}) {
			t.Errorf("answer %q has reactions %q, want the App's 👍", c.Body, c.Reactions)
		}
	}

	writes := len(f.Writes())
	iterate(t, d)
	if w := f.Writes(); len(w) != writes {
		t.Errorf("the pass after wrote %+v; want nothing", w[writes:])
	}
	from = len(f.Requests())
	iterate(t, d)
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
}

// Only a trusted, clean answer that isn't a command moves the task: a
// command, an untrusted comment, the App's own comment and another bot's
// move nothing, and the task stays at clarifying. Nor does a trusted
// comment on a task at clarifying whose latest move isn't the
// implementer's round, such as one the owner set there by hand, or an
// owner's task that waits there (SPEC §7.2).
func TestTaskAnswerIgnores(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		file func(f *githubtest.Fake)
	}{
		{"a command", func(f *githubtest.Fake) { f.AddComment(taskAnswerTask(f), "owner", "/priority p0") }},
		{"an untrusted comment", func(f *githubtest.Fake) { f.AddComment(taskAnswerTask(f), "eve", "1a") }},
		{"the App's comment", func(f *githubtest.Fake) { f.AddComment(taskAnswerTask(f), app, "1a") }},
		{"another bot's comment", func(f *githubtest.Fake) { f.AddComment(taskAnswerTask(f), "other[bot]", "1a") }},
		{"a task the owner set to clarifying by hand", func(f *githubtest.Fake) {
			n := implementerTask(f)
			f.AddComment(n, app, "<!-- issuedog:transition from=in-progress to=clarifying by=owner event=900 -->")
			f.AddLabel(n, app, "stage:clarifying")
			f.RemoveLabel(n, app, "stage:in-progress")
			f.AddComment(n, "owner", "1a")
		}},
		{"a task with no implementer round", func(f *githubtest.Fake) {
			n := fileAt(f, "clarifying", "Refuse an expired quote.")
			f.SetType(n, "Task")
			f.AddComment(n, "owner", "1a")
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			tt.file(f)
			iterate(t, newDaemon(t, f, newOrigin(t), nil))
			is := f.State().Issues[0]
			if !slices.Contains(is.Labels, "stage:clarifying") || slices.Contains(is.Labels, "stage:ready") {
				t.Errorf("the task's labels are %v, want stage:clarifying still", is.Labels)
			}
			for _, c := range is.Comments {
				if strings.Contains(c.Body, "to=ready") {
					t.Errorf("the task has a move to ready, %q; want none", c.Body)
				}
			}
		})
	}
}

// A task closed since its round waits at clarifying no more: a visit to
// it, as one read just after it closed, before step 1 records the close,
// writes nothing for its owner's answer, though its markers still have it
// at clarifying.
func TestTaskAnswerClosedTask(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := taskAnswerTask(f)
	f.AddComment(n, "owner", "1a")
	f.Close(n)
	d := newDaemon(t, f, newOrigin(t), nil)
	d.Step.Trust = ivTrust
	s, err := d.Step.Client.Snapshot(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Closed {
		t.Fatalf("the task's snapshot isn't closed; want it closed")
	}
	writes := len(f.Writes())
	if err := d.Step.AnswerVisit(t.Context(), s); err != nil {
		t.Fatalf("AnswerVisit: %v", err)
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("AnswerVisit on the closed task wrote %+v; want nothing", w)
	}
}

// A task retyped since its round, here to a story, has no clarifying →
// ready row on answered (SPEC §7.2), so the answer's move is a state
// error (SPEC §5.3): it's reported once, with an error comment keyed by
// the answer, and the pass goes on, as do the passes after, which report
// it no more. The task stays at clarifying.
func TestTaskAnswerRetypedReportsOnce(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := taskAnswerTask(f)
	f.SetType(n, "Story")
	answer := f.AddComment(n, "owner", "1a")
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	iterate(t, d)

	is := f.State().Issues[n-1]
	if !slices.Contains(is.Labels, "stage:clarifying") || slices.Contains(is.Labels, "stage:ready") {
		t.Errorf("the task's labels are %v, want stage:clarifying still", is.Labels)
	}
	key := fmt.Sprintf("<!-- issuedog:error kind=transition key=%d -->", answer)
	reports := 0
	for _, c := range is.Comments {
		if strings.Contains(c.Body, "to=ready") {
			t.Errorf("the task has a move to ready, %q; want none", c.Body)
		}
		if c.Author == app && strings.HasSuffix(c.Body, key) {
			reports++
		}
	}
	if reports != 1 {
		t.Errorf("the task has %d error comments ending %s; want 1. Its comments are %+v", reports, key, is.Comments)
	}
}

// The answer's move, a crash at each of its writes, then a restart: the
// replay ends with the same writes, the answer's 👍 and the move once
// (SPEC §5.2).
func TestTaskAnswerReplay(t *testing.T) {
	t.Parallel()
	o := newOrigin(t)
	githubtest.Replay(t, func(tb testing.TB) *githubtest.Fake {
		f := newFake(tb)
		f.AddComment(taskAnswerTask(f), "owner", "1a")
		return f
	}, func(f *githubtest.Fake) error {
		return newDaemon(t, f, o, nil).Iterate(t.Context())
	})
}

// A task waiting at clarifying for its round's answer costs no request
// past the poll's: the pass after the round writes nothing, and an
// unchanged pass makes no request but conditional GETs answered 304
// (SPEC §8.1).
func TestTaskAnswerWaitIsQuiet(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	taskAnswerTask(f)
	d := newDaemon(t, f, newOrigin(t), nil)
	iterate(t, d)
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("the first pass wrote %+v; want nothing while the round waits", w)
	}
	from := len(f.Requests())
	iterate(t, d)
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
}

// A hold at clarifying keeps the implementer's round (SPEC §6.4, §9.1):
// once /resume sends the task back there, the owner's answer, posted
// while it was held, moves it to ready, keyed by that answer.
func TestTaskAnswerAfterResume(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	n := implementerTask(f)
	round := f.AddComment(n, app, taskAnswerRound)
	f.AddLabel(n, app, "stage:clarifying")
	f.RemoveLabel(n, app, "stage:in-progress")
	hold := f.AddComment(n, "owner", "/hold")
	f.AddComment(n, app, fmt.Sprintf("<!-- issuedog:transition from=clarifying to=held at=clarifying reason=owner cmd=%d seen=%d -->", hold, round))
	f.AddReaction(hold, app, "+1")
	f.AddLabel(n, app, "stage:held")
	f.RemoveLabel(n, app, "stage:clarifying")
	answer := f.AddComment(n, "owner", "1a")
	resume := f.AddComment(n, "owner", "/resume")
	f.AddComment(n, app, fmt.Sprintf("<!-- issuedog:transition from=held to=clarifying cmd=%d seen=%d -->", resume, round))
	f.AddReaction(resume, app, "+1")
	f.AddLabel(n, app, "stage:clarifying")
	f.RemoveLabel(n, app, "stage:held")
	iterate(t, newDaemon(t, f, newOrigin(t), nil))

	is := f.State().Issues[n-1]
	if !slices.Contains(is.Labels, "stage:ready") || slices.Contains(is.Labels, "stage:clarifying") {
		t.Errorf("the task's labels are %v, want stage:ready in place of stage:clarifying", is.Labels)
	}
	want := fmt.Sprintf("Round 1 has an answer, so the task goes back to `ready`. The implementer's next run gets it.\n\n"+
		"<!-- issuedog:transition from=clarifying to=ready cmd=%d seen=%d -->", answer, answer)
	if last := is.Comments[len(is.Comments)-1]; last.Author != app || last.Body != want {
		t.Errorf("the task's newest comment, by %s, =\n%s\nwant the App's\n%s", last.Author, last.Body, want)
	}
}
