package daemon

import (
	"bytes"
	"cmp"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/mertenvg/issuedog/internal/github"
)

// GitHub's limits a plan is validated against (SPEC §11.3), which its
// validation line names.
const (
	planMaxSubIssues = 100 // sub-issues per parent
	planMaxLinks     = 50  // dependency links of one type per issue
)

// planApproveLine ends a plan comment that waits for review (SPEC §11.4).
const planApproveLine = "**To approve this plan, react 👍 to this comment.** Any other comment sends it back for revision."

// planView is what renderPlan needs to know beyond the plan itself.
type planView struct {
	Issue int // the issue planned
	Rev   int // the plan revision the comment is
	// Priority is the issue's priority, p0 to p3, which an item without
	// one of its own gets, or "" if the issue has none.
	Priority string
	Notify   []string // config's notify, for the cc line
	// NoReview is set when the plan skips review: a story's task plan with
	// plan.review false, not after a discussion hold. The issue moves
	// straight to planned, so nothing waits on the owner.
	NoReview bool
	// The validation facts, for the validation line: the daemon checked
	// the plan against these before it renders it.
	MaxTasks  int // plan.max_tasks
	SubIssues int // the issue's sub-issues before the plan's are created
	// Revised is set for a revision of an issue planned before (SPEC
	// §11.3), whose items have statuses, and Earlier is the approved
	// revision's items, whose dependencies a kept item keeps.
	Revised bool
	Earlier []planItem
}

// renderPlan renders p as plan revision f.Rev's comment on issue f.Issue
// (SPEC §11.4), its text without markers. In order: the heading; why,
// then the summary; what changed since the last revision, from the
// message, if p has one; the assumptions to confirm, if any; each item;
// the order; the validation line, which the daemon writes from its own
// counts; then the cc line and how to approve, or, when the plan skips
// review, a line saying so instead.
//
// It shows no file paths (SPEC §11.4): of a task's body, only a human
// task's "Done when" text, and its check by kind, never its path, label,
// owner or other value. Agent text is escaped as a spec is (SPEC §11.1):
// headings demoted, task list boxes stripped, mentions defused, and HTML
// tags and markers escaped.
func renderPlan(p planBody, f planView) string {
	parts := []string{fmt.Sprintf("### Plan for #%d (rev %d)", f.Issue, f.Rev), inline(p.Why), block(p.Summary)}
	if strings.TrimSpace(p.Message) != "" {
		if f.Rev > 1 {
			parts = append(parts, fmt.Sprintf("**Changes since rev %d.**", f.Rev-1))
		}
		parts = append(parts, block(p.Message))
	}
	if len(p.Assumptions) > 0 {
		lead := "**Assumptions to confirm with your 👍:**"
		if f.NoReview {
			lead = "**Assumptions:**"
		}
		parts = append(parts, lead+"\n"+bullets(p.Assumptions))
	}
	for _, it := range p.Items {
		parts = append(parts, renderPlanItem(it, p.Kind == planTasks, f.Priority, f.Revised))
	}
	parts = append(parts, "**Order:** "+inline(p.Order), planValidation(p, f))
	if f.NoReview {
		parts = append(parts, fmt.Sprintf("This plan skips review, as `plan.review` is false: #%d moves straight to `planned`.", f.Issue))
	} else {
		parts = append(append(parts, ccLines(f.Notify)...), planApproveLine)
	}
	return github.Escape(strings.Join(parts, "\n\n"))
}

