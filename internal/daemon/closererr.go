package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"

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
//     (github.CloserErrUnderNode) on loop.max_attempts passes. Then the issue gets one state error, kind=closer, keyed by
//     the closed event's ID, naming the error (closerErrReport), and that
//     failure stops being the pass's error: a closed issue's close stays
//     unjudged, with its stage label as it is (closerErrClosed), and an
//     open issue's close holds nothing (closerErrStep1), its error posted
//     by step 1's visit of the issue (closerErrPost). The question is
//     still asked once a pass, so the close is judged as soon as GitHub
//     answers, which ends the error. Any other failure, a failed request
//     or GraphQL failing as a whole, says nothing of the close itself, as
//     an outage would, so it stays the pass's error, without bound, and
//     counts no pass towards the state error.
//   - Closing an issue again after an untrusted reopen (closingCloseAgain)
//     waits for the question of the close the reopen undid until it has
//     failed, in any way, on closerErrRetries passes, writing nothing
//     meanwhile, so one transient failure doesn't change the state reason.
//     Only then does it close the issue with the closed stage's reason.
//
// The counts are in memory, as the closer answers are (Applier.closingBy):
// a restart forgets them, so each failing question starts from none again.
// A state error posted already is found again, not posted twice: on an
// open issue it stands at once, from its comment; on a closed one, whose
// comment is read only when the count reaches the bound, it's found then.
// Until then, after a restart, the failure is the pass's error again.
//
// A closed issue's state error can't be open the way an open issue's is
// (openErrors): no step 1 visits it, so no 👍 is read, and nothing is
// assigned on it (assign.go). The digest still lists it, from memory
// (Daemon.closerErrOn), while the close stays unjudged.

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

// closerErrReport returns the ID of the comment on s that carries the
// closer state error for cl, posting it first with text if s has none,
// and none was acknowledged, and 0 for an acknowledged one whose comment
// is gone. The question's last error under node isn't the App's text:
// it's fenced and escaped, and cut as an attempt's cause is
// (lastFailure).
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
		text += "\n\nThe last question failed:\n\n" + ivFenced(lastFailure(t.cause))
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
// close of n, a closed issue. It reports true when cause is an error under
// node (github.CloserErrUnderNode) and the closer state error stands for
// cl: the question has failed so on loop.max_attempts passes, and the
// error is posted on n, or found there, and listed on the digest
// (closerErrOn). The close then stays unjudged, and due, with no error.
// The issue is read once, when the count reaches the bound; after that, a
// pass makes no request for it beyond the question. An issue found open
// again gets nothing: the poll forgets its close.
func (d *Daemon) closerErrClosed(ctx context.Context, n int, cl github.Closing, cause error) (bool, error) {
	a := d.Step.Applier
	if !github.CloserErrUnderNode(cause) || a.closerErrNodePasses(cl) < a.closerErr.max {
		return false, nil
	}
	key := strconv.FormatInt(cl.ID, 10)
	if on, ok := d.closerErrOn[n]; ok && on.key == key {
		logr.Debugf("%s/%s#%d: its close %d stays unjudged while its state error stands: %v", a.Owner, a.Repo, n, cl.ID, cause)
		return true, nil
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
	text := fmt.Sprintf("I can't tell how this issue was closed: asking GitHub whether %s's close was a merge failed on %d passes. "+
		"So I've neither accepted nor reverted it, and its stage label stays as it is. "+
		"I ask again on every pass, and judge the close once GitHub answers.", discLogin(cl.By.Login), a.closerErrNodePasses(cl))
	id, err := a.closerErrReport(ctx, s, dirt.Has, cl, text)
	if err != nil {
		return false, wrap(err)
	}
	if d.closerErrOn == nil {
		d.closerErrOn = map[int]closerErrClosedOn{}
	}
	e := openErr{ErrorKey: state.ErrorKey{Kind: closerErrKind, Key: key}, Comment: id}
	d.closerErrOn[n] = closerErrClosedOn{key: key, acked: state.Derive(s, a.AppSlug, dirt.Has).Acked(e.Kind, e.Key),
		item: waitItem{Issue: n, Kind: waitError, Key: fmt.Sprintf("%s-%d-%s-%s", waitKeyPrefix[waitError], n, e.Kind, e.Key),
			Title: s.Title, Lines: closerErrLines(s, e), Link: waitLink(a.Owner, a.Repo, n, id), Priority: priority(s),
			Created: s.CreatedAt, Comment: id}}
	return true, nil
}

// closerErrClosedOn is the closer state error that stands on a closed
// issue (closerErrClosed): its key, whether a trusted 👍 acknowledged it
// before the issue closed, which keeps it off the digest, and its digest
// item, keyed as waitFind keys any state error.
type closerErrClosedOn struct {
	key   string
	acked bool
	item  waitItem
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

// closerErrDigest returns the closed issues' closer state errors that no
// 👍 acknowledged, by issue, for the digest (digestCacheRanked).
func (d *Daemon) closerErrDigest() []waitItem {
	var items []waitItem
	for _, n := range slices.Sorted(maps.Keys(d.closerErrOn)) {
		if on := d.closerErrOn[n]; !on.acked {
			items = append(items, on.item)
		}
	}
	return items
}
