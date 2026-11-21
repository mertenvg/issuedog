package githubtest

import (
	"net/http"
	"slices"
	"strconv"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/issuedog/internal/github"
)

// Reviews. ReviewPR submits a review on an open pull request the fake
// opened, as a person does from the web page: the pull request's timeline
// then gives it as a `reviewed` event, which, as GitHub's does, carries the
// reviewer as its user and the time as its submitted_at, not an actor and
// a created_at, and its state lower case. The review's ID is its event's.
// Its line comments are served on GET /pulls/{n}/reviews/{review}/comments,
// oldest first, paged. A review moves the pull request's updated_at, as
// GitHub is believed to (SPEC §16), so a poll lists it.

// review is one review on a pull request: its `reviewed` timeline event,
// and its line comments, oldest first.
type review struct {
	event    *gogithub.Timeline
	comments []*gogithub.PullRequestComment
}

// ReviewLine is one line comment of a review ReviewPR submits: the file
// it's on, its line in that file, or 0 for a comment on the whole file, and
// its text.
type ReviewLine struct {
	Path string
	Line int
	Body string
}

// reviewStates are the states ReviewPR takes, as a review's `reviewed`
// event gives them. A dismissed one stands for a review dismissed since.
var reviewStates = []string{github.ReviewCommented, github.ReviewChangesRequested, github.ReviewApproved, github.ReviewDismissed}

// handleReviews registers the review routes.
func (f *Fake) handleReviews() {
	f.handle("GET /pulls/{n}/reviews/{review}/comments", f.reviewComments)
}

// ReviewPR submits a review on open pull request number as login, with
// state, one of the github.Review* states, body as its text and comments as
// its line comments, and returns the review's ID. It moves the pull
// request's updated_at to its submitted_at. A pull request the fake didn't
// open, a closed one, and a state GitHub's event doesn't give fail the
// test, naming why.
func (f *Fake) ReviewPR(number int, login, state, body string, comments []ReviewLine) int64 {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	is := f.fakePull(number)
	if is.GetState() != "open" {
		f.fakeFail("githubtest: review #%d: it's %s, and only an open pull request is reviewed here", number, is.GetState())
	}
	if !slices.Contains(reviewStates, state) {
		f.fakeFail("githubtest: review #%d: state %q is none of %q", number, state, reviewStates)
	}
	now := f.tick()
	id := f.nextID()
	r := &review{event: &gogithub.Timeline{ID: &id, Event: new("reviewed"), User: user(login), State: &state, Body: &body, SubmittedAt: now}}
	for _, l := range comments {
		cm := &gogithub.PullRequestComment{ID: new(f.nextID()), PullRequestReviewID: &id, User: user(login), Path: new(l.Path),
			Body: new(l.Body), CreatedAt: now, UpdatedAt: now, SubjectType: new("file")}
		if l.Line != 0 {
			cm.Line, cm.OriginalLine, cm.SubjectType = new(l.Line), new(l.Line), new("line")
		}
		r.comments = append(r.comments, cm)
	}
	is.pr.reviews = append(is.pr.reviews, r)
	is.UpdatedAt = now
	return id
}

// reviewComments serves the line comments of review {review} on pull
// request {n}, oldest first, paged, or a 404 for an issue the fake didn't
// open as a pull request and for a review that pull request hasn't got.
func (f *Fake) reviewComments(c *call) (int, any) {
	id, err := strconv.ParseInt(c.r.PathValue("review"), 10, 64)
	if c.issue.pr == nil || err != nil {
		return http.StatusNotFound, message("Not Found")
	}
	i := slices.IndexFunc(c.issue.pr.reviews, func(r *review) bool { return r.event.GetID() == id })
	if i < 0 {
		return http.StatusNotFound, message("Not Found")
	}
	return page(c, c.issue.pr.reviews[i].comments)
}
