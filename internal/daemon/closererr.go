package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// A closer question that keeps failing (SPEC §5.3, §6.4; #232, #233). How a
// close was made (closingCloser) is asked again on every pass while it
// fails with anything but FORBIDDEN or NOT_FOUND under node (#204). The
// Applier counts the passes on which each close's question failed
// (closerErrState), once a pass however often it's asked, with no answer
// between, as an answer is kept for the run, and the count bounds what a
// failure does:
//   - Judging a close nobody trusted made (closingAccepted, on a closed
//     issue, or closingCounts, for step 1's close triggers on an open one)
//     is the pass's error, as before, though it fails only that issue for
//     the pass, until the question has failed with an error under node
//     (github.CloserErrUnderNode) on loop.max_attempts passes. Then the
//     issue gets one state error, kind=closer, keyed by the closed event's
//     ID, pointing to the log for the error, which is GitHub's text
//     (closerErrReport, publicCause), and that failure stops being the
//     pass's error: a closed issue's close stays unjudged, with its stage
//     label as it is (closerErrClosed), and an open issue's close holds
//     nothing (closerErrStep1), its error posted by step 1's visit of the
//     issue (closerErrPost). The question is still asked once a pass, so
//     the close is judged as soon as GitHub answers, which ends the
//     error. On a closed issue, that question is all a pass asks while
//     the listing still shows the close the error is for: the timeline
//     isn't read (closingJudge). The listing shows the close by its
//     closed_at and the stage labels, so a reopen and a close again within
//     the same second, or a close again that keeps closed_at, isn't seen
//     while the question fails: the older close stays the one asked, and
//     its error stands, until GitHub answers or the listing changes (a gap
//     SPEC §5.3 states). On an open issue, the error also ends once step 1
//     no longer asks the question, as its close is no longer the one to
//     judge (closerErrUnasked). Any other failure, a failed
//     request or GraphQL failing as a whole, says nothing of the close
//     itself, as an outage would, so it stays the pass's error, without
//     bound, and counts no pass towards the state error.
//   - Closing an issue again after an untrusted reopen (closingCloseAgain)
//     waits for the question of the close the reopen undid until it has
//     failed, in any way, on closerErrRetries passes, writing nothing
//     meanwhile, so one transient failure doesn't change the state reason.
//     Only then does it close the issue with the closed stage's reason.
//
// The counts are in memory, as the closer answers are (Applier.closingBy):
// a restart forgets them, so each failing question starts from none again.
// A state error posted already is found again, not posted twice, and
// stands at once: on an open issue, from its comment; on a closed one, from
// the one read of the issue a run makes at the question's first failure
// under node (closerErrClosed), so the digest keeps listing it across a
// restart. A closed issue that doesn't carry it yet is read again only
// when the count reaches the bound, to post it.
//
// A closed issue's state error can't be open the way an open issue's is
// (openErrors): no step 1 visits it, so no 👍 is read, and nothing is
// assigned on it (assign.go). The digest still lists it, from memory
// (Daemon.closerErrOn), while the close stays unjudged. Once the issue is
// reopened, as to settle it by closing it again, which its comment
// advises, the error is open there, and the digest lists it as any open
// error, but it still assigns nobody (closerErrPostedClosed): so the close
// again leaves nobody assigned to the closed issue for it.

// closerErrKind is the kind of the state error for a closer question that
// keeps failing.
const closerErrKind = "closer"

// closerErrRetries is how many passes the question of the close an
// untrusted reopen undid may fail on before the close again takes the
// closed stage's reason (#233): the first failure is tried again next pass.
const closerErrRetries = 2

// closerErrState is the Applier's memory of failed closer questions, for
// the run.
type closerErrState struct {
	pass   int                       // the loop's pass, as Daemon.Iterate counts them (closerErrBegin)
	max    int                       // loop.max_attempts, as of the pass
	failed map[string]closerErrTally // failing questions, by the close's event node ID
	done   map[string]bool           // closed event IDs whose question was answered
}

// closerErrTally is how many passes a close's question has failed on with
// no answer between, and the last of them: in any way, and with an error
// under node (github.CloserErrUnderNode).
type closerErrTally struct {
	passes, last int
	node, atNode int
	cause        error // the last error under node, for the state error
	looked       bool  // closerErrClosed read the closed issue for its state error, and found none
}

// closerErrBegin starts a pass for the closer counts: the next pass, with
// loop.max_attempts as its bound, at least 1.
func (d *Daemon) closerErrBegin() {
	c := &d.Step.closerErr
	c.pass++
	limit := config.Defaults().Loop.MaxAttempts
	if d.Config != nil {
		limit = d.Config.Loop.MaxAttempts
	}
	c.max = max(1, int(limit))
}

