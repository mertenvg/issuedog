package daemon_test

import (
	"context"
	"maps"
	"reflect"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github/githubtest"
)

// models parses a models.allowed list, as YAML, through config.Parse, so
// it's one a config could have.
func models(t testing.TB, allowed string) config.Models {
	t.Helper()
	cfg, err := config.Parse([]byte("models:\n  allowed: " + allowed + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Models
}

// threeModels allows opus, claude-sonnet-5-5 with the label sonnet, and
// long, a name too long for a label, so its labels take its last 35
// characters, longSuffix.
const (
	threeModels = "[opus, {name: claude-sonnet-5-5, label: sonnet}, " + long + "]"
	long        = "us.anthropic.claude-haiku-5-5-20261001-v1:0"
	longSuffix  = "opic.claude-haiku-5-5-20261001-v1:0"
)

// settingPass runs step 1 on issue 1 of o/r with daemon.Commands and
// daemon.SettingCommands of allowed, "owner" trusted.
func settingPass(t testing.TB, allowed config.Models) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		tr := trusted{"owner": true}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: tr, Commands: daemon.Commands(tr)}
		maps.Copy(st.Commands, daemon.SettingCommands(func() config.Models { return allowed }))
		_, err = st.Run(context.Background(), s)
		return err
	}
}

// taskWith files issue 1 at ready with labels, each "<login> <label>",
// then posts body as login, comment 1002.
func taskWith(login, body string, labels ...string) func(testing.TB) *githubtest.Fake {
	return func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		file(f, 0, ready)
		for _, l := range labels {
			by, name, _ := strings.Cut(l, " ")
			f.AddLabel(1, by, name)
		}
		if id := f.AddComment(1, login, body); id != 1002 {
			t.Fatalf("command ID = %d, want 1002", id)
		}
		return f
	}
}

