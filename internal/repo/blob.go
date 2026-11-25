package repo

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// Blobs returns the content of each of paths that's a file in commit's
// tree, a regular file or a symlink, by path. A path the tree has no file
// at, as one it lacks, a dir or a submodule, is left out. Each path is
// matched as it is, never as a pattern (GIT_LITERAL_PATHSPECS). The blobs
// are read with cat-file, so no textconv or filter runs, and no attribute
// changes what's read. commit is a full SHA.
func (r *Repo) Blobs(ctx context.Context, commit string, paths []string) (map[string][]byte, error) {
	if err := checkSHAs(commit); err != nil {
		return nil, fmt.Errorf("read files: %w", err)
	}
	blobs := map[string][]byte{}
	if len(paths) == 0 {
		return blobs, nil
	}
	l := *r
	l.env = append(append([]string(nil), r.env...), "GIT_LITERAL_PATHSPECS=1")
	out, err := l.git(ctx, "", slices.Concat([]string{"ls-tree", "-r", "-z", commit, "--"}, paths)...)
	if err != nil {
		return nil, fmt.Errorf("read files at %s: %w", commit, err)
	}
	// With -z, each entry is "<mode> <type> <object>\t<path>", unquoted,
	// ended by a NUL. -r lists the files under a dir a path names, which
	// aren't asked for.
	ids := map[string]string{}
	var batch strings.Builder
	for _, entry := range nulFields(out) {
		meta, path, ok := strings.Cut(entry, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("read files at %s: bad ls-tree entry %q", commit, entry)
		}
		if fields[1] != "blob" || !slices.Contains(paths, path) {
			continue
		}
		ids[path] = fields[2]
		batch.WriteString(fields[2] + "\n")
	}
	if len(ids) == 0 {
		return blobs, nil
	}
	out, err = r.run(ctx, strings.NewReader(batch.String()), "", "cat-file", "--batch")
	if err != nil {
		return nil, fmt.Errorf("read files at %s: %w", commit, err)
	}
	contents, err := parseBatch(out)
	if err != nil {
		return nil, fmt.Errorf("read files at %s: %w", commit, err)
	}
	for path, id := range ids {
		content, ok := contents[id]
		if !ok {
			return nil, fmt.Errorf("read files at %s: cat-file gave no blob %s for %q", commit, id, path)
		}
		blobs[path] = content
	}
	return blobs, nil
}

// parseBatch reads cat-file --batch's output, each object's header,
// "<object> <type> <size>", then its content and a newline, by object.
// An object git reports missing, or one of any type but a blob, is an
// error.
func parseBatch(out []byte) (map[string][]byte, error) {
	br := bufio.NewReader(bytes.NewReader(out))
	contents := map[string][]byte{}
	for {
		header, err := br.ReadString('\n')
		if errors.Is(err, io.EOF) && header == "" {
			return contents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read cat-file's header %q: %w", header, err)
		}
		fields := strings.Fields(header)
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("cat-file gave %q, not a blob's header", strings.TrimSpace(header))
		}
		size, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("cat-file gave a bad size in %q: %w", strings.TrimSpace(header), err)
		}
		if size < 0 {
			return nil, fmt.Errorf("cat-file gave a negative size in %q", strings.TrimSpace(header))
		}
		content := make([]byte, size+1)
		if _, err := io.ReadFull(br, content); err != nil {
			return nil, fmt.Errorf("read blob %s: %w", fields[0], err)
		}
		if content[size] != '\n' {
			return nil, fmt.Errorf("read blob %s: no newline after its %d bytes", fields[0], size)
		}
		contents[fields[0]] = content[:size]
	}
}
