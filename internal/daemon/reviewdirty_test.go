package daemon_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// The owner edits the interviewer's spec revision comment, which used to
// put the epic back at clarifying, with a disagreement, for an interviewer
// run of its own. Now a whole pass through the loop records the move to
// spec-review again, notes the edit, and posts revision 1 again from the
// App's clean body, with no state error and no run. The next two passes
// write nothing and start no run, and the second, unchanged, asks only
// conditional GETs answered 304. Then the owner's 👍 on the new revision
// approves the spec.
func TestSpecDirtyRun(t *testing.T) {
	t.Parallel()
	f, g, n, answer, rev := specRev1(t)
	entry := f.State().Issues[n-1].Comments[0].Body
	if !strings.Contains(entry, "from=intake to=clarifying event=5") {
		t.Fatalf("the epic's first comment is %q; want its move to clarifying", entry)
	}
	hash := state.BodyHash(f.State().Issues[n-1].Body)
	f.EditComment(rev, "owner", "The spec, mine now.")
	writes := len(f.Writes())
	g.pass()
	var got []string
	for _, w := range f.Writes()[writes:] {
		got = append(got, w.Method+" "+w.Path)
	}
	if want := []string{"POST /issues/1/comments", fmt.Sprintf("PATCH /issues/comments/%d", rev), "POST /issues/1/comments"}; !slices.Equal(got, want) {
		t.Errorf("the edit's pass wrote %q; want the record, the note on the edit, and the new revision, %q", got, want)
	}
	cs := f.State().Issues[n-1].Comments
	// The record's seen is the bound of the move to clarifying, which
	// carries none: its own comment, the epic's first.
	first := ivFirstApp(t, f, n)
	record, revised := cs[len(cs)-2].Body, cs[len(cs)-1].Body
	if want := specDirtyRecorded(rev, first); record != want {
		t.Errorf("the record =\n%s\nwant\n%s", record, want)
	}
	want := fmt.Sprintf("**Revision 1** of the spec, `%s`.\n\n"+
		"- To approve it, react 👍 to this comment, or post `/approve`.\n"+
		"- To send it back, post `/revise <notes>`, or any other comment.\n\n"+
		"<!-- issuedog:revision rev=1 body=%s seen=%d -->", hash, hash, answer)
	if revised != want {
		t.Errorf("the new revision =\n%s\nwant\n%s", revised, want)
	}
	for _, c := range cs {
		if strings.Contains(c.Body, "<!-- issuedog:error ") {
			t.Errorf("a comment carries a state error: %q", c.Body)
		}
	}
	if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:spec-review", "type:epic"}) || g.runs() != 1 {
		t.Errorf("labels %v after %d runs; want stage:spec-review, and no run since the spec's", labels, g.runs())
	}

	writes = len(f.Writes())
	g.pass()
	from := len(f.Requests())
	g.pass()
	if w := f.Writes(); len(w) != writes || g.runs() != 1 {
		t.Errorf("the next passes wrote %+v, after %d runs; want no write and no run", w[writes:], g.runs())
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}

	f.AddReaction(specNewest(t, f, n), "owner", "+1")
	g.pass()
	if labels := ivLabels(f, n); !slices.Equal(labels, []string{"stage:planning", "type:epic"}) {
		t.Errorf("labels after the owner's 👍 on the new revision = %v, want stage:planning", labels)
	}
	if got, want := newestComment(f, n), fmt.Sprintf("from=spec-review to=planning rev=1 body=%s", hash); !strings.Contains(got, want) {
		t.Errorf("the approval =\n%s\nwant it to carry %q", got, want)
	}
}

// A spec revision comment made dirty (#447), as a plan comment is (#315):
// its move to spec-review no longer counts, but the issue stays at
// spec-review, as its label shows, with the move recorded again in an App
// comment of its own, and no state error. The record's seen is the bound
// of the latest move the clean markers show, so of the comments since,
// one the App 👍'd as an answer before the dirty comment was posted may be
// one the run read, and isn't a revision request, and any other still is
// (SPEC §5.1, §7.4).

// specDirtyRecorded is the App's comment recording again the move to
// spec-review that dirty comment id hid, with seen.
func specDirtyRecorded(id, seen int64) string {
	return "A comment of mine was edited without approval, and it held this issue's move to `spec-review`, so I've recorded the move again here. " +
		"It's no revision of the spec: to approve the spec, approve the next revision I post. To send it back, post `/revise <notes>`, or any other comment.\n\n" +
		fmt.Sprintf("<!-- issuedog:transition from=clarifying to=spec-review event=spec-dirty-%d seen=%d -->", id, seen)
}

// specDirtyRevision is the spec revision ReviewVisit posts on its own,
// revision rev of "body", having read up to comment seen.
func specDirtyRevision(rev int, seen int64) string {
	h := state.BodyHash("body")
	return fmt.Sprintf("**Revision %d** of the spec, `%s`.\n\n"+
		"- To approve it, react 👍 to this comment, or post `/approve`.\n"+
		"- To send it back, post `/revise <notes>`, or any other comment.\n\n"+
		"<!-- issuedog:revision rev=%d body=%s seen=%d -->", rev, h, rev, h, seen)
}

