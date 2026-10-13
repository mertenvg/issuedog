package daemon

import (
	"testing"
	"time"

	"github.com/mertenvg/issuedog/internal/github"
)

// The fake's clock moves a second per change, so it can't put an edit in
// the 👍's second; this pins that rule.
func TestEditedSince(t *testing.T) {
	acted := time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC)
	for _, tc := range []struct {
		name string
		edit time.Time
		want bool
	}{
		{"never edited", time.Time{}, false},
		{"edited before", acted.Add(-time.Second), false},
		{"edited in the same second", acted, true},
		{"edited after", acted.Add(time.Second), true},
	} {
		if got := editedSince(github.Comment{Edit: github.Edit{At: tc.edit}}, acted); got != tc.want {
			t.Errorf("%s: editedSince = %v, want %v", tc.name, got, tc.want)
		}
	}
}
