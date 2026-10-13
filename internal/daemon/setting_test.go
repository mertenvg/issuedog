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

// labeledBy adds labels to issue 1 as login, after what's there, as when
// login adds them after the command.
func labeledBy(login string, labels ...string) func(f *githubtest.Fake) {
	return func(f *githubtest.Fake) {
		for _, l := range labels {
			f.AddLabel(1, login, l)
		}
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
		{name: "/model of a model a reload dropped after Apply added its label stands",
			setup:  then(taskWith("owner", "/model Fable", app+" model:opus"), labeledBy(app, "model:fable")),
			labels: []string{"model:fable", "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{"DELETE /issues/1/labels/model:opus", react}},
		{name: "/model of a long name a reload dropped after Apply added its label of the last 35 characters stands",
			allowed: "[opus, {name: claude-sonnet-5-5, label: sonnet}]",
			setup:   then(taskWith("owner", "/model "+long, app+" model:opus"), labeledBy(app, "model:"+longSuffix)),
			labels:  []string{"model:" + longSuffix, "stage:ready"},
			reacts:  []string{app + " +1"}, writes: []string{"DELETE /issues/1/labels/model:opus", react}},
		{name: "/model of a dropped model keeps the one stray label the App added since, removing a stray one from before; a label with no colon isn't one",
			allowed: "[sonnet]",
			setup:   then(taskWith("owner", "/model claude-opus-5-5", app+" model:retired", app+" model:sonnet"), labeledBy(app, "model", "model:opus5")),
			labels:  []string{"model", "model:opus5", "stage:ready"},
			reacts:  []string{app + " +1"}, writes: []string{"DELETE /issues/1/labels/model:retired", "DELETE /issues/1/labels/model:sonnet", react}},
		{name: "/model of a dropped model keeps a stray label the App added since in another case",
			allowed: "[sonnet]",
			setup:   then(taskWith("owner", "/model old", app+" model:sonnet"), labeledBy(app, "Model:old")),
			labels:  []string{"Model:old", "stage:ready"},
			reacts:  []string{app + " +1"}, writes: []string{"DELETE /issues/1/labels/model:sonnet", react}},
		{name: "/model of a dropped model is rejected with two stray labels the App added since",
			allowed: "[sonnet]",
			setup:   then(taskWith("owner", "/model claude-opus-5-5"), labeledBy(app, "model:opus5", "model:haiku")),
			labels:  []string{"model:haiku", "model:opus5", "stage:ready"},
			comment: rejected("/model claude-opus-5-5", "`claude-opus-5-5` isn't on `models.allowed`, which has `sonnet`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model gpt is rejected on an issue with a stray label from before it",
			allowed: "[sonnet]",
			setup:   taskWith("owner", "/model gpt", app+" model:sonnet", app+" model:retired"),
			labels:  []string{"model:retired", "model:sonnet", "stage:ready"},
			comment: rejected("/model gpt", "`gpt` isn't on `models.allowed`, which has `sonnet`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model verifier gpt is rejected on an issue with a stray verifier label from before it",
			allowed: "[sonnet]",
			setup:   taskWith("owner", "/model verifier gpt", app+" verifier-model:sonnet", app+" verifier-model:retired"),
			labels:  []string{"stage:ready", "verifier-model:retired", "verifier-model:sonnet"},
			comment: rejected("/model verifier gpt", "`gpt` isn't on `models.allowed`, which has `sonnet`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model of a dropped model is rejected when a person, not the App, added its stray label since",
			allowed: "[sonnet]",
			setup:   then(taskWith("owner", "/model fable", app+" model:sonnet"), labeledBy("owner", "model:fable")),
			labels:  []string{"model:fable", "model:sonnet", "stage:ready"},
			comment: rejected("/model fable", "`fable` isn't on `models.allowed`, which has `sonnet`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model of a dropped model is rejected when the App's stray label was removed and added back by a person since",
			allowed: "[sonnet]",
			setup: then(taskWith("owner", "/model fable", app+" model:sonnet"), func(f *githubtest.Fake) {
				f.AddLabel(1, app, "model:fable")
				f.RemoveLabel(1, "owner", "model:fable")
				f.AddLabel(1, "owner", "model:fable")
			}),
			labels:  []string{"model:fable", "model:sonnet", "stage:ready"},
			comment: rejected("/model fable", "`fable` isn't on `models.allowed`, which has `sonnet`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model leaves a label whose kind only starts with model",
			setup:  taskWith("owner", "/model opus", "owner modelx:foo", app+" model:sonnet"),
			labels: []string{"model:opus", "modelx:foo", "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{label, "DELETE /issues/1/labels/model:sonnet", react}},
		{name: "/model picks the exact name among names that differ only in case",
			allowed: "[{name: Opus, label: o1}, {name: opus, label: o2}]",
			setup:   taskWith("owner", "/model Opus"),
			labels:  []string{"model:o1", "stage:ready"},
			reacts:  []string{app + " +1"}, writes: []string{label, react}},
		{name: "/model picks the other exact name among names that differ only in case",
			allowed: "[{name: Opus, label: o1}, {name: opus, label: o2}]",
			setup:   taskWith("owner", "/model opus"),
			labels:  []string{"model:o2", "stage:ready"},
			reacts:  []string{app + " +1"}, writes: []string{label, react}},
		{name: "/effort HIGH sets effort:high on an issue with none",
			setup:  taskWith("owner", "/effort HIGH"),
			labels: []string{"effort:high", "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{label, react}},
		{name: "/model VERIFIER opus is the verifier form",
			setup:  taskWith("owner", "/model VERIFIER opus", app+" model:sonnet"),
			labels: []string{"model:sonnet", "stage:ready", "verifier-model:opus"},
			reacts: []string{app + " +1"}, writes: []string{label, react}},
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

// A /model whose Apply crashed after adding its label, before removing the
// old one, isn't rejected on the next pass after a reload dropped its model:
// the issue ends with one model:* label, and the command 👍'd.
func TestModelRetryAfterReloadDropsIt(t *testing.T) {
	before := models(t, "[sonnet, {name: claude-opus-5-5, label: opus5}]")
	for _, crash := range []struct {
		name  string
		at    func(f *githubtest.Fake)
		after string // models.allowed after the reload
	}{
		{"before the removal", func(f *githubtest.Fake) { f.FailWrite(2) }, "[sonnet]"},
		{"after the add applied", func(f *githubtest.Fake) { f.FailWriteAfter(1) }, "[sonnet]"},
		{"before the removal, and the reload dropped both models", func(f *githubtest.Fake) { f.FailWrite(2) }, "[haiku]"},
		{"after the add applied, and the reload dropped both models", func(f *githubtest.Fake) { f.FailWriteAfter(1) }, "[haiku]"},
	} {
		after := models(t, crash.after)
		t.Run(crash.name, func(t *testing.T) {
			f := taskWith("owner", "/model claude-opus-5-5", app+" model:sonnet")(t)
			crash.at(f)
			if err := settingPass(t, before)(f); err == nil {
				t.Fatal("the crashed pass returned no error")
			}
			if got := f.State().Issues[0].Labels; !reflect.DeepEqual(got, []string{"model:opus5", "model:sonnet", "stage:ready"}) {
				t.Fatalf("labels after the crash = %q, want model:opus5 added and model:sonnet still there", got)
			}
			f.FailWrite(0)
			if err := settingPass(t, after)(f); err != nil { // the reload dropped claude-opus-5-5
				t.Fatal(err)
			}
			is := f.State().Issues[0]
			if !reflect.DeepEqual(is.Labels, []string{"model:opus5", "stage:ready"}) {
				t.Errorf("labels = %q, want model:opus5 alone", is.Labels)
			}
			if len(is.Comments) != 2 || !reflect.DeepEqual(is.Comments[1].Reactions, []string{app + " +1"}) {
				t.Errorf("comments = %+v, want the command 👍'd and no reply", is.Comments)
			}
		})
	}
}

// A failed timeline read, on the path that looks for a begun Apply, fails
// the pass rather than rejecting the command, and nothing is written.
func TestModelTimelineFails(t *testing.T) {
	f := taskWith("owner", "/model gpt", app+" model:retired")(t)
	timeline := func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1/timeline" }
	// The label rules read the timeline first; the command's read fails.
	f.BeforeRequest(timeline, func() { f.AnswerOnce(timeline, 500) })
	err := settingPass(t, models(t, "[sonnet]"))(f)
	if err == nil || !strings.Contains(err.Error(), "labels added since comment 1002") {
		t.Errorf("pass = %v, want the timeline error", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes = %+v, want none", w)
	}
}

// A stray label whose latest event is an unlabeled one, as when it went
// after the issue was read, isn't taken, even when the App removed it.
func TestModelStrayLabelGoneSince(t *testing.T) {
	f := then(taskWith("owner", "/model fable", app+" model:sonnet"), labeledBy(app, "model:fable"))(t)
	timeline := func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1/timeline" }
	// The label rules read the timeline first; before the command's read,
	// the label goes.
	f.BeforeRequest(timeline, func() {
		f.BeforeRequest(timeline, func() { f.RemoveLabel(1, app, "model:fable") })
	})
	if err := settingPass(t, models(t, "[sonnet]"))(f); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	if !reflect.DeepEqual(is.Labels, []string{"model:sonnet", "stage:ready"}) {
		t.Errorf("labels = %q, want model:sonnet still", is.Labels)
	}
	if got := is.Comments[1].Reactions; !reflect.DeepEqual(got, []string{app + " -1"}) {
		t.Errorf("the command's reactions = %q, want the App's 👎", got)
	}
}

// A failed write ends Apply with an error and writes nothing after it, so
// the command isn't 👍'd with its labels half set: the label's add, the old
// label's removal and the 👍, for each command that sets a label.
func TestSettingWriteFails(t *testing.T) {
	for _, tc := range []struct{ body, old string }{
		{"/model opus", "model:sonnet"},
		{"/effort verifier high", "verifier-effort:low"},
		{"/priority p1", "priority:p2"},
	} {
		for n := 1; n <= 3; n++ {
			f := taskWith("owner", tc.body, app+" "+tc.old)(t)
			f.FailWrite(n)
			if err := settingPass(t, models(t, threeModels))(f); err == nil {
				t.Errorf("%s with write %d failing: no error", tc.body, n)
			}
			if w := f.Writes(); len(w) != n {
				t.Errorf("%s with write %d failing: writes %+v, want none after it", tc.body, n, w)
			}
		}
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
