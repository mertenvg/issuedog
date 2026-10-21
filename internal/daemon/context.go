package daemon

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/state"
)

// This file is the context-file code a stage that runs an agent on an
// issue's conversation shares (#254): the issue's header and body
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
// (contextAdmission); d is the dirty content. It reads trust only,
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

// contextAdmission returns who changed s's title and body after the issue
// was admitted, if its author isn't trusted (the owner's option (a) on #6):
// an approval approves what the approver read, not what the author writes
// later. Each is the login of the change that taints it, or "" if it's
// trusted as it stands. The App's issues, and trusted authors', are.
//   - The body is tainted by an edit at or after the admission
//     (ivAdmitted), unless the App made it, as when it wrote the spec. The
//     last edit is the only one GitHub dates, but any later one taints it,
//     whoever made it: someone else's makes it dirty too.
//   - The title is tainted by its latest rename, if that's at or after the
//     admission and by someone untrusted: a trusted user's later rename
//     makes it theirs.
//
// Times are to the second, so a change in the admission's second counts
// as after it. With no admission found, every change counts as after, and
// never is set: an untrusted author's issue that never entered the
// pipeline, such as an epic the owner put a story under, whose whole
// title and body the planner leaves out of parent.md (#312).
func contextAdmission(ctx context.Context, a *Applier, s *github.Snapshot, d Dirt, tc trustCache, tl *ivTimeline) (titleBy, bodyBy string, never bool, err error) {
	if state.IsApp(s.Author, a.AppSlug) {
		return "", "", false, nil
	}
	trusted, err := tc.trusted(ctx, s.Author)
	if err != nil {
		return "", "", false, fmt.Errorf("the issue's author: %w", err)
	}
	if trusted {
		return "", "", false, nil
	}
	timeline, err := tl.get(ctx)
	if err != nil {
		return "", "", false, fmt.Errorf("the admission of an untrusted author's issue: %w", err)
	}
	at, admitted, err := ivAdmitted(ctx, a, s, d, timeline, tc)
	if err != nil {
		return "", "", false, fmt.Errorf("the admission of an untrusted author's issue: %w", err)
	}
	after := func(t time.Time) bool { return !admitted || !t.Before(at) }
	if e := s.BodyEdit; !e.At.IsZero() && !state.IsApp(e.By, a.AppSlug) && after(e.At) {
		bodyBy = login(e.By)
	}
	var rename *gogithub.Timeline
	for _, e := range timeline {
		if e.GetEvent() == "renamed" {
			rename = e
		}
	}
	if rename == nil || !after(rename.GetCreatedAt().Time) {
		return "", bodyBy, !admitted, nil
	}
	actor := github.Author{Login: rename.GetActor().GetLogin(), Type: rename.GetActor().GetType()}
	if state.IsApp(actor, a.AppSlug) {
		return "", bodyBy, !admitted, nil
	}
	if trusted, err = tc.trusted(ctx, actor); err != nil {
		return "", "", false, fmt.Errorf("who renamed the issue: %w", err)
	}
	if !trusted {
		titleBy = login(actor)
	}
	return titleBy, bodyBy, !admitted, nil
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
// made its last edit, else escaped (plannerTrustedAppBody): a body the App
// rewrote as a spec is the App's, whoever filed it, and one a trusted user
// edited isn't (SPEC §5.1). titleNote and bodyNote, if set, stand in for a
// title or body left out (contextText), saying why and where it is, if
// anywhere, or for the planner, a title as of its last trusted change
// (plannerTrustedTitle). It names the parent, so a run knows the issue has
// one: the interviewer's, for one, knows not to type the issue a story
// (#135's verify, and its preamble's rule). more are header lines a stage
// adds after the Author line, such as the planner's priority.
func contextIssue(s *github.Snapshot, appSlug, titleNote, bodyNote string, more ...string) string {
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
	for _, l := range more {
		head += "\n" + l
	}
	body := ivFenced(s.Body)
	if plannerTrustedAppBody(s, appSlug) {
		body = ivFenced(ivStripMarkers(s.Body))
	}
	if bodyNote != "" {
		body = bodyNote
	}
	return head + "\n\n## Body\n\n" + body + "\n"
}
