package state

import (
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
)

// Sanctioned reports whether an edit by editor to content id, a comment ID
// or BodyID, by author is sanctioned (SPEC §5.1; #169). It is if
//   - the App made it: the App rewrites bodies others wrote, such as a
//     spec, so its edits always are;
//   - the author made it: for the body, once the App has rewritten it
//     whole, that is the App (AuthorEditWriter, which Dirty applies); or
//   - a user trusted holds as trusted now (SPEC §6.1) made it, unless the
//     content is a comment the App wrote. Such a comment carries state
//     markers, so only the App may edit it. The issue body counts as
//     people's here, whoever wrote it: Derive reads its markers only while
//     the App made its last edit, so a trusted edit doesn't make them
//     readable.
//
// An editor GitHub doesn't name never is, nor is a bot other than the App.
// trusted takes a login and may be nil, which trusts nobody. So an editor
// who loses trust makes their earlier edits unsanctioned again. Logins are
// compared as github normalises them, so the App is the Bot
// "<appSlug>[bot]".
func Sanctioned(id int64, author, editor github.Author, appSlug string, trusted func(login string) bool) bool {
	switch {
	case IsApp(editor, appSlug):
		return true
	case editor.Login == "":
		return false
	case editor.Login == author.Login:
		return true
	case editor.Type == "Bot" || (id != BodyID && IsApp(author, appSlug)):
		return false
	}
	return trusted != nil && trusted(editor.Login)
}

// IsApp reports whether a is the App: the Bot "<appSlug>[bot]".
func IsApp(a github.Author, appSlug string) bool {
	return appSlug != "" && a.Type == "Bot" && a.Login == appSlug+"[bot]"
}

// Dirty reports whether history, the history of content id by author, has
// an edit that isn't Sanctioned after the App's last full rewrite of it,
// and returns the oldest such editor. For a comment, that is any edit: the
// App never rewrites one whole. For the body, an edit before the newest
// entry that Rewrote is replaced, so it no longer counts (SPEC §5.1, #169). Each
// edit is judged with the author its text had before it
// (AuthorEditWriter): for the body, the App once it has rewritten it.
// Dirty content's markers are never read, and it is never a command or an
// answer (SPEC §5.1, §6.4). history is as github.History returns it,
// oldest first. trusted is as for Sanctioned.
func Dirty(id int64, author github.Author, history []github.Edit, appSlug string, trusted func(login string) bool) (github.Author, bool) {
	since := SinceRewrite(id, history, appSlug)
	from := len(history) - len(since)
	for i, e := range since {
		if !Sanctioned(id, AuthorEditWriter(id, author, history[:from+i], appSlug), e.By, appSlug, trusted) {
			return e.By, true
		}
	}
	return github.Author{}, false
}

// AuthorEditWriter returns whose text content id by author is after
// history, its entries so far, for Sanctioned to judge the next edit:
// author's, unless id is the body and the App has rewritten it whole
// (Rewrote), after which the text is the App's (SPEC §5.1). So the issue's
// author's own body edit after a rewrite is sanctioned only as anyone
// else's is, by trust: an untrusted author's makes the body dirty. Their
// edits before any rewrite still are. history is as for Dirty. An App
// entry whose text someone deleted may have been a rewrite, so it counts
// as one here, which fails closed: else deleting the rewrite's text from
// the history would sanction the author's edits after it again. The App's
// only other body edit is the note on a body that was dirty already
// (Applier.MarkDirty).
func AuthorEditWriter(id int64, author github.Author, history []github.Edit, appSlug string) github.Author {
	if id != BodyID {
		return author
	}
	for _, e := range history {
		if Rewrote(e, appSlug) || (e.Text == "" && IsApp(e.By, appSlug)) {
			return github.Author{Login: appSlug + "[bot]", Type: "Bot"}
		}
	}
	return author
}

// SinceRewrite returns the entries of history, the history of content id,
// that Dirty counts: all of a comment's, and only the body's after its
// newest entry that Rewrote, which leaves none if that is the last (SPEC
// §5.1, #169). So an editor whose edits all come before it decides nothing, and
// needn't be asked about (#174). history is as for Dirty, and the result
// shares its array.
func SinceRewrite(id int64, history []github.Edit, appSlug string) []github.Edit {
	from := 0
	if id == BodyID {
		for i, e := range history {
			if Rewrote(e, appSlug) {
				from = i + 1
			}
		}
	}
	return history[from:]
}

// RewriteKind is the kind of the marker on the first line of every whole
// body the App writes, <!-- issuedog:rewrite run=<run id> -->, with
// replaced=<login> when the body it replaced was dirty, naming who made it
// so (SPEC §5.1, #169).
const RewriteKind = "rewrite"

// Rewrote reports whether e, an entry of an issue body's history, is the
// App's full rewrite of the body: an edit by the App whose text starts
// with a rewrite marker (ParseRewrite). Nobody else can make it:
//   - Only the App's installation token edits as "<appSlug>[bot]", a Bot,
//     and GitHub records each edit's editor itself.
//   - The text is GitHub's own record of the body after the edit, its
//     userContentEdits diff, which nobody can edit. Someone with write
//     access can delete it, which leaves e.Text empty: the rewrite is then
//     not found, and the edits before it count again, which fails closed.
//   - The App escapes "<!--" in all text it didn't write (SPEC §5.1), so
//     any text of its own starting with an unescaped rewrite marker is one
//     it put there. It does so only on a whole body it writes; its other
//     body edit, the dirty note (daemon.Applier.MarkDirty), starts with
//     the dirty marker and escapes the text it keeps.
func Rewrote(e github.Edit, appSlug string) bool {
	_, _, ok := ParseRewrite(e.Text)
	return ok && IsApp(e.By, appSlug)
}

// ParseRewrite returns the rewrite marker on text's first line, and the
// text below that line, if the line is one exactly as github.FormatMarker
// writes it, with nothing else. A web edit may turn line endings into CRLF,
// and GitHub may return bare CRs (as BodyHash allows), so the line ends at
// the first CRLF, LF or CR. It doesn't check who wrote text: a rewrite
// marker means a full rewrite only in the App's text.
func ParseRewrite(text string) (marker github.Marker, rest string, ok bool) {
	line := text
	if i := strings.IndexAny(text, "\r\n"); i >= 0 {
		line, rest = text[:i], text[i+1:]
		if strings.HasPrefix(text[i:], "\r\n") {
			rest = text[i+2:]
		}
	}
	m := github.ParseMarkers(line)
	if len(m) != 1 || m[0].Kind != RewriteKind {
		return github.Marker{}, "", false
	}
	for k := range m[0].Fields {
		if k != "run" && k != "replaced" {
			return github.Marker{}, "", false
		}
	}
	if f, err := github.FormatMarker(m[0]); err != nil || f != line {
		return github.Marker{}, "", false
	}
	return m[0], rest, true
}
