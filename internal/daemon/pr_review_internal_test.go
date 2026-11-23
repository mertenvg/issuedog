package daemon

import (
	"maps"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/issuedog/internal/state"
)

// The window of a task's PR's reviews is its PR's window (prWindow), or,
// when the task's latest move into pr-open ends a round that a review, a
// new head, or main or a story branch's tip moving sent it on from
// pr-open, since that move, so a
// review posted while the task was away on that round counts once it's
// back (SPEC §7.2). The verify loop's own moves and a hold in the round
// keep it; a move that starts a round of its own, such as the pick's
// after a hand-off's /done, or a send-back by hand, doesn't. A hold at
// pr-open after the round keeps the window it had, and a trusted reopen's
// move back from a hand-off begins it there.
func TestPRReviewWindow(t *testing.T) {
	t.Parallel()
	const (
		toPROpen   = "<!-- issuedog:transition from=verifying to=pr-open run=rv head=abc pr=2 -->"
		toPROpen2  = "<!-- issuedog:transition from=verifying to=pr-open run=rw head=def pr=2 -->"
		reviewBack = "<!-- issuedog:transition from=pr-open to=in-progress event=r5 seen=101 -->"
		newHead    = "<!-- issuedog:transition from=pr-open to=verifying event=nabc head=abc seen=101 -->"
		mainBack   = "<!-- issuedog:transition from=pr-open to=in-progress event=m1 -->"
		tipBack    = "<!-- issuedog:transition from=pr-open to=in-progress event=b1 -->"
		byHand     = "<!-- issuedog:transition from=pr-open to=in-progress by=owner event=77 -->"
		committed  = "<!-- issuedog:transition from=in-progress to=verifying attempt=1 run=r1 head=def -->"
		findings   = "<!-- issuedog:transition from=verifying to=in-progress attempt=1 event=v103 head=abc -->"
		hold       = "<!-- issuedog:transition from=in-progress to=held at=in-progress reason=owner cmd=900 -->"
		resume     = "<!-- issuedog:transition from=held to=in-progress cmd=901 seen=101 -->"
		holdOpen   = "<!-- issuedog:transition from=pr-open to=held at=pr-open reason=owner cmd=902 -->"
		resumeOpen = "<!-- issuedog:transition from=held to=pr-open cmd=903 seen=101 -->"
		capped     = "<!-- issuedog:transition from=in-progress to=needs-human event=ain-progress-1 -->"
		done       = "<!-- issuedog:transition from=needs-human to=ready cmd=904 seen=904 -->"
		pick       = "<!-- issuedog:transition from=ready to=in-progress event=s106 seen=106 -->"
		handoff    = "<!-- issuedog:transition from=pr-open to=needs-human event=c2-1 -->"
		reopened   = "<!-- issuedog:transition from=needs-human to=pr-open event=18734412 pr=2 -->"
	)
	for _, tt := range []struct {
		name     string
		comments []string
		want     int // the comment the window begins at, by index, or -1 for none
	}{
		{"none", []string{"<!-- issuedog:transition from=ready to=in-progress event=s1 -->"}, -1},
		{"the PR step's move alone", []string{toPROpen}, 0},
		{"a review's round", []string{toPROpen, reviewBack, committed, toPROpen2}, 1},
		{"a new head's round, through findings", []string{toPROpen, newHead, findings, committed, toPROpen2}, 1},
		{"main moving's round", []string{toPROpen, mainBack, committed, toPROpen2}, 1},
		{"a story branch's tip moving's round", []string{toPROpen, tipBack, committed, toPROpen2}, 1},
		{"a round through a hold", []string{toPROpen, reviewBack, hold, resume, committed, toPROpen2}, 1},
		{"a round started over", []string{toPROpen, reviewBack, capped, done, pick, committed, toPROpen2}, 6},
		{"a send-back by hand", []string{toPROpen, byHand, committed, toPROpen2}, 3},
		{"a hold at pr-open after the round", []string{toPROpen, reviewBack, committed, toPROpen2, holdOpen, resumeOpen}, 1},
		{"a trusted reopen after the round", []string{toPROpen, reviewBack, committed, toPROpen2, handoff, reopened}, 5},
		{"away on the round still", []string{toPROpen, reviewBack, committed}, 0},
	} {
		s := waitSnap(1, []string{"stage:pr-open"})
		s.Type = "task"
		for i, c := range tt.comments {
			s.Comments = append(s.Comments, github.Comment{ID: int64(101 + i), Author: waitApp, Body: c, CreatedAt: waitT0.Add(time.Duration(i+1) * time.Minute)})
		}
		want := time.Time{}
		if tt.want >= 0 {
			want = s.Comments[tt.want].CreatedAt
		}
		if got := prReviewWindow(s, waitSlug, Dirt{}.Has); !got.Equal(want) {
			t.Errorf("%s: the reviews' window begins at %s, want %s", tt.name, got, want)
		}
	}
}

