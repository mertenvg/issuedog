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
- The daemon acts on GitHub as a GitHub App (section 15) through a Go client: `go-github` with an App installation-token transport, plus raw REST calls for endpoints the client lacks, such as sub-issues and issue dependencies. There is no `gh` dependency. This gives native ETags for polling (section 8.1), typed responses, and tests against `httptest` fakes.
- The daemon starts a fresh `claude -p` process for every stage run, including every retry. Sessions are never resumed, so each run starts from the issue and the code, and the verifier never shares context with the implementer.
- The base image follows the existing sandbox setup: `~/.claude/CLAUDE.md` bind-mounted read-only, `--dangerously-skip-permissions` as the non-root `agent` uid, feature branch per task.
- The host must not sleep while issuedog runs (for example `caffeinate -s`, or the equivalent Docker Desktop setting).

### 3.1 Who writes to GitHub

Only the daemon writes to GitHub. Agents return structured output and the daemon validates and applies it.

- The GitHub App private key lives in a file readable only by the `issuedog` uid. The daemon mints short-lived installation tokens and uses them for its own API calls. Nothing running as `agent` ever receives a GitHub credential. That includes GitHub connectors and GitHub MCP servers, which are never configured for an agent.
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
  [--max-budget-usd <stages.max_budget_usd.<stage>>]
