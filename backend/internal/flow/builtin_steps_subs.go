// Subtitle/font/chapter extraction + muxing built-ins (P6).
//
// These four steps form the SEPARATE "default-subs" flow that runs as its
// OWN job AFTER the encode flow (the operator queues it; nothing chains it
// into the encode defaults automatically):
//
//	subtitle_extract → font_extract → chapter_extract → mux_extras
//
// DESIGN DECISIONS (documented in Docs/md/Operations.md):
//
//   - Source discovery: the three extraction steps read the episode's
//     source media via Find-SourceFile (src.* in the episode dir) — the
//     same contract the encode flow uses. They never touch the encoded
//     output.
//
//   - Track selection: subtitle_extract takes ALL subtitle tracks (fansub
//     releases keep every language/signs track); font_extract takes
//     attachments whose MIME type is a font type (content_type, with a
//     legacy mime_type fallback) or whose file name ends in
//     .ttf/.otf/.woff/.ttc; chapter_extract runs only when mkvmerge -J
//     reports chapter editions with entries.
//
//   - Artifact contracts (episode workdir): subs_<id>_<lang>.<ext> files +
//     subs.json manifest, fonts/<id>_<name> files + fonts.json manifest,
//     chapters.xml (Matroska XML chapters — mkvextract's default format,
//     which mkvmerge --chapters reads). Manifests are ALWAYS rewritten
//     (empty when nothing was found) so a re-run reflects the current
//     source; chapters.xml is removed on the no-chapters path.
//
//   - Graceful skips: no subtitle tracks / no font attachments / no
//     chapters is a SUCCESS (log line + ENCODE_STEP 100), never a failure.
//     mux_extras with no extracted extras at all succeeds WITHOUT touching
//     the output (no mkvmerge invocation).
//
//   - mux_extras output discovery: optional `output` param (explicit file
//     name in the episode dir) → the standard release-named MKV
//     ($EpisodeDir/$OutputName, exactly what mux writes and release_copy
//     reads) → newest non-source *.mkv in the episode dir. EVERY candidate
//     is checked against the source with FILE IDENTITY (Test-SameFile /
//     bash -ef), never path strings — the source-discovery fallback can
//     return the output itself, and muxing into the source would destroy
//     it (same guard class as quality_gate's self-compare protection).
//
//   - mkvmerge syntax (verified against mkvtoolnix.download/doc): the
//     encoded MKV is the FIRST input with --no-subtitles --no-chapters
//     --no-attachments (keep video+audio, strip any previous extras so
//     re-runs REPLACE instead of duplicating); each extracted subtitle
//     file follows with --no-video --no-audio plus its language/name/
//     default/forced flags (--default-track-flag / --forced-display-flag,
//     the current documented names); --chapters (global chapter option)
//     and --attach-file (global attachment option) carry the extracted
//     chapters/fonts. Natural input order keeps video, audio, then subs.
//
//   - Atomic replace: mkvmerge writes a UNIQUE temp
//     .mux_extras.<pid>.<rand>.tmp.mkv in the episode dir (same
//     filesystem), the result is verified non-empty, then renamed over
//     the output (Move-Item -Force / mv -f). A per-episode lock
//     (.mux_extras.lock) serializes concurrent runs; the temp is cleaned
//     up on every exit path and discovery excludes both temp patterns.
//     A failed or partial mux never corrupts the encoded output.
//
//   - Tool resolution: Windows resolves mkvmerge.exe/mkvextract.exe from
//     $Job.BinDir via Resolve-Tool; Linux uses the system PATH with a
//     $BinDir fallback (existing bash convention). Missing tools fail with
//     an error naming the tool AND the mkvtoolnix package.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

// SubsManifestName / FontsManifestName / ChaptersXMLName are the artifact
// contracts between the extraction steps and mux_extras (episode workdir).
const (
	SubsManifestName  = "subs.json"
	FontsManifestName = "fonts.json"
	ChaptersXMLName   = "chapters.xml"
	// MuxExtrasTempName is the temp output mkvmerge writes before the
	// atomic rename. Discovery excludes it so a crashed prior run's
	// leftover can never be picked as the encoded output.
	MuxExtrasTempName = ".mux_extras.tmp.mkv"
)

func subtitleExtractTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "subtitle_extract",
		Label:       "Subtitle extract",
		Description: "Extract ALL subtitle tracks from the source with mkvextract (mkvmerge -J identifies them). Writes subs_<id>_<lang>.<ext> plus a subs.json manifest (language/name/default/forced flags) for mux_extras. No subtitle tracks → logged graceful skip. Requires MKVToolNix (mkvmerge + mkvextract). Run as part of the default-subs flow AFTER the encode flow.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell:  subtitleExtractPS,
	}
}

func fontExtractTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "font_extract",
		Label:       "Font extract",
		Description: "Extract font attachments from the source with mkvextract (font MIME types or .ttf/.otf/.woff/.ttc names). Writes fonts/<id>_<name> plus a fonts.json manifest for mux_extras. No font attachments → logged graceful skip. Requires MKVToolNix (mkvmerge + mkvextract). Run as part of the default-subs flow AFTER the encode flow.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell:  fontExtractPS,
	}
}

func chapterExtractTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "chapter_extract",
		Label:       "Chapter extract",
		Description: "Extract chapters from the source with mkvextract chapters (Matroska XML format) into chapters.xml for mux_extras. No chapters → logged graceful skip (stale chapters.xml removed). Requires MKVToolNix (mkvmerge + mkvextract). Run as part of the default-subs flow AFTER the encode flow.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell:  chapterExtractPS,
	}
}

func muxExtrasTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "mux_extras",
		Label:       "Mux extras (subs/fonts/chapters)",
		Description: "Merge the extracted subtitles, fonts and chapters INTO the encoded output MKV (mkvmerge), then atomically replace it (temp + rename). Locates the encoded output: optional output param, else the standard release-named MKV, else the newest non-source MKV in the episode dir (file-identity guard — never the source). Re-runs replace extras instead of duplicating them. No extras extracted → succeeds without modifying the output. Missing encoded output → loud error. Requires MKVToolNix (mkvmerge). Run AFTER the encode flow (default-subs).",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "output", Label: "Encoded output filename (blank: auto-discover)", Placeholder: "Series - 01 [1080p].mkv"},
		},
		PowerShell: muxExtrasPS,
	}
}

