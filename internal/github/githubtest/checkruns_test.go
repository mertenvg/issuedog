package githubtest

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"
)

// checkRunsList lists the check runs on ref at query, as "<name> <app ID>
// <conclusion>", in the order served, with the total_count served.
func checkRunsList(t *testing.T, f *Fake, ref, query string) ([]string, int) {
	t.Helper()
	var out gogithub.ListCheckRunsResults
	if status, msg := pullDo(t, f, "GET", "repos/o/r/commits/"+ref+"/check-runs?"+query, nil, &out); status != http.StatusOK {
		t.Fatalf("list %s?%s: %d %s", ref, query, status, msg)
	}
	var got []string
	for _, r := range out.CheckRuns {
		got = append(got, fmt.Sprintf("%s %d %s", r.GetName(), r.GetApp().GetID(), r.GetConclusion()))
	}
	return got, out.GetTotal()
}

// checkRunPost posts body to /check-runs as the App, and returns the
// status, the message of a refusal, and the check run of a 201.
func checkRunPost(t *testing.T, f *Fake, body map[string]any) (int, string, gogithub.CheckRun) {
	t.Helper()
	var run gogithub.CheckRun
	status, msg := pullDo(t, f, "POST", "repos/o/r/check-runs", body, &run)
	return status, msg, run
}

// The list serves a commit's check runs oldest first, in GitHub's shape,
// filtered by check_name, app_id and status, the newest of each App's runs
// of each name by default (filter=latest), every one with filter=all, and
// none of another commit's.
func TestCheckRunsListFilters(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a")
	sha, main := r.tip("a"), r.tip("main")
	f.SetCheckRun(sha, "issuedog/verify", "failure", AppID)
	f.SetCheckRun(sha, "issuedog/verify", "success", 2)
	f.SetCheckRun(sha, "issuedog/tip", "success", AppID)
	f.SetCheckRun(sha, "issuedog/verify", "success", AppID)
	f.SetCheckRun(main, "issuedog/verify", "neutral", AppID)
	for _, tt := range []struct {
		query string
		want  []string
	}{
		{"filter=all", []string{"issuedog/verify 1 failure", "issuedog/verify 2 success", "issuedog/tip 1 success", "issuedog/verify 1 success"}},
		{"", []string{"issuedog/verify 2 success", "issuedog/tip 1 success", "issuedog/verify 1 success"}},
		{"filter=latest", []string{"issuedog/verify 2 success", "issuedog/tip 1 success", "issuedog/verify 1 success"}},
		{"filter=all&check_name=issuedog%2Fverify", []string{"issuedog/verify 1 failure", "issuedog/verify 2 success", "issuedog/verify 1 success"}},
		{"filter=all&app_id=1", []string{"issuedog/verify 1 failure", "issuedog/tip 1 success", "issuedog/verify 1 success"}},
		{"filter=all&app_id=2", []string{"issuedog/verify 2 success"}},
		{"filter=all&check_name=issuedog%2Fverify&app_id=1", []string{"issuedog/verify 1 failure", "issuedog/verify 1 success"}},
		{"filter=all&status=queued", nil},
		{"filter=all&check_name=other", nil},
	} {
		got, total := checkRunsList(t, f, sha, tt.query)
		if !reflect.DeepEqual(got, tt.want) || total != len(tt.want) {
			t.Errorf("list ?%s: %q, total_count %d; want %q", tt.query, got, total, tt.want)
		}
	}
	var all gogithub.ListCheckRunsResults
	if status, msg := pullDo(t, f, "GET", "repos/o/r/commits/"+sha+"/check-runs?filter=all", nil, &all); status != http.StatusOK {
		t.Fatalf("list: %d %s", status, msg)
	}
	var slugs []string
	for _, r := range all.CheckRuns {
		slugs = append(slugs, r.GetApp().GetSlug())
	}
	if want := []string{"issuedog", "app-2", "issuedog", "issuedog"}; !reflect.DeepEqual(slugs, want) {
		t.Errorf("Apps' slugs %q; want %q, the App's its own", slugs, want)
	}
	if got, _ := checkRunsList(t, f, main, ""); !reflect.DeepEqual(got, []string{"issuedog/verify 1 neutral"}) {
		t.Errorf("list on main: %q; want main's alone", got)
	}
	if got, total := checkRunsList(t, f, r.commit("a"), ""); got != nil || total != 0 {
		t.Errorf("list on a commit with none: %q, total_count %d; want none", got, total)
	}
}

