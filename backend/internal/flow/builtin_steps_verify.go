// Post-mux output integrity verification step.
//
// verify_output runs AFTER the mux step and locates the EXACT artifact mux
// produces (Join-Path $Job.EpisodeDir $Job.OutputName — the mux step writes
// there; release_copy is what copies it into the release folder). It fails
// the job when the file is missing/zero-length, lacks a video track, or
// lacks an audio track, and performs a duration sanity check.
//
// Duration-integrity approach: the original plan wanted a ±2s comparison
// against the source media file. In practice the source is discovered by
// Find-SourceFile (src.* in $Job.EpisodeDir) and its duration is read with
// MediaInfo. That comparison is ROBUST when a source media file is present
// (the common case: the flow ran source_rename and the raw is still there),
// so verify_output does it when a source is discoverable. When the source
// has been removed mid-flow or Find-SourceFile throws, the step falls back
// to a SANITY check: the MKV duration must be > 0 ms and > 60 seconds. The
// fallback never fails a job whose MKV simply has no source to compare
// against — it only catches the degenerate (truncated/empty-timeline) case.
// The chosen mode is reported on the ENCODE_STEP log line.
//
// REAL TOOL UNIT CONVENTIONS (adversarial review — unit mismatch fix):
//
//   - mkvmerge -J (JSON identify, schema v20) reports the segment duration
//     at `container.properties.duration` in NANOSECONDS ("The file's/
//     segment's duration in nanoseconds" — a 24-min episode reports
//     1440000000000; verified against mkvtoolnix.download/doc/
//     mkvmerge-identification-output-schema-v20.json). container.duration
//     and a top-level duration do NOT exist in schema-v20 output; the code
//     reads container.properties.duration first and falls back to those two
//     legacy shapes defensively, treats the raw value as NANOSECONDS, and
//     converts to ms via [int64]([double]$rawNs / 1000000.0). Treating ns
//     as ms made source-compare fail with a ~10^6× delta and sanity pass
//     trivially (a huge number clears the 60s floor without verifying
//     anything); reading the wrong nesting made the duration always 0 and
//     failed every real job at the "zero or missing" check.
//
//   - MediaInfo CLI --Output=JSON General-track `Duration` units differ
//     across builds: modern builds emit milliseconds (float); some emit
//     seconds. The step UNIT-NORMALIZES the raw value against the now-correct
//     mkvmerge ms value: it evaluates the raw value as ms, seconds (×1000),
//     µs (÷1000), and ns (÷1e6), then picks the interpretation minimizing the
//     absolute delta to $mkvDurationMs. This makes the check correct
//     regardless of the installed MediaInfo version. Normalization is needed
//     because operators run mixed MediaInfo builds and the source-compare
//     tolerance (±2s) is meaningless if the source is off by a unit scale.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