const subtitleExtractPS = `function Invoke-SubtitleExtract {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP subtitle_extract 0"
    try {
        $mkvmerge = Resolve-Tool $Job.BinDir 'mkvmerge.exe'
        $mkvextract = Resolve-Tool $Job.BinDir 'mkvextract.exe'
    } catch {
        throw "subtitle_extract: MKVToolNix tools missing (mkvmerge.exe/mkvextract.exe in $($Job.BinDir)) - install the mkvtoolnix package. $($_.Exception.Message)"
    }
    $src = Find-SourceFile $Job.EpisodeDir
    $raw = & $mkvmerge -J $src
    if ($LASTEXITCODE -ne 0) {
        throw "subtitle_extract: mkvmerge -J exited with code $LASTEXITCODE"
    }
    $ident = ([string]::Join([char]10, $raw)) | ConvertFrom-Json
    $tracks = @()
    if ($null -ne $ident.PSObject.Properties['tracks'] -and $null -ne $ident.tracks) { $tracks = @($ident.tracks) }
    $subs = @($tracks | Where-Object { $_.type -eq 'subtitles' })
    $manifest = @()
    if ($subs.Count -eq 0) {
        Write-Output "[subtitle_extract] no subtitle tracks in $src - skipping"
    } else {
        Write-Output "ENCODE_STEP subtitle_extract 50"
        # One mkvextract call carries every track spec (source FIRST, then
        # the 'tracks' mode, then <id>:<dest> pairs - mkvextract syntax).
        $extractArgs = @($src, 'tracks')
        foreach ($t in $subs) {
            $id = [string]$t.id
            if ($id -notmatch '^[0-9]+$' -or $id.Length -gt 6) {
                throw "subtitle_extract: invalid subtitle track id '$id'"
            }
            # Null-safe properties access (StrictMode): a track may carry
            # no properties object at all — default everything then.
            $props = $null
            if ($null -ne $t.PSObject.Properties['properties']) { $props = $t.properties }
            # Untrusted metadata: strip C0 controls + DEL from language and
            # track_name and cap the name BEFORE they reach file names,
            # manifests or echoed command lines (marker/flag injection).
            $langRaw = ''
            if ($null -ne $props -and $null -ne $props.PSObject.Properties['language'] -and $null -ne $props.language) { $langRaw = [string]$props.language }
            $langRaw = $langRaw -replace '[\x00-\x1F\x7F]', ''
            $lang = 'und'
            if ($langRaw -match '^[a-z]{2,3}$') { $lang = $langRaw }
            # Extension from the Matroska codec id; unknown codecs extract
            # as raw .sub (logged) rather than guessing a text format.
            $codecId = ''
            if ($null -ne $props -and $null -ne $props.PSObject.Properties['codec_id'] -and $null -ne $props.codec_id) { $codecId = [string]$props.codec_id }
            $ext = 'sub'
            switch ($codecId) {
                'S_TEXT/UTF8'  { $ext = 'srt' }
                'S_TEXT/ASCII' { $ext = 'srt' }
                'S_TEXT/ASS'   { $ext = 'ass' }
                'S_ASS'        { $ext = 'ass' }
                'S_TEXT/SSA'   { $ext = 'ssa' }
                'S_SSA'        { $ext = 'ssa' }
                'S_TEXT/WEBVTT'{ $ext = 'vtt' }
                'S_HDMV/PGS'   { $ext = 'sup' }
            }
            $file = "subs_${id}_${lang}.${ext}"
            $extractArgs += "${id}:$(Join-Path $Job.EpisodeDir $file)"
            $name = ''
            if ($null -ne $props -and $null -ne $props.PSObject.Properties['track_name'] -and $null -ne $props.track_name) {
                $name = ([string]$props.track_name) -replace '[\x00-\x1F\x7F]', ''
                if ($name.Length -gt 200) { $name = $name.Substring(0, 200) }
            }
            $defVal = $null
            if ($null -ne $props -and $null -ne $props.PSObject.Properties['default_track']) { $defVal = $props.default_track }
            $def = ConvertTo-FlagYesNo $defVal
            $frcVal = $null
            if ($null -ne $props -and $null -ne $props.PSObject.Properties['forced_track']) { $frcVal = $props.forced_track }
            $frc = ConvertTo-FlagYesNo $frcVal
            $manifest += [ordered]@{ file = $file; language = $lang; track_name = $name; default_track = $def; forced_track = $frc }
            Write-Output "[subtitle_extract] track $id ($lang, $codecId) -> $file"
        }
        Invoke-Tool -ExePath $mkvextract -Arguments $extractArgs -Label 'mkvextract'
        foreach ($m in $manifest) {
            $p = Join-Path $Job.EpisodeDir $m.file
            if (-not (Test-Path -LiteralPath $p)) {
                throw "subtitle_extract: mkvextract finished but $p was not created"
            }
        }
        Write-Output "[subtitle_extract] extracted $($manifest.Count) subtitle track(s)"
    }
    # Always rewrite the manifest (empty when nothing was found) so a
    # re-run reflects the current source and mux_extras never reads stale
    # entries. -InputObject keeps single-element arrays as arrays.
    $manifestPath = Join-Path $Job.EpisodeDir 'subs.json'
    ConvertTo-Json -InputObject @($manifest) -Depth 5 | Set-Content -LiteralPath $manifestPath -Encoding UTF8
    Write-Output "ENCODE_STEP subtitle_extract 100"
}`