// specDirtyAfterHold files issue 1, the owner's story with body "body", at
// spec-review after a discussion hold from spec-review: revision 1
// (comment 1002), the hold (1003), its end, back to clarifying (1004), and
// the interviewer's spec rewritten there, revision 2 (1005), which carries
// origin. The App moved the labels with each move. Then it runs more.
func specDirtyAfterHold(more func(t testing.TB, f *githubtest.Fake)) func(testing.TB) *githubtest.Fake {
	return filed(nil, func(t testing.TB, f *githubtest.Fake) {
		f.SetType(1, "Story")
		h := state.BodyHash("body")
		f.AddLabel(1, app, "stage:clarifying")
		post(t, f, 1001, app, "Questions.\n\n<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
		post(t, f, 1002, app, revision(1, "body"))
		f.AddLabel(1, app, "stage:spec-review")
		f.RemoveLabel(1, app, "stage:clarifying")
		post(t, f, 1003, app, "On hold.\n\n<!-- issuedog:transition from=spec-review to=held at=spec-review origin=spec-review reason=discussion event=c9 seen=1001 -->")
		f.AddLabel(1, app, "stage:held")
		f.RemoveLabel(1, app, "stage:spec-review")
		post(t, f, 1004, app, "Resolved.\n\n<!-- issuedog:transition from=held to=clarifying at=spec-review origin=spec-review event=h1003 seen=1001 -->")
		f.AddLabel(1, app, "stage:clarifying")
		f.RemoveLabel(1, app, "stage:held")
		post(t, f, 1005, app, fmt.Sprintf("The spec, rewritten.\n\n<!-- issuedog:transition from=clarifying to=spec-review rev=2 body=%s origin=spec-review run=r2 seen=1004 -->\n"+
			"<!-- issuedog:revision rev=2 body=%s -->", h, h))
		f.AddLabel(1, app, "stage:spec-review")
		labelEvent(t, f.RemoveLabel(1, app, "stage:clarifying"), 9009)
		more(t, f)
	})
}

// A visit, step 1 then ReviewVisit, to a story whose spec revision comment
// is made dirty: the move to spec-review is recorded again, and ReviewVisit
// posts the next revision, as for a hidden one, so the spec can be approved
// with no interviewer run. The owner's answer the run read stays an
// answer; a comment posted during the run is a revision request, as
// before the edit. The clarifying after a discussion hold isn't recorded
// again: its moves carry origin, which only the dirty text holds.
func TestSpecDirtyStaysAtSpecReview(t *testing.T) {
	t.Parallel()
	specReview, clarifying := []string{"stage:spec-review"}, []string{"stage:clarifying"}
	none := func(testing.TB, *githubtest.Fake) int64 { return 0 }
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"the owner's edit to the spec revision comment leaves the issue at spec-review, with a new revision",
			func(t testing.TB) *githubtest.Fake {
				f := reviewResumeSpecReview(true, none)(t)
				f.EditComment(1004, "owner", "The spec, mine now.")
				return f
			},
			outcome{comments: []string{specDirtyRecorded(1004, 1001), specDirtyRevision(1, 1002)}, labels: specReview, acks: []string{"1a"}}},
		{"a comment posted during the interviewer run is still a revision request",
			func(t testing.TB) *githubtest.Fake {
				f := reviewResumeSpecReview(true, func(t testing.TB, f *githubtest.Fake) int64 {
					post(t, f, 1004, "owner", "also X")
					return 0
				})(t)
				f.EditComment(1005, "owner", "The spec, mine now.")
				return f
			},
			outcome{comments: []string{specDirtyRecorded(1005, 1001), sentBack("1004")}, labels: clarifying, acks: []string{"1a", "also X"}}},
		{"the interviewer's spec after a discussion hold still disagrees",
			specDirtyAfterHold(func(t testing.TB, f *githubtest.Fake) { f.EditComment(1005, "owner", "Edited.") }),
			outcome{comments: []string{planDirtyDisagreed(state.Clarifying, "`stage:spec-review`", 9009)}, labels: clarifying}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var in []string
			check(t, tt.setup, visit(t, owners, &in), tt.want)
		})
	}
}

