# KANBAN

Mirror of the session task list. Move cards through columns as work lands.

## Backlog

- HDR10/4K pipeline validation on a real HDR source + real encode (test VMs
  have no HDR material; MediaInfo stubs + dovi_tool roundtrip on 219 cover
  the logic, not full-resolution pixels)

## In progress

- GPU-path validation on a real Nvidia node (test VMs have no GPU: DGIndexNV
  and KNLMeansCL/OpenCL filters untestable there)

## Done (release pipeline + one-command installs, 2026-09-29)

- Release workflow now publishes a complete offline bundle
  (encode-system-<ver>-deploy.tar.gz: binaries, ui/, provision/,
  Dockerfile.runtime, compose, .env.example, installers, SHA256SUMS), pushes
  ghcr.io/badskater/encode-system:<ver>+latest, and attaches loose
  install.sh / install-agent.ps1. Assets publicly downloadable despite the
  private repo. Fixes along the way: gh needs explicit GH_TOKEN in
  workflows; notes via API + --notes-file; @vitejs/plugin-react bumped to v5
  (vite-8 peer range — stale v4 lockfile broke npm ci in the image build).
- docker/install.sh (controller host): resolves the release, verifies
  checksums, installs /opt/encode-system (ENCODE_HOME), preserves .env,
  snapshots the DB, compose pull || build, health-check. install-agent.ps1
  (node): downloads the agent, writes agent.json (pairing code or token),
  registers the service.
- Isolated-test-deploy support: ENCODE_CONTAINER_NAME override + a
  compose-project guard that reads it (refuses to touch a container owned by
  a different project). Gotcha paid for in production once: both composes
  defaulted to project name encode-system and a test `docker compose up`
  recreated the LIVE controller.
