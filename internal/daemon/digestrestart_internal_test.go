package daemon

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
	"github.com/mertenvg/issuedog/internal/repo"
)

// A close the poll lists marks the issue as one whose items aren't cached
// only if it had items cached, which leave drops (digestRestartLeave): one
// with none had nothing for the digest to list, so a command that writes
// to it before its turn, after a reverted close, needn't read it again.
func TestDigestRestartLeaveCachedOnly(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	cached, none := f.AddIssue("owner", "Cached", ""), f.AddIssue("owner", "None", "")
	d := &Daemon{Step: &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}}, stale: map[int]bool{}}
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.digestRestartUnknown[cached] || !d.digestRestartUnknown[none] {
		t.Fatalf("unknown %v after the start's first poll; want both issues", d.digestRestartUnknown)
	}
	// Step 1 succeeds on both, and finds an item on one alone.
	d.digestRestartKnow(cached)
	d.digestRestartKnow(none)
	d.digestCache = map[int][]waitItem{cached: {{Issue: cached, Kind: waitError, Key: "err-1-labels-0", Title: "T", Link: "https://github.com/o/r/issues/1"}}}
	f.CloseBy(cached, "mallory")
	f.CloseBy(none, "mallory")
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.digestRestartUnknown[cached] || d.digestRestartUnknown[none] {
		t.Errorf("unknown %v after the closes; want #%d alone, which had items cached", d.digestRestartUnknown, cached)
	}
}

// The poll after the digest is dropped lists every issue again, and counts
// as a start's first for the issues it lists as changed alone: one it
// lists unchanged since its last read keeps its items cached, and isn't
// marked (SPEC §9.4). The passes after write nothing, and an unchanged one
// asks only for 304s.
func TestDigestRestartDropChangedOnly(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.SetPermission("owner", "admin")
	f.AddIssue("issuedog[bot]", "issuedog digest", digestBody("", nil))
	var tasks []int
	for range 2 {
		n := f.AddIssue("owner", "Mine", "")
		f.AddLabel(n, "issuedog[bot]", "stage:ready")
		f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=intake to=ready run=r1 -->")
		tasks = append(tasks, n)
	}
	changed, unchanged := tasks[0], tasks[1]
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: ownerOnly{}}
	d := &Daemon{Step: st, Clone: repo.New(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults(), Digest: true}
	for range 2 {
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(d.digestRestartUnknown) != 0 {
		t.Fatalf("unknown %v after two passes; want none", d.digestRestartUnknown)
	}
	d.digestDrop(1, errors.New("404"))
	f.EditBody(changed, "owner", "Edited.")
	if err := d.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.digestRestartUnknown[changed] || d.digestRestartUnknown[unchanged] {
		t.Errorf("unknown %v after the poll that lists every issue again; want #%d alone, which changed", d.digestRestartUnknown, changed)
	}
	writes := len(f.Writes())
	for range 2 {
		if err := d.Iterate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if w := f.Writes()[writes:]; len(w) != 0 {
		t.Errorf("writes %+v after the digest was found again; want none", w)
	}
	from := len(f.Requests())
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, r := range f.Requests()[from:] {
		if r.Method != http.MethodGet || !r.Conditional || r.Status != http.StatusNotModified {
			t.Errorf("an unchanged pass sent %+v; want only conditional GETs answered 304", r)
		}
	}
}

// An issue dropped as gone, on two reads, forgets that its items aren't
// cached, as it forgets its items: GitHub never reuses its number.
func TestDigestRestartGoneForgotten(t *testing.T) {
	t.Parallel()
	f := githubtest.New(t, "o/r", "issuedog")
	f.SetPermission("owner", "admin")
	f.AddIssue("issuedog[bot]", "issuedog digest", digestBody("", nil))
	n := f.AddIssue("owner", "Mine", "")
	f.AddLabel(n, "issuedog[bot]", "stage:ready")
	f.AddComment(n, "issuedog[bot]", "<!-- issuedog:transition from=intake to=ready run=r1 -->")
	st := &Step{Applier: &Applier{Client: f.Client(t), Owner: "o", Repo: "r", AppSlug: "issuedog"}, Trust: ownerOnly{}}
	d := &Daemon{Step: st, Clone: repo.New(filepath.Join(t.TempDir(), "none"), filepath.Join(t.TempDir(), "r.git")), Config: config.Defaults(), Digest: true}
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	d.digestRestartUnknown[n] = true // as after a visit that failed
	f.DeleteIssue()
	d.reread(n, true) // one read found it gone already
	if err := d.Iterate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.digestCache[n]; ok || d.digestRestartUnknown[n] {
		t.Errorf("cache %+v, unknown %v; want #%d in neither", d.digestCache, d.digestRestartUnknown, n)
	}
}