// At plan-review, the record's seen is the bound of the move to planning,
// the spec's approval, which read up to 1001, so the owner's comment, 1004,
// posted during the planner run, is a revision request still (#318's
// window), whether the App 👍'd it during a read that didn't list the
// planner's move yet (#318) or not at all, and so is one after the plan
// comment, before a later dirty App comment. A later dirty App comment
// keying the record doesn't hide that the 👍 came after the plan comment.
// One the App 👍'd as an answer before the plan comment was posted is one
// the run read, as a resumed run leaves no answer it didn't read
// (TestAnswerAfterLaggedApply), so it isn't, nor once the plan comment is
// deleted after the record. Its edit after the record gets the edited
// reply; one before gets none, as the run may have read it (#485). A
// /resume's record bounds nothing more: its seen is the hidden move's own.
func TestPlanDirtySeen(t *testing.T) {
	t.Parallel()
	planning, atReview := []string{"stage:planning"}, []string{"stage:plan-review"}
	during := func(t testing.TB, f *githubtest.Fake) { post(t, f, 1004, "owner", "also X") }
	answered := func(t testing.TB, f *githubtest.Fake) {
		during(t, f)
		f.AddReaction(1004, app, "+1")
	}
	for _, tt := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
		want  outcome
	}{
		{"a comment the App 👍'd after the planner's move is a request",
			planReviewRun(during, func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1004, app, "+1")
				f.EditComment(1005, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyRecorded(1005, 1001), planSentBack("1004")}, labels: planning, acks: []string{"also X"}}},
		{"so is one posted during the run, with no 👍",
			planReviewRun(during, func(t testing.TB, f *githubtest.Fake) { f.EditComment(1005, "owner", "Edited.") }),
			outcome{comments: []string{planDirtyRecorded(1005, 1001), planSentBack("1004")}, labels: planning, acks: []string{"also X"}}},
		{"and one after the plan comment, before a later dirty App comment",
			revisePlanReview(func(t testing.TB, f *githubtest.Fake) {
				post(t, f, 1005, "owner", "Split it.")
				post(t, f, 1006, app, "Noted.")
				f.EditComment(1004, "owner", "Edited.")
				f.EditComment(1006, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyRecorded(1006, 1001), planSentBack("1005")}, labels: planning, acks: []string{"Split it."}}},
		{"one the App 👍'd before the plan comment isn't",
			planReviewRun(answered, func(t testing.TB, f *githubtest.Fake) { f.EditComment(1006, "owner", "Edited.") }),
			outcome{comments: []string{planDirtyRecorded(1006, 1001)}, labels: atReview, acks: []string{"also X"}}},
		{"nor is it once the plan comment is deleted after the record",
			planReviewRun(answered, func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1006, "owner", "Edited.")
				post(t, f, 1007, app, planDirtyRecorded(1006, 1001))
				f.DeleteCommentBy("owner", 1006)
			}),
			outcome{labels: atReview, acks: []string{"also X"}}},
		{"one the App 👍'd after the plan comment is a request, though a later dirty App comment keys the record",
			planReviewRun(during, func(t testing.TB, f *githubtest.Fake) {
				if id := f.AddReaction(1004, app, "+1"); id != 1006 {
					t.Fatalf("the App's 👍 is %d; want 1006", id)
				}
				post(t, f, 1007, app, "Noted.")
				f.EditComment(1005, "owner", "Edited.")
				f.EditComment(1007, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyRecorded(1007, 1001), planSentBack("1004")}, labels: planning, acks: []string{"also X"}}},
		{"an answer the run read, edited after the record, gets the edited reply",
			planReviewRun(answered, func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1006, "owner", "Edited.")
				post(t, f, 1007, app, planDirtyRecorded(1006, 1001))
				f.EditComment(1004, "owner", "also Y")
			}),
			outcome{comments: []string{edited("also Y", "1004")}, labels: atReview, acks: []string{"also Y"}}},
		{"but none when edited before the record",
			planReviewRun(answered, func(t testing.TB, f *githubtest.Fake) {
				f.EditComment(1004, "owner", "also Y")
				f.EditComment(1006, "owner", "Edited.")
			}),
			outcome{comments: []string{planDirtyRecorded(1006, 1001)}, labels: atReview, acks: []string{"also Y"}}},
		// The plan comment is clean, and a /resume's move back to
		// plan-review is dirty, as after a crash before the pass that
		// would have handled 1004: its record carries the hold's seen, so
		// 1004, 👍'd after the plan comment, is a request.
		{"a /resume's record swallows no 👍'd comment",
			planReviewRun(during, func(t testing.TB, f *githubtest.Fake) {
				f.AddReaction(1004, app, "+1")
				post(t, f, 1007, "owner", "/hold")
				f.AddReaction(1007, app, "+1")
				post(t, f, 1009, app, moved("On hold by owner, from `plan-review`.", "from=plan-review to=held at=plan-review reason=owner cmd=1007 seen=1001"))
				f.AddLabel(1, app, "stage:held")
				f.RemoveLabel(1, app, "stage:plan-review")
				post(t, f, 1010, "owner", "/resume")
				f.AddReaction(1010, app, "+1")
				post(t, f, 1012, app, moved("Resumed by owner, back to `plan-review`.", "from=held to=plan-review cmd=1010 seen=1001"))
				f.AddLabel(1, app, "stage:plan-review")
				f.RemoveLabel(1, app, "stage:held")
				f.EditComment(1012, "owner", "Edited.")
			}),
			outcome{comments: []string{planResumeDirtyRecorded(1012, 1001), planSentBack("1004")}, labels: planning,
				acks: []string{"also X", "/hold", "/resume"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var in []string
			check(t, tt.setup, visit(t, owners, &in), tt.want)
		})
	}
}
