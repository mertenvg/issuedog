package github

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/mertenvg/logr/v2"
)

// This file is #245: a revert of the issue type sets the type that stands by
// the name the org gives it now, not the one on the timeline. An org can
// rename a type, which keeps its ID, while the timeline keeps the name its
// events were written with, and GitHub refuses a name the org renamed away
// with a 422.

// ErrIssueTypeGone is wrapped by typeRenameCurrent's error when the type
// that stands, which a revert would set back, is no type the org lists any
// more, as once the org deleted it. Nothing is written: the App can't set
// the type back, and neither keeping the untrusted type nor removing it is
// a revert it can explain. So CheckLabels leaves the type out for the type
// that stands, with the revert it blocks (IssueTypeLeftOut.Gone), which
// the daemon reports once (#409). A trusted user's own change of the type
// ends it.
var ErrIssueTypeGone = errors.New("the org has no such issue type any more")

// IssueTypeNames is what one org's issue types are named now, by ID, for the
// reverts of the issue type in one pass on its repo (SPEC §5.1). It keeps
// the list of the org the first revert asks for, and doesn't check the org
// a later one asks for, so a caller keeps one for one org only, as each
// Applier serves one repo. The org's list, GET /orgs/{org}/issue-types, is
// read only when a revert needs a type's name, and then once: so a pass
// with no revert reads nothing, and the next pass, with a new
// IssueTypeNames, sees a later rename. A failed read is kept too, so a pass
// makes at most one request for the list, whatever it returns. A refused
// one, a 403 or a 404, names no type, and makes none gone (#409). The zero
// value is ready to use, and safe for concurrent use.
type IssueTypeNames struct {
	mu    sync.Mutex
	read  bool
	names map[int64]string
	err   error
}

// typeRenameName returns the name of org's issue type id, as the org lists
// it now, reading the list the first time it's asked. false is a type the
// org doesn't list. A failed read is the error, again on each later ask,
// with no new request. So is a refused one, wrapping errTypeGoneRefused: a
// 403, or a 404, which OrgIssueTypes gives as an org that isn't one, as an
// org the App can't see, and which is no empty list (#409). That's logged
// once, when read.
func (n *IssueTypeNames) typeRenameName(ctx context.Context, c *Client, org string, id int64) (string, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.read {
		n.read, n.names = true, map[int64]string{}
		types, isOrg, err := c.OrgIssueTypes(ctx, org)
		switch {
		case typeGoneRefusal(err), err == nil && !isOrg:
			why := "HTTP 404"
			if err != nil {
				why = err.Error()
			}
			n.err = fmt.Errorf("read the org's issue types: %s: %w", why, errTypeGoneRefused)
			logr.Warnf("GitHub refused the read of %s's issue types (%s), so a revert of the issue type restores the type by the timeline's name, "+
				"which GitHub refuses if the org renamed it: the App needs the organization permission Issue Types read (SPEC §15)", org, why)
		case err != nil:
			n.err = fmt.Errorf("read the org's issue types: %w", err)
		}
		for _, t := range types {
			n.names[t.ID] = t.Name
		}
	}
	if n.err != nil {
		return "", false, n.err
	}
	name, ok := n.names[id]
	return name, ok, nil
}

// typeRenameCurrent sets rv.To, the type that stands, which rv sets back, to
// the name the org gives that type now, by rv.ToID, from names, or a fresh
// IssueTypeNames if nil. A removal, which has no ID, needs no name, and a
// type with no ID keeps the timeline's name, as nothing else names it:
// neither reads the org's list. Nor does a refused read name it, so the
// type keeps the timeline's name then too, as before #245, and a revert
// with no rename still goes through (#409). A type the org no longer lists
// is an error wrapping ErrIssueTypeGone, and a failed read is an error, so
// in either case nothing is written, and the next pass tries again.
func (r *labelRules) typeRenameCurrent(ctx context.Context, rv *IssueTypeRevert, names *IssueTypeNames) error {
	if rv.ToID == 0 {
		return nil
	}
	if names == nil {
		names = &IssueTypeNames{}
	}
	name, ok, err := names.typeRenameName(ctx, r.c, r.owner, rv.ToID)
	switch {
	case errors.Is(err, errTypeGoneRefused):
		logr.Infof("the issue type %q to restore on %s/%s#%d keeps the timeline's name: %v", rv.To, r.owner, r.repo, r.number, err)
		return nil
	case err != nil:
		return fmt.Errorf("the issue type %q to restore, by its ID %d: %w", rv.To, rv.ToID, err)
	case !ok:
		return fmt.Errorf("the issue type %q to restore, by its ID %d: %w", rv.To, rv.ToID, ErrIssueTypeGone)
	}
	if name != rv.To {
		logr.Infof("the issue type %q to restore on %s/%s#%d is named %q in the org now, so that name is restored", rv.To, r.owner, r.repo, r.number, name)
		rv.To = name
	}
	return nil
}
