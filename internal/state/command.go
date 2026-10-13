package state

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
)

// Command is a comment whose first line is a command (SPEC §9.2), such as
// "/priority p1".
type Command struct {
	// Name is the word after the slash, in lower case. It may name no
	// command; see Check.
	Name string
	// Arg is the rest of the first line, trimmed.
	Arg string
	// Text is everything after the name, later lines included, trimmed:
	// /revise's notes.
	Text string
	// glued is set when Arg follows the name with no space between.
	glued bool
}

var commandRE = regexp.MustCompile(`^/([A-Za-z][A-Za-z0-9_-]*)(.*)$`)

// ParseCommand reads body's first line as a command: a slash, a word, and
// any argument. It reports false for anything else, which isn't a command.
// A known command's name followed by anything but a space, as in
// "/approve.", is that command with a bad argument (see Check), not plain
// text. An unknown word must end at a space or the line's end, so a path
// such as "/usr/bin" stays plain text.
func ParseCommand(body string) (Command, bool) {
	line, rest, _ := strings.Cut(strings.TrimSpace(body), "\n")
	m := commandRE.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return Command{}, false
	}
	name := strings.ToLower(m[1])
	_, known := commands[name]
	glued := m[2] != "" && m[2][0] != ' ' && m[2][0] != '\t'
	if glued && !known {
		return Command{}, false
	}
	arg := strings.TrimSpace(m[2])
	return Command{Name: name, Arg: arg, Text: strings.TrimSpace(arg + "\n" + rest), glued: glued}, true
}

// Why Check rejects a command.
var (
	ErrUnknownCommand = errors.New("unknown command")
	ErrBadArgument    = errors.New("bad argument")
	ErrWrongStage     = errors.New("wrong stage")
)

// commands are the known commands, each with the stages it applies at. Nil
// means any stage.
var commands = map[string][]Stage{
	"approve": {SpecReview, PlanReview}, "revise": {SpecReview, PlanReview}, "done": {NeedsHuman},
	"hold": nil, "resume": nil, "priority": nil,
}

var priorityRE = regexp.MustCompile(`^p[0-3]$`)

// Check returns why c doesn't apply at stage, or nil. The error wraps
// ErrUnknownCommand, ErrBadArgument or ErrWrongStage, and reads as the reason
// in a rejection reply.
func (c Command) Check(stage Stage) error {
	stages, known := commands[c.Name]
	switch {
	case !known:
		return fmt.Errorf("%w `/%s`: the commands are `/approve`, `/revise <text>`, `/hold`, `/resume`, `/priority pN` and `/done`",
			ErrUnknownCommand, c.Name)
	case c.glued:
		return fmt.Errorf("%w: put a space between `/%s` and what follows it", ErrBadArgument, c.Name)
	case c.Name == "revise" && c.Text == "":
		return fmt.Errorf("%w: `/revise` needs the notes to send back", ErrBadArgument)
	case c.Name == "priority" && !priorityRE.MatchString(c.Arg):
		return fmt.Errorf("%w: `/priority` takes p0, p1, p2 or p3", ErrBadArgument)
	case c.Name != "revise" && c.Name != "priority" && c.Arg != "":
		return fmt.Errorf("%w: `/%s` takes no argument", ErrBadArgument, c.Name)
	case stages != nil && !slices.Contains(stages, stage):
		names := make([]string, len(stages))
		for i, s := range stages {
			names[i] = "`" + string(s) + "`"
		}
		return fmt.Errorf("%w: `/%s` applies only at %s, and this issue is at `%s`", ErrWrongStage, c.Name, strings.Join(names, " or "), stage)
	}
	return nil
}

// Inputs are s's comments that are input to the next stage run and that no
// marker names yet (plan #5, assumption 5), oldest first. At clarifying,
// those are the answers: comments counts accepts that aren't commands (SPEC
// §9.1). counts reports whether a comment is trusted input (SPEC §6.2).
func (is Issue) Inputs(s *github.Snapshot, counts func(github.Comment) bool) []github.Comment {
	if is.Stage != Clarifying {
		return nil
	}
	var in []github.Comment
	for _, c := range s.Comments {
		if _, cmd := ParseCommand(c.Body); counts(c) && !cmd && !is.Keys[CmdKey(c.ID)] {
			in = append(in, c)
		}
	}
	return in
}
