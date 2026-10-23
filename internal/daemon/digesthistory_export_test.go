package daemon

import (
	"maps"
	"slices"
)

// DigestHistoryDigests returns the issues d takes as digests, sorted, for
// the title rule's tests in daemon_test.
func DigestHistoryDigests(d *Daemon) []int { return slices.Sorted(maps.Keys(d.digest.issues)) }
