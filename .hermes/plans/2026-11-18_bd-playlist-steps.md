# BD playlist steps (MPLS → M2TS split/merge) — approved 2026-11-18

User ask: split M2TS by MPLS chapters, merge M2TS per MPLS; **output stays M2TS**
(eac3to audio chain). Engine: **tsMuxeR v3.1.0, teaching-droid/tsMuxer fork**
(Apache-2.0, active). User explicit: must work on Linux too.

## Verified ground (probe 2026-11-18, scratch/tsmuxer-probe)

- sha256-verified release assets; CLI runs headless on Debian 13 (needs
  libfreetype6/libpng16/libbrotli — AppImage does NOT bundle them).
- `--split-chapters` splits at MPLS marks (keyframe-aligned); `--playlist-clips
  played` default trims clips to PlayItem IN/OUT; MPLS meta lines expand per
  PlayItem (seamless branching); `+` joins loose m2ts; `--cut-start/--cut-end`
  for chapter ranges; detection mode prints tracks + `Marks:` line.
- Synthetic disc round-trip (ffmpeg testsrc2 1s GOPs → tsMuxeR --blu-ray
  --custom-chapters): merge 30.02s, split 3×10.02s, rejoin 30.02s, all decode
  CLEAN. Full recipe: skill encode-system-fleet-ops references/tsmuxer-bd-operations.md.

## Design

### New built-in steps (PS + bash semantic parity, repo conventions)

- `bd_merge`: resolve playlist → tsMuxeR detection → generate .meta (all tracks
  referencing the mpls) → mux to `src.m2ts` + `chapters.xml` sidecar (Matroska
  XML from Marks; M2TS cannot hold chapters — mux_extras consumes the sidecar).
- `bd_split`: same input; mode `chapters` (default) → `--split-chapters` →
  rename `*.split.N.m2ts` → `chapter-NN.m2ts` + per-part `chapter-NN.xml`
  (rebased marks); mode `range` + `range=2-5` → `--cut-start/--cut-end` at mark
  times → single `src.m2ts` + chapters.xml for the kept range.
- Params: `playlist` (optional; name or rel path, sanitized, no `..`/absolute),
  `mode`, `range`. Playlist auto-discovery: `BDMV/PLAYLIST/*.mpls` then flat
  `*.mpls`; exactly one → use; several → loud error listing candidates; none →
  loud error. No marks: merge proceeds (log, no sidecar), split fails loudly.
- Tool resolution: PS `Resolve-Tool $Job.BinDir 'tsMuxeR.exe'`; bash
  `command -v tsMuxeR` → `$BinDir/tsMuxeR` with `LD_LIBRARY_PATH=$BinDir/tsMuxeR-lib`
  when that dir exists (bundled-libs layout).
- Guards: missing tool / missing playlist / bad range / nonzero tsMuxeR /
  missing outputs → ENCODE_STEP_FAILED + exit 1 (bash) / throw (PS).
- Markers: 0 / 30 (detected) / 60 (meta+mux) / 100.
- Files: `builtin_steps_bd.go`, `builtin_steps_bd_linux.go`; register in
  BuiltinStepTemplates() + builtinBashSources.

### Scanner BDMV support (both scanners, parity)

- inspectEpisode / inspectS3Episode: when no flat source, accept
  `BDMV/PLAYLIST/*.mpls` + `BDMV/STREAM/*.m2ts` presence; SourceFile =
  playlist rel path (informational; scripts find source at runtime).
  Stability gate uses newest mtime/LastModified across the BDMV files.
- S3 grouping currently drops depth>2 keys: record BDMV playlist/stream keys
  per episode dir instead of ignoring them. Agent S3 staging already downloads
  the whole episode prefix (verify s3transfer).

### Tool distribution (operational, existing bin-package mechanism)

- Bin package zip gains: `tsMuxeR.exe` + Windows DLLs (windows-x64.zip),
  `tsMuxeR` (linux ELF from linux-x64.zip) + `tsMuxeR-lib/` (freetype/png/brotli
  .so for minimal hosts). Publish via POST /api/bin (version bump) during live
  phase; nodes self-sync. sha256-verify downloads from the fork's releases ONLY.
- linux-node ansible role: add freetype/libpng/brotli system packages (apt+dnf).

## Phases / commits (one PR: feat/bd-playlist-steps)

1. Plan + branch + early PR.
2. Scanner BDMV support — TDD (unit tests first: fs + s3 fixtures).
3. bd_merge/bd_split PS + bash + registration — render e2e + guard tests.
4. Linux exec tests: tsMuxeR stub in stubBinDir + dedicated bd exec tests;
   REAL-binary gated test (ENCODE_TEST_TSMUXER_BIN + ffmpeg, PG-gated pattern)
   executed HERE against the synthetic fixture = the Linux proof.
5. Provision libs + docs (Architecture/Operations/KANBAN/README) + skill sync.
6. CI green → adversarial-model-review → fix → squash merge → tag → release.
7. Live: upgrade farm (release runbook), publish bin package w/ tsMuxeR,
   synthetic-fixture episode job on a Windows node, verify outputs + chapters;
   report. (Farm upgrade + bin publish = the approved phase-4 live smoke.)

## Out of scope (v1)

- Split fan-out into separate episode jobs (files land in episode dir).
- UI playlist picker / controller-side MPLS parser (detection listing suffices).
- ISO input (tsMuxeR supports it natively; steps take folders/files).
