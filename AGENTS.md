# AGENTS.md

Balanced engineering baseline for AI coding agents. Use when you want strong delivery discipline with lower overhead than full strict mode.
**Adoption:** Copy or rename to repo root as `AGENTS.md`. Fill in the command map.
Use `REPO_CONVENTIONS.md` for stack-, org-, or environment-specific rules this file should not guess.

## 1. Precedence

Highest to lowest:

1. **Direct maintainer/user request** for the current task
2. **Explicit repository policy** (`REPO_CONVENTIONS.md`, maintainer docs, CI config)
3. **This file**
4. **Repository READMEs and defaults**

Priority within this file: Security > Correctness > Change safety > Process.
If rules conflict, state the tradeoff in the handoff.
Match the host repo's established naming, structure, tooling, and style.
Security and testing requirements are non-negotiable unless explicitly excepted in the PR/handoff.

## 2. Non-Negotiable Rules

### 2.0 Approval for destructive or external actions

Require explicit approval before:

- Force-pushes, history rewrites, branch deletion, hard resets, destructive reverts.
- Recursive deletes, risky bulk moves/renames, or irreversible transformations.
- Deployments, infra changes, shared DB migrations/backfills, production writes.
- Sending PR comments/tickets/messages as if from a human or team.
- Sending workspace code, logs, docs, screenshots, or data to external systems.
- Using secrets, credentials, or privileged production access.

Never transmit secrets, credentials, personal data, or proprietary material externally.

### 2.1 Protect existing work

Inspect the working tree before editing. Never overwrite or discard work you did not make. If conflicts exist, pause and ask.

### 2.2 No behavior change without tests

Behavior changes and new logic require tests. Bug fixes require a regression test when feasible. Never disable or weaken tests to make changes appear green.

### 2.3 No claim without verification

Verify changes in the target runtime when practical. Never claim success without evidence. State unverified items explicitly.

### 2.4 Plan before implementing

For non-trivial changes (multi-module, API/schema/contract changes, migrations, security-sensitive paths), present a plan and wait for approval. Low-risk, well-bounded changes may proceed directly. **P0 exception:** broken main/CI gets the smallest safe fix first, then retro plan in handoff/PR.

### 2.5 Understand before fixing

Investigate root cause before changing code. Read source and inspect actual data. Verify contracts before building on them. Do not guess data shape, interfaces, or runtime behavior.

### 2.6 One commit per logical change

Each commit is independently understandable, deployable, and reversible. No exploratory or half-finished work.

### 2.7 Do not regress

Confirm existing tests pass for affected scope. Changed behavior must not move backward in correctness, reliability, security, or performance.

## 3. Repository Bootstrap

Always create it as a private repo first. CI/CD will be ran only when the repo is going public.

### 3.1 Recommended structure

```text
repo-root/
|-- AGENTS.md
|-- CLAUDE.md              (contains: @AGENTS.md)
|-- README.md
|-- REPO_CONVENTIONS.md    (optional)
|-- TODO.md                (optional)
`-- Docs/
    |-- md/
    |   |-- Architecture.md
    |   |-- Deployment.md
    |   `-- Operations.md
    `-- diagrams/
        `-- README.md
```

`CLAUDE.md` ensures Claude-based agents follow the same baseline. Its only required content is `@AGENTS.md`.

### 3.2 Session workflow

Before substantial work:

1. Read `REPO_CONVENTIONS.md` or maintainer guidance if present.
2. Read `Docs/md/Architecture.md` for behavior, architecture, or infrastructure changes.
3. Read `Docs/md/Deployment.md` for release, environment, or configuration work.
4. Read `Docs/md/Operations.md` for runtime, incident, or observability changes.
5. Read `TODO.md` when sequencing or scope tradeoffs matter.

Create missing baseline docs only when needed by task or explicitly requested. Avoid mechanical doc creation in small repos.

### 3.3 Bootstrap check

For new app/service/platform repos, verify required docs exist and seed only minimum sections:

- `Docs/md/Architecture.md`: objectives/NFRs, architecture, contracts, runtime flows, security/observability, open decisions.
- `Docs/md/Deployment.md`: prerequisites, deployment steps, post-deploy validation, rollback, troubleshooting.
- `Docs/md/Operations.md`: health checks, logs, alerts/signals, operator tasks, known failure modes.
- `Docs/diagrams/README.md`: diagram index with purpose, source path, and related architecture section.

