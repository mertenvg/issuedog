package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	gogithub "github.com/google/go-github/v92/github"

	"github.com/mertenvg/issuedog/internal/github"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// checkRunsQueries are the queries of the check run lists on sha among the
// fake's requests from from on.
func checkRunsQueries(f *githubtest.Fake, from int, sha string) []string {
	var queries []string
	for _, r := range f.Requests()[from:] {
		if r.Method == http.MethodGet && r.Path == "/commits/"+sha+"/check-runs" {
			queries = append(queries, r.Query)
		}
	}
	return queries
}

// VerifyChecks lists every issuedog/verify run the App posted on the SHA,
// over pages of 100, newest first, though the fake serves them oldest
// first, and leaves out another App's run of that name, the App's run of
// another name, and its runs on another SHA.
func TestVerifyChecks(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task")
	sha, main := git("rev-parse", "refs/heads/issuedog/7-task"), git("rev-parse", "refs/heads/main")
	for i := range 101 {
		conclusion := "failure"
		if i%2 == 0 {
			conclusion = "success"
		}
		f.SetCheckRun(sha, github.VerifyCheck, conclusion, githubtest.AppID)
		if i == 50 {
			f.SetCheckRun(sha, github.VerifyCheck, "success", 2)
			f.SetCheckRun(sha, "issuedog/tip", "success", githubtest.AppID)
			f.SetCheckRun(main, github.VerifyCheck, "success", githubtest.AppID)
		}
	}
	c, ctx := f.Client(t), context.Background()
	from := len(f.Requests())
	runs, err := c.VerifyChecks(ctx, "o", "r", sha)
	if err != nil {
		t.Fatal(err)
	}
	queries := checkRunsQueries(f, from, sha)
	if len(queries) != 2 || !strings.Contains(queries[0], "check_name=issuedog%2Fverify") || !strings.Contains(queries[0], "app_id=1") ||
		!strings.Contains(queries[0], "filter=all") || !strings.Contains(queries[0], "per_page=100") || !strings.Contains(queries[1], "page=2") {
		t.Errorf("queries %q; want two pages of 100, check_name=issuedog/verify, app_id=1, filter=all", queries)
	}
	if len(runs) != 101 {
		t.Fatalf("listed %d; want the App's 101", len(runs))
	}
	for i, r := range runs {
		if r.Name != github.VerifyCheck || r.AppID != githubtest.AppID || r.HeadSHA != sha || r.Status != "completed" || r.ID == 0 {
			t.Errorf("run %d %+v; want the App's %s on %s, completed", i, r, github.VerifyCheck, sha)
		}
		if i > 0 && r.ID >= runs[i-1].ID {
			t.Errorf("run %d has ID %d after %d; want newest first", i, r.ID, runs[i-1].ID)
		}
	}
	if runs[0].Conclusion != "success" || runs[1].Conclusion != "failure" {
		t.Errorf("newest two %+v, %+v; want the last posted, success, then failure", runs[0], runs[1])
	}
}

