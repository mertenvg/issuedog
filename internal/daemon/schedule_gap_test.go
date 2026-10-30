package daemon_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/logtest"
)

// pickGapHolder files a task at in-progress, which holds the worker, with
// priority:p0, under a story at planned, so it gets no run of its own
// until #624, and returns its number.
func pickGapHolder(f *githubtest.Fake) int {
	story := fileAt(f, "planned", "A story.")
	f.SetType(story, "Story")
	holder := fileAt(f, "in-progress", "The task holding the worker.", "priority:p0")
	f.SetType(holder, "Task")
	f.SetParent(holder, story)
	return holder
}

// pickGapPass runs a pass of g that must fail with an error holding want,
// and returns the issue searches and blocked_by reads it made.
func pickGapPass(g *rig, want string) (searches, blockedBy int) {
	g.t.Helper()
	from := len(g.f.Requests())
	if err := g.d.Iterate(context.Background()); err == nil || !strings.Contains(err.Error(), want) {
		g.t.Fatalf("the pass = %v; want an error with %q", err, want)
	}
	return pickReads(g.f, from)
}

// pickGapNotPicked fails the test unless nothing was picked: no run, the ready
// task waiting still at ready, and no search or blocked_by read in the
// pass that made searches and blockedBy.
func pickGapNotPicked(g *rig, waiting int, searches, blockedBy int, pass string) {
	g.t.Helper()
	if g.runs() != 0 || pickStage(g.f, waiting) != "stage:ready" || searches != 0 || blockedBy != 0 {
		g.t.Errorf("%s: claude ran %d times, #%d is at %s, and the pass made %d searches and %d blocked_by reads; "+
			"want no run, the task left at ready, and neither request", pass, g.runs(), waiting, pickStage(g.f, waiting), searches, blockedBy)
	}
}

// pickGapFreed has the owner /hold the task holding the worker, which releases
// it, and checks the pass that handles it picks the ready task waiting as
// usual, keyed by its move to ready.
func pickGapFreed(g *rig, holder, waiting int) {
	g.t.Helper()
	g.f.AddComment(holder, "owner", "/hold")
	g.pass()
	if got := pickStage(g.f, holder); got != "stage:held" {
		g.t.Fatalf("the holder is at %s after the owner's /hold, want stage:held", got)
	}
	if got := g.leaseIssues(); !slices.Equal(got, []int{waiting}) || pickStage(g.f, waiting) != "stage:verifying" {
		g.t.Errorf("with the worker free, the runs leased %v, and #%d is at %s; want #%d picked, and at verifying", got, waiting, pickStage(g.f, waiting), waiting)
	}
	cs := implementerComments(g.t, g.f, waiting)
	if len(cs) < 2 || cs[1].Body != pickMoved(cs[0].ID, cs[0].ID) {
		g.t.Errorf("#%d's comments are %+v; want its move to ready, then the pick's", waiting, cs)
	}
}

