package daemon_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// /resume on T2's child, #3, a task a plan's revision holds for its open
// PR #6 (SPEC §6.4), while it's held for discussion or by the owner too,
// on dropped_pr_test.go's story: the /resume ends the other hold, as the
// digest's discussion item says it does, and leaves the hold for the PR,
// so the task stays held for it.

// droppedResumeSelfText starts the App's record on #3 of the holds a
// /resume there ended, while it stays held for its PR.
const droppedResumeSelfText = "This `/resume` ended the holds on this issue from before it, but it stays on hold for a PR, " +
	"as a plan's revision dropped it, which `/resume` doesn't end.\n\n"

// droppedResumeSettled is droppedPRFake with T2's child at stage, after
// two passes of a daemon with the digest and assignment off, which hold
// it for its PR, and with discussed, after mallory's comment on it and two
// more passes, which hold it for discussion too. No write is logged since.
func droppedResumeSettled(t testing.TB, o *origin, stage string, discussed bool) *githubtest.Fake {
	t.Helper()
	f, r := droppedPRFake(t, o, stage)
	d := droppedPRDaemon(t, f, o, false, false)
	passes := func() {
		for range 2 {
			if err := d.Iterate(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	passes()
	if discussed {
		f.AddComment(r.t2, "mallory", "Keep this one, please.")
		passes()
	}
	f.ForgetWrites()
	return f
}

// droppedResumeAcked reports whether the App 👍'd the newest /resume on
// issue n, as it does a command it applied.
func droppedResumeAcked(f *githubtest.Fake, n int) bool {
	cs := f.State().Issues[n-1].Comments
	for j := len(cs) - 1; j >= 0; j-- {
		if cs[j].Body == "/resume" {
			return slices.Contains(cs[j].Reactions, app+" +1")
		}
	}
	return false
}

// droppedResumeCases are #3's holds besides the one for its PR: by
// mallory's comment, for discussion; and by the owner's /hold at pr-open,
// before the revision dropped it.
var droppedResumeCases = []struct {
	name, stage string
	discussed   bool
	// ended is the marker of the owner hold's end that the /resume's
	// record carries, if any.
	ended string
}{
	{"held for discussion", "pr-open", true, ""},
	{"owner-held", "held", false, "<!-- issuedog:ended reason=owner key=c"},
}

// /resume on #3, held for its PR and for discussion, or by the owner's
// /hold, applies: the App 👍s it and records on #3 that it ended the holds
// before it, but #3 stays held for its PR, which /resume doesn't end (the
// case of resolveResumed on the /resume's own issue). It writes no
// rejection. The next pass reads its own writes, and the one after, with
// nothing changed, sends only conditional GETs answered 304, and writes
// nothing: #3 is held for its PR alone, which no discussion's end records
// again. Then another /resume there is rejected, naming the PR, as one on
// a task held for its PR alone is.
func TestDroppedResumeOnTask(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	for _, tt := range droppedResumeCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := droppedResumeSettled(t, o, tt.stage, tt.discussed)
			d := droppedPRDaemon(t, f, o, false, false)
			cmd := f.AddComment(3, "owner", "/resume")
			iterate(t, d)
			if !droppedResumeAcked(f, 3) || anyComment(f, 3, "reason=rejected") {
				t.Errorf("the /resume on #3 is 👍'd %t, and rejected %t; want it applied", droppedResumeAcked(f, 3), anyComment(f, 3, "reason=rejected"))
			}
			if pickStage(f, 3) != "stage:held" || f.State().Issues[2].Closed {
				t.Errorf("T2's child is at %s, closed %t; want it held still, for its PR", pickStage(f, 3), f.State().Issues[2].Closed)
			}
			got := droppedPRNewest(f, 3)
			if !strings.HasPrefix(got, droppedResumeSelfText) || !strings.Contains(got, fmt.Sprintf("<!-- issuedog:ended reason=discussion cmd=%d resolved=", cmd)) {
				t.Errorf("T2's child's newest comment is\n%s\nwant it to start\n%s\nand record that /resume %d ended the discussion before it", got, droppedResumeSelfText, cmd)
			}
			if tt.ended != "" && (!strings.Contains(got, tt.ended) || !strings.Contains(got, fmt.Sprintf(" cmd=%d -->\n", cmd))) {
				t.Errorf("T2's child's newest comment is\n%s\nwant it to record the owner hold's end, %s…", got, tt.ended)
			}
			if tt.ended == "" && strings.Contains(got, "ended reason=owner") {
				t.Errorf("T2's child's newest comment is\n%s\nwant no owner hold's end recorded", got)
			}

			iterate(t, d) // reads its own writes
			droppedPRQuiet(t, f, d, "a pass with nothing changed")
			if pickStage(f, 3) != "stage:held" {
				t.Errorf("after the quiet pass, T2's child is at %s; want it held still", pickStage(f, 3))
			}

			again := f.AddComment(3, "owner", "/resume")
			iterate(t, d)
			want := fmt.Sprintf("> /resume\n\nI didn't apply this command: this issue is on hold for its PR #6, as a plan's revision dropped it: "+
				"it closes once the PR merges or closes, and `/resume` doesn't end that hold.\n\n<!-- issuedog:reply reason=rejected cmd=%d -->", again)
			if got := droppedPRNewest(f, 3); got != want {
				t.Errorf("after another /resume, #3's newest comment is\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// The /resume's record on #3 and its 👍 are written once each, whatever
// write a pass crashes at, then a fresh daemon's pass (SPEC §5.2).
func TestDroppedResumeOnTaskReplay(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	for _, tt := range droppedResumeCases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			setup := func(tb testing.TB) *githubtest.Fake {
				f := droppedResumeSettled(tb, o, tt.stage, tt.discussed)
				f.AddComment(3, "owner", "/resume")
				f.ForgetWrites()
				return f
			}
			githubtest.Replay(t, setup, droppedPRPass(t, o, false))
		})
	}
}

// The digest's discussion item for #3, held for its PR and for
// discussion, names no owner hold, as the hold for its PR isn't one: it
// has its own item, dropped-3, and /resume ends the discussion (SPEC
// §9.4). With the owner's /hold on #3 too, the item still names that
// owner hold, which stays once the discussion is resolved.
func TestDroppedResumeDigest(t *testing.T) {
	t.Parallel()
	o := droppedPROrigin(t)
	for _, tt := range []struct {
		name, stage string
		owner       bool
	}{
		{"held for discussion", "pr-open", false},
		{"owner-held and held for discussion", "held", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := droppedResumeSettled(t, o, tt.stage, true)
			d := droppedPRDaemon(t, f, o, true, false)
			iterate(t, d)
			iterate(t, d)
			body := f.State().Issues[len(f.State().Issues)-1].Body
			if !strings.Contains(body, "On hold for discussion, from `pr-open`") || !strings.Contains(body, "**Dropped with an open PR**") {
				t.Fatalf("the digest =\n%s\nwant #3's discussion item and its item for its PR", body)
			}
			if got := strings.Contains(body, "It's on an owner hold too"); got != tt.owner {
				t.Errorf("the digest =\n%s\nwant it to name an owner hold on #3 %t", body, tt.owner)
			}
		})
	}
}
