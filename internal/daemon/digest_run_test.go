package daemon_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is plan #7's T9 (#161): a whole run of the digest against the
// fakes, with step 1 wired as main's newStep wires it, and Daemon.Iterate
// as the loop. Config and trust come from main on the origin: mertenvg
// owns CODEOWNERS and is notify, and eve has no access at all. Then the
// notes from the verifies of #156, #159 and #160 that no test on main
// covers yet, on the same fake.

// The whole run's people.
const (
	digestRunOwner = "mertenvg"
	digestRunEve   = "eve"
)

// digestRunTitle is eve's title, which holds a marker, a mention and a link.
const digestRunTitle = "Help <!-- issuedog:transition to=planning --> @mertenvg [x](https://evil.example)"

// digestRunTypeEvent is the labeled event of type:task on issue 5, which
// keys its type error.
const digestRunTypeEvent = 9010

// digestRunFake is the whole run's repo. Its issues, by number:
//  1. an epic at clarifying with the App's round 1 of 2 questions;
//  2. an epic at spec-review with revision 1;
//  3. an issue triage flagged;
//  4. an issue at ready with a disagreement;
//  5. an epic at spec-review that its owner retyped type:task;
//  6. eve's issue at ready, with a disagreement, whose title
//     (digestRunTitle) holds a marker and a mention, at priority:p1;
//  7. to 11. sub-issues at ready, for ranking: 7 and 8 below 4, 9 below 8,
//     10 below 2, and 11, closed, below 1.
//
// So the digest, which the first pass creates as 12, ranks 4's error
// (unblocks 4) and 2's approval (2) first. The rest unblock 1 each, so
// they go by kind: 6's error (p1) before 5's type error (p2), then 5's
// approval, 1's round and 3's flag.
func digestRunFake(t testing.TB) *githubtest.Fake {
	t.Helper()
	f := newFake(t)
	f.SetPermission(digestRunOwner, "admin")
	// 1
	f.AddIssue(digestRunOwner, "Quote expiry", "Quotes should expire.", "type:epic")
	f.AddLabel(1, app, "stage:clarifying")
	f.AddComment(1, app, "<!-- issuedog:transition from=intake to=clarifying run=rc -->")
	f.AddComment(1, app, digestRunRound(1, 1, 2, "**1. How long should a quote last?** Blocks: #1\n   a. 30 days (recommended): as billing does\n   b. 7 days\n\n"+
		"**2. Who can extend one?** Blocks: #1\n   a. Owners (recommended): fewer mistakes\n   b. Anyone\n   c. No one"))
	// 2
	f.AddIssue(digestRunOwner, "Quote reminders", "Remind before expiry.", "type:epic")
	f.AddLabel(2, app, "stage:spec-review")
	f.AddComment(2, app, revision(1, "Remind before expiry."))
	// 3
	f.AddIssue(digestRunOwner, "Odd export", "The export looks odd.")
	f.AddComment(3, app, digestRunFlag(3, "Export format"))
	f.AddLabel(3, app, "triage:ask")
	// 4
	f.AddIssue(digestRunOwner, "Broken sync", "Fix the sync.")
	f.AddLabel(4, app, "stage:ready")
	f.AddComment(4, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	digestErrorOn(f, 4, "1")
	// 5
	f.AddIssue(digestRunOwner, "Quote audit", "Audit quotes.", "type:epic")
	f.AddLabel(5, app, "stage:spec-review")
	f.AddComment(5, app, revision(1, "Audit quotes."))
	f.RemoveLabel(5, digestRunOwner, "type:epic")
	flagWant(t, f.AddLabel(5, digestRunOwner, "type:task"), digestRunTypeEvent)
	// 6
	f.AddIssue(digestRunEve, digestRunTitle, "Please look.")
	f.AddLabel(6, app, "stage:ready")
	f.AddComment(6, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
	digestErrorOn(f, 6, "2")
	f.AddLabel(6, digestRunOwner, "priority:p1")
	// 7 to 11
	for _, parent := range []int{4, 4, 8, 2, 1} {
		n := f.AddIssue(digestRunOwner, "Part", "A part.")
		f.AddLabel(n, app, "stage:ready")
		f.AddComment(n, app, "<!-- issuedog:transition from=intake to=ready run=rready -->")
		f.SetParent(n, parent)
	}
	f.Close(11)
	return f
}

// digestRunRound is the App's round comment on issue n: round round of
// count questions, with the cc line and the round's move.
func digestRunRound(n, round, count int, questions string) string {
	return fmt.Sprintf("### Questions for #%d (round %d)\n\n%s\n\nReply in one comment, e.g. `1a 2c 3: free text`.\n\ncc @%s\n\n"+
		"<!-- issuedog:transition from=clarifying to=clarifying run=r%d round=%d questions=%d -->", n, round, questions, digestRunOwner, round, round, count)
}

// digestRunFlag is triage's flag on issue n, recommending handle.
func digestRunFlag(n int, summary string) string {
	return fmt.Sprintf("### Should issuedog handle #%d?\n\n**1. %s** Blocks: #%d\n   a. Handle it (recommended): it's a bug\n   b. Leave it\n\n"+
		"React 👍 to take the recommendation, or post `/track` or `/ignore`.\n\ncc @%s\n\n"+
		"<!-- issuedog:triage verdict=ask run=t%d recommend=handle -->", n, summary, n, digestRunOwner, n)
}

// digestRunOrigin is main with mertenvg in CODEOWNERS and as notify.
func digestRunOrigin(t testing.TB) *origin {
	t.Helper()
	o := newOrigin(t)
	o.commit(".github/CODEOWNERS", "* @"+digestRunOwner+"\n")
	o.commit(config.Path, "types: labels\nnotify: ["+digestRunOwner+"]\n")
	return o
}

// digestRunDaemon is a daemon of o/r on f that keeps the digest, as
// startDaemon makes it, with step 1 wired as main's newStep wires it. It
// talks to GitHub through c if set, and else through a client of f's.
// Its Work adds each issue it's given to *worked, if set.
func digestRunDaemon(t testing.TB, f *githubtest.Fake, o *origin, c *github.Client, worked *[]int) *daemon.Daemon {
	t.Helper()
	d := newDaemon(t, f, o, nil)
	if c != nil {
		d.Step.Client = c
	}
	d.Digest = true
	d.Work = func(_ context.Context, ready []daemon.Ready) error {
		for _, r := range ready {
			if worked != nil {
				*worked = append(*worked, r.S.Number)
			}
		}
		return nil
	}
	// As newStep wires step 1.
	st := d.Step
	st.Commands = daemon.Commands(st.CurrentTrust())
	st.Commands["approve"] = daemon.ApproveCommand()
	st.Commands["revise"] = daemon.ReviseCommand()
	maps.Copy(st.Commands, daemon.SettingCommands(st.CurrentTrust(), d.CurrentModels))
	maps.Copy(st.Commands, daemon.FlagCommands())
	approve, answer := daemon.ApproveByReaction(st.CurrentTrust()), daemon.FlagByReaction(st.CurrentTrust())
	errAck := daemon.ErrorByReaction(st.CurrentTrust())
	st.Reactions = func(ctx context.Context, a *daemon.Applier, s *github.Snapshot, dirt daemon.Dirt) error {
		if err := approve(ctx, a, s, dirt); err != nil {
			return fmt.Errorf("approval by 👍: %w", err)
		}
		if err := answer(ctx, a, s, dirt); err != nil {
			return fmt.Errorf("flag answer by 👍: %w", err)
		}
		if err := errAck(ctx, a, s, dirt); err != nil {
			return fmt.Errorf("state error ack by 👍: %w", err)
		}
		return nil
	}
	return d
}

// digestRunItemRE matches an item's heading and the line under it in the
// digest's body: its issue, kind, priority and unblock count.
var digestRunItemRE = regexp.MustCompile(`(?m)^### [0-9]+\. #([0-9]+)[^\n]*\n\n\*\*([^*]+)\*\*, priority (p[0-3]), unblocks ([0-9]+) open issue`)

// digestRunItems is the items in body, the digest's, in order, as
// "#<issue> <kind> <priority> <unblocks>".
func digestRunItems(body string) []string {
	var got []string
	for _, m := range digestRunItemRE.FindAllStringSubmatch(body, -1) {
		got = append(got, fmt.Sprintf("#%s %s %s %s", m[1], m[2], m[3], m[4]))
	}
	return got
}

// digestRunCommentID returns the ID of the comment on issue n whose body
// contains text, and fails t if there's none.
func digestRunCommentID(t *testing.T, f *githubtest.Fake, n int, text string) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if strings.Contains(c.Body, text) {
			return c.ID
		}
	}
	t.Fatalf("issue %d has no comment with %q", n, text)
	return 0
}

