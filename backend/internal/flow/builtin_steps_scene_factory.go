package flow

// Factory snapshots of the scene_detect PowerShell template for the guarded
// upgrade chain in seedStepTemplates (InsertStepTemplateIfAbsent never
// overwrites existing rows, so a release that FIXES a builtin must carry the
// prior factory text and upgrade untouched copies explicitly — the mux
// template precedent). User-edited scripts never match byte-for-byte and
// stay in effect.

// SceneDetectFactoryV1 is the v1.25.0 factory script: direct ffprobe property
// access (throws under Set-StrictMode on m2ts, which omits nb_frames) and an
// unescaped filtergraph log path.
const SceneDetectFactoryV1 = `function Invoke-SceneDetect {
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
        # Plain-decimal pre-validation (parity with bash: reject 1e1/10./
        # padded forms BEFORE parsing) + trim.
        $tt = ([string]$Params.threshold).Trim()
        if ($tt -notmatch '^[0-9]{1,3}(\.[0-9]+)?$') {
            throw "scene_detect: invalid threshold '$($tt -replace '[\r\n]', ' ')'"
        }
        $t = [double]::Parse($tt, $inv)
        if ($t -lt 0 -or $t -gt 100) { throw 'scene_detect: threshold must be 0..100' }
        # Compare against the SAME rounded value the scdet filter receives
        # (bash normalizes before both uses; full-precision comparison would
        # select different boundaries for scores in [rounded, original)).
        $threshold = [double]$t.ToString('0.###', $inv)
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
    # nb_frames: digit cap before the cast (parity with bash fail-closed).
    # The count is finalized after the scdet pass (decoded-frame fallback).
    $nbOk = ("$($vs.nb_frames)" -match '^[0-9]{1,18}$' -and [int64]$vs.nb_frames -gt 0)
    Write-Output "ENCODE_STEP scene_detect 30"
    Write-Output "[scene_detect] source=$(Split-Path $src -Leaf) fps=$($fps.ToString('0.######', $inv)) threshold=$($threshold.ToString('0.###', $inv))"

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
    $curFrame = [int64]-1; $curTime = 0.0; $maxFrame = [int64]-1
    foreach ($line in [System.IO.File]::ReadLines($log)) {
        if ($line -match '^frame:([0-9]{1,9})\s+pts:[0-9]+\s+pts_time:([0-9]+(\.[0-9]+)?)\s*$') {
            $curFrame = [int64]$Matches[1]
            $curTime = [double]::Parse($Matches[2], $inv)
            if ($curFrame -gt $maxFrame) { $maxFrame = $curFrame }
        } elseif ($line -match '^lavfi\.scd\.score=([0-9]+(\.[0-9]+)?)\s*$') {
            if ($curFrame -lt 0) { throw 'scene_detect: scdet.log score line before any frame line' }
            $score = [double]::Parse($Matches[1], $inv)
            if ($curFrame -gt 0 -and $score -ge $threshold) {
                $bFrames += $curFrame; $bTimes += $curTime; $bScores += $score
            }
        }
    }
    if ($curFrame -lt 0) { throw 'scene_detect: scdet.log has no frame lines (unexpected ffmpeg output)' }
    # Frame count: nb_frames when present, else the ACTUAL decoded count
    # (last frame + 1) — exact for VFR too, unlike duration*fps.
    $frameCount = if ($nbOk) { [int64]$vs.nb_frames } else { $maxFrame + 1 }
    if ($frameCount -lt 1) { throw 'scene_detect: frame count must be >= 1' }
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
    Write-Output "[scene_detect] wrote scenes.json: $($starts.Count) scene(s), $frameCount frame(s)"
    Write-Output "ENCODE_STEP scene_detect 100"
}`

