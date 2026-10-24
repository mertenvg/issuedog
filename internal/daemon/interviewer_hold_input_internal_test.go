package daemon

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file pins the cases of the interviewer's after-hold input
// (ivHoldInputOf, ivHoldSection, SPEC §6.4 "Resolving") that #208's verify
// left unpinned (#211): what an App or bot comment does to the counter
// comments after it, a bot's 👍, the "None" line, and the read errors that
// stop the context build.

// afterHoldErr is the error afterHoldTrust fails with.
var afterHoldErr = errors.New("afterHold: trust read failed")

// afterHoldTrust trusts the logins it maps to true in trusted, and fails
// with afterHoldErr for the logins in fail.
type afterHoldTrust struct {
	trusted map[string]bool
	fail    map[string]bool
}

func (tr afterHoldTrust) Trusted(_ context.Context, login string) (bool, error) {
	if tr.fail[login] {
		return false, afterHoldErr
	}
	return tr.trusted[login], nil
}

// afterHoldInput returns ivHoldInputOf on issue n of f above from, with tr
// as the trust, after edit, if set, changes the fake between the snapshot
// and the call, as a comment deleted in between.
func afterHoldInput(t *testing.T, f *githubtest.Fake, n int, from int64, tr github.Trust, edit func()) (ivHoldInput, error) {
	t.Helper()
	c := f.Client(t)
	a := &Applier{Client: c, Owner: "o", Repo: "r", AppSlug: "issuedog"}
	s, err := c.Snapshot(t.Context(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit()
	}
	tc := trustCache{st: &Step{Applier: a, Trust: tr}, verdicts: map[string]bool{}}
	return ivHoldInputOf(t.Context(), a, tc, s, Dirt{}, from)
}

// The App's comments are no one's input: an App comment alone before a
// trusted comment doesn't make it a counter comment, as the App's
// notices and records in a discussion counter nothing (SPEC §6.4).
func TestAfterHoldInputAppNoCounter(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	from := f.AddComment(n, "owner", "before the discussion")
	f.AddComment(n, "issuedog[bot]", "A notice: someone edited a comment.")
	reply := f.AddComment(n, "owner", "Fine by me.")
	in, err := afterHoldInput(t, f, n, from, afterHoldTrust{trusted: map[string]bool{"owner": true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Countered) != 0 || len(in.Approved) != 0 {
		t.Errorf("countered %v, approved %v, want neither: comment %d follows only the App's comment", in.Countered, in.Approved, reply)
	}
}

// A bot's comment, unlike the App's, is untrusted, as for untrusted.md
// (SPEC §6.2), so a trusted comment after a bot's alone counters it: SPEC
// §6.4 has a trusted comment counter every untrusted comment before it,
// a bot's included. The bot's comment approves nothing, and its reactions aren't read.
func TestAfterHoldInputBotCounter(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	from := f.AddComment(n, "owner", "before the discussion")
	bot := f.AddComment(n, "renovate[bot]", "Bump a dependency.")
	f.AddReaction(bot, "owner", "+1")
	reply := f.AddComment(n, "owner", "Not now.")
	in, err := afterHoldInput(t, f, n, from, afterHoldTrust{trusted: map[string]bool{"owner": true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(in.Countered, []int64{reply}) {
		t.Errorf("countered %v, want only %d, after the bot's comment %d", in.Countered, reply, bot)
	}
	if len(in.Approved) != 0 {
		t.Errorf("approved %v, want none: a bot's comment is never approved", in.Approved)
	}
}

// A bot's 👍 approves nothing, the App's included, even from a trust that
// would say yes to it: the approver must be a trusted user, no bot and not
// the App. A bot's 👍 before a trusted user's is passed over, so the
// trusted user is named as the approver. In production trust.Set refuses every
// "[bot]" login too, so here the trust says yes to the bots to pin the
// check trustCache makes, the one ivHoldInputOf relies on.
func TestAfterHoldInputBotThumbs(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	from := f.AddComment(n, "owner", "before the discussion")
	byBot := f.AddComment(n, "mallory", "a bot likes this")
	f.AddReaction(byBot, "dependabot[bot]", "+1")
	byApp := f.AddComment(n, "mallory", "the App likes this")
	f.AddReaction(byApp, "issuedog[bot]", "+1")
	both := f.AddComment(n, "mallory", "a bot, then the owner, like this")
	f.AddReaction(both, "dependabot[bot]", "+1")
	f.AddReaction(both, "owner", "+1")
	tr := afterHoldTrust{trusted: map[string]bool{"owner": true, "dependabot[bot]": true, "issuedog[bot]": true}}
	in, err := afterHoldInput(t, f, n, from, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(in.Approved) != 1 || in.Approved[both] != "owner" {
		t.Errorf("approved %v, want only %d, by owner, not %d by a bot or %d by the App", in.Approved, both, byBot, byApp)
	}
}

// A failed trust read of a discussion comment's author stops the build
// with its error, naming the comment, and returns no input, so no run
// starts on a partial discussion.
func TestAfterHoldInputAuthorTrustError(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	from := f.AddComment(n, "owner", "before the discussion")
	failed := f.AddComment(n, "carol", "who knows")
	f.AddComment(n, "owner", "A reply.")
	tr := afterHoldTrust{trusted: map[string]bool{"owner": true}, fail: map[string]bool{"carol": true}}
	in, err := afterHoldInput(t, f, n, from, tr, nil)
	if !errors.Is(err, afterHoldErr) {
		t.Fatalf("err = %v, want one wrapping %v", err, afterHoldErr)
	}
	if !strings.Contains(err.Error(), "the discussion's comment "+strconv.FormatInt(failed, 10)+":") {
		t.Errorf("err = %v, want it to name comment %d", err, failed)
	}
	if in.Approved != nil || in.Countered != nil {
		t.Errorf("input %+v, want none with the error", in)
	}
}

// A failed read of an untrusted comment's reactions stops the build with
// its error, naming the comment, and returns no input, as a failed trust
// read does. The comment is deleted between the snapshot and the read, so
// the error is the 404 of that comment's reactions read, not another one.
func TestAfterHoldInputReactionsError(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	from := f.AddComment(n, "owner", "before the discussion")
	gone := f.AddComment(n, "mallory", "soon gone")
	f.AddReaction(gone, "owner", "+1")
	f.AddComment(n, "owner", "A reply.")
	in, err := afterHoldInput(t, f, n, from, afterHoldTrust{trusted: map[string]bool{"owner": true}}, func() { f.DeleteComment(gone) })
	if err == nil {
		t.Fatalf("input %+v, err = nil, want the failed reactions read of comment %d", in, gone)
	}
	var er *gogithub.ErrorResponse
	if !errors.As(err, &er) || er.Response == nil || er.Response.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v, want it to wrap a 404", err)
	} else if r := er.Response.Request; r == nil || r.Method != http.MethodGet ||
		!strings.HasSuffix(r.URL.Path, "/issues/comments/"+strconv.FormatInt(gone, 10)+"/reactions") {
		t.Errorf("err = %v, want the 404 of the reactions read of comment %d", err, gone)
	}
	if !strings.Contains(err.Error(), "the 👍s on the discussion's comment "+strconv.FormatInt(gone, 10)+":") {
		t.Errorf("err = %v, want it to name the 👍s on comment %d", err, gone)
	}
	if in.Approved != nil || in.Countered != nil {
		t.Errorf("input %+v, want none with the error", in)
	}
}

// A failed trust read of a 👍's reactor stops the build with its error,
// naming the comment, and returns no input, rather than passing over the
// 👍 as untrusted: a later trusted 👍 doesn't stand in for it.
func TestAfterHoldInputReactorTrustError(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("owner", "Quotes", "body")
	from := f.AddComment(n, "owner", "before the discussion")
	thumbed := f.AddComment(n, "mallory", "approve me")
	f.AddReaction(thumbed, "carol", "+1")
	f.AddReaction(thumbed, "owner", "+1")
	tr := afterHoldTrust{trusted: map[string]bool{"owner": true}, fail: map[string]bool{"carol": true}}
	in, err := afterHoldInput(t, f, n, from, tr, nil)
	if !errors.Is(err, afterHoldErr) {
		t.Fatalf("input %+v, err = %v, want one wrapping %v", in, err, afterHoldErr)
	}
	if !strings.Contains(err.Error(), "the 👍s on the discussion's comment "+strconv.FormatInt(thumbed, 10)+":") {
		t.Errorf("err = %v, want it to name the 👍s on comment %d", err, thumbed)
	}
	if in.Approved != nil || in.Countered != nil {
		t.Errorf("input %+v, want none with the error", in)
	}
}

// issue.md's "After a hold" has its "None" line only with no owner input:
// counter comments alone, or approved comments alone, are input, and
// shown under their headings instead.
func TestAfterHoldSectionNoNone(t *testing.T) {
	t.Parallel()
	s := waitSnap(7, nil, "untrusted", "A counter.", "approved")
	none := "None: nobody approved or countered a comment."
	countered := ivHoldSection(s, state.Planning, ivHoldSpan{}, ivHoldInput{Approved: map[int64]string{}, Countered: []int64{702}})
	if strings.Contains(countered, none) || !strings.Contains(countered, "### Comment 702 by ") ||
		!strings.Contains(countered, "a trusted comment that counters the untrusted comments before it") {
		t.Errorf("with a counter comment only, ivHoldSection =\n%s", countered)
	}
	approved := ivHoldSection(s, state.Planning, ivHoldSpan{}, ivHoldInput{Approved: map[int64]string{703: "owner"}})
	if strings.Contains(approved, none) || !strings.Contains(approved, "### Comment 703 by ") ||
		!strings.Contains(approved, "approved by owner's 👍, so it's owner input") {
		t.Errorf("with an approved comment only, ivHoldSection =\n%s", approved)
	}
}