const fontExtractPS = `function Invoke-FontExtract {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP font_extract 0"
    try {
        $mkvmerge = Resolve-Tool $Job.BinDir 'mkvmerge.exe'
        $mkvextract = Resolve-Tool $Job.BinDir 'mkvextract.exe'
    } catch {
        throw "font_extract: MKVToolNix tools missing (mkvmerge.exe/mkvextract.exe in $($Job.BinDir)) - install the mkvtoolnix package. $($_.Exception.Message)"
    }
    $src = Find-SourceFile $Job.EpisodeDir
    $raw = & $mkvmerge -J $src
    if ($LASTEXITCODE -ne 0) {
        throw "font_extract: mkvmerge -J exited with code $LASTEXITCODE"
    }
    $ident = ([string]::Join([char]10, $raw)) | ConvertFrom-Json
    $attachments = @()
    if ($null -ne $ident.PSObject.Properties['attachments'] -and $null -ne $ident.attachments) { $attachments = @($ident.attachments) }
    # Font detection: schema v20 reports content_type; older identify
    # output used mime_type - accept both. Extension fallback catches
    # fonts attached with a generic MIME type.
    $fontMimes = @('application/x-truetype-font','application/vnd.ms-opentype','application/font-sfnt','application/x-font-ttf','application/x-font-otf','application/font-woff','font/ttf','font/otf','font/woff','font/collection','font/sfnt')
    $fonts = @()
    foreach ($a in $attachments) {
        $mime = ''
        if ($a.PSObject.Properties['content_type'] -and $a.content_type) { $mime = [string]$a.content_type }
        elseif ($a.PSObject.Properties['mime_type'] -and $a.mime_type) { $mime = [string]$a.mime_type }
        # MIME types are case-insensitive — lowercase before matching so
        # FONT/TTF extracts exactly like font/ttf (bash parity).
        $mime = $mime.ToLowerInvariant()
        $fname = ''
        if ($a.PSObject.Properties['file_name'] -and $a.file_name) { $fname = [string]$a.file_name }
        $isFont = ($mime -in $fontMimes) -or $mime.StartsWith('font/')
        if (-not $isFont -and $fname -match '\.(ttf|otf|woff|ttc)$') { $isFont = $true }
        if ($isFont) { $fonts += $a }
    }
    $manifest = @()
    if ($fonts.Count -eq 0) {
        Write-Output "[font_extract] no font attachments in $src - skipping"
    } else {
        Write-Output "ENCODE_STEP font_extract 50"
        $fontsDir = Join-Path $Job.EpisodeDir 'fonts'
        if (-not (Test-Path -LiteralPath $fontsDir)) {
            New-Item -ItemType Directory -Path $fontsDir -Force | Out-Null
        }
        $extractArgs = @($src, 'attachments')
        foreach ($a in $fonts) {
            $id = [string]$a.id
            if ($id -notmatch '^[0-9]+$' -or $id.Length -gt 6) {
                throw "font_extract: invalid attachment id '$id'"
            }
            $mime = ''
            if ($a.PSObject.Properties['content_type'] -and $a.content_type) { $mime = [string]$a.content_type }
            elseif ($a.PSObject.Properties['mime_type'] -and $a.mime_type) { $mime = [string]$a.mime_type }
            $mime = $mime.ToLowerInvariant()
            $fname = ''
            if ($a.PSObject.Properties['file_name'] -and $a.file_name) { $fname = [string]$a.file_name }
            # Sanitize the attachment name for the filesystem (no path
            # separators / reserved chars); fall back to a MIME-derived
            # name when the attachment carries no usable file name.
            $safe = ($fname -replace '[^A-Za-z0-9._-]', '_')
            if ($safe.Length -gt 100) { $safe = $safe.Substring(0, 100) }
            if ($safe -eq '' -or $safe -eq '.' -or $safe -eq '..') {
                $ext = '.bin'
                if ($mime -in @('application/x-truetype-font','application/x-font-ttf','font/ttf')) { $ext = '.ttf' }
                elseif ($mime -in @('application/vnd.ms-opentype','application/x-font-otf','font/otf')) { $ext = '.otf' }
                elseif ($mime -in @('application/font-woff','font/woff')) { $ext = '.woff' }
                elseif ($mime -eq 'font/collection') { $ext = '.ttc' }
                $safe = "attachment_${id}${ext}"
            }
            $file = "fonts/${id}_${safe}"
            $extractArgs += "${id}:$(Join-Path $Job.EpisodeDir $file)"
            $manifest += [ordered]@{ file = $file }
            Write-Output "[font_extract] attachment $id ($mime) -> $file"
        }
        Invoke-Tool -ExePath $mkvextract -Arguments $extractArgs -Label 'mkvextract'
        foreach ($m in $manifest) {
            $p = Join-Path $Job.EpisodeDir $m.file
            if (-not (Test-Path -LiteralPath $p)) {
                throw "font_extract: mkvextract finished but $p was not created"
            }
        }
        Write-Output "[font_extract] extracted $($manifest.Count) font attachment(s)"
    }
    $manifestPath = Join-Path $Job.EpisodeDir 'fonts.json'
    ConvertTo-Json -InputObject @($manifest) -Depth 5 | Set-Content -LiteralPath $manifestPath -Encoding UTF8
    Write-Output "ENCODE_STEP font_extract 100"
}`