// A review's move is pr-open → in-progress keyed event=r<review ID>, and
// the largest ID such a key names marks that review and every older one
// handled; no other key or move names one (SPEC §5).
func TestPRReviewsHandled(t *testing.T) {
	t.Parallel()
	for marker, want := range map[string]int64{ // reviewMove's review ID, or 0 for none
		"<!-- issuedog:transition from=pr-open to=in-progress event=r42 seen=9 -->":   42,
		"<!-- issuedog:transition from=pr-open to=in-progress event=r -->":            0,
		"<!-- issuedog:transition from=pr-open to=in-progress event=rx1 -->":          0,
		"<!-- issuedog:transition from=pr-open to=in-progress event=m42 -->":          0,
		"<!-- issuedog:transition from=pr-open to=in-progress by=owner event=42 -->":  0,
		"<!-- issuedog:transition from=pr-open to=verifying event=r42 -->":            0,
		"<!-- issuedog:transition from=held to=in-progress event=r42 -->":             0,
		"<!-- issuedog:transition from=pr-open to=in-progress event=r0 -->":           0,
		"<!-- issuedog:revert from=pr-open to=in-progress event=r42 -->":              0,
		"<!-- issuedog:transition from=pr-open to=in-progress event=r42 head=abc -->": 42,
	} {
		m := github.ParseMarkers(marker)[0]
		got := int64(0)
		if reviewMove(m) {
			got = prReviewID(m)
		}
		if got != want {
			t.Errorf("reviewMove and prReviewID(%s) = %d, want %d", marker, got, want)
		}
	}
	marker := func(fields map[string]string) github.Marker { return github.Marker{Kind: "transition", Fields: fields} }
	is := state.Issue{Markers: []github.Marker{
		marker(map[string]string{"from": "pr-open", "to": "in-progress", "event": "r42"}),
		marker(map[string]string{"from": "in-progress", "to": "verifying", "run": "r1"}),
		marker(map[string]string{"from": "pr-open", "to": "in-progress", "event": "r7"}),
		{Kind: "verify", Fields: map[string]string{"event": "r99"}},
	}}
	if got := prReviewsHandled(is); got != 42 {
		t.Errorf("prReviewsHandled = %d; want 42, the largest a move names", got)
	}
	if got := prReviewsHandled(state.Issue{}); got != 0 {
		t.Errorf("prReviewsHandled with no move = %d; want 0", got)
	}
}

// A review's move carries the branch's tip as head= only when it's a head
// other than the PR's verified head, and drops one found before otherwise
// (SPEC §3.2).
func TestPRReviewHead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		fields   map[string]string
		old, tip string
		want     map[string]string
	}{
		{"a new head", map[string]string{"seen": "9"}, "aaa", "bbb", map[string]string{"seen": "9", "head": "bbb"}},
		{"the verified head", map[string]string{"seen": "9", "head": "bbb"}, "aaa", "aaa", map[string]string{"seen": "9"}},
		{"a branch that's gone", map[string]string{"head": "bbb"}, "aaa", "", map[string]string{}},
		{"none verified", nil, "", "bbb", map[string]string{"head": "bbb"}},
	} {
		if got := prReviewHead(tt.fields, tt.old, tt.tip); !maps.Equal(got, tt.want) {
			t.Errorf("%s: prReviewHead = %v; want %v", tt.name, got, tt.want)
		}
	}
}

