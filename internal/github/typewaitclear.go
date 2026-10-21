package github

import "strings"

// TypeWaitClear forgets every issue type of owner/repo's issues that no
// event explains (issueTypeRevertGuard), held back or left out, and
// returns how many it forgot, keeping their unexplained labels. It's for a
// repo that no longer guards issue types, as after a switch to types:
// labels (#246): no CheckLabels there finds such a type explained or
// reverts it, so Due would name it due on every pass from then on, and
// the issue would be read on every pass.
func (u *Unexplained) TypeWaitClear(owner, repo string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	// The issues' keys, as unexplainedKey gives them, but for the number.
	prefix := owner + "/" + repo + "#"
	n := 0
	for issue := range u.types {
		if strings.HasPrefix(issue, prefix) {
			delete(u.types, issue)
			n++
		}
	}
	return n
}