// closerErrFail counts a failure, err, of the question of cl's closer on
// this pass: once a pass, however often it's asked, and as a pass with an
// error under node too if err is one.
func (a *Applier) closerErrFail(cl github.Closing, err error) {
	c := &a.closerErr
	if c.failed == nil {
		c.failed = map[string]closerErrTally{}
	}
	t := c.failed[cl.NodeID]
	if t.passes == 0 || t.last != c.pass {
		t.passes, t.last = t.passes+1, c.pass
	}
	if github.CloserErrUnderNode(err) {
		if t.node == 0 || t.atNode != c.pass {
			t.node, t.atNode = t.node+1, c.pass
		}
		t.cause = err
	}
	c.failed[cl.NodeID] = t
}

// closerErrAnswer forgets cl's failures, as its question was answered, and
// records the answer, which ends its state error (closerErrAnswered).
func (a *Applier) closerErrAnswer(cl github.Closing) {
	c := &a.closerErr
	delete(c.failed, cl.NodeID)
	if c.done == nil {
		c.done = map[string]bool{}
	}
	c.done[strconv.FormatInt(cl.ID, 10)] = true
}

// closerErrPasses is how many passes cl's question has failed on, with no
// answer between.
func (a *Applier) closerErrPasses(cl github.Closing) int {
	return a.closerErr.failed[cl.NodeID].passes
}

// closerErrNodePasses is how many passes cl's question has failed on with
// an error under node, with no answer between.
func (a *Applier) closerErrNodePasses(cl github.Closing) int {
	return a.closerErr.failed[cl.NodeID].node
}

// closerErrAnswered reports whether the closer question of the closed
// event keyed key, its ID, was answered while the daemon runs: the closer
// state error keyed key has ended (openErrors).
func (a *Applier) closerErrAnswered(key string) bool {
	return a.closerErr.done[key]
}

// closerErrEnded returns what ends a closer state error on s, an open issue
// as step 1 left it, with dirty as for state.Derive and trust as step 1's
// look for triggers asks it (openErrors): given the error's key, whether
// the question of its close was answered while the daemon runs
// (closerErrAnswered), or step 1 no longer asks it (closerErrUnasked).
func (a *Applier) closerErrEnded(s *github.Snapshot, dirty func(int64) bool, trust github.Trust) func(key string) bool {
	return func(key string) bool {
		return a.closerErrAnswered(key) || closerErrUnasked(s, a.AppSlug, dirty, trust, key)
	}
}

// closerErrUnasked reports whether step 1 no longer asks how the close of
// s keyed key was made, as its close is no longer the one to judge: by
// what closingCounts judges before it asks anyone, s.Closings lists it,
// and it's before the discussion floor (discFloor), as after a move out of
// held, or a marker records it, or s has no floor, or trust can't name a
// write collaborator who isn't trusted (discKnowsWriters), as with no
// CODEOWNERS file, so discFind looks for no trigger. That needs no
// request. A close s.Closings doesn't list, as on a snapshot CheckLabels
// hasn't checked, is still asked.
func closerErrUnasked(s *github.Snapshot, appSlug string, dirty func(int64) bool, trust github.Trust, key string) bool {
	i := slices.IndexFunc(s.Closings, func(cl github.Closing) bool { return !cl.Reopen && strconv.FormatInt(cl.ID, 10) == key })
	if i < 0 {
		return false
	}
	b, ok := discFloor(s, appSlug, dirty)
	if !ok || !discKnowsWriters(trust) {
		return true
	}
	open, recorded := discOpenAfter(b, discRecords(s, appSlug, dirty))(key, s.Closings[i].At, 0)
	return !open || recorded
}

// closerErrPostedClosed reports whether e, an open closer state error on
// s, an open issue, was posted while s was closed (closerErrClosed), not
// by step 1 (closerErrPost): its comment is from before the first reopen
// s.Closings lists after the close e is keyed by. Such an error assigns
// nobody (assign.go), as on the closed issue, since reopening the issue
// and closing it again is how its comment says to settle it. One posted
// in the same second as that reopen, or with no reopen or close listed,
// as on a snapshot CheckLabels hasn't checked, or in the body, assigns
// as any open error does. It makes no request.
func closerErrPostedClosed(s *github.Snapshot, e openErr) bool {
	i := slices.IndexFunc(s.Closings, func(cl github.Closing) bool { return !cl.Reopen && strconv.FormatInt(cl.ID, 10) == e.Key })
	c := slices.IndexFunc(s.Comments, func(c github.Comment) bool { return c.ID == e.Comment })
	if e.Kind != closerErrKind || i < 0 || c < 0 {
		return false
	}
	for _, cl := range s.Closings[i+1:] {
		if cl.Reopen {
			return s.Comments[c].CreatedAt.Before(cl.At)
		}
	}
	return false
}