// input.md's reviews are headed by the one reviewer the move names, a
// valid login in code, or else as the reviews (SPEC §12).
func TestReviewsLabel(t *testing.T) {
	t.Parallel()
	for reviewer, want := range map[string]string{
		"owner":           "the review by `owner`",
		"merten_acme":     "the review by `merten_acme`",
		"":                "the reviews",
		"not`a-login":     "the reviews",
		"-not-a-login-":   "the reviews",
		"dependabot[bot]": "the review by `dependabot[bot]`",
	} {
		m := github.Marker{Kind: "transition", Fields: map[string]string{"from": "pr-open", "to": "in-progress", "event": "r5"}}
		if reviewer != "" {
			m.Fields[fieldReviewer] = reviewer
		}
		if got := reviewsLabel(m); got != want {
			t.Errorf("reviewsLabel with reviewer %q = %q; want %q", reviewer, got, want)
		}
	}
}

// reviewLink is review id's link on PR #2.
func reviewLink(id string) string { return prReconcileLink + "#pullrequestreview-" + id }

// The review's move's comment says the reviews send the task back, then
// quotes each, oldest first, under a heading naming its author, in code,
// defused, and its state, linking it: its text, fenced, or that it has
// none, then each line comment, on its file, in a code span, and line, if
// any, fenced (SPEC §5.1, §7.2). Each text is cut to its first 1,000
// characters, with a line saying so and linking the review, and its issue
// references are defused, so a fence can't be closed from inside, nor an
// issue closed or a person mentioned.
func TestRenderReviews(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 999) + "yz"
	quotes := []reviewQuote{
		{prReview: prReview{ID: 5, Author: github.Author{Login: "owner"}, State: github.ReviewChangesRequested, Body: "Fixes #3, as @eve said.\n\n```\nnot closed\n"},
			Comments: []github.ReviewComment{{Path: "a.go", Line: 2, Body: "Here."}, {Path: "b `x`.go", Body: long}}},
		{prReview: prReview{ID: 7, Author: github.Author{Login: "o@wner"}, State: github.ReviewCommented},
			Comments: []github.ReviewComment{{Path: "c.go", Line: 9, Body: "And see owner/repo#12."}}},
	}
	want := "2 reviews of PR #2 send this task back to `in-progress`, for the implementer to answer them, oldest first. " +
		"The PR waits until the task's next head is verified.\n\n" +
		"### `owner` asked for changes\n\nIn [review 5](" + reviewLink("5") + "):\n\n" +
		"````text\nFixes #​3, as @eve said.\n\n```\nnot closed\n````\n\n" +
		"On `a.go`, line 2:\n\n```text\nHere.\n```\n\n" +
		"On ``b `x`.go``:\n\n```text\n" + strings.Repeat("x", 999) + "y…\n```\n\n" +
		"This is its first 1,000 characters: [the review](" + reviewLink("5") + ") has the whole.\n\n" +
		"### `o@​wner` commented\n\n[Review 7](" + reviewLink("7") + ") has no text of its own, only its line comments.\n\n" +
		"On `c.go`, line 9:\n\n```text\nAnd see owner/repo#​12.\n```"
	if got := renderReviews(prReconcileLink, 2, quotes); got != want {
		t.Errorf("the reviews' comment =\n%s\nwant\n%s", got, want)
	}
	one := renderReviews(prReconcileLink, 2, quotes[1:])
	if want := "A review of PR #2 sends this task back to `in-progress`, for the implementer to answer it. " +
		"The PR waits until the task's next head is verified.\n\n### `o@​wner` commented"; !strings.HasPrefix(one, want) {
		t.Errorf("one review's comment =\n%s\nwant it to start\n%s", one, want)
	}
}

