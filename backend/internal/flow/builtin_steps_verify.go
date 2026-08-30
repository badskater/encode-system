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

    # Container duration in ms (mkvmerge -J reports it on the container's
    # top-level duration field). Used for both the source-compare and the
    # sanity-check modes.
    $mkvDurationMs = 0
    if ($ident.PSObject.Properties['duration']) {
        try { $mkvDurationMs = [int64][double]$ident.duration } catch { }
    } elseif ($ident.container.PSObject.Properties['duration']) {
        try { $mkvDurationMs = [int64][double]$ident.container.duration } catch { }
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
                        try { $sourceDurationMs = [int64][double]$general.Duration } catch { }
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
		Description: "Post-mux integrity check: confirms the release MKV exists and is non-empty, has at least one video AND one audio track, and passes a duration check. Duration is compared against the source media (±2s) when a source is discoverable via Find-SourceFile + MediaInfo; otherwise a sanity floor (60s) is applied. Place AFTER mux, BEFORE release_copy so it checks the mux artifact directly.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell:  VerifyOutputFactoryV1,
	}
}
