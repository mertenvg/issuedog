# issuedog specification

Status: draft 3, 2026-10-11

`issuedog` turns rough GitHub issues into reviewed pull requests. It asks the questions needed to write a spec, breaks the spec into ordered tasks, implements each task with headless Claude Code inside a Docker sandbox, verifies the result with real test runs and an independent reviewer, and opens a PR against a protected `main`. A human merges.

## 1. Goals

- Keep work moving on a repo while the owner is away, one task at a time.
- Make the owner's part fast: every question, decision and human task is self-contained and answerable from the GitHub mobile app.
- Treat automated testing and change verification as the first priority of every change.
- Use GitHub as the only control channel and the single source of truth. The daemon keeps no database.

## 2. Non-goals (for now)

- Running on GitHub-hosted runners or consuming GitHub Actions minutes.
- Parallel tasks within one repo.
- Merging PRs. Merging is always a human action.
- Answering on the owner's behalf. A question waits for an answer indefinitely.

## 3. Architecture

```
Mac host (kept awake while issuedog runs)
├── container: issuedog-rumi-black   (one per repo)
│   ├── issuedog daemon (Go, uid issuedog)   holds the GitHub App key, owns /work/<repo>.git
│   ├── claude -p (uid agent)                one process per stage run
│   ├── verify commands (uid agent)          no GitHub credentials
│   ├── /work/runs/<run-id>                  agent-owned clone for one run
│   └── /run/issuedog/<run-id>               context files, read-only to agent
└── container: issuedog-dmcn
```

- One container per repo, so a failure in one repo leaves the others running.
- The daemon acts on GitHub as a GitHub App (section 15) through a Go client: `go-github` with an App installation-token transport, plus raw REST calls for endpoints the client lacks, such as sub-issues and issue dependencies, and GraphQL for edit histories and deleted-comment events (6.4). There is no `gh` dependency. This gives native ETags for polling (section 8.1), typed responses, and tests against `httptest` fakes.
- The daemon starts a fresh `claude -p` process for every stage run, including every retry. Sessions are never resumed, so each run starts from the issue and the code, and the verifier never shares context with the implementer.
- The base image follows the existing sandbox setup: `~/.claude/CLAUDE.md` bind-mounted read-only, `--dangerously-skip-permissions` as the non-root `agent` uid, feature branch per task.
- Nothing in the container runs as root. The image starts the daemon as `issuedog`. The daemon binary is the only file with capabilities: `cap_setuid` and `cap_setgid` to start stage runs as `agent`, and `cap_net_admin` to apply the agent firewall at startup. It is `root:issuedog 0750`, so `agent` can't run it, and processes started as `agent` get no capabilities. Every `docker run` of the image needs `--cap-add NET_ADMIN`, without which the kernel refuses to start the binary, and must not use `--security-opt no-new-privileges`, which ignores file capabilities.
- The host must not sleep while issuedog runs (for example `caffeinate -s`, or the equivalent Docker Desktop setting).

### 3.1 Who writes to GitHub

Only the daemon writes to GitHub. Agents return structured output and the daemon validates and applies it.

- The GitHub App private key lives in `/state/app-key.pem`, on the state volume, readable only by the `issuedog` uid. It is imported once with `issuedog key import` (section 15), so it is never bind-mounted from the host or built into an image. The daemon mints short-lived installation tokens and uses them for its own API calls. Nothing running as `agent` ever receives a GitHub credential. That includes GitHub connectors and GitHub MCP servers, which are never configured for an agent.
- Claude and every repo command (tests, lint, build) run as the `agent` uid. A different uid cannot read the key file or the daemon's memory.
- Claude's own credential (`CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`) is necessarily in the agent's environment. Outbound traffic from the `agent` uid is limited to `network.allow` (section 13), enforced inside the container with an owner-matched firewall rule, so a prompt-injected agent cannot send it, or the code, elsewhere.
- Agents read issue context from files the daemon writes to `/run/issuedog/<run-id>/`, outside the clone, so the daemon controls which comments an agent sees (section 6) and nothing it writes appears in the diff.

This keeps prompt-injected text from reaching GitHub directly and makes every write retry-safe.

### 3.2 Git handling

The daemon never runs git, or any other repo command, as `issuedog` inside a directory the agent can write. Hooks and config in an agent-writable repo (`pre-push`, `core.sshCommand`, `core.fsmonitor` and so on) would otherwise run with the daemon's privileges.

- The daemon owns a bare clone, `/work/<repo>.git`. It is the only repo the daemon pushes from, and the only place it reads trusted files from (section 3.4).
- For each run the daemon creates `/work/runs/<run-id>` by cloning the bare repo as `agent`. Implementer runs start from the task branch head, or from `main` on the first attempt.
- After an implementer run, the daemon runs `git bundle create` as `agent` for `base..HEAD`, then fetches the bundle as `issuedog` into `refs/issuedog/runs/<run-id>` in the bare repo. A bundle is plain data, so nothing from the agent's repo config or hooks executes.
- All diff inspection (section 10.1, rules 2 to 7) happens in the bare repo. The daemon's own git config defines no diff or filter drivers, so `.gitattributes` in the branch cannot trigger commands.
- Verify commands and the LLM verifier run in fresh `agent`-owned checkouts of the exact head SHA, never in the implementer's clone. Untracked files the implementer left behind (for example `.claude/settings.local.json`) never reach them.
- Task branches are named `issuedog/<issue>-<slug>` and always branch from `main`. A task is blocked until its dependencies merge, so stacked branches are never needed.

### 3.3 Claude invocation

```
claude -p "$(cat /run/issuedog/<run-id>/prompt.md)" \
  --output-format json \
  --json-schema "$(cat schemas/<stage>.json)" \
  --append-system-prompt-file /run/issuedog/<run-id>/system.md \
  --dangerously-skip-permissions \
  --no-session-persistence \
  --max-turns <stages.max_turns.<stage>> \
  [--max-budget-usd <stages.max_budget_usd.<stage>>] \
  [--model <model>] \
  [--effort <level>] \
  [--fallback-model <model>,<model>,...]
```

- The daemon validates the stage's output from the `structured_output` field of the JSON result. The run fails (fail closed) if `subtype` is anything other than `success`, including `error_max_structured_output_retries`, or if `structured_output` is missing or fails the daemon's own validation.
- The daemon writes the full JSON result to `/state/runs/<run-id>.json` before acting on it, for restart recovery (section 8.2).
- Each stage has a timeout from config. On timeout the daemon kills the whole process group, and the run counts as failed.
- `--max-budget-usd` is a client-side estimate and is optional.
- **Model and effort.** The daemon resolves a run's model, effort and fallback models when the run starts, and passes each flag only when it has a value, so an unset one leaves the CLI's default.
  - It starts from the stage's `stages.model`, `stages.effort` and `stages.fallback_model` (section 13).
  - An issue's `model:*` and `effort:*` labels override them for every run on the issue except the verifier's, such as the interviewer and planner on an epic or story and the implementer on a task. `verifier-model:*` and `verifier-effort:*` apply only to verifier runs. A model label names an allowed model by its label suffix (section 13). Labels match ignoring case.
  - Each implementer attempt after the first in a round (8.3) raises the effort one level, up to `stages.max_effort`. A raise never lowers: an effort above the cap, from a stage default or a label, is used as it is. An unset effort stays unset.
  - `--fallback-model` is the stage's list, in order and comma-joined, without the run's own model.
  - Every run checks all four kinds of label, not only the two it uses. Two labels of one kind, or one that names no allowed model or effort level, start no run on the issue at all, and the issue gets one state error, `kind=labels` (5.3).
  - The lease records the resolved settings (8.2), so a label changed during a run affects only the next run, and a restart applies a result with the settings it ran with.
  - The JSON result's `modelUsage` names each model that ran, by full name, such as `claude-haiku-5-5` for `--model haiku`. The run marker records them (section 5).
- **Credential.** The agent gets exactly one Claude credential: `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`, whichever is set in the daemon's environment. If both are set, the daemon refuses to start.
- **Usage limits.** `-p` mode does not report the reset time in a machine-readable form. The daemon matches the result text of a failed run (a non-`success` subtype or `is_error`) against `usage_limit.pattern`, so model text that mentions a limit in a successful run doesn't count. If the text includes a reset time it waits until then. Otherwise it backs off, starting at 15 minutes and doubling to a 2-hour cap. While it waits, steps 1 and 2 of the loop (section 8.1) keep running, so commands such as `/hold` still apply. Usage-limit waits do not count toward the retry cap.

### 3.4 Trusted inputs come from `main`

The daemon reads these from `main` in its bare clone, never from a task branch:

- `CODEOWNERS`
- `.issuedog/config.yaml`, including `verify.commands`
- `.issuedog/prompts/*.md`; a missing file falls back to the default that ships with issuedog
- JSON schemas, which ship with issuedog and are not overridable

The triage agent, interviewer, planner and story verifier run in a fresh checkout of `main`.

The task verifier runs in a fresh checkout of the head SHA, but only after rule 2 of section 10.1 has passed. Every agent-config path (section 6.3) in that checkout therefore matches `main`.

### 3.5 Daemon settings

The daemon's own settings come from its environment, not from the repo:

- `ISSUEDOG_REPO`: the repo, as `owner/name`.
- `ISSUEDOG_APP_ID` and `ISSUEDOG_INSTALLATION_ID`: the GitHub App and its installation on that owner (section 15).
- The Claude credential (section 3.3).

The App's slug is read from `GET /app` at startup, so it can't be misconfigured. The App's comments are those by the bot `<slug>[bot]` (5.1).

Startup checks the Claude credential, the settings and the App key, then reads the slug, mints a token, syncs the bare clone, loads config and trust from `main`, and creates missing labels (section 15). It also checks the stages registered in code: none may run at `intake` while triage is set, since a triage run's lease records `intake` (8.2), so that stage's results would all be dropped. Only then does it apply the agent firewall and start the loop. A missing or malformed setting, or a failed step, stops the daemon with exit code 1 and the reason logged.

`network.allow` and `usage_limit.pattern` (section 13) are read once at startup, so a change to either takes effect on restart. The rest of the config, and trust, are reloaded on every loop iteration (8.1).

## 4. Work item types

| Type | Meaning | Created by |
|---|---|---|
| `epic` | A rough idea or large feature | Owner |
| `story` | One user-visible outcome with acceptance criteria | Planner (epic plan), or owner |
| `task` | One PR, one scope; a human task has no PR (section 9.3) | Planner (epic or story plan), or owner for a small change |
| `bug` | Something broken | Owner, or story verifier |

- Repos owned by an organization use GitHub issue types. The org's defaults are Task, Bug and Feature, so an org owner must create `Epic` and `Story` first. Repos owned by a personal account have no issue types and use `type:epic`, `type:story`, `type:task`, `type:bug` labels instead. Config selects which.
- For now, issuedog sets a type only by label (`types: labels`). On a `types: issue-types` repo the interviewer can't set one: on an untyped issue there, one of its questions asks the owner to set the type on the issue.
- Hierarchy uses native sub-issues: epic → story → task, or epic → task. Sub-issues must share the parent's owner. GitHub allows 100 sub-issues per parent.
- An epic with a single user-facing outcome is planned directly into tasks and acts as its own story, including story verification (section 7.3).
- **Untyped issues.** The owner may file an issue with no type. At `clarifying` the interviewer picks one of the four types and sets it with a question round, a spec, or for a bug or task the `typed` outcome (9.1). The App adds the `type:<type>` label and removes any other `type:*` label. Every comment that sets a type starts with the type and a one-line reason (9.1). If it can't tell yet, it asks questions first and sets the type once the issue is clear. A spec on an untyped issue must set it to epic or story, so an issue with no type doesn't leave `clarifying`.
- **Changing the type.** The interviewer may set or change only a type it set itself: on an untyped issue, or on one each of whose `type:*` labels stands, on the timeline, as the App's add. It runs only at `clarifying`, so that's the only stage it types at. Every other type change is the owner's. The owner can change it at any time, and the daemon follows the owner's type from then on. If the issue's current stage doesn't apply to the new type (section 7.2), the daemon leaves it where it is and reports a state error (5.3).
- **A child is never typed `story`.** The interviewer doesn't run on a story with a parent, and the table has no spec move for one (7.2). So typing an issue with a parent as `story` is invalid interviewer output and a failed attempt (8.3). `issue.md` names the parent, and the interviewer's preamble states this rule (section 12).
- A story filed by the owner has no parent epic. It goes through clarifying, spec review and planning like an epic, then story verification (section 7.3).
- A `bug`, or a `task` filed by the owner, goes through clarifying and then becomes a single task. It skips spec review and planning. Until build phase 3 (section 17), such an issue, whether filed with its type or typed by the interviewer, waits at `clarifying` with no interviewer runs (7.2). A bug filed against earlier work starts its body with `Follow-up to #N` and includes the acceptance criterion "the original behavior of #N is unaffected."

