package githubtest

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// State is what a pass can change in the fake, without IDs and times, which a
// replayed pass is free to change.
type State struct {
	Issues []IssueState
	// RepoLabels are the repo's labels, a set (repolabels.go).
	RepoLabels []string
	// PRs are the pull requests the fake opened (pulls.go), by number. Each
	// is in Issues too, with its labels and assignees.
	PRs []PRState
	// CheckRuns are the check runs on the git remote's commits
	// (checkruns.go), by SHA, sorted, and each SHA's in the order posted.
	CheckRuns []CheckRunState
	// Branches are the git remote's branches under issuedog/, the App's
	// own, by name, sorted, each with its tip, so a merge's squash commit
	// and a branch's delete are compared. It's nil with no git remote
	// (SetGitRemote).
	Branches []BranchState
}

// BranchState is one branch of the git remote in a State: its name and
// the SHA of its tip.
type BranchState struct {
	Name, SHA string
}

// CheckRunState is one check run in a State: the commit it's on, its name
// and conclusion, the ID of the App that posted it, and its external_id, ""
// for none.
type CheckRunState struct {
	SHA, Name, Conclusion string
	AppID                 int64
	ExternalID            string
}

// PRState is one pull request in a State: its head and base branches, the
// SHA its head showed when last read, its title, body and state, whether
// it merged, and who merged it, "" until it did.
type PRState struct {
	Number                                  int
	Head, Base, HeadSHA, Title, Body, State string
	Merged                                  bool
	MergedBy                                string
}

// IssueState is one issue in a State.
type IssueState struct {
	Number                    int
	Title, Body, Author, Type string
	// BodyEditedBy is the body's last editor, empty if never edited.
	BodyEditedBy string
	Labels       []string // sorted
	Parent       int
	// SubIssues are the numbers of its sub-issues in this repo, in the
	// order they were linked, which GitHub lists them in. BlockedBy are
	// the numbers of the issues blocking it, sorted, since GitHub treats
	// them as a set (dependencies.go).
	SubIssues, BlockedBy []int
	Comments             []CommentState // oldest first
	// Assignees are the logins assigned, sorted, since GitHub treats them
	// as a set (assign.go).
	Assignees []string

	// Closed and Pinned are whether the issue is closed, and pinned to the
	// repo's issue list.
	Closed, Pinned bool
}

// CommentState is one comment in a State.
type CommentState struct {
	Author, Body string
	// EditedBy is the last editor, empty if never edited.
	EditedBy string
	// Reactions are "<login> <content>", sorted, since GitHub treats them as
	// a set: one of each content per user.
	Reactions []string
}

// State returns the fake's current State, issues by number.
func (f *Fake) State() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	var s State
	for _, is := range f.issues {
		st := IssueState{Number: is.GetNumber(), Title: is.GetTitle(), Body: is.GetBody(), Author: is.GetUser().GetLogin(),
			Type: is.GetType().GetName(), BodyEditedBy: is.lastEdit.by, Parent: is.parent,
			Closed: is.GetState() == "closed", Pinned: is.pinned,
			SubIssues: f.subIssueNumbers(is.GetNumber()), BlockedBy: slices.Sorted(slices.Values(is.blockedBy))}
		for _, l := range is.Labels {
			st.Labels = append(st.Labels, l.GetName())
		}
		for _, u := range is.Assignees {
			st.Assignees = append(st.Assignees, u.GetLogin())
		}
		slices.Sort(st.Assignees)
		for _, c := range f.comments {
			if c.issue != is.GetNumber() {
				continue
			}
			cs := CommentState{Author: c.GetUser().GetLogin(), Body: c.GetBody(), EditedBy: c.lastEdit.by}
			for _, r := range c.reactions {
				cs.Reactions = append(cs.Reactions, r.GetUser().GetLogin()+" "+r.GetContent())
			}
			slices.Sort(cs.Reactions)
			st.Comments = append(st.Comments, cs)
		}
		s.Issues = append(s.Issues, st)
		if pr := is.pr; pr != nil {
			s.PRs = append(s.PRs, PRState{Number: is.GetNumber(), Head: pr.head, Base: pr.base, HeadSHA: pr.headSHA,
				Title: is.GetTitle(), Body: is.GetBody(), State: is.GetState(), Merged: pr.merged, MergedBy: pr.mergedBy})
		}
	}
	s.RepoLabels = f.repoLabelState()
	s.CheckRuns = f.checkRunState()
	s.Branches = f.branchState()
	return s
}

