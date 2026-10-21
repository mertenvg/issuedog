package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github"
)

// Assignment (SPEC §9.6, #191) assigns every
// user in notify to a pipeline issue while something on it waits on the
// owner, and nobody otherwise. It takes the items from the digest's own
// list (Daemon.digestCache), which Iterate fills whether or not the digest
// is on, so the two never disagree. It holds no item kind of its own:
// any item on an issue assigns it, so a kind the list gains, such as
// #192's holds, assigns with no change here. It takes the items that
// stand on the pass, as the digest does (Daemon.digestCacheOf), which
// leaves out a finished story's while a child is open (#278).
//
// A pipeline issue, here (assignPipeline), is an open issue the daemon
// holds a snapshot of (never the digest, which has none) with a stage
// label, an App transition marker, or an item on the list. The last takes
// in a triage flag, which waits at intake with no stage label, and a state
// error on an issue outside the stages. An issue with none of these, such
// as one triage left to people, or one not triaged yet, is left alone,
// and so is a closed issue, whose snapshot the poll drops.
//
// The assignees a pipeline issue should have are exactly the notify
// users if it has an item, and nobody if it hasn't. Anyone else's
// change is reverted on the next poll, which reads the issue again when
// its updated_at moves. Only the difference is written: who is assigned
// but shouldn't be is removed, then who should be but isn't is added, so
// the add doesn't meet GitHub's cap of ten assignees with ones that are
// going. Logins compare in any case, as GitHub's do.
//
// Each pass looks only at the issues whose step 1 succeeded on the
// snapshot held (Daemon.inputs), as Work does: an issue whose read or
// step 1 failed, or that a command touched, keeps its assignees that
// pass. Assignees are read from the snapshot, which Snapshot fills from
// the issue GET it makes anyway, so an unchanged pass makes no request. A
// write's answer is the issue's assignees after it, which the snapshot
// takes, so the next pass compares against what GitHub has.
//
// GitHub silently leaves out a login it can't assign, such as a notify
// user with no access to the repo (assignDrop). Such a login is
// remembered, logged once, and not asked for again, so it can't cause a
// write on every pass. The memory is kept for the start and the notify
// list it was found with: a change to notify, or a restart, forgets it,
// and the first issue to want it then tries once more, so a restart with
// such a login in notify makes that one add, which changes nothing. A
// remembered login that is assigned all the same, as by hand once it has
// access, stays where an item wants it: only an add skips it. It isn't
// forgotten for that, since another issue's add would then drop it again,
// and the two would trade a write on every pass.
//
// An issue that leaves the pipeline while it's open, as a flag that
// /ignore answers does, loses the notify users it was assigned, once
// (Daemon.assignManaged): the rest of its assignees, and any later change
// to them, are left alone. That memory is the start's too, so an issue
// that left while no daemon ran keeps them (a gap for SPEC §16, #194).