// A review with more line comments than GitHub's comment limit, less
// textRoom, can hold keeps the oldest that fit, then says how many more
// didn't, and links the PR, which has them all.
func TestRenderReviewsFit(t *testing.T) {
	t.Parallel()
	var comments []github.ReviewComment
	for range 100 {
		comments = append(comments, github.ReviewComment{Path: "a.go", Line: 1, Body: strings.Repeat("x", 1000)})
	}
	quotes := []reviewQuote{
		{prReview: prReview{ID: 5, Author: github.Author{Login: "owner"}, State: github.ReviewCommented, Body: "Many."}, Comments: comments},
		{prReview: prReview{ID: 6, Author: github.Author{Login: "owner"}, State: github.ReviewCommented, Body: "And more."},
			Comments: comments[:2]},
	}
	got := renderReviews(prReconcileLink, 2, quotes)
	if n := textLen(got); n > maxText-textRoom {
		t.Errorf("the comment is %d characters; want at most %d", n, maxText-textRoom)
	}
	kept := strings.Count(got, "On `a.go`, line 1:")
	want := "The rest didn't fit here: 1 more review and " + strconv.Itoa(102-kept) + " more line comments. [PR #2](" + prReconcileLink + ") has them all."
	if kept < 50 || kept >= 100 || !strings.HasSuffix(got, want) || strings.Contains(got, "And more.") {
		t.Errorf("the comment kept %d line comments, and ends\n%s\nwant most of the first review's, and to end\n%s", kept, got[len(got)-200:], want)
	}
}

// A move that waited for the worker is made to fit task 1's branch as the
// bare clone has it when it's made, at no request: a new head's stands
// only while the branch is at the head its key names, and is dropped
// otherwise, or with two branches, so the PR is judged again; a review's
// takes the branch's tip as head= when it isn't the PR's verified head,
// and none when it is, and with two branches is left as it was found.
func TestPRWaitingTip(t *testing.T) {
	t.Parallel()
	one, tip := branchRemote(t, "issuedog/1-refuse-an-expired-quote")
	two, _ := branchRemote(t, "issuedog/1-refuse-an-expired-quote", "issuedog/1-another")
	other := strings.Repeat("b", 40)
	verified := func(head string) state.Issue {
		return state.Issue{Markers: []github.Marker{{Kind: "transition",
			Fields: map[string]string{"from": "verifying", "to": "pr-open", "run": "rv", "head": head, "pr": "2"}}}}
	}
	newHead := func(at string) Move {
		return Move{To: state.Verifying, Trigger: state.TriggerNewHead, Key: newHeadKey(at), Fields: map[string]string{"head": at, "seen": "9"}}
	}
	review := Move{To: state.InProgress, Trigger: state.TriggerReviewChanges, Key: prReviewKey(5), Fields: map[string]string{"seen": "9", "head": other}}
	for _, tt := range []struct {
		name       string
		clone      *repo.Repo
		is         state.Issue
		m          Move
		stands     bool
		wantFields map[string]string
	}{
		{"a new head still at the tip", one, verified(other), newHead(tip), true, map[string]string{"head": tip, "seen": "9"}},
		{"a new head the branch moved on from", one, verified(other), newHead(other), false, nil},
		{"a new head with two branches", two, verified(other), newHead(tip), false, nil},
		{"a review, the branch at a new head", one, verified(strings.Repeat("a", 40)), review, true, map[string]string{"head": tip, "seen": "9"}},
		{"a review, the branch at the verified head", one, verified(tip), review, true, map[string]string{"seen": "9"}},
		{"a review with two branches", two, verified(tip), review, true, map[string]string{"head": other, "seen": "9"}},
	} {
		d := prHeadDaemon()
		d.Clone = tt.clone
		d.prWaiting = map[int]Move{1: tt.m}
		s := &github.Snapshot{Number: 1, Title: "Refuse an expired quote"}
		got, ok, err := d.prWaitingTip(t.Context(), s, tt.is, tt.m)
		if err != nil {
			t.Fatalf("%s: prWaitingTip: %v", tt.name, err)
		}
		if ok != tt.stands || ok && !maps.Equal(got.Fields, tt.wantFields) {
			t.Errorf("%s: prWaitingTip = %v, %t; want %v, %t", tt.name, got.Fields, ok, tt.wantFields, tt.stands)
		}
		if _, kept := d.prWaiting[1]; kept != tt.stands {
			t.Errorf("%s: the waiting move kept %t; want %t", tt.name, kept, tt.stands)
		}
	}
	if review.Fields["head"] != other {
		t.Errorf("the review's move found was changed to %v; want it as found", review.Fields)
	}
}

