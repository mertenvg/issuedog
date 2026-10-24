package daemon

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

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
// a gone issue (Iterate), unless the issue's snapshot is from this pass:
// this pass's poll listed it, as it lists one that changed, which it
// marks stale, and the one at its cursor, which a pass just read. So the
// read, this pass, drops a deleted or transferred issue at once: from
// Work, the digest's items and assignment, while one the poll has just
// shown in the repo needs two reads that find it gone, as ever. One that
// is there after all, such as one closed since the poll, is kept, or left
// by the next poll.
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
// every issue that changed.
//
// An issue the listing has and the daemon holds is read no more than
// before, and a pass between two listings makes no request here. A start's
// first pass lists nothing, as its poll lists every issue and the daemon
// holds none before it, and nor does a pass that holds none. A failed
// listing is the pass's error, and the next listing waits
// transferUnlistedEvery from it, as from a good one, so a listing that
// keeps failing is tried once every 10 minutes, not on every pass.
func (d *Daemon) transferUnlistedCheck(ctx context.Context) error {
	st := d.Step
	now := st.Client.Now()
	switch {
	case d.transferUnlistedAt.IsZero():
		d.transferUnlistedAt = now
		return nil
	case len(d.snaps) == 0 || now.Sub(d.transferUnlistedAt) < transferUnlistedEvery:
		return nil
	}
	d.transferUnlistedAt = now // a failed listing waits as long as a good one
	issues, err := st.Client.TransferUnlistedOpen(ctx, st.Owner, st.Repo)
	if err != nil {
		return fmt.Errorf("the check for held issues no longer listed, tried again in %s: %w", transferUnlistedEvery, err)
	}
	listed := make(map[int]bool, len(issues))
	for _, is := range issues {
		n := is.GetNumber()
		listed[n] = true
		if d.snaps[n] != nil || d.stale[n] || is.IsPullRequest() || d.digest.has(n) || !is.GetUpdatedAt().Before(d.issuesSince) {
			continue
		}
		logr.Infof("%s/%s#%d is listed open, but the daemon holds nothing of it, as when wrong answers dropped it as gone, so it's read again", st.Owner, st.Repo, n)
		d.stale[n] = true
	}
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		switch {
		case listed[n]:
		case d.transferUnlistedPolled[n]:
			logr.Infof("%s/%s#%d isn't listed open any more, though this pass's poll listed it, so it's read again, and dropped if two reads find it gone", st.Owner, st.Repo, n)
			d.reread(n, false)
		default:
			logr.Infof("%s/%s#%d isn't listed open any more, so it's read again, and dropped if it's gone, deleted or transferred", st.Owner, st.Repo, n)
			d.reread(n, true)
		}
	}
	return nil
}
