package githubtest

import (
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	gogithub "github.com/google/go-github/v92/github"
)

// Check runs. The fake keeps each one by the commit it's on, with the App
// that posted it: POST /check-runs posts one as the App, whose ID is AppID,
// as the installation token names it, and SetCheckRun adds one as if
// another App, or the App by hand, had posted it. GET
// /commits/{sha}/check-runs lists a commit's, in GitHub's shape
// (total_count and check_runs), with the check_name, app_id, status and
// filter query parameters, paged. GitHub's docs give the list no order, so
// the fake serves them oldest first, and a caller that wants the newest
// orders them itself. As on GitHub, a check run on a SHA the repo hasn't
// got is refused with a 422, and so, here, is one on a commit no branch of
// the git remote (SetGitRemote) holds, so a test can't check a commit that
// was never pushed.

// checkRun is one check run the fake has: its ID, the commit it's on, its
// name, status, conclusion, output and details URL, and the App that posted
// it, by ID and slug.
type checkRun struct {
	id                                  int64
	headSHA, name, status, conclusion   string
	title, summary, detailsURL, appSlug string
	appID                               int64
	startedAt, completedAt              *gogithub.Timestamp
}

// checkRunSHA matches a full commit SHA, the only ref the fake lists check
// runs for and the only head_sha it takes.
var checkRunSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// checkRunConclusions are the conclusions an App may set, as GitHub lists
// them, without stale, which only GitHub sets.
var checkRunConclusions = []string{"action_required", "cancelled", "failure", "neutral", "success", "skipped", "timed_out"}

// handleCheckRuns registers the check run routes.
func (f *Fake) handleCheckRuns() {
	f.handle("GET /commits/{ref}/check-runs", f.listCheckRuns)
	f.handle("POST /check-runs", f.createCheckRun)
}

// SetCheckRun adds a completed check run named name, with conclusion, on
// commit sha, as the App with ID appID posted it: another App's with an ID
// other than AppID, or the App's own. It's no write, and sha isn't checked
// against the git remote.
func (f *Fake) SetCheckRun(sha, name, conclusion string, appID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	slug := fmt.Sprintf("app-%d", appID)
	if appID == AppID {
		slug = f.slug
	}
	now := f.tick()
	f.addCheckRun(&checkRun{headSHA: sha, name: name, status: "completed", conclusion: conclusion, appID: appID, appSlug: slug,
		startedAt: now, completedAt: now})
}

// addCheckRun gives run the next ID and keeps it on its commit, newest
// last. f is locked.
func (f *Fake) addCheckRun(run *checkRun) {
	run.id = f.nextID()
	if f.checkRuns == nil {
		f.checkRuns = map[string][]*checkRun{}
	}
	f.checkRuns[run.headSHA] = append(f.checkRuns[run.headSHA], run)
}

// checkRunServe is run as GitHub serves it.
func (f *Fake) checkRunServe(run *checkRun) *gogithub.CheckRun {
	out := &gogithub.CheckRun{ID: &run.id, HeadSHA: &run.headSHA, Name: &run.name, Status: &run.status,
		StartedAt: run.startedAt, CompletedAt: run.completedAt,
		HTMLURL: new(fmt.Sprintf("%s/%s/runs/%d", f.srv.URL, f.repo, run.id)),
		App:     &gogithub.App{ID: &run.appID, Slug: &run.appSlug}}
	if run.conclusion != "" {
		out.Conclusion = &run.conclusion
	}
	if run.detailsURL != "" {
		out.DetailsURL = &run.detailsURL
	}
	if run.title != "" || run.summary != "" {
		out.Output = &gogithub.CheckRunOutput{Title: &run.title, Summary: &run.summary}
	}
	return out
}

// listCheckRuns serves commit ref's check runs, oldest first, paged, with
// total_count the number that match: only those named check_name=, posted
// by the App app_id= names, and with status=, if given. filter=latest, the
// default, serves only the newest of each App's runs of each name, as
// GitHub serves the most recent, and filter=all serves every one. A ref
// that isn't a full SHA, as a branch name, isn't modelled and is refused,
// as are a filter, a status and an app_id GitHub wouldn't take, and a SHA
// the git remote hasn't got, with GitHub's 422.
func (f *Fake) listCheckRuns(c *call) (int, any) {
	q, ref := c.r.URL.Query(), c.r.PathValue("ref")
	if !checkRunSHA.MatchString(ref) {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("githubtest: ref %q isn't a full commit SHA; branches and tags aren't modelled", ref))
	}
	filter := q.Get("filter")
	switch filter {
	case "":
		filter = "latest"
	case "latest", "all":
	default:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: filter %q is not latest or all", filter))
	}
	if s := q.Get("status"); s != "" && !slices.Contains([]string{"queued", "in_progress", "completed"}, s) {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: status %q is not queued, in_progress or completed", s))
	}
	var app int64
	if s := q.Get("app_id"); s != "" {
		var err error
		if app, err = strconv.ParseInt(s, 10, 64); err != nil {
			return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: app_id %q is not an integer", s))
		}
	}
	if status, msg := f.checkRunCommit(c, ref); msg != nil {
		return status, msg
	}
	var match []*checkRun
	for _, run := range f.checkRuns[ref] {
		if q.Has("check_name") && run.name != q.Get("check_name") || app != 0 && run.appID != app ||
			q.Has("status") && run.status != q.Get("status") {
			continue
		}
		match = append(match, run)
	}
	if filter == "latest" {
		match = slices.DeleteFunc(match, func(run *checkRun) bool {
			return slices.ContainsFunc(match, func(o *checkRun) bool { return o.appID == run.appID && o.name == run.name && o.id > run.id })
		})
	}
	out := make([]*gogithub.CheckRun, 0, len(match))
	for _, run := range match {
		out = append(out, f.checkRunServe(run))
	}
	return http.StatusOK, gogithub.ListCheckRunsResults{Total: new(len(out)), CheckRuns: pageOf(c, out)}
}