// A review a move named, or an older one, never counts again, even when
// its window holds it, as when GitHub's clock puts its submitted_at at or
// after the move's comment: with only such reviews, trusted ones of states
// that count, nothing moves, and nothing is read (SPEC §5.2, §7.2).
func TestPRReviewMoveHandled(t *testing.T) {
	t.Parallel()
	d := prHeadDaemon()
	d.prTrusted = map[int64]bool{5: true, 7: true}
	is := state.Issue{Stage: state.PROpen, Markers: []github.Marker{{Kind: "transition",
		Fields: map[string]string{"from": "pr-open", "to": "in-progress", "event": "r7"}}}}
	owner := github.Author{Login: "owner", Type: "User"}
	reviews := []prReview{{ID: 7, Author: owner, State: github.ReviewChangesRequested}, {ID: 5, Author: owner, State: github.ReviewCommented}}
	m, err := d.prReviewMove(t.Context(), &github.Snapshot{Number: 1}, is, listedPR{Number: 2, Open: true}, reviews)
	if m != nil || err != nil {
		t.Errorf("prReviewMove on handled reviews = %+v, %v; want no move", m, err)
	}
}

// Step 2's judge of a task at pr-open makes a move that waited for the
// worker fit the branch as it is when it's made, with no read of the PR:
// a new head's whose branch moved on is dropped, and the new tip judged
// in its place, and a review's carries the tip as head=.
func TestPRJudgeOpenHeadWaitingTip(t *testing.T) {
	t.Parallel()
	clone, tip := branchRemote(t, "issuedog/1-refuse-an-expired-quote")
	old, other := strings.Repeat("a", 40), strings.Repeat("b", 40)
	is := state.Issue{Stage: state.PROpen, TransitionComment: 10, Markers: []github.Marker{{Kind: "transition",
		Fields: map[string]string{"from": "verifying", "to": "pr-open", "run": "rv", "head": old, "pr": "2"}}}}
	s := &github.Snapshot{Number: 1, Title: "Refuse an expired quote"}
	for _, tt := range []struct {
		name string
		m    Move
		want state.Key
	}{
		{"a new head the branch moved on from", Move{To: state.Verifying, Trigger: state.TriggerNewHead, Key: newHeadKey(other),
			Fields: map[string]string{"head": other}}, newHeadKey(tip)},
		{"a review", Move{To: state.InProgress, Trigger: state.TriggerReviewChanges, Key: prReviewKey(5),
			Fields: map[string]string{"seen": "9"}}, prReviewKey(5)},
	} {
		d := prHeadDaemon(s)
		d.Clone = clone
		d.prJudged = map[int]int64{1: 10}
		d.prWait(1, tt.m, "#3 holds it")
		got, err := d.prJudgeOpenHead(t.Context(), s, Dirt{}, is, listedPR{Number: 2, Open: true})
		if err != nil {
			t.Fatalf("%s: prJudgeOpenHead: %v", tt.name, err)
		}
		if got == nil || got.Key != tt.want || got.Fields["head"] != tip {
			t.Errorf("%s: prJudgeOpenHead = %+v; want the move keyed %v, with head=%s", tt.name, got, tt.want, tip)
		}
	}
}
