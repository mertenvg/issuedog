package daemon

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/verify"
)

// This file keeps a task's PR text on GitHub, and renders the PR from it
// (SPEC §5, §11.5): the implementer's title and summary, in a pr-text marker
// on its move to verifying, and the verifier's notes, in a verify-notes
// marker on its pass comment. Both are kept as plan-data keeps a plan
// (markerData), so the PR is built from GitHub alone, after any restart.

// The pr-text and verify-notes markers (SPEC §5):
//
//	<!-- issuedog:pr-text v=1 data=<base64url(deflate(JSON))> -->
//	<!-- issuedog:verify-notes v=1 data=<base64url(deflate(JSON))> -->
//
// v is the format's version, and a reader refuses any other. pr-text's data
// is the implementer's title and summary, as a JSON object with the schema's
// field names, and it goes after the transition marker of the implementer's
// move to verifying. verify-notes' data is the verifier's notes, as a JSON
// string, and it goes right after the pass comment's verify marker, before
// any verify-data marker. Only the App's markers are read.
const (
	prTextKind         = "pr-text"
	prTextVersion      = "1"
	verifyNotesKind    = "verify-notes"
	verifyNotesVersion = "1"
)

// prTextData is a pr-text marker's data: the implementer's title and
// summary, as its schema names them.
type prTextData struct {
	Title   *implementerTitle   `json:"title"`
	Summary *implementerSummary `json:"summary"`
}

// prTextMarker is the pr-text marker holding t and s, for Move.Markers.
func prTextMarker(t implementerTitle, s implementerSummary) (github.Marker, error) {
	data, err := markerData(prTextData{Title: &t, Summary: &s})
	if err != nil {
		return github.Marker{}, fmt.Errorf("pr text: %w", err)
	}
	return github.Marker{Kind: prTextKind, Fields: map[string]string{"v": prTextVersion, "data": data}}, nil
}

// readPRText reads the title and the summary back from c's pr-text marker,
// as prTextMarker wrote them. It reports false, with no error, when c has
// no pr-text marker the App wrote: one in a comment by anyone else, a
// malformed one ParseMarkers skips, or an escaped one in text the App quoted
// are no marker. One that doesn't read back is an error: a second one,
// another version, data that isn't base64url, raw DEFLATE, or JSON of
// exactly prTextData's fields, and a title or summary missing or null, or
// one checkCommits couldn't have passed: a title's type or sentence that
// isn't one line, a blank what_changed or why, or a test that isn't one
// line. It doesn't check whether c is dirty: the caller does, by reading
// only a comment Derive counts.
func readPRText(c github.Comment, appSlug string) (implementerTitle, implementerSummary, bool, error) {
	m, ok, err := oneDataMarker(c, appSlug, prTextKind, prTextVersion)
	if err != nil {
		return implementerTitle{}, implementerSummary{}, false, fmt.Errorf("pr text in %w", err)
	}
	if !ok {
		return implementerTitle{}, implementerSummary{}, false, nil
	}
	fail := func(format string, args ...any) (implementerTitle, implementerSummary, bool, error) {
		return implementerTitle{}, implementerSummary{}, false, fmt.Errorf("pr text in comment %d: %s", c.ID, fmt.Sprintf(format, args...))
	}
	var d prTextData
	if err := readMarkerData(m.Fields["data"], &d); err != nil {
		return implementerTitle{}, implementerSummary{}, false, fmt.Errorf("pr text in comment %d: %w", c.ID, err)
	}
	if d.Title == nil || d.Summary == nil {
		return fail("no title or no summary")
	}
	if !oneLine(d.Title.Type) || !oneLine(d.Title.Sentence) {
		return fail("the title's type or sentence is blank or more than one line")
	}
	if strings.TrimSpace(d.Summary.WhatChanged) == "" || strings.TrimSpace(d.Summary.Why) == "" {
		return fail("the summary's what_changed or why is blank")
	}
	for _, ts := range [][]string{d.Summary.TestsAdded, d.Summary.TestsUpdated} {
		for _, t := range ts {
			if !oneLine(t) {
				return fail("a test is blank or more than one line")
			}
		}
	}
	return *d.Title, *d.Summary, true, nil
}

// verifyNotesMarker is the verify-notes marker holding notes.
func verifyNotesMarker(notes string) (github.Marker, error) {
	data, err := markerData(notes)
	if err != nil {
		return github.Marker{}, fmt.Errorf("verify notes: %w", err)
	}
	return github.Marker{Kind: verifyNotesKind, Fields: map[string]string{"v": verifyNotesVersion, "data": data}}, nil
}

