package daemon

import (
	"context"
	"maps"

	"github.com/mertenvg/logr/v2"
)

// A command on one issue may write to another before the other's turn in
// the pass, as a parent's /resume writes its "ended" record to a child
// (Step.Touched). The other's snapshot is then out of date, so it skips
// step 1 and Work, is read again next pass, and keeps its items for the
// digest and assignment till then (digestCacheSet). But on a start, it
// has none yet, though the digest's body, which the last daemon wrote,
// may list them: on a restart's first pass, which reads and visits every
// issue, a child numbered after the parent would drop out of the digest
// for that pass, an edit that says "Nothing is waiting on you", and come
// back on the next, another edit, unannounced, as its key was announced
// already (#216). So an issue the start's first poll listed open, whose
// step 1 hasn't succeeded since (Daemon.digestRestartUnknown), is read
// again at its turn instead, after the writes made to it, and visited on
// that snapshot, as the next pass would visit it. It stays touched all
// the same: it's left out of Work, and read again next pass, as an issue
// a command wrote to after its visit is, which is what a child numbered
// before its parent gets anyway. But its assignees are judged on the
// items its visit found, which the digest lists (assignPass), so
// assignment doesn't lag the digest by a pass (#360). So the next pass's read finds what this one did,
// mostly in 304s. The read again costs one snapshot read on such a pass.
// On a later pass, an issue has items
// cached, or was first listed after the start, so the digest never listed
// it, as a child a plan just filed, and it's read no more often than
// before, but for one whose visits have failed since the start. The poll
// after digestDrop lists every issue again, but it isn't a start's first:
// digestDrop keeps digestCache, so the items stay cached, and it marks no
// issue (digestRestartList), changed or not. Each stays as it was, marked
// or not, as above. An unchanged pass touches nothing.
//
// A close the poll lists drops the issue's items too (Daemon.leave),
// though the digest's body, as last written, lists them. So an issue the
// poll lists closed with items cached is unknown again, and a close
// reverted before the visits, on a pass whose command writes to the issue
// before its turn, keeps its items the same way.
//
// A visit that fails, or waits for its next pass, after its command wrote
// to the issue may have left that command half done, to finish on the
// next pass, so the issue isn't read again: its own commands would come
// before the rest of that one's, out of the order they're handled in
// (holdOrderWaits, #205). It skips the pass, as before, and so does one
// whose read again fails. A read again that finds it gone counts as the
// first of the two reads that drop it, as any read does. Such an issue,
// and one whose own visit fails after a restart, which isn't touched, has
// no items cached, so the digest lists what its own body showed for it,
// as the start read it back (digestkept.go), till a visit succeeds.

// digestRestartTouches are the issues the commands of one pass's visits
// wrote to (Step.Touched), for digestRestartRead.
type digestRestartTouches struct {
	visit  map[int]bool // by the visit under way
	failed map[int]bool // by a visit that failed, or waits for its next pass
}

// touch records that the visit under way may write to issue n.
func (t *digestRestartTouches) touch(n int) {
	if t.visit == nil {
		t.visit = map[int]bool{}
	}
	t.visit[n] = true
}

// begin starts a visit.
func (t *digestRestartTouches) begin() { clear(t.visit) }

// fail records that the visit under way failed, or waits for its next
// pass, so the issues it wrote to aren't read again this pass.
func (t *digestRestartTouches) fail() {
	if t.failed == nil {
		t.failed = map[int]bool{}
	}
	maps.Copy(t.failed, t.visit)
}

// digestRestartRead reads issue n again, which a command on another issue
// wrote to this pass before n's turn, and holds the snapshot, for n's
// visit, if the digest may list items for n that aren't cached, and no
// visit that failed wrote to it (touches). It reports whether it did. A
// failed read is logged, and n then skips the pass, as a touched issue
// does. A read that finds n gone, deleted or transferred, is the first of
// the two that drop it, as the pass's own read would be (reread), so the
// next pass's read drops it if it's gone still.
func (d *Daemon) digestRestartRead(ctx context.Context, n int, touches *digestRestartTouches) bool {
	if !d.digestRestartUnknown[n] || touches.failed[n] {
		return false
	}
	st := d.Step
	s, err := st.Client.Snapshot(ctx, st.Owner, st.Repo, n)
	if gone(err) {
		logr.Infof("%s/%s#%d, which a command wrote to, looks gone, deleted or transferred, when read again, so it waits for the next pass, which reads it again: %v",
			st.Owner, st.Repo, n, err)
		d.reread(n, true)
		return false
	}
	if err != nil {
		logr.Warnf("%s/%s#%d, which a command wrote to, failed to read again, so it waits for the next pass, and the digest lists only what its body showed for it till then: %v",
			st.Owner, st.Repo, n, err)
		return false
	}
	d.snaps[n] = s
	return true
}

// digestRestartList records open issue n, which the poll listed as
// changed, as one whose items aren't cached, if the poll is the start's
// first, start, which lists every issue. The poll after digestDrop lists
// every issue again too, but isn't the start's (Daemon.startPolled): the
// items it would mark are cached still.
func (d *Daemon) digestRestartList(n int, start bool) {
	if !start {
		return
	}
	if d.digestRestartUnknown == nil {
		d.digestRestartUnknown = map[int]bool{}
	}
	d.digestRestartUnknown[n] = true
}

// digestRestartLeave records issue n, which left the pipeline, as one
// whose items aren't cached, if it had any, which leave drops. Any items
// read back for it from the digest's body go: a closed issue has nothing
// waiting, and a digest lists nothing of its own.
func (d *Daemon) digestRestartLeave(n int) {
	delete(d.digestKept, n)
	if _, ok := d.digestCache[n]; !ok {
		return
	}
	if d.digestRestartUnknown == nil {
		d.digestRestartUnknown = map[int]bool{}
	}
	d.digestRestartUnknown[n] = true
}

// digestRestartGone forgets issue n, dropped as gone, deleted or
// transferred: GitHub never reuses its number, so no poll lists it again.
// Any items read back for it from the digest's body go too.
func (d *Daemon) digestRestartGone(n int) {
	delete(d.digestRestartUnknown, n)
	delete(d.digestKept, n)
}

// digestRestartKnow records that step 1 succeeded on issue n, so its items
// are cached (digestRestartRead), in place of any read back for it from
// the digest's body (digestkept.go).
func (d *Daemon) digestRestartKnow(n int) {
	delete(d.digestRestartUnknown, n)
	delete(d.digestKept, n)
}
