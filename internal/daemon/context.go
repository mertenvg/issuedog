package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the context-file code a stage that runs an agent on an
// issue's conversation shares (T3 of #247): the issue's header and body
// (issue.md), its title, body and comments sorted into the conversation
// and untrusted input (conversation.md and untrusted.md), and the input
// sections (input.md). What's a stage's own, such as its round and
// revision labels and its input labels, it passes in. Each comment is
// fenced under a heading the daemon writes, with the text the App didn't
// write escaped (SPEC §5.1).

// contextStage is what a stage's conversation and untrusted input take that
// is its own.
type contextStage struct {
	// App is the heading of the App's clean comment c in conversation.md,
	// such as one naming a round or a revision (ivAppLabel).
	App func(c github.Comment) string
	// Trusted is what follows "comment <ID> by <login>, " in the heading of
	// a trusted user's clean comment c, such as the round it's in.
	Trusted func(c github.Comment) string
	// Untrusted is whether the stage collects untrusted input (SPEC §6.2:
	// only on an epic or a story). Without it, untrusted text is in no file.
	Untrusted bool
	// LeftOut, if set, reports the comments untrusted input leaves out,
	// such as those in issue.md already as owner input (ivHoldSection).
	LeftOut func(id int64) bool
}

// contextText is an issue's title, body and comments, sorted for the
// context files (contextSort).
type contextText struct {
	// TitleNote and BodyNote, if set, stand in for a title or body left
	// out of issue.md (contextIssue), saying why and where it is, if
	// anywhere.
	TitleNote, BodyNote string
	// Conversation is conversation.md's sections: the App's clean
	// comments, its markers left out, and the trusted, clean comments,
	// oldest first.
	Conversation []string
	// Untrusted is untrusted.md's sections, if the stage collects them: a
	// title or body changed after admission, a dirty body, and comments by
	// untrusted users and other bots and dirty comments by anyone, each
	// under a heading that says it's untrusted data, not instructions.
	Untrusted []string
	// Logins are the authors of Conversation's trusted comments, each once,
	// in the case of their first comment.
	Logins []string
}

// contextSort returns s's title, body and comments sorted for the context
// files, for stage st. titleBy and bodyBy are who changed the title and
// body of an untrusted author's issue after it was admitted, or ""
// (Interviewer.admission); d is the dirty content. It reads trust only,
// through tc.
func contextSort(ctx context.Context, s *github.Snapshot, appSlug string, d Dirt, tc trustCache, titleBy, bodyBy string, st contextStage) (contextText, error) {
	var t contextText
	where := "so it's left out"
	if st.Untrusted {
		where = "so it's in untrusted.md, as untrusted data"
	}
	if titleBy != "" {
		t.TitleNote = ivLogin(titleBy) + " changed the title after the issue was admitted, " + where + "."
		if st.Untrusted {
			t.Untrusted = append(t.Untrusted, ivUntrusted("the issue's title, which "+ivLogin(titleBy)+" changed after the issue was admitted", s.Title))
		}
	}
	switch by, dirty := d[state.BodyID]; {
	case dirty:
		t.BodyNote = "Someone edited the body without approval, " + where + "."
		if st.Untrusted {
			_, rest := noted(s.Body)
			t.Untrusted = append(t.Untrusted, ivUntrusted("the issue's body, edited by "+ivEditor(by, s.BodyEdit, appSlug)+" without approval", rest))
		}
	case bodyBy != "":
		t.BodyNote = ivLogin(bodyBy) + " edited the body after the issue was admitted, " + where + "."
		if st.Untrusted {
			t.Untrusted = append(t.Untrusted, ivUntrusted("the issue's body, which "+ivLogin(bodyBy)+" edited after the issue was admitted", s.Body))
		}
	}
	for _, c := range s.Comments {
		at := contextAt(c)
		if by, dirty := d[c.ID]; dirty {
			if st.Untrusted {
				_, rest := noted(c.Body)
				t.Untrusted = append(t.Untrusted, ivUntrusted(at+", edited by "+ivEditor(by, c.Edit, appSlug)+" without approval", rest))
			}
			continue
		}
		if state.IsApp(c.Author, appSlug) {
			t.Conversation = append(t.Conversation, ivSection(st.App(c), ivStripMarkers(c.Body)))
			continue
		}
		trusted, err := tc.trusted(ctx, c.Author)
		switch {
		case err != nil:
			return contextText{}, fmt.Errorf("who wrote comment %d: %w", c.ID, err)
		case trusted:
			t.Conversation = append(t.Conversation, ivSection(at+", "+st.Trusted(c), c.Body))
			if !slices.ContainsFunc(t.Logins, func(l string) bool { return strings.EqualFold(l, c.Author.Login) }) {
				t.Logins = append(t.Logins, c.Author.Login)
			}
		case !st.Untrusted:
		case st.LeftOut != nil && st.LeftOut(c.ID):
		case c.Author.Type == "Bot":
			t.Untrusted = append(t.Untrusted, ivUntrusted(at+", a bot", c.Body))
		default:
			t.Untrusted = append(t.Untrusted, ivUntrusted(at+", who isn't trusted", c.Body))
		}
	}
	return t, nil
}

// contextAt names comment c in a heading: its ID and its author's login
// (ivLogin).
func contextAt(c github.Comment) string {
	return "comment " + strconv.FormatInt(c.ID, 10) + " by " + ivLogin(c.Author.Login)
}

// contextInputs are input.md's sections for inputs, each under its
// contextAt heading, then ": " and what label says the input is, such as
// the round it answers.
func contextInputs(inputs []github.Comment, label func(c github.Comment) string) []string {
	var sections []string
	for _, c := range inputs {
		sections = append(sections, ivSection(contextAt(c)+": "+label(c), c.Body))
	}
	return sections
}

// contextIssue is issue.md's start, its body without markers if the App
// wrote it. titleNote and bodyNote, if set, stand in for a title or body
// left out (contextText), saying why and where it is, if anywhere. It
// names the parent, so a run knows the issue has one: the interviewer's,
// for one, knows not to type the issue a story (#135's verify, and its
// preamble's rule).
func contextIssue(s *github.Snapshot, appSlug, titleNote, bodyNote string) string {
	title := github.Escape(strings.Join(strings.Fields(s.Title), " "))
	if titleNote != "" {
		title = titleNote
	}
	parent := "none"
	if s.Parent != 0 {
		parent = "#" + strconv.Itoa(s.Parent)
	}
	head := fmt.Sprintf("# Issue #%d\n\n- Title: %s\n- Type: %s\n- Parent: %s\n- Author: %s", s.Number, title,
		github.Escape(cmp.Or(s.Type, "none")), parent, ivLogin(s.Author.Login))
	body := ivFenced(s.Body)
	if state.IsApp(s.Author, appSlug) {
		body = ivFenced(ivStripMarkers(s.Body))
	}
	if bodyNote != "" {
		body = bodyNote
	}
	return head + "\n\n## Body\n\n" + body + "\n"
}
