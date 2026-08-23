# KANBAN

Mirror of the session task list. Move cards through columns as work lands.

## Backlog

- UI button to change a pending job's flow (API exists: PATCH /api/jobs/{id})

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

## Done

- Repo bootstrap + baseline docs
- Backend: store, flow renderer, scanner, HTTP API dispatcher, agent
- EncodeLib.ps1 Opus pipeline (eac3to → WAV → opusenc)
- React SPA: dashboard, jobs, nodes, visual flow builder
- Ansible: NFS client, C:\\bin toolchain, agent service
- Docker controller image + compose with NFS volume mounts
- Live E2E on Linux: scanner → job → agent → full pipeline → release folder → keyframes; reboot enforcement verified