// renderPlanItem renders one item: its ID and title, then priority, which
// is the issue's priority unless the item has its own, and model and effort
// when set. A task, when task is set, also shows its scope first, and
// "human" after the priority for a human task. Then the outcome, the
// criteria as plain bullets, a human task's "Done when", and what it
// depends on. In a revision, when revised is set, the item's status comes
// first after the title (SPEC §11.4), and a kept or dropped item, which
// has its ID and title only, is that one line.
func renderPlanItem(it planItem, task bool, priority string, revised bool) string {
	head := "**" + inline(it.ID) + ". " + inline(it.Title) + "**"
	if revised {
		head += " · " + inline(it.Status)
		if planBare(it.Status) {
			return head
		}
	}
	var attrs []string
	if task && it.Scope != "" {
		attrs = append(attrs, "scope:"+inline(it.Scope))
	}
	if p := cmp.Or(it.Priority, priority); p != "" {
		attrs = append(attrs, inline(p))
	}
	if task && it.Human {
		attrs = append(attrs, "human")
	}
	if it.Model != "" {
		attrs = append(attrs, "model:"+inline(it.Model))
	}
	if it.Effort != "" {
		attrs = append(attrs, "effort:"+inline(it.Effort))
	}
	for _, a := range attrs {
		head += " · " + a
	}
	// The blank line ends the criteria's list, so what follows isn't read
	// as part of its last item.
	var tail []string
	if h := it.HumanTask; task && it.Human && h != nil {
		done := "Done when: " + inline(h.DoneWhen.Text)
		if c := h.DoneWhen.Check; c != nil {
			done += " (checked automatically: " + inline(c.Kind) + ")"
		}
		tail = append(tail, done)
	}
	deps := "nothing"
	if len(it.DependsOn) > 0 {
		ids := make([]string, len(it.DependsOn))
		for i, d := range it.DependsOn {
			ids[i] = inline(d)
		}
		deps = strings.Join(ids, ", ")
	}
	tail = append(tail, "Depends on: "+deps)
	return head + "\nOutcome: " + inline(it.Outcome) + "\n" + bullets(it.Criteria) + "\n\n" + strings.Join(tail, "\n")
}

// planValidation is the plan comment's validation line (SPEC §11.4),
// written from the daemon's own counts and f's facts, not the planner's
// words: the items and what they are, the dependency graph, the
// sub-issues the issue will have, and the most dependency links of one
// type any item will have, blocked by or blocking. In a revision, the
// items counted are those that aren't dropped, the sub-issues added are
// the new items, a kept item's links are its approved item's
// (planPrior.keptDeps), and the line says every child is listed once,
// with the closed ones kept (checkStatuses).
func planValidation(p planBody, f planView) string {
	n, added := 0, 0
	for _, it := range p.Items {
		if !f.Revised || it.Status != planDropped {
			n++
		}
		if !f.Revised || it.Status == planNew {
			added++
		}
	}
	what := fmt.Sprintf("%d %s, with no tasks", n, plural(n, "story", "stories"))
	if p.Kind == planTasks {
		what = fmt.Sprintf("%d %s, within `plan.max_tasks` of %d, each with one scope from the list", n, plural(n, "task", "tasks"), f.MaxTasks)
	}
	if f.Revised {
		what += "; every child listed once, kept, changed or dropped, and each closed one kept"
	}
	var prior *planPrior
	if f.Revised {
		prior = &planPrior{Items: f.Earlier}
	}
	depsOf := planDepsOf(p, prior.keptDeps(p))
	links := 0
	blocking := map[string]int{}
	for _, it := range p.Items {
		deps := depsOf(it)
		links = max(links, len(deps))
		for _, d := range deps {
			blocking[d]++
		}
	}
	for _, b := range blocking {
		links = max(links, b)
	}
	subs := f.SubIssues + added
	return fmt.Sprintf("Validation (11.3): %s; no dependency cycles; %d %s in all, within %d; at most %d dependency %s of one type per issue, within %d.",
		what, subs, plural(subs, "sub-issue", "sub-issues"), planMaxSubIssues, links, plural(links, "link", "links"), planMaxLinks)
}

// plural is one if n is 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// The plan-data marker keeps a plan's data in its plan comment (SPEC §5,
// §11.4), for what is done with the plan once it's approved: the
// children are created from the approved revision's data (SPEC §5.2),
// task bodies and checks included, which the comment's text never shows,
// and a revision reads an earlier revision's items from it to revise a
// plan (SPEC §11.3). Run results are
// scratch, so this is the only copy:
//
//	<!-- issuedog:plan-data v=1 data=<base64url(deflate(JSON))> -->
//
// v is the format's version, planDataVersion, and a reader refuses any
// other. data is the planBody as JSON (encoding/json, with the planner
// schema's field names), compressed with raw DEFLATE (RFC 1951,
// compress/flate, with no zlib or gzip header), then encoded as unpadded
// base64url (RFC 4648 §5), which fits a marker field's value, [^\s<>]+.
//
// The marker goes in the plan comment between the move's transition
// marker and the plan revision marker, which is always the last line
// (Move.Markers, then Move.PlanRev). So the revision's hash, state.PlanHash,
// covers it: an approval of that revision approves exactly this data, and
// any edit to it is a dirty comment or a hash that no longer matches
// (state.Revision.Matches). Only the App's markers are read
// (readPlanData).
const (
	planDataKind    = "plan-data"
	planDataVersion = "1"
	// maxPlanData bounds a plan's JSON, so readPlanData never inflates
	// more than this, and planDataMarker never writes what it couldn't
	// read back. A plan comment is at most 65536 characters, so its data
	// is far smaller.
	maxPlanData = 1 << 20
)