const chapterExtractPS = `function Invoke-ChapterExtract {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP chapter_extract 0"
    try {
        $mkvmerge = Resolve-Tool $Job.BinDir 'mkvmerge.exe'
        $mkvextract = Resolve-Tool $Job.BinDir 'mkvextract.exe'
    } catch {
        throw "chapter_extract: MKVToolNix tools missing (mkvmerge.exe/mkvextract.exe in $($Job.BinDir)) - install the mkvtoolnix package. $($_.Exception.Message)"
    }
    $src = Find-SourceFile $Job.EpisodeDir
    $raw = & $mkvmerge -J $src
    if ($LASTEXITCODE -ne 0) {
        throw "chapter_extract: mkvmerge -J exited with code $LASTEXITCODE"
    }
    $ident = ([string]::Join([char]10, $raw)) | ConvertFrom-Json
    # mkvmerge -J reports chapter editions at the top level; an edition
    # with zero entries carries no chapters. Sum entries across editions.
    # EVERY edition is validated (fail closed, bash parity): a non-object
    # edition or a missing/null num_entries is an invalid edition; a
    # present value must be 1-9 digits (no negatives, floats or overflow).
    $editions = @()
    if ($null -ne $ident.PSObject.Properties['chapters'] -and $null -ne $ident.chapters) { $editions = @($ident.chapters) }
    $totalEntries = [long]0
    foreach ($ed in $editions) {
        if ($null -eq $ed -or $ed -isnot [pscustomobject] -or $null -eq $ed.PSObject.Properties['num_entries'] -or $null -eq $ed.num_entries) {
            throw "chapter_extract: invalid chapter edition (expected an object with num_entries)"
        }
        if ($ed.num_entries -is [System.Array]) {
            throw "chapter_extract: invalid chapter num_entries (expected a scalar)"
        }
        $n = [string]$ed.num_entries
        # \A..\z — .NET's $ would tolerate a trailing newline.
        if ($n -notmatch '\A[0-9]{1,9}\z') {
            throw "chapter_extract: invalid chapter num_entries '$n'"
        }
        $totalEntries += [long]$n
    }
    $dest = Join-Path $Job.EpisodeDir 'chapters.xml'
    if ($totalEntries -eq 0) {
        Write-Output "[chapter_extract] no chapters in $src - skipping"
        if (Test-Path -LiteralPath $dest) {
            Remove-Item -LiteralPath $dest -Force
            Write-Output "[chapter_extract] removed stale chapters.xml"
        }
    } else {
        Write-Output "ENCODE_STEP chapter_extract 50"
        # mkvextract chapters <source> <dest> writes Matroska XML chapters
        # (the default format) - exactly what mkvmerge --chapters reads.
        Invoke-Tool -ExePath $mkvextract -Arguments @($src, 'chapters', $dest) -Label 'mkvextract'
        if (-not (Test-Path -LiteralPath $dest) -or (Get-Item -LiteralPath $dest).Length -le 0) {
            throw "chapter_extract: mkvextract finished but $dest was not created"
        }
        Write-Output "[chapter_extract] extracted $totalEntries chapter entries"
    }
    Write-Output "ENCODE_STEP chapter_extract 100"
}`

