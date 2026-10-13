package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/agent"
)

// The lease is replaced by a rename, never written in place: a link to the
// old one keeps the old content, and no temp file is left.
func TestWriteLeaseReplaces(t *testing.T) {
	dir := t.TempDir()
	r := &Runs{Runner: &agent.Runner{StateDir: dir}}
	path := filepath.Join(dir, "lease.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, filepath.Join(dir, "old")); err != nil {
		t.Fatal(err)
	}
	want := Lease{Issue: 7, Stage: "clarifying", RunID: "ab12", Clone: "/work/runs/ab12",
		Started: time.Unix(100, 0).UTC(), Polled: time.Unix(90, 0).UTC(), Seen: 42, Edits: map[int64]time.Time{41: time.Unix(80, 0).UTC()}}
	if err := r.writeLease(want); err != nil {
		t.Fatal(err)
	}
	if old, err := os.ReadFile(filepath.Join(dir, "old")); err != nil || string(old) != "old" {
		t.Errorf("the old lease's other link reads %q, %v; want it untouched by a rename", old, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Lease
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Issue != want.Issue || got.Stage != want.Stage || got.RunID != want.RunID || got.Clone != want.Clone ||
		!got.Started.Equal(want.Started) || !got.Polled.Equal(want.Polled) || got.Seen != want.Seen || !got.Edits[41].Equal(want.Edits[41]) {
		t.Errorf("lease = %+v, want %+v", got, want)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("lease mode = %v, %v; want 0600", fi, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Errorf("state dir has %v, %v; want only the lease and the link", entries, err)
	}
}

// A lease that can't be read is removed, as nothing can resume it.
func TestReadLeaseRemovesBadOnes(t *testing.T) {
	for name, data := range map[string]string{
		"malformed": "{",
		"no run ID": `{"issue":1}`,
		"path":      `{"issue":1,"run_id":"../x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := &Runs{Runner: &agent.Runner{StateDir: t.TempDir()}}
			if err := os.WriteFile(r.leasePath(), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := r.readLease(); ok || err != nil {
				t.Errorf("readLease = %t, %v; want no lease and no error", ok, err)
			}
			if _, err := os.Stat(r.leasePath()); !os.IsNotExist(err) {
				t.Errorf("the lease is still there: %v", err)
			}
		})
	}
}