// planDataMarker is the plan-data marker holding p, for Move.Markers.
func planDataMarker(p planBody) (github.Marker, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return github.Marker{}, fmt.Errorf("plan data: %w", err)
	}
	if len(raw) > maxPlanData {
		return github.Marker{}, fmt.Errorf("plan data: %d bytes of JSON, over %d", len(raw), maxPlanData)
	}
	var z bytes.Buffer
	w, err := flate.NewWriter(&z, flate.BestCompression)
	if err != nil {
		return github.Marker{}, fmt.Errorf("plan data: compress: %w", err)
	}
	if _, err := w.Write(raw); err != nil {
		return github.Marker{}, fmt.Errorf("plan data: compress: %w", err)
	}
	if err := w.Close(); err != nil {
		return github.Marker{}, fmt.Errorf("plan data: compress: %w", err)
	}
	return github.Marker{Kind: planDataKind, Fields: map[string]string{
		"v": planDataVersion, "data": base64.RawURLEncoding.EncodeToString(z.Bytes())}}, nil
}

// readPlanData reads the plan back from c's plan-data marker, as
// planDataMarker wrote it. It reports false, with no error, when c has no
// plan-data marker the App wrote: one in a comment by anyone else, a
// malformed one ParseMarkers skips, or an escaped one in text the App
// quoted are no marker. One that doesn't read back is an error: a second
// one, another version, or data that isn't base64url, raw DEFLATE, or JSON
// of exactly planBody's fields. It doesn't check whether c is dirty, or
// still matches its revision's hash: the caller does, as for any revision
// (state.LatestRevision, state.Revision.Matches).
func readPlanData(c github.Comment, appSlug string) (planBody, bool, error) {
	var found []github.Marker
	for _, m := range github.TrustedMarkers(c.Author, c.Body, appSlug) {
		if m.Kind == planDataKind {
			found = append(found, m)
		}
	}
	if len(found) == 0 {
		return planBody{}, false, nil
	}
	fail := func(format string, args ...any) (planBody, bool, error) {
		return planBody{}, false, fmt.Errorf("plan data in comment %d: %s", c.ID, fmt.Sprintf(format, args...))
	}
	if len(found) > 1 {
		return fail("%d plan-data markers, not one", len(found))
	}
	m := found[0]
	if v := m.Fields["v"]; v != planDataVersion {
		return fail("version %q, not %s", v, planDataVersion)
	}
	p, err := decodePlanData(m.Fields["data"])
	if err != nil {
		return planBody{}, false, fmt.Errorf("plan data in comment %d: %w", c.ID, err)
	}
	return p, true, nil
}

// decodePlanData undoes planDataMarker's data field.
func decodePlanData(data string) (planBody, error) {
	z, err := base64.RawURLEncoding.DecodeString(data)
	if err != nil {
		return planBody{}, fmt.Errorf("decode base64url: %w", err)
	}
	r := flate.NewReader(bytes.NewReader(z))
	raw, err := io.ReadAll(io.LimitReader(r, maxPlanData+1))
	if err != nil {
		return planBody{}, fmt.Errorf("inflate: %w", err)
	}
	if err := r.Close(); err != nil {
		return planBody{}, fmt.Errorf("inflate: %w", err)
	}
	if len(raw) > maxPlanData {
		return planBody{}, fmt.Errorf("inflate: over %d bytes", maxPlanData)
	}
	var p planBody
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return planBody{}, fmt.Errorf("decode JSON: %w", err)
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return planBody{}, errors.New("decode JSON: more after the plan")
	}
	return p, nil
}
