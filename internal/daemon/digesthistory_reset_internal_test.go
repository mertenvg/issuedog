package daemon

import (
	"context"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/state"
)

// #614: a post of the digesthistory state error that works shows the
// held issue is there, so a gone answer before it counts no more
// (digestHistoryReport, SPEC §9.4, A failed history read). The post moves
// the issue's updated_at, so the next poll lists it, which resets the
// count too (digestState.listed, #579). So the passes after the first
// run digestTake alone, with no poll between, and the post's own reset is
// all that keeps the next NOT_FOUND the first of two again. Without it,
// that NOT_FOUND would let the issue go, which may be the digest, and a
// second digest would be created (#500).
func TestDigestHistoryPostResetsGone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := githubtest.New(t, "o/r", "issuedog")
	n := f.AddIssue("issuedog[bot]", "issuedog digest", "gone")
	cfg := config.Defaults()
	cfg.Loop.MaxAttempts = 1 // the first failed read posts the state error
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}},
		snaps: map[int]*github.Snapshot{}, stale: map[int]bool{}, Config: cfg}
	f.HistoryNotFoundOnce(n)
	if err := d.poll(ctx); err != nil {
		t.Fatal(err)
	}
	if h := d.digest.history[n]; d.digest.titled[n] == nil || h.gone == 0 {
		t.Fatalf("held %v, history %+v; want #%d held, with a gone answer counted", d.digest.titled, h, n)
	}
	f.FailGraphQL("Something went wrong")
	d.digestTake(ctx) // the read fails, and the state error's post works
	if !d.digest.historyPostedOn(n, state.BodyHash("gone")) || d.digest.titled[n] == nil {
		t.Fatalf("held %v, state errors %v; want #%d held, with its state error posted", d.digest.titled, d.digest.historyErrs, n)
	}
	if h := d.digest.history[n]; h.gone != 0 {
		t.Errorf("history %+v; want the gone answer counted no more after the post", h)
	}
	f.HistoryNotFoundOnce(n)
	d.digestTake(ctx)
	if h := d.digest.history[n]; d.digest.titled[n] == nil || h.gone != d.digest.pass {
		t.Errorf("held %v, history %+v; want #%d held still, this pass's NOT_FOUND the first of two", d.digest.titled, h, n)
	}
}