// digestRunNews counts the digest-new comments on issue n.
func digestRunNews(f *githubtest.Fake, n int) int {
	news := 0
	for _, c := range digestComments(f, n) {
		if strings.Contains(c, "<!-- issuedog:digest-new ") {
			news++
		}
	}
	return news
}

// digestRunStep runs one Iterate of d, which must succeed, and checks that
// it wrote want, as "METHOD /path" in order.
func digestRunStep(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, step string, want ...string) {
	t.Helper()
	if got := digestPass(t, f, d); !slices.Equal(got, want) {
		t.Errorf("%s: writes %q; want %q", step, got, want)
	}
}

// digestRunFail runs one Iterate of d, which must fail with an error that
// holds each of errs, and returns the writes it made.
func digestRunFail(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, errs ...string) []string {
	t.Helper()
	from := len(f.Writes())
	err := d.Iterate(context.Background())
	for _, e := range errs {
		if err == nil || !strings.Contains(err.Error(), e) {
			t.Fatalf("Iterate = %v; want an error with %q", err, e)
		}
	}
	return digestWrites(f, from)
}

// digestRunFirst is the first pass's writes: the type error on 5, then the
// digest's create, its pin, the edit that drops the pin note, and the one
// announcement.
var digestRunFirst = []string{"POST /issues/5/comments", "POST /issues", "POST /graphql", "PATCH /issues/12", "POST /issues/12/comments"}