// SceneDetectFactoryV1a is the v1.25.1 factory script: StrictMode-safe probe
// access, but the filtergraph log path still unescaped (Windows paths with
// backslashes/spaces/colons fail the ffmpeg graph parser).
const SceneDetectFactoryV1a = `function Invoke-SceneDetect {
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
        # Plain-decimal pre-validation (parity with bash: reject 1e1/10./
        # padded forms BEFORE parsing) + trim.
        $tt = ([string]$Params.threshold).Trim()
        if ($tt -notmatch '^[0-9]{1,3}(\.[0-9]+)?$') {
            throw "scene_detect: invalid threshold '$($tt -replace '[\r\n]', ' ')'"
        }
        $t = [double]::Parse($tt, $inv)
        if ($t -lt 0 -or $t -gt 100) { throw 'scene_detect: threshold must be 0..100' }
        # Compare against the SAME rounded value the scdet filter receives
        # (bash normalizes before both uses; full-precision comparison would
        # select different boundaries for scores in [rounded, original)).
        $threshold = [double]$t.ToString('0.###', $inv)
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
    # Null-safe property access (StrictMode Latest throws on missing keys):
    # m2ts and other containers omit nb_frames and often stream duration.
    $streams = $null
    if ($null -ne $j.PSObject.Properties['streams']) { $streams = @($j.streams) }
    $vs = @($streams) | Select-Object -First 1
    if (-not $vs) { throw 'scene_detect: ffprobe found no video stream' }
    $rfr = ''
    if ($null -ne $vs.PSObject.Properties['r_frame_rate']) { $rfr = [string]$vs.r_frame_rate }
    if ($rfr -notmatch '^([0-9]+)/([0-9]+)$') {
        throw "scene_detect: unusable r_frame_rate '$rfr'"
    }
    $num = [double]$Matches[1]; $den = [double]$Matches[2]
    if ($num -le 0 -or $den -le 0) { throw 'scene_detect: r_frame_rate must be positive' }
    $fps = $num / $den
    $dur = 0.0
    $sd = ''
    if ($null -ne $vs.PSObject.Properties['duration']) { $sd = [string]$vs.duration }
    $fd = ''
    if ($null -ne $j.PSObject.Properties['format'] -and $null -ne $j.format -and $null -ne $j.format.PSObject.Properties['duration']) { $fd = [string]$j.format.duration }
    if ($sd -match '^[0-9]+(\.[0-9]+)?$') { $dur = [double]$sd }
    elseif ($fd -match '^[0-9]+(\.[0-9]+)?$') { $dur = [double]$fd }
    if ($dur -le 0) { throw 'scene_detect: no usable duration from ffprobe' }
    # nb_frames: digit cap before the cast (parity with bash fail-closed).
    # Absent key (m2ts) -> decoded-frame fallback after the scdet pass.
    $nbRaw = ''
    if ($null -ne $vs.PSObject.Properties['nb_frames']) { $nbRaw = [string]$vs.nb_frames }
    $nbOk = ($nbRaw -match '^[0-9]{1,18}$' -and [int64]$nbRaw -gt 0)
    Write-Output "ENCODE_STEP scene_detect 30"
    Write-Output "[scene_detect] source=$(Split-Path $src -Leaf) fps=$($fps.ToString('0.######', $inv)) threshold=$($threshold.ToString('0.###', $inv))"

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
    $curFrame = [int64]-1; $curTime = 0.0; $maxFrame = [int64]-1
    foreach ($line in [System.IO.File]::ReadLines($log)) {
        if ($line -match '^frame:([0-9]{1,9})\s+pts:[0-9]+\s+pts_time:([0-9]+(\.[0-9]+)?)\s*$') {
            $curFrame = [int64]$Matches[1]
            $curTime = [double]::Parse($Matches[2], $inv)
            if ($curFrame -gt $maxFrame) { $maxFrame = $curFrame }
        } elseif ($line -match '^lavfi\.scd\.score=([0-9]+(\.[0-9]+)?)\s*$') {
            if ($curFrame -lt 0) { throw 'scene_detect: scdet.log score line before any frame line' }
            $score = [double]::Parse($Matches[1], $inv)
            if ($curFrame -gt 0 -and $score -ge $threshold) {
                $bFrames += $curFrame; $bTimes += $curTime; $bScores += $score
            }
        }
    }
    if ($curFrame -lt 0) { throw 'scene_detect: scdet.log has no frame lines (unexpected ffmpeg output)' }
    # Frame count: nb_frames when present, else the ACTUAL decoded count
    # (last frame + 1) — exact for VFR too, unlike duration*fps.
    $frameCount = if ($nbOk) { [int64]$nbRaw } else { $maxFrame + 1 }
    if ($frameCount -lt 1) { throw 'scene_detect: frame count must be >= 1' }
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
    Write-Output "[scene_detect] wrote scenes.json: $($starts.Count) scene(s), $frameCount frame(s)"
    Write-Output "ENCODE_STEP scene_detect 100"
}`
