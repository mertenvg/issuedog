package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// #216: a command on one issue that writes to another before the other's
// turn, as a parent's /resume writes its "ended" record to a child, makes
// the child skip step 1 that pass. With no items known for the child on
// the start, as on a restart's first pass, the digest would drop them for
// a pass and list them again on the next, two edits and a false "Nothing
// is waiting on you". So such a child is read again at its turn, and
// visited (digestRestartRead).

// digestRestartGets counts, by issue number, the GETs of each issue itself,
// "/issues/<n>", among f's requests from from on: one for each of the
// issue's reads, as Snapshot reads the issue first.
func digestRestartGets(f *githubtest.Fake, from int) map[int]int {
	gets := map[int]int{}
	for _, r := range f.Requests()[from:] {
		rest, ok := strings.CutPrefix(r.Path, "/issues/")
		if n, err := strconv.Atoi(rest); ok && err == nil && r.Method == http.MethodGet {
			gets[n]++
		}
	}
	return gets
}

// digestRestartHeld runs l, on discRunFamily, up to TestDiscRunParentResume's
// "#3 held for discussion": a pass that knows every issue, then the epic's
// /hold, the tasks' own /holds, and mallory's comment on #3, which the
// digest lists and announces, and which assigns owner to #3. It checks
// that the /hold, on a pass after the start's first, reads the tasks only
// as the /hold's walk of the family does, once each. It returns mallory's
// comment's ID.
func digestRestartHeld(t *testing.T, l *discRunLoop) int64 {
	t.Helper()
	f := l.f
	l.quiet("the family")
	hold := f.AddComment(2, "owner", "/hold")
	from := len(f.Requests())
	l.step("the epic's /hold", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:ready",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:ready",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:planned", discRunReact(hold))
	if got := digestRestartGets(f, from); got[3] != 1 || got[4] != 1 {
		t.Errorf("the /hold's pass read the tasks %v times; want #3 and #4 once each, by the /hold's walk alone", got)
	}
	l.quiet("the family held")
	own3, own4 := f.AddComment(3, "owner", "/hold"), f.AddComment(4, "owner", "/hold")
	l.step("the tasks' own /holds", discRunReact(own3), discRunReact(own4))
	m := f.AddComment(3, "mallory", "Wait.")
	l.step("mallory's comment on #3", "POST /issues/1/comments", "PATCH /issues/1", "POST /issues/3/assignees")
	l.quiet("#3 held for discussion")
	if body := l.digest(); !strings.Contains(body, fmt.Sprintf("`mallory` posted comment %d", m)) || !strings.Contains(body, "It's on an owner hold too") {
		t.Fatalf("digest %q; want #3's hold listed, with its owner hold", body)
	}
	return m
}

// #216's acceptance: the epic's /resume, posted while no daemon runs, is
// the restart's first pass's to handle. It writes the "ended" record to
// each task, so each is read again at its turn and visited: #3's hold
// item stays, and the pass writes what the /resume writes on a pass that
// isn't a restart's, with no digest edit, and owner stays assigned. The
// epic, which nothing else wrote to, is read once. The passes after it
// write nothing, and an unchanged one asks only for 304s.
func TestDigestRestartTouchedResume(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	digestRestartHeld(t, l)
	before := l.digest()

	resume := f.AddComment(2, "owner", "/resume")
	l.start()
	from := len(f.Requests())
	l.step("the epic's /resume on a restart's first pass", "POST /issues/3/comments", "POST /issues/4/comments",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:held", discRunReact(resume))
	if body := l.digest(); body != before {
		t.Errorf("digest\n%q\nwant it as before the restart\n%q", body, before)
	}
	if a := assignees(f, 3); !slices.Equal(a, []string{"owner"}) {
		t.Errorf("#3's assignees %q; want owner", a)
	}
	// The epic: the pass's read. Each task: the pass's read, the /resume's
	// two, and the read again at its turn.
	if got := digestRestartGets(f, from); got[2] != 1 || got[3] != 4 || got[4] != 4 {
		t.Errorf("the restart's first pass read the issues %v times; want #2 once, #3 and #4 four times each", got)
	}
	for n, want := range map[int][]string{2: {"stage:planned", "type:epic"}, 3: {"stage:held", "type:task"}, 4: {"stage:held", "type:task"}} {
		if got := ivLabels(f, n); !slices.Equal(got, want) {
			t.Errorf("#%d: labels %q; want %q", n, got, want)
		}
	}
	l.quiet("after the restart")
	if body := l.digest(); body != before {
		t.Errorf("digest\n%q\nwant it as before the restart\n%q", body, before)
	}
	l.restart("after the restart")
}

// digestRestartResumes builds, on l, on discRunFamily, digestRestartHeld's
// run, then posts the epic's /resume and #3's own after it, both for the
// restart's first pass. It returns the two /resumes' IDs.
func digestRestartResumes(t *testing.T, l *discRunLoop) (resume, r3 int64) {
	t.Helper()
	digestRestartHeld(t, l)
	return l.f.AddComment(2, "owner", "/resume"), l.f.AddComment(3, "owner", "/resume")
}

// The epic's /resume and #3's own after it, both for a restart's first
// pass: #3, read again at its turn after the epic's record, is visited on
// that snapshot, so its /resume is handled in the same pass, as the next
// would handle it, which takes it back to ready. The digest is written
// once, with nothing waiting. #3 is still left out of Work and assignment
// on that pass, as any issue a command wrote to is, so owner is
// unassigned on the next, which reads it again. The passes after that
// write nothing.
func TestDigestRestartTouchedOwnResume(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	entry := discRunFind(t, f, 3, "issuedog:transition")
	resume, r3 := digestRestartResumes(t, l)
	l.start()
	l.step("both /resumes on a restart's first pass", "POST /issues/3/comments", "POST /issues/4/comments",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:held", discRunReact(resume),
		"POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:held", discRunReact(r3),
		"PATCH /issues/1")
	if got := l.added(3); len(got) != 2 || got[1] != moved("Resumed by `/resume`, back to `ready`.", fmt.Sprintf("from=held to=ready cmd=%d seen=%d", r3, entry)) {
		t.Errorf("#3's new comments %q; want the record, then the move back to ready", got)
	}
	if l.digest() != digestList {
		t.Errorf("digest %q; want nothing listed", l.digest())
	}
	l.step("the restart's second pass", "DELETE /issues/3/assignees")
	if l.digest() != digestList || len(assignees(f, 3)) != 0 {
		t.Errorf("digest %q, #3's assignees %q; want nothing listed or assigned", l.digest(), assignees(f, 3))
	}
	l.quiet("#3 resumed")
	l.restart("#3 resumed")
}

// Not only a restart forgets an issue's items: a close, as the poll lists
// it, does too (Daemon.leave). Mallory's close of #3, while it's listed,
// and the epic's /resume, in one pass that isn't a restart's: the close
// is reverted before the visits, and the /resume writes its record to #3
// before #3's turn, so #3 is read again then, and visited. The digest is
// written once, with #3's hold and its new trigger, the close, and the
// pass after writes nothing.
func TestDigestRestartTouchedReopened(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	m := digestRestartHeld(t, l)
	f.CloseBy(3, "mallory")
	resume := f.AddComment(2, "owner", "/resume")
	l.step("mallory's close and the epic's /resume", "PATCH /issues/3", "POST /issues/3/comments", "POST /issues/4/comments",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:held", discRunReact(resume),
		"POST /issues/3/comments", "PATCH /issues/1")
	notice := discRunFind(t, f, 3, "closed this issue")
	if body := l.digest(); !strings.Contains(body, fmt.Sprintf("`mallory` posted comment %d", m)) ||
		!strings.Contains(body, fmt.Sprintf("`mallory` closed this issue, which I reopened. A code owner's 👍 on its notice accepts it. See https://github.com/o/r/issues/3#issuecomment-%d", notice)) {
		t.Errorf("digest %q; want #3's hold listed, with mallory's comment and her close", body)
	}
	l.quiet("after the close")
}

// Only the visit that failed holds back what it wrote to. Mallory's
// comment holds #4 for discussion too, so the digest lists both tasks.
// On a restart's first pass, the epic's /resume writes to both, and #3's
// visit, read again, fails at its own /resume's first write. #4, which
// only the epic's visit, which worked, wrote to, is still read again and
// visited, so its item stays. #3's goes, as a failed visit after a restart
// has none to keep (#360): the digest is written without it, and the next
// pass, which handles #3's /resume, takes it back to ready.
func TestDigestRestartTouchedFailedVisit(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	digestRestartHeld(t, l)
	m4 := f.AddComment(4, "mallory", "Wait.")
	l.step("mallory's comment on #4", "POST /issues/1/comments", "PATCH /issues/1", "POST /issues/4/assignees")
	l.quiet("both tasks held for discussion")
	resume, r3 := f.AddComment(2, "owner", "/resume"), f.AddComment(3, "owner", "/resume")
	l.start()
	f.FailWrite(len(f.Writes()) + 7) // #3's /resume's first, after the epic's six
	l.fail("both /resumes on a restart's first pass, #3's failing", []string{"step 1 on o/r#3"}, "POST /issues/3/comments", "POST /issues/4/comments",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:held", discRunReact(resume),
		"POST /issues/3/comments", "PATCH /issues/1")
	if body := l.digest(); !strings.Contains(body, "1 item waits on you.") || !strings.Contains(body, fmt.Sprintf("`mallory` posted comment %d", m4)) {
		t.Errorf("digest %q; want #4's hold alone listed", body)
	}
	f.FailWrite(0)
	l.step("the restart's second pass", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:held", discRunReact(r3),
		"DELETE /issues/3/assignees")
	l.quiet("#3 resumed")
}

// digestRestartTransitions is the bodies of issue n's App comments that
// carry a transition marker whose fields hold fields.
func digestRestartTransitions(f *githubtest.Fake, n int, fields string) []string {
	var got []string
	for _, c := range f.State().Issues[n-1].Comments {
		if c.Author == app && strings.Contains(c.Body, "<!-- issuedog:transition ") && strings.Contains(c.Body, fields) {
			got = append(got, c.Body)
		}
	}
	return got
}

// The epic's /hold, then #3's own, both for a restart's first pass. The
// epic's holds #3, before #3's turn, so #3 is read again then, and its
// own /hold is handled on that snapshot, which shows it held: it writes no
// marker, only its 👍 (6.4). Visited on the snapshot from before the
// epic's /hold, #3 would look at ready, and get a second hold marker.
func TestDigestRestartTouchedHolds(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	l.quiet("the family")
	hold, own := f.AddComment(2, "owner", "/hold"), f.AddComment(3, "owner", "/hold")
	l.start()
	l.step("both /holds on a restart's first pass", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:ready",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:ready",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:planned", discRunReact(hold), discRunReact(own))
	if got := digestRestartTransitions(f, 3, "to=held"); len(got) != 1 || !strings.Contains(got[0], fmt.Sprintf("cmd=%d ", hold)) {
		t.Errorf("#3's hold markers %q; want the epic's /hold's alone", got)
	}
	if got := ivLabels(f, 3); !slices.Equal(got, []string{"stage:held", "type:task"}) {
		t.Errorf("#3: labels %q; want it held", got)
	}
	l.quiet("both held")
	l.restart("both held")
}

// The epic held by its /hold, with mallory's comment holding #4 for
// discussion too, so the digest lists #4's hold, on the epic's owner hold
// too. Then the owner's /resume on the epic, and a later /hold on #3, both
// for a restart's first pass. The /resume takes the epic and #3 back to
// their stages, and ends its hold on #4, which stays held for discussion
// alone, so its item no longer names an owner hold. #3, read again at its
// turn, at ready, is held again by its own /hold, with one marker for it.
// Visited on the snapshot from before the /resume, #3 would look held, and
// its /hold would write no marker, so the /resume would leave it at ready.
func TestDigestRestartTouchedResumeThenHold(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	l.quiet("the family")
	hold := f.AddComment(2, "owner", "/hold")
	l.step("the epic's /hold", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:ready",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:ready",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:planned", discRunReact(hold))
	m := f.AddComment(4, "mallory", "Wait.")
	l.step("mallory's comment on #4", "POST /issues/1/comments", "PATCH /issues/1", "POST /issues/4/assignees")
	if body := l.digest(); !strings.Contains(body, fmt.Sprintf("`mallory` posted comment %d", m)) || !strings.Contains(body, "It's on an owner hold too") {
		t.Fatalf("digest %q; want #4's hold listed, on the owner hold too", body)
	}
	l.quiet("#4 held for discussion")
	resume, own := f.AddComment(2, "owner", "/resume"), f.AddComment(3, "owner", "/hold")
	l.start()
	l.step("the /resume and #3's /hold on a restart's first pass", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:held",
		"POST /issues/4/comments", "POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:held", discRunReact(resume),
		"POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:ready", discRunReact(own), "PATCH /issues/1")
	if got := digestRestartTransitions(f, 3, fmt.Sprintf("cmd=%d ", own)); len(got) != 1 || !strings.Contains(got[0], "from=ready to=held ") {
		t.Errorf("#3's markers for its own /hold %q; want one hold from ready", got)
	}
	for n, want := range map[int][]string{2: {"stage:planned", "type:epic"}, 3: {"stage:held", "type:task"}, 4: {"stage:held", "type:task"}} {
		if got := ivLabels(f, n); !slices.Equal(got, want) {
			t.Errorf("#%d: labels %q; want %q", n, got, want)
		}
	}
	if body := l.digest(); !strings.Contains(body, "1 item waits on you.") || !strings.Contains(body, fmt.Sprintf("`mallory` posted comment %d", m)) ||
		strings.Contains(body, "owner hold too") {
		t.Errorf("digest %q; want #4's hold alone listed, with no owner hold", body)
	}
	l.quiet("#3 held by its own /hold")
	l.restart("#3 held by its own /hold")
}

// An issue read again whose visit then fails is still one whose items
// aren't cached. The epic's /resume, and #3's own /hold after it, which
// #3, held already, answers with a 👍 alone, for a restart's first pass:
// #3's visit, read again, fails at that 👍, so the digest drops #3's item
// (#360). On the next pass, the epic's new /hold writes to #3 before its
// turn again, so #3 is read again again, and visited: its 👍 is posted,
// and its item is back.
func TestDigestRestartTouchedFailedThenTouched(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	m := digestRestartHeld(t, l)
	resume, own := f.AddComment(2, "owner", "/resume"), f.AddComment(3, "owner", "/hold")
	l.start()
	f.FailWrite(len(f.Writes()) + 7) // #3's 👍, after the epic's six
	l.fail("the /resume and #3's /hold on a restart's first pass, #3's 👍 failing", []string{"step 1 on o/r#3"},
		"POST /issues/3/comments", "POST /issues/4/comments", "POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:held",
		discRunReact(resume), discRunReact(own), "PATCH /issues/1")
	f.FailWrite(0)
	hold := f.AddComment(2, "owner", "/hold")
	l.step("the epic's /hold, and #3 read again", "POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:planned",
		discRunReact(hold), discRunReact(own), "PATCH /issues/1")
	if body := l.digest(); !strings.Contains(body, fmt.Sprintf("`mallory` posted comment %d", m)) {
		t.Errorf("digest %q; want #3's hold listed again", body)
	}
	l.quiet("all held")
}

// A visit that waits for its next pass after its command wrote to an issue
// holds the issue back too. The owner's label on #3, whose event GitHub
// hasn't written yet, and the epic's /resume, for a restart's first pass:
// the /resume's walk reaches #3, and checks its labels, which are held
// back (ErrLabelsHeld), so the epic's visit waits, and #3 isn't read again.
// Once the event is written, the next pass handles the /resume.
func TestDigestRestartTouchedLabelsHeld(t *testing.T) {
	t.Parallel()
	f := discRunFamily(t)
	l := newDiscRunLoop(t, f)
	l.quiet("the family")
	hold := f.AddComment(2, "owner", "/hold")
	l.step("the epic's /hold", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:ready",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:ready",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:planned", discRunReact(hold))
	l.quiet("the family held")
	f.LagEvents(5 * time.Second)
	f.AddLabel(3, "owner", "priority:p1")
	f.LagEvents(0)
	resume := f.AddComment(2, "owner", "/resume")
	l.start()
	from := len(f.Requests())
	l.step("the /resume on a restart's first pass")
	// #3: the pass's read and the /resume's three, and no read again.
	if got := digestRestartGets(f, from); got[3] != 4 {
		t.Errorf("the restart's first pass read the issues %v times; want #3 four times", got)
	}
	f.Advance(10 * time.Second)
	l.step("the /resume, once the event is written", "POST /issues/3/comments", "POST /issues/3/labels", "DELETE /issues/3/labels/stage:held",
		"POST /issues/4/comments", "POST /issues/4/labels", "DELETE /issues/4/labels/stage:held",
		"POST /issues/2/comments", "POST /issues/2/labels", "DELETE /issues/2/labels/stage:held", discRunReact(resume))
	l.quiet("resumed")
}

// A crash at any write of TestDigestRestartTouchedOwnResume's restart's
// first pass replays safely: each replay is a restart, so its first pass
// reads #3 again at its turn, and handles its /resume there, unless the
// crash failed the epic's visit after its /resume wrote to #3, which then
// waits for the next pass, as the epic's /resume does.
func TestDigestRestartTouchedReplay(t *testing.T) {
	t.Parallel()
	discRunReplay(t, discRunFamily, nil, func(l *discRunLoop) { digestRestartResumes(t, l) })
}
