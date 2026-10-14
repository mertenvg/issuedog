package agent

import (
	"bytes"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/mertenvg/issuedog/internal/repo"
	"github.com/mertenvg/logr/v2"
)

// PromptDir holds a repo's stage prompts on main (SPEC §3.4, §12).
const PromptDir = ".issuedog/prompts"

// MaxPromptSize is the largest prompt file read from main, 64 KiB.
const MaxPromptSize = 64 << 10

// shipped holds the default stage prompts, which a file on main overrides,
// and the stage schemas, which nothing overrides (SPEC §3.4). Embedding them
// means the image needs no extra files.
//
//go:embed prompts/*.md schemas/*.json
var shipped embed.FS

// Reader reads a file from main. A missing file gives an error matching
// repo.ErrNotFound. *repo.Repo satisfies it.
type Reader interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// Prompt returns the prompt for the stage name: PromptDir/<name>.md on
// main, or the shipped default when main has no such file. A file that is
// empty or only whitespace also gives the default, with a warning. override
// reports whether main's file was used. A name with no shipped default, a
// file over MaxPromptSize, and a read error other than not found are errors.
func Prompt(ctx context.Context, r Reader, name string) (prompt []byte, override bool, err error) {
	def, err := fs.ReadFile(shipped, "prompts/"+name+".md")
	if err != nil {
		return nil, false, fmt.Errorf("no shipped prompt for stage %q: %w", name, err)
	}
	path := PromptDir + "/" + name + ".md"
	data, err := r.ReadFile(ctx, path)
	switch {
	case errors.Is(err, repo.ErrNotFound):
		return def, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("read prompt %s: %w", path, err)
	case len(data) > MaxPromptSize:
		return nil, false, fmt.Errorf("prompt %s is %d bytes, over the limit of %d", path, len(data), MaxPromptSize)
	case len(bytes.TrimSpace(data)) == 0:
		logr.Warnf("prompt %s on main is blank; using the default %s prompt", path, name)
		return def, false, nil
	}
	return data, true, nil
}

// StageSchema returns the shipped JSON schema of the stage name's
// structured output.
func StageSchema(name string) ([]byte, error) {
	s, err := fs.ReadFile(shipped, "schemas/"+name+".json")
	if err != nil {
		return nil, fmt.Errorf("no schema for stage %q: %w", name, err)
	}
	return s, nil
}
