# SST3 Solo Workflow

## 5-Stage Solo Workflow Model

**Your Role**: Orchestrate research/review via subagent swarms; implement directly. See `../dotfiles/SST3/workflow/WORKFLOW.md` for full 5-stage workflow.

**Default: PLANNING MODE** — execute only when user says "work on #X" / "implement this". No file changes, no commits in planning mode. When unclear, ask.

**MANDATORY READING**:
1. `../dotfiles/SST3/standards/STANDARDS.md` (ALWAYS)
2. `../dotfiles/SST3/standards/ANTI-PATTERNS.md` (ALWAYS — the documented failure modes you must not repeat)
3. `{repository-name}/CLAUDE.md` (ALWAYS - replace with repo root)

**Reading Confirmation Checklist** (MUST display and complete):
- [ ] Read STANDARDS.md
- [ ] Read ANTI-PATTERNS.md
- [ ] Read {repository-name}/CLAUDE.md

**Critical behavioural rules** (full detail in STANDARDS.md + ANTI-PATTERNS.md):
- **GREP BEFORE WRITING/CODING**: before creating ANY new file, rule, memory, helper, hook, harness, function, class, component, workflow, process, design, or piece of logic — grep relevant directories with multiple synonyms. Update existing in place if found. New files only after grep confirms nothing exists. (AP #10)
- **MULTI-LAYER SUBAGENT DISCIPLINE** (AP #14): never stingy. Subagent count is DYNAMIC, scaled to cover every directory / file / claim category line-by-line — no stone left unturned. NOT 2-3 as a default. If the work has 12 claim categories, dispatch ≥12 subagents. Use LAYERS cross-checking each other from DIFFERENT angles (layer 2 ≠ layer 1 prompt). Main agent VERIFIES every subagent finding against source — never assume the subagent got it right. Every claim must be factually provable AND the proof method must be documented inline so future audits don't false-positive on it.
- **AP #9 Single-Source Edits**: every edit to a multi-research artefact must integrate ALL relevant sources in the same pass. Never apply one in isolation.
- **AP #11 Stopping vs Applying**: when an audit surfaces a documented violation, RUN the full process (false-positive sweep then apply). Don't stop to ask permission for fixes the standards already mandate. Don't apply without the sweep.
- **AP #12 No Observability**: every component needs structured logs, metrics, and audit trails AT WRITE TIME. Not after the first incident.
- **AP #13 "Proceed" ≠ "Bypass Process"**: when the user says okay / proceed / yes / go ahead, that means **proceed using the full standard process** — not skip the sweeps, gates, Ralph reviews, or guardrails. User authorisation never bypasses workflow.
- **AP #17 Keep Going Until Done**: do NOT stop mid-work to ask permission, wait for user confirmation, or "check in". Phase checkpoints post a comment to the Issue and CONTINUE. Stop ONLY for: (a) context approaching ~50% remaining (~500K of 1M / ~100K of 200K) — on long/multi-phase work the agent AUTO-runs `/handover` + compact + CONTINUE (does not wait for the operator); never operate below 50% remaining; near-completion exemption: ~2-3 turns from done at ~51% → finish, (b) irreversible destructive action needing user consent (force-push, rm -rf, DROP TABLE, branch deletion), (c) genuinely stuck after investigation (not a first-response-to-friction reflex), (d) task complete. Compaction is a CONTINUATION mechanism, not premature stopping.
- **AP #16 Monitor, Don't Fire-and-Forget**: every script / command / subprocess / test / deployment / commit / push you launch must be verified end-to-end (tail logs, check exit code, verify output, confirm side effects). "Started" is not "done". For `run_in_background`, use Monitor or read its output file. Be the user's eyes and ears, not just their executioner. If you cannot answer "what happened?" with specifics, you fired and forgot — go check NOW.
- **AP #18 Sample Invocation Validates Workflow**: for any change touching pipeline / backtest / SL1 / SL2 / orchestration / CLI-wiring / cross-module function-arg propagation — run an actual end-to-end sample invocation (real CLI, real DB, small liquid basket 8 tickers) BEFORE closing. Unit + smoke tests are necessary but NOT sufficient. Mocks that accept `**kwargs` silently discard params and do NOT prove propagation — assert `call_args.kwargs[...]` explicitly. Stage 4 Verification Loop mandatory gate. See STANDARDS.md "Testing Priority — Workflow Validation Gate".
- **Per-Stage Feedback Capture** (canonical: STANDARDS.md §Per-Stage Feedback Capture). Write `dotfiles/SST3-metrics/leader-feedback/feedback-<repo>-<issue>.md` `## Stage N` block at each `/Leader` stage close. For a NEW file, copy the write-time template `dotfiles/SST3/templates/leader-feedback-template.md` (canonical frontmatter + `## Stage N — <Title>` H2 headings + 10 fields) — never hand-roll the structure; a bare `## Stage N` heading is rejected by the strict parser (the dotfiles#486/#488 contention class). 10 fields per stage (model / worked / didnt / why / improvement / improvement_status / evidence / friction / rule_self_caught / rule_user_caught). Channel rule (forward-preference-blocklist enforced via pre-commit hook `sst3-metrics-feedback-present`): feedback files MUST NOT contain `prefers / always / from now on / default ON / going forward` phrasing — that's auto-memory's channel; attribution wording (`operator flagged`, `user pointed out`) is FINE.

**STOP if**: No GitHub Issue exists. Create Issue using `../dotfiles/SST3/templates/issue-template.md`.

### Solo Workflow Overview

**Context**: read the injected `SST3 CONTEXT GAUGE` line; `/clear` between unrelated tasks
**Content Budget**: `/Leader` stages load per-stage subsets via `load-stage-rules.sh`
**Handover at**: ~50% remaining (~500K of 1M / ~100K of 200K) — on long/multi-phase work the agent AUTO-runs `/handover` + compact + CONTINUE; never operate below 50% remaining (near-completion exemption: ~2-3 turns from done at ~51% → finish).
**Issue Header**: `## Solo Assignment (SST3 Automated)`
**Branch**: `solo/issue-{number}-{description}` (commit per file, no PR)
**Merge**: Direct merge to main after Ralph Review passes (BEFORE user review - protects work)

### Execution Guardrails (Built-in)

Pre-start read (CLAUDE.md + STANDARDS.md + Issue) → phase checkpoints (on long work, auto-`/handover` + compact + continue when remaining nears ~50%; never below 50% remaining) → post-compact re-read → verification loop until clean → user-review-checklist.md.

### Branch Safety (CRITICAL — DO NOT VIOLATE)

**Worktree-per-agent is the canonical Stage-4 isolation model (dotfiles#488 Fix-A).** A git clone has exactly one working dir, one HEAD, one index — so a second concurrent agent's `git checkout -b` moves the first agent's HEAD and muddles its implementation. Before any code edit, a Stage-4 implementing agent MUST **work in a worktree**: call the `EnterWorktree` tool (named `solo/issue-{N}-{desc}`) to get an isolated worktree on its own solo branch, instead of a bare `git checkout -b solo/...` in the shared clone. This instruction lives HERE (CLAUDE.md / memory) because the `EnterWorktree` tool only activates when "worktree" is explicitly directed by the user **or in CLAUDE.md / memory** — a slash-command-only instruction would leave the tool inert. `.claude/commands/Leader.md` Stage-4 step 1 and `.claude/commands/SST3-solo.md` "Before Starting Work" reference this anchor. **Worktree dep setup (dotfiles#516 AC 4.2):** immediately after EnterWorktree, run `scripts/setup-worktree-deps.sh` to symlink `.venv` / `node_modules` from the parent clone and copy a gitignored `.env` (else first-commit hooks fail), and use worktree-relative paths for all file ops — canonical-clone absolute paths silently no-op when CWD is in the worktree.

- **NEVER switch branches** (`git checkout main`, `git checkout -b`, `git switch`) — this remains the in-worktree invariant: it is correct *inside* an isolated worktree (commit + push to that worktree's solo branch only).
- **Always commit and push to the CURRENT worktree's solo branch** — it gets merged later via the recursion-safe remote fast-forward procedure (NEVER a shared-tree `git checkout main` — see Leader.md Gate 2 / AC 1.3).
- If you need something from main, **ask the user** — do NOT switch yourself.
- The only branch creation is the `EnterWorktree` solo branch at the START of work; `ExitWorktree action:keep` until the push is confirmed landed, then `action:remove`.
- **Runtime backstop (dotfiles#490)**: a Claude Code PreToolUse hook — canonical `claude/hooks/sst3-branch-guard.sh`, installed user-scope by `scripts/install.sh`, wired in `claude/settings.json` `hooks.PreToolUse` — deterministically intercepts a Bash branch *switch-to-existing / non-`solo/*` create* **before it executes**, so this rule is no longer prose-only (STANDARDS.md:30 "not honor system"). WARN by default (advisory, fully reversible: remove the `hooks` block or set `disableAllHooks`); one config flip `SST3_BRANCH_GUARD_MODE=DENY` hard-blocks (exit 2, overrides `permissions.allow`). Tests: `claude/hooks/tests/test_branch_guard.sh`.

### Command Interface

- `/start` — list repos, prompt selection, load CLAUDE.md, WAIT for task.
- `/SST3-solo` — load STANDARDS.md + repo CLAUDE.md, display summary, prompt for task, execute with guardrails.
- `/handover` — pre-compact: write a structured AI-to-AI handover to `~/handover/handover_<slug>_<date>.md` + point `~/handover/current-task-<repo>.txt` at it (repo-scoped since dotfiles#568 — one pointer per repository, so concurrent sessions in different repos never read each other's), so post-compact resume loses no context (the `~/handover` dir survives compaction AND a WSL VM reboot, and is auto-pruned after 7 days). Handovers are ephemeral resume aids — NOT auto-memory (durable lessons go to `feedback_*`/`project_*` memories instead).

Handover template: `../dotfiles/SST3/templates/chat-handover.md` (post checkpoint to Issue FIRST).

## External Research References

**Location**: `docs/research/` in project root
**Check BEFORE external research**: Existing research references
**Capture AFTER research**: If 3+ external resources found, create/update research reference
See: `../dotfiles/SST3/reference/research-reference-guide.md` for complete guide

## Quality Standards

**See STANDARDS.md** — Never Assume (read source before concluding), Fix Everything (no scope/language excuses, no priority deferrals), Critical Thinking (challenge with evidence). Only valid skip reason: confirmed false positive (document why).

**Voice Content Protection** — when editing operator-voice prose (CV, LinkedIn, cover letters, blogs): wrap in `<!-- iamhoi --> ... <!-- iamhoiend -->`. Canonical rules in `../dotfiles/SST3/standards/STANDARDS.md` "Voice Content Protection" + AP #15. Single source of truth for banned words: `../dotfiles/SST3/scripts/voice_rules.py`. (#406 F3.8 dedup.)

## Ralph Review Loop (MANDATORY)

**Subagents are PLANNING ONLY** - they review, they do NOT write code.

**Flow**: Implement → Haiku → Sonnet → Opus (up to 3 restarts, then ONE escalation, then ONE final loop, then stop-and-report) → **Merge to main** → User Review

| Tier | Model | Purpose | Invocation |
|------|-------|---------|------------|
| 1 | `haiku` (MANDATORY) | Surface checks | `Agent(model=haiku, subagent_type=ralph-review, run_in_background=false, prompt="Review per SST3/ralph/haiku-review.md...")` |
| 2 | `sonnet` (MANDATORY) | Logic checks | `Agent(model=sonnet, subagent_type=ralph-review, run_in_background=false, prompt="Review per SST3/ralph/sonnet-review.md...")` |
| 3 | `opus` (MANDATORY) | Deep analysis | `Agent(model=opus, subagent_type=ralph-review, run_in_background=false, prompt="Review per SST3/ralph/opus-review.md...")` |

**On FAIL any tier**: Main agent fixes → Restart from Tier 1 (Haiku), up to 3 restarts. Restart 4: NOT taken — escalate to a class-sweep, then resume Ralph with the count reset to zero for exactly ONE further loop; if that loop does not PASS, STOP and report the outstanding findings + classes to the operator (terminal state — never a silent abandon). **Signal the counter at both boundaries — it cannot observe either for itself**: `--restart` when you restart, `--escalate` when you escalate (the only reset within a stage; `--stage5` opens Stage 5's own loop). Unsignalled the count stays 0 forever, so the bound is never reached.
**On PASS all 3**: Merge to main immediately (protects work), then user review

**Checklists**: `../dotfiles/SST3/ralph/`

## Quick Reference

### 5-Stage Workflow (ORDER-DEPENDENT — no skipping, no reordering)
```
Stage 1: Research — subagent swarm → main agent writes /tmp (findings + gaps + plan)
Stage 2: Issue Creation — main agent from /tmp, illustrations, phase checkpoints, quality mantras verbatim
Stage 3: Triple-Check — subagents verify scope vs audit = 100%, chat history, dead code
Stage 4: Implementation — main agent implements, Verification Loop, Ralph Review, merge, user-review-checklist
Stage 5: Post-Implementation Review — subagent swarm: wiring, goal alignment, quality scan, regression tests + completeness gate (Layer A pre-flight `bash $SST3/leader-stage5-completeness-check.sh <issue>` + Layer B post-flight failsafe `.github/workflows/stage5-completeness.yml`; both mandatory where Layer B EXISTS, and neither replaces the other; #460 W4. Layer-B coverage is three-state, not binary: `SST3/drift-manifest.json` `layer_b_coverage` records each repo as covered, `exempt` (no GitHub Actions surface exists to replay a gate on — structural and permanent) or `pending` (a workflow is expected but not yet landed). Derive the current states from that field; the repos are named there and in dotfiles#565, deliberately not here, because this line publishes verbatim to public consumers. It read "both mandatory" unconditionally until #565 round 11, which is false for an exempt repo and was propagated to every consumer including that one)
```

### Solo Execution Checklist (Stage 4)
```
## Working on Issue #X
Read CLAUDE.md, STANDARDS.md, Issue
Enter isolated worktree: call EnterWorktree tool named solo/issue-{X}-{description}
Execute phase 1, commit per file, push, post checkpoint
Execute phase 2, commit per file, push, post checkpoint
...
Run verification loop until clean (overengineering, reuse, duplication, fallbacks, wiring, regression, quality)
Run Ralph Review (Haiku → Sonnet → Opus)
Merge to main (BEFORE user review - protects work, check for conflicts first)
Post user-review-checklist.md (from TEMPLATE, ALL sections mandatory)
User reviews and approves
Post closing-summary comment (sentinel <!-- sst3-closing-summary --> on its own line; C18 gate — the final comment closing agents forget)
Cleanup branch, close Issue
```

### Emergency Procedures
- **Context overflow**: Create handover immediately
- **Stuck**: Re-read Issue, identify blocker, post to Issue
- **User compact**: Read the handover file in full (if `~/handover/current-task-<repo>.txt` present) + re-read the active `/Leader` stage section line-by-line + CLAUDE.md, STANDARDS.md, Issue last comment — a pre-compact read does not count; post-compact memory is diluted.

### MCP Configuration (Global)
- **Location**: `~/.claude.json` (user scope)
- **Verify**: Run `claude mcp list` or `/mcp` inside Claude Code
- **Servers**: chrome-devtools, github-checkbox, github
- **Wrapper-lane (Issue #445; #447 Phase 6+8 expansion)**: Stateless, request-scoped bash wrappers — no daemon, no SQLite, no persistent graph. Invoked via scripts in `dotfiles/SST3/scripts/`, family-prefixed plus cross-cutting. Family-prefixed: Phase A (code): `sst3-code-{status,update,search,callers,callers-transitive,callees,subclasses,impact,large,review,config,coverage,orphans,entry-points,untested-py,secrets,cross-lang,shell,recent-changes,at-ref}.sh`. Phase A-security: `sst3-sec-{subprocess,deserialize,secret-touchpoints,input-sources}.sh`. Phase A-dep: `sst3-dep-{list,usage,blast-radius,cve}.sh`. Phase B (doc): `sst3-doc-{lint,links,yaml,frontmatter,toc}.sh`. Phase C (sync): `sst3-sync-{related-code,tool-eviction,doc-to-code}.sh`. Cross-cutting: `sst3-check.sh` (Phase D Layer-2 orchestrator, also exposes the `/sync-check` skill) + `sst3-self-test.sh` (wrapper-lane regression gate) + `sst3-bash-utils.sh` (shared self-bootstrap helper) + `sst3-test-vacuity.sh` (pre-Ralph vacuous-assertion + allowlist-overreach GATE, #567). Inner engines: `ast-grep` + `ripgrep` + `git` + `coverage.py` + `jq` + `markdownlint-cli2` + `lychee` + `yamllint` + `shellcheck` + `python3` + `pip-audit` + `cargo audit` + `npm audit`. See `../dotfiles/docs/guides/code-query-playbook.md` for the operational guide. **Workflow wiring (#484 W6.1/W6.3)**: the CODE lane is `graph_applicable`-gated (code-SEED); the DOC lane (`sst3-doc-*`) is diff-triggered on `*.md`/frontmatter regardless of `graph_applicable` (Ralph haiku doc-lane checkbox + WORKFLOW.md Stage 1); the SYNC lane (`sst3-sync-*`) is diff-triggered on `docs/research/*` frontmatter changes (Ralph sonnet sync-lane checkbox). **SEC lane (`sst3-sec-*`) + DEP lane (`sst3-dep-*`) — WIRED, shape-gated (#507)**: security + dependency audit runs via `sst3-check.sh --sec/--dep --strict` (fail-loud — engine-missing → exit 2, never silent-clean). SEC (offline ast-grep, diff-scoped) fires in the Ralph **Sonnet** tier (net-new-finding gate) + a pre-commit hook (`sst3-sec`) + a pre-commit-framework `stages: [pre-push]` hook for operator/non-Claude commits; DEP-cve (network) fires in the `sec-dep-audit.yml` GHA job, NOT pre-commit. **Shape-gated** via `sst3_utils.sec_dep_applicable`: code-bearing shapes (Service / eBay-MCP / Config-heavy, plus mt5-ea SEC-only — Python production surface, no dependency manifest so DEP skip-clean is honest) run; non-code shapes skip-clean (no vacuous PASS); shapes whose production code the SEC wrappers cannot parse — GAS (`.gs`), lab-automation, a TypeScript-only service (`service-ts`, which still runs DEP) — skip SEC as could-not-look, named by `sst3_utils.sec_skip_reason`, never as clean. Doctrine: STANDARDS.md "Security & Dependency Audit Gate"; AP #27.
- **Guide**: `../dotfiles/docs/guides/mcp-configuration.md`
- **Tool Selection**: See `../dotfiles/SST3/reference/tool-selection-guide.md`

### MCP Tools
- **Checkboxes**: `mcp__github-checkbox__update_issue_checkbox(issue_number, checkbox_text, evidence)`
- **Frontend**: Chrome DevTools MCP — guide `../dotfiles/docs/guides/chrome-devtools-mcp.md`, screenshots → `../screenshots/`
- **Frontend fallback (no MCP)**: `playwright` Python lib — guide `../dotfiles/docs/guides/playwright-fallback.md`. Use when chrome-devtools MCP is disabled (operator may have it off by default) or its tools don't surface this session; or for cheap re-runnable AP #18 regression scripts
- **GitHub Issues**: issue_write, add_issue_comment, search_issues, get_file_contents, create_pull_request

### Google Drive Sync Conflicts
Edit fails with "File has been unexpectedly modified" → copy to `C:/temp/`, edit copy, copy back. See `../dotfiles/docs/guides/google-drive-sync.md`.

### Append vs Extend (authoring guidance for the project-specific section below)

> About to add a paragraph to `## Project-Specific Notes` (below the boundary) or to any other CLAUDE.md section? **First check whether an existing `docs/<area>.md` is the right home** — extend the destination and leave a one-line pointer here. Appending to CLAUDE.md is the **last resort**, not the default. (Rationale: every CLAUDE.md line costs tokens on every future session; one consumer's CLAUDE.md was trimmed 58k→28k after WHY-prose accumulation — the repo is named in dotfiles#565, not here, because this line sits ABOVE the boundary marker and is published verbatim to the public consumers. Full rule: `../dotfiles/SST3/standards/STANDARDS.md` "Append vs Extend Rule".)

---
<!-- ============================================================== -->
<!-- ⚠️ DO NOT MODIFY OR DELETE ANYTHING ABOVE THIS LINE ⚠️ -->
<!-- ============================================================== -->
<!-- All content ABOVE is SST3 standard managed by dotfiles issues -->
<!-- Modifications require dotfiles repository SST3 issue approval -->
<!-- Project-specific configuration begins BELOW this boundary -->
<!-- ============================================================== -->

# Project-Specific Configuration

## Project Overview

groupwarden is a self-hosted anti-spam moderator for WhatsApp Communities. It runs as a linked device on a dedicated number, reads every group it is in, and applies the deployer's combination rules (keywords plus link and other signals). A matched post is deleted, its sender removed and banned from every moderated group, and the admins are told in a private Telegram group. There are no warning tiers.

## Technology Stack

- Language: Go (version in `go.mod`)
- WhatsApp client: `go.mau.fi/whatsmeow`, pinned to one pseudo-version and only ever behind `internal/client`
- Storage: SQLite through `modernc.org/sqlite` (pure Go, no cgo)
- Config: YAML (`go.yaml.in/yaml/v3`, unknown keys rejected)

## Repository Structure
```
groupwarden/
├── cmd/groupwarden/        # CLI: pair, run, groups, resolve-link, check, healthcheck
├── internal/
│   ├── client/             # Adapter interface + types; whatsmeow/ is the only whatsmeow importer
│   ├── pipeline/           # durable inbox -> worker -> decision
│   ├── store/              # groupwarden.db: migrations, inbox, pauses, status
│   ├── app/                # supervisor: connect, backoff, lifecycle, health monitors
│   ├── config/             # typed config + secrets file
│   ├── alert/              # Alerter interface
│   └── mask/               # masks phone numbers, LIDs and group IDs in logs
├── scripts/                # vendored SST3 scripts (managed by propagation, never hand-edit)
└── .github/workflows/      # ci.yml + vendored scan workflows
```

## Development Setup
```bash
git clone https://github.com/hoiung/groupwarden.git
cd groupwarden
go build ./...
pre-commit install --hook-type pre-commit --hook-type pre-push --hook-type commit-msg
```

## Project Standards

### Code Quality
- Formatter: `gofmt`
- Linters: `go vet`, `staticcheck`, `gosec`, `govulncheck` (all run in `.github/workflows/ci.yml`)

### Testing
```bash
go test -race ./...
go test -race -run 'TestName' ./internal/...
```

### Git Workflow
- Branch naming: `solo/issue-[number]-description`
- Commit format: `type: description (#issue)`

## Project-Specific Notes

- **Public repo.** Fixtures are synthetic only: phone numbers in the +44 7700 900xxx range and IDs with the `99999000000` prefix, each listed exactly in `.secret-pii-allowlist`. Never commit a real number, JID, community name, deployment host or the deployer's private config repo name.
- **Secret scanner.** A word like `token`, `secret` or `password` directly followed by `=` or `:` is flagged; build token-shaped test strings at run time. Device JIDs (`user:N@server`) read as email addresses; build them in code.
- **whatsmeow.** Upgrade only by changing the pin in `go.mod` deliberately; nothing outside `internal/client/whatsmeow` imports it.
- **Exit codes.** Config errors exit 1; fatal WhatsApp states (logged out, banned, outdated client) exit 78 so systemd does not restart into them.