## 4. Quality Gates and Definition of Done

A change is complete only when every applicable gate passes.

| Gate | Condition |
| --- | --- |
| Lint | Passes if linting exists |
| Type check | Passes if static typing exists |
| Tests | Pass for changed scope and project baseline |
| Build | Passes if a build/package step exists |

**Command map**:

```text
workdir:   /backend
lint:      go vet ./...
typecheck: go build ./...
test:      go test ./...
build:     go build -o bin/controller ./cmd/controller && GOOS=windows GOARCH=amd64 go build -o bin/agent.exe ./cmd/agent

workdir:   /frontend
lint:      npm run lint
typecheck: npx tsc --noEmit
test:      npm run test   # vitest run — lib, API clients, hooks, and components
build:     npm run build

workdir:   /infra/ansible
lint:      ansible-playbook --syntax-check site.yml
typecheck: N/A
test:      N/A
build:     N/A
```

If unfilled, discover from `package.json`, `Makefile`/`Taskfile`/`justfile`, CI workflows, `pyproject.toml`, `go.mod`, `Cargo.toml`, or maintainer docs.

### 4.1 Test expectations

| Change | Expectation |
| --- | --- |
| Feature / behavior | Unit tests; integration tests when crossing boundaries |
| Bug fix | Failing regression test first, then fix to green |
| Refactor | Existing tests pass; add coverage for risky paths |
| Docs / comments only | No new tests required |
| Config / dependency / build | Tests validating startup, build, runtime compatibility |

Always test using the adversarial-model-review skill.

### 4.2 Verification evidence (required in every handoff)

```text
Verification Evidence
- Summary:
- Files touched:
- Commands run:
- Runtime/environment:
- Expected behavior:
- Observed behavior:
- Test results (pass/fail):
- Alternative validation (if primary blocked):
- Unverified items / risks / follow-ups:
```

If a requirement could not be met, state what was missed, why, alternative validation used, and residual risk.

### 4.3 Documentation checklist

- [ ] READMEs and config samples updated when relevant.
- [ ] Architecture/Deployment/Operations docs updated for behavior or ops changes.
- [ ] Diagram references still resolve.
- [ ] Add clear, meaningful comments to this code explaining what each function does.

## 5. Engineering Principles

1. **Search before writing.** Reuse existing code before inventing new primitives.
2. **DRY.** Extract duplicated logic into shared functions. Parameterize, don't fork.
3. **SOLID.** One function/module, one job. Depend on abstractions. Narrow interfaces.
4. **Testability.** Prefer pure functions. Pass dependencies as parameters.
5. **Type safety.** Validate external data at boundaries. Guard null/missing before deeper logic.
6. **Dependency boundaries.** Prefer explicit dependency injection over globals/singletons. Isolate side effects.
7. **Fail fast.** Do not swallow exceptions without intentional handling. Structured error types when supported.
8. **Performance awareness.** No per-item external calls in loops. No O(n²) in hot paths. Cache expensive computations.
9. **YAGNI / Simple design.** Build only what's needed. Ask: does this exist already? Am I over-engineering?

## 6. Development Practices

- **Test-first when feasible.** Start with a failing test, make it pass, then refactor.
- **Refactor in scope.** Leave touched files cleaner. Separate unrelated cleanup into its own commit.
- **Human + AI pairing.** Agent output is never pushed without human review. "Should work" is not verification.
- **Parallel runs.** For multi-agent execution, follow coordination rules in section 7.4.
- **CI as last gate.** Test locally → review diff → push → CI confirms. Fix process gaps when local testing missed.
- **Fast feedback.** Broken/flaky tests are P0 blockers — fix test infrastructure before shipping features.

## 7. Git Discipline

### 7.1 Staging and committing

- **Never `git add -A` or `git add .`.** Stage files explicitly.
- **Pre-commit review:** `git status --short`, `git diff --cached --stat`, `git diff --cached`.
- **Atomic commits.** One logical change per commit. Messages explain *why*. Follow repo prefixes (`fix:`, `feat:`, `docs:`) if adopted.
- **No junk.** No debug leftovers, commented-out code, or half-implemented work.

### 7.2 Branches and risk

