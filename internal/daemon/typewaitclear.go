package daemon

import (
	"maps"
	"slices"

	"github.com/mertenvg/logr/v2"
)

// typeWaitClear, on a repo that uses labels (types: labels), drops what the
// daemon keeps of issue types no event explains (#246). A repo that used
// issue types until its config switched, while an issue's type waited,
// held back or left out (SPEC §5.1), guards issue types no more, so no
// CheckLabels would find the type explained or revert it: it would stay
// due, and the issue would be read on every pass. So the memory of such
// types is forgotten, and each issue held with its type left out, for the
// type that stands, is read again once, to take its own type, as a repo
// that uses labels reads it. reload calls it on every pass, as the config
// may switch on any, so it costs nothing when nothing waits: no request,
// and no read. On a restart nothing waits, as the memory is in-process.
func (d *Daemon) typeWaitClear() {
	a := d.Step.Applier
	if a.IssueTypes {
		return
	}
	forgot := a.seen().TypeWaitClear(a.Owner, a.Repo)
	var reread []int
	for _, n := range slices.Sorted(maps.Keys(d.snaps)) {
		if d.snaps[n].TypeLeftOut != nil {
			d.reread(n, false)
			reread = append(reread, n)
		}
	}
	if forgot > 0 || len(reread) > 0 {
		logr.Infof("%s/%s uses labels for types now, so %d issue types no event explains are forgotten, and these issues, held with their type left out, are read again: %v",
			a.Owner, a.Repo, forgot, reread)
	}
}
