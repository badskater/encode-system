# KANBAN

Mirror of the session task list. Move cards through columns as work lands.

## Backlog

(empty)

## In progress

- First real deploy to a Windows Server 2025 node (needs a physical node)

## Done (phase 2)

- One-shot pairing codes: issue in UI, agent self-registers and persists its credential
- Per-series flow selection + series pause (scanner-driven registry)
- Step templates: every flow section owns its PowerShell; custom steps in the UI,
  syntax-checked, linked into the rendered final flow
- Flow JSON export/import (embeds custom templates); multiple saved flows with one default
- Live E2E: 2 nodes, 2 series, per-series flows, custom step executed in pwsh, pairing bootstrap

## Review status

- Adversarial review (GLM-5.2 + DeepSeek-v4) over backend, flow renderer,
  scanner, update store, agent, and EncodeLib.ps1: all findings adjudicated —
  fixed with regression tests, or rejected with reason. Three rounds; live
  smoke re-run after each round.
- Phase-2 review round: 33 findings. Fixed: renderer PowerShell-injection
  hardening (sanitized identifiers, comment-safe values, duplicate-function
  refusal, validation parity), mandatory update checksums, lib swap deferred
  while a job runs, atomic pairing validation before node creation, import
  protection against template overwrite, bounded name-collision scan,
  keyframes freshness, counter-write error surfacing, empty-credential
  re-pairing. Both documented residual risks were later closed in code:
  the swap sidecar now waits for process exit and retries the move with a
  bounded loop (restart only on success; POSIX gets a direct rename path),
  and the agent warns loudly when the controller URL is plain HTTP.

## Done (backlog run)

- UI flow changer: pending jobs show a flow dropdown in the Jobs table
  (PATCH /api/jobs/{id} surfaced; locked once assigned/running)
- Agent binary swap hardening: wait-for-exit + bounded move retry + restart
  only on success + failure logging (regression-tested)
- Plain-HTTP controller URL warning at agent start

## Done

- Repo bootstrap + baseline docs
- Backend: store, flow renderer, scanner, HTTP API dispatcher, agent
- EncodeLib.ps1 Opus pipeline (eac3to → WAV → opusenc)
- React SPA: dashboard, jobs, nodes, visual flow builder
- Ansible: NFS client, C:\\bin toolchain, agent service
- Docker controller image + compose with NFS volume mounts
- Live E2E on Linux: scanner → job → agent → full pipeline → release folder → keyframes; reboot enforcement verified