## 5. State

GitHub is the single source of truth. The daemon keeps only an in-memory cache, rebuilt from GitHub on startup and corrected on every poll.

| Concern | Where it lives |
|---|---|
| Stage | `stage:*` label, exactly one per open issue, valid only as described in 5.1 |
| Type | Issue type, or `type:*` label |
| Hierarchy | Native sub-issues |
| Dependencies | Native blocked-by / blocking within one owner, `depends-on` marker across owners (section 14) |
| Priority | `priority:p0` to `priority:p3` (default `p2`) |
| Scope | `scope:<name>` from the repo's allowed list |
| Model and effort | At most one each of `model:<suffix>`, `effort:<level>`, `verifier-model:<suffix>` and `verifier-effort:<level>` (3.3, section 13) |
| Transition history | Bot comments carrying a `transition` marker, e.g. `<!-- issuedog:transition from=clarifying to=spec-review rev=3 body=9f2a61c0 run=8f2c effort=high model=opus ran=claude-opus-5-5 seen=2214587 -->` |
| Attempt count | Hidden marker on the latest bot verifier comment for a task. For other stage runs, the daemon's memory and the lease (8.2, 8.3), so a restart resets every count but the latest run's |
| State errors | Bot comments carrying an `error` marker with a stable key (5.3) |
| Label reverts | Bot comments carrying a `revert` marker keyed by the reverted timeline event, so a replay doesn't comment twice (5.1, 5.2) |
| Processed commands | The bot's 👍 on an applied command comment or 👎 on a rejected one, plus `cmd=<comment id>` in the transition marker or a `reply` marker (9.2). A reaction command (section 9.2) is recorded as `cmd=r<reaction id>` |
| Spec and plan revisions | One revision comment per revision, carrying a `revision` marker with `rev` and the body's hash. A transition posted with a revision, or approving one, carries both too (section 7.4); for plans, see section 16 |
| Triage decisions | Bot comments carrying a `triage` marker: a flag waiting on a code owner, or an issue left to people (6.2) |
| Dirty content | A visible note carrying a `dirty` marker at the top of the comment or body (5.1) |
| Origin of bot-created issues | Hidden `origin` marker in the issue body (section 6.2) |

Every marker is an HTML comment, `<!-- issuedog:<kind> key=value ... -->`, with its fields in a fixed order. The order is a fixed list in code, such as `from`, `to`, `round`, `run`, `event`, `cmd`. Any field not on the list, such as `effort`, `model`, `questions`, `ran`, `recommend` or `seen`, follows them, sorted by name. The kinds so far:

- `transition`: `from`, `to`, and the key that makes it idempotent (5.2). The key is `cmd=<comment id>`, or `cmd=r<reaction id>`, for a command; `run=<run id>` for a stage run's result; or `event=<timeline event id>`. An event key carries `by=<login>` for a stage label a trusted user changed by hand (5.1), and no `by` for a move into the pipeline by a label (6.2). Some transitions carry more:
  - A move to `held` carries `at`, the stage the issue was at, and `reason`: `owner` for `/hold` or a hold set by hand, or `discussion` or `dropped` (6.4). A discussion hold also carries `origin`.
  - A move that posts or approves a revision carries `rev` and `body`, the body's hash (7.4).
  - `seen=<comment id>` is the newest comment read before the move. It bounds the next round's input (9.2). On a stage run's move it is the newest comment on the issue when the run started. On a move by `/revise`, a revision request or `/approve`, it is that comment. On an approval by 👍 it is the newest comment the daemon had read.
  - A stage run's move carries `model` and `effort`, the settings the run started with, each only when set, and `ran`, the full names of the models that ran, comma-separated, from the result's `modelUsage` (3.3). A value a marker can't hold, such as one with a space, is left out and logged.
  - An interviewer question round's move, `clarifying → clarifying`, carries `round=<n>` and `questions=<count>` (9.1), for example `<!-- issuedog:transition from=clarifying to=clarifying round=2 run=8f2c effort=high model=opus questions=3 ran=claude-opus-5-5 seen=2214587 -->`. The interviewer's `typed` move (9.1) is the same move and carries neither.
  - A move into the pipeline is keyed by how the issue got in (6.2): `event=` for `stage:intake` or `approved`, `run=` for triage, `cmd=` for `/track` and `cmd=r` for a 👍 on a flag.
- `revision rev=<n> body=<hash>`: in each revision comment (7.4). Posted without a transition, it also carries `seen`.
- `reply reason=<rejected|edited> cmd=<comment id>`: on the App's reply to a rejected command, or to an edit made after the App used the comment (9.2). It marks the comment as processed. A command the App 👎'd gets no edited reply.
- `error kind=<kind> key=<key>`, and the `ack` reply to it: a state error (5.3).
- `revert key=<event id>`: an untrusted label change the daemon reverted (5.1). For an unexplained label it removed, the key is `<label>@<event id>`: the label in lower case, URL-escaped, and the ID of its latest event, or 0. One comment may carry several.
- `triage verdict=<ask|leave>`: a triage decision on an issue with no stage label (6.2), with one key. The key is `run=<run id>` for a triage run's result, `cmd=<comment id>` for `/ignore`, or `cmd=r<reaction id>` for a 👍 on a flag. A flag, `verdict=ask`, also carries `recommend=<handle|leave>`, its recommendation, for example `<!-- issuedog:triage verdict=ask run=8f2c recommend=handle -->`. The issue's decision is the latest triage marker in its comments. One in dirty content doesn't count.
- `dirty by=<login>`: the note on dirty content (5.1).
- `origin`: in the body of an issue the daemon created (5.2), or reposted in a comment once that body is dirty (6.4).
- `depends-on url=<issue url>`: a dependency across owners (section 14).
- Planned: the attempt marker on the latest verifier comment of a task (8.3), not built yet.

Assignees, and the priority labels on PRs, are not state either: the daemon derives them (section 9.6) and corrects them on every poll. Nor are the `triage:*` labels, which follow the latest triage marker (6.2).

Apart from the App key (section 3.1), the only local files are the lease (section 8.2) and the run results in `/state/runs/`. Both are scratch: losing the container loses nothing except the current run and the count of failed non-task runs toward the attempt cap (8.3).

### 5.1 Reading rules

- **Markers.** The daemon only reads markers in comments and issue bodies written by the issuedog App (3.5). A marker anywhere else is ignored.
  - **Embedded text.** Whenever the App writes a comment or body, it escapes `<!--` as `&lt;!--` in all text it didn't write itself, such as agent output, quoted comments, issue titles and file contents. So the only unescaped markers are the daemon's own.
  - **Dirty content.** A comment or body is dirty for good once anyone other than its author or the App edits it, trusted users included. The daemon never reads markers in dirty content, and it never counts as a command, an answer or an approval, however it is edited later.
    - The daemon finds such edits in the GraphQL `userContentEdits` history. It reads the history only for content whose last edit was by its author or the App, since someone else may have edited it before them. A history that doesn't reach the last edit counts as dirty.
    - GitHub keeps only the original post and the 99 newest edits (section 16). So the App puts a visible note at the top of dirty content, carrying `<!-- issuedog:dirty by=<login> -->`, and keeps the rest of the text, escaped. The note keeps the content dirty after the history drops the edit. A note that is removed, or names someone else, is put back.
  - **Edits and deletions by untrusted users.** When an untrusted write collaborator edits trusted content, or deletes a comment, section 6.4 says what happens.
- **Labels.** A `stage:*`, `priority:*`, `scope:*`, `type:*` or `approved` label, or a `model:*`, `effort:*`, `verifier-model:*` or `verifier-effort:*` label (3.3), counts only if the issue timeline shows that its most recent `labeled` event came from the App or a trusted user (section 6.1). On org repos the same applies to the issue type and its most recent type event.
  - When an untrusted actor adds or removes one of these labels, the daemon posts a short comment carrying a `revert` marker, then reverts the change.
  - **Unexplained labels.** A pipeline label on the issue that no standing `labeled` event explains is unexplained. A repo label rename makes one, since it adds no event to the issues that carry the label. GitHub also writes label events a second or two late, so the daemon holds such an issue back, acting on nothing of it, for up to 30 seconds from when it first finds one. After that it carries on with the unexplained labels left out of everything it reads and acts on, and reads the issue again when one falls due for removal. A label falls due once it has been unexplained for 5 minutes, or, once the issue has had unexplained labels for 5 minutes, once the label has been there for 30 seconds. A change to the label's events starts its time again. The daemon then posts a comment carrying a `revert` marker and removes the label.
    - A stage label that names the latest transition marker's stage is never unexplained.
    - When the daemon sets a label that's on the issue but unexplained, it removes the label and adds it again, so its own event explains it.
    - A rename can't name who made it, so a trusted rename of a pipeline label is removed too. The owner adds the new name back by hand.
  - When a trusted user changes a stage label by hand, the daemon treats it as an owner override and records a transition marker with `by=<login>` and `event=<timeline event id>`. It does this even if the change is not in the transition table. Setting `stage:held` by hand is an owner hold: its marker also carries `at=<previous stage>` and `reason=owner`. A change to the stage the issue is already at, `held` included, writes no marker, since holds stack (6.4).
- **Disagreement.** If the stage label and the latest transition marker disagree and 5.1 does not explain the difference, the daemon reports a state error (5.3), then puts the label back to match the marker.
- **Unfinished writes.** If no stage label has changed since the latest transition marker, other than changes toward it, the disagreement is the App's own write cut short (5.2). The daemon finishes it by setting the labels to match the marker. That isn't a state error.

### 5.2 Idempotency

Every transition goes through one table in code (section 7). Each write is checked first, so a crashed run can be replayed safely:

- **Transitions.** A transition writes in this order: the comment with its marker, the new stage label, removal of any other stage label, then the 👍 for a command comment. A marker with the same key (`cmd`, `run` or `event`) means the transition is written, so a replay only finishes the labels and the 👍. A crash between the marker and the labels leaves an unfinished write, which the next poll finishes (5.1).
- **Commands.** For a command comment, the daemon writes the transition marker with `cmd=<comment id>`, then adds its 👍 reaction. GitHub has no ✅ reaction. On replay, a marker carrying that `cmd` means the command is applied, and only the labels and the 👍 may be missing. `/priority`, `/model` and `/effort` write no marker, so the 👍 is the record. A rejected command gets the App's 👎 first, then the reply (9.2). For a reaction command, the marker carries `cmd=r<reaction id>`. A reaction can't be reacted to, so the transition comment is the acknowledgment, or for a 👍 on a state-error comment, the ack reply (5.3).
- **Created issues.** Every issue the daemon creates has an `origin` marker with a stable item ID, such as `plan=#12 rev=3 item=T4`. Before creating an issue, the daemon searches for a marker with the same plan and item, whatever its `rev`, and reuses any match. A planner revising a plan keeps the item ID of every item it keeps or changes, gives new items new IDs, and marks each item in the plan comment as kept, changed, new or dropped. The same applies to sub-issue links and dependency links: the daemon reads the current links before adding any.

