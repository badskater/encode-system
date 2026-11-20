# Series scripts + scene analysis/zones — implementation plan

> **For Hermes:** Use subagent-driven-development skill to implement this plan task-by-task.

**Status (2026-11-20):** Phases 1–3 implemented on `feat/series-scripts-scene-zones` (store/scanner/API commits e17bfa0/6f39590/b19c02c, scene_detect 3228a13, zone_build+use_zones 27356e8); frontend delegated. Correction to the original context note: the scanner header did NOT contain a stale template comment — the mechanism is newly implemented, and the header now documents it.

**Goal:** Let a series carry its AviSynth/VapourSynth script so detected episodes are job-ready automatically, and add pre-encode scene analysis that builds encoder zones (and later per-scene filtering) from the detected scenes.

**Architecture:** Three additive phases on the existing flow-step system. (1) Series gain `script_type`/`script_template`; the scanner loop materializes the template into each episode at detection time (placeholder substitution, never overwrites an existing per-episode script). (2) New `scene_detect` builtin runs ffmpeg `scdet` + `metadata=print` over the source and publishes `scenes.json`. (3) New `zone_build` builtin turns scenes + chapter names + rules into `zones.json`; the `encode` step gains `use_zones` and appends `--zones` to x265. Per-scene *filter chains* (phase 4) are deferred — they need a rules-format discussion; zones cover the bitrate/quality case losslessly.

**Tech Stack:** Go (controller: model/store/scanner/api/flow), PowerShell + bash step dialects (semantic parity), ffmpeg (scdet), x265 `--zones`, SQLite+PG dual store, vitest frontend.

---

## Current context (verified in code)

- `model.Series` (backend/internal/model/model.go:564) has NO script fields. The scanner.go header comment (lines 10–13) describes a per-series template mechanism that **does not exist** — stale comment, fix it as part of phase 1.
- `scanner.Scan` is pure/read-only: a candidate needs source + script already present (`inspectEpisode`). Job creation happens in the scanner loop (backend/internal/scanner/loop*.go) — materialization belongs there, not in `Scan`.
- `encode` builtin (backend/internal/flow/builtin_steps.go:230): x265 fork reads `--input $Job.ScriptFile` (.avs/.vpy) directly; args assembled from named fields or raw `x265_args`; `--input`/`-o` appended LAST so params can't override them. Zones inject before that tail. Bash port lives in builtin_steps_linux.go (same contract).
- x265 zones (verified against videolan/x265 param.cpp + CLI docs): `--zones <start>,<end>,q=<int>/<start>,<end>,b=<float>`; q forces QP, b is a bitrate multiplier; overlapping zones: later wins. CRF+b is approximate (document); q= is exact.
- `keyframes` builtin is the precedent for analyze-before-encode steps (skip-if-fresh artifact convention, stub + gated real-binary tests, PS/bash parity).
- Linux nodes already have ffmpeg (decode-only audio path). Windows bin package has NO ffmpeg/ffprobe — phase 2 ships them in a bin add-on (same pattern as tsMuxeR v4/v5).
- BD synergy: `bd_merge` already writes `chapters.xml` (Matroska XML, chapter names preserved) — zone rules can key off chapter names (OP/ED/Credits).
- Farm: controller 172.24.92.232 v1.24.0, one idle Windows node (bin v5), scripts store = named volume `/data/scripts`; node scripts dir is node-local; WinRM firewalled (use bin-package + custom-step delivery for fixtures, per skill reference).

## Assumptions / defaults (user approves wholesale or overrides)

