# KANBAN

Mirror of the session task list. Move cards through columns as work lands.

## Backlog

- HDR10/4K pipeline validation on a real HDR source + real encode (test VMs
  have no HDR material; MediaInfo stubs + dovi_tool roundtrip on 219 cover
  the logic, not full-resolution pixels)

## In progress

- GPU-path validation on a real Nvidia node (test VMs have no GPU: DGIndexNV
  and KNLMeansCL/OpenCL filters untestable there)

## Done (CI/CD on GitHub Actions)

- Repo went public → security pre-flight first: 4 agent core dumps (~376 MB,
  containing process memory incl. a credential fragment) were committed and
  got untracked + gitignored; full-tree secret scan otherwise clean (test
  fixture creds only).
- CI workflow (.github/workflows/ci.yml): the AGENTS.md command map — backend
  gofmt/vet/test (pwsh E2E tests run on the runner) + cross-build
  (linux controller, windows agent); frontend eslint/tsc/vitest/vite build.
  Concurrency cancellation per branch.
- Release workflow (release.yml): version tags build the deploy bundle
  (controller + encode-agent.exe with main.Version, SPA, provision playbook,
  SHA256SUMS) and publish a GitHub release. Deployment to the Docker host
  stays operator-driven (private LAN, unreachable from runners).
- gofmt drift on 8 pre-existing files cleaned so the format gate passes.
- Docs: Deployment.md CI/CD section.

## Done (live Discord webhook in Settings)

- Discord webhook is now a live Settings-page field (Settings → Discord
  notifications): edits apply immediately — no restart — to both the
  job-outcome alerts (notify package, resolved per alert) and the
  discord_notify step's fallback (injected into $Job at render time). The
  static boot-time Notifier was replaced by per-call resolution off the
  settings row; env ENCODE_DISCORD_WEBHOOK seeds the default, and a saved
  blank value turns notifications OFF even when the env var is set.
- URL validation on save (discord.com/discordapp.com webhooks or loopback),
  mirroring the step's exfil guard. 3 new API tests (validation matrix,
  resolution precedence incl. blank-disables, env→live propagation into
  rendered job scripts) + full suite green.

## Done (Discord notification flow step)

- New discord_notify step: posts an episode-progress message to a Discord
  webhook when the flow reaches it. Webhook from the step param, falling
  back to the controller's ENCODE_DISCORD_WEBHOOK injected into $Job at
  render time (no per-flow pasting). Best-effort: unreachable webhook =
  warning, encode continues. URL guard (discord.com/discordapp.com +
  loopback) blocks exfiltration to arbitrary hosts; 2000-char Discord cap
  handled; UTF-8 body for unicode series names (WebClient, PS 5.1-safe).
- pwsh E2E vs a real mock webhook server: param path, controller fallback,
  polite no-op skip, unreachable-webhook warning, exfil guard — all covered.
- Distinct from the controller's job-outcome alerts (notify package), which
  fire on done/failed regardless of the flow.

## Done (Dolby Vision RPU in encode_4k)

- encode_4k consumes hdr.json from the separate hdr_probe step (no probing
  inside the encode). DoVi path verified closed-loop on node 219's real fork:
  dovi_tool extract-rpu → x265 --dolby-vision-profile 8.1 --dolby-vision-rpu
  (RPU roundtrips out of the encoded stream; no mux-side inject needed) +
  the mandatory --vbv-maxrate/--vbv-bufsize/--master-display/--max-cll set.
- Fork correctness found live: this build accepts smpte2084/bt2020nc and
  REJECTS smpte-st2084/bt2020-as-colormatrix — HDR10/HLG signaling fixed
  everywhere. Extraction failures fall back to HDR10 signaling with a loud
  warning; rpu.bin is reuse-cached across job retries.
- Guarded factory upgrade Encode4kFactoryV1 → current (byte-for-byte guard,
  user edits survive) + regression test that the guard is live. New pwsh E2E
  covers both the full DoVi path and the fallback.

## Done (HDR/4K pipeline, language-aware audio, series scaffolding)

- Create Series system: UI Series page → Create series dialog (name, episode
  count, tag, flow) → POST /api/series scaffolds the scripts-share episode
  folders (`<ScriptsRoot>/<Name>/Ep 01…Ep NN`, empty by design) AND the
  release folder (`[Group] <Name> - Raws [Tag]` on the release share),
  registers the series row. Idempotent extend (re-run with a higher count);
  Windows-reserved-char + traversal validation before any mkdir; 3-digit
  episode padding for 100+ episode shows.
- Audio auto-select by language: new `audio_lang` step — MediaInfo language
  priority list (default jpn,eng), falls back to track 1 with a loud warning,
  eac3to → WAV → opusenc as usual, writes audio.json; mux template upgraded
  to read audio.json and set the mkvmerge track language (byte-for-byte
  guarded factory upgrade chain V1→V3, V2→V3 — user edits survive).
