package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is step 2's reviews (SPEC §7.2, §8.1): a trusted user's
// "comment" or "request changes" review on a task's PR to main, at
// pr-open, sends the task back to in-progress, its next run reading the
// review. An approving review never does, with comments or without (#234's
// Q1.5), nor a dismissed one, nor an untrusted user's: they write nothing
// and reach no agent, as a task takes no untrusted input (SPEC §6.2). A
// review is seen at no request of its own: the timeline read step 2 makes
// of a PR the poll lists as changed holds it (prEvents). Only the line
// comments of the reviews that send the task back are read, on the pass
// that moves it.

// prReview is one review on a task's PR, as its `reviewed` timeline event
// gives it: its ID, its author, the reviewer, its state, lower case (the
// github.Review* states), its text and when it was submitted.
type prReview struct {
	ID     int64
	Author github.Author
	State  string
	Body   string
	At     time.Time
}

// prReviewOf is the review e, a `reviewed` timeline event, gives: the
// reviewer is its user, not an actor, and its time its submitted_at.
func prReviewOf(e *gogithub.Timeline) prReview {
	return prReview{ID: e.GetID(), Author: github.Author{Login: e.GetUser().GetLogin(), Type: e.GetUser().GetType()},
		State: e.GetState(), Body: e.GetBody(), At: e.GetSubmittedAt().Time}
}

// reviewQuote is a review that sends a task back, with its line comments,
// oldest first, for the move's comment (renderReviews).
type reviewQuote struct {
	prReview
	Comments []github.ReviewComment
}

// fieldReviewer is the marker field reviewer=<login> on a review's move,
// pr-open → in-progress, that quotes one review: its author, so the next
// run's input.md names it (reviewsLabel, SPEC §5).
const fieldReviewer = "reviewer"

// prReviewKey keys a task's move pr-open → in-progress on its PR's
// reviews by the newest of them, review id (SPEC §5): event=r<id>.
func prReviewKey(id int64) state.Key {
	return state.Key{Field: "event", ID: "r" + strconv.FormatInt(id, 10)}
}

// reviewMove reports whether m is a move pr-open → in-progress on a
// review (prReviewMove), keyed event=r<review ID>.
func reviewMove(m github.Marker) bool {
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.PROpen && state.Stage(m.Fields["to"]) == state.InProgress &&
		prReviewID(m) > 0
}

