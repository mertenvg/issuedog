# issuedog specification

Status: draft 1, 2026-10-10

`issuedog` turns rough GitHub issues into reviewed pull requests. It asks the questions needed to write a spec, breaks the spec into ordered tasks, implements each task with headless Claude Code inside a Docker sandbox, verifies the result with real test runs and an independent reviewer, and opens a PR against a protected `main`. A human merges.

## 1. Goals

- Keep work moving on a repo while the owner is away, one task at a time.
- Make the owner's part fast: every question, decision and human task is self-contained and answerable from the GitHub mobile app.
- Treat automated testing and change verification as the first priority of every change.
- Use GitHub as the only control channel and the only durable state.

## 2. Non-goals (for now)

- Running on GitHub-hosted runners or consuming GitHub Actions minutes.
- Parallel tasks within one repo.
- Merging PRs. Merging is always a human action.
- Answering on the owner's behalf. A question waits for an answer indefinitely.

## 3. Architecture

```
Mac host
├── container: issuedog-rumi-black   (one per repo)
│   ├── issuedog daemon (Go, uid issuedog)   holds the GitHub bot token
│   ├── claude -p (uid agent)                one process per stage run
│   └── /work/<repo> checkout + per-task worktree
└── container: issuedog-dmcn
```

- One container per repo, so a failure in one repo leaves the others running.
- The daemon polls GitHub with `gh` (v2.94.0 or later, for sub-issue and dependency flags).
- The daemon starts a fresh `claude -p` process for every stage run, including every retry. Sessions are never resumed, so each run starts from the issue and the code, and the verifier never shares context with the implementer.
- The base image follows the existing sandbox setup: `~/.claude/CLAUDE.md` bind-mounted read-only, `--dangerously-skip-permissions` inside the container, feature branch per task.

### 3.1 Who writes to GitHub

Only the daemon writes to GitHub. Agents return structured output and the daemon validates and applies it.

- The bot token lives in a file readable only by the `issuedog` uid. Claude runs as the `agent` uid with `GH_TOKEN` and `GITHUB_TOKEN` removed from its environment, so it cannot read the token through `/proc`.
- Agents commit to the task worktree. The daemon pushes the branch and opens the PR.
- Agents read issue context from files the daemon writes into the worktree (`.issuedog/context/`), so the daemon controls which comments an agent sees (see section 6).

This keeps prompt-injected text from reaching GitHub directly and makes every write retry-safe.

### 3.2 Claude invocation

```
claude -p "$(cat stage-prompt)" \
  --output-format json \
  --dangerously-skip-permissions \
  --append-system-prompt "$(cat .issuedog/prompts/<stage>.md)"
```

- The daemon parses the final `result` from the JSON output.
- Each stage writes its machine-readable output to `.issuedog/out/<stage>.json`. The daemon validates it against the stage schema. Missing or invalid output counts as a failed run (fail closed).
- Each stage has a timeout from config. A timeout counts as a failed run.
- When Claude reports a usage limit, the daemon sleeps until the reported reset time and retries the same run. Usage-limit sleeps do not count toward the retry cap.

## 4. Work item types

| Type | Meaning | Created by |
|---|---|---|
| `epic` | A rough idea or large feature | Owner |
| `story` | One user-visible outcome with acceptance criteria | Planner |
| `task` | One PR, one scope | Planner |
| `bug` | Something broken | Owner, or verifier (story checks) |

- Repos in an organization use GitHub issue types. Repos under a personal account use `type:epic`, `type:story`, `type:task`, `type:bug` labels. Config selects which.
- Hierarchy uses native sub-issues: epic → story → task, or epic → task.
- The planner skips the story level when an epic has a single user-facing outcome.
- A `bug` goes through clarifying and then becomes a single task. It skips spec review and planning. A bug filed against earlier work starts its body with `Follow-up to #N` and includes the acceptance criterion "the original behavior of #N is unaffected."

## 5. State

GitHub holds all durable state:

| Concern | Where it lives |
|---|---|
| Stage | `stage:*` label (exactly one per open issue) |
| Type | Issue type, or `type:*` label |
| Hierarchy | Native sub-issues |
| Dependencies | Native blocked-by / blocking |
| Priority | `priority:p0` to `priority:p3` (default `p2`) |
| Scope | `scope:<name>` from the repo's allowed list |
| Transition history | Comments carrying a hidden marker, e.g. `<!-- issuedog:transition from=clarifying to=spec-review rev=3 run=8f2c -->` |
| Attempt count | Hidden marker on the latest verifier comment |

