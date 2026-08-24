# KANBAN

Mirror of the session task list. Move cards through columns as work lands.

## Backlog

(empty)

## In progress

- GPU-path validation on a real Nvidia node (test VM has no GPU: DGIndexNV
  and KNLMeansCL/OpenCL filters untestable there)

## Done (real x265 fork validated — after VM vCPU change to i9-13900HX)

- Patman/JPSDR x265 fork runs with the FULL legacy argument set (aq-mode 5,
  aq-strength-edge, aq-bias-strength-edge etc.) reading .avs via AviSynth+
  3.7.5: 120 frames encoded, muxed, released, keyframed -> job done exit 0
- Node env fix found: AviSynth+ plugins64 contained two 32-bit DLLs
  (dfttest.dll, libfftw3f-3.dll) that abort the 64-bit loader; real nodes
  need the 64-bit builds (dfttest backs TTempSmooth in the filter chain)
- Agent credential + task counter survived a full VM reboot (persistence OK)

## Done (first real Windows Server 2025 deploy — 172.24.92.219)

- Full stack live on WS2025 Datacenter + PowerShell 5.1.26100: controller +
  agent binaries, pairing self-registration, scanner, series registry,
  per-series flow selection, custom step template created via API and executed
- Real tools ran: eac3to (AC3->WAV), opusenc (deployed, Opus @ 262 kbit/s),
  mkvmerge, ffmpeg libx265 (cpu-test flow), release copy
- Four real bugs found and fixed (all committed with tests):
  1. agent.json loader choked on UTF-8 BOM (PS Set-Content default)
  2. rendered job.ps1 written without BOM -> PS 5.1 ANSI mojibake of
     non-ASCII content (anime series names)
  3. Invoke-Tool logged 'RemoteException' noise for native stderr (PS 5.1
     ErrorRecord wrapping)
  4. SCXvid invocation contract wrong: it reads y4m on STDIN and takes the
     output log as its only arg (verified against the real binary's usage
     text; step rewritten cross-platform with .NET process piping)

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

## Done (deploy prep + notifications)

- First-node deploy runbook: Docs/md/FirstNodeDeploy.md (copy-paste steps:
  controller, tool staging, inventory, credentials, smoke episode)
- Docker fixes: state volume now covers the whole /data (DB persistence),
  SPA served from /app/ui via ENCODE_UI_DIR, version ldflag ARG fixed
- Discord notifications: ENCODE_DISCORD_WEBHOOK env; done/failed alerts with
  series/episode/node/error/duration (5 unit tests + live E2E vs mock webhook)

## Done

- Repo bootstrap + baseline docs
- Backend: store, flow renderer, scanner, HTTP API dispatcher, agent
- EncodeLib.ps1 Opus pipeline (eac3to → WAV → opusenc)
- React SPA: dashboard, jobs, nodes, visual flow builder
- Ansible: NFS client, C:\\bin toolchain, agent service
- Docker controller image + compose with NFS volume mounts
- Live E2E on Linux: scanner → job → agent → full pipeline → release folder → keyframes; reboot enforcement verified
