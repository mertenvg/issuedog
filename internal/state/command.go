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
// any argument, else it reports false. A known name glued to more text, as in
// "/approve.", has a bad argument (see Check); an unknown one, as in
// "/usr/bin", is plain text.
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
	"hold": nil, "resume": nil, "priority": nil, "model": nil, "effort": nil,
}

// takesArg are the commands that take an argument.
var takesArg = []string{"revise", "priority", "model", "effort"}

var priorityRE = regexp.MustCompile(`^p[0-3]$`)

// Check returns why c doesn't apply at stage, or nil. The error wraps
// ErrUnknownCommand, ErrBadArgument or ErrWrongStage, and reads as the reason
// in a rejection reply.
func (c Command) Check(stage Stage) error {
	stages, known := commands[c.Name]
	switch {
	case !known:
		return fmt.Errorf("%w `/%s`: the commands are `/approve`, `/revise <text>`, `/hold`, `/resume`, `/priority pN`, "+
			"`/model [verifier] <name>`, `/effort [verifier] <level>` and `/done`", ErrUnknownCommand, c.Name)
	case c.glued:
		return fmt.Errorf("%w: put a space between `/%s` and what follows it", ErrBadArgument, c.Name)
	case c.Name == "revise" && c.Text == "":
		return fmt.Errorf("%w: `/revise` needs the notes to send back", ErrBadArgument)
	case c.Name == "priority" && !priorityRE.MatchString(c.Arg):
		return fmt.Errorf("%w: `/priority` takes p0, p1, p2 or p3", ErrBadArgument)
	case c.Name == "model" && !c.setting():
		return fmt.Errorf("%w: `/model` takes a model name, or `verifier` and a model name", ErrBadArgument)
	case c.Name == "effort" && !c.setting():
		return fmt.Errorf("%w: `/effort` takes an effort level, or `verifier` and an effort level", ErrBadArgument)
	case c.Arg != "" && !slices.Contains(takesArg, c.Name):
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

// Setting returns the value a /model or /effort command sets, and whether it
// sets it for the verifier's runs: "/model verifier opus" sets opus for them,
// and "/model opus" for the others (plan #93, assumption 4). "verifier" is
// matched in any case. For an argument of any other shape, which Check
// rejects, value is "".
func (c Command) Setting() (value string, verifier bool) {
	switch f := strings.Fields(c.Arg); {
	case c.glued:
	case len(f) == 1 && !strings.EqualFold(f[0], "verifier"):
		return f[0], false
	case len(f) == 2 && strings.EqualFold(f[0], "verifier"):
		return f[1], true
	}
	return "", false
}

// setting reports whether c's argument is a value, or "verifier" and a value.
func (c Command) setting() bool {
	v, _ := c.Setting()
	return v != ""
}

// Inputs are s's input to the next stage run, oldest first: comments that
// counts accepts as trusted input, posted above is.Round, that aren't
// commands, and that no cmd or reply marker names. At clarifying, those are
// this round's answers (plan #5, amended assumption 5), after the revision
// request that sent the spec back, if one did, as the notes (SPEC §7.4). At
// planning, they are comments posted after the approval. It's only right
// after a successful daemon Step.Run on s, which posts a lost reply again:
// without it, a 👎'd command edited into plain text would be listed.
func (is Issue) Inputs(s *github.Snapshot, counts func(github.Comment) bool) []github.Comment {
	if is.Stage != Clarifying && is.Stage != Planning {
		return nil
	}
	notes := is.notes()
	var in []github.Comment
	for _, c := range s.Comments {
		if _, cmd := ParseCommand(c.Body); counts(c) && (c.ID == notes || c.ID > is.Round && !cmd && !is.Keys[CmdKey(c.ID)]) {
			in = append(in, c)
		}
	}
	return in
}

// notes returns the ID of the comment whose revision request sent the spec
// back, if the latest transition did so: from spec-review to clarifying,
// keyed by the comment. A reply marker naming it, as for an edit made after
// it was used, drops it. Otherwise it returns 0.
func (is Issue) notes() int64 {
	var last github.Marker
	for _, m := range is.Markers {
		if _, sets := RoundBound(m, 0); sets {
			last = m
		}
	}
	key := last.Fields["cmd"]
	id, ok := Key{"cmd", key}.Comment()
	if !ok || Stage(last.Fields["from"]) != SpecReview || Stage(last.Fields["to"]) != Clarifying {
		return 0
	}
	for _, m := range is.Markers {
		if m.Kind == "reply" && m.Fields["cmd"] == key {
			return 0
		}
	}
	return id
}
