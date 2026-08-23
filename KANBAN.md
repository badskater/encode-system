# KANBAN

Mirror of the session task list. Move cards through columns as work lands.

## Backlog

- Agent self-registration token bootstrap UX (one-shot pairing code)
- Flow templates: import/export as JSON
- Per-series default flow selection rules

## In progress

- First real deploy to a Windows Server 2025 node (needs a physical node)

## Done

- Repo bootstrap + baseline docs
- Backend: store, flow renderer, scanner, HTTP API dispatcher, agent
- EncodeLib.ps1 Opus pipeline (eac3to → WAV → opusenc)
- React SPA: dashboard, jobs, nodes, visual flow builder
- Ansible: NFS client, C:\\bin toolchain, agent service
- Docker controller image + compose with NFS volume mounts
- Live E2E on Linux: scanner → job → agent → full pipeline → release folder → keyframes; reboot enforcement verified
