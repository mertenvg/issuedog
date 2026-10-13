package daemon

import (
	"context"
	"strings"

	"github.com/mertenvg/logr/v2"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/github"
)

// LabelOptions returns the labels cfg asks for beyond the fixed set: its
// scopes, its type labels, and a model and an effort label of each kind for
// each allowed model and each effort level. A model whose labels fall back
// to the last 35 characters of its name is logged.
func LabelOptions(cfg *config.Config) github.LabelOptions {
	opts := github.LabelOptions{Scopes: cfg.Scopes, TypeLabels: cfg.Types == "labels", Efforts: config.EffortLevels()}
	for _, m := range cfg.Models.Allowed {
		suffix := m.Suffix()
		if m.Label == "" && suffix != m.Name {
			logr.Infof("models.allowed: %q has no label and is longer than 35 characters, so its labels use its last 35 characters, %q", m.Name, suffix)
		}
		opts.Models = append(opts.Models, github.ModelLabel{Suffix: suffix, Name: m.Name})
	}
	return opts
}

// newModels reports whether cfg allows a model whose label suffix old, which
// may be nil, has no label for. Labels match ignoring case.
func newModels(old, cfg *config.Config) bool {
	var have []config.Model
	if old != nil {
		have = old.Models.Allowed
	}
	for _, m := range cfg.Models.Allowed {
		known := false
		for _, h := range have {
			known = known || strings.EqualFold(h.Suffix(), m.Suffix())
		}
		if !known {
			return true
		}
	}
	return false
}

// ensureLabels creates the labels d.Config asks for that the repo lacks, and
// reports whether it succeeded. A failure is logged.
func (d *Daemon) ensureLabels(ctx context.Context) bool {
	a := d.Step.Applier
	created, err := github.EnsureLabels(ctx, a.Client, a.Owner, a.Repo, LabelOptions(d.Config))
	if len(created) > 0 {
		logr.Infof("created the labels %s on %s/%s", strings.Join(created, ", "), a.Owner, a.Repo)
	}
	if err != nil {
		logr.Errorf("creating the labels of the new models on %s/%s failed, so it's tried again next iteration: %v", a.Owner, a.Repo, err)
		return false
	}
	return true
}