The only local state is the lease file (section 8.2). Losing the container loses nothing except the current run.

Every transition goes through one table in code (section 7). Applying a transition whose marker already exists is a no-op, so a crashed run can be replayed safely.

## 6. Trust model

### 6.1 Trusted people

A GitHub user is trusted when either holds:

1. They are listed in `CODEOWNERS` as read from `main`. The daemon never reads `CODEOWNERS` from a working branch.
2. No `CODEOWNERS` file exists on `main`, and the user has `write`, `maintain` or `admin` permission on the repo (`gh api repos/{owner}/{repo}/collaborators/{user}/permission`).

Team entries (`@org/team`) are resolved through the API. The bot account is never trusted, even if listed.

### 6.2 What the daemon acts on

- An issue enters the pipeline when a trusted user opened it, or when a trusted user adds the `approved` label.
- Only comments by trusted users count as answers, approvals or commands. Agents receive other comments in a separate, clearly labeled "untrusted input" section of their context files, and only for epics and stories.
- A comment edited after the daemon read it is re-read. A command counts once, keyed by comment ID.

### 6.3 Protecting the trust inputs

- `CODEOWNERS` lists itself (`/.github/CODEOWNERS @owner`).
- A ruleset requires code-owner review on `main`.
- Any diff that touches `CODEOWNERS`, `.github/**`, `.issuedog/**` or `.claude/**` fails deterministic verification (section 10.1) and needs a human-authored change instead.

## 7. Pipeline

### 7.1 Stages

| Label | Agent | Output |
|---|---|---|
| `stage:intake` | none | Owner's rough idea |
| `stage:clarifying` | interviewer | Numbered questions (section 9.1) |
| `stage:spec-review` | interviewer | Spec written into the issue body (section 11.1) |
| `stage:planning` | planner | Proposed stories and tasks in one comment |
| `stage:plan-review` | none | Waiting for owner approval of the plan |
| `stage:planned` | none | Parent of created sub-issues |
| `stage:ready` | none | Task waiting for the scheduler |
| `stage:in-progress` | implementer | Commits in the task worktree |
| `stage:verifying` | verifier | Pass, or findings |
| `stage:pr-open` | none | PR waiting for human review |
| `stage:needs-human` | none | Task only the owner can do (section 9.3) |
| `stage:held` | none | Paused by the owner |

### 7.2 Transitions

| From | To | Trigger |
|---|---|---|
| intake | clarifying | Issue is trusted (6.2) |
| clarifying | clarifying | Owner answered; interviewer has more questions |
| clarifying | spec-review | Interviewer has no open questions; spec written |
| spec-review | clarifying | Owner `/revise <text>` |
| spec-review | planning | Owner `/approve` on the current spec revision |
| planning | plan-review | Planner output passes validation (11.3) |
| plan-review | planning | Owner `/revise <text>` |
| plan-review | planned | Owner `/approve`; daemon creates sub-issues and dependencies |
| ready | in-progress | Scheduler picks the task (8.1) |
| in-progress | clarifying | Implementer needs an answer; worker is released |
| in-progress | verifying | Implementer exits with commits and valid output |
| in-progress | needs-human | Implementer reports the work needs an owner action |
| verifying | in-progress | Findings, attempt < cap |
| verifying | needs-human | Findings, attempt = cap |
| verifying | pr-open | Pass; daemon pushes, opens PR, posts `issuedog/verify` success |
| pr-open | in-progress | Trusted reviewer requests changes on the PR |
| pr-open | closed | PR merged (`Closes #N`) |
| needs-human | ready | Owner `/done` |
| any open | held | Owner `/hold` |
| held | previous | Owner `/resume` |

Any transition missing from this table is an error. The daemon logs it, leaves the issue unchanged and lists it in the digest.

Bug issues use intake → clarifying → ready. When the interviewer finishes clarifying a bug, the daemon converts the bug into a task-shaped body (section 11.2) and labels it `stage:ready`.

### 7.3 Story and epic completion

- When every child of a story is closed, the daemon runs the story verifier on `main` (section 10.3). Failures create `bug` sub-issues under the story. Passing closes the story.
- An epic closes when every child is closed.