- HDR/DoVi: new `hdr_probe` step — MediaInfo transfer/primaries/MaxCLL/DV
  detection → hdr.json; DoVi detected and signaled as HDR10 (RPU passthrough
  tracked in backlog).
- 4K step: scanner now recognizes 2160.avs/2160.vpy (outranks 1080 scripts,
  vpy wins at equal resolution); new `encode_4k` template (CTU 64 defaults,
  structured fields) reads hdr.json and switches to bt2020/PQ signaling.
- Per-series tag override: series.tag column + UI Tag column; renderer uses
  it for output names and release folders (e.g. 4K re-encodes of 1080p shows).
- Tests: scanner 4K priority, 7 create-series API tests, tag override →
  rendered script, helper unit tests, 5 dialog component tests, and two pwsh
  E2Es running hdr_probe/audio_lang/encode_4k/mux end to end (jpn selected
  over earlier eng stream; bt2020/PQ flags emitted; audio.json/hdr.json
  sidecars verified).

## Done (controller-driven provisioning)

- WebUI Provision page: form (host, WinRM port/user/password, node name,
  toolchain/NFS/bin toggles) → controller runs bundled ansible-core over
  WinRM with the live Settings (controller_url, path mapping, NFS exports).
- Zero-touch pairing: each run auto-issues a one-shot code; the agent
  self-registers as a Windows service and persists its own credential.
- Toolchain installs (idempotent, silent): MediaInfo CLI 26.05, AviSynth+
  3.7.5 (InnoSetup /VERYSILENT), Python 3.14.7 x64, VapourSynth R79 via the
  OFFICIAL NSIS installer (per operator decision — not pip), functional
  idempotency checks (python import for VS).
- Bin folder push: published bin-package.zip staged from the update store,
  uploaded via win_copy, expanded over the node's tools dir.
- Security: WinRM password never persisted (0600 temp vars file, deleted on
  run end, never logged, never on a command line); run logs streamed live
  with a 512 KiB cap; stale runs reconciled to failed at startup.
- Node deletion endpoint (busy-guarded) so hosts can be re-provisioned after
  name collisions; UI delete button on the Nodes page.
- Live-verified: provisioning enc-test-docker-2 end-to-end from the browser.
- Adversarial review round (engine/api/fe, GLM+DeepSeek): HIGH fixed —
  strings.Builder copied by value in the flush retry path would panic on the
  exact transient DB failure it was meant to survive. MEDIUMs fixed: single-
  flusher log pipeline (ordered appends, credential redaction, scanner
  errors surfaced), persisted log column capped in SQLite, 45-min timeout
  starts only after the serialization lock, busy-node delete made atomic
  (conditional DELETE), controller URL trimmed before save, stale staging
  dirs swept at startup (crash-leaked vars.yml holds the WinRM password),
  pinned ansible-core 2.21.3 / pywinrm 0.5.0 / ansible.windows 3.7.0,
  playbook fixes ('mounted:' idempotency, Expand-Archive error handling,
  lib_path only when deployed, pairing code stripped after pairing), UI
  polling stops at terminal status and autoscroll respects manual scroll.
- Rejected findings: verbose-flag secret echo (verbosity never raised +
  redaction added), pairing code in agent.json as HIGH (bounded one-shot +
  now stripped), React password-in-memory (standard form behavior).

## Done (Settings page + WebUI fleet push)

- Settings page (live, no restart): NFS share record (server/exports),
  controller roots, REMOTE PATH MAPPING (node bin/scripts/release dirs used
  by the job renderer), scan interval, reboot threshold, group/tag. Strict
  path validation (Windows absolute vs Unix absolute, drive-relative and
  slash-UNC rejected). Single-row settings table; env seeds defaults.
  Scanner loop + job renderer + heartbeat reboot limit read settings LIVE.
- Publishing from the WebUI: agent binary, EncodeLib.ps1, and bin-folder ZIP
  packages (version-gated, zip-slip/symlink/drive/UNC validated at publish,
  served to nodes with SHA-256). Nodes sync on idle heartbeat: lib -> bin
  (idempotent re-extract, version bumped only after full success, locked
  files retry next heartbeat) -> agent binary (service OR task/bare relaunch
  via swap sidecar).
- Agent hardening from adversarial review: sync steps independent (bin
  failure never blocks agent self-update), Syncing heartbeat flag blocks job
  assignment mid-swap, 1 GiB bin download cap matching the publish cap,
  streaming decompression cap, per-payload manifest recovery across restarts.
- Live-verified on the fleet: 219's C:\bin (137 MiB) zipped, published, and
  auto-extracted on BOTH nodes; agent pushed 0.3.3 -> 0.8.x purely via the
  publish endpoint; settings edits change scanner cadence without restart.
- Fix found live: loadFromDisk demanded every payload exist, so publishing
  agent+bin without EncodeLib wiped the manifest on restart — payloads now
  recover independently (regression-tested).

