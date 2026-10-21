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
// has passed by GitHub's clock since it last did, and has each issue the
// daemon holds a snapshot of that the listing leaves out read again
// (reread), with the listing counted as the first of the two reads that
// drop a gone issue (Iterate). So the read, this pass, drops a deleted or
// transferred issue at once: from Work, the digest's items and
// assignment. One that is there after all, such as one closed since the
// poll, is kept, or left by the next poll. An issue the listing has is
// read no more than before, and a pass between two listings makes no
// request here.
//
// A start's first pass lists nothing, as its poll lists every issue and
// the daemon holds none before it, and nor does a pass that holds none.
// A failed listing is the pass's error, and the next pass lists again.
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
	issues, err := st.Client.TransferUnlistedOpen(ctx, st.Owner, st.Repo)
	if err != nil {
		return fmt.Errorf("the check for held issues no longer listed, tried again next pass: %w", err)
	}
	d.transferUnlistedAt = now
	listed := make(map[int]bool, len(issues))
	for _, is := range issues {
		listed[is.GetNumber()] = true
	}
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		if !listed[n] {
			logr.Infof("%s/%s#%d isn't listed open any more, so it's read again, and dropped if it's gone, deleted or transferred", st.Owner, st.Repo, n)
			d.reread(n, true)
		}
	}
	return nil
}