- Template location: `<scripts root>/_templates/<Series>/<file>` on the share (operator-editable). `_templates` becomes a RESERVED top-level name excluded from series scanning (fs + S3).
- Materialized script keeps the template's filename (e.g. `1080.vpy`); existing per-episode scripts always win.
- Placeholders: `{{source}}` (source filename), `{{series}}`, `{{episode}}` (episode folder name), `{{episode_dir}}` (relative dir, forward slashes). Unknown `{{...}}` left intact.
- scene_detect: threshold default 10, `min_scene_len` default 0 (off). Artifact `scenes.json`, skip when newer than source.
- zone_build defaults: credits `b=0.6` (first/last scene ≤ 90 s), OP/ED chapter-name matches `b=0.8`, manual overrides win. Artifact `zones.json` containing the ready `--zones` string + structured zones for audit.
- encode: `use_zones` bool default false; when true, `zones.json` must exist (fail closed). Works in both named-fields and raw `x265_args` modes (appended after raw args, before the input/output tail).
- Phase 4 (per-scene filter chains via generated VapourSynth Trim/Splice include + `{{scene_filters}}` placeholder) is DEFERRED — separate design pass with the user.

---

## Phase 1 — Series script templates

### Task 1.1: Model + store migration
**Files:** modify `backend/internal/model/model.go` (Series: `ScriptType string json:"script_type"`, `ScriptTemplate string json:"script_template"`), `backend/internal/store/store_ext.go` (ALTER TABLE series ADD COLUMN … tolerant pattern already used there), `backend/internal/store/store_pg.go` (PG schema), series CRUD scan/insert/update sites.
**Tests:** store contract round-trip (SQLite + PG gated) asserting the new fields survive create/update/list; migration idempotence.
**Verify:** `go test ./internal/store/ -count=1` (+ PG DSN gated run per skill recipe).