// assignPass sets each pipeline issue's assignees, if Daemon.Assign is
// set, after the digest pass and before Work. A failed write is the pass's
// error, which Run logs, and the rest of the issues, and Work, carry on,
// its issue included. The next pass reads the issue again, and writes
// again if it still differs. A write that finds the issue gone is no
// error, keeps the issue from Work, and counts as one of the two reads
// that drop it (assignFailed).
func (d *Daemon) assignPass(ctx context.Context) error {
	if !d.Assign {
		return nil
	}
	notify := d.assignNotify()
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(d.inputs)) {
		if s := d.snaps[n]; s != nil && !s.Closed {
			if err := d.assignIssue(ctx, s, notify); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// assignNotify returns config's notify users, without an empty login or
// a login twice in any case. A notify list that changed forgets the
// dropped logins (assignDropped).
func (d *Daemon) assignNotify() []string {
	var notify []string
	if d.Config != nil {
		notify = d.Config.Notify
	}
	if key := strings.ToLower(strings.Join(notify, " ")); key != d.assignFor {
		d.assignFor, d.assignDropped = key, nil
	}
	var users []string
	for _, l := range notify {
		if l != "" && !assignHas(users, l) {
			users = append(users, l)
		}
	}
	return users
}

// assignIssue writes the difference between s's assignees and what they
// should be (see the top of this file), and records whether s is managed.
func (d *Daemon) assignIssue(ctx context.Context, s *github.Snapshot, notify []string) error {
	n, items := s.Number, len(d.digestCacheOf(s.Number)) > 0
	pipeline := assignPipeline(s, d.Step.AppSlug, items)
	var add, remove []string
	switch {
	case pipeline:
		if d.assignManaged == nil {
			d.assignManaged = map[int]bool{}
		}
		d.assignManaged[n] = true
		var want []string
		if items {
			want = notify
			add = slices.DeleteFunc(assignMinus(notify, s.Assignees), func(l string) bool { return d.assignDropped[strings.ToLower(l)] })
		}
		remove = assignMinus(s.Assignees, want)
	case d.assignManaged[n]:
		// It left the pipeline: only the notify users go, once.
		remove = slices.DeleteFunc(slices.Clone(s.Assignees), func(l string) bool { return !assignHas(notify, l) })
	}
	a := d.Step.Applier
	if len(remove) > 0 {
		got, err := a.Client.RemoveAssignees(ctx, a.Owner, a.Repo, n, remove)
		if err != nil {
			return d.assignFailed(n, err)
		}
		logr.Infof("unassigned %s from %s/%s#%d", strings.Join(remove, ", "), a.Owner, a.Repo, n)
		s.Assignees = got
	}
	if len(add) > 0 {
		got, err := a.Client.AddAssignees(ctx, a.Owner, a.Repo, n, add)
		if err != nil {
			return d.assignFailed(n, err)
		}
		logr.Infof("assigned %s to %s/%s#%d", strings.Join(add, ", "), a.Owner, a.Repo, n)
		s.Assignees = got
		d.assignDrop(n, assignMinus(add, got))
	}
	if !pipeline {
		delete(d.assignManaged, n) // a failed removal returned above, and is tried again next pass
	}
	return nil
}

// assignFailed returns the error of a failed assignee write to issue n,
// err, for the pass. The poll never lists a gone issue again, deleted or
// transferred, so its snapshot would stay, and every pass would write
// again. So after any failed write the issue is read again next pass.
// Whether it's gone is then the snapshot read's to say, whatever answer
// the write got (#199). A write that gone recognises as finding the issue
// gone, deleted (404 or 410) or transferred (a 301, which Go follows with
// a GET that 404s on an assignee path, or an answer from another repo),
// is no error, and is the first of the two reads that drop it: Work
// doesn't get the issue this pass (Daemon.reread), as a stage run does
// when its fresh read finds the issue gone.
//
// Any other failure leaves Work the issue this pass, since a failed write
// says nothing of the snapshot step 1 took: only the next pass's read is
// made sure of (Daemon.stale). So a write that fails on every pass, such
// as with a 422 for a login GitHub can't assign, costs that read and the
// write on each pass, and never keeps the issue from its stage work
// (#238). A gone issue whose write got such an answer is found gone by
// its next read, the first of the two that drop it: the read before a
// stage run (Runs.runStage) or the read of a run's lease, on applying
// its saved result or after a restart (Runs.apply, Runs.resume), if Work
// makes one this pass, or else the next pass's.
func (d *Daemon) assignFailed(n int, err error) error {
	a := d.Step.Applier
	if gone(err) {
		logr.Infof("%s/%s#%d looks gone, deleted or transferred, so its assignees aren't written and it's read again: %v", a.Owner, a.Repo, n, err)
		d.reread(n, true)
		return nil
	}
	if d.stale == nil {
		d.stale = map[int]bool{}
	}
	d.stale[n] = true
	return fmt.Errorf("the assignees of %s/%s#%d, read again next pass: %w", a.Owner, a.Repo, n, err)
}

// assignDrop remembers logins, which GitHub left out of an add to issue
// n, as ones it can't assign, so no pass asks for them again until notify
// changes or the daemon restarts, and logs them.
func (d *Daemon) assignDrop(n int, logins []string) {
	if len(logins) == 0 {
		return
	}
	if d.assignDropped == nil {
		d.assignDropped = map[string]bool{}
	}
	for _, l := range logins {
		d.assignDropped[strings.ToLower(l)] = true
	}
	a := d.Step.Applier
	logr.Warnf("GitHub left %s out when assigning %s/%s#%d, so they can't be assigned, as for no access to the repo: "+
		"they aren't asked for again until notify changes or the daemon restarts", strings.Join(logins, ", "), a.Owner, a.Repo, n)
}

// assignPipeline reports whether s, an open issue the daemon holds, is a
// pipeline issue (see the top of this file): it has items on the list, a
// stage label, or a transition marker of the App's, slug's, in its body
// or a comment. A marker in a comment that has been edited since still
// counts: the App wrote it, so the issue has been in the pipeline.
func assignPipeline(s *github.Snapshot, slug string, items bool) bool {
	if items || stageOf(s) != "" {
		return true
	}
	transition := func(m github.Marker) bool { return m.Kind == "transition" }
	if slices.ContainsFunc(github.TrustedMarkers(s.Author, s.Body, slug), transition) {
		return true
	}
	for _, c := range s.Comments {
		if slices.ContainsFunc(github.TrustedMarkers(c.Author, c.Body, slug), transition) {
			return true
		}
	}
	return false
}

// assignHas reports whether logins has login, in any case.
func assignHas(logins []string, login string) bool {
	return slices.ContainsFunc(logins, func(l string) bool { return strings.EqualFold(l, login) })
}

// assignMinus returns the logins in a that b hasn't, in any case, in a's
// order, or nil for none.
func assignMinus(a, b []string) []string {
	var out []string
	for _, l := range a {
		if !assignHas(b, l) {
			out = append(out, l)
		}
	}
	return out
}

// assignListed is the logins of a listed issue's assignees, for the poll
// (changed).
func assignListed(is *gogithub.Issue) []string {
	var logins []string
	for _, u := range is.Assignees {
		logins = append(logins, u.GetLogin())
	}
	return logins
}

// assignSame reports whether a and b hold the same logins, in any case
// and order.
func assignSame(a, b []string) bool {
	return len(assignMinus(a, b)) == 0 && len(assignMinus(b, a)) == 0
}
