package github

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	gogithub "github.com/google/go-github/v92/github"
)

// VerifyCheck is the name of the check run the App posts on a verified head,
// which the main ruleset requires, with the App as its pinned source (SPEC
// §10.4, §15).
const VerifyCheck = "issuedog/verify"

// TipCheck is the name of the check run the App posts on a story branch's
// tip once it checked it, naming the branch in its external_id, which marks
// the tips it checked and their results (SPEC §7.5, §10.4).
const TipCheck = "issuedog/tip"

// CheckRun is one check run as the daemon reads it: its ID, the commit it's
// on, its name, its status ("queued", "in_progress" or "completed") and
// conclusion ("" until it's completed), the ID of the App that posted it,
// and its external_id, the reference that App gave it, or "" for none.
type CheckRun struct {
	ID                                int64
	HeadSHA, Name, Status, Conclusion string
	AppID                             int64
	ExternalID                        string
}

// NewCheckRun is a completed check run to post: the commit it goes on,
// its conclusion, such as "success", its output's title and summary, the
// page its details link to, such as the verifier comment (SPEC §10.4), and
// its external_id, sent only when it's set.
type NewCheckRun struct {
	HeadSHA, Conclusion, Title, Summary, DetailsURL string
	ExternalID                                      string
}

// checkRunOf is the CheckRun GitHub's r describes.
func checkRunOf(r *gogithub.CheckRun) CheckRun {
	return CheckRun{ID: r.GetID(), HeadSHA: r.GetHeadSHA(), Name: r.GetName(), Status: r.GetStatus(),
		Conclusion: r.GetConclusion(), AppID: r.GetApp().GetID(), ExternalID: r.GetExternalID()}
}

// AppID is the ID of the App the client acts as, ISSUEDOG_APP_ID's
// (Config.AppID). Its check runs carry it, and only they count (SPEC §10.4).
func (c *Client) AppID() int64 { return c.appID }

// CheckRuns lists the check runs named name that the App posted on commit
// sha in owner/repo, newest first, reading every page. It reads GET
// /repos/{owner}/{repo}/commits/{sha}/check-runs with check_name, the
// App's app_id, so another App's run of that name is left out, and
// filter=all, so an older run is listed too, not only the newest. GitHub's
// docs give the list no order, so it's ordered here, by ID, which GitHub
// gives in the order runs are made. A repeat of an unchanged listing is a
// 304, which costs no rate limit.
func (c *Client) CheckRuns(ctx context.Context, owner, repo, sha, name string) ([]CheckRun, error) {
	opts := &gogithub.ListCheckRunsOptions{CheckName: new(name), AppID: new(c.appID), Filter: new("all"),
		ListOptions: gogithub.ListOptions{PerPage: 100}}
	all, err := collect(c.Checks.ListCheckRunsForRefIter(ctx, owner, repo, sha, opts))
	if err != nil {
		return nil, fmt.Errorf("list %s check runs on %s in %s/%s: %w", name, sha, owner, repo, err)
	}
	runs := make([]CheckRun, 0, len(all))
	for _, r := range all {
		runs = append(runs, checkRunOf(r))
	}
	slices.SortStableFunc(runs, func(a, b CheckRun) int { return cmp.Compare(b.ID, a.ID) })
	return runs, nil
}

// VerifyChecks lists the VerifyCheck runs the App posted on commit sha in
// owner/repo, newest first, every page, as CheckRuns lists them.
func (c *Client) VerifyChecks(ctx context.Context, owner, repo, sha string) ([]CheckRun, error) {
	runs, err := c.CheckRuns(ctx, owner, repo, sha, VerifyCheck)
	if err != nil {
		return nil, fmt.Errorf("read the App's verify checks: %w", err)
	}
	return runs, nil
}

// PostCheck posts run as a completed check run named name, as the App,
// with its conclusion, its output's title and summary, its details URL and
// its external_id, if set, and returns it as GitHub answers. It isn't
// idempotent: each call posts one more, so a caller that may replay reads
// CheckRuns first.
func (c *Client) PostCheck(ctx context.Context, owner, repo, name string, run NewCheckRun) (CheckRun, error) {
	opts := gogithub.CreateCheckRunOptions{
		Name: name, HeadSHA: run.HeadSHA, DetailsURL: &run.DetailsURL, Status: new("completed"), Conclusion: &run.Conclusion,
		Output: &gogithub.CheckRunOutput{Title: &run.Title, Summary: &run.Summary},
	}
	if run.ExternalID != "" {
		opts.ExternalID = &run.ExternalID
	}
	r, _, err := c.Checks.CreateCheckRun(ctx, owner, repo, opts)
	if err != nil {
		return CheckRun{}, fmt.Errorf("post %s %s on %s in %s/%s: %w", name, run.Conclusion, run.HeadSHA, owner, repo, err)
	}
	return checkRunOf(r), nil
}

// PostVerifyCheck posts run as a completed VerifyCheck, as PostCheck posts
// it. It isn't idempotent, so a caller that may replay reads VerifyChecks
// first.
func (c *Client) PostVerifyCheck(ctx context.Context, owner, repo string, run NewCheckRun) (CheckRun, error) {
	r, err := c.PostCheck(ctx, owner, repo, VerifyCheck, run)
	if err != nil {
		return CheckRun{}, fmt.Errorf("post the App's verify check: %w", err)
	}
	return r, nil
}