### 5.3 State errors

A state error is something the daemon reports to the owner instead of acting on: a transition missing from the table (7.2) other than a rejected command, a stage label that disagreed with its marker (5.1), a type that doesn't fit the issue's stage (4), a stage run that failed `loop.max_attempts` times in a row (8.3), or model and effort labels that give a run no settings (3.3).

- The daemon posts one comment per error, carrying `<!-- issuedog:error kind=<transition|disagreement|type|attempts|labels> key=<key> -->`. The key is the ID of what triggered it: the comment, reaction or timeline event (such as a `labeled` or issue-type event). For a stage run, the key names the stage and its round of input (8.3), so the stage doesn't run again until the issue moves or gets new input. A run's missing transition also keys on the issue's type and parent. For model and effort labels, the key is the timeline event of the newest change to any of the four kinds that stands (5.1): the App's or a trusted user's, but not an untrusted change or the App's revert of one. So the same bad labels set again after a fix are reported again, and an untrusted change posts nothing. Before posting, the daemon looks for a comment with that key and kind and reuses it, so the same cause is posted once across polls and restarts, and a new occurrence gets a new key.
- For a disagreement, the comment is posted before the label is put back, so a crash between the two can't lose the report.
- For model and effort labels, the daemon reads the issue again first. It posts nothing if the issue closed, changed stage, has no stage work pending, or has other model and effort labels than it had in step 1 (8.1), since the change may fix them.
- A trusted 👍 on the comment acknowledges the error. The daemon records it by replying with `<!-- issuedog:error key=<key> ack=r<reaction id> -->`, never posts that key again, and the error stops counting toward assignment (9.6). Removing the 👍 later changes nothing.
- Otherwise the error ends when its cause is gone. A type mismatch ends when the type fits the stage again. A missing transition or a disagreement ends at the issue's next valid transition, including a trusted user's manual stage change (5.1). A stage run's error ends when the issue moves or gets new input. A labels error ends at the next change to the labels that stands; if they still give no settings, that is a new occurrence.
- A command that doesn't apply at the issue's stage isn't a state error. The rejection reply (9.2) is enough, and nobody is assigned for it.

## 6. Trust model

### 6.1 Trusted people

A GitHub user is trusted when either holds:

1. They are an owner of the CODEOWNERS file itself, under the CODEOWNERS rules as read from `main` (section 3.4). These are the people who already control trust.
   - GitHub reads the first file it finds, in this order: `.github/CODEOWNERS`, `CODEOWNERS`, `docs/CODEOWNERS`. The daemon uses the same order.
   - Team entries (`@org/team`) are resolved through the API. Email entries are ignored and logged.
   - GitHub ignores code owners without write access, and so does the daemon.
2. No CODEOWNERS file exists on `main`, and the user's `role_name` is `write`, `maintain` or `admin` (`GET /repos/{owner}/{repo}/collaborators/{user}/permission`).

The issuedog App, and any other bot account, is never trusted.

### 6.2 What the daemon acts on

The App creates an issue in the pipeline when its `origin` marker points to an approved plan revision, or to a story-verifier run. Issues the App creates in another repo (section 14) carry no such marker, so they need `approved`.

Every other issue enters by one move, `intake → clarifying`, on the "issue is trusted" row (7.2). The ways in, with the key the move's marker carries (section 5):

- It has `stage:intake`, set by a trusted user or the App, and a trusted author. The key is `event=<the labeled event of stage:intake that stands>`.
- A trusted user added `approved` (checked on the timeline), and the issue is at `stage:intake` or has no stage label. The key is `event=<that labeled event>`.
- A triage run admits it (below). The key is `run=<run id>`.
- A trusted user posts `/track` at intake (`cmd=<comment id>`), or reacts 👍 to a flag that recommends handling it (`cmd=r<reaction id>`).

If the event already keys a transition, as when an override brought the issue back to intake, the move is keyed `event=<the ID of the comment holding that transition>` instead. Every move into the pipeline says in its comment why the issue is in. For a bug or a task, and a story with a parent, it also says that the issue waits at `clarifying` (7.2).

An untrusted author's issue enters only by `approved`, `/track` or a trusted 👍 on a flag. Each of these is the approval. A trusted user's `stage:intake` alone doesn't let it in.

An issue the table can't move, such as an approved issue of a type the pipeline doesn't know, stays at intake and gets one `kind=transition` state error (5.3), keyed by the admitting event, comment or reaction. Triage doesn't run on it while that error stands. Only these move it on: a trusted 👍 on the error, which lets triage run again, a stage label, `approved` or `/track`.

**Triage.** An open issue with no stage label (none left out, 5.1), no transition marker, no triage marker that counts, no `approved` label and no standing admission error is classified once by a short triage run. That covers the issues already open when the daemon first runs on a repo, and each new one. Closed issues and pull requests aren't triaged, and an issue with a stage label is adopted by the label rules (5.1) instead.

- **When.** Triage runs only in a pass where no other stage ran, on the newest such issue first, one per pass (8.1). Like every stage run, it runs under the usage gate and the attempt cap (8.3). It uses the interviewer's `stages` settings (section 13).
- **Output.** `verdict` (`handle` or `leave`), `sure` (true or false), `reason` and `summary`, each one line. The daemon decides:
  - A sure `handle` on a trusted author's issue admits it, keyed `run=<run id>`.
  - A sure `leave` records "left": a short comment with the reason, saying that a code owner can `/track` it, carrying `<!-- issuedog:triage verdict=leave run=<run id> -->`. The issue isn't classified again.
  - Anything else posts a flag, with the verdict as the recommendation. That covers every verdict that isn't sure, and any `handle` on an untrusted author's issue, which triage never admits.
- **A flag** is one App comment in the 9.1 style, carrying `<!-- issuedog:triage verdict=ask run=<run id> recommend=<verdict> -->`:

  ```
  ### Should issuedog handle #N?

  **1. <summary>** Blocks: #N
     a. Handle it (recommended): <reason>
     b. Leave it

  React 👍 to take the recommendation, or post `/track` or `/ignore`.

  cc @<notify>
  ```

  With a `leave` recommendation, "Leave it" is option a.
- **During the run.** If a trusted user adds `approved`, or a code owner's answer records a triage marker, the result writes nothing, and the run isn't counted. A triage marker in dirty content doesn't count, so that issue is classified again.

**Answering a flag.** Each answer must come from a trusted user, in a clean comment or a 👍:

- The earliest trusted 👍 on the latest flag takes its recommendation, keyed `cmd=r<reaction id>`. A `handle` admits the issue, an untrusted author's too. A `leave` records "left" with `<!-- issuedog:triage verdict=leave cmd=r<reaction id> -->`, unless the issue is in the pipeline or has `approved`. With `approved`, no "left" comment is written, and the move into the pipeline answers the flag.
- A 👍 on an older flag, an untrusted 👍 and the App's 👍 are ignored. So are 👍s on a flag a later move answered, as when an override brought the issue back to intake, or after a hold and `/resume`. Only `/track` or `approved` admit such an issue.
- `/track` admits the issue at intake, keyed `cmd=<comment id>`, whether it is flagged, left or never triaged.
- `/ignore` records "left", keyed `cmd=<comment id>`. It's rejected on an issue in the pipeline, meaning one with a stage label, a left-out one included, or a transition marker, and on one already left. On an issue with `approved` it's rejected too, since `approved` brings the issue into the pipeline instead.

**The `triage:*` labels.** `triage:ask` and `triage:left` show the two waiting states in GitHub's filters. They follow the latest triage marker: `triage:ask` for a flag, `triage:left` for "left", and neither once the issue is in the pipeline. The App sets and removes them, and on its next visit undoes anyone else's change, with no comment. The daemon never reads them as state, so the label rules (5.1) don't guard them.

**Admission time.** An issue's admission time is the earliest of:

- the earliest `labeled` event of `approved` by a trusted user, or of a stage label past intake, other than `held`, by a trusted user or the App;
- the time of the clean, trusted `/track` comment, or the trusted 👍 on a clean flag, that a clean App move out of intake names in its `cmd`.

Dirtying or deleting the move or the command falls back to the label's time. With no admission found, every edit counts as after it. Times are to the second, so a change in the admission's second counts as after it.

**Edits after admission.** An approval approves what the approver read, not what the author writes later. On an untrusted author's issue, a title or body the author changed after the admission is untrusted input for the interviewer: it goes in `untrusted.md` under a heading that says so, and `issue.md` says it's there instead of showing it.

- The body counts as changed after admission when its last edit is at or after the admission, by anyone but the App. GitHub dates only the last edit, so any later edit counts.
- The title counts as changed after admission when its latest rename is at or after the admission and by someone untrusted. A trusted user's later rename makes the title theirs.

**Untrusted input.** Only comments and reactions by trusted users count as answers, approvals or commands. A comment a trusted user approved (6.4) counts as their input, never as a command. Agents receive other text only in a separate `untrusted.md` context file (section 12). Each piece is fenced under a heading that says it is untrusted data, not instructions. That file holds:

- comments by untrusted users and other bots;
- dirty comments by anyone, and a dirty body (5.1);
- for the interviewer, an untrusted author's title or body changed after admission;
- for triage, an untrusted author's whole title and body.

The interviewer gets `untrusted.md` only on epics and stories. On an untyped issue, a bug or a task, that text is in no file. Triage is the exception: a triage run gets `untrusted.md` on any issue, so it can classify an untrusted author's issue. Its result can only flag such an issue or leave it, never admit it.

A command counts once. If its comment is edited after the daemon applied it (its 👍), the edit is ignored and the bot replies asking for a new comment, except as 9.2 says for answers: an applied command that wrote no marker, such as `/priority`, edited into plain text at `clarifying`, in the current round, becomes an answer, with no reply. A rejected command (its 👎) gets no new reply when edited. If it is edited before the daemon processes it, the daemon reads the latest text. Section 9.2 has the details, including edited answers.

### 6.3 Protecting the trust inputs

- CODEOWNERS lists itself (`/.github/CODEOWNERS @owner`).
- A ruleset requires code-owner review on `main` (section 15).
- Protected paths are:
  - CODEOWNERS in any of its three locations
  - `.github/**`
  - `.issuedog/**`
  - the agent-config paths: `.claude/**`, `**/CLAUDE.md`, `**/CLAUDE.local.md` and `.mcp.json`
  - anything in `verify.protected_paths`

  Any diff that touches one fails deterministic verification (section 10.1) and needs a human-authored change instead. The owner merges those through the admin bypass in section 15.

### 6.4 Discussion holds

Write collaborators who aren't trusted users (6.1) can take part in an issue, but they can't move it. Any of these on an issue in the pipeline (6.2) puts that issue, and only that issue, on hold:

- a new comment by such a user
- their edit to a comment or body written by the App or a trusted user, seen in its GraphQL `userContentEdits`, or to their own comment after it was approved
- their deletion of such a comment, seen as a `CommentDeletedEvent` on the issue timeline
- their renaming of the issue

Closes and reopens are separate. Any close by someone who isn't trusted, the App, or a merged PR is reverted at once: the daemon reopens the issue, so a parent never closes on an untrusted close (7.3). The close holds the issue only if the closer is a write collaborator. An untrusted reopen of a closed issue is reverted too, and holds nothing.

Comments from people without write access hold nothing. They reach agents only as untrusted input (6.2).