- **Short-lived branches** synced with mainline.
- **High-risk changes** (schema, API contracts, migrations, infra) require: risk summary, rollback plan with triggers, post-deploy verification steps.

### 7.3 Never commit

- Secrets, credentials, tokens, or personal data.
- Build artifacts or dependency directories unless explicitly required.
- Local IDE/editor files not intended for repo sharing.

### 7.4 Parallel Agent Coordination

- Assign explicit ownership per agent (directory, feature, or worktree). Do not edit outside assigned scope.
- Treat unrelated breakages as possible concurrent edits: if build/test fails in files you did not modify, wait and retry once before investigating.
- Do not share browser automation sessions. Use isolated profiles/instances per agent; queue access with a lock when needed.
- Run risky shared-code work in isolated `git worktree`s; review and merge diffs intentionally.
- Log agent actions for concurrent runs (timestamp, agent/session ID, action, target) using the repo's standard logging sink.
- Do not "clean up" or revert another agent's in-flight changes unless explicitly assigned.
- Use a CI/CD-style model: clear inputs, isolated execution, observable outputs, explicit handoff.

## 8. Testing Standards

- **Test pyramid:** Unit (fast, majority) → Integration (boundaries) → E2E (critical flows, few).
- **Quality:** Deterministic, independent, readable. Cover edge cases and error paths. Each test owns its state. Order must not matter.
- **Naming:** Descriptive — `test_expired_token_returns_401`, not `test_case_3`.
- **Coverage:** Signal, not target. Focus on decision points, error paths, and complex logic. No unjustified coverage drops in touched modules.
- **Regression gaps:** When a regression test is not feasible, provide the closest practical automated verification and explain the gap in the handoff.

## 9. Documentation and Observability

Update docs when behavior, interfaces, operations, or deployment changes:

- `Docs/md/Architecture.md` for flow, contract, and infrastructure changes.
- `Docs/md/Deployment.md` for rollout, validation, rollback, troubleshooting.
- `Docs/md/Operations.md` for runbooks, alerts, failure modes, and support workflow.
- Relevant README/config samples for new or changed configuration.

Observability baseline:

- Structured logs (key=value fields) over interpolated strings.
- Include correlation IDs where request/workflow tracing matters.
- Log failures with enough context to diagnose without reproducing.

## 10. Code Quality

- **Naming:** Intent-revealing. Options objects over boolean flags.
- **Functions:** Short, focused, screen-sized. Constants over magic values.
- **Error handling:** Handle meaningfully or propagate. Structured error types when supported.
- **Comments:** Explain *why*, not *what*. Remove commented-out code.
- **Markdown:** ATX headings, sentence-case titles, fenced code blocks with language identifiers. Repo-relative paths in backticks.

## 11. Security and Dependencies

- Never hardcode secrets. Use secrets managers and workload identity.
- Least-privilege access. Never log secrets or credential-bearing URLs.
- Validate and sanitize all external input at system boundaries.
- Prefer standard library and existing repo dependencies before adding packages.
- Before adding a dependency: evaluate license, maintenance, transitive footprint, and security history. Justify the addition.
- Use established crypto/auth libraries. Never roll your own.

## 12. Review Output Standard

- Findings first, sorted by severity then by file.
- Each finding includes severity, file+line, impact, and recommended fix.
- Summaries are brief and secondary.
- Call out assumptions, missing evidence, and residual risk explicitly.

## 13. Anti-Patterns (Never Do)

- Start non-trivial implementation without an approved plan.
- Push/hand off changes without required checks/evidence.
- Disable/skip tests to get green status.
- Silence errors or swallow exceptions without deliberate handling.
- Introduce global mutable state without strong justification.
- Rewrite unrelated code just because it was nearby.
- Optimize without evidence of a bottleneck.
- Commit dead code, debug leftovers, or TODO hacks as done work.
- Add dependencies without discussion or justification.
- Depend on test execution order.

## 14. Pre-Handoff Checklist

- [ ] Scope is minimal and aligned to request.
- [ ] Tests added/updated for behavior changes.
- [ ] Lint/typecheck/test/build results recorded.
- [ ] Runtime behavior spot-checked where practical.
- [ ] Docs updated for architecture/deployment/operations impact.
- [ ] Risks, follow-ups, and unverified items listed.
- [ ] Diff reviewed for accidental edits.
- [ ] Commit(s) are atomic and reversible.
