// Package trust decides which GitHub users are trusted on a repo (SPEC §6.1).
package trust

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
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
		return nil, err
	}
	if path == "" {
		return s, nil
	}
	rules, err := codeowners.ParseFile(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	rule, err := rules.Match(path)
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
				return nil, err
			}
		default:
			logr.Warnf("%s: ignoring owner %s, since email owners can't be trusted", path, o)
		}
	}
	return s, nil
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

// addTeam adds the members of org/team to the owners.
func (s *Set) addTeam(ctx context.Context, team string) error {
	org, slug, _ := strings.Cut(team, "/")
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
// of case.
func (s *Set) Trusted(ctx context.Context, login string) (bool, error) {
	login = strings.ToLower(login)
	if login == "" || strings.HasSuffix(login, "[bot]") {
		return false, nil
	}
	if s.owners != nil && !s.owners[login] {
		return false, nil
	}
	perm, resp, err := s.api.Repositories.GetPermissionLevel(ctx, s.owner, s.repo, login)
	if notFound(resp) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("permission of %s on %s/%s: %w", login, s.owner, s.repo, err)
	}
	if perm.GetUser().GetType() == "Bot" {
		return false, nil
	}
	if s.owners != nil {
		// GitHub ignores code owners without write access. maintain reports
		// as write here, and custom roles as their base role.
		p := perm.GetPermission()
		return p == "write" || p == "admin", nil
	}
	switch perm.GetRoleName() {
	case "write", "maintain", "admin":
		return true, nil
	}
	return false, nil
}

func notFound(resp *gogithub.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}