// A read of the task holding the worker that fails, here a 502 once, or
// finds it gone, which may be wrong, leaves it out of the pass, so the
// pass can't see it holds the worker: nothing is picked that pass, with no
// search and no blocked_by read. The next pass, its read answering again,
// sees it at in-progress, so nothing is picked either. Once the owner's
// /hold frees the worker, the ready task is picked as usual (SPEC §8.1).
func TestPickNoneAfterHolderReadFails(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		status int
		err    string
	}{
		{"a 502", http.StatusBadGateway, "502"},
		{"found gone", http.StatusNotFound, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newFake(t)
			holder := pickGapHolder(f)
			waiting := pickReady(f, "p1")
			g := pickRig(t, f)
			searches, blockedBy := pickPass(g)
			pickGapNotPicked(g, waiting, searches, blockedBy, "with the worker held")
			f.AddComment(holder, "owner", "Still on it?")
			f.AnswerOnce(func(r githubtest.Request) bool {
				return r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/%d", holder)
			}, tt.status)
			if tt.err != "" {
				searches, blockedBy = pickGapPass(g, tt.err)
			} else {
				searches, blockedBy = pickPass(g)
			}
			pickGapNotPicked(g, waiting, searches, blockedBy, "the pass whose read of the holder failed")
			searches, blockedBy = pickPass(g)
			pickGapNotPicked(g, waiting, searches, blockedBy, "the pass after, its read answering again")
			if got := pickStage(f, holder); got != "stage:in-progress" {
				t.Errorf("the holder is at %s, want it left at stage:in-progress", got)
			}
			// Quiet again (SPEC §8.1, Polling cost): the skip costs no request.
			from := len(f.Requests())
			searches, blockedBy = pickPass(g)
			pickGapNotPicked(g, waiting, searches, blockedBy, "an unchanged pass after")
			for _, r := range f.Requests()[from:] {
				if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
					t.Errorf("an unchanged pass after sent %+v; want only conditional GETs answered 304", r)
				}
			}
			pickGapFreed(g, holder, waiting)
		})
	}
}

// A step 1 that fails on the task holding the worker, here on putting back
// its stage:in-progress label, which mallory, who isn't trusted, took off,
// leaves the pass with a snapshot that shows no stage: nothing is picked
// that pass, with no search and no blocked_by read. The next pass puts the
// label back, so nothing is picked either. Once the owner's /hold frees
// the worker, the ready task is picked as usual (SPEC §5.1, §8.1).
func TestPickNoneAfterHolderStep1Fails(t *testing.T) {
	t.Parallel()
	f := newFake(t)
	holder := pickGapHolder(f)
	waiting := pickReady(f, "p1")
	g := pickRig(t, f)
	searches, blockedBy := pickPass(g)
	pickGapNotPicked(g, waiting, searches, blockedBy, "with the worker held")
	f.RemoveLabel(holder, "mallory", "stage:in-progress")
	f.AnswerOnce(func(r githubtest.Request) bool {
		return r.Method != http.MethodGet && strings.HasPrefix(r.Path, fmt.Sprintf("/issues/%d/", holder))
	}, http.StatusInternalServerError)
	searches, blockedBy = pickGapPass(g, "500")
	pickGapNotPicked(g, waiting, searches, blockedBy, "the pass whose step 1 failed on the holder")
	if got := pickStage(f, holder); got != "" {
		t.Errorf("the holder is at %q after the failed pass, want no stage label yet", got)
	}
	searches, blockedBy = pickPass(g)
	pickGapNotPicked(g, waiting, searches, blockedBy, "the pass after, which puts the label back")
	if got := pickStage(f, holder); got != "stage:in-progress" {
		t.Errorf("the holder is at %q, want its stage:in-progress put back", got)
	}
	pickGapFreed(g, holder, waiting)
}

// The pass whose read of the task holding the worker fails logs, at
// debug, that nothing is picked, and why: the issue step 1 didn't finish
// on, and its gap.
func TestPickNoneAfterHolderReadFailsLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output (logtest.CaptureTo).
	f := newFake(t)
	holder := pickGapHolder(f)
	pickReady(f, "p1")
	g := pickRig(t, f)
	g.pass()
	f.AddComment(holder, "owner", "Still on it?")
	f.AnswerOnce(func(r githubtest.Request) bool {
		return r.Method == http.MethodGet && r.Path == fmt.Sprintf("/issues/%d", holder)
	}, http.StatusBadGateway)
	var debug bytes.Buffer
	stop := logtest.CaptureTo(t, &debug, logr.D)
	pickGapPass(g, "502")
	stop()
	want := fmt.Sprintf("no task is picked: step 1 didn't finish on every issue this pass, so a task holding the worker may be out of it: #%d, as its read failed", holder)
	if !strings.Contains(debug.String(), want) {
		t.Errorf("debug logs = %q, want %q", debug.String(), want)
	}
}