**While held.**
- The daemon sets `stage:held` with a transition marker carrying `reason=discussion` and two stages: `at`, where the issue was, and `origin`, where it was before the first discussion hold. Every marker written in the `clarifying` that follows a hold, including question rounds and any later hold of either kind, carries `origin` forward, so back-and-forth discussion never loses it. `/resume` restores `at`. "Nothing changed" returns to `origin`. For anything other than a comment it also posts a notice naming it, since there is nothing else to react to.
- Holds stack. Any hold on an issue that is already held, whether by `/hold`, by hand (5.1) or for discussion, keeps the stage recorded by the first hold. The issue leaves `held` only when every hold on it has ended. A discussion hold ends when resolved, or by `/resume` on the issue itself. An owner hold ends by `/resume` on the issue itself, on the issue where its `/hold` was posted, or on any issue between the two. A hold set by hand ends by `/resume` on the issue itself. Every hold on the issue also ends at once when a trusted user sets another stage label by hand, which 5.1 records as an override. Removing `stage:held` alone is a disagreement (5.3) and is put back. If a discussion hold ended while an owner hold stayed, the final `/resume` sends the issue to `clarifying` as below.
- Content edited by an untrusted write collaborator is dirty (5.1), so it never counts as a command or an answer, even after the edit is accepted. In loop step 1 (8.1) the daemon looks for triggers before it processes commands.
- Before applying any run result, the daemon checks the issue for triggers since the last poll before the run was picked up. If it finds one, it holds the issue and drops the result.
- Comments and pushes made while the issue is held are handled once it leaves `held`, in the stage it lands in. A PR review made while held is handled when the task next reaches `pr-open`, through the usual `pr-open` rows (7.2).
- Any run for the issue is stopped and its result dropped. Nothing is pushed, no PR is opened or updated, and the issue makes no other transition until the hold ends. Other issues, including its parent and children, carry on.
- The digest lists the issue (9.4).

**Resolving.** Every trigger needs a trusted user's response:
- A 👍 on an untrusted comment approves it. From then on it counts as that user's input (6.2).
- A trusted comment counters every untrusted comment posted before it, and is itself owner input. Commands in it apply as usual, except that any that would move the issue wait. This covers `/approve`, and a 👍 on a revision comment made while held, but not `/resume`, which applies at once. A deferred `/revise` that the interviewer already handled in the post-hold `clarifying` isn't applied again: the interviewer's transition marker records it with `cmd=` (5.2). Such a command applies when the issue next reaches a stage it fits, if the revision it names still matches its hash (7.4). Otherwise it gets a rejection reply (9.2).
- A 👍 on a notice accepts the change. Markers in edited or deleted content are never read again (5.1). When that content was an App-written body, the daemon reposts its `origin` marker in a new App comment, from the App's own last version in the edit history, so the issue stays in the pipeline (6.2) and de-duplication still finds it (5.2). GitHub keeps only the original post and the 99 newest edits (section 16), so if that version is gone, the daemon takes the marker from the original post, which the App wrote when it created the issue.

**Leaving the hold.**
- Once every trigger is resolved, the issue goes to `clarifying`, and its transition marker carries the stage before the hold. The interviewer reads the discussion, with approved and countering comments as owner input, and picks the next stage. Comments it handled there don't count again as revision requests (7.4):
  - Nothing in the discussion changes the issue: `origin`. A task whose run was dropped goes to `ready`.
  - The spec changes: the interviewer rewrites it, and the issue goes to `spec-review` for a new 👍 (7.4).
  - The plan changes, or a child story's body: `planning`, then `plan-review` for a new 👍, even when `plan.review` is false. For an epic or story that was already `planned`, the approved revision updates its children rather than starting over:
    - **Kept** items keep their issues and stages (5.2).
    - **Changed** items keep their issues, which the daemon rewrites with the new body and `rev`, and any run is dropped. A changed task goes to `ready`, or `needs-human` for a human task. A changed story goes to `planning`, where its own children are updated the same way. A changed item that is held stays held: the stage it would go to becomes its hold's `at` and `origin`. A closed item can't be marked changed (11.3).
    - **New** items become new children.
    - **Dropped** items, and every open issue below them, are closed as not planned, deepest first, with a comment naming the revision. Closing ends any hold, and the daemon removes dependency links to them, so kept children aren't left blocked. A story closed this way skips story verification (7.3). The one exception is an issue with an open PR: it is held with `reason=dropped`, any run for it is stopped, and it is listed for the owner (9.4, 9.6). `/resume` doesn't end this hold. Once its PR merges or closes, or the owner closes the issue, it closes as well, and so do its now-empty ancestors in the dropped subtree.
  - A task's body changes: the interviewer rewrites it in its own template (11.2, or 9.3 for a human task), and the task goes to `ready`, or back to `needs-human` for a human task.
  - More answers are needed: a question round, staying at `clarifying`.
- Each of these writes a fresh transition marker, so an edited or deleted marker can't roll state back.
- A trusted `/resume` on the issue itself ends the discussion hold at once. It accepts every open trigger, including triggers whose notice was deleted, and restores `at`, except that a task whose run was dropped goes to `ready`. A `/resume` on an ancestor doesn't end a discussion hold.
- A restart changes nothing, because the hold marker and its triggers are all on GitHub, and the lease of a held issue is dropped (8.2).

## 7. Pipeline

### 7.1 Stages

| Label | Agent | Output |
|---|---|---|
| `stage:intake` | none | Owner's rough idea |
| `stage:clarifying` | interviewer | Numbered questions (section 9.1) |
| `stage:spec-review` | interviewer | Spec written into the issue body (section 11.1) |
| `stage:planning` | planner | Proposed stories or tasks in one comment |
| `stage:plan-review` | none | Waiting for owner approval of the plan |
| `stage:planned` | story verifier when children close | Parent of created sub-issues |
| `stage:ready` | none | Task waiting for the scheduler |
| `stage:in-progress` | implementer | Commits in the run clone |
| `stage:verifying` | verifier | Pass, or findings |
| `stage:pr-open` | none | PR waiting for human review |
| `stage:needs-human` | none | Task only the owner can do (section 9.3) |
| `stage:held` | none | Paused by the owner, or held for discussion (6.4) |

The lifecycle of each type:

- **Epic.** intake → clarifying → spec-review → planning → plan-review → planned → closed.
- **Story.** Created at `stage:planning` from an approved epic plan, which already carries its acceptance criteria. Then planning → plan-review (or straight to planned, see `plan.review`) → planned → closed.
- **Story filed by the owner.** intake → clarifying → spec-review → planning, then as a story.
- **Task.** A task from the planner is created at `stage:ready`, or at `stage:needs-human` for a human task (section 11.3). Then ready → in-progress → verifying → pr-open → closed, with loops back through clarifying, in-progress or needs-human. A human task from the planner goes needs-human → closed.
- **Bug, or task filed by the owner.** intake → clarifying → ready, then as a task. Until build phase 3 it waits at clarifying (7.2).

### 7.2 Transitions

| From | To | Applies to | Trigger |
|---|---|---|---|
| intake | clarifying | all | Issue is trusted (6.2) |
| clarifying | clarifying | all | The interviewer asks a question round: the first on entering clarifying, or the next once the owner answered (9.1). Also the interviewer's `typed` outcome, which types an untyped issue as a bug or task (9.1) |
| clarifying | spec-review | epic, or story with no parent | No open questions; spec written into the body |
| clarifying | ready | bug, or task filed by the owner | No open questions; daemon rewrites the body as a task (11.2). Not built until phase 3: until then the issue waits at clarifying with no interviewer runs |
| clarifying | ready | task | Owner answered the implementer's or verifier's question |
| spec-review | clarifying | epic, or story with no parent | Owner `/revise <text>`, or any other trusted comment (7.4) |
| spec-review | planning | epic, or story with no parent | Owner approves the current revision with 👍 or `/approve` (7.4) |
| planning | plan-review | epic, story | Planner output passes validation (11.3) |
| planning | planned | story | Output passes validation and `plan.review` is false, unless the planning follows a discussion hold (6.4); daemon creates tasks |
| plan-review | planning | epic, story | Owner `/revise <text>`, or any other trusted comment (7.4) |
| plan-review | planned | epic, story | Owner approves the current revision with 👍 or `/approve` (7.4); daemon creates sub-issues, human tasks and dependencies, or after a discussion hold updates them (6.4) |
| any open | ready | task | The parent's newly approved revision changed it (6.4); any run is dropped |
| any open | needs-human | task | The parent's newly approved revision changed this human task (6.4) |
| any open | planning | story | The parent's newly approved revision changed it (6.4) |
| any open | held | all | Dropped from the parent's newly approved revision while it has an open PR (6.4, `reason=dropped`) |
| planned | closed | story, or epic planned into tasks | Every child closed and story verification passes (7.3) |
| planned | closed | epic planned into stories | Every child closed |
| ready | in-progress | task | Scheduler picks the task (8.1) |
| in-progress | clarifying | task | Implementer needs an answer; worker is released |
| in-progress | verifying | task | Implementer exits with commits and valid output |
| in-progress | needs-human | task | Implementer reports the work needs an owner action |
| verifying | in-progress | task | Findings, attempt < cap |
| verifying | needs-human | task | Findings, attempt = cap |
| verifying | clarifying | task | Verifier reports an acceptance criterion is ambiguous |
| verifying | pr-open | task | Pass; daemon pushes, opens or updates the PR, posts the `issuedog/verify` check |
| verifying | pr-open | task | The verified head was pushed by a person: daemon posts the check (success or failure) and any findings as a comment |
| pr-open | in-progress | task | A trusted reviewer submits a review with comments, whether its state is "request changes" or "comment"; or `main` no longer merges cleanly with the branch. An approving review with no comments does not count |
| pr-open | verifying | task | A new head SHA that the daemon did not push |
| pr-open | pr-open | task | `main` moved; daemon re-runs section 10.1 on the merge result and updates the check |
| pr-open | closed | task | PR merged (`Closes #N`) |
| pr-open | needs-human | task | PR closed without merging; daemon asks the owner to reopen it, `/done` to start over, or close the issue |
| needs-human | ready | task | Owner `/done`, or a 👍 on the latest hand-off comment (9.3), on a task that was waiting on the owner; attempt count resets to 0 |
| needs-human | closed | task | Owner `/done`, or 👍 on the issue, for a human task from the planner, and its **Done when** check passes when it can be checked automatically (9.3) |
| any open | held | all | Owner `/hold` on the issue or an ancestor |
| any open | held | all | An untrusted write collaborator comments, edits or deletes trusted content, renames the issue or closes it (6.4) |
| held | clarifying | all | Every trigger of a discussion hold is resolved and no owner `/hold` remains (6.4); the marker carries `origin` |
| clarifying | previous (`origin`) | all | The interviewer finds the discussion changed nothing (6.4); a task whose run was dropped goes to ready |
| clarifying | spec-review | epic, or story with no parent | The interviewer rewrote the spec after a discussion hold (6.4) |
| clarifying | planning | epic, story | The plan changes after a discussion hold (6.4) |
| clarifying | ready | task, bug | The interviewer rewrote the task body after a discussion hold (6.4) |
| clarifying | needs-human | task | The interviewer rewrote a human task's body after a discussion hold (6.4) |
| held | previous (`at`) | all | Owner `/resume` that ends the owner hold (6.4), when no other hold remains and no discussion hold ended meanwhile; a task whose run a discussion hold dropped goes to ready instead |
| held | clarifying | all | Owner `/resume` that ends the last hold, after a discussion hold ended while the owner hold stayed (6.4) |
| any open | closed | all | A trusted user, the App or a merged PR closed the issue; daemon stops any run for it. An untrusted close is reverted (6.4) |
| closed | held | all | A trusted user reopened the issue. An untrusted reopen is reverted (6.4) |

From `ready` on, a `bug` follows the rows for `task`.

