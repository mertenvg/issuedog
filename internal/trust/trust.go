// Package trust decides which GitHub users are trusted on a repo (SPEC §6.1).
package trust

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/hmarr/codeowners"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/repo"
)

// Locations is the order GitHub looks for CODEOWNERS in. The first file found
// is the only one read.
var Locations = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS"}

// FileReader reads a file from main. A missing file gives an error matching
// repo.ErrNotFound. *repo.Repo satisfies it.
type FileReader interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// Set answers whether a user is trusted on one repo. It holds the CODEOWNERS
// owners as read when it was loaded, and asks the API for permissions.
type Set struct {
	api         *gogithub.Client
	owner, repo string
	// owners holds the lowercase logins that own the CODEOWNERS file itself,
	// teams resolved. It is nil when main has no CODEOWNERS file.
	owners map[string]bool
}

// Load reads CODEOWNERS from main and resolves the owners of the file itself.
// owner/name is the repo on GitHub.
func Load(ctx context.Context, files FileReader, api *gogithub.Client, owner, name string) (*Set, error) {
	s := &Set{api: api, owner: owner, repo: name}
	path, data, err := find(ctx, files)
	if err != nil {
		return nil, fmt.Errorf("load trust for %s/%s: %w", owner, name, err)
	}
	if path == "" {
		return s, nil
	}
	rule, err := parse(path, data).Match(path)
	if err != nil {
		return nil, fmt.Errorf("match %s against itself: %w", path, err)
	}
	s.owners = map[string]bool{}
	if rule == nil {
		logr.Warnf("%s has no rule for itself, so nobody is trusted", path)
		return s, nil
	}
	for _, o := range rule.Owners {
		switch o.Type {
		case codeowners.UsernameOwner:
			s.owners[strings.ToLower(o.Value)] = true
		case codeowners.TeamOwner:
			if err := s.addTeam(ctx, o.Value); err != nil {
				return nil, fmt.Errorf("load trust for %s/%s: %w", owner, name, err)
			}
		default:
			logr.Warnf("%s: ignoring owner %s, since only users and teams can be trusted", path, o)
		}
	}
	return s, nil
}

// parse reads CODEOWNERS line by line and skips, with a warning, any line that
// doesn't parse, as GitHub does. An owner that isn't a user, team or email,
// such as an email the library's pattern rejects, is kept untyped and ignored.
func parse(path string, data []byte) codeowners.Ruleset {
	matchers := append(slices.Clone(codeowners.DefaultOwnerMatchers), codeowners.OwnerMatchFunc(
		func(s string) (codeowners.Owner, error) { return codeowners.Owner{Value: s}, nil }))
	var rules codeowners.Ruleset
	for i, line := range strings.Split(string(data), "\n") {
		r, err := codeowners.ParseFile(strings.NewReader(line), codeowners.WithOwnerMatchers(matchers))
		if err != nil {
			logr.Warnf("%s: skipping line %d, as GitHub does: %v", path, i+1, err)
			continue
		}
		rules = append(rules, r...)
	}
	return rules
}

// find returns the first CODEOWNERS file on main, or an empty path if there's
// none.
func find(ctx context.Context, files FileReader) (string, []byte, error) {
	for _, path := range Locations {
		data, err := files.ReadFile(ctx, path)
		if errors.Is(err, repo.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", nil, fmt.Errorf("find CODEOWNERS: %w", err)
		}
		return path, data, nil
	}
	return "", nil, nil
}