// readVerifyNotes reads the notes back from c's verify-notes marker, as
// verifyNotesMarker wrote them. It reports false, with no error, when c has
// no verify-notes marker the App wrote, as readPRText does. One that doesn't
// read back is an error: a second one, another version, and data that isn't
// base64url, raw DEFLATE, or a JSON string. It doesn't check whether c is
// dirty.
func readVerifyNotes(c github.Comment, appSlug string) (string, bool, error) {
	m, ok, err := oneDataMarker(c, appSlug, verifyNotesKind, verifyNotesVersion)
	if err != nil {
		return "", false, fmt.Errorf("verify notes in %w", err)
	}
	if !ok {
		return "", false, nil
	}
	var notes *string
	if err := readMarkerData(m.Fields["data"], &notes); err != nil {
		return "", false, fmt.Errorf("verify notes in comment %d: %w", c.ID, err)
	}
	if notes == nil {
		return "", false, fmt.Errorf("verify notes in comment %d: null, not a string", c.ID)
	}
	return *notes, true, nil
}

// oneDataMarker is c's one data marker of kind, at version, when the App
// wrote c (github.TrustedMarkers). It reports false, with no error, when c
// has none, and a second one or another version is an error, naming c,
// which the caller says what it was reading in.
func oneDataMarker(c github.Comment, appSlug, kind, version string) (github.Marker, bool, error) {
	var found []github.Marker
	for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
		if m.Kind == kind {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		return github.Marker{}, false, nil
	}
	if len(found) > 1 {
		return github.Marker{}, false, fmt.Errorf("comment %d: %d %s markers, not one", c.ID, len(found), kind)
	}
	if v := found[0].Fields["v"]; v != version {
		return github.Marker{}, false, fmt.Errorf("comment %d: version %q, not %s", c.ID, v, version)
	}
	return found[0], true, nil
}

// prSource is what a task's PR is rendered from, all of it read from
// GitHub: the task's number; the implementer's title and summary, from the
// pr-text marker on its move to verifying (readPRText), or nil for a move
// with none, as one made by hand, and SummaryLink, that move's comment; the
// verifier's notes, from its pass comment's verify-notes marker
// (readVerifyNotes), and VerifierLink, that comment; the head the pass
// verified, and main's SHA its checks ran against; and the checks' results
// on them, each of verify.commands.
type prSource struct {
	Task         int
	Title        *implementerTitle
	Summary      *implementerSummary
	SummaryLink  string
	Notes        string
	Head, Main   string
	Results      []verify.CommandResult
	VerifierLink string
}

// renderPRTitle is the PR's title (SPEC §11.5): the implementer's
// <type>: <sentence>, or for a move with no pr-text, the first of
// titleTypes, then the task's title, taskTitle. It's text the App didn't
// write, and a squash merge makes it the commit's subject (SPEC §15), so
// it's made one line, its mentions and issue references defused (defuse,
// defuseRefs) and its markers escaped. A title isn't Markdown, so nothing
// else in it needs escaping. It's cut to implementerMaxTitle, ending in "…",
// as GitHub refuses a longer one, and the defusing can make it longer.
func renderPRTitle(src prSource, titleTypes []string, taskTitle string) string {
	title := taskTitle
	if len(titleTypes) > 0 {
		title = titleTypes[0] + ": " + taskTitle
	}
	if src.Title != nil {
		title = src.Title.String()
	}
	title = github.Escape(defuseRefs(defuse(strings.TrimSpace(normalize(title, "")))))
	return cutText(title, implementerMaxTitle)
}

// The lines that stand in for text the PR's body doesn't hold.
const (
	prNoSummary  = "The implementer's summary isn't on #%d: the task's move to `verifying` keeps no `pr-text` data, as a move made by hand doesn't."
	prCutSummary = "The implementer's summary is too long for this description, so it's left out. It's in [its comment](%s)."
	prCutNotes   = "The verifier's notes are too long for this description, so they're left out. They're in [its comment](%s)."
)

// renderPRBody is the PR's body, in the SPEC §11.5 format: "Closes #N",
// then What changed, Why and Tests from the implementer's summary, Results,
// a table of each of verify.commands and its result, then a row saying
// rules 2 to 7 passed, then the verifier's notes, and last a line naming the
// head verified and main's SHA, and linking the verifier's comment. A move
// with no pr-text has a line saying the summary isn't on the issue in place
// of What changed, Why and Tests.
//
// Each agent field is made safe as a comment's is (block, inline), then its
// issue references are defused (defuseRefs), so on a squash merge, whose
// commit takes the title and the body (SPEC §15), only "Closes #N" closes
// anything, and the whole body's markers are escaped (SPEC §5.1). A command
// is the repo's config, but it's text the App didn't write, so it's
// defused and escaped too. A body over maxText, GitHub's limit, has the
// notes left out, or if that's not enough, the summary, or else both, each
// for a line saying so and linking its comment, so the PR always opens.
func renderPRBody(src prSource) string {
	for _, cut := range []struct{ summary, notes bool }{{false, false}, {false, true}, {true, false}} {
		if body := prBody(src, cut.summary, cut.notes); textLen(body) <= maxText {
			return body
		}
	}
	return prBody(src, true, true)
}