// branchState is the git remote's branches under issuedog/ for a State, by
// name, with their tips, or nil when there's no git remote. A remote that
// can't be read fails the test. f is locked.
func (f *Fake) branchState() []BranchState {
	f.t.Helper()
	if f.pulls.remote == "" {
		return nil
	}
	out, err := f.remoteGit(context.Background(), "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/issuedog/")
	if err != nil {
		f.fakeFail("githubtest: the git remote's branches: %v", err)
	}
	var branches []BranchState
	for line := range strings.Lines(out) {
		sha, ref, _ := strings.Cut(strings.TrimSpace(line), " ")
		branches = append(branches, BranchState{Name: strings.TrimPrefix(ref, "refs/heads/"), SHA: sha})
	}
	return branches
}

// Replay checks that pass is safe to run again after a crash at any write
// (SPEC §5.2). The contract:
//
//  1. setup builds a fake and fills it. Replay calls it once per run, so it
//     must build the same fake every time. It may call t.Fatal.
//  2. Clean run: pass runs once on a fresh fake and must return nil. Its
//     final State is the reference, and its W writes are the crash points.
//  3. For each n from 1 to W, twice, on a fresh fake from setup: once with
//     FailWrite(n), where write n fails with a 500 before it changes anything
//     (a crash just before it), and once with FailWriteAfter(n), where write n
//     takes effect but answers 500 (a crash after GitHub applied it). Each
//     time pass runs, its error only logged, then the failure is cleared and
//     pass runs again and must return nil. The first pass must have reached
//     write n. Then once more with nothing failed: a second pass over the
//     clean result, a crash after the last write.
//  4. Each run's final State must equal the clean run's. State leaves out IDs
//     and times. Labels and reactions are sets, so those may be redone in
//     another order, and so are assignees and blocked-by links; comments
//     are compared in order, with their bodies, and sub-issues in the
//     order they were linked, and pull requests by number, with their
//     heads, bases, titles, bodies, states and mergers, and check runs by
//     SHA, in the order posted, with their names, conclusions, Apps and
//     external_ids, and the git remote's issuedog/ branches by name, with
//     their tips. A run may
//     not leave an extra comment, reaction, label, assignee, sub-issue,
//     blocked-by link, pull request, check run or branch, or miss one.
//
// pass must build its own client and in-memory state from the fake it gets,
// as the daemon does on restart, and must decide its writes only from what
// it reads, so a run makes the same writes as the clean run up to write n.
func Replay(t *testing.T, setup func(testing.TB) *Fake, pass func(*Fake) error) {
	t.Helper()
	for _, p := range replay(t, setup, pass) {
		t.Error(p)
	}
}

// replay runs Replay's checks and returns the problems found.
func replay(t testing.TB, setup func(testing.TB) *Fake, pass func(*Fake) error) []string {
	clean := setup(t)
	if err := pass(clean); err != nil {
		return []string{fmt.Sprintf("clean pass: %v", err)}
	}
	want, w := clean.State(), len(clean.Writes())
	var problems []string
	for i := 0; i <= 2*w; i++ {
		n, after := i/2+1, i%2 == 1
		at := fmt.Sprintf("crash before write %d", n)
		if after {
			at = fmt.Sprintf("crash after write %d applied", n)
		}
		if i == 2*w {
			n, at = w+1, "crash after the last write"
		}
		f := setup(t)
		if after {
			f.FailWriteAfter(n)
		} else {
			f.FailWrite(n)
		}
		if err := pass(f); err != nil {
			t.Logf("%s: first pass: %v", at, err)
		}
		if writes := f.Writes(); n <= w && (len(writes) < n || !writes[n-1].Failed) {
			problems = append(problems, fmt.Sprintf("%s: the first pass never made write %d: %+v", at, n, writes))
			continue
		}
		f.FailWrite(0)
		if err := pass(f); err != nil {
			problems = append(problems, fmt.Sprintf("%s: second pass: %v", at, err))
			continue
		}
		if got := f.State(); !reflect.DeepEqual(got, want) {
			problems = append(problems, fmt.Sprintf("%s: the state after the second pass is\n%+v\nwant the clean run's\n%+v", at, got, want))
		}
	}
	return problems
}
