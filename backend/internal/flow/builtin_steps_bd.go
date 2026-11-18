// Package flow: Blu-ray playlist built-ins (MPLS-driven M2TS split/merge).
//
// ENGINE: tsMuxeR (teaching-droid/tsMuxer fork, v3.1.0 verified) — a BD
// authoring muxer, so PAT/PMT insertion, continuity counters, PCR and
// seamless-branching joins are handled by the tool, losslessly. Output
// stays M2TS (the eac3to audio chain reads BD-native TrueHD/DTS-HD/LPCM
// straight from it).
//
// Verified tsMuxeR contracts (synthetic-disc round-trip 2026-11-18, see
// skill encode-system-fleet-ops references/tsmuxer-bd-operations.md):
//   - detection mode `tsMuxeR <mpls>` prints Track ID / Stream ID pairs and
//     a `Marks: hh:mm:ss.zzz ...` line (the playlist's chapter marks);
//   - a meta line referencing the .mpls expands to one file per PlayItem,
//     trimmed to the PlayItem IN/OUT points (--playlist-clips played is the
//     DEFAULT — the raw m2ts black-padding never leaks into the output);
//   - MUXOPT --split-chapters cuts at the marks (first keyframe >= mark),
//     writing <out>.split.N.m2ts parts (no base file);
//   - MUXOPT --cut-start/--cut-end keep a time range (chapter selection).
//
// ARTIFACT CONTRACTS:
//
//	bd_merge        → src.m2ts + chapters.xml (Matroska XML sidecar from the
//	                  marks; M2TS itself cannot hold chapters — mux_extras
//	                  consumes the sidecar for the final MKV). No marks →
//	                  stale chapters.xml removed, merge still succeeds.
//	                  Stale chapter-* split artifacts from a previous
//	                  bd_split run are cleaned (mode-switch orphan guard).
//	bd_split        → mode=chapters: chapter-NN.m2ts + chapter-NN.xml per
//	                  part (single rebased chapter at 0, original number in
//	                  the name); a NON-ZERO first mark adds a chapter-00
//	                  pre-chapter segment (tsMuxeR emits N+1 parts —
//	                  verified against the real engine). mode=range +
//	                  range=N-M: src.m2ts + chapters.xml for the kept
//	                  chapters, cut with --cut-pictures=inside (first key
//	                  picture AT OR AFTER the mark — no previous-chapter
//	                  pre-roll) and rebased against the MEASURED first
//	                  video PTS (ffprobe required; nominal mark math would
//	                  shift every chapter by the keyframe snap). Stale
//	                  artifacts cleaned in ALL modes (>99-chapter names
//	                  included).
//	Meta files (bd-merge.meta / bd-split.meta) are kept in the episode dir
//	for debugging, rewritten on every run. No fresh-skip: a remux is cheap
//	and re-running must always reflect the current playlist/params.
//
// Both steps are self-contained (renderer links one function per template;
// helpers come only from EncodeLib.ps1). The bash ports live in
// builtin_steps_bd_linux.go with semantic parity.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

func bdMergeTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "bd_merge",
		Label:       "BD merge (MPLS → M2TS)",
		Description: "Merge the clips of a Blu-ray playlist into one src.m2ts with tsMuxeR (lossless, seamless-branching aware, clips trimmed to their PlayItem IN/OUT points). Writes chapters.xml from the playlist marks for mux_extras. Input: BDMV folder or loose .mpls+.m2ts in the episode dir. Requires tsMuxeR in the bin package.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "playlist", Label: "Playlist (empty = auto-detect)", Placeholder: "00001.mpls or BDMV/PLAYLIST/00001.mpls"},
		},
		PowerShell: `function Invoke-BdMerge {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP bd_merge 0"
    $tsmuxer = Resolve-Tool $Job.BinDir 'tsMuxeR.exe'

    # --- Resolve the playlist: param > BDMV/PLAYLIST > flat *.mpls ---
    $mplsRel = ''
    if ($Params.playlist) {
        $p = ([string]$Params.playlist).Trim() -replace '\\', '/'
        # Untrusted param: no traversal, no absolute paths/drive letters,
        # no control chars or double quotes (the meta file quotes paths).
        if ($p -match '(^|/)\.\.(/|$)' -or $p -match '^/' -or $p -match '^[A-Za-z]:' -or $p -match '[\x00-\x1f\x7f"]') {
            throw "unsafe playlist parameter: $($p -replace '[\r\n]', ' ')"
        }
        $cand = Join-Path $Job.EpisodeDir $p
        if (-not (Test-Path -LiteralPath $cand -PathType Leaf)) {
            throw "playlist not found: $($cand -replace '[\r\n]', ' ')"
        }
        $mplsRel = $p
    } else {
        $plDir = Join-Path $Job.EpisodeDir 'BDMV/PLAYLIST'
        $searchDir = $Job.EpisodeDir
        $prefix = ''
        if (Test-Path -LiteralPath $plDir -PathType Container) {
            $searchDir = $plDir
            $prefix = 'BDMV/PLAYLIST/'
        }
        $pls = @(Get-ChildItem -LiteralPath $searchDir -File -Filter '*.mpls' | Sort-Object Name)
        if ($pls.Count -eq 0) {
            throw "no Blu-ray playlist (*.mpls) found in $($Job.EpisodeDir) (expected BDMV/PLAYLIST or a loose .mpls beside the .m2ts)"
        }
        if ($pls.Count -gt 1) {
            $names = (($pls | ForEach-Object { $_.Name -replace '[\r\n]', ' ' }) -join ', ')
            throw "multiple playlists in $($Job.EpisodeDir) ($names) - set the playlist parameter"
        }
        $mplsRel = $prefix + $pls[0].Name
    }
    $mpls = Join-Path $Job.EpisodeDir $mplsRel
    if ($mpls -match '[\r\n"]') {
        throw "playlist path contains quotes or newlines (cannot be meta-quoted): $($mpls -replace '[\r\n]', ' ')"
    }
    Write-Output "[bd_merge] playlist: $($mplsRel -replace '[\r\n]', ' ')"

    # --- Detection listing: tracks + chapter marks ---
    # EAP relaxed for the native call (PS 5.1 wraps stderr as ErrorRecords;
    # same pattern as EncodeLib Invoke-Tool). Exit code decides failure.
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $det = & $tsmuxer $mpls 2>&1 } finally { $ErrorActionPreference = $prevEAP }
    if ($LASTEXITCODE -ne 0) {
        $tail = ((@($det) | ForEach-Object { "$_" }) -join ' ') -replace '[\r\n]', ' '
        throw "tsMuxeR playlist detection failed (exit $LASTEXITCODE): $tail"
    }
    $ids = @(); $codecs = @(); $marks = @()
    $pendingId = $null
    foreach ($line in (@($det) | ForEach-Object { if ($_ -is [System.Management.Automation.ErrorRecord]) { $_.Exception.Message } else { "$_" } })) {
        if ($line -match '^Track ID:\s*([0-9]+)\s*$') { $pendingId = $Matches[1]; continue }
        if ($line -match '^Stream ID:\s*(\S+)\s*$') {
            if ($null -eq $pendingId) { throw "tsMuxeR listing: Stream ID line without a preceding Track ID" }
            $codec = $Matches[1]
            if ($codec -notmatch '^[VAS]_[A-Za-z0-9/]+$') { throw "tsMuxeR listing: unsupported stream id '$codec'" }
            $ids += $pendingId; $codecs += $codec; $pendingId = $null
            continue
        }
        if ($line -match '^Marks:\s*(.+)$') {
            foreach ($m in ($Matches[1].Trim() -split '\s+')) {
                if ($m -notmatch '^[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}$') { throw "tsMuxeR listing: invalid chapter mark '$m'" }
                $marks += $m
            }
        }
    }
    if ($ids.Count -eq 0) { throw "tsMuxeR found no muxable tracks in $mplsRel" }
    Write-Output "[bd_merge] $($ids.Count) track(s), $($marks.Count) chapter mark(s)"
    Write-Output "ENCODE_STEP bd_merge 30"

    # --- Meta file: every track references the PLAYLIST (tsMuxeR expands
    # it per PlayItem, trimmed to IN/OUT). UTF-8 without BOM (PS 5.1
    # Set-Content -Encoding UTF8 writes a BOM that breaks MUXOPT parsing).
    $meta = Join-Path $Job.EpisodeDir 'bd-merge.meta'
    $out = Join-Path $Job.EpisodeDir 'src.m2ts'
    $metaLines = @('MUXOPT')
    for ($i = 0; $i -lt $ids.Count; $i++) {
        $metaLines += ('{0}, "{1}", track={2}' -f $codecs[$i], $mpls, $ids[$i])
    }
    $utf8NoBom = New-Object System.Text.UTF8Encoding $false
    [System.IO.File]::WriteAllLines($meta, $metaLines, $utf8NoBom)
    Write-Output "ENCODE_STEP bd_merge 60"
    # Clean stale split artifacts from a previous bd_split run in this dir
    # (mode-switch orphan guard — parity with bd_split's own cleanup).
    foreach ($stale in @(Get-ChildItem -LiteralPath $Job.EpisodeDir -File |
        Where-Object { $_.Name -match '^chapter-[0-9]+\.(m2ts|xml)$' -or $_.Name -match '^bd-split\.split\.[0-9]+\.m2ts$' })) {
        Remove-Item -LiteralPath $stale.FullName -Force
    }
    if (Test-Path -LiteralPath $out) { Remove-Item -LiteralPath $out -Force }
    Invoke-Tool -ExePath $tsmuxer -Arguments @($meta, $out) -Label 'tsMuxeR'
    if (-not (Test-Path -LiteralPath $out -PathType Leaf)) {
        throw "tsMuxeR finished but $out was not created"
    }
    if ((Get-Item -LiteralPath $out).Length -lt 192) {
        throw "src.m2ts is smaller than one 192-byte source packet - mux produced no data"
    }

    # --- Chapters sidecar (Matroska XML, mkvmerge --chapters format) ---
    $chXml = Join-Path $Job.EpisodeDir 'chapters.xml'
    if ($marks.Count -eq 0) {
        Write-Output "[bd_merge] playlist has no chapter marks - no chapters.xml"
        if (Test-Path -LiteralPath $chXml) {
            Remove-Item -LiteralPath $chXml -Force
            Write-Output "[bd_merge] removed stale chapters.xml"
        }
    } else {
        $xml = @('<?xml version="1.0" encoding="UTF-8"?>', '<Chapters>', '  <EditionEntry>')
        for ($i = 0; $i -lt $marks.Count; $i++) {
            $xml += '    <ChapterAtom>'
            # hh:mm:ss.zzz + six zeros = nanosecond-precision start time.
            $xml += ('      <ChapterTimeStart>{0}000000</ChapterTimeStart>' -f $marks[$i])
            $xml += '      <ChapterDisplay>'
            $xml += ('        <ChapterString>Chapter {0:D2}</ChapterString>' -f ($i + 1))
            $xml += '        <ChapterLanguage>eng</ChapterLanguage>'
            $xml += '      </ChapterDisplay>'
            $xml += '    </ChapterAtom>'
        }
        $xml += '  </EditionEntry>'
        $xml += '</Chapters>'
        [System.IO.File]::WriteAllLines($chXml, $xml, $utf8NoBom)
        Write-Output "[bd_merge] wrote chapters.xml ($($marks.Count) chapters)"
    }
    Write-Output "ENCODE_STEP bd_merge 100"
}`,
	}
}

func bdSplitTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "bd_split",
		Label:       "BD split (MPLS chapters → M2TS)",
		Description: "Split a Blu-ray playlist at its chapter marks with tsMuxeR (lossless, keyframe-aligned). mode=chapters: one chapter-NN.m2ts per chapter (+ chapter-NN.xml sidecar); a non-zero first mark adds a chapter-00 pre-chapter segment. mode=range with range=N-M: keep chapters N..M as a single src.m2ts (+ chapters.xml rebased against the MEASURED output start; requires ffprobe). Cuts use --cut-pictures=inside: no previous-chapter pre-roll; off-keyframe marks lose up to one GOP at the head (compliant BDs mark key pictures, so cuts are exact there). Requires tsMuxeR in the bin package.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "playlist", Label: "Playlist (empty = auto-detect)", Placeholder: "00001.mpls or BDMV/PLAYLIST/00001.mpls"},
			{Key: "mode", Label: "Mode (chapters | range)", Default: "chapters"},
			{Key: "range", Label: "Chapter range for mode=range", Placeholder: "2-5 (or a single chapter: 3)"},
		},
		PowerShell: `function Invoke-BdSplit {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP bd_split 0"
    $tsmuxer = Resolve-Tool $Job.BinDir 'tsMuxeR.exe'
    $mode = if ($Params.mode) { ([string]$Params.mode).Trim().ToLowerInvariant() } else { 'chapters' }
    if ($mode -ne 'chapters' -and $mode -ne 'range') {
        throw "bd_split: mode must be 'chapters' or 'range' (got '$($mode -replace '[\r\n]', ' ')')"
    }

    # --- Resolve the playlist: param > BDMV/PLAYLIST > flat *.mpls ---
    $mplsRel = ''
    if ($Params.playlist) {
        $p = ([string]$Params.playlist).Trim() -replace '\\', '/'
        if ($p -match '(^|/)\.\.(/|$)' -or $p -match '^/' -or $p -match '^[A-Za-z]:' -or $p -match '[\x00-\x1f\x7f"]') {
            throw "unsafe playlist parameter: $($p -replace '[\r\n]', ' ')"
        }
        $cand = Join-Path $Job.EpisodeDir $p
        if (-not (Test-Path -LiteralPath $cand -PathType Leaf)) {
            throw "playlist not found: $($cand -replace '[\r\n]', ' ')"
        }
        $mplsRel = $p
    } else {
        $plDir = Join-Path $Job.EpisodeDir 'BDMV/PLAYLIST'
        $searchDir = $Job.EpisodeDir
        $prefix = ''
        if (Test-Path -LiteralPath $plDir -PathType Container) {
            $searchDir = $plDir
            $prefix = 'BDMV/PLAYLIST/'
        }
        $pls = @(Get-ChildItem -LiteralPath $searchDir -File -Filter '*.mpls' | Sort-Object Name)
        if ($pls.Count -eq 0) {
            throw "no Blu-ray playlist (*.mpls) found in $($Job.EpisodeDir) (expected BDMV/PLAYLIST or a loose .mpls beside the .m2ts)"
        }
        if ($pls.Count -gt 1) {
            $names = (($pls | ForEach-Object { $_.Name -replace '[\r\n]', ' ' }) -join ', ')
            throw "multiple playlists in $($Job.EpisodeDir) ($names) - set the playlist parameter"
        }
        $mplsRel = $prefix + $pls[0].Name
    }
    $mpls = Join-Path $Job.EpisodeDir $mplsRel
    if ($mpls -match '[\r\n"]') {
        throw "playlist path contains quotes or newlines (cannot be meta-quoted): $($mpls -replace '[\r\n]', ' ')"
    }
    Write-Output "[bd_split] playlist: $($mplsRel -replace '[\r\n]', ' ') (mode=$mode)"

    # --- Detection listing: tracks + chapter marks ---
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try { $det = & $tsmuxer $mpls 2>&1 } finally { $ErrorActionPreference = $prevEAP }
    if ($LASTEXITCODE -ne 0) {
        $tail = ((@($det) | ForEach-Object { "$_" }) -join ' ') -replace '[\r\n]', ' '
        throw "tsMuxeR playlist detection failed (exit $LASTEXITCODE): $tail"
    }
    $ids = @(); $codecs = @(); $marks = @()
    $pendingId = $null
    foreach ($line in (@($det) | ForEach-Object { if ($_ -is [System.Management.Automation.ErrorRecord]) { $_.Exception.Message } else { "$_" } })) {
        if ($line -match '^Track ID:\s*([0-9]+)\s*$') { $pendingId = $Matches[1]; continue }
        if ($line -match '^Stream ID:\s*(\S+)\s*$') {
            if ($null -eq $pendingId) { throw "tsMuxeR listing: Stream ID line without a preceding Track ID" }
            $codec = $Matches[1]
            if ($codec -notmatch '^[VAS]_[A-Za-z0-9/]+$') { throw "tsMuxeR listing: unsupported stream id '$codec'" }
            $ids += $pendingId; $codecs += $codec; $pendingId = $null
            continue
        }
        if ($line -match '^Marks:\s*(.+)$') {
            foreach ($m in ($Matches[1].Trim() -split '\s+')) {
                if ($m -notmatch '^[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{3}$') { throw "tsMuxeR listing: invalid chapter mark '$m'" }
                $marks += $m
            }
        }
    }
    if ($ids.Count -eq 0) { throw "tsMuxeR found no muxable tracks in $mplsRel" }
    if ($marks.Count -eq 0) { throw "bd_split: playlist $mplsRel has no chapter marks - nothing to split" }

    # --- Range validation (1-based chapter numbers, inclusive) ---
    # Digit cap BEFORE the [int] cast (parity with bash, which wraps mod
    # 2^64 on uncapped digits): 9 digits is far above any chapter count.
    $rangeA = 1; $rangeB = $marks.Count
    $ffprobe = $null
    if ($mode -eq 'range') {
        $r = if ($Params.range) { ([string]$Params.range).Trim() } else { '' }
        if ($r -notmatch '^([0-9]{1,9})(?:-([0-9]{1,9}))?$') {
            throw "bd_split: range must be N or N-M chapter numbers (got '$($r -replace '[\r\n]', ' ')')"
        }
        $rangeA = [int]$Matches[1]
        $rangeB = $rangeA
        if ($Matches.ContainsKey('2') -and $Matches['2']) { $rangeB = [int]$Matches['2'] }
        if ($rangeA -lt 1 -or $rangeB -lt $rangeA -or $rangeB -gt $marks.Count) {
            throw "bd_split: range $rangeA-$rangeB is outside the playlist's 1-$($marks.Count) chapters"
        }
        # Range mode rebases chapters against the MEASURED output start —
        # ffprobe is required (fail fast, before the remux).
        $ffprobe = Join-Path $Job.BinDir 'ffprobe.exe'
        if (-not (Test-Path -LiteralPath $ffprobe)) {
            throw "bd_split: range mode requires ffprobe.exe for exact chapter rebasing (add it to $($Job.BinDir))"
        }
    }
    Write-Output "[bd_split] $($ids.Count) track(s), $($marks.Count) chapter mark(s)"
    Write-Output "ENCODE_STEP bd_split 30"

    # --- Clean stale artifacts (ALL modes: a mode switch must not leave
    # orphans from a previous run; patterns cover >99-chapter names) ---
    foreach ($stale in @(Get-ChildItem -LiteralPath $Job.EpisodeDir -File |
        Where-Object { $_.Name -match '^chapter-[0-9]+\.(m2ts|xml)$' -or $_.Name -match '^bd-split\.split\.[0-9]+\.m2ts$' -or $_.Name -eq 'chapters.xml' })) {
        Remove-Item -LiteralPath $stale.FullName -Force
    }

    # --- Meta file ---
    $utf8NoBom = New-Object System.Text.UTF8Encoding $false
    $meta = Join-Path $Job.EpisodeDir 'bd-split.meta'
    $metaLines = @()
    $out = ''
    if ($mode -eq 'chapters') {
        $metaLines += 'MUXOPT --split-chapters'
        $out = Join-Path $Job.EpisodeDir 'bd-split.m2ts'
    } else {
        # Keep chapters rangeA..rangeB. --cut-pictures inside starts at
        # the first key picture AT OR AFTER the cut — no previous-chapter
        # pre-roll (compliant BDs place entry marks on key pictures, so
        # the cut is exact there). --start-time pins the mux base to 600s
        # so the ffprobe rebase below recovers the source-relative content
        # start from the first video PTS.
        $muxopt = 'MUXOPT --cut-start=' + $marks[$rangeA - 1] + ' --cut-pictures=inside --start-time=27000000'
        if ($rangeB -lt $marks.Count) { $muxopt += ' --cut-end=' + $marks[$rangeB] }
        $metaLines += $muxopt
        $out = Join-Path $Job.EpisodeDir 'src.m2ts'
        if (Test-Path -LiteralPath $out) { Remove-Item -LiteralPath $out -Force }
    }
    for ($i = 0; $i -lt $ids.Count; $i++) {
        $metaLines += ('{0}, "{1}", track={2}' -f $codecs[$i], $mpls, $ids[$i])
    }
    [System.IO.File]::WriteAllLines($meta, $metaLines, $utf8NoBom)
    Write-Output "ENCODE_STEP bd_split 60"
    Invoke-Tool -ExePath $tsmuxer -Arguments @($meta, $out) -Label 'tsMuxeR'

    if ($mode -eq 'range') {
        if (-not (Test-Path -LiteralPath $out -PathType Leaf)) {
            throw "tsMuxeR finished but $out was not created"
        }
        if ((Get-Item -LiteralPath $out).Length -lt 192) {
            throw "src.m2ts is smaller than one 192-byte source packet - mux produced no data"
        }
        # Exact rebase against the MEASURED output start: tsMuxeR keeps
        # source-relative PTS offset by the pinned 600s mux base, so the
        # first video PTS minus 600 is the source time of the first kept
        # picture (the key picture at/after the cut mark). Nominal mark
        # math would silently shift every chapter by the keyframe snap.
        $prevEAP = $ErrorActionPreference
        $ErrorActionPreference = 'Continue'
        try { $fp = & $ffprobe '-v' 'error' '-select_streams' 'v:0' '-show_entries' 'packet=pts_time' '-of' 'default=noprint_wrappers=1:nokey=1' '-read_intervals' '%+#1' $out 2>&1 } finally { $ErrorActionPreference = $prevEAP }
        if ($LASTEXITCODE -ne 0) { throw "bd_split: ffprobe failed on the cut output (exit $LASTEXITCODE)" }
        $firstPts = (@($fp) | ForEach-Object { "$_" } | Select-Object -First 1).Trim()
        if ($firstPts -notmatch '^[0-9]+(\.[0-9]+)?$') {
            throw "bd_split: ffprobe returned no usable first video PTS ('$($firstPts -replace '[\r\n]', ' ')')"
        }
        $startMs = [int][math]::Round(([double]$firstPts - 600) * 1000)
        if ($startMs -lt 0) {
            throw "bd_split: cut output starts before the pinned mux base (first PTS $firstPts) - unexpected tsMuxeR timing"
        }
        $xml = @('<?xml version="1.0" encoding="UTF-8"?>', '<Chapters>', '  <EditionEntry>')
        for ($i = $rangeA - 1; $i -lt $rangeB; $i++) {
            $mp = $marks[$i] -split '[:.]'
            $ms = ([int]$mp[0] * 3600 + [int]$mp[1] * 60 + [int]$mp[2]) * 1000 + [int]$mp[3]
            $rel = $ms - $startMs
            # The first kept mark can predate the key picture the cut
            # actually starts at (off-keyframe mark): clamp to 0.
            if ($rel -lt 0) { $rel = 0 }
            $rh = [math]::Floor($rel / 3600000)
            $rm = [math]::Floor(($rel % 3600000) / 60000)
            $rs = [math]::Floor(($rel % 60000) / 1000)
            $rf = ($rel % 1000) * 1000000
            $xml += '    <ChapterAtom>'
            $xml += ('      <ChapterTimeStart>{0:D2}:{1:D2}:{2:D2}.{3:D9}</ChapterTimeStart>' -f $rh, $rm, $rs, $rf)
            $xml += '      <ChapterDisplay>'
            $xml += ('        <ChapterString>Chapter {0:D2}</ChapterString>' -f ($i + 1))
            $xml += '        <ChapterLanguage>eng</ChapterLanguage>'
            $xml += '      </ChapterDisplay>'
            $xml += '    </ChapterAtom>'
        }
        $xml += '  </EditionEntry>'
        $xml += '</Chapters>'
        [System.IO.File]::WriteAllLines((Join-Path $Job.EpisodeDir 'chapters.xml'), $xml, $utf8NoBom)
        Write-Output "[bd_split] kept chapters $rangeA-$rangeB -> src.m2ts + chapters.xml"
        Write-Output "ENCODE_STEP bd_split 100"
        return
    }

    # --- chapters mode: rename split parts to chapter-NN.m2ts + sidecars ---
    # A playlist whose FIRST mark is non-zero gets an extra pre-chapter
    # segment (0 -> first mark) from --split-chapters: N marks -> N+1
    # parts (verified against real tsMuxeR 3.1.0). It maps to chapter-00.
    $prePart = 0
    if ($marks[0] -ne '00:00:00.000') { $prePart = 1 }
    $expectedParts = $marks.Count + $prePart
    $partCount = @(Get-ChildItem -LiteralPath $Job.EpisodeDir -File -Filter 'bd-split.split.*.m2ts').Count
    if ($partCount -ne $expectedParts) {
        throw "tsMuxeR produced $partCount split parts but expected $expectedParts ($($marks.Count) chapters, pre-chapter segment: $prePart)"
    }
    for ($p = 1; $p -le $expectedParts; $p++) {
        $cn = $p - $prePart
        $part = Join-Path $Job.EpisodeDir "bd-split.split.$p.m2ts"
        if (-not (Test-Path -LiteralPath $part -PathType Leaf)) {
            throw "tsMuxeR split part missing: $part"
        }
        if ((Get-Item -LiteralPath $part).Length -lt 192) {
            throw "split part $part is smaller than one 192-byte source packet"
        }
        $nn = '{0:D2}' -f $cn
        # -Force: overwrite a stale file at the target (bash mv -f parity).
        Rename-Item -LiteralPath $part -NewName "chapter-$nn.m2ts" -Force
        # Each part is exactly one chapter: a single mark at 0, original
        # chapter number kept in the display name (chapter-00 = the
        # pre-chapter segment when the first mark is non-zero).
        $xml = @(
            '<?xml version="1.0" encoding="UTF-8"?>',
            '<Chapters>',
            '  <EditionEntry>',
            '    <ChapterAtom>',
            '      <ChapterTimeStart>00:00:00.000000000</ChapterTimeStart>',
            '      <ChapterDisplay>',
            ('        <ChapterString>Chapter {0}</ChapterString>' -f $nn),
            '        <ChapterLanguage>eng</ChapterLanguage>',
            '      </ChapterDisplay>',
            '    </ChapterAtom>',
            '  </EditionEntry>',
            '</Chapters>'
        )
        [System.IO.File]::WriteAllLines((Join-Path $Job.EpisodeDir "chapter-$nn.xml"), $xml, $utf8NoBom)
    }
    Write-Output "[bd_split] wrote $expectedParts chapter files (chapter-$('{0:D2}' -f (1 - $prePart)).m2ts .. chapter-$('{0:D2}' -f $marks.Count).m2ts)"
    Write-Output "ENCODE_STEP bd_split 100"
}`,
	}
}
