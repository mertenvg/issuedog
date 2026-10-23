package daemon

import (
	"context"
	"fmt"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// An issue the digest's title rule would take, whose edit history failed
// to read, can't be judged: it may be the digest, whose marker was taken
// out, or an App task that lost its origin and was renamed (digestTake).
// So it's held, out of step 1 and Work, and no digest is created while it
// is, as a new one would be a second (#305). Each pass reads its history
// again, till digestHistoryStrikes reads have failed on this start. Then
// it's a state error, kind=digesthistory, reported once (SPEC §5.3), and
// the issue stays held, with no digest created, while its history is read
// once every digestHistoryStrikes passes, so an outage longer than the
// strikes ends on its own, and each failed read is logged as an error
// (#437). A restart reads it again at once, and a rename, or the identity
// marker put back, lets it go. No step 1 visits a held issue, so a 👍 on
// the error isn't read, and the digest, if there's one, doesn't list it.
// It ends once the issue is judged: step 1 visits only an issue the title
// rule has judged, so it's never open there (openErrors).

// digestHistoryErrKind is the kind of the state error for an issue the
// digest's title rule can't judge, as its edit history failed to read.
const digestHistoryErrKind = "digesthistory"

// digestHistoryStrikes is how many passes of a start may fail to read an
// issue's edit history for the title rule before it's a state error, and
// is read only once every that many passes: loop.max_attempts, as for a
// closer question (closerErrBegin), at least 1.
func (d *Daemon) digestHistoryStrikes() int {
	limit := config.Defaults().Loop.MaxAttempts
	if d.Config != nil {
		limit = d.Config.Loop.MaxAttempts
	}
	return max(1, int(limit))
}

// digestHistoryFail is what a start knows of the failed reads of an issue's
// edit history for the title rule: how many reads failed, in a row, the
// last one's error, for the state error, and the title rule's pass
// (digestState.pass) to read it on next.
type digestHistoryFail struct {
	fails int
	cause error
	next  int
}

// digestHistoryPosted is an App comment's digesthistory state error: the
// issue it's on, and its key.
type digestHistoryPosted struct {
	issue int
	key   string
}

// historyFailed counts a failed read, err, of the edit histories of issues
// nums, on this pass: each is read again on the next, till strikes reads
// have failed, and then once every strikes passes.
func (g *digestState) historyFailed(nums []int, err error, strikes int) {
	if g.history == nil {
		g.history = map[int]digestHistoryFail{}
	}
	for _, n := range nums {
		h := g.history[n]
		h.fails, h.cause, h.next = h.fails+1, err, g.pass+1
		if h.fails >= strikes {
			h.next = g.pass + strikes
		}
		g.history[n] = h
	}
}

// historyComment records comment id on issue n as one with a digesthistory
// state error if markers, the App's in it, have one that isn't an ack, and
// forgets it otherwise, as an edit may have taken it out.
func (g *digestState) historyComment(id int64, n int, markers []github.Marker) {
	for _, m := range markers {
		if m.Kind == "error" && m.Fields["kind"] == digestHistoryErrKind && m.Fields["ack"] == "" {
			if g.historyErrs == nil {
				g.historyErrs = map[int64]digestHistoryPosted{}
			}
			g.historyErrs[id] = digestHistoryPosted{issue: n, key: m.Fields["key"]}
			return
		}
	}
	delete(g.historyErrs, id)
}

// historyPostedOn reports whether an App comment on issue n carries the
// digesthistory state error keyed key, as the poll listed them or this
// daemon posted it.
func (g *digestState) historyPostedOn(n int, key string) bool {
	for _, p := range g.historyErrs {
		if p.issue == n && p.key == key {
			return true
		}
	}
	return false
}

// digestHistoryHold keeps is, an issue the title rule holds as listed, whose
// edit history no pass has read yet, out of step 1 and Work: it drops its
// snapshot and inputs, if it had any, and its items, so the digest doesn't
// list what its last visit found, and an open one is marked to be read
// again, which Iterate does once it's judged, and not before. The poll marks it
// so too, unless its snapshot was read after the change that made the
// title rule hold it, as when it was renamed between a poll and its read.
// Once the read has failed digestHistoryStrikes times, it reports the
// state error.
//
// It keeps the issue's assignment state (Daemon.assignManaged): assignment
// writes only to an issue with a snapshot, so it can't touch a held one,
// and keeping it lets the first pass after the issue is let go take off
// the notify users if the issue isn't in the pipeline then.
func (d *Daemon) digestHistoryHold(ctx context.Context, is *gogithub.Issue) {
	n := is.GetNumber()
	delete(d.snaps, n)
	delete(d.inputs, n)
	delete(d.digestCache, n)
	if is.GetState() == "open" {
		d.stale[n] = true
	}
	if d.digest.history[n].fails >= d.digestHistoryStrikes() {
		d.digestHistoryReport(ctx, is)
	}
}

// digestHistoryGone lets go of issue n, held by the title rule, which err,
// a request's, found gone, deleted or transferred, so a digest can be
// created.
func (d *Daemon) digestHistoryGone(n int, err error) {
	a, g := d.Step.Applier, &d.digest
	logr.Infof("%s/%s#%d, held as the digest's title rule couldn't judge it, is gone, deleted or transferred, so it's held no more: %v",
		a.Owner, a.Repo, n, err)
	delete(g.titled, n)
	delete(g.history, n)
	delete(d.stale, n)
}

// digestHistoryReport posts the digesthistory state error on is, keyed by
// the hash of its body as listed (state.BodyHash, as a revision records
// one), so the same body is reported once across passes and restarts, and
// an edit is a new occurrence. It posts none if an App comment on is
// carries it already, as this daemon posted it, or the poll, which lists
// every comment on a start, found it. It's logged as an error when posted,
// and at debug level after, so a pass that finds it writes nothing. A failed
// post is logged, and tried again on the next pass; one that finds is gone,
// deleted or transferred, drops it, so a digest can be created. GitHub
// answers a POST about a transferred issue with a 301, which Go follows
// with a GET of the comments in the issue's new repo: a post answered from
// anywhere but this repo's path posted nothing, and is a transfer
// (digestMoved), as for the digest's own comments (digestAnnounce). A held
// issue has no snapshot, so transferUnlistedCheck never finds it moved.
func (d *Daemon) digestHistoryReport(ctx context.Context, is *gogithub.Issue) {
	a, g := d.Step.Applier, &d.digest
	n := is.GetNumber()
	h := g.history[n]
	key := state.BodyHash(is.GetBody())
	desc := fmt.Sprintf("%s/%s#%d, titled %q, can't be judged the digest or not, as its edit history failed to read on %d passes: "+
		"it's held out of the pipeline, and no digest is created, till a read works, tried once every %d passes and on a restart, keyed %s",
		a.Owner, a.Repo, n, digestTitle, h.fails, d.digestHistoryStrikes(), key)
	if g.historyPostedOn(n, key) {
		logr.Debugf("state error, reported already: %s", desc)
		return
	}
	marker, err := github.FormatMarker(github.Marker{Kind: "error", Fields: map[string]string{"kind": digestHistoryErrKind, "key": key}})
	if err != nil {
		logr.Errorf("report %s/%s#%d's edit history that failed to read: %v", a.Owner, a.Repo, n, err)
		return
	}
	c, resp, err := a.Client.Issues.CreateComment(ctx, a.Owner, a.Repo, n, gogithub.IssueCommentRequest{Body: join(digestHistoryText(h), marker)})
	if moved := digestMoved(a, resp, nil); moved != nil {
		err = moved // a redirected POST, a GET of the comments, posted nothing, whether that decoded or not
	}
	if err != nil {
		err = fmt.Errorf("comment on %s/%s#%d: %w", a.Owner, a.Repo, n, err)
	}
	switch {
	case gone(err):
		d.digestHistoryGone(n, err)
	case err != nil:
		logr.Errorf("report %s/%s#%d's edit history that failed to read, which is tried again next pass: %v", a.Owner, a.Repo, n, err)
	default:
		g.historyComment(c.GetID(), n, []github.Marker{{Kind: "error", Fields: map[string]string{"kind": digestHistoryErrKind, "key": key}}})
		logr.Errorf("state error: %s", desc)
	}
}

// digestHistoryText says that h's issue can't be judged the digest or not,
// as its edit history failed to read on h.fails passes, so it's kept out
// of the pipeline and no digest is created, and what ends that. The last
// read's error is GitHub's text, so it says only that the daemon's log
// has it, as the title rule logged it (digestTake), since it quotes no
// cause the daemon didn't write (publicCause). The rest is the App's own,
// with no marker, so it's escaped once, there.
func digestHistoryText(h digestHistoryFail) string {
	text := fmt.Sprintf("I can't tell whether this issue is the digest. It's mine, titled `%s`, and its body has no `origin` marker: "+
		"it may be the digest with its identity marker taken out, or a task of mine that lost its `origin` and was renamed. "+
		"Only its first version shows which, and reading its edit history failed on %d passes in a row. "+
		"So I keep it out of the pipeline, and create no digest while I can't tell, so there are never two: "+
		"with no other digest, this repo has none till then. "+
		"I keep reading its history, less often now, so this ends on its own once a read works, and I read it again when I restart. "+
		"If this issue isn't the digest, rename it, and I'll take it as any other issue; "+
		"if it is, putting the digest's identity marker back in its body makes it the digest again. "+
		"A 👍 on this comment changes nothing: the error ends once I can tell.", digestTitle, h.fails)
	if h.cause != nil {
		text += publicCauseText("read", publicCause(h.cause))
	}
	return text
}
