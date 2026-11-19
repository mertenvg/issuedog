package daemon

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/mertenvg/issuedog/internal/repo"
)

// errTwoBranches is taskBranch's error when more than one branch has the
// task's prefix, so the daemon can't tell which is the task's (SPEC §7.5).
var errTwoBranches = errors.New("more than one branch has the task's prefix")

// taskSlugMax is the most characters a branch's slug has (SPEC §7.5).
const taskSlugMax = 40

// taskBranchPrefix is the prefix every branch of task n has,
// issuedog/<n>-, by which its branch is found (SPEC §7.5).
func taskBranchPrefix(n int) string { return repo.BranchPrefix + strconv.Itoa(n) + "-" }

// taskSlug is the slug of a new branch for a task titled title (SPEC
// §7.5): its lower-case ASCII letters and digits, any other run of
// characters a single "-", with none at either end, cut to taskSlugMax
// characters, or "task" when nothing is left.
func taskSlug(title string) string {
	var b strings.Builder
	dash := false
	for _, c := range strings.ToLower(title) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(c)
			dash = false
			continue
		}
		dash = true
	}
	slug := b.String()
	if len(slug) > taskSlugMax {
		slug = strings.TrimRight(slug[:taskSlugMax], "-")
	}
	if slug == "" {
		return "task"
	}
	return slug
}

// taskBranch names task n's branch and its tip, given title, the task's
// title, and branches, the bare repo's branches, name to tip, as
// repo.Repo.Branches lists them (SPEC §7.5). A branch with the task's
// prefix is the task's, whatever its slug, so a retitled task keeps its
// branch. With none, the branch is a new one, named from title, and its tip
// is "". With more than one, the error matches errTwoBranches and names
// them, and no branch is the task's.
func taskBranch(n int, title string, branches map[string]string) (name, tip string, err error) {
	prefix := taskBranchPrefix(n)
	var found []string
	for b := range branches {
		if strings.HasPrefix(b, prefix) {
			found = append(found, b)
		}
	}
	switch len(found) {
	case 0:
		return prefix + taskSlug(title), "", nil
	case 1:
		return found[0], branches[found[0]], nil
	}
	slices.Sort(found)
	return "", "", fmt.Errorf("task %d's branch: %w: %q", n, errTwoBranches, found)
}
