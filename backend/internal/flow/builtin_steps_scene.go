// Package flow: scene_detect built-in — pre-encode scene analysis.
//
// Runs ffmpeg's scdet filter over the episode source (decode-only) and
// publishes scenes.json: frame-accurate scene boundaries for the zone
// builder. Contract (PS + bash parity):
//   - source via Find-SourceFile (src.* preferred), ffmpeg + ffprobe required
//   - skip when scenes.json exists and is newer than the source
//   - ffprobe: fps from r_frame_rate, frame_count from nb_frames (fallback
//     round(duration*fps) for containers without the metadata, e.g. m2ts)
//   - ffmpeg -vf scdet=threshold=<T>,metadata=print:file=<ep>/scdet.log
//   - boundaries = frames with lavfi.scd.score >= threshold (frame > 0);
//     min_scene_len merges shorter scenes into the previous one
//   - scenes.json written atomically; zero boundaries = one scene (valid)
//
// The scdet.log format is frozen against ffmpeg 9.x (see the gated
// real-binary test): "frame:<N> pts:<P> pts_time:<T>" lines followed by
// "lavfi.scd.mafd=<M>" / "lavfi.scd.score=<S>" metadata lines.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

func sceneDetectTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "scene_detect",
		Label:       "Scene detection (scdet)",
		Description: "Analyze the source with ffmpeg scdet and publish scenes.json (frame-accurate scene boundaries) for zone building. Decode-only; skips when scenes.json is newer than the source.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "threshold", Label: "Scene score threshold (0-100)", Type: "number", Default: "10"},
			{Key: "min_scene_len", Label: "Minimum scene length in frames (0 = off)", Type: "number", Default: "0"},
		},
		PowerShell: `function Invoke-SceneDetect {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP scene_detect 0"
    $ffmpeg = Resolve-Tool $Job.BinDir 'ffmpeg.exe'
    $ffprobe = Resolve-Tool $Job.BinDir 'ffprobe.exe'
    $src = Find-SourceFile $Job.EpisodeDir
    $out = Join-Path $Job.EpisodeDir 'scenes.json'
    if ((Test-Path -LiteralPath $out) -and ((Get-Item -LiteralPath $out).LastWriteTime -gt (Get-Item -LiteralPath $src).LastWriteTime)) {
        Write-Output "scenes.json already present and newer than source, reusing"
        Write-Output "ENCODE_STEP scene_detect 100"
        return
    }
    $inv = [System.Globalization.CultureInfo]::InvariantCulture
    $threshold = 10.0
    if ($Params.threshold) {
        $t = 0.0
        if (-not [double]::TryParse([string]$Params.threshold, [System.Globalization.NumberStyles]::Float, $inv, [ref]$t)) {
            throw "scene_detect: invalid threshold '$($Params.threshold -replace '[\r\n]', ' ')'"
        }
        if ($t -lt 0 -or $t -gt 100) { throw 'scene_detect: threshold must be 0..100' }
        $threshold = $t
    }
    $minLen = 0
    if ($Params.min_scene_len) {
        $m = 0
        if (-not [int]::TryParse([string]$Params.min_scene_len, [ref]$m)) {
            throw "scene_detect: invalid min_scene_len '$($Params.min_scene_len -replace '[\r\n]', ' ')'"
        }
        if ($m -lt 0 -or $m -gt 100000) { throw 'scene_detect: min_scene_len must be 0..100000' }
        $minLen = $m
    }

    # --- Probe: fps / frame count / duration (fail closed) ---
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $probeRaw = & $ffprobe '-v' 'error' '-select_streams' 'v:0' '-show_entries' 'stream=nb_frames,r_frame_rate,duration' '-show_entries' 'format=duration' '-of' 'json' $src 2>&1 } finally { $ErrorActionPreference = $prevEAP }
    if ($LASTEXITCODE -ne 0) {
        throw "scene_detect: ffprobe failed (exit $LASTEXITCODE): $(($probeRaw -join ' ') -replace '[\r\n]', ' ')"
    }
    try { $j = [string]::Join([Environment]::NewLine, $probeRaw) | ConvertFrom-Json } catch { throw "scene_detect: ffprobe output is not JSON" }
    $vs = @($j.streams) | Select-Object -First 1
    if (-not $vs) { throw 'scene_detect: ffprobe found no video stream' }
    if ("$($vs.r_frame_rate)" -notmatch '^([0-9]+)/([0-9]+)$') {
        throw "scene_detect: unusable r_frame_rate '$($vs.r_frame_rate)'"
    }
    $num = [double]$Matches[1]; $den = [double]$Matches[2]
    if ($num -le 0 -or $den -le 0) { throw 'scene_detect: r_frame_rate must be positive' }
    $fps = $num / $den
    $dur = 0.0
    if ("$($vs.duration)" -match '^[0-9]+(\.[0-9]+)?$') { $dur = [double]$vs.duration }
    elseif ("$($j.format.duration)" -match '^[0-9]+(\.[0-9]+)?$') { $dur = [double]$j.format.duration }
    if ($dur -le 0) { throw 'scene_detect: no usable duration from ffprobe' }
    $frameCount = [int64]0
    if ("$($vs.nb_frames)" -match '^[0-9]+$' -and [int64]$vs.nb_frames -gt 0) {
        $frameCount = [int64]$vs.nb_frames
    } else {
        # m2ts and friends lack nb_frames: derive from duration*fps.
        $frameCount = [int64][math]::Round($dur * $fps)
    }
    if ($frameCount -lt 1) { throw 'scene_detect: frame count must be >= 1' }
    Write-Output "ENCODE_STEP scene_detect 30"
    Write-Output "[scene_detect] source=$(Split-Path $src -Leaf) fps=$($fps.ToString('0.######', $inv)) frames=$frameCount threshold=$($threshold.ToString('0.###', $inv))"

    # --- scdet pass (decode-only; metadata log is the parse contract) ---
    $log = Join-Path $Job.EpisodeDir 'scdet.log'
    if (Test-Path -LiteralPath $log) { Remove-Item -LiteralPath $log -Force }
    $vf = 'scdet=threshold=' + $threshold.ToString('0.###', $inv) + ',metadata=print:file=' + $log
    Invoke-Tool -ExePath $ffmpeg -Arguments @('-hide_banner', '-nostdin', '-i', $src, '-an', '-vf', $vf, '-f', 'null', '-') -Label 'ffmpeg'
    if (-not (Test-Path -LiteralPath $log) -or (Get-Item -LiteralPath $log).Length -lt 10) {
        throw 'scene_detect: scdet.log missing or empty after the ffmpeg pass'
    }
    Write-Output "ENCODE_STEP scene_detect 60"

    # --- Parse boundaries: frame lines + score lines ---
    $bFrames = @(); $bTimes = @(); $bScores = @()
    $curFrame = [int64]-1; $curTime = 0.0
    foreach ($line in [System.IO.File]::ReadLines($log)) {
        if ($line -match '^frame:([0-9]+)\s+pts:[0-9]+\s+pts_time:([0-9]+(\.[0-9]+)?)') {
            $curFrame = [int64]$Matches[1]
            $curTime = [double]::Parse($Matches[2], $inv)
        } elseif ($line -match '^lavfi\.scd\.score=([0-9]+(\.[0-9]+)?)\s*$') {
            if ($curFrame -lt 0) { throw 'scene_detect: scdet.log score line before any frame line' }
            $score = [double]::Parse($Matches[1], $inv)
            if ($curFrame -gt 0 -and $score -ge $threshold) {
                $bFrames += $curFrame; $bTimes += $curTime; $bScores += $score
            }
        }
    }
    if ($curFrame -lt 0) { throw 'scene_detect: scdet.log has no frame lines (unexpected ffmpeg output)' }
    # min_scene_len: drop boundaries closer than minLen frames to the
    # previous kept boundary (merges the short scene into its predecessor).
    if ($minLen -gt 0) {
        $kF = @(); $kT = @(); $kS = @(); $prev = [int64]0
        for ($i = 0; $i -lt $bFrames.Count; $i++) {
            if (($bFrames[$i] - $prev) -lt $minLen) { continue }
            $kF += $bFrames[$i]; $kT += $bTimes[$i]; $kS += $bScores[$i]
            $prev = $bFrames[$i]
        }
        $bFrames = $kF; $bTimes = $kT; $bScores = $kS
    }

    # --- scenes.json (manual invariant JSON: ConvertTo-Json unwraps
    # single-element arrays on Windows PowerShell 5.1) ---
    $starts = @([int64]0) + $bFrames
    $times = @(0.0) + $bTimes
    $scores = @(0.0) + $bScores
    $sb = [System.Text.StringBuilder]::new()
    [void]$sb.AppendLine('{')
    [void]$sb.AppendLine('  "source": ' + (ConvertTo-Json (Split-Path $src -Leaf)) + ',')
    [void]$sb.AppendLine('  "fps": ' + $fps.ToString('0.######', $inv) + ',')
    [void]$sb.AppendLine('  "frame_count": ' + $frameCount + ',')
    [void]$sb.AppendLine('  "duration": ' + $dur.ToString('0.######', $inv) + ',')
    [void]$sb.AppendLine('  "threshold": ' + $threshold.ToString('0.###', $inv) + ',')
    [void]$sb.AppendLine('  "min_scene_len": ' + $minLen + ',')
    [void]$sb.AppendLine('  "scenes": [')
    for ($i = 0; $i -lt $starts.Count; $i++) {
        $endFrame = if ($i + 1 -lt $starts.Count) { $starts[$i + 1] - 1 } else { $frameCount - 1 }
        if ($endFrame -lt $starts[$i]) { $endFrame = $starts[$i] }
        $endTime = if ($i + 1 -lt $times.Count) { $times[$i + 1] } else { $dur }
        $comma = if ($i + 1 -lt $starts.Count) { ',' } else { '' }
        [void]$sb.AppendLine('    {"index": ' + ($i + 1) + ', "start_frame": ' + $starts[$i] + ', "end_frame": ' + $endFrame + ', "start_time": ' + $times[$i].ToString('0.######', $inv) + ', "end_time": ' + $endTime.ToString('0.######', $inv) + ', "score": ' + $scores[$i].ToString('0.###', $inv) + '}' + $comma)
    }
    [void]$sb.AppendLine('  ]')
    [void]$sb.AppendLine('}')
    $tmp = Join-Path $Job.EpisodeDir '.scenes.json.tmp'
    [System.IO.File]::WriteAllText($tmp, $sb.ToString(), [System.Text.UTF8Encoding]::new($false))
    Move-Item -LiteralPath $tmp -Destination $out -Force
    Write-Output "[scene_detect] wrote scenes.json: $($starts.Count) scene(s)"
    Write-Output "ENCODE_STEP scene_detect 100"
}`,
	}
}