package state

import "github.com/mertenvg/issuedog/internal/github"

// Sanctioned reports whether an edit by editor to content by author is
// sanctioned: it is if the author or the App made it (#91). The App
// rewrites bodies others wrote, such as a spec, so its edits always are.
// Anyone else's edit isn't, trusted users' included, and nor is one by an
// editor GitHub doesn't name. Logins are compared as github normalises
// them, so the App is the Bot "<appSlug>[bot]".
func Sanctioned(author, editor github.Author, appSlug string) bool {
	if editor.Login == "" {
		return false
	}
	app := appSlug != "" && editor.Type == "Bot" && editor.Login == appSlug+"[bot]"
	return app || editor.Login == author.Login
}

// Dirty reports whether history has an edit that isn't Sanctioned, and
// returns the oldest such editor. Such content is dirty for good: its
// markers are never read, and it is never a command or an answer (SPEC
// §5.1, §6.4). history is as github.History returns it, oldest first.
func Dirty(author github.Author, history []github.Edit, appSlug string) (github.Author, bool) {
	for _, e := range history {
		if !Sanctioned(author, e.By, appSlug) {
			return e.By, true
		}
	}
	return github.Author{}, false
}
