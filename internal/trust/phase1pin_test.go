package trust

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// This file pins two trust behaviours epic #1's verifier found untested
// (#228): docs/CODEOWNERS as the third place CODEOWNERS is looked up, and
// a CODEOWNERS team whose members come back over more than one page.

// TestPhase1PinDocsCodeownersThird: with neither .github/CODEOWNERS nor
// CODEOWNERS on main, docs/CODEOWNERS is the file read, and only its owners
// are trusted. With either of the others there too, that one is read, and
// docs/CODEOWNERS isn't: it comes last.
func TestPhase1PinDocsCodeownersThird(t *testing.T) {
	t.Parallel()
	api := fakeAPI(t, map[string]access{"alice": writer, "bob": writer, "carol": writer}, nil)
	docs := "/docs/CODEOWNERS @carol\n"
	check(t, fakeFiles{"docs/CODEOWNERS": docs}, api,
		map[string]bool{"alice": false, "bob": false, "carol": true})
	check(t, fakeFiles{"CODEOWNERS": "/CODEOWNERS @bob\n", "docs/CODEOWNERS": docs}, api,
		map[string]bool{"alice": false, "bob": true, "carol": false})
	check(t, fakeFiles{".github/CODEOWNERS": "/.github/CODEOWNERS @alice\n", "docs/CODEOWNERS": docs}, api,
		map[string]bool{"alice": true, "bob": false, "carol": false})
}

// phase1PinPagedAPI serves acme/app's permissions, all write, for every
// login in members, and the team acme/core, with write access to acme/app,
// whose members are members, served perPage at a time with the Link header
// GitHub sends. It records the page each members request asked for.
func phase1PinPagedAPI(t *testing.T, members []string, perPage int) (*gogithub.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var pages []string
	known := map[string]bool{}
	for _, m := range members {
		known[m] = true
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/acme/app/collaborators/{user}/permission", func(w http.ResponseWriter, r *http.Request) {
		if !known[r.PathValue("user")] {
			http.NotFound(w, r)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"permission": "write", "role_name": "write",
			"user": map[string]any{"login": r.PathValue("user"), "type": "User"},
		}); err != nil {
			t.Error(err)
		}
	})
	mux.HandleFunc("GET /orgs/acme/teams/core/repos/acme/app", func(w http.ResponseWriter, _ *http.Request) {
		if err := json.NewEncoder(w).Encode(map[string]any{"permissions": map[string]bool{"push": true}}); err != nil {
			t.Error(err)
		}
	})
	var srv *httptest.Server
	mux.HandleFunc("GET /orgs/acme/teams/core/members", func(w http.ResponseWriter, r *http.Request) {
		page := 1
		if p := r.URL.Query().Get("page"); p != "" {
			var err error
			if page, err = strconv.Atoi(p); err != nil {
				http.Error(w, "bad page", http.StatusBadRequest)
				return
			}
		}
		mu.Lock()
		pages = append(pages, r.URL.Query().Get("page"))
		mu.Unlock()
		from, to := min((page-1)*perPage, len(members)), min(page*perPage, len(members))
		if to < len(members) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/teams/core/members?per_page=%d&page=%d>; rel="next"`, srv.URL, perPage, page+1))
		}
		out := []map[string]any{}
		for _, m := range members[from:to] {
			out = append(out, map[string]any{"login": m, "type": "User"})
		}
		if err := json.NewEncoder(w).Encode(out); err != nil {
			t.Error(err)
		}
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	base := srv.URL + "/"
	c, err := gogithub.NewClient(gogithub.WithURLs(&base, &base))
	if err != nil {
		t.Fatal(err)
	}
	return c, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), pages...)
	}
}

// TestPhase1PinTeamMembersPaged: a CODEOWNERS team of more members than
// one page holds has each page read, so a member on the second page is
// trusted as one on the first is. The pages are read in order, and no
// further once the last has no next link.
func TestPhase1PinTeamMembersPaged(t *testing.T) {
	t.Parallel()
	var members []string
	for i := range 100 {
		members = append(members, fmt.Sprintf("m%03d", i))
	}
	members = append(members, "zed")
	api, pages := phase1PinPagedAPI(t, members, 100)
	check(t, fakeFiles{"CODEOWNERS": "/CODEOWNERS @acme/core\n"}, api,
		map[string]bool{"m000": true, "m099": true, "zed": true, "stranger": false})
	if got := pages(); len(got) != 2 || got[0] != "" || got[1] != "2" {
		t.Errorf("the members requests asked for the pages %q; want the first, then 2", got)
	}
}
