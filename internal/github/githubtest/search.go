package githubtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"unicode"
)

// searchEntry is one issue as the issue search sees it: its number, the
// item the search answers with it, and what its qualifiers read.
type searchEntry struct {
	number  int
	item    json.RawMessage // the issue as the issue list gives it (withDependencies), as JSON, so a frozen one keeps its bytes
	open    bool
	pr      bool
	blocked bool     // one of its blocked-by issues is open
	labels  []string // in lower case
}

// searchAnswer is the issue search's answer, one page of it.
type searchAnswer struct {
	TotalCount        int               `json:"total_count"`
	IncompleteResults bool              `json:"incomplete_results"`
	Items             []json.RawMessage `json:"items"`
}

// searchQuery is a parsed issue search query: what each qualifier asks.
type searchQuery struct {
	issueOnly, openOnly, blocked bool
	labels                       []string // in lower case, all of which an issue must have
}

// handleSearch registers the issue search, which is outside the repo's
// path, but served as any read is (serve), with ETags and 304s.
func (f *Fake) handleSearch() {
	f.mux.HandleFunc("GET /search/issues", func(w http.ResponseWriter, r *http.Request) {
		f.serve(w, r, false, f.searchIssues)
	})
}

// FreezeSearch makes the issue search answer from the issues as they are
// now until ThawSearch, to model search lagging behind a blocked-by link, a
// close or a label change. A frozen answer keeps its items and its ETag, so
// a repeat of a search is a 304 however the issues change meanwhile. An
// issue filed meanwhile isn't found. A second FreezeSearch takes the issues
// as they are then. A blocker the fake hasn't got, which no helper leaves
// (fakeNumberFind), fails the test.
func (f *Fake) FreezeSearch() {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	entries, err := f.searchEntries()
	if err != nil {
		f.fakeFail("githubtest: freeze the search: %v", err)
	}
	f.searchFrozen = entries
}

// ThawSearch makes the issue search answer from the issues as they are
// again, after FreezeSearch.
func (f *Fake) ThawSearch() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchFrozen = nil
}

// searchIssues serves the issue search (GET /search/issues) for advanced
// search's q, which must name this repo with repo:, as the fake has only
// it. It reads the qualifiers is:issue, is:open, label: and is:blocked, and
// any other, free text included, is a 422, as is a search that isn't
// advanced (advanced_search=true), so a typo or a missed qualifier fails
// rather than matching more. An issue is blocked when one of its blocked-by
// issues is open. It answers the issues that match, by number, paged by
// per_page and page, each as the issue list gives it, with the total. A
// transferred issue isn't found, as it isn't listed (transfer.go). While
// FreezeSearch holds, it answers from the issues as they were then.
func (f *Fake) searchIssues(c *call) (int, any) {
	if c.r.URL.Query().Get("advanced_search") != "true" {
		return http.StatusUnprocessableEntity, message("Validation Failed: githubtest serves advanced search only (advanced_search=true)")
	}
	query, err := f.searchParse(c.r.URL.Query().Get("q"))
	if err != nil {
		return http.StatusUnprocessableEntity, message("Validation Failed: " + err.Error())
	}
	entries := f.searchFrozen
	if entries == nil {
		if entries, err = f.searchEntries(); err != nil {
			return http.StatusInternalServerError, message("githubtest: " + err.Error())
		}
	}
	matched := []json.RawMessage{}
	for _, e := range entries {
		if query.matches(e) {
			matched = append(matched, e.item)
		}
	}
	status, v := page(c, matched)
	items, ok := v.([]json.RawMessage)
	if !ok {
		return http.StatusInternalServerError, message(fmt.Sprintf("githubtest: a page of the search is %T", v))
	}
	return status, searchAnswer{TotalCount: len(matched), Items: items}
}

// searchEntries is every issue this repo lists, by number, as the issue
// search sees it now. f is locked.
func (f *Fake) searchEntries() ([]searchEntry, error) {
	entries := []searchEntry{}
	for _, is := range f.issues {
		if !is.listed() {
			continue
		}
		e := searchEntry{number: is.GetNumber(), open: is.GetState() == "open", pr: is.IsPullRequest()}
		for _, l := range is.Labels {
			e.labels = append(e.labels, strings.ToLower(l.GetName()))
		}
		for _, n := range is.blockedBy {
			blocker, err := f.fakeNumberFind(n)
			if err != nil {
				return nil, fmt.Errorf("a blocker of #%d: %w", is.GetNumber(), err)
			}
			if blocker.GetState() == "open" {
				e.blocked = true
			}
		}
		item, err := json.Marshal(f.withDependencies(is))
		if err != nil {
			return nil, fmt.Errorf("issue #%d as a search item: %w", is.GetNumber(), err)
		}
		e.item = item
		entries = append(entries, e)
	}
	return entries, nil
}

// searchParse reads q's qualifiers, or returns the error to answer 422
// with: for a qualifier it doesn't read, free text, an unclosed quote, and
// a q with no repo: or one naming another repo.
func (f *Fake) searchParse(q string) (searchQuery, error) {
	var query searchQuery
	terms, err := searchTerms(q)
	if err != nil {
		return query, fmt.Errorf("split q: %w", err)
	}
	repo := false
	for _, term := range terms {
		key, value, _ := strings.Cut(term, ":")
		switch {
		case key == "repo" && strings.EqualFold(value, f.repo):
			repo = true
		case key == "repo":
			return query, fmt.Errorf("githubtest has only %s, not %q", f.repo, value)
		case term == "is:issue":
			query.issueOnly = true
		case term == "is:open":
			query.openOnly = true
		case term == "is:blocked":
			query.blocked = true
		case key == "label" && value != "":
			query.labels = append(query.labels, strings.ToLower(value))
		default:
			return query, fmt.Errorf("githubtest doesn't read %q in q=%q", term, q)
		}
	}
	if !repo {
		return query, fmt.Errorf("githubtest needs repo:%s in q=%q", f.repo, q)
	}
	return query, nil
}

// searchTerms splits q at spaces outside double quotes, and takes the
// quotes out, so label:"stage:ready" is the term label:stage:ready. An
// unclosed quote is an error.
func searchTerms(q string) ([]string, error) {
	var terms []string
	var term strings.Builder
	quoted, started := false, false
	for _, r := range q {
		switch {
		case r == '"':
			quoted, started = !quoted, true
		case unicode.IsSpace(r) && !quoted:
			if started {
				terms = append(terms, term.String())
			}
			term.Reset()
			started = false
		default:
			term.WriteRune(r)
			started = true
		}
	}
	if quoted {
		return nil, fmt.Errorf("an unclosed quote in q=%q", q)
	}
	if started {
		terms = append(terms, term.String())
	}
	return terms, nil
}

// matches reports whether e meets every qualifier of the query.
func (query searchQuery) matches(e searchEntry) bool {
	switch {
	case query.issueOnly && e.pr, query.openOnly && !e.open, query.blocked && !e.blocked:
		return false
	}
	for _, l := range query.labels {
		if !slices.Contains(e.labels, l) {
			return false
		}
	}
	return true
}
