package state

import "github.com/mertenvg/issuedog/internal/github"

// Sanctioned reports whether an edit by editor to content by author is
// sanctioned: only the author's own edits are, the App's and trusted users'
// included (#91). An editor GitHub doesn't name isn't, as no one can show
// it was the author. Logins are compared as github normalises them, so the
// App's is "<slug>[bot]".
func Sanctioned(author, editor github.Author) bool {
	return editor.Login != "" && editor.Login == author.Login
}

// Dirty reports whether history has an edit that isn't Sanctioned, and
// returns the oldest such editor. Such content is dirty for good: its
// markers are never read, and it is never a command or an answer (SPEC
// §5.1, §6.4). history is as github.History returns it, oldest first.
func Dirty(author github.Author, history []github.Edit) (github.Author, bool) {
	for _, e := range history {
		if !Sanctioned(author, e.By) {
			return e.By, true
		}
	}
	return github.Author{}, false
}
