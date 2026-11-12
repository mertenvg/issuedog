package daemon_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// A pass's candidates filed as follow-ups through whole runs (SPEC §8.1,
// §10.2), on a rig wired as main's, with real temporary repos and a fake
// claude.

// filingPassID is the ID of the comment on task n carrying the verify
// marker of run.
func filingPassID(t *testing.T, f *githubtest.Fake, n int, run string) int64 {
	t.Helper()
	s, err := f.Client(t).Snapshot(context.Background(), "o", "r", n)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Comments {
		if strings.Contains(c.Body, "<!-- issuedog:verify verdict=pass ") && strings.Contains(c.Body, " run="+run+" ") {
			return c.ID
		}
	}
	t.Fatalf("issue %d has no pass of run %s", n, run)
	return 0
}

// filingFollowUps are the issues of f whose body carries a follow-up's
// origin marker.
func filingFollowUps(f *githubtest.Fake) []githubtest.IssueState {
	var found []githubtest.IssueState
	for _, is := range f.State().Issues {
		if strings.Contains(is.Body, "<!-- issuedog:origin verify=") {
			found = append(found, is)
		}
	}
	return found
}

// A whole run: the implementer commits and moves the task to verifying, the
// next pass runs the checks, and the pass after runs the verifier, which
// passes the head with two candidates and files nothing. The pass after
// that files both, in step 1, in the pass's order: each a task the App
// filed, titled as the candidate, with type:task, priority:p3, its scope
// and stage:intake, whose body names the pass, gives the candidate's
// fields, says how to approve it, ends in the cc line and has its origin
// last, then the record on the task, naming both. The pass after reads
// what it wrote, and writes nothing, and each unchanged pass after makes no
// request but 304s, writes nothing and starts no run. claude ran twice in
// all, the implementer and the verifier, so no other Stage ran, triage, the
// Unlabeled stage, included, on the follow-ups or anything else.
func TestFollowUpsWholeRun(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	v.out(1, ivResult(t, implementerOutput(t, "commits")))
	v.out(2, ivResult(t, verifierCandidateOutput("A quote that expires mid-booking", "A refund after the quote expires")))
	n := f.AddIssue(wholeRunOwner, "Refuse an expired quote", "Booking an expired quote returns 410.", "type:task", "priority:p1", "scope:booking")
	f.AddLabel(n, wholeRunOwner, "stage:in-progress")
	if n != 1 {
		t.Fatalf("the task is #%d, want #1", n)
	}

	v.passes(3)
	if v.runs() != 2 || len(passComments(f, n)) != 1 || pickStage(f, n) != "stage:verifying" {
		t.Fatalf("after the verifier's pass: %d runs, verdicts %q, at %s; want two, one, verifying", v.runs(), passComments(f, n), pickStage(f, n))
	}
	if got := filingFollowUps(f); len(got) != 0 {
		t.Fatalf("the verifier's pass filed %+v; want nothing, as filing is the next pass's step 1", got)
	}
	issues := len(f.State().Issues)
	run := v.leaseOf("2").RunID
	pass := filingPassID(t, f, n, run)

	v.pass()
	got := filingFollowUps(f)
	if len(got) != 2 || len(f.State().Issues) != issues+2 {
		t.Fatalf("the follow-ups = %+v; want two, and no other issue", got)
	}
	labels := []string{"priority:p3", "scope:booking", "stage:intake", "type:task"}
	for i, want := range []githubtest.IssueState{
		{Number: issues + 1, Title: "A quote that expires mid-booking", Author: app, Labels: labels,
			Body: filingBody(pass, run, "F1", "The scenario of A quote that expires mid-booking", wholeRunOwner)},
		{Number: issues + 2, Title: "A refund after the quote expires", Author: app, Labels: labels,
			Body: filingBody(pass, run, "F2", "The scenario of A refund after the quote expires", wholeRunOwner)},
	} {
		if g := got[i]; g.Number != want.Number || g.Title != want.Title || g.Author != want.Author || g.Body != want.Body ||
			!slices.Equal(g.Labels, want.Labels) || g.Parent != 0 || g.Closed {
			t.Errorf("follow-up %d =\n%+v\nwant\n%+v, open, with no parent", i+1, g, want)
		}
	}
	record := filingRecord(pass, run, []int{issues + 1, issues + 2},
		"- F1 #"+strconv.Itoa(issues+1)+": A quote that expires mid-booking", "- F2 #"+strconv.Itoa(issues+2)+": A refund after the quote expires")
	if got := filingComments(f, "filed"); !slices.Equal(got, []string{record}) {
		t.Errorf("the records =\n%q\nwant\n%q", got, []string{record})
	}
	if comments := f.State().Issues[n-1].Comments; comments[len(comments)-1].Body != record {
		t.Errorf("the task's last comment =\n%s\nwant the record, the filing's last write", comments[len(comments)-1].Body)
	}

	writes := len(f.Writes())
	v.pass()
	if got := f.Writes()[writes:]; len(got) != 0 {
		t.Errorf("the pass after the filing wrote %+v, want nothing", got)
	}
	for _, step := range []string{"first", "second"} {
		v.quietPass(step)
	}
	if v.runs() != 2 {
		t.Errorf("claude ran %d times, want twice: the implementer and the verifier", v.runs())
	}
	for _, is := range filingFollowUps(f) {
		if !slices.Contains(is.Labels, "stage:intake") || len(is.Comments) != 0 {
			t.Errorf("follow-up #%d has labels %q and comments %q; want stage:intake, and none", is.Number, is.Labels, is.Comments)
		}
	}
}

// A failing verdict files nothing: a request_changes with candidates moves
// the task back to in-progress, and no pass after files a follow-up, as its
// comment carries no pass and keeps no candidate.
func TestFollowUpsNotOnRequestChanges(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	v := newVerdictRig(t, f, 3)
	head := v.head()
	n, _ := verifierTask(f, head)
	out := verifierOutput(t, "request_changes")
	out["follow_ups"] = verifierCandidateOutput("A dropped candidate")["follow_ups"]
	v.out(1, ivResult(t, out))
	v.out(2, ivResult(t, implementerOutput(t, "commits")))

	v.passes(2)
	if v.runs() != 1 || pickStage(f, n) != "stage:in-progress" {
		t.Fatalf("after the verdict: %d runs, at %s; want one, in-progress", v.runs(), pickStage(f, n))
	}
	v.passes(2)
	if got := filingFollowUps(f); len(got) != 0 {
		t.Errorf("the follow-ups = %+v; want none", got)
	}
	for _, c := range f.State().Issues[n-1].Comments {
		for _, m := range github.ParseMarkers(c.Body) {
			if m.Kind == "verify-data" || m.Kind == "filed" {
				t.Errorf("the task has a %s marker, in\n%s\nwant none", m.Kind, c.Body)
			}
		}
	}
}