// The list is paged as GitHub pages it, each page linking the next, with
// total_count every match, not the page's.
func TestCheckRunsListPages(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a")
	sha := r.tip("a")
	for i := range 5 {
		f.SetCheckRun(sha, fmt.Sprintf("c%d", i), "success", AppID)
	}
	var names []string
	for p := 1; p <= 3; p++ {
		got, total := checkRunsList(t, f, sha, fmt.Sprintf("per_page=2&page=%d", p))
		if total != 5 {
			t.Errorf("page %d: total_count %d; want 5", p, total)
		}
		names = append(names, got...)
	}
	want := []string{"c0 1 success", "c1 1 success", "c2 1 success", "c3 1 success", "c4 1 success"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("pages %q; want %q", names, want)
	}
	c := f.Client(t)
	req, err := c.NewRequest(t.Context(), "GET", "repos/o/r/commits/"+sha+"/check-runs?per_page=2", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.NextPage != 2 {
		t.Errorf("first page links page %d next; want 2", resp.NextPage)
	}
}

// A check run posted with a conclusion is completed, the App's, with
// AppID and the App's slug, its output and details URL; one with no
// status is queued. A refused list or post is GitHub's 422, or a 500 with
// no git remote, and posts nothing.
func TestCheckRunsCreate(t *testing.T) {
	t.Parallel()
	f, r := pullFake(t, "a")
	sha := r.tip("a")
	status, msg, run := checkRunPost(t, f, map[string]any{"name": "issuedog/verify", "head_sha": sha, "conclusion": "success",
		"details_url": "https://example.com/d", "output": map[string]string{"title": "T", "summary": "S"}})
	if status != http.StatusCreated {
		t.Fatalf("post: %d %s", status, msg)
	}
	if run.GetName() != "issuedog/verify" || run.GetHeadSHA() != sha || run.GetStatus() != "completed" || run.GetConclusion() != "success" ||
		run.GetApp().GetID() != AppID || run.GetApp().GetSlug() != "issuedog" || run.GetDetailsURL() != "https://example.com/d" ||
		run.GetOutput().GetTitle() != "T" || run.GetOutput().GetSummary() != "S" || run.GetID() == 0 || run.CompletedAt == nil {
		t.Errorf("posted %+v; want the App's completed success, with its output and details URL", run)
	}
	if status, msg, run := checkRunPost(t, f, map[string]any{"name": "queued", "head_sha": sha}); status != http.StatusCreated ||
		run.GetStatus() != "queued" || run.Conclusion != nil || run.CompletedAt != nil {
		t.Errorf("post with no status: %d %s %+v; want a queued run", status, msg, run)
	}
	main := r.tip("main")
	loose := r.git("commit-tree", r.git("mktree"), "-m", "on no branch")
	for _, tt := range []struct {
		name string
		body map[string]any
		want string
	}{
		{"no name", map[string]any{"head_sha": sha, "conclusion": "success"}, "needs a name and a head_sha"},
		{"no SHA", map[string]any{"name": "n", "conclusion": "success"}, "needs a name and a head_sha"},
		{"short SHA", map[string]any{"name": "n", "head_sha": sha[:7], "conclusion": "success"}, "is not a full commit SHA"},
		{"a branch", map[string]any{"name": "n", "head_sha": "a", "conclusion": "success"}, "is not a full commit SHA"},
		{"unknown SHA", map[string]any{"name": "n", "head_sha": strings.Repeat("0", 40), "conclusion": "success"}, "No commit found for SHA"},
		{"on no branch", map[string]any{"name": "n", "head_sha": loose, "conclusion": "success"}, "no branch holds " + loose},
		{"completed, no conclusion", map[string]any{"name": "n", "head_sha": sha, "status": "completed"}, "needs a conclusion"},
		{"stale", map[string]any{"name": "n", "head_sha": sha, "conclusion": "stale"}, `conclusion "stale"`},
		{"bad status", map[string]any{"name": "n", "head_sha": sha, "status": "done"}, `status "done"`},
		{"no summary", map[string]any{"name": "n", "head_sha": sha, "conclusion": "success", "output": map[string]string{"title": "T"}}, "a title and a summary"},
	} {
		if status, msg, _ := checkRunPost(t, f, tt.body); status != http.StatusUnprocessableEntity || !strings.Contains(msg, tt.want) {
			t.Errorf("%s: %d %q; want a 422 saying %q", tt.name, status, msg, tt.want)
		}
	}
	if status, msg, _ := checkRunPost(t, f, map[string]any{"name": "on main", "head_sha": main, "conclusion": "neutral"}); status != http.StatusCreated {
		t.Errorf("post on main's tip: %d %s; want a 201", status, msg)
	}
	for _, tt := range []struct{ ref, query, want string }{
		{"main", "", "isn't a full commit SHA"},
		{sha, "filter=newest", `filter "newest"`},
		{sha, "status=done", `status "done"`},
		{sha, "app_id=x", `app_id "x"`},
		{strings.Repeat("0", 40), "", "No commit found for SHA"},
	} {
		if status, msg := pullDo(t, f, "GET", "repos/o/r/commits/"+tt.ref+"/check-runs?"+tt.query, nil, nil); status != http.StatusUnprocessableEntity ||
			!strings.Contains(msg, tt.want) {
			t.Errorf("list %s?%s: %d %q; want a 422 saying %q", tt.ref, tt.query, status, msg, tt.want)
		}
	}
	want := []CheckRunState{{SHA: sha, Name: "issuedog/verify", Conclusion: "success", AppID: AppID}, {SHA: sha, Name: "queued", AppID: AppID},
		{SHA: main, Name: "on main", Conclusion: "neutral", AppID: AppID}}
	if sha > main {
		want = append(want[2:], want[:2]...)
	}
	if got := f.State().CheckRuns; !reflect.DeepEqual(got, want) {
		t.Errorf("State().CheckRuns %+v; want %+v, by SHA, each SHA's in the order posted", got, want)
	}
	bare := New(t, "o/r", "issuedog")
	if status, msg, _ := checkRunPost(t, bare, map[string]any{"name": "n", "head_sha": sha, "conclusion": "success"}); status != http.StatusInternalServerError ||
		!strings.Contains(msg, "call SetGitRemote") {
		t.Errorf("post with no git remote: %d %q; want a 500 naming SetGitRemote", status, msg)
	}
	if got := bare.State().CheckRuns; got != nil {
		t.Errorf("State().CheckRuns with no git remote %+v; want none", got)
	}
}