// prReviewID is the review ID m's event=r<ID> key names, or 0 for none.
func prReviewID(m github.Marker) int64 {
	ev := m.Fields["event"]
	if !strings.HasPrefix(ev, "r") {
		return 0
	}
	id, err := strconv.ParseInt(ev[1:], 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}

// prReviewsHandled is the largest review ID that an event=r<ID> key of
// task is's markers names, or 0 for none. Review IDs only grow, so a move
// that names a review has handled it and every older one: none counts
// again, after a replay or a restart too (SPEC §5.2).
func prReviewsHandled(is state.Issue) int64 {
	var handled int64
	for _, m := range is.Markers {
		if m.Kind == "transition" {
			handled = max(handled, prReviewID(m))
		}
	}
	return handled
}

// prSendBack reports whether m is a move out of pr-open that sends a task
// on a round, back to in-progress or verifying, on its PR: a review's,
// keyed event=r<ID>, a new head's, event=n<SHA>, or main's moving,
// event=m<SHA> (SPEC §5). A move by a stage label, keyed by its timeline
// event, a number, isn't one.
func prSendBack(m github.Marker) bool {
	to, ev := state.Stage(m.Fields["to"]), m.Fields["event"]
	return m.Kind == "transition" && state.Stage(m.Fields["from"]) == state.PROpen && (to == state.InProgress || to == state.Verifying) &&
		len(ev) > 1 && strings.ContainsRune("rnm", rune(ev[0]))
}

// prReviewWindow is when the window of the reviews of task s's PR begins,
// as discMarkers reads its markers with dirty (SPEC §7.2): its PR's window
// (prWindow), or, when the task's latest move into pr-open ends a round
// that a move out of pr-open sent it on (prSendBack), the time of that
// move, if earlier, so a review posted while the task was away on that
// round counts once it's back. A round here is as SPEC §8.3 has it: it
// starts at a move other than the verify loop's own (verifyLoopMove), but
// a hold is looked through, as are a pr-open → pr-open move and the
// interviewer's move back to pr-open after a discussion hold from there,
// as prWindow looks through them. A review older than one a move named is
// handled, whatever the window (prReviewsHandled).
func prReviewWindow(s *github.Snapshot, appSlug string, dirty func(int64) bool) time.Time {
	win := prWindow(s, appSlug, dirty)
	var from, start time.Time
	sentBack := false
	discMarkers(s, appSlug, dirty, func(_ int64, at time.Time, m github.Marker) {
		if m.Kind != "transition" {
			return
		}
		f, to := state.Stage(m.Fields["from"]), state.Stage(m.Fields["to"])
		switch {
		case to == state.Held, f == state.Held, f == state.PROpen && to == state.PROpen, verifyLoopMove(m):
			return
		case f == state.Clarifying && to == state.PROpen && state.Stage(m.Fields["origin"]) == state.PROpen:
			return
		case to == state.PROpen:
			from = time.Time{}
			if sentBack {
				from = start
			}
			sentBack = false
			return
		}
		start, sentBack = at, prSendBack(m)
	})
	if !from.IsZero() && from.Before(win) {
		return from
	}
	return win
}

// prReviewCounts reports whether review r, of a task's PR, has a state
// that sends the task back (SPEC §7.2): commented or changes_requested. An
// approved one never does, with comments or without, nor a dismissed one.
func prReviewCounts(r prReview) bool {
	return r.State == github.ReviewCommented || r.State == github.ReviewChangesRequested
}

// prReviewMove judges reviews, those in the window of p, the open PR of
// task s at pr-open, whose state is is, from step 2's read of its
// timeline (prEvents), and returns the move they make, or nil (SPEC
// §7.2's pr-open → in-progress). A review counts when no move has handled
// it (prReviewsHandled), its state counts (prReviewCounts), and its author
// is trusted, asked once per review while the daemon runs
// (prReopenTrusted). Each counted review's line comments are read, one
// request each, only then: so an approval, or any review that counts not,
// costs no request of its own. The move is pr-open → in-progress on
// review-changes, keyed event=r<the newest counted review's ID>, with
// seen=, the newest comment read, as the pick's (SPEC §5), head=, the
// task's branch's tip, when it isn't the PR's verified head
// (prReviewHead), so a new head in the same pass goes with the review,
// and reviewer=, the author, when it counts one review. Its comment
// quotes each counted review, oldest first (renderReviews). A failed
// question or read is an error.
func (d *Daemon) prReviewMove(ctx context.Context, s *github.Snapshot, is state.Issue, p listedPR, reviews []prReview) (*Move, error) {
	a, n := d.Step.Applier, s.Number
	handled := prReviewsHandled(is)
	var counted []prReview
	for _, r := range slices.SortedFunc(slices.Values(reviews), func(x, y prReview) int { return cmp.Compare(x.ID, y.ID) }) {
		if r.ID <= handled {
			continue
		}
		if !prReviewCounts(r) {
			logr.Debugf("%s/%s#%d: review %d of its PR #%d is %s, so it moves nothing", a.Owner, a.Repo, n, r.ID, p.Number, r.State)
			continue
		}
		trusted, err := d.prReopenTrusted(ctx, r.ID, r.Author)
		if err != nil {
			return nil, fmt.Errorf("the reviewer of review %d: %w", r.ID, err)
		}
		if !trusted {
			logr.Debugf("%s/%s#%d: review %d of its PR #%d is by %s, who isn't trusted, so it moves nothing", a.Owner, a.Repo, n, r.ID, p.Number, login(r.Author))
			continue
		}
		counted = append(counted, r)
	}
	if len(counted) == 0 {
		return nil, nil
	}
	quotes := make([]reviewQuote, 0, len(counted))
	for _, r := range counted {
		comments, err := a.Client.ReviewComments(ctx, a.Owner, a.Repo, p.Number, r.ID)
		if err != nil {
			return nil, fmt.Errorf("read review %d's line comments: %w", r.ID, err)
		}
		quotes = append(quotes, reviewQuote{prReview: r, Comments: comments})
	}
	fields := map[string]string{}
	var seen int64
	for _, c := range s.Comments {
		seen = max(seen, c.ID)
	}
	if seen > 0 {
		fields[state.FieldSeen] = strconv.FormatInt(seen, 10)
	}
	_, tip, ok, err := d.prTip(ctx, s)
	if err != nil {
		return nil, fmt.Errorf("read the task's branch: %w", err)
	}
	if ok {
		fields = prReviewHead(fields, prHeadOf(is), tip)
	}
	if who := counted[0].Author.Login; len(counted) == 1 && ivLogin(who) == who {
		fields[fieldReviewer] = who
	}
	newest := counted[len(counted)-1].ID
	logr.Infof("%s/%s#%d: %d review(s) of its PR #%d, the newest %d, send it back to %s", a.Owner, a.Repo, n, len(counted), p.Number, newest, state.InProgress)
	return &Move{To: state.InProgress, Trigger: state.TriggerReviewChanges, Key: prReviewKey(newest), Fields: fields,
		Text: renderReviews(prLink(a.Owner, a.Repo, p.Number), p.Number, quotes)}, nil
}

// prReviewHead is fields, a review's move's, with head= tip, the task's
// branch's tip, when it's a head and not old, the PR's verified head
// (prHeadOf), so the task's head moves there (taskHeadFollow), and with no
// head= otherwise (SPEC §3.2).
func prReviewHead(fields map[string]string, old, tip string) map[string]string {
	if fields == nil {
		fields = map[string]string{}
	}
	if tip != "" && tip != old {
		fields["head"] = tip
	} else {
		delete(fields, "head")
	}
	return fields
}

// reviewRoom is how much of maxText, less textRoom, the review move's
// comment keeps for its last line, which says what didn't fit.
const reviewRoom = 300

// renderReviews is the comment of a task's move pr-open → in-progress on
// quotes, reviews of its PR, pr, at link (SPEC §7.2): it says the reviews
// send the task back, then quotes each, oldest first, under a heading
// naming its author and state, linking it: its text, then each line
// comment with its file and line. The text and each comment is a person's,
// so each is cut to its first findingChars characters, with a line saying
// so and linking the review, its issue references defused (defuseRefs),
// and fenced (ivFenced), as findings are (SPEC §5.1, §10.1); a file's path
// is in a code span (checkCode). The whole stays within GitHub's limit,
// less textRoom: what doesn't fit is left out, oldest kept, and the last
// line says how much, as the reviews on the PR hold it. Transition escapes
// the whole.
func renderReviews(link string, pr int, quotes []reviewQuote) string {
	intro := fmt.Sprintf("A review of PR #%d sends this task back to `in-progress`, for the implementer to answer it. "+
		"The PR waits until the task's next head is verified.", pr)
	if len(quotes) > 1 {
		intro = fmt.Sprintf("%d reviews of PR #%d send this task back to `in-progress`, for the implementer to answer them, oldest first. "+
			"The PR waits until the task's next head is verified.", len(quotes), pr)
	}
	parts := []string{intro}
	size := textLen(intro)
	reviewsLeft, commentsLeft := 0, 0
	add := func(part string) bool {
		if reviewsLeft+commentsLeft > 0 || size+textLen(part)+2 > maxText-textRoom-reviewRoom {
			return false
		}
		parts = append(parts, part)
		size += textLen(part) + 2
		return true
	}
	for _, q := range quotes {
		at := link + "#pullrequestreview-" + strconv.FormatInt(q.ID, 10)
		said := "commented"
		if q.State == github.ReviewChangesRequested {
			said = "asked for changes"
		}
		head := fmt.Sprintf("### `%s` %s\n\nIn [review %d](%s):", defuse(q.Author.Login), said, q.ID, at)
		if strings.TrimSpace(q.Body) == "" {
			head = fmt.Sprintf("### `%s` %s\n\n[Review %d](%s) has no text of its own, only its line comments.", defuse(q.Author.Login), said, q.ID, at)
		} else {
			head += "\n\n" + reviewQuoted(q.Body, at)
		}
		if !add(head) {
			reviewsLeft++
			commentsLeft += len(q.Comments)
			continue
		}
		for _, c := range q.Comments {
			on := "On " + checkCode(defuseRefs(c.Path))
			if c.Line > 0 {
				on += fmt.Sprintf(", line %d", c.Line)
			}
			if !add(on + ":\n\n" + reviewQuoted(c.Body, at)) {
				commentsLeft++
			}
		}
	}
	if reviewsLeft+commentsLeft > 0 {
		var left []string
		if reviewsLeft > 0 {
			left = append(left, fmt.Sprintf("%d %s", reviewsLeft, plural(reviewsLeft, "more review", "more reviews")))
		}
		if commentsLeft > 0 {
			left = append(left, fmt.Sprintf("%d %s", commentsLeft, plural(commentsLeft, "more line comment", "more line comments")))
		}
		parts = append(parts, fmt.Sprintf("The rest didn't fit here: %s. [PR #%d](%s) has them all.", strings.Join(left, " and "), pr, link))
	}
	return strings.Join(parts, "\n\n")
}

// reviewQuoted is text, a review's or one of its line comments, from a
// person, cut to its first findingChars characters, with a line saying so
// and linking the review, at, its issue references defused, and fenced.
func reviewQuoted(text, at string) string {
	text = strings.TrimSpace(text)
	if utf8.RuneCountInString(text) <= findingChars {
		return ivFenced(defuseRefs(text))
	}
	return ivFenced(defuseRefs(firstChars(text, findingChars))) +
		fmt.Sprintf("\n\nThis is its first %s characters: [the review](%s) has the whole.", formatThousands(findingChars), at)
}

// formatThousands is n with a comma between each group of three digits,
// as 1,000.
func formatThousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