const muxExtrasPS = `function Invoke-MuxExtras {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP mux_extras 0"
    try {
        $mkvmerge = Resolve-Tool $Job.BinDir 'mkvmerge.exe'
    } catch {
        throw "mux_extras: MKVToolNix tools missing (mkvmerge.exe in $($Job.BinDir)) - install the mkvtoolnix package. $($_.Exception.Message)"
    }
    # --- gather extracted extras (manifests written by the extract steps) ---
    # Manifests are a trust boundary even though machine-written: they must
    # be JSON ARRAYS. Array-ness is decided on the RAW text (first
    # non-whitespace char '[') because ConvertFrom-Json unwraps single-
    # element arrays and returns $null for '[]' — then @() wrap restores
    # the array shape (5.1-safe). Scalars/objects/empty files fail closed.
    $subs = @()
    $subsJson = Join-Path $Job.EpisodeDir 'subs.json'
    if (Test-Path -LiteralPath $subsJson) {
        try {
            $rawSubs = [System.IO.File]::ReadAllText($subsJson)
            # Fail CLOSED on an empty/truncated manifest (bash jq errors on
            # it; ConvertFrom-Json would silently return null = "no subs").
            if ($rawSubs.Trim() -eq '') { throw "empty file" }
            if (-not $rawSubs.TrimStart().StartsWith('[')) { throw "manifest must be a JSON array" }
            $sj = $rawSubs | ConvertFrom-Json
            if ($null -ne $sj) { $subs = @($sj) }
        } catch {
            throw "mux_extras: subs.json is unreadable ($($_.Exception.Message))"
        }
    }
    $fonts = @()
    $fontsJson = Join-Path $Job.EpisodeDir 'fonts.json'
    if (Test-Path -LiteralPath $fontsJson) {
        try {
            $rawFonts = [System.IO.File]::ReadAllText($fontsJson)
            if ($rawFonts.Trim() -eq '') { throw "empty file" }
            if (-not $rawFonts.TrimStart().StartsWith('[')) { throw "manifest must be a JSON array" }
            $fj = $rawFonts | ConvertFrom-Json
            if ($null -ne $fj) { $fonts = @($fj) }
        } catch {
            throw "mux_extras: fonts.json is unreadable ($($_.Exception.Message))"
        }
    }
    $chaptersXml = Join-Path $Job.EpisodeDir 'chapters.xml'
    $hasChapters = (Test-Path -LiteralPath $chaptersXml) -and ((Get-Item -LiteralPath $chaptersXml).Length -gt 0)
    if ($subs.Count -eq 0 -and $fonts.Count -eq 0 -and -not $hasChapters) {
        Write-Output "[mux_extras] no extracted extras found (subs/fonts/chapters) - output unchanged"
        Write-Output "ENCODE_STEP mux_extras 100"
        return
    }
    # Validate manifest entries before touching anything: relative names
    # only (no separators, '..', drive letters or control chars), existing
    # regular files, and the RESOLVED path must stay inside the episode
    # dir (a relative symlink pointing out is an exfil vector).
    $epRoot = [System.IO.Path]::GetFullPath($Job.EpisodeDir)
    try { $epRoot = [System.IO.Path]::GetFullPath((Resolve-LinkFullPath $Job.EpisodeDir)) } catch { }
    $epPrefix = $epRoot.TrimEnd([System.IO.Path]::DirectorySeparatorChar) + [System.IO.Path]::DirectorySeparatorChar
    $pathComparison = [System.StringComparison]::Ordinal
    if ([System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT) { $pathComparison = [System.StringComparison]::OrdinalIgnoreCase }
    foreach ($s in $subs) {
        if ($null -eq $s -or $s -isnot [pscustomobject] -or $null -eq $s.PSObject.Properties['file'] -or $null -eq $s.file) {
            throw "mux_extras: subs.json entry without a file name"
        }
        $sf = [string]$s.file
        if ($sf -eq '' -or $sf.Contains('\') -or $sf.StartsWith('/') -or $sf.Contains('..') -or $sf -match '^[A-Za-z]:' -or $sf -match '[\x00-\x1F\x7F]') {
            throw "mux_extras: unsafe subtitle file entry '$sf' (expected a relative path inside the episode dir)"
        }
        $sp = Join-Path $Job.EpisodeDir $sf
        if (-not (Test-Path -LiteralPath $sp -PathType Leaf)) {
            throw "mux_extras: subtitle file from subs.json missing: $sp (re-run subtitle_extract)"
        }
        $resolved = [System.IO.Path]::GetFullPath((Resolve-LinkFullPath $sp))
        if (-not $resolved.StartsWith($epPrefix, $pathComparison)) {
            throw "mux_extras: subtitle file entry '$sf' escapes the episode dir"
        }
    }
    foreach ($f in $fonts) {
        if ($null -eq $f -or $f -isnot [pscustomobject] -or $null -eq $f.PSObject.Properties['file'] -or $null -eq $f.file) {
            throw "mux_extras: fonts.json entry without a file name"
        }
        $ff = [string]$f.file
        if ($ff -eq '' -or $ff.Contains('\') -or $ff.StartsWith('/') -or $ff.Contains('..') -or $ff -match '^[A-Za-z]:' -or $ff -match '[\x00-\x1F\x7F]') {
            throw "mux_extras: unsafe font file entry '$ff' (expected a relative path inside the episode dir)"
        }
        $fp = Join-Path $Job.EpisodeDir $ff
        if (-not (Test-Path -LiteralPath $fp -PathType Leaf)) {
            throw "mux_extras: font file from fonts.json missing: $fp (re-run font_extract)"
        }
        $resolved = [System.IO.Path]::GetFullPath((Resolve-LinkFullPath $fp))
        if (-not $resolved.StartsWith($epPrefix, $pathComparison)) {
            throw "mux_extras: font file entry '$ff' escapes the episode dir"
        }
    }

    # --- locate the encoded output (NEVER the source) ---
    $src = ''
    try { $src = Find-SourceFile $Job.EpisodeDir } catch { $src = '' }
    $out = ''
    $explicit = if ($Params.output) { ([string]$Params.output).Trim() } else { '' }
    if ($explicit -ne '') {
        Assert-SafeName -Value $explicit -What "output name"
        $candidate = Join-Path $Job.EpisodeDir $explicit
        if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
            throw "mux_extras: output param file not found: $candidate"
        }
        # File-identity guard: an explicit param pointing at the source
        # (directly or via symlink/hardlink) must never be muxed into.
        if ($src -and (Test-SameFile $candidate $src)) {
            throw "mux_extras: output param points at the source media ($candidate) - refusing to modify the source"
        }
        $out = $candidate
    } else {
        # Standard release naming first (exactly what mux writes and
        # release_copy reads), then newest non-source MKV (covers outputs
        # renamed by crc32_rename in the encode job). Both mux temp patterns
        # are excluded so a crashed run's leftover is never picked.
        $candidates = @()
        $standard = Join-Path $Job.EpisodeDir $Job.OutputName
        if (Test-Path -LiteralPath $standard -PathType Leaf) { $candidates += $standard }
        $candidates += @(Get-ChildItem -LiteralPath $Job.EpisodeDir -Filter '*.mkv' -File |
            Where-Object { $_.Name -notlike '.mux_extras.*.tmp.mkv' -and $_.Name -ne '.mux_extras.tmp.mkv' } |
            Sort-Object LastWriteTime -Descending | ForEach-Object { $_.FullName })
        foreach ($c in $candidates) {
            if ($src -and (Test-SameFile $c $src)) { continue }
            $out = $c
            break
        }
    }
    if ($out -eq '') {
        throw "mux_extras: encoded output MKV not found in $($Job.EpisodeDir) (expected $($Job.OutputName); run AFTER the encode flow or set the output param)"
    }
    Write-Output "[mux_extras] encoded output: $out"
    Write-Output "ENCODE_STEP mux_extras 20"

    # --- per-episode lock + UNIQUE temp: concurrent mux_extras runs on the
    # same episode dir (two jobs, or a retry racing the original) must
    # serialize — a fixed temp name let them destroy each other's output
    # while both reported success. The lock file is shared with the bash
    # port (flock on the same inode); FileShare.None gives mutual exclusion
    # between PS runs (and, via .NET's Unix flock emulation, bash runs too).
    $lockPath = Join-Path $Job.EpisodeDir '.mux_extras.lock'
    $lockStream = $null
    $lockDeadline = [DateTime]::UtcNow.AddSeconds(600)
    while ($null -eq $lockStream) {
        try {
            $lockStream = [System.IO.File]::Open($lockPath, [System.IO.FileMode]::OpenOrCreate, [System.IO.FileAccess]::Write, [System.IO.FileShare]::None)
        } catch {
            if ([DateTime]::UtcNow -ge $lockDeadline) {
                throw "mux_extras: could not acquire the episode mux lock within 600s ($lockPath)"
            }
            Start-Sleep -Milliseconds 200
        }
    }
    $tmp = Join-Path $Job.EpisodeDir ".mux_extras.$PID.$([guid]::NewGuid().ToString('N').Substring(0, 8)).tmp.mkv"
    try {
        # --- mkvmerge: encoded first (keep video+audio, strip old extras so
        # re-runs replace instead of duplicating), then one input per sub file
        # with its flags; chapters + fonts ride the global options. ---
        $arguments = @('-o', $tmp, '--quiet')
        if ($hasChapters) { $arguments += @('--chapters', $chaptersXml) }
        foreach ($f in $fonts) {
            $arguments += @('--attach-file', (Join-Path $Job.EpisodeDir ([string]$f.file)))
        }
        $arguments += @('--no-subtitles', '--no-chapters', '--no-attachments', $out)
        foreach ($s in $subs) {
            # Re-sanitize untrusted manifest metadata (defense in depth —
            # the extract steps already sanitize, but a hand-placed or
            # older manifest may carry control chars / marker text).
            $langRaw = ''
            if ($null -ne $s.PSObject.Properties['language'] -and $null -ne $s.language) { $langRaw = [string]$s.language }
            $langRaw = $langRaw -replace '[\x00-\x1F\x7F]', ''
            $lang = 'und'
            if ($langRaw -match '^[a-z]{2,3}$') { $lang = $langRaw }
            $arguments += @('--no-video', '--no-audio', '--language', "0:$lang")
            $name = ''
            if ($null -ne $s.PSObject.Properties['track_name'] -and $null -ne $s.track_name) {
                $name = ([string]$s.track_name) -replace '[\x00-\x1F\x7F]', ''
                if ($name.Length -gt 200) { $name = $name.Substring(0, 200) }
            }
            if ($name -ne '') {
                $arguments += @('--track-name', "0:$name")
            }
            $defVal = $null
            if ($null -ne $s.PSObject.Properties['default_track']) { $defVal = $s.default_track }
            $def = ConvertTo-FlagYesNo $defVal
            $frcVal = $null
            if ($null -ne $s.PSObject.Properties['forced_track']) { $frcVal = $s.forced_track }
            $frc = ConvertTo-FlagYesNo $frcVal
            $arguments += @('--default-track-flag', "0:$def", '--forced-display-flag', "0:$frc", (Join-Path $Job.EpisodeDir ([string]$s.file)))
        }
        Write-Output "[mux_extras] mkvmerge $($arguments -join ' ')"
        Invoke-Tool -ExePath $mkvmerge -Arguments $arguments -Label 'mkvmerge'
        if (-not (Test-Path -LiteralPath $tmp)) {
            throw "mux_extras: mkvmerge finished but $tmp was not created"
        }
        if ((Get-Item -LiteralPath $tmp -Force).Length -le 0) {
            throw "mux_extras: mkvmerge produced a zero-length output: $tmp"
        }
        # Atomic replace: same-directory rename over the encoded output.
        Move-Item -LiteralPath $tmp -Destination $out -Force
    } finally {
        # Own-tmp cleanup on EVERY path + lock release.
        if (Test-Path -LiteralPath $tmp) {
            Remove-Item -LiteralPath $tmp -Force -ErrorAction SilentlyContinue
        }
        $lockStream.Dispose()
    }
    Write-Output "ENCODE_STEP mux_extras 80"
    $chLog = 'no'
    if ($hasChapters) { $chLog = 'yes' }
    Write-Output "[mux_extras] muxed $($subs.Count) subtitle track(s), $($fonts.Count) font attachment(s), chapters: $chLog -> $(Split-Path $out -Leaf)"
    Write-Output "ENCODE_STEP mux_extras 100"
}`