### Task 1.2: Scanner reserved prefix + loop materialization
**Files:** modify `backend/internal/scanner/scanner.go` (skip `_templates` in seriesDirs; fix stale header comment to describe the REAL mechanism), `backend/internal/scanner/s3scan.go` (same exclusion), loop file (materialize before job creation).
**Contract:** `materializeScript(root, series, candidate, templateDir)`:
- series.ScriptTemplate == "" → no-op (legacy behavior).
- episode already has a script → no-op (override wins).
- read `_templates/<Series>/<name>`, substitute the four placeholders, write atomically (temp+rename) into the episode dir; re-run `inspectEpisode` to build the candidate.
- template missing/unreadable → skip episode + log (fail closed, no job with a broken script).
- name validation: base name only, reject `/`, `\`, `..`, leading `.`.
**Tests (TDD):** table-driven loop tests — absent→materialized+substituted; present→untouched (byte compare); no-template→legacy; missing template→skipped+logged; traversal names rejected; `_templates` never scanned as a series; S3 parity test mirroring scanner_bdmv_test.go style.
**Verify:** `go test ./internal/scanner/ -count=1`.

### Task 1.3: API + validation
**Files:** `backend/internal/api/handlers_series.go` (create/patch accept new fields; validate type ∈ {"",vpy,avs}, template base-name rules; when set, template must exist under `_templates/<Series>/` → 400 otherwise), new `GET /api/series/{id}/templates` listing the dir (name+size+mtime), route in server.go.
**Tests:** handler tests — valid assignment, bad type, traversal, missing template 400, listing endpoint, patch clears fields with "".

### Task 1.4: Frontend
**Files:** series edit form (frontend/src/… series components — locate via `search_files("node_group", path="frontend/src")`), template picker fed by the new endpoint, type select.
**Tests:** vitest component tests (assignment renders, picker lists, validation errors surface); `npm run lint && npx tsc --noEmit && npm run test && npm run build`.

### Task 1.5: Docs + commit
Architecture.md (series fields, scanner materialization rule, reserved `_templates`), Operations.md runbook (authoring templates, placeholders, per-episode override), REPO_CONVENTIONS.md contract line, KANBAN. One commit per task; phase commit message references the stale-comment fix.

## Phase 2 — scene_detect builtin

### Task 2.1: Contract + PS template
**Files:** create `backend/internal/flow/builtin_steps_scene.go` (or extend builtin_steps.go), register in `BuiltinStepTemplates()`.
**Contract:**
- source via find_source_file (src.* preferred); ffprobe required (frame_count, fps, duration) — fail closed if absent/unusable.
- `ffmpeg -hide_banner -nostdin -i <source> -an -vf "scdet=threshold=<T>,metadata=print:file=<ep>/scdet.log" -f null -` (exact scdet option spelling verified against the real binary in Task 2.3 BEFORE freezing the stub).
- parse scdet.log: `frame:<N> pts:<…> pts_time:<T>` followed by `lavfi.scd.score=<S>` / `lavfi.scd.time=<T>` lines; boundary frames = score ≥ threshold.
- scenes.json: `{"source","fps","frame_count","threshold","min_scene_len","scenes":[{"index","start_frame","end_frame","start_time","end_time","score"}]}`; scene 0 starts at frame 0; last ends frame_count-1; min_scene_len merges shorter scenes into the previous one.
- skip when scenes.json exists and is newer than the source (keyframes convention); ENCODE_STEP markers 0/30/60/100; min-size guard on the log; loud failure on zero detected boundaries is NOT an error (single-scene video is valid).
**Tests:** render contract tests (markers, params, artifact names) mirroring bd/keyframes suites.

### Task 2.2: Bash port + registration
**Files:** `builtin_steps_linux.go` (bashSourceSceneDetect, builtinBashSources, builtinLinuxToolMap: ffmpeg+ffprobe), generic harness stub extension.
**Tests:** bash exec suite with contract-shaped ffmpeg/ffprobe stubs (log sidecars: normal, no-cuts, malformed-log, missing-ffprobe, stale-skip, min-scene-merge); parity assertions vs PS render.

### Task 2.3: Real-binary gated E2E + ffmpeg bin add-on
**Files:** `builtin_steps_scene_real_linux_test.go` (env-gated like TestBdRealBinaryLinuxEndToEnd): author a synthetic video with HARD CUTS (concat 3 distinct testsrc2/color segments via ffmpeg), run rendered bash, assert scenes.json boundaries match the authored cut points ±1 frame.
**Bin:** build `encode-bin-ffmpeg-<ver>.zip` add-on (BtbN static win64 ffmpeg.exe+ffprobe.exe; Linux nodes already provisioned) — publish as bin v6 during live phase; sha256-verify downloads.
**Verify:** `go test ./internal/flow/ -count=1` + gated real run; `GOOS=windows go build ./...`.

## Phase 3 — zone_build + encode zones

### Task 3.1: zone_build builtin (PS + bash)
**Contract:** inputs scenes.json (required, fail closed) + optional chapters.xml; params: `credits_b` (0.6), `credits_max_seconds` (90), `op_ed_b` (0.8), `name_patterns` (regex, default `OP|Opening|ED|Ending|Credits|Preview`), `overrides` (JSON array [{start_frame,end_frame,b|q}]).
**Rules precedence:** overrides > chapter-name matches (chapter time→frame via fps; name regex case-insensitive) > position heuristic (first/last scene ≤ credits_max_seconds). Output zones.json: `{"zones":[{start,end,mode:"b"|"q",value,reason}],"x265":"<start>,<end>,b=<v>/…"}` ordered so higher-precedence zones come LAST (x265 later-wins). Validate frames within [0,frame_count-1], start<end; b ∈ (0,10], q ∈ [0,51].
**Tests:** rule matrix (each source, precedence ordering, overlap, chapter-time→frame math, bad overrides rejected, missing scenes.json fails), stub exec both dialects, parity.

### Task 3.2: encode use_zones injection
**Files:** encodeTemplate (builtin_steps.go:230) + bash port: new param `use_zones` (bool, default false). When true: read zones.json, validate `x265` string shape `^[0-9]+,[0-9]+,(q=[0-9]+|b=[0-9]+(\.[0-9]+)?)(/[0-9]+,[0-9]+,(q=[0-9]+|b=[0-9]+(\.[0-9]+)?))*$`, append `--zones <string>` AFTER raw/named args and BEFORE the `--input`/`-o` tail. Missing/invalid zones.json → throw (fail closed). Log the injected zones.
**Tests:** render + exec tests — injection position (input/output still last), raw-args mode, invalid string refused, missing file refused, default-off unchanged (byte-compare legacy arg list).

### Task 3.3: Real-binary zone verification + docs
Gated E2E: scene_detect → zone_build on the cut fixture with a chapters.xml naming one segment "ED" → assert zones.json marks that frame range with b=0.8 and the string validates. Live-phase check: x265 fork `--fullhelp` zones support on the farm node (custom verify step). Docs: Architecture (2 new builtins, encode param), Operations (flow composition bd_merge→scene_detect→zone_build→encode, tuning guidance incl. CRF+b approximation), KANBAN, skill reference.

## Phase 4 — DEFERRED (per-scene filter chains)

Generated VapourSynth include applying per-scene Trim+filter+Splice from a rules JSON, injected via a `{{scene_filters}}` template placeholder (rides phase 1's materialization). Needs a rules-format design pass with the user (which filters, per-scene params, preview workflow). Do NOT implement in this batch.

---

## Files likely to change (summary)

- backend/internal/model/model.go, store/store_ext.go, store/store_pg.go (+ series CRUD)
- backend/internal/scanner/scanner.go, s3scan.go, loop*.go (+ tests)
- backend/internal/api/handlers_series.go, server.go (+ tests)
- backend/internal/flow/builtin_steps.go, builtin_steps_linux.go, NEW builtin_steps_scene.go, builtin_steps_zones.go (+ exec/real tests, harness stubs)
- frontend series form/components (+ vitest)
- Docs/md/Architecture.md, Operations.md, REPO_CONVENTIONS.md, KANBAN.md
- bin add-on: ffmpeg/ffprobe win64 (published v6, not committed)

## Tests / validation gates

- Per task: TDD red→green; `go test ./internal/<pkg>/ -count=1`.
- Phase gates: full `go test ./... -count=1` (PG-gated run per skill), `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`, gofmt, frontend lint/tsc/test/build, ansible untouched.
- Real-binary gated E2E for scene_detect + zone_build (synthetic cut fixture).
- Adversarial-model-review before merge (mandated); PR early; CI watch headSha-matched; squash merge; tag v1.25.0; release asset check.
- Live: farm upgrade (re-uncomment TLS mounts AFTER install.sh — known gotcha), bin v6 publish, fixture with cuts + OP/ED-named chapters, flow [bd_merge, scene_detect, zone_build] + verify step on Windows AND Linux (re-pair scratch agent); zones string + scenes.json asserted from job logs/artifacts. Full encode smoke optional (toolchain-heavy).

## Risks / tradeoffs / open questions

- **scdet log format drift** across ffmpeg versions → freeze contract against the shipped binary; parse defensively, fail closed; real-binary test is the gate.
- **x265 fork zones support** unverified on the JPSdr/Patman fork → live --fullhelp check before declaring done; fallback: `--zonefile` (same fork family) or document Windows-only limitation.
- **CRF + b-factor is approximate** (forum-verified): zones nudge quantizers, not hard bitrate caps. Document; use q= when exact QP is wanted.
- **Template substitution** must not mangle scripts with literal braces → only the four known keys substituted.
- **`_templates` exclusion** must cover fs + S3 scans and the series API (a series named `_templates` is refused).
- **Bin size**: ffmpeg static adds ~60–90 MB to the Windows bin channel — acceptable (add-on zip, additive overlay); Linux unaffected.
- Open (defaults proposed, user may override): zone factor defaults; whether OP/ED detection should also use scene-length heuristics mid-episode (proposed: no — chapter names + first/last only, keep deterministic); phase 4 timing.