Until build phase 3, the interviewer runs only on an epic, a story with no parent and an untyped issue (9.1). A bug or a task, whether filed so or typed so by the interviewer, waits at `clarifying` with no more runs until the implementer stage adds the 11.2 rewrite. So does a story with a parent. Every move into the pipeline says so (6.2).

Any transition missing from this table is an error, except a trusted user's manual label change (5.1) or a rejected command (9.2). The daemon logs the error, leaves the issue unchanged and reports a state error (5.3).

`/hold` applies `stage:held` to every open descendant of the issue, then to the issue, each with its own transition marker, then 👍s the command. An issue that is already held gets no new marker (6.4). `/resume` ends that owner hold on each one, restoring the stage named in its marker unless another hold remains (6.4). A `/resume` that would end no hold is rejected, naming the issues where `/resume` would.

### 7.3 Story and epic completion

- When every child of a story, or of an epic planned directly into tasks, is closed, the daemon runs story verification on `main` (section 10.3), unless the story itself was dropped from its parent's plan (6.4). The verifier sees each child's close reason, so tasks closed as not planned are not mistaken for delivered work.
  - Failures create `bug` sub-issues under the story.
  - Passing closes the story.
- An epic planned into stories closes when every child is closed.

### 7.4 Approvals bind to a revision

- **Revision numbers.** Each time the daemon writes a spec or plan, it increments `rev` and posts a revision comment naming the revision. For a spec, the comment carries `<!-- issuedog:revision rev=<n> body=<hash> -->`. For a plan, the plan comment is the revision comment (section 16).
  - The hash is the first 8 hex digits of the SHA-256 of the body, with CRLF and CR line endings normalised to LF. Nothing else is normalised, so markers and a dirty note (5.1) are part of it.
  - A revision posted with a transition goes in the transition's comment, and the transition marker carries `rev` and `body` too. It is posted even when the hash is unchanged, since an answered question is a new revision and 👍s on the earlier one mustn't approve it. A revision posted without a transition, such as after a body edit, is skipped when the latest revision already has the body's hash.
  - The latest revision is the last revision comment that counts. A dirty revision comment (5.1) is no revision. A dirty App comment after the latest revision may hide a newer one, so nothing approves until the daemon posts a clean revision after it.
- **Approving.** A trusted 👍 on the latest revision comment, or a trusted `/approve` posted after it, approves that revision, and only while the current body or plan still matches the recorded hash and the body isn't dirty. The approval's transition marker carries `rev`, `body` and the approving `cmd`. With several trusted 👍s, the earliest is recorded.
  - 👍s on earlier revision comments, and 👍s from untrusted users or from the App, are ignored.
  - Removing a 👍 after the daemon applied the approval changes nothing.
  - A rejected `/approve` gets a reply with the reason (9.2), such as a hash that no longer matches.
- **Revising.** In `spec-review` or `plan-review`, any trusted comment that is not a command (9.2) and isn't dirty, posted after the latest revision comment and after the issue entered the stage, is a revision request with the comment as the notes. "After the revision comment" means after the newest comment the App had read when it posted it, its `seen`, if it has one. A revision request is handled exactly like `/revise`: applied with a 👍, rejected with a reason, and replied to if edited after it was applied (5.2, 9.2).
  - A comment beats a pending 👍, since comments are handled before reactions.
  - A question counts as a revision request: the agent answers it and posts the next revision, even if nothing else changed.
  - A comment posted after the approval was applied goes to the next stage as owner input. The transition's `seen` (section 5) decides which comments come after it, so one posted in the same poll isn't lost.
- **The body is edited.** The hash no longer matches. After an edit by the body's author, if trusted, or by the App, the daemon increments `rev` and posts a new revision comment with the new hash (for plans, see section 16), which voids any pending approval. An edit by anyone else makes the body dirty (5.1): it gets no revision, nothing can approve it, and a move that would have posted one says why there is none. An untrusted author's own edit gets no revision either, and the mismatch keeps approval blocked.
  - **Leaving `spec-review` with a dirty body.** The interviewer's spec keeps the dirty note at the top of the body (11.1), so the spec goes to `spec-review` with no revision to approve. A body stays dirty for good, so a `/revise` or a revision request sends it back to `clarifying`, but the next spec has no revision either. The way out is an override: a trusted user sets the next stage label by hand (5.1).
- **PR approval.** This relies on GitHub. The ruleset dismisses stale approvals when new commits are pushed, and `issuedog/verify` is posted for an exact head SHA.

## 8. Scheduling

### 8.1 Picking work

Each loop iteration:

1. Process new trusted comments, reactions and label changes on open issues: answers, `/approve`, `/revise`, `/done`, `/hold`, `/resume`, `/priority`, `/model`, `/effort`, `/track`, `/ignore`, 👍 on revision comments, triage flags (6.2), hand-off comments, state-error comments, human-task issues, untrusted comments and hold notices (6.4), and other comments in review stages (7.4). Revert untrusted label changes, record overrides and note dirty content first (5.1). Then move admitted issues from intake to clarifying (6.2), and bring the `triage:*` labels in line. An issue with `triage:ask` is visited on every pass, since a 👍 on its flag doesn't change the issue's `updated_at`.
2. Reconcile PRs: merges, closes, reviews with comments (7.2), new head SHAs, and `main` moving under open PRs.
3. Run any pending non-task stage work for the highest-priority issue that has it: clarifying, spec, planning, story verification. If no stage ran, run triage (6.2) on the newest open issue that needs it.
4. Otherwise pick the next task:
   - It is `stage:ready`.
   - It is not blocked, judged by advanced search `is:blocked` plus any open `depends-on` markers. Search results lag behind dependency writes, so the daemon confirms a candidate through the issue dependencies API before picking it.
   - Ties are ordered by priority, then the parent's priority, then issue number.
5. If nothing is available, sleep `idle_sleep` (default 60s).

- **One task at a time.** Only one task is `stage:in-progress` at a time. A task waiting on the owner releases the worker, and the scheduler moves on to the next independent task.
- **Polling cost.** Polling uses conditional requests (ETags) and `since=` filters, so unchanged resources don't count against the rate limit.
- **Reloading.** Each iteration first syncs the bare clone and reloads config and trust from `main` (3.4, 3.5). If that fails, the last good ones stay and the failure is logged. A config that allows a new model gets the model's labels created (section 15), tried again each iteration until that succeeds.

### 8.2 Leases and restart