// The App's ID is the one the client was made with, ISSUEDOG_APP_ID's, and
// VerifyChecks lists only that App's runs.
func TestVerifyChecksAppID(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task")
	sha := git("rev-parse", "refs/heads/issuedog/7-task")
	f.SetCheckRun(sha, github.VerifyCheck, "failure", githubtest.AppID)
	f.SetCheckRun(sha, github.VerifyCheck, "success", 42)
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
	c, err := github.New(github.Config{AppID: 42, InstallationID: 1, Key: key, BaseURL: f.URL(), Transport: f.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	if got := c.AppID(); got != 42 {
		t.Errorf("AppID %d; want 42, the Config's", got)
	}
	if got := f.Client(t).AppID(); got != githubtest.AppID {
		t.Errorf("the fake's client's AppID %d; want %d", got, githubtest.AppID)
	}
	runs, err := c.VerifyChecks(context.Background(), "o", "r", sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].AppID != 42 || runs[0].Conclusion != "success" {
		t.Errorf("listed %+v; want App 42's success alone", runs)
	}
}

// PostVerifyCheck posts a completed issuedog/verify run on the SHA, as the
// App, with its conclusion, title, summary and details URL, and returns it
// as GitHub answers. VerifyChecks then lists it.
func TestPostVerifyCheck(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task")
	sha := git("rev-parse", "refs/heads/issuedog/7-task")
	c, ctx := f.Client(t), context.Background()
	run, err := c.PostVerifyCheck(ctx, "o", "r", github.NewCheckRun{HeadSHA: sha, Conclusion: "success", Title: "Verified",
		Summary: "The verifier passed this head.", DetailsURL: "https://github.com/o/r/issues/7#issuecomment-9"})
	if err != nil {
		t.Fatal(err)
	}
	want := github.CheckRun{ID: run.ID, HeadSHA: sha, Name: github.VerifyCheck, Status: "completed", Conclusion: "success", AppID: githubtest.AppID}
	if run != want || run.ID == 0 {
		t.Errorf("posted %+v; want %+v", run, want)
	}
	writes := f.Writes()
	if len(writes) != 1 || writes[0].Method != http.MethodPost || writes[0].Path != "/check-runs" {
		t.Fatalf("writes %+v; want one POST /check-runs", writes)
	}
	var body struct {
		Name       string `json:"name"`
		HeadSHA    string `json:"head_sha"`
		DetailsURL string `json:"details_url"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		Output     struct{ Title, Summary string }
	}
	if err := json.Unmarshal([]byte(writes[0].Body), &body); err != nil {
		t.Fatal(err)
	}
	if body.Name != github.VerifyCheck || body.HeadSHA != sha || body.Status != "completed" || body.Conclusion != "success" || body.Output.Title != "Verified" ||
		body.Output.Summary != "The verifier passed this head." || body.DetailsURL != "https://github.com/o/r/issues/7#issuecomment-9" {
		t.Errorf("posted %s; want the name, completed, the conclusion, the title, the summary and the details URL", writes[0].Body)
	}
	runs, err := c.VerifyChecks(ctx, "o", "r", sha)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(runs, []github.CheckRun{want}) {
		t.Errorf("listed %+v; want the run posted", runs)
	}
	if got, want := f.State().CheckRuns, []githubtest.CheckRunState{{SHA: sha, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID}}; !reflect.DeepEqual(got, want) {
		t.Errorf("State().CheckRuns %+v; want %+v", got, want)
	}
}

// A check run on a commit no branch holds, or one the repo hasn't got, is
// GitHub's 422, an error that names the check and the SHA, and posts
// nothing.
func TestPostVerifyCheckRefused(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t)
	loose := git("commit-tree", git("mktree"), "-m", "on no branch")
	c, ctx := f.Client(t), context.Background()
	for _, sha := range []string{loose, strings.Repeat("0", 40)} {
		_, err := c.PostVerifyCheck(ctx, "o", "r", github.NewCheckRun{HeadSHA: sha, Conclusion: "success", Title: "T", Summary: "S"})
		var er *gogithub.ErrorResponse
		if !errors.As(err, &er) || er.Response.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(err.Error(), github.VerifyCheck+" success on "+sha) {
			t.Errorf("post on %s: %v; want GitHub's 422, naming the check and the SHA", sha, err)
		}
	}
	if got := f.State().CheckRuns; got != nil {
		t.Errorf("State().CheckRuns %+v; want none", got)
	}
}

// verifyCheckPass posts the App's issuedog/verify success on sha unless the
// newest one the App posted there is a success already, as the daemon does
// (SPEC §10.4).
func verifyCheckPass(t *testing.T, sha string) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c, ctx := f.Client(t), context.Background()
		runs, err := c.VerifyChecks(ctx, "o", "r", sha)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		if len(runs) > 0 && runs[0].Conclusion == "success" {
			return nil
		}
		if _, err := c.PostVerifyCheck(ctx, "o", "r", github.NewCheckRun{HeadSHA: sha, Conclusion: "success", Title: "Verified",
			Summary: "The verifier passed this head.", DetailsURL: "https://github.com/o/r/issues/1#issuecomment-1"}); err != nil {
			return fmt.Errorf("post: %w", err)
		}
		return nil
	}
}

// Posting the check is safe to replay after a crash at any write: one more
// success, after the App's older failure, and another App's success on the
// same SHA doesn't stand in for it.
func TestVerifyCheckReplay(t *testing.T) {
	t.Parallel()
	_, git := pullsRemote(t, "issuedog/7-task")
	sha := git("rev-parse", "refs/heads/issuedog/7-task")
	setup := func(tb testing.TB) *githubtest.Fake {
		f, _ := pullsFake(tb, "issuedog/7-task")
		f.SetCheckRun(sha, github.VerifyCheck, "failure", githubtest.AppID)
		f.SetCheckRun(sha, github.VerifyCheck, "success", 2)
		return f
	}
	clean := setup(t)
	if err := verifyCheckPass(t, sha)(clean); err != nil {
		t.Fatal(err)
	}
	want := []githubtest.CheckRunState{
		{SHA: sha, Name: github.VerifyCheck, Conclusion: "failure", AppID: githubtest.AppID},
		{SHA: sha, Name: github.VerifyCheck, Conclusion: "success", AppID: 2},
		{SHA: sha, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID},
	}
	if got := clean.State().CheckRuns; !reflect.DeepEqual(got, want) {
		t.Fatalf("State().CheckRuns %+v; want %+v", got, want)
	}
	githubtest.Replay(t, setup, verifyCheckPass(t, sha))
}

// PostVerifyCheck sends the run's external_id when it's set, and none when
// it isn't, and the check reads back with it, as posted and as
// VerifyChecks lists it. An older check with none, as one SetCheckRun
// adds, reads back with "".
func TestVerifyCheckExternalID(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/7-task")
	sha, main := git("rev-parse", "refs/heads/issuedog/7-task"), git("rev-parse", "refs/heads/main")
	f.SetCheckRun(sha, github.VerifyCheck, "success", githubtest.AppID)
	c, ctx := f.Client(t), context.Background()
	named, err := c.PostVerifyCheck(ctx, "o", "r", github.NewCheckRun{HeadSHA: sha, Conclusion: "failure", Title: "Failed on main",
		Summary: "A test fails on the merge.", ExternalID: main})
	if err != nil {
		t.Fatal(err)
	}
	if named.ExternalID != main {
		t.Errorf("posted %+v; want external_id %s", named, main)
	}
	unnamed, err := c.PostVerifyCheck(ctx, "o", "r", github.NewCheckRun{HeadSHA: sha, Conclusion: "success", Title: "Verified", Summary: "Passed."})
	if err != nil {
		t.Fatal(err)
	}
	if unnamed.ExternalID != "" {
		t.Errorf("posted %+v; want no external_id", unnamed)
	}
	writes := f.Writes()
	if len(writes) != 2 {
		t.Fatalf("writes %+v; want two posts", writes)
	}
	for i, want := range []any{main, nil} {
		var body map[string]any
		if err := json.Unmarshal([]byte(writes[i].Body), &body); err != nil {
			t.Fatal(err)
		}
		if got, ok := body["external_id"]; want == nil && ok || want != nil && got != want {
			t.Errorf("post %d sent %s; want external_id %v", i+1, writes[i].Body, want)
		}
	}
	runs, err := c.VerifyChecks(ctx, "o", "r", sha)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.Conclusion+" "+r.ExternalID)
	}
	if want := []string{"success ", "failure " + main, "success "}; !reflect.DeepEqual(got, want) {
		t.Errorf("listed %q; want %q, newest first", got, want)
	}
}

// verifyCheckMainPass posts the App's issuedog/verify success on sha,
// naming main in its external_id, unless the newest one the App posted
// there is a success naming main already, as a check judged against main
// is posted (SPEC §10.4).
func verifyCheckMainPass(t *testing.T, sha, main string) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c, ctx := f.Client(t), context.Background()
		runs, err := c.VerifyChecks(ctx, "o", "r", sha)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		if len(runs) > 0 && runs[0].Conclusion == "success" && runs[0].ExternalID == main {
			return nil
		}
		if _, err := c.PostVerifyCheck(ctx, "o", "r", github.NewCheckRun{HeadSHA: sha, Conclusion: "success", Title: "Verified",
			Summary: "The checks passed on the merge with main.", ExternalID: main}); err != nil {
			return fmt.Errorf("post: %w", err)
		}
		return nil
	}
}

// Posting a check naming main is safe to replay after a crash at any
// write: the App's older success, which names no main, doesn't stand in
// for it, and the replay posts one success naming main, no more.
func TestVerifyCheckExternalIDReplay(t *testing.T) {
	t.Parallel()
	_, git := pullsRemote(t, "issuedog/7-task")
	sha, main := git("rev-parse", "refs/heads/issuedog/7-task"), git("rev-parse", "refs/heads/main")
	setup := func(tb testing.TB) *githubtest.Fake {
		f, _ := pullsFake(tb, "issuedog/7-task")
		f.SetCheckRun(sha, github.VerifyCheck, "success", githubtest.AppID)
		return f
	}
	clean := setup(t)
	if err := verifyCheckMainPass(t, sha, main)(clean); err != nil {
		t.Fatal(err)
	}
	want := []githubtest.CheckRunState{
		{SHA: sha, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID},
		{SHA: sha, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID, ExternalID: main},
	}
	if got := clean.State().CheckRuns; !reflect.DeepEqual(got, want) {
		t.Fatalf("State().CheckRuns %+v; want %+v", got, want)
	}
	githubtest.Replay(t, setup, verifyCheckMainPass(t, sha, main))
}

// CheckRuns and PostCheck take the check's name. PostCheck posts a
// completed TipCheck, issuedog/tip, with its external_id naming the
// branch, and CheckRuns for TipCheck lists the App's of that name alone,
// newest first, with check_name=issuedog/tip, leaving out the App's
// issuedog/verify and another App's issuedog/tip on the same commit.
// VerifyChecks leaves the TipCheck out.
func TestCheckRunsNamed(t *testing.T) {
	t.Parallel()
	f, git := pullsFake(t, "issuedog/5-story")
	tip := git("rev-parse", "refs/heads/issuedog/5-story")
	f.SetCheckRun(tip, github.TipCheck, "failure", githubtest.AppID)
	f.SetCheckRun(tip, github.TipCheck, "success", 2)
	f.SetCheckRun(tip, github.VerifyCheck, "success", githubtest.AppID)
	c, ctx := f.Client(t), context.Background()
	posted, err := c.PostCheck(ctx, "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: tip, Conclusion: "success", Title: "Checked",
		Summary: "The story branch's tip passed.", ExternalID: "branch=issuedog/5-story"})
	if err != nil {
		t.Fatal(err)
	}
	want := github.CheckRun{ID: posted.ID, HeadSHA: tip, Name: "issuedog/tip", Status: "completed", Conclusion: "success", AppID: githubtest.AppID,
		ExternalID: "branch=issuedog/5-story"}
	if github.TipCheck != "issuedog/tip" || posted != want {
		t.Errorf("posted %+v; want %+v", posted, want)
	}
	var body struct {
		Name       string `json:"name"`
		ExternalID string `json:"external_id"`
	}
	if w := f.Writes(); len(w) != 1 || json.Unmarshal([]byte(w[0].Body), &body) != nil || body.Name != "issuedog/tip" || body.ExternalID != "branch=issuedog/5-story" {
		t.Errorf("writes %+v; want one post named issuedog/tip, with the branch as its external_id", w)
	}
	from := len(f.Requests())
	runs, err := c.CheckRuns(ctx, "o", "r", tip, github.TipCheck)
	if err != nil {
		t.Fatal(err)
	}
	if queries := checkRunsQueries(f, from, tip); len(queries) != 1 || !strings.Contains(queries[0], "check_name=issuedog%2Ftip") || !strings.Contains(queries[0], "app_id=1") ||
		!strings.Contains(queries[0], "filter=all") {
		t.Errorf("queries %q; want one, with check_name=issuedog/tip, app_id=1, filter=all", queries)
	}
	var got []string
	for _, r := range runs {
		got = append(got, r.Name+" "+r.Conclusion+" "+r.ExternalID)
	}
	if want := []string{"issuedog/tip success branch=issuedog/5-story", "issuedog/tip failure "}; !reflect.DeepEqual(got, want) {
		t.Errorf("listed %q; want %q, newest first", got, want)
	}
	verify, err := c.VerifyChecks(ctx, "o", "r", tip)
	if err != nil {
		t.Fatal(err)
	}
	if len(verify) != 1 || verify[0].Name != github.VerifyCheck {
		t.Errorf("VerifyChecks %+v; want the App's issuedog/verify alone", verify)
	}
	if _, err := c.PostCheck(ctx, "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: strings.Repeat("0", 40), Conclusion: "success", Title: "T", Summary: "S"}); err == nil ||
		!strings.Contains(err.Error(), "issuedog/tip success on "+strings.Repeat("0", 40)) {
		t.Errorf("post on a commit the repo hasn't got: %v; want an error naming the check and the SHA", err)
	}
}

// tipCheckPass posts the App's TipCheck success on sha, naming branch,
// unless one of the App's there names it already, as a story branch's tip
// is checked once (SPEC §7.5).
func tipCheckPass(t *testing.T, sha, branch string) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c, ctx := f.Client(t), context.Background()
		runs, err := c.CheckRuns(ctx, "o", "r", sha, github.TipCheck)
		if err != nil {
			return fmt.Errorf("list: %w", err)
		}
		for _, r := range runs {
			if r.ExternalID == "branch="+branch {
				return nil
			}
		}
		if _, err := c.PostCheck(ctx, "o", "r", github.TipCheck, github.NewCheckRun{HeadSHA: sha, Conclusion: "success", Title: "Checked",
			Summary: "The story branch's tip passed.", ExternalID: "branch=" + branch}); err != nil {
			return fmt.Errorf("post: %w", err)
		}
		return nil
	}
}

// Posting a TipCheck is safe to replay after a crash at any write: one
// check naming the branch, and the App's issuedog/verify on the same
// commit doesn't stand in for it.
func TestTipCheckReplay(t *testing.T) {
	t.Parallel()
	_, git := pullsRemote(t, "issuedog/5-story")
	tip := git("rev-parse", "refs/heads/issuedog/5-story")
	setup := func(tb testing.TB) *githubtest.Fake {
		f, _ := pullsFake(tb, "issuedog/5-story")
		f.SetCheckRun(tip, github.VerifyCheck, "success", githubtest.AppID)
		return f
	}
	clean := setup(t)
	if err := tipCheckPass(t, tip, "issuedog/5-story")(clean); err != nil {
		t.Fatal(err)
	}
	want := []githubtest.CheckRunState{
		{SHA: tip, Name: github.VerifyCheck, Conclusion: "success", AppID: githubtest.AppID},
		{SHA: tip, Name: github.TipCheck, Conclusion: "success", AppID: githubtest.AppID, ExternalID: "branch=issuedog/5-story"},
	}
	if got := clean.State().CheckRuns; !reflect.DeepEqual(got, want) {
		t.Fatalf("State().CheckRuns %+v; want %+v", got, want)
	}
	githubtest.Replay(t, setup, tipCheckPass(t, tip, "issuedog/5-story"))
}
