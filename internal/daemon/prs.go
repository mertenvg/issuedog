package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/state"
)

// The PRs the App opened, as the poll lists them (SPEC §5, §9.4, §9.6).
// The poll lists pull requests with the issues, and keeps each one the
// App opened, in memory (Daemon.prs): a start's first poll lists every
// issue, so a restart loses none, and each later poll lists the ones that
// changed. A PR someone else opened is never a task's (SPEC §7.5), so it's
// never kept. The digest's pull request item, the PRs' assignees and their
// priority labels are judged from what's kept, at no request of their own,
// and a write's answer is kept as GitHub gave it, so the next pass
// compares against what GitHub has.

// listedPR is what the poll last listed of a PR the App opened, or what a
// write to it answered: whether it's open, whether it merged, when it
// closed and when it was last updated, its title, which is raw, and its
// labels and assignees.
type listedPR struct {
	Number              int
	Title               string
	Open, Merged        bool
	ClosedAt, UpdatedAt time.Time
	Labels, Assignees   []string
}

// prKeep keeps is, a pull request the poll listed, in Daemon.prs, if the
// App opened it. A PR someone else opened is left out, so it's never a
// task's, whatever a move's pr= names (SPEC §7.5).
func (d *Daemon) prKeep(is *gogithub.Issue) {
	if !strings.EqualFold(is.GetUser().GetLogin(), d.Step.AppSlug+"[bot]") {
		return
	}
	var labels []string
	for _, l := range is.Labels {
		labels = append(labels, l.GetName())
	}
	if d.prs == nil {
		d.prs = map[int]listedPR{}
	}
	d.prs[is.GetNumber()] = listedPR{Number: is.GetNumber(), Title: is.GetTitle(), Open: is.GetState() == "open",
		Merged: !is.GetPullRequestLinks().GetMergedAt().IsZero(), ClosedAt: is.GetClosedAt().Time, UpdatedAt: is.GetUpdatedAt().Time,
		Labels: labels, Assignees: assignListed(is)}
}

// taskPR is the PR of task is (SPEC §5): the pr= of its latest move into
// pr-open that names one, or 0 for none. A move into pr-open by hand, as
// a stage:pr-open set back, names none, so it looks through that to the
// PR step's move before it.
func taskPR(is state.Issue) int {
	pr := 0
	for _, m := range is.Markers {
		if m.Kind != "transition" || state.Stage(m.Fields["to"]) != state.PROpen {
			continue
		}
		if n, err := strconv.Atoi(m.Fields["pr"]); err == nil && n > 0 {
			pr = n
		}
	}
	return pr
}

// prTaskSet records task n's PR, pr (taskPR), as of its last good step 1,
// for its priority label (prLabelPass), or forgets it for 0.
func (d *Daemon) prTaskSet(n, pr int) {
	if pr == 0 {
		delete(d.prOf, n)
		return
	}
	if d.prOf == nil {
		d.prOf = map[int]int{}
	}
	d.prOf[n] = pr
}

// prTasks is the task of each PR some task names (prOf), by PR: the
// lowest-numbered task, should two name one.
func (d *Daemon) prTasks() map[int]int {
	tasks := map[int]int{}
	for _, n := range slices.Sorted(maps.Keys(d.prOf)) {
		if _, ok := tasks[d.prOf[n]]; !ok {
			tasks[d.prOf[n]] = n
		}
	}
	return tasks
}

// prLabelPass keeps the priority label of each open PR the App opened
// whose task, the issue whose latest move into pr-open names it (taskPR),
// is held open (SPEC §5, §9.6): its priority:* labels are made exactly the
// task's, priority:pN from the task's snapshot, or priority:p2 with none,
// as the PR step labels it. The label is derived, not state: anyone's
// add, removal or change is undone, with no comment, no marker and no
// timeline read, and only the difference is written, each other
// priority:* label removed, then the task's added. Each write is judged
// from what the poll listed and the answers to the last writes (prs.go),
// so an unchanged poll makes no request. Iterate runs it after step 1,
// whose /priority it follows in the same pass, and before the digest. A
// failed write is the pass's error, and the rest carry on: the next pass
// writes again if the listing still differs.
func (d *Daemon) prLabelPass(ctx context.Context) error {
	a, tasks := d.Step.Applier, d.prTasks()
	var errs []error
	for _, n := range slices.Sorted(maps.Keys(d.prs)) {
		p := d.prs[n]
		task, ok := tasks[n]
		s := d.snaps[task]
		if !p.Open || !ok || s == nil || s.Closed {
			continue
		}
		if err := d.prLabel(ctx, p, task, fmt.Sprintf("priority:p%d", priority(s))); err != nil {
			errs = append(errs, fmt.Errorf("the priority label of %s/%s#%d, task #%d's PR: %w", a.Owner, a.Repo, n, task, err))
		}
	}
	return errors.Join(errs...)
}

// prLabel makes p's priority:* labels exactly want, for task: each other
// one is removed, one gone meanwhile included, then want is added, unless
// p has it, and p is kept with the labels GitHub answers with.
func (d *Daemon) prLabel(ctx context.Context, p listedPR, task int, want string) error {
	a := d.Step.Applier
	for _, l := range slices.Clone(p.Labels) {
		if !strings.HasPrefix(strings.ToLower(l), "priority:") || strings.EqualFold(l, want) {
			continue
		}
		resp, err := a.Client.Issues.RemoveLabelForIssue(ctx, a.Owner, a.Repo, p.Number, l)
		if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
			return fmt.Errorf("remove %s: %w", l, err)
		}
		p.Labels = slices.DeleteFunc(p.Labels, func(x string) bool { return x == l })
		d.prs[p.Number] = p
		logr.Infof("%s/%s#%d: removed %s from it, as its task #%d's priority is %s", a.Owner, a.Repo, p.Number, l, task, want)
	}
	if slices.ContainsFunc(p.Labels, func(l string) bool { return strings.EqualFold(l, want) }) {
		return nil
	}
	labels, _, err := a.Client.Issues.AddLabelsToIssue(ctx, a.Owner, a.Repo, p.Number, []string{want})
	if err != nil {
		return fmt.Errorf("add %s: %w", want, err)
	}
	p.Labels = nil
	for _, l := range labels {
		p.Labels = append(p.Labels, l.GetName())
	}
	d.prs[p.Number] = p
	logr.Infof("%s/%s#%d: added %s to it, its task #%d's priority", a.Owner, a.Repo, p.Number, want, task)
	return nil
}

// prLink is the link to PR pr, for its digest item.
func prLink(owner, repo string, pr int) string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", owner, repo, pr)
}

// prOfDigest is the PR of task is (taskPR), as the poll listed it, for
// waitFind's pull request item (waitInput.PR), if the App opened it, or
// nil.
func (d *Daemon) prOfDigest(is state.Issue) *listedPR {
	p, ok := d.prs[taskPR(is)]
	if !ok {
		return nil
	}
	return &p
}

// prItemStands reports whether it, a pull request item (waitPR), stands
// on this pass: its PR is listed open (SPEC §9.4). A PR that merges or
// closes changes nothing on its task until a merge closes it, so the item
// is found on the task's last visit and judged again here on each pass,
// from what the poll listed, with no request.
func (d *Daemon) prItemStands(it waitItem) bool {
	return d.prs[it.AssignPR].Open
}