## Done (change-password system — no password in .env)

- POST /api/auth/password: verifies current password (wrong attempts count
  against the login throttle), min 10 chars, must differ, bcrypt rehash,
  revokes all OTHER sessions of the user (performing session survives).
- UI: Change password dialog in the sidebar (client-side policy validation,
  server errors surfaced, success screen advising .env cleanup). 5 component
  tests + 8 backend tests (policy, throttle, revocation, session survival).
- Recovery hatch: ENCODE_ADMIN_FORCE_PASSWORD=1 makes startup overwrite the
  stored admin hash from ENCODE_ADMIN_PASSWORD once (warn-logged); without
  the flag env never touches an existing account. Tested.
- Compose: ENCODE_ADMIN_PASSWORD now optional (`:-` instead of `:?`).
- Live-verified on the Docker host: rotated the real admin password through
  the endpoint (old 401, new works), removed the password line from the
  host's .env, recreated the container — login now runs purely off the DB
  hash; nodes/jobs/templates all intact.
- Adversarial review round-trip (GLM + DeepSeek, both diffs): fixed the
  401-bounce swallowing wrong-current-password errors (raw fetch), throttle
  now GATES the change endpoint (bcrypt oracle closed), 72-byte bcrypt cap,
  force-reset revokes all sessions + ERROR log + compose wiring, dialog
  re-entrancy/backdrop/IME guards. Rejected: session-revocation TOCTOU and
  non-transactional update+revoke (sub-second race inherent to
  middleware-time session auth; requires DB failure to bite).

## Done (FileFlows-style plugins + fully editable steps)

- Three plugin steps shipped as built-ins (FileFlows-inspired, Tier-1 picks):
  media_probe (MediaInfo JSON -> container/video/audio report incl. suggested
  eac3to track index), audio_branch (lossy/lossless-aware Opus bitrate
  budgeting), crc32_rename (streaming CRC32 -> [ABC1234D] release naming,
  propagates $Job.OutputName so release_copy/keyframes follow).
- x265 encode step now has structured Opus-style fields (preset, crf, aq_mode,
  aq_strength(+edge), psy_rd/rdoq, rd, ctu, no_sao/b_pyramid/open_gop bools);
  blank field = documented default; x265_args remains as raw override.
- Step scripts are FULLY UI-editable and PERSIST ACROSS RESTARTS: boot
  seeding switched to insert-if-absent; POST /api/step-templates/{id}/reset
  restores factory defaults; live-verified on the Docker host (edit survived
  a container restart).
- Flow builder: typed widgets (checkbox for bool, number inputs), defaults
  prefilled on add; Steps page edits param type + default columns.
- Tests: 6 new API tests (edit survival, reset, plugin seeding), 1 pwsh E2E
  running all three plugin steps with a MediaInfo JSON stub; all gates green.

## Done (multi-node fleet)

- Second Windows node added by cloning the test VM (172.24.92.229). Clone
  hazard handled: the clone inherited node 1's persisted credential +
  scheduled tasks — wiped `C:\encode-agent-dist\{agent.json,node.token}`,
  deleted inherited tasks, then paired fresh as `enc-test-docker-2`.
  Agent restarts survive via an `/sc onstart` scheduled task.
- True 2-node distribution validated against the Docker-host controller:
  two episodes seeded, one job per worker (`enc-test-docker`,
  `enc-test-docker-2`), both done exit 0, release MKVs verified on each node.

## Done (Docker-host deployment — distributed topology validated)

- Controller deployed to the Docker host (172.24.92.232, Debian 13, Docker
  29.7.2) as a slim runtime container: `/opt/encode-system`
  (`docker/Dockerfile.runtime` pattern — pre-built binary + SPA, no build
  deps), health + SPA on :8080, state in a named volume.
- True distributed E2E passed: agent `enc-test-docker` self-paired over the
  LAN to the Docker controller; job dispatched Docker-host -> Windows node;
  full pipeline completed with real tools — eac3to->opusenc Opus (348 kbit/s),
  Patman x265 fork encoding YV12 .avs via AviSynth+ 3.7.5 (AVX2), mkvmerge
  mux, release copy, SCXvid keyframes. Job done, exit 0; release artifacts
  verified on the node.
- Path mapping: controller env `ENCODE_NODE_SCRIPTS/RELEASE/BIN` must point
  at each node's local dirs when there's no shared NFS (set for the test).
- Fixture/E2E pitfalls logged: eac3to rejects mono AC3 (use stereo); the
  audio step `track` param is the 1-based track index (e.g. 2), not `1.0`;
  fixture .avs must be YUV (`pixel_type="YV12"`) or the fork refuses the
  colorspace; PS 5.1 `ConvertTo-Json` decorates strings — read files with
  `[IO.File]::ReadAllText()` before posting JSON.

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