func TestModelEffortCommands(t *testing.T) {
	const (
		react  = "POST /issues/comments/1002/reactions"
		label  = "POST /issues/1/labels"
		reply  = "POST /issues/1/comments"
		notGPT = "`gpt` isn't on `models.allowed`, which has `opus`, `sonnet` and `" + longSuffix + "`"
	)
	tests := []struct {
		name    string
		allowed string // models.allowed; threeModels if empty
		setup   func(testing.TB) *githubtest.Fake
		labels  []string // issue 1's, after
		comment string   // the App's new comment, if any
		reacts  []string // to the command, after
		writes  []string
	}{
		{name: "/model opus replaces model:sonnet, and leaves verifier-model:* and effort:* alone",
			setup:  taskWith("owner", "/model opus", app+" model:sonnet", "owner verifier-model:sonnet", "owner effort:high"),
			labels: []string{"effort:high", "model:opus", "stage:ready", "verifier-model:sonnet"},
			reacts: []string{app + " +1"}, writes: []string{label, "DELETE /issues/1/labels/model:sonnet", react}},
		{name: "/effort verifier max sets verifier-effort:max, and leaves effort:* alone",
			setup:  taskWith("owner", "/effort verifier max", app+" effort:low", "owner verifier-effort:high"),
			labels: []string{"effort:low", "stage:ready", "verifier-effort:max"},
			reacts: []string{app + " +1"}, writes: []string{label, "DELETE /issues/1/labels/verifier-effort:high", react}},
		{name: "/model verifier opus leaves model:* alone",
			setup:  taskWith("owner", "/model verifier opus", app+" model:sonnet", app+" verifier-model:sonnet"),
			labels: []string{"model:sonnet", "stage:ready", "verifier-model:opus"},
			reacts: []string{app + " +1"}, writes: []string{label, "DELETE /issues/1/labels/verifier-model:sonnet", react}},
		{name: "/effort high sets effort:high on an issue with none",
			setup:  taskWith("owner", "/effort high"),
			labels: []string{"effort:high", "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{label, react}},
		{name: "/model with a long name sets the label of its last 35 characters",
			setup:  taskWith("owner", "/model "+long, app+" model:opus"),
			labels: []string{"model:" + longSuffix, "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{label, "DELETE /issues/1/labels/model:opus", react}},
		{name: "/model takes a label suffix in any case, and sets it as configured",
			setup:  taskWith("owner", "/model "+strings.ToUpper(longSuffix)),
			labels: []string{"model:" + longSuffix, "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{label, react}},
		{name: "/model verifier takes a full name in any case, and sets its label suffix",
			setup:  taskWith("owner", "/model Verifier CLAUDE-Sonnet-5-5"),
			labels: []string{"stage:ready", "verifier-model:sonnet"},
			reacts: []string{app + " +1"}, writes: []string{label, react}},
		{name: "/effort takes a level in any case, and keeps its label in another case",
			setup:  taskWith("owner", "/effort High", "owner Effort:HIGH", app+" effort:low"),
			labels: []string{"Effort:HIGH", "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{"DELETE /issues/1/labels/effort:low", react}},
		{name: "/model gpt is rejected with a reply",
			setup:   taskWith("owner", "/model gpt", app+" model:sonnet"),
			labels:  []string{"model:sonnet", "stage:ready"},
			comment: rejected("/model gpt", notGPT, "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/effort extreme is rejected with a reply",
			setup:   taskWith("owner", "/effort extreme", app+" effort:low"),
			labels:  []string{"effort:low", "stage:ready"},
			comment: rejected("/effort extreme", "`extreme` isn't an effort level: the levels are `low`, `medium`, `high`, `xhigh` and `max`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model verifier with no name is rejected",
			setup:   taskWith("owner", "/model verifier"),
			labels:  []string{"stage:ready"},
			comment: rejected("/model verifier", "bad argument: `/model` takes a model name, or `verifier` and a model name", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "a rejection escapes the text it quotes",
			setup:   taskWith("owner", "/model <!--x-->"),
			labels:  []string{"stage:ready"},
			comment: rejected("/model &lt;!--x-->", "`&lt;!--x-->` isn't on `models.allowed`, which has `opus`, `sonnet` and `"+longSuffix+"`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model is rejected when the config allows no model",
			allowed: "[]",
			setup:   taskWith("owner", "/model opus"),
			labels:  []string{"stage:ready"},
			comment: rejected("/model opus", "`opus` isn't on `models.allowed`, and the config allows no model", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model is rejected when its name matches more than one ignoring case",
			allowed: "[{name: Opus, label: o1}, {name: opus, label: o2}]",
			setup:   taskWith("owner", "/model OPUS"),
			labels:  []string{"stage:ready"},
			comment: rejected("/model OPUS", "`OPUS` matches `Opus` and `opus` on `models.allowed`, ignoring case, so give the name in its exact case", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model of a model no longer allowed whose label the issue has, as after a crash and a reload, stands",
			setup:  taskWith("owner", "/model Fable", app+" model:fable", app+" model:opus"),
			labels: []string{"model:fable", "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{"DELETE /issues/1/labels/model:opus", react}},
		{name: "an untrusted user's /model isn't processed",
			setup:  taskWith("mallory", "/model opus", app+" model:sonnet"),
			labels: []string{"model:sonnet", "stage:ready"}},
		{name: "a /model already applied isn't applied again",
			setup: then(taskWith("owner", "/model opus", app+" model:opus"), func(f *githubtest.Fake) {
				f.AddReaction(1002, app, "+1")
				f.AddLabel(1, "owner", "model:sonnet")
			}),
			labels: []string{"model:opus", "model:sonnet", "stage:ready"},
			reacts: []string{app + " +1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			allowed := tt.allowed
			if allowed == "" {
				allowed = threeModels
			}
			pass := settingPass(t, models(t, allowed))
			githubtest.Replay(t, tt.setup, pass)

			before := tt.setup(t).State().Issues[0]
			f := run(t, tt.setup, pass) // a second pass writes nothing
			is := f.State().Issues[0]
			if !reflect.DeepEqual(is.Labels, tt.labels) {
				t.Errorf("labels = %q, want %q", is.Labels, tt.labels)
			}
			var added []string
			for _, c := range is.Comments[len(before.Comments):] {
				added = append(added, c.Body)
			}
			if want := []string{tt.comment}; tt.comment == "" && added != nil || tt.comment != "" && !reflect.DeepEqual(added, want) {
				t.Errorf("new comments = %q, want %q", added, tt.comment)
			}
			if got := is.Comments[len(before.Comments)-1].Reactions; !reflect.DeepEqual(got, tt.reacts) {
				t.Errorf("the command's reactions = %q, want %q", got, tt.reacts)
			}
			var writes []string
			for _, w := range f.Writes() {
				writes = append(writes, w.Method+" "+w.Path)
			}
			if !reflect.DeepEqual(writes, tt.writes) {
				t.Errorf("writes = %q, want %q", writes, tt.writes)
			}
		})
	}
}

// The handlers ask Daemon.CurrentModels on each call, so a reload that
// changes models.allowed on main changes what /model accepts, both ways.
func TestIterateModelsReload(t *testing.T) {
	f := newFake(t)
	fileAt(f, "ready", "do it")
	o := newOrigin(t)
	o.commit(config.Path, "types: labels\nmodels:\n  allowed: [opus]\n")
	d := newDaemon(t, f, o, nil)
	maps.Copy(d.Step.Commands, daemon.SettingCommands(d.CurrentModels))
	last := func() githubtest.CommentState {
		cs := f.State().Issues[0].Comments
		return cs[len(cs)-1]
	}

	f.AddComment(1, "owner", "/model sonnet")
	iterate(t, d)
	if got := last().Body; !strings.Contains(got, "`sonnet` isn't on `models.allowed`, which has `opus`.") {
		t.Errorf("the reply to /model sonnet = %q, want its rejection", got)
	}

	o.commit(config.Path, "types: labels\nmodels:\n  allowed: [opus, sonnet]\n")
	id := f.AddComment(1, "owner", "/model sonnet")
	iterate(t, d)
	if got := f.State().Issues[0].Labels; !reflect.DeepEqual(got, []string{"model:sonnet", "stage:ready"}) {
		t.Errorf("labels after a reload allowed sonnet = %q, want model:sonnet and stage:ready", got)
	}
	if got := last().Reactions; !reflect.DeepEqual(got, []string{app + " +1"}) {
		t.Errorf("reactions to comment %d = %q, want the App's 👍", id, got)
	}

	o.commit(config.Path, "types: labels\nmodels:\n  allowed: [opus]\n")
	f.AddComment(1, "owner", "/model verifier sonnet")
	iterate(t, d)
	if got := last().Body; !strings.Contains(got, "`sonnet` isn't on `models.allowed`, which has `opus`.") {
		t.Errorf("the reply to /model verifier sonnet = %q, want its rejection once a reload dropped sonnet", got)
	}
	if got := f.State().Issues[0].Labels; !reflect.DeepEqual(got, []string{"model:sonnet", "stage:ready"}) {
		t.Errorf("labels = %q, want model:sonnet and stage:ready still", got)
	}
}

// CurrentModels is empty before a config loads.
func TestCurrentModelsNoConfig(t *testing.T) {
	if got := (&daemon.Daemon{}).CurrentModels(); got.Allowed != nil {
		t.Errorf("CurrentModels() = %+v, want none", got)
	}
}
