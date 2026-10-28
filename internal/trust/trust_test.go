package trust

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/logtest"
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

// fakeAPI serves collaborator permissions on acme/app, and acme's team members
// and team access to acme/app: write, except for teams named readers-*.
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
	mux.HandleFunc("GET /orgs/acme/teams/{slug}/repos/acme/app", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := teams[r.PathValue("slug")]; !ok {
			http.NotFound(w, r)
			return
		}
		push := !strings.HasPrefix(r.PathValue("slug"), "readers-")
		_ = json.NewEncoder(w).Encode(map[string]any{"permissions": map[string]bool{"push": push}})
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
		map[string][]string{"core": {"Dave", "erin"}, "readers-all": {"frank"}})
	files := fakeFiles{"CODEOWNERS": "CODEOWNERS @acme/core @acme/gone @acme/readers-all\n"}
	check(t, files, api, map[string]bool{"dave": true, "erin": true, "frank": false})
}

func TestEmailOwnerIgnoredAndLogged(t *testing.T) {
	// Not parallel: it reads the process-wide logr output.
	var logs bytes.Buffer
	stop := logtest.CaptureTo(t, &logs, logr.All)
	defer stop()

	api := fakeAPI(t, map[string]access{"alice": writer}, nil)
	// An invalid line elsewhere is skipped, not fatal, as GitHub skips it.
	files := fakeFiles{"CODEOWNERS": "[ab] @bob\n/CODEOWNERS alice@example.com dev@corp.technology @alice\n"}
	check(t, files, api, map[string]bool{"alice": true, "alice@example.com": false})

	logr.Wait()
	for _, want := range []string{"alice@example.com", "dev@corp.technology", "skipping line 1"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("logs = %q, want %q", logs.String(), want)
		}
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

// discCheck loads the set and asserts each login's DiscAccess: trusted,
// and a write collaborator who isn't trusted. Trusted must agree.
func discCheck(t *testing.T, files fakeFiles, api *gogithub.Client, want map[string][2]bool) {
	t.Helper()
	ctx := context.Background()
	s, err := Load(ctx, files, api, "acme", "app")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for login, w := range want {
		trusted, writer, err := s.DiscAccess(ctx, login)
		if errors.Is(err, ErrNoRecord) != (login == "stranger") {
			t.Errorf("DiscAccess(%s) = %v; want no record only for stranger", login, err)
		} else if err != nil && !errors.Is(err, ErrNoRecord) {
			t.Fatalf("DiscAccess(%s): %v", login, err)
		}
		if got := [2]bool{trusted, writer}; got != w {
			t.Errorf("DiscAccess(%s) = trusted %v, writer %v; want %v", login, trusted, writer, w)
		}
		if tr, err := s.Trusted(ctx, login); err != nil || tr != trusted {
			t.Errorf("Trusted(%s) = %v, %v; want %v, as DiscAccess says", login, tr, err, trusted)
		}
	}
}

// Under CODEOWNERS, a writer, maintainer or admin it doesn't trust is a
// write collaborator who isn't trusted (SPEC §6.4). A trusted
// owner isn't, nor is an owner without write access, anyone with a lesser
// role, a stranger or a bot.
func TestDiscAccessUnderCodeowners(t *testing.T) {
	admin := access{perm: "admin", role: "admin", typ: "User"}
	api := fakeAPI(t, map[string]access{
		"alice": admin,
		"wanda": writer,
		"mia":   {perm: "write", role: "maintain", typ: "User"},
		"adam":  admin,
		"rita":  {perm: "read", role: "triage", typ: "User"},
		"tom":   {perm: "read", role: "triage", typ: "User"},
		"ron":   {perm: "read", role: "read", typ: "User"},
		"robo":  {perm: "admin", role: "admin", typ: "Bot"},
	}, nil)
	discCheck(t, fakeFiles{"CODEOWNERS": "/CODEOWNERS @alice @rita\n"}, api, map[string][2]bool{
		"alice": {true, false}, "Wanda": {false, true}, "mia": {false, true}, "adam": {false, true},
		"rita": {false, false}, "tom": {false, false}, "ron": {false, false}, "stranger": {false, false},
		"robo": {false, false}, "issuedog[bot]": {false, false}, "": {false, false},
	})
}

// With no CODEOWNERS file, every writer is trusted, so nobody is a write
// collaborator who isn't.
func TestDiscAccessWithoutCodeowners(t *testing.T) {
	api := fakeAPI(t, map[string]access{"wanda": writer, "tom": {perm: "read", role: "triage", typ: "User"}}, nil)
	discCheck(t, fakeFiles{}, api, map[string][2]bool{"wanda": {true, false}, "tom": {false, false}})
}

// Under CODEOWNERS, Trusted answers for a login it doesn't name with no
// request; only DiscAccess asks about them.
func TestTrustedNonOwnerAsksNothing(t *testing.T) {
	asked := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		asked++
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	api, err := gogithub.NewClient(gogithub.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Load(context.Background(), fakeFiles{"CODEOWNERS": "/CODEOWNERS @alice\n"}, api, "acme", "app")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Trusted(context.Background(), "bob"); ok || err != nil || asked != 0 {
		t.Errorf("Trusted(bob) = %v, %v after %d requests; want false with none", ok, err, asked)
	}
	if _, _, err := s.DiscAccess(context.Background(), "bob"); !errors.Is(err, ErrNoRecord) || asked != 1 {
		t.Errorf("DiscAccess(bob) = %v after %d requests; want one, with no record", err, asked)
	}
	if s.DiscNoWriters() {
		t.Error("DiscNoWriters under CODEOWNERS = true")
	}
	if none, err := Load(context.Background(), fakeFiles{}, api, "acme", "app"); err != nil || !none.DiscNoWriters() {
		t.Errorf("DiscNoWriters with no CODEOWNERS = false, %v", err)
	}
}

// A failed permission request is DiscAccess's error, and Trusted's.
func TestDiscAccessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	api, err := gogithub.NewClient(gogithub.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	s := &Set{api: api, owner: "acme", repo: "app"}
	if _, _, err := s.DiscAccess(context.Background(), "wanda"); err == nil {
		t.Error("DiscAccess on a 500: no error")
	}
	if _, err := s.Trusted(context.Background(), "wanda"); err == nil {
		t.Error("Trusted on a 500: no error")
	}
}