// digestRunRanked is the first pass's items, as digestRunItems gives them.
var digestRunRanked = []string{
	"#4 State error p2 4", "#2 Approval p2 2",
	"#6 State error p1 1", "#5 State error p2 1", "#5 Approval p2 1", "#1 Question round p2 1", "#3 Triage flag p2 1",
}

// digestRunWithout is digestRunRanked without the items of issue n, or
// with kind, only n's of that kind.
func digestRunWithout(n int, kind string) []string {
	prefix := fmt.Sprintf("#%d %s", n, kind)
	return slices.DeleteFunc(slices.Clone(digestRunRanked), func(s string) bool { return strings.HasPrefix(s, prefix) })
}

// digestRunAnnounced is the end of the first announcement: its cc line,
// and the keys of the three state errors, in rank order.
const digestRunAnnounced = "\n\ncc @mertenvg\n\n<!-- issuedog:digest-new items=err-4-disagreement-1,err-6-disagreement-2,err-5-type-9010 -->"

// A whole run against the fakes (T9's acceptance criteria). The first pass
// creates and pins the digest, lists every item in Q3a's order and
// announces the new state errors in one comment, with one mention. An
// extra pass writes nothing. An answer, which step 1 takes with its 👍,
// removes its round without a mention, and a new round is listed, with
// none (Q2a). A 👍 on the type error posts its ack, and only its item goes:
// the retyped epic's approval stays (#156's verify). A new daemon on the
// same fake writes nothing. Eve's title is escaped and mentions nobody,
// in the body and the announcement. The digest never reaches Work.
func TestDigestRunWhole(t *testing.T) {
	f, o := digestRunFake(t), digestRunOrigin(t)
	var worked []int
	d := digestRunDaemon(t, f, o, nil, &worked)
	digestRunStep(t, f, d, "the first pass", digestRunFirst...)
	digest := f.State().Issues[11]
	if digest.Title != "issuedog digest" || digest.Author != app || !digest.Pinned || digest.Closed || strings.HasPrefix(digest.Body, "I couldn't pin") {
		t.Fatalf("issue 12 = %+v; want the App's open, pinned digest, with no pin note", digest)
	}
	if got := digestRunItems(digest.Body); !slices.Equal(got, digestRunRanked) {
		t.Errorf("items = %q; want %q", got, digestRunRanked)
	}
	news := digestComments(f, 12)
	if len(news) != 1 || strings.Count(news[0], "@"+digestRunOwner) != 1 || !strings.HasSuffix(news[0], digestRunAnnounced) {
		t.Errorf("the digest's comments = %q; want one announcement of the three errors, ending %q, with its one mention", news, digestRunAnnounced)
	}
	digestRunStep(t, f, d, "an extra pass")

	// The answer: step 1's 👍 on it, and the round's item goes.
	answer := f.AddComment(1, digestRunOwner, "1a 2b")
	digestRunStep(t, f, d, "the answer", fmt.Sprintf("POST /issues/comments/%d/reactions", answer), "PATCH /issues/12")
	if got, want := digestRunItems(f.State().Issues[11].Body), digestRunWithout(1, ""); !slices.Equal(got, want) {
		t.Errorf("items after the answer = %q; want %q", got, want)
	}
	// Round 2, as the interviewer would post it, is listed, with no mention.
	f.AddComment(1, app, digestRunRound(1, 2, 1, "**1. Keep expired quotes?** Blocks: #1\n   a. Yes (recommended): for audits\n   b. No"))
	digestRunStep(t, f, d, "round 2", "PATCH /issues/12")
	body := f.State().Issues[11].Body
	if got := digestRunItems(body); !slices.Equal(got, digestRunRanked) ||
		!strings.Contains(body, "- Round 2 asks 1 question, and waits for an answer.\n- **Question 1:** Keep expired quotes?\n") {
		t.Errorf("items after round 2 = %q, body %q; want round 2 listed where round 1 was", got, body)
	}
	// A 👍 on the type error: its ack, and only its item goes.
	typeErr := digestRunCommentID(t, f, 5, fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", digestRunTypeEvent))
	reaction := f.AddReaction(typeErr, digestRunOwner, "+1")
	digestRunStep(t, f, d, "the 👍 on the type error", "POST /issues/5/comments", "PATCH /issues/12")
	if got := commentBodies(f, 5); got[len(got)-1] != errAckReply(digestRunOwner, "type", strconv.Itoa(digestRunTypeEvent), reaction) {
		t.Errorf("the comments on 5 = %q; want the ack last", got)
	}
	body = f.State().Issues[11].Body
	if got, want := digestRunItems(body), digestRunWithout(5, "State error"); !slices.Equal(got, want) ||
		!strings.Contains(body, "- It can't be approved yet: its type doesn't fit `spec-review`") {
		t.Errorf("items after the ack = %q, body %q; want %q, with the approval held back", got, body, want)
	}
	digestRunStep(t, f, d, "an extra pass after the ack")
	for i := range 2 {
		digestRunStep(t, f, digestRunDaemon(t, f, o, nil, &worked), fmt.Sprintf("restart %d", i+1))
	}
	if slices.Contains(worked, 12) {
		t.Errorf("Work got issues %v; want never the digest", worked)
	}
	if n := digestRunNews(f, 12); n != 1 || len(f.State().Issues) != 12 {
		t.Errorf("%d announcements, %d issues; want one announcement, and no other digest", n, len(f.State().Issues))
	}

	// Eve's title, escaped, and her mention, defused, in both texts.
	body, news = f.State().Issues[11].Body, digestComments(f, 12)
	escaped := `#6: Help &lt;\!-- issuedog:transition to=planning --> @` + "​" + `mertenvg \[x\](https://evil.example)`
	if !strings.Contains(body, "### 3. "+escaped+"\n") || !strings.Contains(news[0], "on "+escaped+". Answer here:") {
		t.Errorf("body %q, announcement %q; want eve's title escaped in both", body, news[0])
	}
	if strings.Contains(body, "@"+digestRunOwner) || strings.Count(body, "<!--") != 1 || strings.Count(news[0], "<!--") != 1 {
		t.Errorf("body %q, announcement %q; want no mention in the body, and no marker but the App's own", body, news[0])
	}
}

// A failed post of the type error in step 1 (#156's verify) is the pass's
// error, and the retyped epic gets neither Work nor items, so the digest
// is created and announced without them. The next pass posts the type
// error once, lists both of 5's items, and announces the type error alone.
func TestDigestRunTypeErrPostFails(t *testing.T) {
	f := digestRunFake(t)
	var worked []int
	d := digestRunDaemon(t, f, digestRunOrigin(t), nil, &worked)
	f.FailWrite(1)
	if got := digestRunFail(t, f, d, "step 1 on o/r#5: check the type: "); !slices.Equal(got, digestRunFirst) {
		t.Errorf("writes = %q; want %q, the first failed", got, digestRunFirst)
	}
	if got, want := digestRunItems(f.State().Issues[11].Body), digestRunWithout(5, ""); !slices.Equal(got, want) || slices.Contains(worked, 5) {
		t.Errorf("items %q, Work got %v; want %q, and not 5", got, worked, want)
	}
	f.FailWrite(0)
	digestRunStep(t, f, d, "the next pass", "POST /issues/5/comments", "POST /issues/12/comments", "PATCH /issues/12")
	news := digestComments(f, 12)
	if got := digestRunItems(f.State().Issues[11].Body); !slices.Equal(got, digestRunRanked) || len(news) != 2 ||
		!strings.HasSuffix(news[1], fmt.Sprintf("<!-- issuedog:digest-new items=err-5-type-%d -->", digestRunTypeEvent)) {
		t.Errorf("items %q, announcements %q; want every item, and the type error announced alone", got, news)
	}
	digestRunStep(t, f, d, "an extra pass")
	if errs := errorMarkers(f, 5); len(errs) != 1 {
		t.Errorf("errors on 5 = %q; want the type error once", errs)
	}
}

// A run blocked by a type error whose post fails (#156's verify) is the
// pass's error, and its lease stays: the next pass applies it again, posts
// the type error once, and starts no run while it stands.
func TestDigestRunBlockedPostFails(t *testing.T) {
	f := newFake(t)
	f.AddIssue("owner", "Idea", "the idea", "type:epic")
	f.AddLabel(1, app, "stage:clarifying")
	f.AddComment(1, app, "Questions.\n\n<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
	f.AddComment(1, "owner", "An answer.")
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.pending = always
	g.stage.applyErr = fmt.Errorf("no row: %w", state.ErrNoTransition)
	var event int64
	g.stage.during = func() {
		if event == 0 {
			event = typeErrRetype(f, 1, "epic", "feature")
			f.FailWrite(len(f.Writes()) + 1) // the type error's post
		}
	}
	g.claude(okOut, 0)
	if err := g.work(1); err == nil || !strings.Contains(err.Error(), "report run run1's move: ") {
		t.Fatalf("work = %v; want the failed post's error", err)
	}
	if w := f.Writes(); !w[len(w)-1].Failed || w[len(w)-1].Path != "/issues/1/comments" || len(errorMarkers(f, 1)) != 0 {
		t.Fatalf("writes %+v; want the type error's post last, failed", w)
	}
	f.FailWrite(0)
	for range 3 {
		if err := g.work(1); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{fmt.Sprintf("<!-- issuedog:error kind=type key=%d -->", event)}
	if errs := errorMarkers(f, 1); g.runs() != 1 || !slices.Equal(errs, want) || g.lease().RunID != "" {
		t.Errorf("%d runs, errors %q, lease %+v; want one run, %q, and the lease gone", g.runs(), errs, g.lease(), want)
	}
}

// An issue held by its type error doesn't stop another issue's run in Work
// (#156's verify): both have new input at clarifying, the first is skipped
// for its type error, and the second runs.
func TestDigestRunTypeErrHoldsOne(t *testing.T) {
	f := newFake(t)
	for _, typ := range []string{"feature", "epic"} {
		n := f.AddIssue("owner", "Idea", "the idea", "type:epic")
		f.AddLabel(n, app, "stage:clarifying")
		f.AddComment(n, app, "Questions.\n\n<!-- issuedog:transition from=intake to=clarifying run=r0 -->")
		if typ != "epic" {
			typeErrRetype(f, n, "epic", typ)
		}
		f.AddComment(n, "owner", "An answer.")
	}
	g := newRig(t, f, newOrigin(t), t.TempDir())
	g.stage.pending = always
	g.claude(okOut, 0)
	g.pass()
	g.pass()
	if got := g.leaseIssues(); !slices.Equal(got, []int{2}) || len(errorMarkers(f, 1)) != 1 || !slices.Contains(ivLabels(f, 2), "stage:spec-review") {
		t.Errorf("runs on %v, errors on 1 %q, labels of 2 %v; want one run, on 2, which moved on, and 1's type error", got, errorMarkers(f, 1), ivLabels(f, 2))
	}
}

// A digest dropped after a one-off 404 on its edit, which came after an
// announcement, and while its pin note was there, is found again (#160's
// verify): its pin is checked again, and it's pinned and loses the note,
// but nothing is announced again, and no other digest is created.
func TestDigestRunRefound(t *testing.T) {
	f := digestRunFake(t)
	d := digestRunDaemon(t, f, digestRunOrigin(t), nil, nil)
	f.FailPin("Forbidden")
	digestRunStep(t, f, d, "the first pass", digestRunFirst...)
	if is := f.State().Issues[11]; is.Pinned || !strings.HasPrefix(is.Body, "I couldn't pin this issue: `Forbidden`. Pin it by hand.\n\n") {
		t.Fatalf("issue 12 = %+v; want it unpinned, with the pin note", is)
	}
	f.FailPin("")
	digestErrorOn(f, 4, "3")
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodPatch && r.Path == "/issues/12" }, http.StatusNotFound)
	if got := digestRunFail(t, f, d, "the digest is gone"); !slices.Equal(got, []string{"POST /issues/12/comments"}) {
		t.Errorf("writes = %q; want the announcement of the new error, and the edit that 404s", got)
	}
	digestRunStep(t, f, d, "the pass that finds it again", "POST /graphql", "PATCH /issues/12")
	is := f.State().Issues
	if !is[11].Pinned || !strings.HasPrefix(is[11].Body, "8 items wait on you") || digestRunNews(f, 12) != 2 || len(is) != 12 {
		t.Errorf("issues %+v, %d announcements; want 12 pinned, listing 8 items, with no note, two announcements, and no other digest", is, digestRunNews(f, 12))
	}
	digestRunStep(t, f, d, "an extra pass")
}

// A one-off failed read of a sub-issue drops it from that pass's snapshots,
// so its ancestor's count may fall for that pass (#160's verify). It
// corrects itself on the next pass, which reads it again: the body is
// what it was, with no announcement, and the pass after writes nothing.
func TestDigestRunCountFlicker(t *testing.T) {
	f := digestRunFake(t)
	d := digestRunDaemon(t, f, digestRunOrigin(t), nil, nil)
	digestRunStep(t, f, d, "the first pass", digestRunFirst...)
	listed := f.State().Issues[11].Body
	f.AddComment(9, digestRunOwner, "Any news?") // 9, below 8 below 4, is read again
	f.AnswerOnce(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/9" }, http.StatusInternalServerError)
	got := digestRunFail(t, f, d, "#9")
	if items := digestRunItems(f.State().Issues[11].Body); len(got) > 1 || len(got) == 1 && (got[0] != "PATCH /issues/12" || items[0] != "#4 State error p2 3") {
		t.Errorf("writes %q, items %q on the failed read; want at most the edit that counts 4's sub-issues without 9", got, items)
	}
	from := len(f.Writes())
	iterate(t, d)
	if got := digestWrites(f, from); len(got) > 1 || f.State().Issues[11].Body != listed || digestRunNews(f, 12) != 1 {
		t.Errorf("writes %q, body %q once 9 is read; want at most one edit, back to %q, and no announcement", got, f.State().Issues[11].Body, listed)
	}
	digestRunStep(t, f, d, "an extra pass")
}

// digestRunMover stands between a client and the fake as GitHub does once
// an issue is transferred to another repo. Every request about the issue
// gets a 301 to its new home, /repositories/9/issues/<n>, which the fake
// serves as the issue's own path, so a redirected write, which Go follows
// with a GET, reads the transferred issue. The issue list leaves it out.
type digestRunMover struct {
	f          *githubtest.Fake
	mu         sync.Mutex
	moved      int      // the issue transferred, or 0 for none
	redirected []string // the writes about it given a 301, as "METHOD /path"
}

// digestRunKey is a key for the App's JWT, which the fake doesn't check.
var digestRunKey = sync.OnceValues(func() ([]byte, error) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("generate a key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
})

// digestRunMove starts a digestRunMover in front of f, and returns it with
// a client that talks to f through it.
func digestRunMove(t *testing.T, f *githubtest.Fake) (*digestRunMover, *github.Client) {
	t.Helper()
	target, err := url.Parse(f.URL())
	if err != nil {
		t.Fatal(err)
	}
	m := &digestRunMover{f: f}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.ModifyResponse = m.list
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		issue := fmt.Sprintf("/repos/o/r/issues/%d", m.issue())
		switch p := r.URL.Path; {
		case strings.HasPrefix(p, "/repositories/9/"):
			r.URL.Path = "/repos/o/r/" + strings.TrimPrefix(p, "/repositories/9/")
		case m.issue() != 0 && (p == issue || strings.HasPrefix(p, issue+"/")):
			if r.Method != http.MethodGet {
				m.mu.Lock()
				m.redirected = append(m.redirected, r.Method+" "+p)
				m.mu.Unlock()
			}
			http.Redirect(w, r, "/repositories/9/"+strings.TrimPrefix(p, "/repos/o/r/"), http.StatusMovedPermanently)
			return
		case m.issue() != 0 && p == "/repos/o/r/issues":
			r.Header.Del("If-None-Match") // a 304 would give the list cached with the issue in it
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	key, err := digestRunKey()
	if err != nil {
		t.Fatal(err)
	}
	c, err := github.New(github.Config{AppID: 1, InstallationID: 1, Key: key, BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return m, c
}

// issue is the issue transferred, or 0 for none.
func (m *digestRunMover) issue() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.moved
}

// move transfers issue n to elsewhere/r.
func (m *digestRunMover) move(n int) {
	m.f.Transfer(n, "elsewhere/r")
	m.mu.Lock()
	defer m.mu.Unlock()
	m.moved = n
}

// writes is the writes given a 301 so far.
func (m *digestRunMover) writes() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.redirected)
}

// list takes the transferred issue out of an answer to an issue list.
func (m *digestRunMover) list(resp *http.Response) error {
	n := m.issue()
	if n == 0 || resp.Request.Method != http.MethodGet || resp.Request.URL.Path != "/repos/o/r/issues" || resp.StatusCode != http.StatusOK {
		return nil
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read the issue list: %w", err)
	}
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("close the issue list: %w", err)
	}
	var issues []json.RawMessage
	if err := json.Unmarshal(raw, &issues); err != nil {
		return fmt.Errorf("decode the issue list: %w", err)
	}
	kept := []json.RawMessage{}
	for _, is := range issues {
		var number struct{ Number int }
		if err := json.Unmarshal(is, &number); err != nil {
			return fmt.Errorf("decode an issue: %w", err)
		}
		if number.Number != n {
			kept = append(kept, is)
		}
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return fmt.Errorf("encode the issue list: %w", err)
	}
	resp.Body, resp.ContentLength = io.NopCloser(bytes.NewReader(out)), int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("ETag")
	return nil
}

// digestRunRelisted reports whether a request from the from'th on lists
// every issue, with no since=.
func digestRunRelisted(f *githubtest.Fake, from int) bool {
	return slices.ContainsFunc(f.Requests()[from:], func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && r.Path == "/issues" && !strings.Contains(r.Query, "since=")
	})
}