// VerifyOutputFactoryV1 is the factory verify_output script as first shipped.
// Boot uses it for a guarded upgrade: the template is only replaced when the
// stored script still equals this version byte-for-byte (user edits block the
// upgrade and stay in effect).
const VerifyOutputFactoryV1 = `function Invoke-VerifyOutput {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP verify_output 0"
    # Locate the muxed MKV — the SAME path the mux step writes to
    # (Join-Path $Job.EpisodeDir $Job.OutputName). release_copy is the step
    # that later moves this into $ReleaseDir/$ReleaseFolder; verify_output
    # runs between them so it checks the mux artifact directly.
    $mkv = Join-Path $Job.EpisodeDir $Job.OutputName
    if (-not (Test-Path -LiteralPath $mkv)) {
        throw "verify_output: muxed MKV not found: $mkv (run AFTER the mux step)"
    }
    $size = (Get-Item -LiteralPath $mkv).Length
    if ($size -le 0) {
        throw "verify_output: muxed MKV is zero-length: $mkv"
    }
    Write-Output "[verify] MKV: $mkv ($size bytes)"

    # mkvmerge -J emits a JSON identify (container + tracks). The mux step
    # already validated mkvmerge via Resolve-Tool; locate it the same way.
    $mkvmerge = Resolve-Tool $Job.BinDir 'mkvmerge.exe'
    $raw = & $mkvmerge -J $mkv
    if ($LASTEXITCODE -ne 0) {
        throw "verify_output: mkvmerge -J exited with code $LASTEXITCODE"
    }
    # PS 5.1 pipes native output line-by-line; join before parsing (same
    # technique as media_probe / hdr_probe).
    $ident = ([string]::Join([char]10, $raw)) | ConvertFrom-Json

    # Track presence: a valid release must have at least one video AND one
    # audio track. Report which is missing so the failure is self-explanatory.
    $videoTracks = @($ident.tracks | Where-Object { $_.type -eq 'video' })
    $audioTracks = @($ident.tracks | Where-Object { $_.type -eq 'audio' })
    if ($videoTracks.Count -eq 0) {
        throw "verify_output: no video track in $mkv (tracks found: $($ident.tracks.Count))"
    }
    if ($audioTracks.Count -eq 0) {
        throw "verify_output: no audio track in $mkv (tracks found: $($ident.tracks.Count))"
    }
    Write-Output "[verify] tracks: $($videoTracks.Count) video, $($audioTracks.Count) audio"

    # Container duration. mkvmerge -J (JSON identify, schema v20) reports the
    # segment duration at container.properties.duration in NANOSECONDS ("The
    # file's/segment's duration in nanoseconds" — e.g. a 24-min episode =
    # 1440000000000 ns). NOTE the nesting: container.duration does NOT exist
    # in real output (the schema is additionalProperties:false with
    # properties/recognized/supported/type); older code and test stubs that
    # assumed container.duration always read zero against the real tool.
    # Read container.properties.duration first, then container.duration,
    # then top-level duration as legacy fallbacks. Convert ns to ms so the
    # source-compare delta and the 60s sanity floor compare real values.
    $mkvDurationMs = 0
    $rawNs = 0.0
    $containerOk = ($null -ne $ident.PSObject.Properties['container'] -and $null -ne $ident.container)
    if ($containerOk -and $null -ne $ident.container.PSObject.Properties['properties'] -and $null -ne $ident.container.properties -and $ident.container.properties.PSObject.Properties['duration']) {
        try { $rawNs = [double]$ident.container.properties.duration } catch { }
    }
    if ($rawNs -le 0 -and $containerOk -and $ident.container.PSObject.Properties['duration']) {
        try { $rawNs = [double]$ident.container.duration } catch { }
    }
    if ($rawNs -le 0 -and $ident.PSObject.Properties['duration']) {
        try { $rawNs = [double]$ident.duration } catch { }
    }
    if ($rawNs -gt 0) {
        $mkvDurationMs = [int64]([double]$rawNs / 1000000.0)
    }

    # Duration integrity. Two modes:
    #  (a) SOURCE-COMPARE (preferred): Find-SourceFile locates the episode's
    #      source media (src.* in $Job.EpisodeDir), MediaInfo reads its
    #      duration, and the MKV must be within ±2s. This catches a mux that
    #      silently dropped part of the encode.
    #  (b) SANITY-CHECK (fallback): when no source is discoverable (the raw
    #      was deleted mid-flow, or Find-SourceFile throws), require the MKV
    #      duration to be > 0 ms and > 60 s. This catches the degenerate
    #      truncated/empty-timeline case without failing a job that simply
    #      has no source to compare against.
    # The chosen mode is reported so operators reading the log know which
    # guarantee was applied.
    $sourceDurationMs = 0
    $mode = 'sanity'
    $src = ''
    try {
        $src = Find-SourceFile $Job.EpisodeDir
    } catch {
        $src = ''
    }
    if ($src) {
        $mi = $null
        foreach ($candidate in @((Join-Path $Job.BinDir 'MediaInfo.exe'), 'C:\Program Files\MediaInfo\MediaInfo.exe', 'C:\Program Files (x86)\MediaInfo\MediaInfo.exe')) {
            if (Test-Path -LiteralPath $candidate) { $mi = $candidate; break }
        }
        if ($mi) {
            try {
                $rawMi = & $mi --Output=JSON $src
                if ($LASTEXITCODE -eq 0) {
                    $info = ([string]::Join([char]10, $rawMi)) | ConvertFrom-Json
                    $tracks = @($info.media.track)
                    $general = $tracks | Where-Object { $_.'@type' -eq 'General' } | Select-Object -First 1
                    if ($general -and $general.PSObject.Properties['Duration']) {
                        # MediaInfo General Duration units differ across
                        # builds (modern: ms float; some: seconds). Parse
                        # the raw value and unit-normalize against the now-
                        # correct mkvmerge ms value by evaluating each
                        # candidate interpretation (raw as ms, raw*1000 =
                        # seconds, raw/1000 = micros, raw/1e6 = ns) and
                        # keeping the one minimizing the absolute delta.
                        $rawDuration = 0.0
                        try { $rawDuration = [double]$general.Duration } catch { }
                        if ($rawDuration -gt 0 -and $mkvDurationMs -gt 0) {
                            $candidates = @(
                                @{ name = 'ms';          ms = [int64]$rawDuration },
                                @{ name = 'seconds';     ms = [int64]([double]$rawDuration * 1000.0) },
                                @{ name = 'microseconds';ms = [int64]([double]$rawDuration / 1000.0) },
                                @{ name = 'nanoseconds';  ms = [int64]([double]$rawDuration / 1000000.0) }
                            )
                            $best = $null
                            $bestDelta = [double]::MaxValue
                            foreach ($c in $candidates) {
                                $d = [math]::Abs($mkvDurationMs - $c.ms)
                                if ($d -lt $bestDelta) {
                                    $bestDelta = $d
                                    $best = $c
                                }
                            }
                            if ($best -and $best.ms -gt 0) {
                                $sourceDurationMs = $best.ms
                                Write-Output "[verify] source duration normalized: raw $rawDuration -> $sourceDurationMs ms (interpreted as $($best.name))"
                            }
                        }
                    }
                    if ($sourceDurationMs -gt 0) {
                        $mode = 'source-compare'
                        Write-Output "[verify] source: $src ($sourceDurationMs ms)"
                    } else {
                        Write-Output "[verify] source duration unreadable; falling back to sanity check"
                    }
                } else {
                    Write-Output "[verify] MediaInfo exited $LASTEXITCODE on source; falling back to sanity check"
                }
            } catch {
                Write-Output "[verify] MediaInfo failed on source ($($_.Exception.Message)); falling back to sanity check"
            }
        } else {
            Write-Output "[verify] MediaInfo not found; falling back to sanity check"
        }
    }

    Write-Output "[verify] MKV duration: $mkvDurationMs ms (mode: $mode)"
    if ($mkvDurationMs -le 0) {
        throw "verify_output: MKV duration is zero or missing (mkvmerge reported no duration)"
    }
    if ($mode -eq 'source-compare') {
        $delta = [math]::Abs($mkvDurationMs - $sourceDurationMs)
        Write-Output "[verify] duration delta vs source: $delta ms (tolerance 2000)"
        if ($delta -gt 2000) {
            throw "verify_output: MKV duration ($mkvDurationMs ms) differs from source ($sourceDurationMs ms) by $delta ms (tolerance 2000)"
        }
    } else {
        # Sanity check: a real episode is longer than a minute. 60s catches
        # a truncated encode (the mux would still produce a playable but
        # short file); it never falsely fails a legitimate short clip because
        # this step runs on real encode jobs (always > 1 min).
        if ($mkvDurationMs -lt 60000) {
            throw "verify_output: MKV duration $mkvDurationMs ms is below the 60s sanity floor (no source to compare against)"
        }
    }
    Write-Output "ENCODE_STEP verify_output 100"
}`

// VerifyOutputTemplate returns the current factory verify_output template
// (exported for the guarded factory upgrade in boot seeding).
func VerifyOutputTemplate() *model.StepTemplate { return verifyOutputTemplate() }

func verifyOutputTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "verify_output",
		Label:       "Verify output (post-mux)",
		Description: "Post-mux integrity check: confirms the release MKV exists and is non-empty, has at least one video AND one audio track, and passes a duration check. Duration is compared against the source media (±2s) when a source is discoverable via Find-SourceFile + MediaInfo; otherwise a sanity floor (60s) is applied. mkvmerge reports the segment duration at container.properties.duration in nanoseconds (converted to ms; schema v20); MediaInfo General Duration is unit-normalized against the mkv value because builds differ (ms vs seconds). Place AFTER mux, BEFORE release_copy so it checks the mux artifact directly.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell:  VerifyOutputFactoryV1,
	}
}
