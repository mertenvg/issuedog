package trust

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/repo"
)

var _ FileReader = (*repo.Repo)(nil)

// fakeFiles is main's tree: path to content.
type fakeFiles map[string]string

func (f fakeFiles) ReadFile(_ context.Context, path string) ([]byte, error) {
	data, ok := f[path]
	if !ok {
		return nil, fmt.Errorf("read main:%s: %w", path, repo.ErrNotFound)
	}
	return []byte(data), nil
}

// access is a user's permission on acme/app.
type access struct{ perm, role, typ string }

var writer = access{perm: "write", role: "write", typ: "User"}

// fakeAPI serves collaborator permissions on acme/app and acme's team members.
// Unknown users and teams are 404s.
func fakeAPI(t *testing.T, users map[string]access, teams map[string][]string) *gogithub.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/app/collaborators/{user}/permission", func(w http.ResponseWriter, r *http.Request) {
		a, ok := users[r.PathValue("user")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"permission": a.perm,
			"role_name":  a.role,
			"user":       map[string]any{"login": r.PathValue("user"), "type": a.typ},
		})
	})
	mux.HandleFunc("GET /orgs/acme/teams/{slug}/members", func(w http.ResponseWriter, r *http.Request) {
		members, ok := teams[r.PathValue("slug")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		var out []map[string]any
		for _, m := range members {
			out = append(out, map[string]any{"login": m, "type": "User"})
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, err := gogithub.NewClient(gogithub.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// check loads the set and asserts each login's trust.
func check(t *testing.T, files fakeFiles, api *gogithub.Client, want map[string]bool) {
	t.Helper()
	ctx := context.Background()
	s, err := Load(ctx, files, api, "acme", "app")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for login, w := range want {
		got, err := s.Trusted(ctx, login)
		if err != nil {
			t.Fatalf("Trusted(%s): %v", login, err)
		}
		if got != w {
			t.Errorf("Trusted(%s) = %v, want %v", login, got, w)
		}
	}
}

func TestLookupOrder(t *testing.T) {
	api := fakeAPI(t, map[string]access{"alice": writer, "bob": writer, "carol": writer}, nil)
	files := fakeFiles{
		".github/CODEOWNERS": "* @bob\n/.github/CODEOWNERS @Alice\n",
		"CODEOWNERS":         "/CODEOWNERS @bob\n",
		"docs/CODEOWNERS":    "/docs/CODEOWNERS @carol\n",
	}
	check(t, files, api, map[string]bool{"ALICE": true, "bob": false, "carol": false})

	delete(files, ".github/CODEOWNERS")
	check(t, files, api, map[string]bool{"alice": false, "bob": true, "carol": false})
}

func TestTeamOwnerMembersTrusted(t *testing.T) {
	api := fakeAPI(t, map[string]access{"dave": writer, "erin": writer, "frank": writer},
		map[string][]string{"core": {"Dave", "erin"}})
	files := fakeFiles{"CODEOWNERS": "CODEOWNERS @acme/core @acme/gone\n"}
	check(t, files, api, map[string]bool{"dave": true, "erin": true, "frank": false})
}

func TestEmailOwnerIgnoredAndLogged(t *testing.T) {
	var logs bytes.Buffer
	stop := logr.AddWriter(&logs)
	defer stop()

	api := fakeAPI(t, map[string]access{"alice": writer}, nil)
	files := fakeFiles{"CODEOWNERS": "/CODEOWNERS alice@example.com @alice\n"}
	check(t, files, api, map[string]bool{"alice": true, "alice@example.com": false})

	logr.Wait()
	if !strings.Contains(logs.String(), "alice@example.com") {
		t.Errorf("logs = %q, want the ignored email owner", logs.String())
	}
}

func TestOwnerWithoutWriteNotTrusted(t *testing.T) {
	api := fakeAPI(t, map[string]access{
		"alice": {perm: "admin", role: "admin", typ: "User"},
		"rita":  {perm: "read", role: "triage", typ: "User"},
	}, nil)
	files := fakeFiles{"CODEOWNERS": "/CODEOWNERS @alice @rita @nobody\n"}
	check(t, files, api, map[string]bool{"alice": true, "rita": false, "nobody": false})
}

func TestFallbackTrustsWriteMaintainAdmin(t *testing.T) {
	api := fakeAPI(t, map[string]access{
		"wanda": writer,
		"mia":   {perm: "write", role: "maintain", typ: "User"},
		"adam":  {perm: "admin", role: "admin", typ: "User"},
		"tom":   {perm: "read", role: "triage", typ: "User"},
		"rita":  {perm: "read", role: "read", typ: "User"},
	}, nil)
	check(t, fakeFiles{}, api, map[string]bool{
		"wanda": true, "mia": true, "adam": true, "tom": false, "rita": false, "stranger": false,
	})
}

func TestBotNeverTrusted(t *testing.T) {
	admin := access{perm: "admin", role: "admin", typ: "Bot"}
	api := fakeAPI(t, map[string]access{"issuedog[bot]": admin, "robo": admin},
		map[string][]string{"core": {"robo"}})
	check(t, fakeFiles{}, api, map[string]bool{"issuedog[bot]": false, "robo": false})
	check(t, fakeFiles{"CODEOWNERS": "/CODEOWNERS @acme/core\n"}, api, map[string]bool{"robo": false})
}
