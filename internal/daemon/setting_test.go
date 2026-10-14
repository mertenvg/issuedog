package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mertenvg/issuedog/internal/config"
	"github.com/mertenvg/issuedog/internal/daemon"
	"github.com/mertenvg/issuedog/internal/github"
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
	return settingPassTrust(t, allowed, trusted{"owner": true})
}

// settingPassTrust is settingPass with daemon.SettingCommands given
// handlers as its trust; the rest of step 1 still trusts "owner" alone.
func settingPassTrust(t testing.TB, allowed config.Models, handlers github.Trust) func(*githubtest.Fake) error {
	return func(f *githubtest.Fake) error {
		c := f.Client(t)
		s, err := c.Snapshot(context.Background(), "o", "r", 1)
		if err != nil {
			return err
		}
		tr := trusted{"owner": true}
		st := &daemon.Step{Applier: &daemon.Applier{Client: c, Owner: "o", Repo: "r", AppSlug: slug}, Trust: tr, Commands: daemon.Commands(tr)}
		maps.Copy(st.Commands, daemon.SettingCommands(handlers, func() config.Models { return allowed }))
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
		{name: "/model of a dropped model keeps a stray label the App added since, renamed in another case since",
			allowed: "[sonnet]",
			setup: then(taskWith("owner", "/model old", app+" model:sonnet"), func(f *githubtest.Fake) {
				f.AddLabel(1, app, "model:old")
				f.RenameLabel("model:old", "Model:OLD")
			}),
			labels: []string{"Model:OLD", "stage:ready"},
			reacts: []string{app + " +1"}, writes: []string{"DELETE /issues/1/labels/model:sonnet", react}},
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
		{name: "/model of a dropped model is rejected when the App added labels of other kinds since",
			allowed: "[sonnet]",
			setup:   then(taskWith("owner", "/model fable", app+" model:sonnet"), labeledBy(app, "verifier-model:fable", "modelx:fable", "effort:high")),
			labels:  []string{"effort:high", "model:sonnet", "modelx:fable", "stage:ready", "verifier-model:fable"},
			comment: rejected("/model fable", "`fable` isn't on `models.allowed`, which has `sonnet`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model verifier of a dropped model is rejected when the App added a model:* label since",
			allowed: "[sonnet]",
			setup:   then(taskWith("owner", "/model verifier fable"), labeledBy(app, "model:fable")),
			labels:  []string{"model:fable", "stage:ready"},
			comment: rejected("/model verifier fable", "`fable` isn't on `models.allowed`, which has `sonnet`", "1002"),
			reacts:  []string{app + " -1"}, writes: []string{react, reply}},
		{name: "/model of a dropped model is rejected when the App added a still-allowed model's label since",
			allowed: "[sonnet]",
			setup:   then(taskWith("owner", "/model fable"), labeledBy(app, "model:sonnet")),
			labels:  []string{"model:sonnet", "stage:ready"},
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

// beforeReload allows claude-opus-5-5, labeled opus5, which the reloads in
// the crash tests drop.
const beforeReload = "[sonnet, {name: claude-opus-5-5, label: opus5}]"

// reloadAfterCrash runs a pass on f under before, with crash set, which
// must fail; then, after a reload to after, a pass that must succeed. The
// handlers of daemon.SettingCommands trust as handlers does.
func reloadAfterCrash(t *testing.T, f *githubtest.Fake, crash func(f *githubtest.Fake), before, after config.Models, handlers github.Trust) {
	t.Helper()
	crash(f)
	if err := settingPassTrust(t, before, handlers)(f); err == nil {
		t.Fatal("the crashed pass returned no error")
	}
	f.FailWrite(0)
	if err := settingPassTrust(t, after, handlers)(f); err != nil {
		t.Fatal(err)
	}
}

// acked checks that issue 1 has labels, and its command, comment 1002, the
// App's 👍 and no reply.
func acked(t *testing.T, f *githubtest.Fake, labels ...string) {
	t.Helper()
	is := f.State().Issues[0]
	if !reflect.DeepEqual(is.Labels, labels) {
		t.Errorf("labels = %q, want %q", is.Labels, labels)
	}
	if len(is.Comments) != 2 || !reflect.DeepEqual(is.Comments[1].Reactions, []string{app + " +1"}) {
		t.Errorf("comments = %+v, want the command 👍'd and no reply", is.Comments)
	}
}

// addRemove adds label to issue 1 as login, then removes it as login.
func addRemove(login, label string) func(f *githubtest.Fake) {
	return func(f *githubtest.Fake) {
		f.AddLabel(1, login, label)
		f.RemoveLabel(1, login, label)
	}
}

// crashes are the two crash points of a /model whose first write is its
// add: before its second write, and after its first applied.
var crashes = []struct {
	name string
	at   func(f *githubtest.Fake)
}{
	{"before write 2", func(f *githubtest.Fake) { f.FailWrite(2) }},
	{"after write 1 applied", func(f *githubtest.Fake) { f.FailWriteAfter(1) }},
}

// mallory adds model:opus5 and then removes it, before or after the
// owner's /model claude-opus-5-5, so the label rules have nothing to put
// back. The command's Apply adds model:opus5 and crashes, and a reload
// drops the model. That add restores no accepted state, so it's the begun
// Apply's, not a revert: the command ends 👍'd, with model:opus5 alone
// (#116, item 1).
func TestModelRetryAfterUntrustedAddAndRemove(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(testing.TB) *githubtest.Fake
	}{
		{"before the command", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			file(f, 0, ready)
			labeledBy(app, "model:sonnet")(f)
			addRemove("mallory", "model:opus5")(f)
			if id := f.AddComment(1, "owner", "/model claude-opus-5-5"); id != 1002 {
				t.Fatalf("command ID = %d, want 1002", id)
			}
			return f
		}},
		{"after the command", then(taskWith("owner", "/model claude-opus-5-5", app+" model:sonnet"), addRemove("mallory", "model:opus5"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, crash := range crashes {
				t.Run(crash.name, func(t *testing.T) {
					f := tc.setup(t)
					reloadAfterCrash(t, f, crash.at, models(t, beforeReload), models(t, "[sonnet]"), trusted{"owner": true})
					acked(t, f, "model:opus5", "stage:ready")
				})
			}
			// The issue as the crash left it: Apply added model:opus5.
			setup := then(tc.setup, labeledBy(app, "model:opus5"))
			pass := settingPass(t, models(t, "[sonnet]"))
			githubtest.Replay(t, setup, pass)
			acked(t, run(t, setup, pass), "model:opus5", "stage:ready")
		})
	}
}

// The issue has model:opus5 already when the owner posts /model
// claude-opus-5-5, so Apply's add writes nothing. It removes the other
// model labels and crashes before its 👍, and a reload drops the model, so
// model:opus5 is the only stray label. The App removed another since the
// command, not as a revert, so the command ends 👍'd (#116, item 2): when
// the reload drops sonnet too, and when Apply removed one label and not
// yet the allowed model:sonnet, which it then removes.
func TestModelRetryAfterNoOpAdd(t *testing.T) {
	for _, tc := range []struct {
		name   string
		labels []string // the App's, before the command
		crash  func(f *githubtest.Fake)
		after  string // models.allowed after the reload
	}{
		{"before the 👍", []string{"model:opus5", "model:sonnet"}, func(f *githubtest.Fake) { f.FailWrite(2) }, "[sonnet]"},
		{"after the removal applied", []string{"model:opus5", "model:sonnet"}, func(f *githubtest.Fake) { f.FailWriteAfter(1) }, "[sonnet]"},
		{"after the removal applied, and the reload dropped both models", []string{"model:opus5", "model:sonnet"}, func(f *githubtest.Fake) { f.FailWriteAfter(1) }, "[haiku]"},
		{"between the removals", []string{"model:old", "model:opus5", "model:sonnet"}, func(f *githubtest.Fake) { f.FailWriteAfter(1) }, "[sonnet]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var labels []string
			for _, l := range tc.labels {
				labels = append(labels, app+" "+l)
			}
			f := taskWith("owner", "/model claude-opus-5-5", labels...)(t)
			reloadAfterCrash(t, f, tc.crash, models(t, beforeReload), models(t, tc.after), trusted{"owner": true})
			acked(t, f, "model:opus5", "stage:ready")
		})
	}
	// The issue as the crash left it: Apply removed model:sonnet.
	setup := then(taskWith("owner", "/model claude-opus-5-5", app+" model:opus5", app+" model:sonnet"),
		func(f *githubtest.Fake) { f.RemoveLabel(1, app, "model:sonnet") })
	pass := settingPass(t, models(t, "[sonnet]"))
	githubtest.Replay(t, setup, pass)
	acked(t, run(t, setup, pass), "model:opus5", "stage:ready")
}

// The App's removal names no label when two are stray: here the owner adds
// model:aaa after a crash like TestModelRetryAfterNoOpAdd's, so the command
// is rejected, the known limit, and both labels stay.
func TestModelNoOpAddTwoStrays(t *testing.T) {
	f := taskWith("owner", "/model claude-opus-5-5", app+" model:opus5", app+" model:sonnet")(t)
	f.FailWriteAfter(1)
	if err := settingPass(t, models(t, beforeReload))(f); err == nil {
		t.Fatal("the crashed pass returned no error")
	}
	f.FailWrite(0)
	f.AddLabel(1, "owner", "model:aaa")
	if err := settingPass(t, models(t, "[sonnet]"))(f); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	if want := []string{"model:aaa", "model:opus5", "stage:ready"}; !reflect.DeepEqual(is.Labels, want) {
		t.Errorf("labels = %q, want %q", is.Labels, want)
	}
	if got := is.Comments[1].Reactions; !reflect.DeepEqual(got, []string{app + " -1"}) {
		t.Errorf("the command's reactions = %q, want the App's 👎", got)
	}
}

// An App removal of a model label counts for /model gpt only if it's since
// the command and not the label rules' revert, so these still reject it,
// and the stray model:retired stays: the App reverting mallory's add, with
// its comment or without, as when mallory deleted it; the App's removal
// before the command; and the owner's, not the App's, since.
func TestModelRemovalNotApply(t *testing.T) {
	const reason = "`gpt` isn't on `models.allowed`, which has `sonnet`"
	for _, tc := range []struct {
		name   string
		setup  func(testing.TB) *githubtest.Fake
		labels []string
	}{
		{"a revert", then(taskWith("owner", "/model gpt", app+" model:retired"), labeledBy("mallory", "model:foo")),
			[]string{"model:retired", "stage:ready"}},
		{"a revert without its comment", then(taskWith("owner", "/model gpt", app+" model:retired"), func(f *githubtest.Fake) {
			f.AddLabel(1, "mallory", "model:foo")
			f.RemoveLabel(1, app, "model:foo")
		}), []string{"model:retired", "stage:ready"}},
		{"before the command", func(t testing.TB) *githubtest.Fake {
			f := newFake(t)
			file(f, 0, ready)
			labeledBy(app, "model:old", "model:retired")(f)
			f.RemoveLabel(1, app, "model:old")
			if id := f.AddComment(1, "owner", "/model gpt"); id != 1002 {
				t.Fatalf("command ID = %d, want 1002", id)
			}
			return f
		}, []string{"model:retired", "stage:ready"}},
		{"by the owner", then(taskWith("owner", "/model gpt", app+" model:retired", app+" model:sonnet"),
			func(f *githubtest.Fake) { f.RemoveLabel(1, "owner", "model:sonnet") }), []string{"model:retired", "stage:ready"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pass := settingPass(t, models(t, "[sonnet]"))
			githubtest.Replay(t, tc.setup, pass)

			is := run(t, tc.setup, pass).State().Issues[0]
			if !reflect.DeepEqual(is.Labels, tc.labels) {
				t.Errorf("labels = %q, want %q", is.Labels, tc.labels)
			}
			if got := is.Comments[1].Reactions; !reflect.DeepEqual(got, []string{app + " -1"}) {
				t.Errorf("the command's reactions = %q, want the App's 👎", got)
			}
			if got := is.Comments[len(is.Comments)-1].Body; got != rejected("/model gpt", reason, "1002") {
				t.Errorf("the last comment = %q, want the rejection", got)
			}
		})
	}
}

// A failed trust lookup, while telling a begun Apply's add from a revert,
// fails the pass with the error wrapped, and nothing is written.
func TestModelTrustFails(t *testing.T) {
	f := then(taskWith("owner", "/model claude-opus-5-5", app+" model:sonnet"), func(f *githubtest.Fake) {
		addRemove("mallory", "model:opus5")(f)
		f.AddLabel(1, app, "model:opus5")
	})(t)
	err := settingPassTrust(t, models(t, "[sonnet]"), failTrust{})(f)
	if !errors.Is(err, errTrust) || !strings.Contains(err.Error(), "labels added since comment 1002: labeled model:opus5: revert: trust mallory: ") {
		t.Errorf("pass = %v, want errTrust, wrapped", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes = %+v, want none", w)
	}
}

// The App removed model:opus5 before the owner's /model claude-opus-5-5,
// whose Apply adds it back and crashes before a reload drops the model.
// That removal is the App's own, so trust isn't asked about it, here
// failing if it is, and the add is the begun Apply's.
func TestModelAppRemovalTrustNotAsked(t *testing.T) {
	setup := func(t testing.TB) *githubtest.Fake {
		f := newFake(t)
		file(f, 0, ready)
		labeledBy(app, "model:opus5", "model:sonnet")(f)
		f.RemoveLabel(1, app, "model:opus5")
		if id := f.AddComment(1, "owner", "/model claude-opus-5-5"); id != 1002 {
			t.Fatalf("command ID = %d, want 1002", id)
		}
		return f
	}
	for _, crash := range crashes {
		t.Run(crash.name, func(t *testing.T) {
			f := setup(t)
			reloadAfterCrash(t, f, crash.at, models(t, beforeReload), models(t, "[sonnet]"), failTrust{})
			acked(t, f, "model:opus5", "stage:ready")
		})
	}
	// The issue as the crash left it: Apply added model:opus5.
	crashed := then(setup, labeledBy(app, "model:opus5"))
	pass := settingPassTrust(t, models(t, "[sonnet]"), failTrust{})
	githubtest.Replay(t, crashed, pass)
	acked(t, run(t, crashed, pass), "model:opus5", "stage:ready")
}

// An untrusted user removes a stray label, before or after the owner's
// /model gpt, and the label rules put it back as the App. That labeled event
// is the revert, not a begun Apply, so /model gpt is still rejected, and the
// allowed label stays (the verifier's attack on attempt 3).
func TestModelRevertIsNotApply(t *testing.T) {
	const reason = "`gpt` isn't on `models.allowed`, which has `sonnet`"
	for _, tc := range []struct {
		name   string
		labels []string
		before bool // the removal is before the command
	}{
		{"one stray, removed after", []string{"model:retired", "model:sonnet"}, false},
		{"one stray, removed before", []string{"model:retired", "model:sonnet"}, true},
		{"two strays, removed after", []string{"model:older", "model:retired"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := func(t testing.TB) *githubtest.Fake {
				f := newFake(t)
				file(f, 0, ready)
				labeledBy(app, tc.labels...)(f)
				if tc.before {
					f.RemoveLabel(1, "mallory", "model:retired")
				}
				if id := f.AddComment(1, "owner", "/model gpt"); id != 1002 {
					t.Fatalf("command ID = %d, want 1002", id)
				}
				if !tc.before {
					f.RemoveLabel(1, "mallory", "model:retired")
				}
				return f
			}
			pass := settingPass(t, models(t, "[sonnet]"))
			githubtest.Replay(t, setup, pass)

			is := run(t, setup, pass).State().Issues[0]
			if want := append(slices.Clone(tc.labels), "stage:ready"); !reflect.DeepEqual(is.Labels, want) {
				t.Errorf("labels = %q, want %q", is.Labels, want)
			}
			if got := is.Comments[1].Reactions; !reflect.DeepEqual(got, []string{app + " -1"}) {
				t.Errorf("the command's reactions = %q, want the App's 👎", got)
			}
			if got := is.Comments[len(is.Comments)-1].Body; got != rejected("/model gpt", reason, "1002") {
				t.Errorf("the last comment = %q, want the rejection", got)
			}
		})
	}
}

// A pass reverts mallory's removal of a stray label, then fails before the
// command; mallory deletes the revert's comment, or edits it so it's dirty,
// and its marker can't be read. The revert is still told by the removal's
// untrusted actor, so the next pass rejects /model gpt, and model:sonnet
// stays (the verifier's probe on attempt 4).
func TestModelRevertMarkerGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		hide func(f *githubtest.Fake, id int64, body string)
	}{
		{"deleted", func(f *githubtest.Fake, id int64, _ string) { f.DeleteComment(id) }},
		{"edited", func(f *githubtest.Fake, id int64, body string) { f.EditComment(id, "mallory", body+" hi") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := taskWith("owner", "/model gpt", app+" model:retired", app+" model:sonnet")(t)
			f.RemoveLabel(1, "mallory", "model:retired")
			f.AnswerOnce(func(r githubtest.Request) bool {
				return r.Method == "GET" && r.Path == "/issues/comments/1002/reactions"
			}, 500)
			pass := settingPass(t, models(t, "[sonnet]"))
			if err := pass(f); err == nil {
				t.Fatal("the first pass returned no error")
			}
			cs, _, err := f.Client(t).Issues.ListComments(context.Background(), "o", "r", 1, nil)
			if err != nil {
				t.Fatal(err)
			}
			revert := cs[len(cs)-1]
			if !strings.Contains(revert.GetBody(), "<!-- issuedog:revert key=") {
				t.Fatalf("the last comment = %q, want the revert's", revert.GetBody())
			}
			tc.hide(f, revert.GetID(), revert.GetBody())
			if err := pass(f); err != nil {
				t.Fatal(err)
			}
			is := f.State().Issues[0]
			if want := []string{"model:retired", "model:sonnet", "stage:ready"}; !reflect.DeepEqual(is.Labels, want) {
				t.Errorf("labels = %q, want %q", is.Labels, want)
			}
			if got := is.Comments[1].Reactions; !reflect.DeepEqual(got, []string{app + " -1"}) {
				t.Errorf("the command's reactions = %q, want the App's 👎", got)
			}
			if got := is.Comments[len(is.Comments)-1].Body; got != rejected("/model gpt", "`gpt` isn't on `models.allowed`, which has `sonnet`", "1002") {
				t.Errorf("the last comment = %q, want the rejection", got)
			}
		})
	}
}

// markerSetup files issue 1, whose model:opus5 owner, a trusted user,
// removed; then the App's note, edited by editor to hold a revert marker
// naming that removal; then the owner's /model claude-opus-5-5, comment
// 1003; then the App adding model:opus5 back, as a begun Apply would. The
// removal is trusted, so only the marker can make that add a revert. It
// returns the marker and the note's ID too.
func markerSetup(t testing.TB, editor string) (*githubtest.Fake, string, int64) {
	f := newFake(t)
	file(f, 0, ready)
	labeledBy(app, "model:opus5", "model:sonnet")(f)
	marker := fmt.Sprintf("<!-- issuedog:revert key=%d -->", f.RemoveLabel(1, "owner", "model:opus5"))
	note := f.AddComment(1, app, "A note.")
	f.EditComment(note, editor, "A note.\n\n"+marker)
	if id := f.AddComment(1, "owner", "/model claude-opus-5-5"); id != 1003 {
		t.Fatalf("command ID = %d, want 1003", id)
	}
	f.AddLabel(1, app, "model:opus5")
	return f, marker, note
}

// A revert marker is read only from a clean comment of the App's. A clean
// one naming the removal makes the App's add a revert, so the command is
// rejected. In a comment mallory edited, MarkDirty escapes it before
// commands run, so the add is the begun Apply.
func TestModelRevertMarker(t *testing.T) {
	for _, tc := range []struct {
		editor string
		labels []string
		react  string
	}{
		{app, []string{"model:opus5", "model:sonnet", "stage:ready"}, "-1"},
		{"mallory", []string{"model:opus5", "stage:ready"}, "+1"},
	} {
		t.Run("edited by "+tc.editor, func(t *testing.T) {
			setup := func(t testing.TB) *githubtest.Fake {
				f, _, _ := markerSetup(t, tc.editor)
				return f
			}
			pass := settingPass(t, models(t, "[sonnet]"))
			githubtest.Replay(t, setup, pass)

			is := run(t, setup, pass).State().Issues[0]
			if !reflect.DeepEqual(is.Labels, tc.labels) {
				t.Errorf("labels = %q, want %q", is.Labels, tc.labels)
			}
			if want := []string{app + " " + tc.react}; !reflect.DeepEqual(is.Comments[2].Reactions, want) {
				t.Errorf("the command's reactions = %q, want %q", is.Comments[2].Reactions, want)
			}
		})
	}
}

// addedSince skips a marker in a dirty comment itself: here mallory puts the
// raw marker back in her dirty comment after MarkDirty escaped it, before
// the command's timeline read, and the begun Apply still stands.
func TestModelRevertMarkerDirtySkipped(t *testing.T) {
	f, marker, note := markerSetup(t, "mallory")
	timeline := func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1/timeline" }
	// The label rules read the timeline first, after the dirty content is
	// read; then Check's read.
	f.BeforeRequest(timeline, func() {
		f.BeforeRequest(timeline, func() { f.EditComment(note, "mallory", "A note.\n\n"+marker) })
	})
	if err := settingPass(t, models(t, "[sonnet]"))(f); err != nil {
		t.Fatal(err)
	}
	is := f.State().Issues[0]
	if !strings.Contains(is.Comments[1].Body, marker) {
		t.Fatalf("the note = %q, want the raw marker in it", is.Comments[1].Body)
	}
	if want := []string{"model:opus5", "stage:ready"}; !reflect.DeepEqual(is.Labels, want) {
		t.Errorf("labels = %q, want %q", is.Labels, want)
	}
	if want := []string{app + " +1"}; !reflect.DeepEqual(is.Comments[2].Reactions, want) {
		t.Errorf("the command's reactions = %q, want %q", is.Comments[2].Reactions, want)
	}
}

// The command's comment gone from the timeline, as when it's deleted after
// the issue was read, fails the pass rather than rejecting the command.
func TestModelCommandNotOnTimeline(t *testing.T) {
	f := taskWith("owner", "/model gpt", app+" model:retired")(t)
	timeline := func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1/timeline" }
	// The label rules read the timeline first; before Check's read, the
	// command goes.
	f.BeforeRequest(timeline, func() { f.BeforeRequest(timeline, func() { f.DeleteComment(1002) }) })
	err := settingPass(t, models(t, "[sonnet]"))(f)
	if err == nil || !strings.Contains(err.Error(), "the comment isn't on the timeline of o/r#1") {
		t.Errorf("pass = %v, want the missing comment's error", err)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes = %+v, want none", w)
	}
}

// Apply reads the timeline again, and a failure there fails the pass, after
// Check's read succeeded: nothing is written, not even a 👎.
func TestModelApplyTimelineFails(t *testing.T) {
	f := then(taskWith("owner", "/model fable", app+" model:sonnet"), labeledBy(app, "model:fable"))(t)
	timeline := func(r githubtest.Request) bool { return r.Method == "GET" && r.Path == "/issues/1/timeline" }
	// The label rules' read, then Check's, then Apply's, which fails.
	f.BeforeRequest(timeline, func() {
		f.BeforeRequest(timeline, func() { f.AnswerOnce(timeline, 500) })
	})
	err := settingPass(t, models(t, "[sonnet]"))(f)
	if err == nil || !strings.Contains(err.Error(), "labels added since comment 1002") {
		t.Errorf("pass = %v, want the timeline error", err)
	}
	reads := 0
	for _, r := range f.Requests() {
		if timeline(r) {
			reads++
		}
	}
	if reads != 3 {
		t.Errorf("%d timeline reads, want 3: the label rules', Check's and Apply's", reads)
	}
	if w := f.Writes(); len(w) != 0 {
		t.Errorf("writes = %+v, want none", w)
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
	maps.Copy(d.Step.Commands, daemon.SettingCommands(d.Step.CurrentTrust(), d.CurrentModels))
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