```

- The daemon validates the stage's output from the `structured_output` field of the JSON result. The run fails (fail closed) if `subtype` is anything other than `success`, including `error_max_structured_output_retries`, or if `structured_output` is missing or fails the daemon's own validation.
- The daemon writes the full JSON result to `/state/runs/<run-id>.json` before acting on it, for restart recovery (section 8.2).
- Each stage has a timeout from config. On timeout the daemon kills the whole process group, and the run counts as failed.
- `--max-budget-usd` is a client-side estimate and is optional.
- **Credential.** The agent gets exactly one Claude credential: `CLAUDE_CODE_OAUTH_TOKEN` or `ANTHROPIC_API_KEY`, whichever is set in the daemon's environment. If both are set, the daemon refuses to start.
- **Usage limits.** `-p` mode does not report the reset time in a machine-readable form. The daemon matches the result text against `usage_limit.pattern`. If the text includes a reset time it waits until then. Otherwise it backs off, starting at 15 minutes and doubling to a 2-hour cap. While it waits, steps 1 and 2 of the loop (section 8.1) keep running, so commands such as `/hold` still apply. Usage-limit waits do not count toward the retry cap.

### 3.4 Trusted inputs come from `main`

The daemon reads these from `main` in its bare clone, never from a task branch:

- `CODEOWNERS`
- `.issuedog/config.yaml`, including `verify.commands`
- `.issuedog/prompts/*.md`; a missing file falls back to the default that ships with issuedog
- JSON schemas, which ship with issuedog and are not overridable

The interviewer, planner and story verifier run in a fresh checkout of `main`.

The task verifier runs in a fresh checkout of the head SHA, but only after rule 2 of section 10.1 has passed. Every agent-config path (section 6.3) in that checkout therefore matches `main`.

## 4. Work item types

| Type | Meaning | Created by |
|---|---|---|
| `epic` | A rough idea or large feature | Owner |
| `story` | One user-visible outcome with acceptance criteria | Planner (epic plan) |
| `task` | One PR, one scope; a human task has no PR (section 9.3) | Planner (epic or story plan) |
| `bug` | Something broken | Owner, or story verifier |

- Repos owned by an organization use GitHub issue types. The org's defaults are Task, Bug and Feature, so an org owner must create `Epic` and `Story` first. Repos owned by a personal account have no issue types and use `type:epic`, `type:story`, `type:task`, `type:bug` labels instead. Config selects which.
- Hierarchy uses native sub-issues: epic → story → task, or epic → task. Sub-issues must share the parent's owner. GitHub allows 100 sub-issues per parent.
- An epic with a single user-facing outcome is planned directly into tasks and acts as its own story, including story verification (section 7.3).
- A `bug` goes through clarifying and then becomes a single task. It skips spec review and planning. A bug filed against earlier work starts its body with `Follow-up to #N` and includes the acceptance criterion "the original behavior of #N is unaffected."

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
| Transition history | Bot comments carrying a hidden marker, e.g. `<!-- issuedog:transition from=clarifying to=spec-review rev=3 body=9f2a61c0 run=8f2c cmd=2214587 -->` |
| Attempt count | Hidden marker on the latest bot verifier comment |
| Processed commands | The bot's 👍 reaction on the command comment, plus `cmd=<comment id>` in the transition marker. A reaction command (section 9.2) is recorded as `cmd=r<reaction id>` |
| Spec and plan revisions | One revision comment per revision, naming `rev`. The hash is in the transition marker (section 7.4); for plans, see section 16 |
| Origin of bot-created issues | Hidden `origin` marker in the issue body (section 6.2) |

The only local files are the lease (section 8.2) and the run results in `/state/runs/`. Both are scratch: losing the container loses nothing except the current run.

### 5.1 Reading rules

- **Markers.** The daemon only reads markers in comments and issue bodies written by the issuedog App. A marker anywhere else is ignored.
- **Labels.** A `stage:*`, `priority:*`, `scope:*` or `approved` label counts only if the issue timeline shows that its most recent `labeled` event came from the App or a trusted user (section 6.1).
  - When an untrusted actor adds or removes one of these labels, the daemon reverts the change and posts a short comment.
  - When a trusted user changes a stage label by hand, the daemon treats it as an owner override and records a transition marker with `by=<login>`. It does this even if the change is not in the transition table.
- **Disagreement.** If the stage label and the latest transition marker disagree and 5.1 does not explain the difference, the daemon puts the label back to match the marker and lists the issue in the digest.

### 5.2 Idempotency

Every transition goes through one table in code (section 7). Each write is checked first, so a crashed run can be replayed safely:

- **Transitions.** Applying a transition whose marker already exists is a no-op.
- **Commands.** For a command comment, the daemon writes the transition marker with `cmd=<comment id>`, then adds its 👍 reaction. GitHub has no ✅ reaction. On replay, a marker carrying that `cmd` means only the reaction is missing. For a reaction command, the marker carries `cmd=r<reaction id>`. A reaction can't be reacted to, so the transition comment is the acknowledgment.
- **Created issues.** Every issue the daemon creates has an `origin` marker with a stable item ID, such as `plan=#12 rev=3 item=T4`. Before creating an issue, the daemon searches for that marker and reuses any match. The same applies to sub-issue links and dependency links: the daemon reads the current links before adding any.

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

An issue enters the pipeline when any of these holds:

- A trusted user opened it.
- A trusted user added the `approved` label (checked on the timeline).
- The App created it with an `origin` marker that points to an approved plan revision, or to a story-verifier run. Issues the App creates in another repo (section 14) carry no such marker, so they need `approved`.

Only comments and reactions by trusted users count as answers, approvals or commands. Agents receive other comments in a separate, clearly labeled "untrusted input" section of their context files, and only for epics and stories.

A command counts once. If its comment is edited after the daemon reacted to it, the edit is ignored and the bot replies asking for a new comment. If it is edited before the daemon processes it, the daemon reads the latest text.

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
| `stage:held` | none | Paused by the owner |

The lifecycle of each type:

- **Epic.** intake → clarifying → spec-review → planning → plan-review → planned → closed.
- **Story.** Created at `stage:planning` from an approved epic plan, which already carries its acceptance criteria. Then planning → plan-review (or straight to planned, see `plan.review`) → planned → closed.
- **Task.** Created at `stage:ready`, or at `stage:needs-human` for a human task from the planner (section 11.3). Then ready → in-progress → verifying → pr-open → closed, with loops back through clarifying, in-progress or needs-human. A human task from the planner goes needs-human → closed.
- **Bug.** intake → clarifying → ready, then as a task.

### 7.2 Transitions

| From | To | Applies to | Trigger |
|---|---|---|---|
| intake | clarifying | all | Issue is trusted (6.2) |
| clarifying | clarifying | all | Owner answered; interviewer has more questions |
| clarifying | spec-review | epic | No open questions; spec written into the body |
| clarifying | ready | bug | No open questions; daemon rewrites the body as a task (11.2) |
| clarifying | ready | task | Owner answered the implementer's or verifier's question |
| spec-review | clarifying | epic | Owner `/revise <text>`, or any other trusted comment (7.4) |
| spec-review | planning | epic | Owner approves the current revision with 👍 or `/approve` (7.4) |
| planning | plan-review | epic, story | Planner output passes validation (11.3) |
| planning | planned | story | Output passes validation and `plan.review` is false; daemon creates tasks |
| plan-review | planning | epic, story | Owner `/revise <text>`, or any other trusted comment (7.4) |
| plan-review | planned | epic, story | Owner approves the current revision with 👍 or `/approve` (7.4); daemon creates sub-issues, human tasks and dependencies |
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
| needs-human | ready | task | Owner `/done` on a task that was waiting on the owner; attempt count resets to 0 |
| needs-human | closed | task | Owner `/done`, or 👍 on the issue, for a human task from the planner, and its **Done when** check passes when it can be checked automatically (9.3) |
| any open | held | all | Owner `/hold` on the issue or an ancestor |
| held | previous | all | Owner `/resume` on the issue or an ancestor |
| any open | closed | all | A person closed the issue; daemon stops any run for it |
| closed | held | all | A person reopened the issue |

Any transition missing from this table is an error, except a trusted user's manual label change (5.1). The daemon logs the error, leaves the issue unchanged and lists it in the digest.

`/hold` applies `stage:held` to the issue and to every open descendant, each with its own transition marker. `/resume` restores each one to the stage named in its marker.

### 7.3 Story and epic completion

- When every child of a story, or of an epic planned directly into tasks, is closed, the daemon runs story verification on `main` (section 10.3). The verifier sees each child's close reason, so tasks closed as not planned are not mistaken for delivered work.
  - Failures create `bug` sub-issues under the story.
  - Passing closes the story.
- An epic planned into stories closes when every child is closed.

### 7.4 Approvals bind to a revision

- **Revision numbers.** Each time the daemon writes a spec or plan, it increments `rev`, records a short hash of the body (or of the plan comment) in the transition marker, and posts a revision comment naming the revision. For a plan, the plan comment is the revision comment.
- **Approving.** A trusted 👍 on the latest revision comment, or a trusted `/approve` posted after it, approves that revision, and only while the current body or plan still matches the recorded hash.
  - 👍s on earlier revision comments, and 👍s from untrusted users or from the App, are ignored.
  - Removing a 👍 after the daemon applied the approval changes nothing.
- **Revising.** In `spec-review` or `plan-review`, any trusted comment that is not a command (9.2), posted after the latest revision comment, is a revision request with the comment as the notes. It is handled, acknowledged and recorded exactly like `/revise` (5.2, 9.2).
  - A comment beats a pending 👍.
  - A question counts as a revision request: the agent answers it and posts the next revision, even if nothing else changed.
  - A comment posted after the approval was applied goes to the next stage as owner input.
- **Owner edits the body.** The hash no longer matches. The daemon increments `rev`, records the new hash, posts a new revision comment (for plans, see section 16), and voids any pending approval.
- **PR approval.** This relies on GitHub. The ruleset dismisses stale approvals when new commits are pushed, and `issuedog/verify` is posted for an exact head SHA.

## 8. Scheduling

### 8.1 Picking work

Each loop iteration:

1. Process new trusted comments, reactions and label changes on open issues: answers, `/approve`, `/revise`, `/done`, `/hold`, `/resume`, `/priority`, 👍 on revision comments and human-task issues, and other comments in review stages (7.4). Revert untrusted label changes (5.1).
2. Reconcile PRs: merges, closes, reviews with comments (7.2), new head SHAs, and `main` moving under open PRs.
3. Run any pending non-task stage work for the highest-priority issue that has it: clarifying, spec, planning, story verification.
4. Otherwise pick the next task:
   - It is `stage:ready`.
   - It is not blocked, judged by advanced search `is:blocked` plus any open `depends-on` markers. Search results lag behind dependency writes, so the daemon confirms a candidate through the issue dependencies API before picking it.
   - Ties are ordered by priority, then the parent's priority, then issue number.
5. If nothing is available, sleep `idle_sleep` (default 60s).

- **One task at a time.** Only one task is `stage:in-progress` at a time. A task waiting on the owner releases the worker, and the scheduler moves on to the next independent task.
- **Polling cost.** Polling uses conditional requests (ETags) and `since=` filters, so unchanged resources don't count against the rate limit.

### 8.2 Leases and restart

- **On pickup.** The daemon writes `/state/lease.json` (write to a temp file, then rename) with the issue number, stage, run ID, clone path and start time. The `stage:in-progress` label and the transition marker show the lease on GitHub.
- **On startup.** The daemon reads the lease.
  - If `/state/runs/<run-id>.json` exists and validates, and for an implementer run the clone also has commits ahead of its base, the daemon applies the transition.
  - Otherwise it deletes the clone, resets the issue to its pre-run stage and starts the run again.

### 8.3 Retries

- The verify loop allows `max_attempts` (default 3) implementer runs per round. A round starts when a task becomes `ready`, or when a review with comments sends it back (7.2).
- Each retry prompt includes the previous verifier findings verbatim.
- After the last failed attempt, the task moves to `stage:needs-human` with the findings attached.

## 9. Owner interaction

The owner should never have to go and work out what is being asked.

Bot-created issues do not subscribe the owner to notifications. So every question round, spec, plan, human task and new digest item @mentions the users in `notify` (section 13).

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

Rules:

- Each question stands alone, lists what it blocks, and offers 2 to 4 options with the recommended option first and marked.
- Free-text answers are always allowed.
- Questions wait for an answer indefinitely. There is no default-on-silence.
- The interviewer asks at most `max_questions_per_round` (default 7) per round, most blocking first.

### 9.2 Commands

| Command | Where | Effect |
|---|---|---|
| `1a 2c 3: text` | Issue | Answers the latest question round |
| 👍 on a revision comment | Issue | Approves that revision (7.4) |
| `/approve` | Issue | Approves the current spec or plan revision |
| Any comment that is not a command | Issue in `spec-review` or `plan-review` | Sends the spec or plan back, with the comment as the notes (7.4) |
| `/revise <text>` | Issue | Sends the spec or plan back with the owner's notes |
| 👍 on the issue | Human task from the planner | Owner finished it (9.3) |
| `/done` | `needs-human` issue | Owner finished the human task (9.3), or wants a closed PR's task started over |
| `/hold`, `/resume` | Any issue | Pauses or resumes the issue and its descendants |
| `/priority p1` | Any issue | Sets priority |

The daemon reacts 👍 to each command comment it has applied, and replies with the reason to each one it rejects. GitHub has no ✅ reaction. A reaction command is acknowledged by the transition comment (5.2).

PR reviews use GitHub's own review flow.

### 9.3 Human tasks

A task reaches `stage:needs-human` in one of two ways:

- **Blocked work.** A task needs an owner action: the implementer reports it, the retry cap is reached (8.3), or its PR was closed without merging. On `/done` the task returns to `stage:ready`.
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

- The body is the current list: open questions, pending approvals, human tasks, PRs waiting for review, and state errors (5.1, 7.2). Items are ordered by how many tasks each one unblocks.
- Each item states what is being decided, the options, the recommendation, and links to the issue.
- The daemon rewrites the body when the list changes. It posts a short comment that @mentions `notify` only when a new item appears, so GitHub sends one notification per new item.

### 9.5 Escalation

Deterministic rules decide what needs the owner. An LLM may add items to the escalation list. It never removes one.

Escalation triggers come from config (`escalate` in section 13), for example changes to dependency manifests, migrations, deployment files or payment code. A triggered PR gets the `review:high-risk` label and a "Look at these first" section listing the matched files and rules.

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

### 11.1 Spec (issue body after spec-review)

```
## Problem
## Goals
## Out of scope
## Users and flows
## Constraints
## Acceptance criteria
- [ ] observable, testable statements
## Decisions
- Q2.1 → c (owner, round 2)
```

### 11.2 Task

```
**Parent:** #N   **Scope:** scope:booking   **Priority:** p2
## Context
What exists today and which files matter.
## Change
What to build.
## Acceptance criteria
- [ ] ...
## Definition of done
- [ ] Tests added or updated for every changed behavior
- [ ] verify.commands pass
- [ ] No new skip markers
## Test plan
Which tests prove each criterion.
```

### 11.3 Planning rules

- An epic plan contains either stories, with acceptance criteria and no tasks, or tasks, when the epic has a single user-facing outcome. A story plan contains tasks.
- At most `plan.max_tasks` (default 10) tasks per plan. Larger work becomes more stories.
- Each task maps to one scope label from the allowed list, and each task other than a human task maps to one PR.
- Each task should fit within `verify.max_diff_lines`.
- Prefer extending existing code over adding new modules; the plan names the existing files each task touches.
- A scope outside the allowed list becomes a question to the owner.
- Work only the owner can do is a task marked `human`, written in the template from section 9.3 rather than 11.2.

The daemon validates the plan before it moves to plan-review:

- schema
- the scope list
- task counts
- the dependency graph has no cycles, including `depends-on` links to other repos
- no parent would exceed 100 sub-issues, and no issue would exceed 50 links of one dependency type

### 11.4 Plan

One comment, which is also the revision comment (section 7.4):

```
### Plan for #N (rev 1)

One line on why this plan has stories or tasks.

**S1. Title**
Outcome: one sentence
- [ ] acceptance criterion
Depends on: nothing

**T1. Title** · scope:<name> · human (only for a human task)
- Change: what to build, and the existing files it touches
- Acceptance: observable, testable statements
Depends on: S1, or nothing

**Order:** S1 → S2 and S3 in parallel → S4

Validation (11.3): item count, structure, scopes, no cycles.

**To approve this plan, react 👍 to this comment.** Any other comment sends it back for revision.
```

A plan contains either stories or tasks (11.3); the template shows both forms. Item IDs (`S1`, `T1`) are the stable IDs used in `origin` markers (section 5.2).

### 11.5 Pull request

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

- **Stage prompts.** These live in `.issuedog/prompts/{interviewer,planner,implementer,verifier}.md`. issuedog ships defaults, and a file on `main` overrides the default (section 3.4).
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
stages:
  timeouts: { interviewer: 10m, planner: 20m, implementer: 60m, verifier: 30m }
  max_turns: { interviewer: 30, planner: 60, implementer: 200, verifier: 80 }
  max_budget_usd: {}      # optional, per stage
loop:
  idle_sleep: 60s
  max_attempts: 3
  max_questions_per_round: 7
network:
  allow: [api.anthropic.com, proxy.golang.org, sum.golang.org]
usage_limit:
  pattern: "(?i)(usage|session) limit"
cross_repo:
  allowed: [mertenvg/dmcn, dmcnp/open-dmcn]
```

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
2. **Plan.** Rulesets work on public repos on GitHub Free, and on private repos only with Pro, Team or Enterprise. Code owners on private repos also need Pro.
3. **Ruleset on `main`:**
   - require a pull request
   - require code-owner review, and dismiss stale approvals on new commits
   - require the `issuedog/verify` check with the issuedog App as the pinned source; leave "require branches to be up to date" off (section 10.4)
   - block force pushes
   - bypass list: Repository admin, "for pull requests only", so the owner can merge their own protected-path PRs; never the write role and never the App

   **Interim.** While the pipeline is run by hand with the owner's token, the owner authors every PR and cannot approve it. Until the App runs, the ruleset requires only a pull request and blocks force pushes; code-owner review and the check are added then.

4. **CODEOWNERS** including `/.github/CODEOWNERS`. The App is never a code owner.
5. **Labels.** `stage:*`, `priority:*`, `scope:*`, `approved`, `review:high-risk`, and `type:*` for personal-account repos. The daemon creates any that are missing on startup.
6. **Org repos.** An org owner creates the `Epic` and `Story` issue types.

## 16. Open items to confirm

- **Cross-owner dependencies.** Do native blocked-by links work across owners? One report says GitHub returns 422. If they do work, they could replace the `depends-on` marker.
- **Pinning the check source.** The ruleset can only pin the App as the source of `issuedog/verify` once the App "has recently submitted a check run". Setup must post one bootstrap check run before adding the rule.
- **Pinning the digest.** The permission `pinIssue` needs from an App is not documented. Test it with Issues write.
- **`--json-schema` in long runs.** It must work together with `--dangerously-skip-permissions` and long tool loops. Also confirm how many structured-output retries are allowed.
- **Where a plan's hash lives.** A plan comment is also its revision comment (section 7.4), so its hash can't be written inside it without changing it. Decide whether it goes in a separate transition comment or is derived from the comment's edit history.
- **Running a Done-when check.** The **Done when** text of a human task is written by an agent. Decide how the daemon runs an automatable check (which uid, which credential) without breaking sections 3.1 and 3.2.
- **Approving PR reviews with comments.** Section 7.2 covers reviews with comments and approving reviews with none. An approving review that has comments is not yet covered.
- **Usage-limit text.** The text `-p` prints when a subscription limit is hit is undocumented, so `usage_limit.pattern` must be checked against a real occurrence.
- **`--bare` becoming the default.** Anthropic plans to make `--bare` the default for `-p`. `--bare` ignores `CLAUDE_CODE_OAUTH_TOKEN` and skips project agents and skills. Pin the Claude Code version, and re-test before upgrading.
- **Egress firewall on macOS.** The owner-matched firewall rule (section 3.1) needs `NET_ADMIN` at container start. Confirm it works on Docker Desktop for Mac.
- **Rumi Black's plan.** Confirm its visibility and plan meet the ruleset requirement in section 15.
- **Public release.**
  - Anthropic does not allow third-party tools to offer claude.ai login or route requests through users' subscriptions without prior approval. The README should therefore require users to bring their own `ANTHROPIC_API_KEY`.
  - Ask Anthropic before supporting subscription tokens in a distributed build.
  - The product name must not include "Claude Code". Plain-text "runs Claude Code" is fine.
- **Max plan limits.** Limits for sustained unattended use on the owner's own Max plan are undocumented. Anthropic's usage policy assumes "ordinary, individual usage".

## 17. Build phases

Rumi Black first; DMCN after phase 3.

1. GitHub App setup, daemon skeleton, Go GitHub client, sandbox uids and egress rule, trust checks, label and marker reading rules, transition table, leases, intake → clarifying → spec-review, digest. GitHub interactions are tested against `httptest` fakes only, and passing them is the acceptance bar.
2. Planning, plan review, human tasks from the planner, idempotent sub-issue creation, dependencies, scopes, priority.
3. Implement, bundle-based git handoff, deterministic checks, LLM verifier, retries, PR, `issuedog/verify` check, PR reconciliation.
4. Story verification, escalation rules, cross-repo issues, DMCN onboarding, domain agents.

## 18. Prior art

- CCPM (automazeio/ccpm, MIT): templates in section 11 are adapted from it.
- agent-foreman (simplerjiang/agent-foreman, MIT): ideas for deterministic escalation, fail-closed review and one-tap decisions. No code copied.
- foreman (bayological/foreman, no license): ideas only, including the explicit transition table and feeding the last failure into retries. No code copied.