// createCheckRun posts a check run as the App, whose ID is AppID, and
// answers 201 with it. As on GitHub, a name and a head_sha are needed, a
// conclusion sets the status to completed, and a completed one needs a
// conclusion, an output a title and a summary, and the status and the
// conclusion are GitHub's values. Its head_sha must be a full SHA that a
// branch of the git remote holds, or it's refused with a 422 (the head of
// this file).
func (f *Fake) createCheckRun(c *call) (int, any) {
	var req struct {
		Name       string                   `json:"name"`
		HeadSHA    string                   `json:"head_sha"`
		DetailsURL string                   `json:"details_url"`
		Status     string                   `json:"status"`
		Conclusion string                   `json:"conclusion"`
		Output     *gogithub.CheckRunOutput `json:"output"`
	}
	if status, msg := c.decode(&req); msg != nil {
		return status, msg
	}
	if req.Name == "" || req.HeadSHA == "" {
		return http.StatusUnprocessableEntity, message("Validation Failed: a check run needs a name and a head_sha")
	}
	if !checkRunSHA.MatchString(req.HeadSHA) {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: head_sha %q is not a full commit SHA", req.HeadSHA))
	}
	if req.Conclusion != "" {
		if !slices.Contains(checkRunConclusions, req.Conclusion) {
			return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: conclusion %q is not one an App may set", req.Conclusion))
		}
		req.Status = "completed"
	}
	switch req.Status {
	case "":
		req.Status = "queued"
	case "queued", "in_progress":
	case "completed":
		if req.Conclusion == "" {
			return http.StatusUnprocessableEntity, message("Validation Failed: a completed check run needs a conclusion")
		}
	default:
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("Validation Failed: status %q is not queued, in_progress or completed", req.Status))
	}
	if req.Output != nil && (req.Output.GetTitle() == "" || req.Output.GetSummary() == "") {
		return http.StatusUnprocessableEntity, message("Validation Failed: a check run's output needs a title and a summary")
	}
	if status, msg := f.checkRunCommit(c, req.HeadSHA); msg != nil {
		return status, msg
	}
	if status, msg := f.checkRunOnBranch(c, req.HeadSHA); msg != nil {
		return status, msg
	}
	now := f.tick()
	run := &checkRun{headSHA: req.HeadSHA, name: req.Name, status: req.Status, conclusion: req.Conclusion,
		title: req.Output.GetTitle(), summary: req.Output.GetSummary(), detailsURL: req.DetailsURL,
		appID: AppID, appSlug: f.slug, startedAt: now}
	if run.status == "completed" {
		run.completedAt = now
	}
	f.addCheckRun(run)
	return http.StatusCreated, f.checkRunServe(run)
}

// checkRunCommit returns 0 and nil if the git remote has commit sha, or
// GitHub's 422 if it hasn't, or a 500 when it can't be read. f is locked.
func (f *Fake) checkRunCommit(c *call, sha string) (int, any) {
	switch _, err := f.remoteGit(c.r.Context(), "rev-parse", "--verify", "--quiet", sha+"^{commit}"); {
	case remoteNo(err):
		return http.StatusUnprocessableEntity, message("No commit found for SHA: " + sha)
	case err != nil:
		return http.StatusInternalServerError, message(err.Error())
	}
	return 0, nil
}

// checkRunOnBranch returns 0 and nil if a branch of the git remote holds
// commit sha, which the remote has, or a 422 if none does, or a 500 when
// the branches can't be read. f is locked.
func (f *Fake) checkRunOnBranch(c *call, sha string) (int, any) {
	branches, err := f.remoteGit(c.r.Context(), "for-each-ref", "--contains", sha, "--format=%(refname)", "refs/heads/")
	if err != nil {
		return http.StatusInternalServerError, message(err.Error())
	}
	if branches == "" {
		return http.StatusUnprocessableEntity, message(fmt.Sprintf("githubtest: no branch holds %s: push it before checking it", sha))
	}
	return 0, nil
}

// checkRunState is the fake's check runs for a State: by SHA, sorted, and
// each SHA's in the order they were posted, so a replay may not post one
// more, miss one or post them in another order.
func (f *Fake) checkRunState() []CheckRunState {
	var out []CheckRunState
	for _, sha := range slices.Sorted(maps.Keys(f.checkRuns)) {
		for _, run := range f.checkRuns[sha] {
			out = append(out, CheckRunState{SHA: sha, Name: run.name, Conclusion: run.conclusion, AppID: run.appID})
		}
	}
	return out
}
