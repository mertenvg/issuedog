package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"
)

// Held issues no longer in the repo (#237, SPEC §8.1). A transferred
// issue drops out of the repo's listings, and nothing about it changes
// there, so no since= poll lists it again, and a deleted one is the same.
// The daemon would go on holding such an issue: Work would get it on
// every pass, and its items would stay in the digest, and its assignees
// kept, until a restart. Only a visited issue, or a stage run's fresh
// read, would find it gone. The repo's listings show no trace of a
// transfer, and what does, the issues webhook's transferred action or
// the TransferredEvent on the issue in its new repo, needs a webhook,
// which the daemon doesn't take, or a request on every pass. So the
// cheapest sign is the open issues listed whole, which the daemon makes
// once in a while.

// transferUnlistedEvery is how often, by GitHub's clock, the daemon lists
// every open issue to find the ones it holds that aren't there any more
// (transferUnlistedCheck).
const transferUnlistedEvery = 10 * time.Minute

// transferUnlistedCheck lists every open issue, once transferUnlistedEvery
// has passed by GitHub's clock since it last tried, and has each issue the
// daemon holds a snapshot of that the listing leaves out read again
// (reread). Being left out counts as the first of the two reads that drop
// a gone issue (Iterate), unless this pass reads the issue anyway: this
// pass's poll listed it, as it lists one that changed, which it marks
// stale, and the one at its cursor, or it's stale already, as for a new
// comment, a label left out that fell due, or a failed step 1. So the
// read, this pass, drops a deleted or transferred issue at once: from
// Work, the digest's items and assignment, while one the pass reads
// anyway needs two reads that find it gone, as ever. One that is there
// after all, such as one closed since the poll, is kept, or left by the
// next poll.
//
// The listing brings back, too, each open issue it has that the daemon
// holds nothing of, neither a snapshot nor a read to come, such as a live
// issue a gap in a listing and a wrong 404 dropped: it's marked stale, so
// this pass reads it and runs step 1 on it, trust checks and all, as on
// any issue read (SPEC §6.2), and it's held again. That leaves out a pull
// request and the digest, which are never in the pipeline, and an issue
// updated at or after the poll's cursor, which the next poll lists and
// judges as it judges every issue, such as one opened since the poll, or
// one reopened, which is the only way a closed one gets listed open.
// Every other issue the listing has is held, or the digest, or was
// dropped, as a start's first poll lists every issue, and each later one
// every issue that changed. One brought back that the digest's title rule
// or identity marker may make a digest (digestState.listed), such as one
// the title rule held that two answers wrongly found deleted
// (digestHistoryGone), may be the digest, so it's held for digestTake, as
// when a poll lists it: the next pass judges it, and reads it, and runs
// step 1 on it, only once it's judged no digest.
//
// It brings each listed issue's count of the open issues it blocks up to
// date too (transferUnlistedBlocking), from the summary the listing gives
// anyway. An issue the listing has and the daemon holds is read no more
// than before, and a pass between two listings makes no request here. A
// start's first pass lists nothing, as its poll lists every issue and the
// daemon holds none before it, and nor does a pass that holds none, unless
// an issue was dropped since the last listing that worked
// (transferUnlistedDrop): so a repo whose only issue wrong answers dropped
// gets it back too. A failed listing is the pass's error, and the next
// listing waits transferUnlistedEvery from it, as from a good one, so a
// listing that keeps failing is tried once every 10 minutes, not on every
// pass.
func (d *Daemon) transferUnlistedCheck(ctx context.Context) error {
	st := d.Step
	now := st.Client.Now()
	switch {
	case d.transferUnlistedAt.IsZero():
		d.transferUnlistedAt = now
		return nil
	case (len(d.snaps) == 0 && len(d.transferUnlistedDropped) == 0) || now.Sub(d.transferUnlistedAt) < transferUnlistedEvery:
		return nil
	}
	d.transferUnlistedAt = now // a failed listing waits as long as a good one
	issues, err := st.Client.TransferUnlistedOpen(ctx, st.Owner, st.Repo)
	if err != nil {
		return fmt.Errorf("the check for held issues no longer listed, tried again in %s: %w", transferUnlistedEvery, err)
	}
	d.transferUnlistedDropped = nil
	listed := make(map[int]bool, len(issues))
	for _, is := range issues {
		n := is.GetNumber()
		listed[n] = true
		d.transferUnlistedBlocking(is)
		if d.snaps[n] != nil || d.stale[n] || is.IsPullRequest() || d.digest.has(n) || !is.GetUpdatedAt().Before(d.issuesSince) {
			continue
		}
		d.stale[n] = true
		d.digest.listed(st.AppSlug, is) // records no digest, as n isn't one: it holds is in titled if a digest rule may take it
		if _, held := d.digest.titled[n]; held {
			logr.Infof("%s/%s#%d is listed open, but the daemon holds nothing of it, as when wrong answers dropped it as gone, "+
				"and it may be the digest, so it's read again once its edit history shows it's no digest", st.Owner, st.Repo, n)
			continue
		}
		logr.Infof("%s/%s#%d is listed open, but the daemon holds nothing of it, as when wrong answers dropped it as gone, so it's read again", st.Owner, st.Repo, n)
	}
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		switch {
		case listed[n]:
		case d.transferUnlistedPolled[n] || d.stale[n]:
			logr.Infof("%s/%s#%d isn't listed open any more, though this pass's poll listed it, or reads it anyway, so it's read again, and dropped if two reads find it gone",
				st.Owner, st.Repo, n)
			d.reread(n, false)
		default:
			logr.Infof("%s/%s#%d isn't listed open any more, so it's read again, and dropped if it's gone, deleted or transferred", st.Owner, st.Repo, n)
			d.reread(n, true)
		}
	}
	return nil
}

// transferUnlistedBlocking brings up to date, from listed issue is, how
// many open issues it blocks, which the digest ranks by (SPEC §9.4, The
// order): its snapshot's Blocking, if one is held, and what its kept
// items rank by while none is, as when its read fails (digestKeptList).
// The listing gives each issue's issue_dependencies_summary, so it costs
// no request. A link's write, or a blocked issue's close, may not move the
// blocker's updated_at, so no poll lists the blocker, and its count would
// stay as its last read had it: this way it catches up by the next
// listing. A snapshot listed with no summary keeps the count it has.
func (d *Daemon) transferUnlistedBlocking(is *gogithub.Issue) {
	if s, sum := d.snaps[is.GetNumber()], is.GetIssueDependenciesSummary(); s != nil && sum != nil {
		s.Blocking = sum.GetBlocking()
	}
	d.digestKeptList(is)
}

// transferUnlistedDrop records issue n as dropped as gone, so the next
// listing is made even if the daemon holds no issue then
// (transferUnlistedCheck).
func (d *Daemon) transferUnlistedDrop(n int) {
	if d.transferUnlistedDropped == nil {
		d.transferUnlistedDropped = map[int]bool{}
	}
	d.transferUnlistedDropped[n] = true
}
