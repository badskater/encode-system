// Perceptual quality gate step (VMAF with SSIM fallback).
//
// quality_gate runs AFTER verify_output (integrity) and BEFORE release_copy:
// it compares the muxed encode against the episode's source media with
// ffmpeg and FAILS the job when the perceptual score is below the threshold.
// verify_output proves the file is structurally sound; quality_gate proves
// the encode actually looks like the source.
//
// DESIGN DECISIONS (documented in Docs/md/Operations.md):
//
//   - Sampling: ffmpeg `select='not(mod(n\,N))'` on BOTH inputs keeps every
//     Nth decoded frame (identical frame indices on both sides, so pairing
//     is exact), `setpts=N/FRAME_RATE/TB` re-timestamps the survivors to
//     consecutive frames, and the comparison filter runs on the reduced
//     stream. This was chosen over libvmaf's n_subsample because (a) the
//     ssim fallback filter has no subsample option — one mechanism serves
//     both metrics, (b) scaling/format conversion after select only touches
//     the sampled frames, and (c) decode cost is unchanged either way, so
//     there is no advantage to deferring subsampling into libvmaf.
//     Default N=12 (~2 comparisons/second at 24fps): a 24-minute episode
//     compares ~2,900 frames instead of ~34,500.
//
//   - Resolution/bit-depth normalization: the source is scaled to the
//     encode's dimensions with scale2ref (the encode may be resized), and
//     BOTH streams are converted to yuv420p10le — the farm's normal case is
//     an 8-bit BD source vs a 10-bit x265 encode, and libvmaf/ssim require
//     identical pixel formats on both inputs.
//
//   - libvmaf probe + LOUD fallback: `ffmpeg -filters` is probed for
//     libvmaf; when absent the step logs a WARNING and falls back to SSIM.
//     A blank threshold then defaults to the SSIM scale (0.95, not 90); an
//     explicit VMAF-scale threshold (>1) fails loudly instead of silently
//     comparing SSIM against 90. fallback_used is recorded in quality.json.
//
//   - Artifacts: quality.json in the episode workdir (metric, score,
//     threshold, sampled_frames, fallback_used) plus ENCODE_METRIC lines.
//     metrics_json is float-only, so quality_metric is numeric: 1=VMAF,
//     0=SSIM (the human-readable name lives in quality.json and the log).
//
//   - Failure classes are distinct: missing ffmpeg / missing inputs /
//     ffmpeg crash / unparseable score are TOOL errors; only a parsed
//     score below the threshold is a QUALITY rejection. Both fail the step
//     (ENCODE_STEP_FAILED) but the messages tell the operator which happened.
//
//   - Artifact paths inside the filtergraph are RELATIVE: ffmpeg runs with
//     the episode dir as its working directory so log_path/stats_file never
//     need filter-escaping of drive-letter colons or backslashes (Windows).
package flow

import "github.com/badskater/encode-system/backend/internal/model"

// Quality gate defaults. The scripts below are self-contained constants, so
// these values are duplicated there on purpose; the render tests assert the
// scripts still carry them (drift guard).
const (
	// QualityGateDefaultSampling compares every 12th frame (~2 samples/sec
	// at 24fps) — fast enough for a full episode, dense enough to catch
	// localized quality drops.
	QualityGateDefaultSampling = "12"
	// QualityGateVMAFThreshold: VMAF < 90 is visibly degraded for anime.
	QualityGateVMAFThreshold = "90"
	// QualityGateSSIMThreshold: SSIM < 0.95 indicates structural loss.
	QualityGateSSIMThreshold = "0.95"
)

// QualityGateTemplate returns the factory quality_gate template.
func QualityGateTemplate() *model.StepTemplate { return qualityGateTemplate() }

func qualityGateTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "quality_gate",
		Label:       "Quality gate (VMAF/SSIM)",
		Description: "Perceptual quality check: compares the muxed encode against the source with ffmpeg (sampled every Nth frame, default 12) and fails the job when the score is below the threshold. metric=vmaf (default) needs an ffmpeg built with libvmaf; without it the step warns loudly and falls back to SSIM. Blank threshold defaults to 90 for VMAF / 0.95 for SSIM (VMAF range 0-100, SSIM range 0-1). Writes quality.json in the episode dir and reports ENCODE_METRIC quality_score (quality_metric: 1=VMAF, 0=SSIM). Place AFTER verify_output, BEFORE release_copy.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "metric", Label: "Metric (vmaf or ssim)", Default: QualityGateDefaultMetric},
			{Key: "threshold", Label: "Minimum score (blank: 90 VMAF / 0.95 SSIM)", Type: "number", Placeholder: "90"},
			{Key: "sampling", Label: "Compare every Nth frame (1 = all)", Type: "number", Default: QualityGateDefaultSampling},
		},
		PowerShell: qualityGatePS,
	}
}