// prBody is renderPRBody's body, with the summary left out when cutSummary,
// and the notes when cutNotes, each for a line saying so.
func prBody(src prSource, cutSummary, cutNotes bool) string {
	parts := []string{fmt.Sprintf("Closes #%d", src.Task)}
	switch {
	case src.Title == nil || src.Summary == nil:
		parts = append(parts, "## What changed\n\n"+fmt.Sprintf(prNoSummary, src.Task))
	case cutSummary:
		parts = append(parts, "## What changed\n\n"+fmt.Sprintf(prCutSummary, src.SummaryLink))
	default:
		sum := src.Summary
		var tests []string
		for _, t := range sum.TestsAdded {
			tests = append(tests, "- added: "+defuseRefs(inline(t)))
		}
		for _, t := range sum.TestsUpdated {
			tests = append(tests, "- updated: "+defuseRefs(inline(t)))
		}
		parts = append(parts,
			"## What changed\n\n"+defuseRefs(block(sum.WhatChanged)),
			"## Why\n\n"+defuseRefs(block(sum.Why)),
			"## Tests\n\n"+orNone(strings.Join(tests, "\n")))
	}
	parts = append(parts, "## Results\n\n"+prResults(src.Results))
	notes := defuseRefs(block(src.Notes))
	if cutNotes {
		notes = fmt.Sprintf(prCutNotes, src.VerifierLink)
	}
	parts = append(parts, "## Verifier notes\n\n"+notes,
		fmt.Sprintf("Verified at `%s`, against `main` at `%s`: %s", src.Head, src.Main, src.VerifierLink))
	return github.Escape(strings.Join(parts, "\n\n"))
}

// prResults is the checks' results, as the PR's Results (SPEC §11.5) and
// its head's issuedog/verify check (SPEC §10.4) give them: a table of each
// of results, verify.commands' as the checks' pass gave them, and its
// result, then a row saying rules 2 to 7 passed. A command is the repo's
// config, but it's text the App didn't write, so it's cut, and defused
// for the squash merge's commit (SPEC §15); the caller escapes it.
func prResults(results []verify.CommandResult) string {
	rows := []string{"| Check | Result |", "|---|---|"}
	for _, r := range results {
		result := "pass"
		if why := r.Failure(); why != "" {
			result = why
		}
		cmd := strings.ReplaceAll(checkCode(defuseRefs(firstChars(r.Command, commandChars))), "|", `\|`)
		rows = append(rows, "| "+cmd+" | "+result+" |")
	}
	rows = append(rows, "| Rules 2 to 7 | pass |")
	return strings.Join(rows, "\n")
}

// zeroWidthSpace is what defuse and defuseRefs put in agent text to break a
// mention or a reference, which nobody sees.
const zeroWidthSpace = "​"

// refURLRE is the path of an issue's or a pull request's URL, up to its
// number's first digit, as in https://github.com/owner/repo/issues/12.
var refURLRE = regexp.MustCompile(`(?i)/(?:issues|pull)/[0-9]`)

// defuseRefs puts a zero-width space after each "#" before a digit, as in
// "#12" and "owner/repo#12", and after "/issues/" or "/pull/" before one in
// a URL, so no issue reference in text the App didn't write can close an
// issue: on a squash merge the commit takes the PR's title and body (SPEC
// §15), and a closing keyword works on a reference in a commit message
// whatever Markdown is around it, in a code fence or span too, so it's
// done everywhere. A "#" whose digits run into a letter, such as a hex
// colour's "#0969DA", is no reference, and like a "#" before anything
// else, as a heading's, it's left as it is. Text it defused already stays
// as it is.
func defuseRefs(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); i++ {
		b.WriteByte(text[i])
		if text[i] == '#' && refDigits(text[i+1:]) {
			b.WriteString(zeroWidthSpace)
		}
	}
	return refURLRE.ReplaceAllStringFunc(b.String(), func(m string) string {
		return m[:len(m)-1] + zeroWidthSpace + m[len(m)-1:]
	})
}

// refDigits reports whether s starts with an issue number: a digit, and a
// run of them that no ASCII letter follows.
func refDigits(s string) bool {
	if s == "" || !isDigit(s[0]) {
		return false
	}
	i := 0
	for i < len(s) && isDigit(s[i]) {
		i++
	}
	return i == len(s) || !('a' <= s[i]|0x20 && s[i]|0x20 <= 'z')
}

// isDigit reports whether c is an ASCII digit.
func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// cutText is s cut short to fit limit, as textLen counts it, at a
// character, ending in "…", or s when it fits.
func cutText(s string, limit int) string {
	if textLen(s) <= limit {
		return s
	}
	const more = "…"
	n, cut := textLen(more), 0
	for i, r := range s {
		if n += textLen(string(r)); n > limit {
			cut = i
			break
		}
	}
	return strings.TrimSpace(s[:cut]) + more
}