// closerErrReport returns the ID of the comment on s that carries the
// closer state error for cl, posting it first with text if s has none,
// and none was acknowledged, and 0 for an acknowledged one whose comment
// is gone. The question's last error under node is GitHub's text, so the
// comment says only that the daemon's log has it, as it quotes no cause
// the daemon didn't write (publicCause), and it's logged in full here.
func (a *Applier) closerErrReport(ctx context.Context, s *github.Snapshot, dirty func(int64) bool, cl github.Closing, text string) (int64, error) {
	k := state.ErrorKey{Kind: closerErrKind, Key: strconv.FormatInt(cl.ID, 10)}
	if is := state.Derive(s, a.AppSlug, dirty); is.Reported(k.Kind, k.Key) {
		return is.Errors[k], nil
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": k.Kind, "key": k.Key}})
	if err != nil {
		return 0, fmt.Errorf("report the closer of close %d: %w", cl.ID, err)
	}
	t := a.closerErr.failed[cl.NodeID]
	if t.cause != nil {
		text += publicCauseText("question", publicCause(t.cause))
	}
	if err := a.comment(ctx, s, join(github.Escape(text), marker)); err != nil {
		return 0, fmt.Errorf("report the closer of close %d: %w", cl.ID, err)
	}
	logr.Errorf("state error: %s/%s#%d: how its close %d was made is unknown after %d passes: %v",
		a.Owner, a.Repo, s.Number, cl.ID, t.node, t.cause)
	return s.Comments[len(s.Comments)-1].ID, nil
}

// closerErrStep1 judges cause, closingCounts' error for cl, a close of s,
// an open issue, in step 1, with no request. It reports true when cause
// is an error under node (github.CloserErrUnderNode) and the closer state
// error stands for cl: s carries it already, as after a restart, or the
// question has now failed so on loop.max_attempts passes, so the visit
// posts it (closerErrPost). cl then holds nothing while the question
// fails. Otherwise cause stays the pass's error. It writes nothing, as it
// may judge a snapshot other than the visit's own, such as a copy a
// command's check made.
func (a *Applier) closerErrStep1(s *github.Snapshot, dirty func(int64) bool, cl github.Closing, cause error) bool {
	if !github.CloserErrUnderNode(cause) {
		return false
	}
	if state.Derive(s, a.AppSlug, dirty).Reported(closerErrKind, strconv.FormatInt(cl.ID, 10)) || a.closerErrNodePasses(cl) >= a.closerErr.max {
		logr.Debugf("%s/%s#%d: its close %d holds nothing while its state error stands: %v", a.Owner, a.Repo, s.Number, cl.ID, cause)
		return true
	}
	return false
}

// closerErrPost posts on s, an open issue step 1 just visited, with its
// dirty content d, the closer state error of each close of s whose
// question failed with an error under node on this pass, as on
// loop.max_attempts passes, and that s doesn't carry yet (closerErrStep1).
// It reads nothing, so an unchanged pass costs no request.
func (a *Applier) closerErrPost(ctx context.Context, s *github.Snapshot, d Dirt) error {
	for _, cl := range s.Closings {
		t, ok := a.closerErr.failed[cl.NodeID]
		if cl.Reopen || cl.NodeID == "" || !ok || t.atNode != a.closerErr.pass || t.node < a.closerErr.max {
			continue
		}
		text := fmt.Sprintf("%s closed this issue, and it was reopened, but I can't tell whether that close was a merge: "+
			"asking GitHub how it was made failed on %d passes. Until GitHub answers, that close holds nothing here. "+
			"I ask again on every pass, and this error ends once it answers.", discLogin(cl.By.Login), t.node)
		if _, err := a.closerErrReport(ctx, s, d.Has, cl, text); err != nil {
			return fmt.Errorf("the closer state errors of %s/%s#%d: %w", a.Owner, a.Repo, s.Number, err)
		}
	}
	return nil
}

// closerErrClosed judges cause, closingAccepted's error for cl, the newest
// close of is, a closed issue as listed. It reports true when cause is an
// error under node (github.CloserErrUnderNode) and the closer state error
// stands for cl: the issue carries it already, as after a restart, or the
// question has now failed so on loop.max_attempts passes, and the error is
// posted on the issue. It's then listed on the digest (closerErrOn), and the close stays
// unjudged, and due, with no error. The issue is read at the question's
// first failure under node in a run, to find the error, and again when the
// count reaches the bound, if it wasn't found; once the error stands, a
// pass makes no request for it beyond the question (closingJudge). An
// issue found open again gets nothing: the poll forgets its close.
func (d *Daemon) closerErrClosed(ctx context.Context, is *gogithub.Issue, cl github.Closing, cause error) (bool, error) {
	a, n := d.Step.Applier, is.GetNumber()
	if !github.CloserErrUnderNode(cause) {
		return false, nil
	}
	key := strconv.FormatInt(cl.ID, 10)
	if on, ok := d.closerErrOn[n]; ok && on.key == key {
		logr.Debugf("%s/%s#%d: its close %d stays unjudged while its state error stands: %v", a.Owner, a.Repo, n, cl.ID, cause)
		on.listed = closingListedOf(is) // its stage labels changed: as listed now, for closingJudge
		d.closerErrOn[n] = on
		return true, nil
	}
	t := a.closerErr.failed[cl.NodeID]
	due := t.node >= a.closerErr.max
	if !due && t.looked {
		return false, nil
	}
	wrap := func(err error) error { return fmt.Errorf("the closer state error of close %d: %w", cl.ID, err) }
	s, err := a.Client.Snapshot(ctx, a.Owner, a.Repo, n)
	if err != nil {
		return false, wrap(err)
	}
	if !s.Closed {
		return false, nil
	}
	dirt, err := a.Dirty(ctx, s, d.Step.Trust)
	if err != nil {
		return false, wrap(err)
	}
	if !due && !state.Derive(s, a.AppSlug, dirt.Has).Reported(closerErrKind, key) {
		t.looked = true
		a.closerErr.failed[cl.NodeID] = t
		return false, nil
	}
	text := fmt.Sprintf("I can't tell how this issue was closed: asking GitHub whether %s's close was a merge failed on %d passes. "+
		"So I've neither accepted nor reverted it, and its stage label stays as it is. "+
		"I ask again on every pass, and judge the close once GitHub answers. "+
		"A 👍 here isn't read while the issue is closed. To settle it yourself, reopen the issue and close it again: "+
		"a trusted user's close stands without asking GitHub how it was made.", discLogin(cl.By.Login), t.node)
	id, err := a.closerErrReport(ctx, s, dirt.Has, cl, text)
	if err != nil {
		return false, wrap(err)
	}
	if d.closerErrOn == nil {
		d.closerErrOn = map[int]closerErrClosedOn{}
	}
	e := openErr{ErrorKey: state.ErrorKey{Kind: closerErrKind, Key: key}, Comment: id}
	d.closerErrOn[n] = closerErrClosedOn{key: key, cl: cl, listed: closingListedOf(is),
		item: waitItem{Issue: n, Kind: waitError, Key: fmt.Sprintf("%s-%d-%s-%s", waitKeyPrefix[waitError], n, e.Kind, e.Key),
			Title: s.Title, Lines: closerErrLines(s, e), Link: waitLink(a.Owner, a.Repo, n, id), Priority: priority(s),
			Created: s.CreatedAt, Comment: id}}
	return true, nil
}

// closerErrClosedOn is the closer state error that stands on a closed
// issue (closerErrClosed): its key, its close, cl, which closingJudge asks
// about with no timeline read while the listing shows the issue as listed,
// the issue as last listed while it stood, and its digest item, keyed as
// waitFind keys any state error.
type closerErrClosedOn struct {
	key    string
	cl     github.Closing
	listed closingListed
	item   waitItem
}

// closerErrLines says what e, the closer state error on s, a closed issue,
// is, as waitErrorLines does, but for how it ends: a 👍 isn't read on a
// closed issue.
func closerErrLines(s *github.Snapshot, e openErr) []waitLine {
	lines := waitErrorLines(s, e)
	if e.Comment == state.BodyID {
		return lines[:1]
	}
	lines[len(lines)-1] = waitLine{Text: "The issue is closed, so a 👍 won't acknowledge it: it ends once GitHub says how the issue was closed."}
	return lines
}

// closerErrJudged drops closed issue n's closer state error from the
// digest unless it's keyed by cl, its newest close whose question failed:
// its close was judged, or another close is now the one to judge. A nil
// cl drops it in any case.
func (d *Daemon) closerErrJudged(n int, cl *github.Closing) {
	on, ok := d.closerErrOn[n]
	if ok && (cl == nil || on.key != strconv.FormatInt(cl.ID, 10)) {
		delete(d.closerErrOn, n)
	}
}

// closerErrDigest returns the closed issues' closer state errors, by
// issue, for the digest (digestCacheRanked). None was acknowledged: each is
// keyed by the close that closed its issue, and posted after it, while no
// 👍 is read.
func (d *Daemon) closerErrDigest() []waitItem {
	var items []waitItem
	for _, n := range slices.Sorted(maps.Keys(d.closerErrOn)) {
		items = append(items, d.closerErrOn[n].item)
	}
	return items
}