// QualityGateDefaultMetric is the metric used when the param is blank.
const QualityGateDefaultMetric = "vmaf"

const qualityGatePS = `function Invoke-QualityGate {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP quality_gate 0"
    # Same artifact contract as verify_output: the mux step writes
    # Join-Path $Job.EpisodeDir $Job.OutputName; the source is discovered
    # by Find-SourceFile (src.* in the episode dir).
    $mkv = Join-Path $Job.EpisodeDir $Job.OutputName
    if (-not (Test-Path -LiteralPath $mkv)) {
        throw "quality_gate: muxed MKV not found: $mkv (run AFTER the mux step)"
    }
    $src = ''
    try { $src = Find-SourceFile $Job.EpisodeDir } catch { $src = '' }
    # Find-SourceFile's fallback picks ANY media file in the dir — with no
    # source present that can be the output MKV itself, directly OR via a
    # symlink/hardlink. Comparing the encode against itself would fake a
    # perfect score, so compare FILE IDENTITY (Test-SameFile), not paths.
    if ($src -and (Test-SameFile $src $mkv)) {
        $src = ''
    }
    if (-not $src) {
        throw "quality_gate: no source media file found in $($Job.EpisodeDir) (quality_gate compares the encode against the source)"
    }
    $ffmpeg = Resolve-Tool $Job.BinDir 'ffmpeg.exe'

    # --- params + validation (mirrored by the bash port) ---
    $metric = if ($Params.metric) { ([string]$Params.metric).Trim().ToLowerInvariant() } else { 'vmaf' }
    if ($metric -ne 'vmaf' -and $metric -ne 'ssim') {
        throw "quality_gate: invalid metric '$metric' (expected vmaf or ssim)"
    }
    $sampling = 12
    if ($Params.sampling) {
        $s = ([string]$Params.sampling).Trim()
        if ($s -notmatch '^[0-9]+$' -or $s.Length -gt 4) {
            throw "quality_gate: sampling must be an integer 1-1000 (got '$s')"
        }
        $sampling = [int]$s
        if ($sampling -lt 1 -or $sampling -gt 1000) {
            throw "quality_gate: sampling must be an integer 1-1000 (got '$sampling')"
        }
    }
    $threshold = if ($metric -eq 'vmaf') { 90.0 } else { 0.95 }
    $thresholdExplicit = $false
    if ($Params.threshold) {
        $t = ([string]$Params.threshold).Trim()
        if ($t -notmatch '^[0-9]+(\.[0-9]+)?$' -or $t.Length -gt 12) {
            throw "quality_gate: threshold must be numeric (got '$t')"
        }
        $threshold = [double]::Parse($t, [System.Globalization.CultureInfo]::InvariantCulture)
        $thresholdExplicit = $true
    }
    $max = if ($metric -eq 'vmaf') { 100.0 } else { 1.0 }
    if ($threshold -lt 0 -or $threshold -gt $max) {
        throw "quality_gate: threshold $threshold out of range for $metric (0-$max)"
    }

    # --- frame-count parity (ffprobe) ---
    # A source/encode frame-count mismatch (IVTC, trims, dropped frames)
    # silently MISPAIRS frames in the comparison and scores stale frames.
    # Diagnose it up front with ffprobe (ships with ffmpeg). ffprobe missing
    # is non-fatal: warn loudly and skip the check rather than break the gate.
    function Get-QgFrameCount {
        param([string]$Probe, [string]$Path)
        # nb_frames from container metadata first; MKV commonly reports N/A,
        # so fall back to counting packets. Empty output = count unknown.
        $queries = @(
            ,@('-v','error','-select_streams','v:0','-show_entries','stream=nb_frames','-of','default=noprint_wrappers=1:nokey=1')
            ,@('-v','error','-select_streams','v:0','-count_packets','-show_entries','stream=nb_read_packets','-of','default=noprint_wrappers=1:nokey=1')
        )
        $prevEAP = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try {
            foreach ($q in $queries) {
                $raw = & $Probe @q $Path 2>$null
                if ($LASTEXITCODE -ne 0) { return $null }
                $n = ([string]::Join('', @($raw | ForEach-Object { [string]$_ }))).Trim()
                if ($n -match '^[0-9]+$' -and $n.Length -le 15) { return [long]$n }
            }
        } finally { $ErrorActionPreference = $prevEAP }
        return $null
    }
    $ffprobe = Join-Path $Job.BinDir 'ffprobe.exe'
    if (-not (Test-Path -LiteralPath $ffprobe)) {
        # Select-Object -First 1: Get-Command can return multiple PATH
        # matches; .Source on the array would stringify into one bad name.
        $probeCmd = Get-Command 'ffprobe' -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
        $ffprobe = if ($probeCmd) { $probeCmd.Source } else { $null }
    }
    if ($ffprobe) {
        $nOut = Get-QgFrameCount $ffprobe $mkv
        $nSrc = Get-QgFrameCount $ffprobe $src
        if ($null -ne $nOut -and $null -ne $nSrc -and $nOut -ne $nSrc) {
            throw "quality_gate: frame-count mismatch: encode has $nOut frames but source has $nSrc frames (IVTC/trim drift?) - comparison would mispair frames"
        }
    } else {
        Write-Output "[quality] WARNING: ffprobe not available - skipping frame-count mismatch check"
    }

    # --- libvmaf probe + loud SSIM fallback ---
    $fallbackUsed = $false
    $effective = $metric
    if ($metric -eq 'vmaf') {
        $prevEAP = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try { $filtersRaw = & $ffmpeg -hide_banner -filters 2>&1 } finally { $ErrorActionPreference = $prevEAP }
        if ($LASTEXITCODE -ne 0) {
            throw "quality_gate: ffmpeg -filters exited with code $LASTEXITCODE (cannot probe libvmaf support)"
        }
        $filtersText = [string]::Join([char]10, @($filtersRaw | ForEach-Object { [string]$_ }))
        if ($filtersText -notmatch '\blibvmaf\b') {
            Write-Output "[quality] WARNING: ffmpeg has no libvmaf support - falling back to SSIM. SSIM scores are NOT comparable to VMAF thresholds."
            $fallbackUsed = $true
            $effective = 'ssim'
            if (-not $thresholdExplicit) {
                $threshold = 0.95
            } elseif ($threshold -gt 1.0) {
                throw "quality_gate: threshold $threshold is VMAF-scale but libvmaf is unavailable; set an SSIM threshold (0-1) explicitly"
            }
        }
    }
    $invCulture = [System.Globalization.CultureInfo]::InvariantCulture
    $thrStr = $threshold.ToString('0.####', $invCulture)
    Write-Output "[quality] metric: $effective (requested: $metric), threshold: $thrStr, sampling: every $sampling frame(s), fallback: $fallbackUsed"
    Write-Output "ENCODE_STEP quality_gate 20"

    # --- sampled comparison ---
    # select every Nth decoded frame on BOTH inputs (identical indices keep
    # the pairing exact), re-timestamp to consecutive frames, normalize to
    # 10-bit 4:2:0 (8-bit source vs 10-bit encode is the farm's normal case),
    # scale the source to the encode's dimensions, then libvmaf/ssim.
    # NOTE: scale2ref is deprecated (ffmpeg 7.1+); the planned drop-in is
    # scale=rw:rh (reference width/height). Keep the reference-FIRST operand
    # order when migrating — swapped operands silently flip the scores.
    $sel = "select='not(mod(n\,$sampling))',setpts=N/FRAME_RATE/TB,format=yuv420p10le"
    $graph = "[0:v]${sel}[dist];[1:v]${sel}[ref];[ref][dist]scale2ref[refs][dists];[dists][refs]"
    $filter = if ($effective -eq 'vmaf') { "${graph}libvmaf=log_fmt=json:log_path=quality_vmaf.json" } else { "${graph}ssim=stats_file=quality_ssim.log" }
    $ffArgs = @('-nostdin', '-hide_banner', '-nostats', '-i', $mkv, '-i', $src, '-lavfi', $filter, '-an', '-sn', '-dn', '-f', 'null', '-')
    Write-Output "[quality] ffmpeg $($ffArgs -join ' ')"
    # Run from the episode dir so filtergraph artifact paths stay relative
    # (no drive-letter colon / backslash escaping inside filter options).
    Push-Location -LiteralPath $Job.EpisodeDir
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $rawOut = @()
    try {
        $rawOut = @(& $ffmpeg @ffArgs 2>&1 | ForEach-Object {
            if ($_ -is [System.Management.Automation.ErrorRecord]) { $_.Exception.Message } else { [string]$_ }
        })
    } finally {
        $ErrorActionPreference = $prevEAP
        Pop-Location
    }
    $code = $LASTEXITCODE
    foreach ($line in $rawOut) { Write-Output "[ffmpeg] $line" }
    if ($code -ne 0) {
        throw "quality_gate: ffmpeg comparison failed with exit code $code (see [ffmpeg] lines above; this is a tool failure, not a quality verdict)"
    }

    # --- score parsing (distinct from a below-threshold verdict) ---
    $score = 0.0
    $scoreRaw = ''
    $scoreParsed = $false
    $sampledFrames = 0
    if ($effective -eq 'vmaf') {
        $vlogPath = Join-Path $Job.EpisodeDir 'quality_vmaf.json'
        if (-not (Test-Path -LiteralPath $vlogPath)) {
            throw "quality_gate: ffmpeg exited 0 but wrote no VMAF log ($vlogPath) - cannot parse a score"
        }
        try {
            $j = [System.IO.File]::ReadAllText($vlogPath) | ConvertFrom-Json
            if ($null -ne $j.PSObject.Properties['pooled_metrics'] -and $null -ne $j.pooled_metrics) {
                $vm = $j.pooled_metrics.PSObject.Properties['vmaf']
                if ($null -ne $vm -and $null -ne $vm.Value -and $null -ne $vm.Value.PSObject.Properties['mean']) {
                    $mean = $vm.Value.mean
                    # ConvertFrom-Json accepts NaN/Infinity literals, bools
                    # and strings; only a JSON NUMBER is a score candidate
                    # (finiteness + range are validated below).
                    if ($null -ne $mean -and $mean -isnot [bool] -and $mean -isnot [string]) {
                        $score = [double]$mean
                        $scoreRaw = $score.ToString($invCulture)
                        $scoreParsed = $true
                    }
                }
            }
            if ($scoreParsed) {
                # frames:null must count as 0 (bash jq length parity) —
                # @($null).Count is 1 and would fake a sampled frame.
                $framesProp = $j.PSObject.Properties['frames']
                $sampledFrames = if ($null -eq $framesProp -or $null -eq $framesProp.Value) { 0 } else { @($framesProp.Value).Count }
            }
        } catch { }
        if (-not $scoreParsed) {
            throw "quality_gate: could not parse VMAF score from $vlogPath (pooled_metrics.vmaf.mean missing)"
        }
    } else {
        # The ssim filter prints its summary to stderr: "SSIM Y:... All:<score> (...)".
        foreach ($line in $rawOut) {
            if ($line -match 'All:(-?[0-9]+(?:\.[0-9]+)?)') { $score = [double]::Parse($Matches[1], $invCulture); $scoreRaw = $Matches[1]; $scoreParsed = $true }
        }
        $sstatsPath = Join-Path $Job.EpisodeDir 'quality_ssim.log'
        if (Test-Path -LiteralPath $sstatsPath) { $sampledFrames = @(Get-Content -LiteralPath $sstatsPath).Count }
        if (-not $scoreParsed) {
            throw "quality_gate: could not parse SSIM score from ffmpeg output (no 'All:' summary line)"
        }
    }
    # Fail CLOSED on non-finite / out-of-range scores. ConvertFrom-Json
    # accepts NaN/Infinity literals and NaN -lt threshold is always FALSE,
    # so an unvalidated score would PASS the gate, write invalid JSON and
    # emit a metric line the agent silently drops. The range check also
    # bounds the fixed-decimal metric line below (no 309-digit numbers).
    $scoreMax = if ($effective -eq 'vmaf') { 100.0 } else { 1.0 }
    if (-not [double]::IsFinite($score) -or $score -lt 0 -or $score -gt $scoreMax) {
        throw "quality_gate: $effective score '$scoreRaw' is outside the valid range 0-$scoreMax (non-finite or out-of-range; failing closed)"
    }
    if ($sampledFrames -le 0) {
        throw "quality_gate: comparison produced no sampled frames (check that source and encode are playable video)"
    }
    Write-Output "ENCODE_STEP quality_gate 80"

    # --- artifact + metrics + verdict ---
    $scoreStr = if ($effective -eq 'vmaf') { $score.ToString('F4', $invCulture) } else { $score.ToString('F6', $invCulture) }
    $artifact = [ordered]@{
        metric         = $effective
        score          = [double]::Parse($scoreStr, $invCulture)
        threshold      = $threshold
        sampled_frames = $sampledFrames
        fallback_used  = $fallbackUsed
    }
    $artifactPath = Join-Path $Job.EpisodeDir 'quality.json'
    $artifact | ConvertTo-Json | Set-Content -LiteralPath $artifactPath -Encoding UTF8
    Write-Output "[quality] wrote $artifactPath"
    # metrics_json is float-only: quality_metric is numeric (1=VMAF, 0=SSIM);
    # the human-readable metric name lives in quality.json and the log line.
    Write-Output "ENCODE_METRIC quality_score=$scoreStr"
    $metricCode = if ($effective -eq 'vmaf') { 1 } else { 0 }
    Write-Output "ENCODE_METRIC quality_metric=$metricCode"
    Write-Output "[quality] $effective score $scoreStr vs threshold $thrStr ($sampledFrames sampled frames)"
    if ($score -lt $threshold) {
        throw "quality_gate: $effective score $scoreStr is below threshold $thrStr ($sampledFrames sampled frames) - encode quality rejected"
    }
    Write-Output "ENCODE_STEP quality_gate 100"
}`