- **On pickup.** The daemon writes `/state/lease.json` (write to a temp file, then rename) with the issue number, stage, run ID, clone path, start time, and the time of the last poll before pickup (6.4). It also records what the run read: the newest comment on the issue (the run's `seen`, section 5), the last edit time of each input and of the body (`body_edit`), and the run's resolved model, effort and fallback models (3.3). A triage run's lease records the stage `intake`, which is what the daemon reads for an issue with no stage label. For a task, the `stage:in-progress` label and the transition marker show the lease on GitHub. A non-task run doesn't move its issue, so the lease's stage is the one a reset returns to.
- **Attempts.** The lease names the round of attempts the run belongs to and its number in it (8.3). When a run ends, failed or not counted, the lease is first marked ended with the count of failed attempts in a row it leaves; then the run's files are deleted, and the lease too if that count is 0. A restart that finds an ended lease only takes its count and deletes what's left. The lease holds only the latest run, so every other count, such as another issue's, is held in memory and resets on any restart.
- **On apply.** The daemon reads the issue again and applies the result through its `run=<run id>` key (5.2). It drops the result, writing nothing and counting no attempt, when the issue is closed or gone, even if the run's move is written already. Unless the move is written already, it also drops the result in these cases, and every stage follows the same rules:
  - The issue's stage changed during the run, since the owner's change wins. A triage result is also dropped once the issue has a stage label or a transition marker.
  - An input was edited or deleted since the run read it, or the body has a later edit by anyone but the App. The stage runs again on what's there. The App's own edit, such as one a result applied part way made before a restart, doesn't drop it.
  - The issue has new trusted input since the run's `seen`: a comment above it by a trusted user that isn't a command and isn't dirty. The stage runs again with that comment as input. Without this, an answer step 1 👍'd before a delayed apply would be lost.
- **Checks.** A stage may check its result past what the schema can say, on the issue read again, before anything is written. The interviewer and triage do (9.1, 6.2). A broken rule is a failed attempt (8.3), with nothing written. A result whose move is written already isn't checked again. A GitHub read that fails during the check says nothing about the result, so it isn't a failed attempt: it is a failure to apply.
- **Failing to apply.** A result that fails to apply keeps its lease, and the next pass tries again. After `loop.max_attempts` failures, the run is a failed attempt (8.3). While the issue has unexplained labels held back (5.1), the result waits with its lease, uncounted, and no other run starts; once they're left out, it applies without them. The read before a run and the attempt cap's check leave them out too.
- **On startup.** The daemon reads the lease.
  - First it checks the issue for discussion triggers since the last poll before the run was picked up (6.4). If the issue is held or closed, it deletes the clone and leaves the issue as it is. A valid saved result is dropped. Without one, the run counts as a failed attempt, unless it hit a usage limit or a marker shows the result was applied. The lease is deleted, unless it holds a count of failed attempts in a row (see Attempts): then it stays, marked ended, with that count.
  - If `/state/runs/<run-id>.json` exists and validates, and for an implementer run the clone also has commits ahead of its base, the daemon applies the transition.
  - Otherwise, unless a marker shows the result was applied before the restart, the run counts as a failed attempt (8.3). The daemon deletes the clone, resets the issue to its pre-run stage and starts the run again.

### 8.3 Retries

- The verify loop allows `max_attempts` (default 3) implementer runs per round. A round starts when a task becomes `ready`, or when a review with comments sends it back (7.2).
- Each retry prompt includes the previous verifier findings verbatim, and each retry runs one effort level higher, up to `stages.max_effort` (3.3).
- After the last failed attempt, the task moves to `stage:needs-human` with the findings attached.
- **Other stage runs.** A failed triage, interviewer, planner or story-verifier run is tried again, up to `loop.max_attempts` (default 3) runs in a row per round. A round starts when the issue moves, or when it gets new input. After the last failed attempt, the issue gets one state error, `kind=attempts` (5.3), and the stage doesn't run again until the round changes.
  - A run fails when its result isn't a `success` or is an error (3.3), when its output fails validation or the stage's check (8.2), when it times out, when it leaves no valid result across a restart (8.2), or when its result fails to apply `loop.max_attempts` times. A GitHub read that fails inside a stage's check is a failure to apply, so it counts only toward that limit.
  - A usage limit (3.3), a failure to start the run, a shutdown and a dropped result (8.2) don't count, nor an apply held back by unexplained labels (5.1).
  - A result asking for a transition the table lacks isn't an attempt either. It gets one state error, `kind=transition`, and the stage doesn't run again until the issue moves, gets new input, or changes type or parent.
  - **The interviewer is an exception to that rule.** Its check (9.1) fails a result that would leave the issue with no move, before anything is written. The main case is typing an issue with a parent as `story` (section 4): the table moves only a story with no parent to `spec-review`, and the interviewer doesn't run on a story with a parent, so a round would get no answers read. Such output is invalid, so it is a failed attempt, not a `kind=transition` state error. After `loop.max_attempts` of them, the owner gets the `kind=attempts` state error. A result on an issue the interviewer no longer runs on, such as one the owner retyped as a task or gave a parent during the run, is a failed attempt too.

## 9. Owner interaction

The owner should never have to go and work out what is being asked.

Bot-created issues do not subscribe the owner to notifications. So every question round, typed comment, spec, plan, human task, triage flag and new digest item @mentions the users in `notify` (section 13). Each of these comments carries a `cc @<login> @<login>` line: at the end, or for a spec, just before the revision (11.1).

Agent text can't mention anyone: the daemon puts a zero-width space after each `@`, or HTML entity for one, that isn't followed by a space. It also escapes markers (5.1) and HTML tags in agent text.

### 9.1 Questions

Questions go in one comment per round:

```
### Questions for #123 (round 2)

**1. Where should the quote expiry be enforced?** Blocks: #130, #131
   a. Server-side on booking (recommended): one place, covers PWA and MCP
   b. Client-side countdown only
   c. Both

**2. ...**

Reply in one comment, e.g. `1a 2c 3: free text`.
```

The comment then ends with the `cc` line (section 9).

Rules:

- Each question stands alone, lists what it blocks, and offers 2 to 4 options with the recommended option first and marked.
- Free-text answers are always allowed.
- Questions wait for an answer indefinitely. There is no default-on-silence.
- The interviewer asks at most `max_questions_per_round` (default 7) per round, most blocking first.
- **The type line.** Every comment that sets a type starts with `Type: <type>, because <reason>`: a question round, a typed comment or a spec's revision comment (11.1). On an issue with no type, the interviewer's first comment starts with it, or with its questions when it can't tell yet (section 4).

**When the interviewer runs.** It runs at `clarifying` on an epic, a story with no parent, or an untyped issue, in two cases:

- **On entry.** The latest transition entered `clarifying` by anything other than an interviewer run: a move into the pipeline (6.2), `/revise` or a revision request, or an override. It runs at once, with no input needed.
- **After a round.** The latest transition is an interviewer run's `clarifying → clarifying`, and there is at least one trusted answer since (9.2). Commands, App comments and dirty comments don't count.

Any answer starts the next run, including a partial one. That run sees every answer so far and asks again whatever is still open. Rounds end when it returns a spec. There's no cap on rounds. A bug, a task and a story with a parent get no runs (7.2).

**Rounds.**

- A round's transition marker carries `round=<n>` and `questions=<count>` (section 5), so round numbers and question IDs come from markers, not from comment text. Question `<k>` of round `<n>` has the ID `Q<n>.<k>`.
- A round is numbered one more than the highest round on a clean round marker.
- A round whose comment is dirty (5.1) isn't read, so it counts as 0 questions. If it was the latest round, its marker no longer counts and the next round takes its number: the round is cancelled, and the interviewer runs again. Under CODEOWNERS trust (6.1), a write collaborator who isn't trusted can edit an App comment, so they can do this.

**The interviewer's outcomes.** Its output is one object whose `outcome` is `questions`, `spec` or `typed`:

- `questions`: a round in the format above, at `clarifying`.
- `spec`: the spec, written into the body and posted as a revision (11.1). The issue moves to `spec-review`.
- `typed`: only for an issue whose type the interviewer may set (section 4), when it finds the issue is a bug or a task. It sets the type, and its comment has the type line, then "Bugs and tasks wait at `clarifying` for now, until issuedog can implement them (build phase 3), so I won't ask anything more here.", then the `cc` line. The move is `clarifying → clarifying`, with no `round` or `questions`. The issue then waits (7.2).

**Checks.** The daemon checks the output after its schema, and fails closed: a broken rule is a failed attempt (8.3), logged with the reason, and nothing is written.

- 1 to `max_questions_per_round` questions, each blocking at least one issue, with 2 to 4 options.
- One-line question, option, reason and list item text.
- A `type` only where the interviewer may set one (section 4). `typed` sets bug or task. A round or a spec sets epic or story, and a spec on an untyped issue must set one.
- The Decisions rules (11.1). A decision's `by` is a login with a clean, trusted comment on the issue.
- Every text it renders fits GitHub's 65,536-character limit, with 1,024 to spare.
- The move fits the issue once its type is set, and the issue is still one the interviewer runs on (8.3).

### 9.2 Commands

| Command | Where | Effect |
|---|---|---|
| `1a 2c 3: text` | Issue | Answers the latest question round |
| 👍 on a revision comment | Issue | Approves that revision (7.4) |
| `/approve` | Issue | Approves the current spec or plan revision |
| Any comment that is not a command | Issue in `spec-review` or `plan-review` | Sends the spec or plan back, with the comment as the notes (7.4) |
| `/revise <text>` | Issue | Sends the spec or plan back with the owner's notes |
| 👍 on the issue | Human task from the planner | Owner finished it (9.3) |
| 👍 on the latest hand-off comment | Blocked task at `needs-human` | Same as `/done` (9.3) |
| 👍 on a state-error comment | Any issue | Acknowledges the error (5.3). The stage doesn't change |
| 👍 on the latest triage flag | Issue at `intake` | Takes the flag's recommendation: admits the issue, or leaves it to people (6.2) |
| `/track` | Issue at `intake` | Admits the issue, flagged, left or never triaged (6.2) |
| `/ignore` | Issue at `intake` that isn't in the pipeline | Leaves the issue to people (6.2) |
| `/done` | `needs-human` issue | Owner finished the human task (9.3), or wants a closed PR's task started over |
| `/hold`, `/resume` | Any issue | Pauses or resumes the issue and its descendants. `/resume` also ends a discussion hold (6.4) |
| 👍 on an untrusted comment or a hold notice | Issue held for discussion | Approves the comment, or accepts the edit or deletion (6.4) |
| `/priority p1` | Any issue | Sets priority |
| `/model <name>`, `/model verifier <name>` | Any issue | Sets the model of the issue's runs other than the verifier's, or with `verifier`, of its verifier runs (3.3) |
| `/effort <level>`, `/effort verifier <level>` | Any issue | Sets the effort the same way (3.3) |

A comment is a command when its first line is a slash and a word, such as `/priority p1`. A known command followed directly by more text, such as `/approve.`, is that command with a bad argument. An unknown word followed directly by more text, such as `/usr/bin`, is plain text. Only comments by trusted users that aren't dirty (5.1) count, oldest first.

- **Applied.** The daemon reacts 👍 to each command comment it has applied, after its marker (5.2). GitHub has no ✅ reaction. A reaction command is acknowledged by the transition comment (5.2).
- **Rejected.** A command that doesn't apply, because it is unknown, has a bad argument, doesn't fit the issue's stage or fails its own check, gets the App's 👎, then a reply with the reason carrying `<!-- issuedog:reply reason=rejected cmd=<comment id> -->`. The 👎 is the record that it was processed: a reply can be edited or deleted, but only the App can remove its own reaction. A command with the App's 👎 but no clean reply gets its reply posted again. A rejection isn't a state error (5.3). The reply to an unknown command lists the commands, `/track` and `/ignore` included.
- **Processed.** A comment is processed once a `cmd` or `reply` marker names it, or it has the App's 👍 or 👎. It is never applied again.
- **Edited.** A comment edited after the App used it gets one reply asking for a new comment, carrying `reply reason=edited cmd=...`, and the edit is ignored. That covers a command edited after its 👍, even into plain text except as below, and an answer edited after a later transition's `seen` covered it. A rejected command edited later gets no new reply, and never counts as an answer.
- **Answers.** At `clarifying`, a trusted comment that is not a command, posted after the round bound, is an answer to the current round, and gets a 👍. The bound is the latest transition marker's `seen` (section 5), or, if it has none, the ID of the App comment holding that transition. Comment IDs only increase, so a comment posted during a run belongs to the next round. Only transition markers move the bound: a reply, error, revision or dirty marker doesn't, so a rejection posted at `clarifying` keeps that round's answers. A command or answer edited before the App uses it is read with its latest text.
- **Model and effort.** `/model` and `/effort` work like `/priority`: each adds one label, `model:*`, `effort:*`, `verifier-model:*` or `verifier-effort:*`, removes the others of that kind, then 👍s the comment, with no marker (5.2). `verifier` matches in any case.
  - `/model` takes a full name on `models.allowed` or an entry's label suffix (section 13), ignoring case, and sets the label with that entry's suffix. A name that matches more than one full name only when case is ignored must be given in its exact case.
  - `/effort` takes `low`, `medium`, `high`, `xhigh` or `max`, ignoring case.
  - A missing or extra argument, a model not on `models.allowed`, or an unknown level is rejected (see Rejected). For a model or level that isn't allowed, the reply lists the allowed label suffixes, or says the config allows no model, or lists the levels.
  - Each write is checked first, so a replay after a crash redoes only what's missing. If a reload dropped the model from `models.allowed` after the label was added, the replay still finishes rather than rejecting the command.
- **Commands edited into answers.** The App 👍s both applied commands, such as `/priority`, which writes no marker, and answers, so GitHub can't tell them apart. An applied command edited into plain text at `clarifying`, in the current round, therefore becomes an answer, and the interviewer reads the edited text. A command that a marker names, or that the App 👎'd, isn't.

PR reviews use GitHub's own review flow.

### 9.3 Human tasks

A task reaches `stage:needs-human` in one of two ways:

- **Blocked work.** A task needs an owner action: the implementer reports it, the retry cap is reached (8.3), or its PR was closed without merging. The daemon posts a hand-off comment using the template below. On `/done`, or a trusted 👍 on the latest hand-off comment, the task returns to `stage:ready`. A 👍 on an earlier hand-off comment, or on the issue itself, doesn't resume blocked work.
- **Planned work.** The planner emits a task marked `human`, such as creating an App or a ruleset (section 11.3). It is created at `stage:needs-human`. The owner signals done with `/done` or a 👍 on the issue. When **Done when** can be checked automatically, the daemon runs the check first; if it fails, the daemon replies with the output and leaves the issue open. Otherwise it closes the issue.

A `stage:needs-human` issue uses this template. The agent must fill every field; an empty field is a validation failure, and the agent asks a question instead.

```
**Goal:** one sentence
**Why it blocks:** #N cannot proceed until ...
**Steps:**
1. exact action, with exact values, commands or settings
2. ...
**Links:** direct links to the console page, file, or doc
**Done when:** observable result the owner can check
**Estimated time:** 5 min
```

### 9.4 Digest

One pinned issue per repo, `issuedog digest`. It uses one of the repo's three pin slots.

- The body is the current list: open questions, pending approvals, hand-offs and unblocked human tasks, PRs waiting for review, issues held for discussion (6.4), dropped children with an open PR (6.4), triage flags waiting on a code owner (6.2), and state errors (5.3). Listing triage flags isn't built yet (#7). These are exactly the items the daemon assigns (9.6). Each state error is also posted on its issue as a comment (5.3). Items are ordered by how many tasks each one unblocks.
- Each item states what is being decided, the options, the recommendation, and links to the issue.
- The daemon rewrites the body when the list changes. It posts a short comment that @mentions `notify` only when a new item appears, so GitHub sends one notification per new item.

### 9.5 Escalation

Deterministic rules decide what needs the owner. An LLM may add items to the escalation list. It never removes one.

Escalation triggers come from config (`escalate` in section 13), for example changes to dependency manifests, migrations, deployment files or payment code. A triggered PR gets the `review:high-risk` label and a "Look at these first" section listing the matched files and rules.

### 9.6 Assignment

The owner finds everything waiting on them in GitHub's own "assigned to me" view. The daemon assigns every user in `notify` to an issue or PR while it waits on them, and unassigns them as soon as it doesn't:

| Item | Assigned while |
|---|---|
| Question round | The issue is at `clarifying` with an unanswered round (9.1) |
| Spec or plan | The issue is at `spec-review` or `plan-review` |
| Hand-off | The task is at `needs-human` after a hand-off comment (9.3) |
| Human task | The task is at `needs-human` and every issue blocking it is closed |
| Pull request | The PR is open and its task is at `pr-open`. It is unassigned when the PR merges or closes, or when the task leaves `pr-open` |
| State error | From the daemon's state-error comment until a trusted user reacts 👍 to it, or its cause is gone (5.3) |
| Discussion hold | The issue is held for discussion and a trigger is unresolved (6.4) |
| Dropped with an open PR | The issue is held with `reason=dropped` (6.4), until its PR merges or closes |
| Triage flag | The issue's latest triage marker is a flag (6.2), until a code owner answers it. Not built yet (#87) |

- These are the same items the digest lists (9.4).
- Each PR carries the `priority:*` label of the task it closes. The task's label is the priority: the daemon copies it to the PR and reverts any other change to the PR's label.
- The owner sees what's waiting with `is:open assignee:@me sort:created-asc`, and the most urgent items by adding `label:priority:p0` or `label:priority:p1`. The digest orders the same items by how many tasks each one unblocks.
- Only the App changes assignees on pipeline issues and PRs. Assignees are never read as state (section 5): a manual change is reverted on the next poll and changes nothing else.

## 10. Verification

Verification is the gate between implementation and a PR.

### 10.1 Deterministic checks (run first)

The daemon decides these itself, outside Claude, from exit codes and its own git inspection of the bare repo.

1. Every command in `verify.commands` (tests, lint, vet, build) passes.
   - The commands run as `agent` in a fresh checkout of the head SHA.
   - They get no GitHub credentials, the `network.allow` egress list and `verify.command_timeout`.
2. The diff touches no protected path (section 6.3).
3. No file matching `verify.test_globs` is deleted, and no new `verify.skip_markers` are added.
   - Markers are checked only on added lines, and only in files matching `verify.test_globs`.
   - Markers are plain strings. A marker that starts with a letter matches only where it is not preceded by a letter, digit or `_`. So `xit(` matches `xit("works", …)` but not `os.Exit(`, and `.skip(` still matches `describe.skip(`.
4. Files in `verify.config_paths` are unchanged, unless the task's scope is `verify.tooling_scope`.
5. When the diff modifies files matching `verify.source_globs` that are not tests, it also adds or modifies files matching `verify.test_globs`. Changes to other files, such as docs, are exempt.
6. The diff is at most `verify.max_diff_lines`, counted by the daemon. Binary files are allowed only if they match `verify.binary_allow`.
7. The branch is based on current `main` and merges cleanly.

Rule 2 is checked before rule 1, so no command ever runs on a diff that changes agent config.

Any failure is a verify failure, with the output attached as findings. A diff over the size limit gets the finding "task too large, split it". The LLM verifier cannot override a deterministic failure.

### 10.2 LLM verifier (task level)

It runs only after the deterministic checks pass. It runs in a fresh process and a fresh checkout of the head SHA (section 3.4), with:

- the task's acceptance criteria and definition of done
- the full diff against the base, which the daemon writes to the context directory (rule 6 guarantees it fits)
- the deterministic check output

It returns `pass`, `request_changes` with findings tied to acceptance criteria, or `question` when a criterion is ambiguous.

The run fails closed: an unparseable or missing verdict counts as `request_changes`.

### 10.3 Story verification

- **When it runs.** The trigger is in section 7.3.
- **What it runs.** The daemon runs `verify.story_commands` (e2e suites) on `main`, under the same rules as 10.1 rule 1.
- **What the LLM checks.** The LLM verifier checks the story's acceptance criteria against the e2e results.
- **On failure.** Failures become `bug` sub-issues with an `origin` marker.

### 10.4 The `issuedog/verify` check

- **On pass.** The daemon posts a check run named `issuedog/verify` with conclusion `success` for the head SHA. It includes a short summary and a link to the verifier comment.
- **Ruleset.** The `main` ruleset requires this check, with the issuedog App pinned as its source. Nobody else, including anyone with write access, can satisfy it.
- **When `main` moves.** The daemon re-runs section 10.1 on the merge result of `main` and the PR head. If that fails, it updates the check to `failure`. The ruleset does not require branches to be up to date, so a merge to `main` doesn't force every open PR to update and be re-approved. The daemon's merge-result check stands in for that.

## 11. Templates

Adapted from CCPM (MIT, Copyright (c) 2025 Ran Aroussi). Attribution goes in `NOTICE`.

Acceptance criteria and the definition of done are plain `-` bullets, not `- [ ]` checkboxes. Nothing reads or ticks them: the verifier's comment and the checks record what's done, and a tick would be a body edit that changes the hash (7.4).

### 11.1 Spec (issue body after spec-review)

```
## Problem
## Goals
## Out of scope
## Users and flows
## Constraints
## Acceptance criteria
- observable, testable statements
## Decisions
- Q2.1 → c: summary of the answer (owner, round 2)
```

The interviewer's `spec` output fills these sections, and the daemon renders them:

- **Writing it.** The spec replaces the whole body. The title stays, and the original text stays in GitHub's edit history.
  - Agent text is escaped (5.1). A `#` or `##` heading in it is demoted to `###`, so it can't add a section.
  - Acceptance criteria are plain `- ` bullets, with any `[ ]` the agent wrote stripped. An empty section reads "None.".
  - With a dirty body (5.1), the dirty note stays at the top, naming the same login, so the body stays dirty (7.4).
- **Write order.** The type label, if the spec sets a type, then the body edit, skipped if the body already matches. Then the revision is posted with the move to `spec-review` (7.4), and its hash is taken from the body GitHub returns. Once the move is written, a replay doesn't edit the body or the type again.
- **The revision comment.** It starts with the type line when the spec sets a type (9.1). Then come the agent's `message`, which answers revision notes or says what changed, a list of the decisions "Added, not asked", and the `cc` line (section 9). Then the revision and how to approve it (7.4).
- **Decisions.** Every question asked appears at least once, in one of these forms:
  - answered: `- Q2.1 → c: <summary> (mertenvg, round 2)`, or `- Q2.1 → free text: <summary> (mertenvg, round 2)`
  - superseded: `- Q1.3 → superseded by Q2.1`, by a question asked in a later round
  - dropped: `- Q1.3 → dropped: <why>`

  A decision the interviewer made without asking reads `- Added, not asked: <summary>`. An answer names a login with a clean, trusted comment on the issue, and the round it was answered in, which is no earlier than the question's. Only questions on clean round markers count as asked (9.1).

### 11.2 Task

```
**Parent:** #N   **Scope:** scope:booking   **Priority:** p2
## Context
What exists today and which files matter.
## Change
What to build.
## Acceptance criteria
- ...
## Definition of done
- Tests added or updated for every changed behavior
- verify.commands pass
- No new skip markers
## Test plan
Which tests prove each criterion.
```

A task that writes comments or issue bodies adds `- Text the daemon didn't write is escaped (5.1)` to its definition of done.

### 11.3 Planning rules

- An epic plan contains either stories, with acceptance criteria and no tasks, or tasks, when the epic has a single user-facing outcome. A story plan contains tasks.
- At most `plan.max_tasks` (default 10) tasks per plan. Larger work becomes more stories.
- Each task maps to one scope label from the allowed list, and each task other than a human task maps to one PR.
- Each task should fit within `verify.max_diff_lines`.
- Prefer extending existing code over adding new modules; the plan names the existing files each task touches.
- A scope outside the allowed list becomes a question to the owner.
- Work only the owner can do is a task marked `human`, written in the template from section 9.3 rather than 11.2.
- A revised plan for an epic or story that is already `planned` keeps the ID of every kept or changed item and marks each item kept, changed, new or dropped (5.2, 6.4). A child whose issue is closed can only be kept; changing it means a new item with a new ID.
- Phase 2, not built yet: the planner proposes a model and an effort for each plan item, from `models.allowed` and the effort levels (section 13). A value outside them fails validation. When the daemon creates the tasks, it sets their `model:*` and `effort:*` labels from those values (3.3). Until then, tasks get these labels only from `/model` and `/effort` (9.2) or by hand.

The daemon validates the plan before it moves to plan-review:

- schema
- the scope list
- task counts
- the dependency graph has no cycles, including `depends-on` links to other repos
- no parent would exceed 100 sub-issues, and no issue would exceed 50 links of one dependency type
- for a revised plan of an issue that is already `planned`: every existing child appears exactly once, marked kept, changed or dropped, and no closed child is marked changed

### 11.4 Plan

One comment, which is also the revision comment (section 7.4):

```
### Plan for #N (rev 1)

One line on why this plan has stories or tasks.

**S1. Title**
Outcome: one sentence
- acceptance criterion
Depends on: nothing

**T1. Title** · scope:<name> · human (only for a human task) · kept, changed, new or dropped (only when revising a `planned` issue, 11.3)
- Change: what to build, and the existing files it touches
- Acceptance: observable, testable statements
Depends on: S1, or nothing

**Order:** S1 → S2 and S3 in parallel → S4

Validation (11.3): item count, structure, scopes, no cycles.

**To approve this plan, react 👍 to this comment.** Any other comment sends it back for revision.
```

A plan contains either stories or tasks (11.3); the template shows both forms. Item IDs (`S1`, `T1`) are the stable IDs used in `origin` markers (section 5.2).

### 11.5 Pull request

The title is `<type>: <short sentence>`, where `<type>` is one of `pr.title_types` (section 13; default `feat`, `fix`, `chore`). The implementer names the type in its structured output, and output naming a type outside the list fails validation (3.3). With squash merging (section 15) the title becomes the commit subject on `main`.

```
Closes #N

## What changed
## Why
## Tests
- added: ...
- updated: ...
## Results
| Check | Result |
|---|---|
| go test ./... | pass (412 tests, 38s) |
## Verifier notes
## Look at these first   (only when escalated)
```

## 12. Agents and skills

- **Stage prompts.** These live in `.issuedog/prompts/{triage,interviewer,planner,implementer,verifier}.md`. issuedog ships defaults, and a file on `main` overrides the default (section 3.4).
  - The defaults and the schemas are embedded in the binary, so the image needs no extra files. `triage.md` and `interviewer.md` ship now. The planner, implementer and verifier prompts ship with their stages. Schemas can't be overridden.
  - A blank file on `main` falls back to the default, with a warning. A file over 64 KiB, or a read error other than not-found, is a setup error: the run doesn't start and isn't counted.
- **Context files.** Each run gets its files in `/run/issuedog/<run-id>/` (3.1). Text in `untrusted.md`, and every comment and body in the interviewer's files, sits in a code fence it can't close, under a heading the daemon writes.
  - `system.md` is a fixed preamble the daemon writes, then the stage prompt. The preamble covers the trust rules, the output contract and this repo's limits, and a prompt on `main` can't remove it. The interviewer's limits are the questions per round, the rounds so far and the next round's number, the logins a decision may name, and whether it may set the type (section 4). Its preamble always says, too, that an issue with a parent is never typed `story`.
  - `prompt.md` is the daemon's short task text, saying where the context files are. Triage's names each one by its full path. The interviewer's names the context directory once, then each file by name.
  - `untrusted.md`, when there is anything for it, holds the untrusted input of 6.2.
- **The interviewer's files.**
  - `issue.md`: the number, title, type or "none", parent (`#N` or "none"), author, and the body. A dirty body, or a title or body an untrusted author changed after admission (6.2), is replaced by a line saying where it is, if anywhere.
  - `conversation.md`: the App's clean comments, with markers left out, earlier rounds labelled "Round N, QN.1–N.k" and revisions labelled; and trusted, clean comments, with author, ID and the round they answer. Oldest first.
  - `input.md`: this run's input (9.2): each input labelled as an answer to the latest round, as the revision notes that sent the spec back, or as new since the issue came to `clarifying`, as after an override. With no input, it says "none: first run".
  - A login in these files' headings is named only when it is a valid GitHub login: letters, digits and single hyphens, with an optional Enterprise Managed User `_shortcode` suffix, 39 characters at most, or an App's `[bot]` login. Anything else shows as "someone unnamed".
- **The triage agent's files.**
  - `issue.md`: the number, the author and whether they're trusted, and the type. For a trusted author, it also has the title and, unless it's dirty, the body.
  - `comments.md`, if there are any: trusted people's clean comments. The App's comments are left out.
- **Domain subagents.** These live in `.claude/agents/*.md` and are available to the implementer and verifier during a run. Add one only when a domain has rules the model keeps getting wrong.
- **Skills.** These live in `.claude/skills/*`, for repeatable procedures such as adding a migration.
- **Changing them.** Agents cannot change any of these. They are protected paths, and the verifier only ever loads the versions on `main`.

## 13. Repo configuration

`.issuedog/config.yaml`, read from `main`:

```yaml
types: issue-types        # or: labels
scopes: [booking, payments, drivers, notifications, infra, tooling]
notify: [mertenvg]
plan:
  max_tasks: 10
  review: true            # false: story task plans skip plan review; epic plans are always reviewed
verify:
  commands:
    - go vet ./...
    - golangci-lint run
    - go test ./...
  story_commands:
    - make e2e
  command_timeout: 20m
  max_diff_lines: 400
  skip_markers: ["t.Skip(", ".skip(", "xit(", "#[ignore]"]   # plain strings, word-start match; added lines in test_globs files only (10.1 rule 3)
  test_globs: ["**/*_test.go", "**/*.test.ts", "e2e/**"]
  source_globs: ["**/*.go", "**/*.ts"]
  config_paths: [".golangci.yml", ".golangci.yaml"]
  tooling_scope: tooling
  binary_allow: []
  protected_paths: []     # added to the built-in list (section 6.3)
escalate:
  paths: ["go.mod", "**/migrations/**", "deploy/**", "**/payments/**"]
pr:
  title_types: [feat, fix, chore]   # PR title is "<type>: <short sentence>" (11.5)
stages:
  timeouts: { interviewer: 10m, planner: 20m, implementer: 60m, verifier: 30m }
  max_turns: { interviewer: 30, planner: 60, implementer: 200, verifier: 80 }
  max_budget_usd: {}      # optional, per stage
  model: { planner: opus, implementer: sonnet }   # optional, per stage; unset passes no --model (3.3)
  effort: { implementer: medium, verifier: high } # optional, per stage
  fallback_model: { implementer: [opus] }         # optional, per stage; a list, tried in order
  max_effort: xhigh       # the cap for retries' raised effort; default max
models:
  allowed:                # names /model, labels and stage defaults may use; empty allows no model overrides
    - opus
    - sonnet
    - { name: claude-haiku-5-5-20261001, label: haiku-dated }   # label: optional label suffix
loop:
  idle_sleep: 60s
  max_attempts: 3
  max_questions_per_round: 7
network:
  allow: [api.anthropic.com, proxy.golang.org, sum.golang.org]
usage_limit:
  pattern: "(?i)((usage|session|weekly|opus|sonnet|fable) limit|you('|’)?ve (hit|reached) your [a-z ]*limit|you have (hit|reached) your [a-z ]*limit)"
cross_repo:
  allowed: [mertenvg/dmcn, dmcnp/open-dmcn]
```

Model and effort keys:

- `stages.model`, `stages.effort` and `stages.fallback_model` are optional for each stage. An unset one passes no flag, so the CLI's default applies (3.3).
- Triage runs (6.2) use the interviewer's entries in every `stages` key, so `stages` has no `triage` key.
- Effort levels are the CLI's five, `low`, `medium`, `high`, `xhigh` and `max`, fixed in code. `stages.effort` and `stages.max_effort` must be one of them.
- Each `models.allowed` entry is a name, such as `opus`, or a map of `name` and an optional `label`. A name has no space or comma.
- An entry's label suffix, as in `model:<suffix>`, is its `label`, else its name if that is at most 35 characters, else the name's last 35 characters. GitHub caps a label name at 50 characters, and `verifier-model:` takes 15. A `label` is at most 35 characters, with no space or comma. The daemon logs each entry whose suffix is cut from its name.
- Two entries whose suffixes match ignoring case, or a name that matches another entry's suffix ignoring case, are refused, so labels and `/model` are never ambiguous.
- `stages.model` and `stages.fallback_model` must name full names on `models.allowed`, in exact case.
- A file that breaks any of these rules is refused. At startup that stops the daemon (3.5); on a reload the last good config stays (8.1).

Changes to this file go through a human-authored PR (section 6.3).

## 14. Cross-repo work

- **Proposing.** A planner may propose an issue in another repo listed in `cross_repo.allowed`.
- **Creating.** The daemon creates it with a link back to the source issue and labels it `stage:intake`. It carries no `origin` marker, so the target repo's daemon waits for a trusted person there to add `approved`.
- **Same owner.** The daemon links the dependency through the issue dependencies API (blocked-by).
- **Different owners** (for example `mertenvg/dmcn` and `dmcnp/open-dmcn`). Sub-issues are not allowed across owners, and native dependencies across owners are unconfirmed (section 16). The daemon instead adds a `<!-- issuedog:depends-on url=... -->` marker to the blocked issue and treats the issue as blocked until the target is closed.
- **Access.** The issuedog App must be installed on every owner in the allowed list.

## 15. GitHub setup per repo

1. **GitHub App.**
   - Repository permissions: Contents read/write, Issues read/write, Pull requests read/write, Checks read/write, Commit statuses read/write, Metadata read.
   - Organization permissions: Members read (for CODEOWNERS teams) and Issue Types read.
   - Install it on each owner: the personal account and each org. Each installation has its own hourly token, and the daemon refreshes them.
   - No admin permissions.
   - Import its private key into the state volume once: `docker run --rm -i --cap-add NET_ADMIN -v issuedog-state:/state issuedog key import < app-key.pem`. Then delete the downloaded file. To rotate the key, generate a new one on GitHub and import it the same way.
2. **Plan.** Rulesets work on public repos on GitHub Free, and on private repos only with Pro, Team or Enterprise. Code owners on private repos also need Pro.
3. **Ruleset on `main`:**
   - require a pull request
   - require code-owner review, and dismiss stale approvals on new commits
   - require the `issuedog/verify` check with the issuedog App as the pinned source; leave "require branches to be up to date" off (section 10.4)
   - block force pushes
   - allow squash merges only
   - bypass list: Repository admin, "for pull requests only", so the owner can merge their own protected-path PRs; never the write role and never the App

   In the repo's general settings, allow only squash merging and set its default commit message to "Pull request title and description", so the PR title becomes the commit subject and the description its body (11.5).

   **Interim.** While the pipeline is run by hand with the owner's token, the owner authors every PR and cannot approve it. Until the App runs, the ruleset requires only a pull request with squash merging and blocks force pushes; code-owner review and the check are added then.

4. **CODEOWNERS** including `/.github/CODEOWNERS`. The App is never a code owner.
5. **Labels.** `stage:*`, `priority:*`, `scope:*`, `approved`, `review:high-risk`, and `type:*` for personal-account repos. `model:<suffix>` and `verifier-model:<suffix>` for each entry in `models.allowed`, and `effort:<level>` and `verifier-effort:<level>` for each effort level (section 13). The daemon creates any that are missing on startup, and a new model's after a reload (8.1). It never edits or deletes a label, so a model dropped from the list keeps its labels.

   `triage:ask` and `triage:left` (6.2) aren't created on startup: GitHub creates each the first time the App adds it.
6. **Org repos.** An org owner creates the `Epic` and `Story` issue types.

## 16. Open items to confirm

- **Edit history and link events.** Checked live (#91): `userContentEdits` keeps at most 100 entries, the original post and the 99 newest edits, newest first, and deleted revisions keep their `editor`. So the history alone can't keep content dirty, and the `dirty` note does (5.1). Section 6.4 reposts an `origin` marker from the App's last version in `userContentEdits`, or from the original post. Confirm each edit entry gives that version's full text. Also confirm which timeline events show who changed sub-issues and dependencies, so untrusted changes to them can be treated as discussion triggers.
- **Cross-owner dependencies.** Do native blocked-by links work across owners? One report says GitHub returns 422. If they do work, they could replace the `depends-on` marker.
- **Pinning the check source.** The ruleset can only pin the App as the source of `issuedog/verify` once the App "has recently submitted a check run". Setup must post one bootstrap check run before adding the rule.
- **Pinning the digest.** The permission `pinIssue` needs from an App is not documented. Test it with Issues write.
- **`--json-schema` in long runs.** It must work together with `--dangerously-skip-permissions` and long tool loops. Also confirm how many structured-output retries are allowed.
- **Kind on an ack.** An ack reply (5.3) names no kind, so it covers its key for every kind. Comment IDs and timeline event IDs come from separate sequences, so an ack of one error could silence a later, different error with the same number. Consider copying `kind=` into the ack and matching on kind and key, with a kindless ack still covering every kind (#78).
- **Where a reposted `origin` lives.** Section 6.4 reposts an `origin` marker in an App comment, since markers in a dirty body are never read. #87 confirms this, and that the class derivation and the de-duplication search (5.2) read it there.
- **Stacked holds.** A `/hold` on a child that is already held by its parent's `/hold` writes no marker (6.4), so the parent's `/resume` ends the child's hold too. #87 decides whether stacked holds are recorded.
- **Where a plan's hash lives.** A plan comment is also its revision comment (section 7.4), so its hash can't be written inside it without changing it. Decide whether it goes in a separate transition comment or is derived from the comment's edit history. If plan revisions reuse the `revision` marker, it needs a field that tells spec revisions from plan revisions.
- **Running a Done-when check.** The **Done when** text of a human task is written by an agent. Decide how the daemon runs an automatable check (which uid, which credential) without breaking sections 3.1 and 3.2.
- **Approving PR reviews with comments.** Section 7.2 covers reviews with comments and approving reviews with none. An approving review that has comments is not yet covered.
- **Usage-limit text.** The text `-p` prints when a subscription limit is hit is undocumented, so `usage_limit.pattern` must be checked against a real occurrence.
- **`--bare` becoming the default.** Anthropic plans to make `--bare` the default for `-p`. `--bare` ignores `CLAUDE_CODE_OAUTH_TOKEN` and skips project agents and skills. Pin the Claude Code version, and re-test before upgrading.
- **Egress firewall on macOS.** The owner-matched firewall rule (section 3.1) needs `--cap-add NET_ADMIN` and the daemon's file capabilities. Confirm both work on Docker Desktop for Mac.
- **Rumi Black's plan.** Confirm its visibility and plan meet the ruleset requirement in section 15.
- **Public release.**
  - Anthropic does not allow third-party tools to offer claude.ai login or route requests through users' subscriptions without prior approval. The README should therefore require users to bring their own `ANTHROPIC_API_KEY`.
  - Ask Anthropic before supporting subscription tokens in a distributed build.
  - The product name must not include "Claude Code". Plain-text "runs Claude Code" is fine.
- **Max plan limits.** Limits for sustained unattended use on the owner's own Max plan are undocumented. Anthropic's usage policy assumes "ordinary, individual usage".

## 17. Build phases

Rumi Black first; DMCN after phase 3.

1. GitHub App setup, daemon skeleton, Go GitHub client, sandbox uids and egress rule, trust checks, label and marker reading rules, transition table, leases, intake → clarifying → spec-review, digest. GitHub interactions are tested against `httptest` fakes only, and passing them is the acceptance bar.
2. Planning, plan review, human tasks from the planner, idempotent sub-issue creation, dependencies, scopes, priority, model and effort for each plan item (11.3).
3. Implement, bundle-based git handoff, deterministic checks, LLM verifier, retries, PR, `issuedog/verify` check, PR reconciliation.
4. Story verification, escalation rules, cross-repo issues, DMCN onboarding, domain agents.

## 18. Prior art

- CCPM (automazeio/ccpm, MIT): templates in section 11 are adapted from it.
- agent-foreman (simplerjiang/agent-foreman, MIT): ideas for deterministic escalation, fail-closed review and one-tap decisions. No code copied.
- foreman (bayological/foreman, no license): ideas only, including the explicit transition table and feeding the last failure into retries. No code copied.
