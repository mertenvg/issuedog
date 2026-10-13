package githubtest

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

// State is what a pass can change in the fake, without IDs and times, which a
// replayed pass is free to change.
type State struct {
	Issues []IssueState
	// RepoLabels are the repo's labels, a set (repolabels.go).
	RepoLabels []string
}

// IssueState is one issue in a State.
type IssueState struct {
	Number                    int
	Title, Body, Author, Type string
	// BodyEditedBy is the body's last editor, empty if never edited.
	BodyEditedBy string
	Labels       []string // sorted
	Parent       int
	Comments     []CommentState // oldest first
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
			Type: is.GetType().GetName(), BodyEditedBy: is.lastEdit.by, Parent: is.parent}
		for _, l := range is.Labels {
			st.Labels = append(st.Labels, l.GetName())
		}
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
	}
	s.RepoLabels = f.repoLabelState()
	return s
}

// Replay checks that pass is safe to run again after a crash at any write
// (plan #5, assumption 4). The contract:
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
//     another order; comments are compared in order, with their bodies. A
//     run may not leave an extra comment, reaction or label, or miss one.
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