// digestRunReplaced checks the passes after the digest, 12, was dropped as
// transferred: the first lists every issue again, finds no digest, and
// creates one, 13, which announces the open errors once. After that,
// nothing is written, and the redirected write, the one that found 12
// gone, is the only one.
func digestRunReplaced(t *testing.T, f *githubtest.Fake, d *daemon.Daemon, m *digestRunMover, redirected string, items []string) {
	t.Helper()
	reqs := len(f.Requests())
	digestRunStep(t, f, d, "the pass after the drop", "POST /issues", "POST /graphql", "PATCH /issues/13", "POST /issues/13/comments")
	if !digestRunRelisted(f, reqs) {
		t.Errorf("the pass after the drop didn't list every issue again: %+v", f.Requests()[reqs:])
	}
	for i := range 2 {
		digestRunStep(t, f, d, fmt.Sprintf("extra pass %d", i+1))
	}
	is := f.State().Issues
	if len(is) != 13 || !is[12].Pinned || digestRunNews(f, 13) != 1 || !strings.HasSuffix(digestComments(f, 13)[0], digestRunAnnounced) {
		t.Fatalf("issues %+v; want one new digest, 13, pinned, which announced the three errors once", is)
	}
	if got := digestRunItems(is[12].Body); !slices.Equal(got, items) {
		t.Errorf("13's items = %q; want %q", got, items)
	}
	if got := m.writes(); !slices.Equal(got, []string{redirected}) {
		t.Errorf("redirected writes = %q; want %q alone", got, redirected)
	}
}

