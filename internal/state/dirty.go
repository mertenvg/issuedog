package state

import "github.com/mertenvg/issuedog/internal/github"

// Dirty reports whether anyone other than author made an edit in history,
// and returns the first such editor. Such content is dirty for good: its
// markers are never read, and it is never a command or an answer (SPEC
// §5.1, §6.4; #91). The App and trusted users are no exception: only the
// author's own edits are sanctioned. An edit GitHub names no editor for
// counts against the content, as no one can show it was the author's.
//
// Logins are compared as github normalises them, so the App's is
// "<slug>[bot]". history is as github.History returns it, oldest first.
func Dirty(author github.Author, history []github.Edit) (github.Author, bool) {
	for _, e := range history {
		if e.By.Login == "" || e.By.Login != author.Login {
			return e.By, true
		}
	}
	return github.Author{}, false
}