- Fresh-volume boot crash fixed: /data must be chowned to the runtime user
  BEFORE VOLUME /data in Dockerfile.runtime (else SQLite "out of memory
  (14)"). v1.17.4+.

## Done (storage shares + full S3 autonomy, 2026-09-28)

- shares table + CRUD (GET|POST /api/shares, PUT|DELETE /api/shares/{id}),
  {kind: nfs|smb|s3, role: scripts|release}. Passwords write-only
  (has_password in responses), AES-GCM encrypted with a key file beside the
  DB (backup downloads never carry secrets); mutations audit as share.*.
  Provisioning resolves one enabled share per role (smb > nfs > s3); SMB
  mounts via Ansible encode_smb_* vars, gated by the mount_shares option.
  Frontend Shares card on Settings.
- S3-backed roles don't mount: renderJob points $ScriptsDir/$ReleaseDir at a
  {{JOBDIR}} staging placeholder + attaches an S3Transfer spec; the agent
  downloads scripts/<episode_dir> (staging mirrors the episode subdir)
  before the run and uploads outputs only after success. Works against any
  plain-S3 endpoint (MinIO, Ceph RGW, AWS) via minio-go; shared client in
  internal/s3 (EndpointFromShare normalizer).
- Full S3 autonomy: an enabled s3 scripts share switches the scanner to
  bucket listing (scanner.ScanS3, depth-2 <series>/<episode>/ keys, same
  source+script rules, LastModified stability gate 2m). Episodes
  auto-enqueue with no mount anywhere. Farm-smoked on 232 against a local
  s3mock server (seeded episode + touch -d '2 hours ago' to clear the
  stability gate; shares created via API; jobs enqueued within ~2 scan
  cycles).

## Done (queue control & admin plane, 2026-09-27)

- Live job log streaming: GET /api/jobs/{id}/log/stream (SSE, admin) —
  snapshot → heartbeat-fed progress events → final event; 15s pings.
  JobLogDialog tails live. Old agents = snapshot-only streams.
- Bulk job retry/cancel: POST /api/jobs/bulk (max 500, per-id guarded —
  wrong-state/missing ids return skipped, never fail the batch) + Jobs-page
  checkbox selection.
- Job history retention: settings.job_retention_days (0 = forever) with an
  hourly prune loop + POST /api/jobs/prune {days:1-3650}; terminal jobs only.
- Dispatch steering: jobs.last_failed_node_id — retries prefer a different
  node within the same priority tier (priority outranks steering).
- Agent release rollback: POST /api/updates/agent/rollback (one .prev slot;
  409 when none) + Settings publish-card button; nodes self-downgrade
  through the normal version-diff sync.
- Per-series pause (series.paused): scanner AND dispatch hold for queued
  jobs; UI pause/resume toggle (distinct from the enable flag).
- Node-group routing: nodes.group + series.node_group (empty group =
  wildcard) + UI on both pages.
- Per-node job concurrency: nodes.max_concurrent_jobs (1-8, default 1),
  store-enforced slot cap, active_jobs in /api/nodes, slots UI.
- Disk-space alert + soft drain: settings.disk_alert_gb — below-threshold
  heartbeat fires a cooldown-guarded Discord alert and holds new jobs until
  recovery; UI threshold setting + warn chip.
- ENCODE_METRIC quality/output stats per job (agent-collected →
  jobs.metrics_json → UI); per-series webhooks (series.webhook_url, direct
  alerts only) + stats context in Discord alerts; job ETA
  (GET /api/jobs/{id}/eta, flow-history average, ≥2 samples); Prometheus
  GET /metrics mounted outside the SPA; audit log (GET /api/audit + Audit
  page); scoped API tokens (/api/tokens, admin|read, hashed, last_used_at);
  scheduled DB backups (GET|POST /api/backup, PUT /api/backup/settings,
  download/delete by name + Settings card).
- Fix found live during heartbeat work: job reports mixed up steps when
  multiple jobs share a heartbeat window (2663dad).

## Done (observability & queue control, 2026-08-30)

- Full job logs: agent completion report carries log_full (last 1 MiB of
  run.log, line-cut aligned to UTF-8 rune boundaries + truncation marker;
  controller re-caps defensively) → jobs.full_log → GET /api/jobs/{id}/log
  (admin-auth, text/plain) → Jobs page log viewer dialog with download.
  Old agents omit the field; both sides tested incl. >1 MiB, no-newline,
  exact-boundary and CJK-corruption cases.
- Step timings: ENCODE_STEP markers timestamped LIVE at output arrival
  (lineObserver writer, no post-hoc scan) → step_timings_json → step-duration
  table + proportional bars in the log dialog. First-seen-wins per step,
  injected-clock tests, CRLF/BOM fixtures.
- Node telemetry: agent collects cpu/ram/disk (PS CimInstance, locale-safe
  parsing), gpu (nvidia-smi guarded, -1 when absent), encode fps (bounded
  64 KiB tail parse of the live run.log) with a 4s budget that can never
  block heartbeats → node_metrics ring (24h retention, per-node index-backed
  prune, ≤500-point downsample) → /api/nodes/{id}/metrics + last_metrics in
  the node list → UI chips, sparklines, fleet dashboard strip.
- Fleet stats: GET /api/stats?range=24h|7d|30d|all — totals, avg duration,
  per-node/flow breakdowns, failures-by-step, done-per-day, pure SQL over
  job history (no schema). Stats page with range picker.
- Retry policy: per-flow options_json (max_retries, backoff minutes; 0 = off,
  floor 1m cap 24h). Silent auto-retry with next_retry_at gate (visible in
  the Jobs table, unassignable until due); Discord only on final failure
  "(after N retries)"; orphan recovery honors the policy; manual retry
  clears the gate; stale-run fields cleared on re-queue.
- Priority + drain: jobs.priority (PATCH while pending) orders assignment
  priority DESC, id ASC (FIFO within tier); fleet drain mode (live setting)
  pauses assignment only — running jobs finish, reboot/update unaffected.
- Series progress: GET /api/series enriched with episodes done/failed/active/
  total ("eventually done wins"; total from scaffolded Ep * folders with
  jobs-derived fallback) → Progress column with bar + counts.
- Notifications: per-series mute (🔔/🔕 toggle), Discord deep links to
  /jobs?job=<id> (auto-opens the log viewer), optional hourly digest mode
  (in-memory buffer, one summary/hour, empty ticks silent, restart-loss by
  design).
- verify_output built-in step (seeded after mux in new default flows;
  existing fleets' flows untouched): MKV exists non-empty, ≥1 video + ≥1
  audio track via mkvmerge -J, duration vs source (MediaInfo ±2s) with a
  >60s sanity fallback. Byte-guarded factory; stubbed pwsh E2E (pass,
  missing file, zero-length, no-audio).
- Docs: Architecture (contracts + feature-set section), Operations
  (11 new runbook rows), Deployment (no infra change), this KANBAN.
- Adversarial review round (GLM + DeepSeek over the full 31-commit diff,
  chunked by subsystem; every finding verified against code before action):
  4 fixed with regression tests — captureRunLog OOM on multi-GB logs
  (bounded tail-window read), verify_output vs the REAL tools (mkvmerge
  schema v20 nests duration at container.properties.duration in
  NANOSECONDS — verified against the official schema; MediaInfo Duration
  unit-normalized against the mkv ms value so ms/seconds builds both work;
  e2e stubs rebuilt to real shapes + both legacy fallback branches),
  completion reports orphaned by the global 1 MiB body cap (JSON escaping
  inflates a 1 MiB log past it — job-complete route now 4 MiB), flow PUT
  silently dropped the retry policy (handleUpdateFlow copied only
  Name/Steps). Also hardened: FinishJobWithReport guarded against
  concurrent cancel (ErrJobNotFinishable), manual retry refreshes the
  auto-retry budget (retry_count=0), Discord deep links survive
  truncation, atomic PatchPendingJob, orphan-recovery GetJob errors
  logged, fractional retry-field validation, keyed Fragment row fix.
  Rejected with evidence: digest buffer "unsynchronized" (mutex present),
  ?range overflow (exact-string switch), orphan reorder hazard (terminal
  idempotency + node-ownership guards), "new" none instruction (predates
  drain).
- Live-verified on the test controller (172.24.92.232, 1.12.0-observability,
  binary md5-matched after compose rebuild; DB snapshotted pre-deploy):
  stats/metrics/log endpoints (auth + empty-state shapes), verify_output
  seeded, flow retry-policy PUT→GET round-trip, drain toggle live
  round-trip, series progress counts computed from real job history
  (4K Test: 3/3), old agents (0.9.0, offline) coexist — new fields
  optional end to end. Agent push to 219/229 deferred to the operator.

## Done (default-4k flow)

- Seeded default-4k flow (8 steps): source_rename → dgindex → hdr_probe →
  audio_lang (jpn,eng @320k) → encode_4k → mux → release_copy → keyframes.
  Boot-seeded alongside default-1080 (name-guarded; operator's default-flag
  choice preserved; custom ENCODE_DEFAULT_FLOW names still seeded as older
  builds did). Guard test pins every step to a builtin template.
  Live-verified on the controller; default flag stays on default-1080.

## Done (bin folder distribution via GitHub — encode-bin repo)

- Public repo badskater/encode-bin: source of truth + distribution for the
  node tools folder. Bin packages ship as GitHub Release assets (144 MB >
  git's 100 MB commit-file cap), tagged v<bin_version>; release notes carry
  the SHA-256. First release v3 = current fleet package (219's C:\bin with
  dovi_tool), sha ca62575b… verified byte-for-byte.
- Controller: new POST /api/updates/bin/url — fetches a package from a URL
  (http(s) only, streamed to a temp file under the 1 GiB cap, optional
  SHA-256 pin, same zip-slip/symlink validation as uploads, version counter
  enforced). UI: Settings → Push to nodes → "Fetch & publish" form.
  2 API tests (happy path + validation matrix incl. scheme guard, sha
  mismatch, 404 upstream, corrupt zip, version conflict).

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