### 7.4 Approvals bind to a revision

- `/approve` on a spec or plan applies to the revision shown in the latest transition marker (`rev=N`). If the body changed after the owner read it, the daemon asks again.
- PR approval relies on GitHub: the ruleset dismisses stale approvals when new commits are pushed, and `issuedog/verify` is posted for an exact head SHA. A new push triggers a new verify run.

## 8. Scheduling

### 8.1 Picking work

Each loop iteration:

1. Process new trusted comments and labels on open issues (answers, `/approve`, `/revise`, `/done`, `/hold`, `/resume`, PR reviews).
2. Run any pending non-task stage work (clarifying, spec, planning) for the highest-priority issue that has it.
3. Otherwise pick the next task: `stage:ready`, not `is:blocked`, ordered by priority, then by parent's priority, then by issue number.
4. If nothing is available, sleep `idle_sleep` (default 60s).

Only one task is `stage:in-progress` at a time. A task waiting on the owner releases the worker, and the scheduler moves to the next independent task.

### 8.2 Leases and restart

- On pickup the daemon assigns the issue to the bot and writes `/state/lease.json` (write to a temp file, then rename) with the issue number, stage, run ID, worktree path and start time.
- On startup the daemon reads the lease. If the run's output file exists and validates, it applies the transition. Otherwise it discards the worktree changes, resets the issue to its pre-run stage and starts the run again.

### 8.3 Retries

- The verify loop allows `max_attempts` (default 3) implementer runs per task.
- Each retry prompt includes the previous verifier findings verbatim.
- After the last failed attempt the task moves to `stage:needs-human` with the findings attached.

## 9. Owner interaction

The owner should never have to go and work out what is being asked.

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
| `/approve` | Issue | Approves the current spec or plan revision |
| `/revise <text>` | Issue | Sends the spec or plan back with the owner's notes |
| `/done` | `needs-human` issue | Owner finished the human task |
| `/hold`, `/resume` | Any issue | Pauses or resumes the issue and its children |
| `/priority p1` | Any issue | Sets priority |

PR reviews use GitHub's own review flow.

### 9.3 Human tasks

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

One pinned issue per repo, `issuedog digest`.

- The body is the current list: open questions, pending approvals, human tasks and PRs waiting for review, ordered by how many tasks each one unblocks.
- Each item states what is being decided, the options, the recommendation, and links to the issue.
- The daemon rewrites the body when the list changes, and posts a short comment only when a new item appears, so GitHub sends one notification per new item.

### 9.5 Escalation

Deterministic rules decide what needs the owner. An LLM may add items to the escalation list. It never removes one.

Escalation triggers come from config (`escalate` in section 13), for example changes to dependency manifests, migrations, deployment files or payment code. A triggered PR gets the `review:high-risk` label and a "Look at these first" section listing the matched files and rules.

## 10. Verification

Verification is the gate between implementation and a PR.

### 10.1 Deterministic checks (run first)

The daemon runs these itself, outside Claude, and uses exit codes only:

1. Every command in `verify.commands` (tests, lint, vet, build) passes.
2. The diff touches no protected path (section 6.3).
3. No test file is deleted, and no new skip markers (`t.Skip`, `.skip(`, `xit(`, `#[ignore]`, configurable) are added.
4. Lint and test configuration is unchanged unless the task's scope is `scope:tooling`.
5. When the change modifies non-test source files, it also adds or modifies test files.

Any failure is a verify failure, with the command output attached as findings. The LLM verifier cannot override a deterministic failure.

### 10.2 LLM verifier (task level)

Runs only after the deterministic checks pass, in a fresh process, with:

- the task's acceptance criteria and definition of done
- the full diff against the base branch
- the deterministic check output

It returns `pass` or `request_changes` with findings tied to acceptance criteria. Fail-closed rules:

- Unparseable or missing verdict → `request_changes`.
- A diff larger than `verify.max_diff_lines` → `request_changes` with the finding "task too large, split it."
- The verifier cannot see the full diff for any reason → `request_changes`.

### 10.3 Story verification

When every task under a story has merged, the daemon runs `verify.story_commands` (e2e suites) on `main` and the LLM verifier checks the story's acceptance criteria against the e2e results. Failures become `bug` sub-issues.

### 10.4 Commit status