// addTeam adds the members of org/team to the owners. GitHub ignores a team
// that has no write access itself, even if its members have it another way.
func (s *Set) addTeam(ctx context.Context, team string) error {
	org, slug, _ := strings.Cut(team, "/")
	r, resp, err := s.api.Teams.IsTeamRepoBySlug(ctx, org, slug, s.owner, s.repo)
	if notFound(resp) || err == nil && !r.GetPermissions().GetPush() && !r.GetPermissions().GetAdmin() {
		logr.Warnf("ignoring CODEOWNERS team @%s, which has no write access to %s/%s", team, s.owner, s.repo)
		return nil
	}
	if err != nil {
		return fmt.Errorf("access of @%s to %s/%s: %w", team, s.owner, s.repo, err)
	}
	opts := &gogithub.TeamListTeamMembersOptions{ListOptions: gogithub.ListOptions{PerPage: 100}}
	for {
		users, resp, err := s.api.Teams.ListTeamMembersBySlug(ctx, org, slug, opts)
		if notFound(resp) {
			logr.Warnf("ignoring CODEOWNERS team @%s, which doesn't exist or isn't visible", team)
			return nil
		}
		if err != nil {
			return fmt.Errorf("list members of @%s: %w", team, err)
		}
		for _, u := range users {
			s.owners[strings.ToLower(u.GetLogin())] = true
		}
		if resp.NextPage == 0 {
			return nil
		}
		opts.Page = resp.NextPage
	}
}

// Trusted reports whether login is trusted. A CODEOWNERS owner must have write
// access. With no CODEOWNERS file, the user's role must be write, maintain or
// admin. Bots, the App included, are never trusted. Logins match regardless
// of case. A login CODEOWNERS doesn't name is untrusted without a request.
func (s *Set) Trusted(ctx context.Context, login string) (bool, error) {
	if s.owners != nil && !s.owners[strings.ToLower(login)] {
		return false, nil
	}
	trusted, _, err := s.DiscAccess(ctx, login)
	if errors.Is(err, ErrNoRecord) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("trust %s: %w", login, err)
	}
	return trusted, nil
}

// ErrNoRecord is wrapped by DiscAccess's error for a login GitHub has no
// permission record of, such as a removed collaborator or a deleted
// account: it answers 404, which no ETag saves, so the caller may remember
// it rather than ask again on every pass. Such a login is neither trusted
// nor a writer.
var ErrNoRecord = errors.New("no permission record")

// DiscNoWriters reports whether nobody can be a write collaborator who
// isn't trusted, as with no CODEOWNERS file every writer is trusted, so a
// caller needn't ask DiscAccess at all.
func (s *Set) DiscNoWriters() bool { return s.owners == nil }

// DiscAccess answers, from one permission request, whether login is trusted,
// as Trusted says, and whether they are a write collaborator who isn't
// trusted, who can hold an issue for discussion but not move it (SPEC §6.4,
// #87's assumption 1): their role_name is write, maintain or admin, and
// CODEOWNERS doesn't trust them. With no CODEOWNERS file every such role is
// trusted, so nobody is the second. Bots, the App included, are neither, so
// a bot's comments hold nothing. Unlike Trusted, it asks the API about a
// login CODEOWNERS doesn't name, as only the API knows their role. A login
// GitHub has no record of gives an error wrapping ErrNoRecord.
func (s *Set) DiscAccess(ctx context.Context, login string) (trusted, writer bool, err error) {
	login = strings.ToLower(login)
	if login == "" || strings.HasSuffix(login, "[bot]") {
		return false, false, nil
	}
	perm, resp, err := s.api.Repositories.GetPermissionLevel(ctx, s.owner, s.repo, login)
	if notFound(resp) {
		return false, false, fmt.Errorf("permission of %s on %s/%s: %w", login, s.owner, s.repo, ErrNoRecord)
	}
	if err != nil {
		return false, false, fmt.Errorf("permission of %s on %s/%s: %w", login, s.owner, s.repo, err)
	}
	if perm.GetUser().GetType() == "Bot" {
		return false, false, nil
	}
	switch perm.GetRoleName() {
	case "write", "maintain", "admin":
		writer = true
	}
	if s.owners == nil {
		return writer, false, nil
	}
	if s.owners[login] {
		// GitHub ignores code owners without write access. maintain reports
		// as write here, and custom roles as their base role.
		p := perm.GetPermission()
		trusted = p == "write" || p == "admin"
	}
	return trusted, writer && !trusted, nil
}

func notFound(resp *gogithub.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}