// A transferred digest, end to end (#160's verify): its body edit gets a
// 301 to another repo, which the client follows with a GET, so the edit
// wrote nothing, and the digest is dropped. The next pass lists every
// issue again, where the transferred one isn't, so it creates one other
// digest, which announces once, and the pass after writes nothing.
func TestDigestRunTransferred(t *testing.T) {
	f := digestRunFake(t)
	m, c := digestRunMove(t, f)
	d := digestRunDaemon(t, f, digestRunOrigin(t), c, nil)
	digestRunStep(t, f, d, "the first pass", digestRunFirst...)
	m.move(12)
	answer := f.AddComment(1, digestRunOwner, "1a 2b") // the round goes, so the digest is edited
	if got := digestRunFail(t, f, d, "the digest is gone", "edit the body of the digest", "answered by GET /repositories/9/issues/12"); !slices.Equal(got,
		[]string{fmt.Sprintf("POST /issues/comments/%d/reactions", answer)}) {
		t.Errorf("writes = %q; want step 1's 👍 alone", got)
	}
	digestRunReplaced(t, f, d, m, "PATCH /repos/o/r/issues/12", digestRunWithout(1, ""))
}

// The revert of an untrusted close, on a digest transferred before it,
// marks it gone (#160's verify): the PATCH that would reopen it gets a
// 301, the digest is dropped, and the next pass replaces it, rather than
// failing the revert on every pass.
func TestDigestRunTransferredRevert(t *testing.T) {
	f := digestRunFake(t)
	m, c := digestRunMove(t, f)
	d := digestRunDaemon(t, f, digestRunOrigin(t), c, nil)
	digestRunStep(t, f, d, "the first pass", digestRunFirst...)
	f.CloseBy(12, "mallory")
	// The poll lists it closed; it's transferred before the revert.
	f.BeforeRequest(func(r githubtest.Request) bool { return r.Method == http.MethodGet && r.Path == "/issues/12/timeline" }, func() { m.move(12) })
	if got := digestRunFail(t, f, d, "the digest is gone", "the close of the digest, o/r#12: reopen it"); len(got) != 0 {
		t.Errorf("writes = %q; want none", got)
	}
	digestRunReplaced(t, f, d, m, "PATCH /repos/o/r/issues/12", digestRunRanked)
}