On pass the daemon posts `issuedog/verify` = `success` for the branch head SHA, with a short summary and a link to the verifier comment. The `main` ruleset requires this status. Whether a ruleset accepts a status posted by an external app with no Actions run needs confirming (section 16).

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

- At most `plan.max_tasks` (default 10) tasks per plan. Larger work becomes more stories.
- Each task maps to one PR and one scope label from the allowed list.
- Each task should fit within `verify.max_diff_lines`.
- Prefer extending existing code over adding new modules; the plan names the existing files each task touches.
- The planner checks the dependency graph for cycles, including cross-repo links, before output is accepted.
- A scope outside the allowed list becomes a question to the owner.

### 11.4 Pull request

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

- Stage prompts: `.issuedog/prompts/{interviewer,planner,implementer,verifier}.md`. issuedog ships defaults; a repo file overrides the default.
- Domain subagents: `.claude/agents/*.md`, available to the implementer and verifier during a run. Add one only when a domain has rules the model keeps getting wrong.
- Skills: `.claude/skills/*`, for repeatable procedures such as adding a migration.

## 13. Repo configuration

`.issuedog/config.yaml`:

```yaml
types: issue-types        # or: labels
scopes: [booking, payments, drivers, notifications, infra, tooling]
plan:
  max_tasks: 10
  review: true
verify:
  commands:
    - go vet ./...
    - golangci-lint run
    - go test ./...
  story_commands:
    - make e2e
  max_diff_lines: 400
  skip_markers: ["t.Skip(", ".skip(", "xit(", "#[ignore]"]
escalate:
  paths: ["go.mod", "**/migrations/**", "deploy/**", "**/payments/**"]
stages:
  timeouts: { interviewer: 10m, planner: 20m, implementer: 60m, verifier: 30m }
loop:
  idle_sleep: 60s
  max_attempts: 3
  max_questions_per_round: 7
cross_repo:
  allowed: [mertenvg/dmcn, dmcnp/open-dmcn]
```

Changes to this file go through a human-authored PR (section 6.3).

## 14. Cross-repo work

- A planner may propose an issue in another repo listed in `cross_repo.allowed`.
- The daemon creates it with a link back to the source issue and labels it `stage:intake`. It carries no `approved` label, so the target repo's daemon waits for a trusted person there to approve it.
- The daemon links the dependency with `gh issue edit <N> --add-blocked-by <url>`.
- The bot token needs issue write access on every repo in the allowed list.

## 15. GitHub setup per repo

1. Machine-user account for the bot with a fine-grained token: contents write, issues write, pull requests write, commit statuses write. No admin.
2. Ruleset on `main`: require PR, require `issuedog/verify`, require code-owner review, dismiss stale approvals on new commits, block force pushes, no bypass for the bot.
3. `CODEOWNERS` including `/.github/CODEOWNERS`.
4. Labels: `stage:*`, `priority:*`, `scope:*`, `approved`, `review:high-risk`, and `type:*` for personal-account repos. The daemon creates any that are missing on startup.

## 16. Open items to confirm

- Cross-repo blocked-by: `gh` accepts issue URLs for `--blocked-by`; confirm it links across repos and owners.
- A ruleset requiring `issuedog/verify` accepts a status posted through the API with no Actions run.
- Structured output: whether `claude -p` supports a JSON-schema flag, which would replace the output-file contract in 3.2.
- Public release: Anthropic's branding rules and whether a public tool may run on users' Claude subscriptions. The README should say users bring their own Claude Code login or API key.
- Max plan limits for sustained unattended use.

## 17. Build phases

Rumi Black first; DMCN after phase 3.

1. Daemon skeleton, trust checks, transition table, leases, intake → clarifying → spec-review, digest.
2. Planning, plan review, sub-issues, dependencies, scopes, priority.
3. Implement, deterministic checks, LLM verifier, retries, PR, `issuedog/verify` status.
4. Story verification, escalation rules, cross-repo issues, DMCN onboarding, domain agents.

## 18. Prior art

- CCPM (automazeio/ccpm, MIT): templates in section 11 are adapted from it.
- agent-foreman (simplerjiang/agent-foreman, MIT): ideas for deterministic escalation, fail-closed review and one-tap decisions. No code copied.
- foreman (bayological/foreman, no license): ideas only, including the explicit transition table and feeding the last failure into retries. No code copied.
