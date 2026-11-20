// Package flow: zone_build built-in — scenes + chapters -> encoder zones.
//
// Reads scenes.json (from scene_detect) and the optional chapters.xml
// (from bd_merge / chapter_extract) and publishes zones.json: a ready
// x265 --zones string plus the structured zones for audit. Rule
// precedence (x265 applies the LAST matching zone, so the output is
// ordered low->high precedence):
//  1. position heuristic: first/last scene <= credits_max_seconds -> b=credits_b
//  2. chapter-name matches (name_patterns, case-insensitive) -> b=op_ed_b
//     over the chapter's frame span (chapter start -> next chapter start-1)
//  3. manual overrides (x265-style "start,end,b=V/start,end,q=V")
//
// The encode step's use_zones param injects the validated string.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

func zoneBuildTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "zone_build",
		Label:       "Zone builder (scenes -> x265 zones)",
		Description: "Build x265 zones from scenes.json + chapter names: credits heuristic (first/last short scenes), OP/ED chapter-name matches, manual overrides. Publishes zones.json for the encode step's use_zones.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "credits_b", Label: "Credits bitrate factor", Type: "number", Default: "0.6"},
			{Key: "credits_max_seconds", Label: "Max credits scene length (s)", Type: "number", Default: "90"},
			{Key: "op_ed_b", Label: "OP/ED chapter bitrate factor", Type: "number", Default: "0.8"},
			{Key: "name_patterns", Label: "Chapter name regex (case-insensitive)", Default: "OP|Opening|ED|Ending|Credits|Preview"},
			{Key: "overrides", Label: "Manual zones: start,end,b=V/start,end,q=V (highest precedence)", Default: ""},
		},
		PowerShell: `function Invoke-ZoneBuild {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP zone_build 0"
    $inv = [System.Globalization.CultureInfo]::InvariantCulture
    $scenesPath = Join-Path $Job.EpisodeDir 'scenes.json'
    if (-not (Test-Path -LiteralPath $scenesPath)) {
        throw 'zone_build: scenes.json missing - run scene_detect first'
    }
    $chaptersPath = Join-Path $Job.EpisodeDir 'chapters.xml'
    $out = Join-Path $Job.EpisodeDir 'zones.json'
    # Skip when zones.json is newer than both inputs.
    if (Test-Path -LiteralPath $out) {
        $zTime = (Get-Item -LiteralPath $out).LastWriteTime
        $fresh = $zTime -gt (Get-Item -LiteralPath $scenesPath).LastWriteTime
        if ($fresh -and (Test-Path -LiteralPath $chaptersPath)) {
            $fresh = $zTime -gt (Get-Item -LiteralPath $chaptersPath).LastWriteTime
        }
        if ($fresh) {
            Write-Output "zones.json already present and newer than inputs, reusing"
            Write-Output "ENCODE_STEP zone_build 100"
            return
        }
    }
    try { $sc = [System.IO.File]::ReadAllText($scenesPath) | ConvertFrom-Json } catch { throw 'zone_build: scenes.json is not valid JSON' }
    $fps = [double]$sc.fps
    $frameCount = [int64]$sc.frame_count
    if ($fps -le 0 -or $frameCount -lt 1) { throw 'zone_build: scenes.json has unusable fps/frame_count' }
    $scenes = @($sc.scenes)
    if ($scenes.Count -lt 1) { throw 'zone_build: scenes.json has no scenes' }

    # --- Params ---
    $creditsB = 0.6
    if ($Params.credits_b) {
        $v = 0.0
        if (-not [double]::TryParse([string]$Params.credits_b, [System.Globalization.NumberStyles]::Float, $inv, [ref]$v)) { throw 'zone_build: invalid credits_b' }
        if ($v -le 0 -or $v -gt 10) { throw 'zone_build: credits_b must be in (0, 10]' }
        $creditsB = $v
    }
    $opEdB = 0.8
    if ($Params.op_ed_b) {
        $v = 0.0
        if (-not [double]::TryParse([string]$Params.op_ed_b, [System.Globalization.NumberStyles]::Float, $inv, [ref]$v)) { throw 'zone_build: invalid op_ed_b' }
        if ($v -le 0 -or $v -gt 10) { throw 'zone_build: op_ed_b must be in (0, 10]' }
        $opEdB = $v
    }
    $creditsMax = 90.0
    if ($Params.credits_max_seconds) {
        $v = 0.0
        if (-not [double]::TryParse([string]$Params.credits_max_seconds, [System.Globalization.NumberStyles]::Float, $inv, [ref]$v)) { throw 'zone_build: invalid credits_max_seconds' }
        if ($v -lt 0 -or $v -gt 36000) { throw 'zone_build: credits_max_seconds must be 0..36000' }
        $creditsMax = $v
    }
    $pattern = 'OP|Opening|ED|Ending|Credits|Preview'
    if ($Params.name_patterns) { $pattern = [string]$Params.name_patterns }
    try { $rx = [regex]::new($pattern, [System.Text.RegularExpressions.RegexOptions]::IgnoreCase) } catch { throw 'zone_build: invalid name_patterns regex' }
    Write-Output "ENCODE_STEP zone_build 30"

    # Zones ordered low->high precedence (x265 later-wins). Each entry is
    # appended only when its frame span is valid (0 <= start < end < count).
    $zones = @()

    # 1. Position heuristic: first/last scene short enough to be credits.
    if ($scenes.Count -ge 2) {
        $first = $scenes[0]; $last = $scenes[$scenes.Count - 1]
        if (([double]$first.end_time - [double]$first.start_time) -le $creditsMax) {
            $s = [int64]$first.start_frame; $e = [int64]$first.end_frame
            if ($s -ge 0 -and $e -lt $frameCount -and $s -lt $e) {
                $zones += [pscustomobject]@{ start = $s; end = $e; mode = 'b'; value = $creditsB; reason = 'credits:first-scene' }
            }
        }
        if (([double]$last.end_time - [double]$last.start_time) -le $creditsMax) {
            $s = [int64]$last.start_frame; $e = [int64]$last.end_frame
            if ($s -ge 0 -and $e -lt $frameCount -and $s -lt $e) {
                $zones += [pscustomobject]@{ start = $s; end = $e; mode = 'b'; value = $creditsB; reason = 'credits:last-scene' }
            }
        }
    }

    # 2. Chapter-name matches: zone spans the whole named chapter.
    if (Test-Path -LiteralPath $chaptersPath) {
        try { [xml]$cx = [System.IO.File]::ReadAllText($chaptersPath) } catch { throw 'zone_build: chapters.xml is not valid XML' }
        $atoms = @($cx.SelectNodes('//ChapterAtom'))
        $chTimes = @(); $chNames = @()
        foreach ($a in $atoms) {
            $ts = [string]$a.ChapterTimeStart
            if ($ts -notmatch '^([0-9]{2}):([0-9]{2}):([0-9]{2})(\.[0-9]+)?$') { throw "zone_build: bad ChapterTimeStart '$ts'" }
            $secs = [double]$Matches[1] * 3600 + [double]$Matches[2] * 60 + [double]$Matches[3]
            if ($Matches[4]) { $secs += [double]::Parse('0' + $Matches[4], $inv) }
            $name = ''
            $dn = $a.SelectSingleNode('ChapterDisplay/ChapterString')
            if ($dn) { $name = [string]$dn.InnerText }
            # Control chars -> space + trim (bash sanitize parity; keeps
            # decoded entities like &#10; from breaking the TSV protocol).
            $name = ($name -replace '[\x00-\x1F]', ' ').Trim()
            $chTimes += $secs; $chNames += $name
        }
        for ($i = 0; $i -lt $chTimes.Count; $i++) {
            if (-not $rx.IsMatch($chNames[$i])) { continue }
            $sf = [int64][math]::Round($chTimes[$i] * $fps)
            $ef = if ($i + 1 -lt $chTimes.Count) { [int64][math]::Round($chTimes[$i + 1] * $fps) - 1 } else { $frameCount - 1 }
            if ($sf -lt 0) { $sf = 0 }
            if ($ef -ge $frameCount) { $ef = $frameCount - 1 }
            if ($sf -lt $ef) {
                $zones += [pscustomobject]@{ start = $sf; end = $ef; mode = 'b'; value = $opEdB; reason = ('chapter:' + ($chNames[$i] -replace '[\r\n\t"]', ' ')) }
            }
        }
    }

    # 3. Manual overrides (x265-style tokens, highest precedence).
    $ov = [string]$Params.overrides
    if ($ov.Trim() -ne '') {
        foreach ($tok in ($ov -split '/')) {
            $t = $tok.Trim()
            if ($t -eq '') { continue }
            if ($t -notmatch '^([0-9]{1,9}),([0-9]{1,9}),(b=([0-9]+(\.[0-9]+)?)|q=([0-9]{1,2}))$') {
                throw "zone_build: invalid override token '$($t -replace '[\r\n]', ' ')'"
            }
            $s = [int64]$Matches[1]; $e = [int64]$Matches[2]
            if ($s -ge $frameCount -or $e -ge $frameCount -or $s -ge $e) {
                throw "zone_build: override frames outside 0..$($frameCount - 1) or start >= end"
            }
            if ($Matches[4]) {
                $v = [double]::Parse($Matches[4], $inv)
                if ($v -le 0 -or $v -gt 10) { throw 'zone_build: override b must be in (0, 10]' }
                $zones += [pscustomobject]@{ start = $s; end = $e; mode = 'b'; value = $v; reason = 'override' }
            } else {
                $q = [int]$Matches[6]
                if ($q -lt 0 -or $q -gt 51) { throw 'zone_build: override q must be 0..51' }
                $zones += [pscustomobject]@{ start = $s; end = $e; mode = 'q'; value = ([double]$q); reason = 'override' }
            }
        }
    }
    Write-Output "ENCODE_STEP zone_build 60"

    # --- zones.json + the x265 string ---
    $toks = @()
    $sb = [System.Text.StringBuilder]::new()
    [void]$sb.AppendLine('{')
    [void]$sb.AppendLine('  "frame_count": ' + $frameCount + ',')
    [void]$sb.AppendLine('  "fps": ' + $fps.ToString('0.######', $inv) + ',')
    [void]$sb.AppendLine('  "zones": [')
    for ($i = 0; $i -lt $zones.Count; $i++) {
        $z = $zones[$i]
        $valStr = if ($z.mode -eq 'q') { ([int]$z.value).ToString($inv) } else { $z.value.ToString('0.###', $inv) }
        $toks += ('{0},{1},{2}={3}' -f $z.start, $z.end, $z.mode, $valStr)
        $comma = if ($i + 1 -lt $zones.Count) { ',' } else { '' }
        [void]$sb.AppendLine('    {"start": ' + $z.start + ', "end": ' + $z.end + ', "mode": "' + $z.mode + '", "value": ' + $valStr + ', "reason": ' + (ConvertTo-Json $z.reason) + '}' + $comma)
    }
    [void]$sb.AppendLine('  ],')
    [void]$sb.AppendLine('  "x265": ' + (ConvertTo-Json ($toks -join '/')))
    [void]$sb.AppendLine('}')
    $tmp = Join-Path $Job.EpisodeDir '.zones.json.tmp'
    [System.IO.File]::WriteAllText($tmp, $sb.ToString(), [System.Text.UTF8Encoding]::new($false))
    Move-Item -LiteralPath $tmp -Destination $out -Force
    Write-Output "[zone_build] wrote zones.json: $($zones.Count) zone(s)"
    Write-Output "ENCODE_STEP zone_build 100"
}`,
	}
}
