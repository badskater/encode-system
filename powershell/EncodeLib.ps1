# EncodeLib.ps1 — encode step implementations for encode-system agents.
#
# Pushed to nodes by the agent's auto-update system; do NOT hand-edit copies
# on nodes. Every function writes structured progress lines the agent parses:
#
#   ENCODE_STEP <name> <pct>     progress update (0-100)
#   ENCODE_STEP_FAILED <name> <msg>
#   ENCODE_JOB_DONE
#
# Conventions: Set-StrictMode latest, Stop on error, all external tools are
# invoked via & with full paths from $BinDir (C:\bin on nodes).

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Resolve-Tool returns the full path to a tool in $BinDir, failing fast when
# the binary is missing so jobs fail with a clear message instead of a
# confusing "not recognized" error.
function Resolve-Tool {
    param(
        [Parameter(Mandatory)] [string] $BinDir,
        [Parameter(Mandatory)] [string] $Name
    )
    $path = Join-Path $BinDir $Name
    if (-not (Test-Path -LiteralPath $path)) {
        throw "required tool not found: $path (is C:\bin deployed?)"
    }
    return $path
}

# Invoke-Tool runs an external process, streams its output with a prefix, and
# throws when the exit code is non-zero. Stdout/stderr are merged so x265's
# progress (written to stderr) appears in order in the job log.
function Invoke-Tool {
    param(
        [Parameter(Mandatory)] [string] $ExePath,
        [Parameter(Mandatory)] [string[]] $Arguments,
        [string] $Label = (Split-Path $ExePath -Leaf)
    )
    Write-Output "[$Label] starting: $ExePath $($Arguments -join ' ')"
    # Windows PowerShell 5.1 wraps native stderr as ErrorRecords; under
    # ErrorActionPreference=Stop that aborts on harmless progress output
    # (x265 writes progress to stderr). Relax EAP for the native call and
    # rely on the exit code for failure detection instead.
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & $ExePath @Arguments 2>&1 | ForEach-Object {
            Write-Output "[$Label] $_"
        }
    } finally {
        $ErrorActionPreference = $prevEAP
    }
    $code = $LASTEXITCODE
    if ($code -ne 0) {
        throw "$Label exited with code $code"
    }
    Write-Output "[$Label] finished (exit 0)"
}

# Assert-SafeName rejects values that could escape the intended directory or
# break Windows filenames. Controller data is admin-authored, but this
# boundary is where it meets the filesystem — defense in depth.
function Assert-SafeName {
    param(
        [Parameter(Mandatory)] [string] $Value,
        [Parameter(Mandatory)] [string] $What
    )
    if ($Value -match '[\\/:*?"<>|]' -or $Value.Contains('..') -or $Value.Trim() -eq '') {
        throw "unsafe $What value: '$Value' (path separators, '..' or reserved characters are not allowed)"
    }
}

# Find-SourceFile locates the episode's source media file. Prefers the
# canonical src.* names, otherwise picks the first supported extension.
function Find-SourceFile {
    param([Parameter(Mandatory)] [string] $EpisodeDir)
    foreach ($name in @('src.m2ts', 'src.ts', 'src.mkv', 'src.mp4')) {
        $p = Join-Path $EpisodeDir $name
        if (Test-Path -LiteralPath $p) { return $p }
    }
    foreach ($ext in @('*.m2ts', '*.ts', '*.mkv', '*.mp4')) {
        $hit = Get-ChildItem -LiteralPath $EpisodeDir -Filter $ext -File | Select-Object -First 1
        if ($hit) { return $hit.FullName }
    }
    throw "no source media file found in $EpisodeDir"
}

# ---------------------------------------------------------------------------
# Step: source_rename
# ---------------------------------------------------------------------------

# Invoke-SourceRename renames the raw upload to the canonical src.<ext>,
# mirroring the legacy "rename *.m2ts src.m2ts" guard.
function Invoke-SourceRename {
    param(
        [Parameter(Mandatory)] [string] $EpisodeDir,
        [string] $SourceName = 'src'
    )
    Write-Output "ENCODE_STEP source_rename 0"
    foreach ($ext in @('.m2ts', '.ts', '.mkv', '.mp4')) {
        $target = Join-Path $EpisodeDir ($SourceName + $ext)
        if (Test-Path -LiteralPath $target) {
            Write-Output "source already named $($SourceName + $ext)"
            Write-Output "ENCODE_STEP source_rename 100"
            return
        }
    }
    $raws = @(Get-ChildItem -LiteralPath $EpisodeDir -File |
        Where-Object { $_.Extension -in @('.m2ts', '.ts', '.mkv', '.mp4') })
    if ($raws.Count -eq 0) {
        throw "no source media file to rename in $EpisodeDir"
    }
    if ($raws.Count -gt 1) {
        # Ambiguous uploads: picking one silently could canonicalize the
        # wrong file. Require the operator to resolve the ambiguity.
        $names = ($raws | ForEach-Object { $_.Name }) -join ', '
        throw "multiple source candidates in $EpisodeDir ($names) - rename one to $SourceName manually"
    }
    $raw = $raws[0]
    $target = Join-Path $EpisodeDir ($SourceName + $raw.Extension)
    Rename-Item -LiteralPath $raw.FullName -NewName ($SourceName + $raw.Extension)
    Write-Output "renamed $($raw.Name) -> $(Split-Path $target -Leaf)"
    Write-Output "ENCODE_STEP source_rename 100"
}

# ---------------------------------------------------------------------------
# Step: dgindex
# ---------------------------------------------------------------------------

# Invoke-DgIndex builds src.dgi from src.<ext> with DGIndexNV (-h = hide UI).
function Invoke-DgIndex {
    param(
        [Parameter(Mandatory)] [string] $BinDir,
        [Parameter(Mandatory)] [string] $EpisodeDir
    )
    Write-Output "ENCODE_STEP dgindex 0"
    $exe = Resolve-Tool $BinDir 'DGIndexNV.exe'
    $src = Find-SourceFile $EpisodeDir
    $dgi = Join-Path $EpisodeDir 'src.dgi'
    if ((Test-Path -LiteralPath $dgi) -and ((Get-Item -LiteralPath $dgi).LastWriteTime -gt (Get-Item -LiteralPath $src).LastWriteTime)) {
        Write-Output "src.dgi already present and newer than source, reusing"
        Write-Output "ENCODE_STEP dgindex 100"
        return
    }
    if (Test-Path -LiteralPath $dgi) {
        Write-Output "src.dgi is stale (older than source), regenerating"
        Remove-Item -LiteralPath $dgi -Force
    }
    Invoke-Tool -ExePath $exe -Arguments @('-i', $src, '-o', $dgi, '-h') -Label 'DGIndexNV'
    if (-not (Test-Path -LiteralPath $dgi)) {
        throw "DGIndexNV finished but $dgi was not created"
    }
    Write-Output "ENCODE_STEP dgindex 100"
}

# ---------------------------------------------------------------------------
# Step: audio (eac3to -> WAV -> opusenc -> Opus)
# ---------------------------------------------------------------------------

# Invoke-AudioExtract demuxes one audio track to 16-bit WAV with eac3to, then
# encodes it to Opus with the standalone opusenc. The intermediate WAV is
# removed on success unless -KeepWav is set (debug aid).
function Invoke-AudioExtract {
    param(
        [Parameter(Mandatory)] [string] $BinDir,
        [Parameter(Mandatory)] [string] $EpisodeDir,
        [string] $TrackIndex = '2',
        [string] $Bitrate = '320',
        [switch] $KeepWav
    )
    Write-Output "ENCODE_STEP audio 0"
    $eac3to = Resolve-Tool $BinDir 'eac3to.exe'
    $opusenc = Resolve-Tool $BinDir 'opusenc.exe'
    $src = Find-SourceFile $EpisodeDir
    $wav = Join-Path $EpisodeDir 'audio.wav'
    $opus = Join-Path $EpisodeDir 'audio.opus'

    if ((Test-Path -LiteralPath $opus) -and ((Get-Item -LiteralPath $opus).LastWriteTime -gt (Get-Item -LiteralPath $src).LastWriteTime)) {
        Write-Output "audio.opus already present and newer than source, reusing"
        Write-Output "ENCODE_STEP audio 100"
        return
    }
    if (Test-Path -LiteralPath $opus) {
        Write-Output "audio.opus is stale (older than source), re-encoding"
        Remove-Item -LiteralPath $opus -Force
    }

    # eac3to "src" 2: audio.wav -down16  → track 2, 16-bit WAV output.
    Write-Output "ENCODE_STEP audio 10"
    Invoke-Tool -ExePath $eac3to -Arguments @($src, "${TrackIndex}:", $wav, '-down16') -Label 'eac3to'
    if (-not (Test-Path -LiteralPath $wav)) {
        throw "eac3to finished but $wav was not created (check track index $TrackIndex)"
    }

    # opusenc: encode at the flow's bitrate (kbps).
    Write-Output "ENCODE_STEP audio 60"
    Invoke-Tool -ExePath $opusenc -Arguments @('--bitrate', $Bitrate, $wav, $opus) -Label 'opusenc'
    if (-not (Test-Path -LiteralPath $opus)) {
        throw "opusenc finished but $opus was not created"
    }

    if (-not $KeepWav) {
        Remove-Item -LiteralPath $wav -Force
        Write-Output "removed intermediate WAV"
    }
    Write-Output "ENCODE_STEP audio 100"
}

# ---------------------------------------------------------------------------
# Step: encode (x265 fork reading .avs/.vpy)
# ---------------------------------------------------------------------------

# Invoke-VideoEncode runs the x265_x64 fork on the episode's AviSynth or
# VapourSynth script. Arguments come from the flow (x265_args param), which
# defaults to the legacy proven parameter set.
# EncodeHevcName is the shared encode-output contract between Invoke-VideoEncode
# and Invoke-Mux; both steps must agree on the filename.
$script:EncodeHevcName = '1080.hevc'

function Invoke-VideoEncode {
    param(
        [Parameter(Mandatory)] [string] $BinDir,
        [Parameter(Mandatory)] [string] $EpisodeDir,
        [Parameter(Mandatory)] [string] $ScriptFile,
        [Parameter(Mandatory)] [string] $OutputFile,
        [Parameter(Mandatory)] [string] $X265Args
    )
    Write-Output "ENCODE_STEP encode 0"
    $x265 = Resolve-Tool $BinDir 'x265_x64.exe'
    if (-not (Test-Path -LiteralPath $ScriptFile)) {
        throw "filter script not found: $ScriptFile"
    }
    # The flow's arg string is trusted controller content (admin-authored).
    # Note: $args is a PowerShell automatic variable — never assign to it.
    # Quoted segments survive as single argv entries (quote-aware split).
    # Input/output are appended last so flow params can never override them.
    $x265ArgList = @([regex]::Matches($X265Args, '("([^"]*)"|\S+)') | ForEach-Object { $_.Value.Trim('"') })
    $x265ArgList += @('--input', $ScriptFile, '-o', $OutputFile)
    Invoke-Tool -ExePath $x265 -Arguments $x265ArgList -Label 'x265'
    if (-not (Test-Path -LiteralPath $OutputFile)) {
        throw "x265 finished but $OutputFile was not created"
    }
    Write-Output "ENCODE_STEP encode 100"
}

# ---------------------------------------------------------------------------
# Step: mux (mkvmerge)
# ---------------------------------------------------------------------------

# Invoke-Mux combines 1080.hevc + audio.opus into the release MKV using the
# legacy track layout: video jpn/default, audio jpn, no chapters/global tags.
function Invoke-Mux {
    param(
        [Parameter(Mandatory)] [string] $BinDir,
        [Parameter(Mandatory)] [string] $EpisodeDir,
        [Parameter(Mandatory)] [string] $OutputName
    )
    Write-Output "ENCODE_STEP mux 0"
    $mkvmerge = Resolve-Tool $BinDir 'mkvmerge.exe'
    $hevc = Join-Path $EpisodeDir $script:EncodeHevcName
    $audio = Join-Path $EpisodeDir 'audio.opus'
    $out = Join-Path $EpisodeDir $OutputName
    foreach ($f in @($hevc, $audio)) {
        if (-not (Test-Path -LiteralPath $f)) { throw "mux input missing: $f" }
    }
    $arguments = @(
        '-o', $out, '--quiet',
        '--language', '0:jpn', '--track-name', '0:Video',
        '--default-track', '0:yes', '--forced-track', '0:no',
        '-d', '0', '-A', '-S', '-T', '--no-global-tags', '--no-chapters',
        '(', $hevc, ')',
        '--language', '0:jpn', '--track-name', '0:Audio', '--forced-track', '0:no',
        '-a', '0', '-D', '-S', '-T', '--no-global-tags', '--no-chapters',
        '(', $audio, ')',
        '--track-order', '0:0,1:0'
    )
    Invoke-Tool -ExePath $mkvmerge -Arguments $arguments -Label 'mkvmerge'
    if (-not (Test-Path -LiteralPath $out)) {
        throw "mkvmerge finished but $out was not created"
    }
    Write-Output "ENCODE_STEP mux 100"
}

# ---------------------------------------------------------------------------
# Step: release_copy
# ---------------------------------------------------------------------------

# Invoke-ReleaseCopy copies the finished MKV into the release folder pattern
# [Group] Series - Raws [Tag]/ on the release share, creating it if needed.
function Invoke-ReleaseCopy {
    param(
        [Parameter(Mandatory)] [string] $EpisodeDir,
        [Parameter(Mandatory)] [string] $ReleaseDir,
        [Parameter(Mandatory)] [string] $ReleaseFolder,
        [Parameter(Mandatory)] [string] $OutputName
    )
    Write-Output "ENCODE_STEP release_copy 0"
    Assert-SafeName -Value $ReleaseFolder -What "release folder"
    Assert-SafeName -Value $OutputName -What "output name"
    $src = Join-Path $EpisodeDir $OutputName
    if (-not (Test-Path -LiteralPath $src)) { throw "finished MKV not found: $src" }
    $destDir = Join-Path $ReleaseDir $ReleaseFolder
    if (-not (Test-Path -LiteralPath $destDir)) {
        New-Item -ItemType Directory -Path $destDir -Force | Out-Null
        Write-Output "created release folder: $destDir"
    }
    $dest = Join-Path $destDir $OutputName
    $existed = Test-Path -LiteralPath $dest
    Copy-Item -LiteralPath $src -Destination $dest -Force
    if ($existed) {
        Write-Output "OVERWROTE existing release: $dest"
    } else {
        Write-Output "copied to $dest"
    }
    # Post-copy integrity check: sizes must match over the NFS share.
    if ((Get-Item -LiteralPath $src).Length -ne (Get-Item -LiteralPath $dest).Length) {
        throw "release copy size mismatch: $dest"
    }
    Write-Output "ENCODE_STEP release_copy 100"
}

# ---------------------------------------------------------------------------
# Step: keyframes (ffmpeg -> y4m -> SCXvid)
# ---------------------------------------------------------------------------

# Invoke-Keyframes generates the scene-change keyframes file via an
# ffmpeg→y4m pipe into SCXvid, matching the legacy batch script. Skipped when
# the keyframes file already exists.
function Invoke-Keyframes {
    param(
        [Parameter(Mandatory)] [string] $BinDir,
        [Parameter(Mandatory)] [string] $ReleaseDir,
        [Parameter(Mandatory)] [string] $ReleaseFolder,
        [Parameter(Mandatory)] [string] $Series,
        [Parameter(Mandatory)] [string] $Episode,
        [Parameter(Mandatory)] [string] $OutputName
    )
    Write-Output "ENCODE_STEP keyframes 0"
    Assert-SafeName -Value $Series -What "series"
    Assert-SafeName -Value $Episode -What "episode"
    Assert-SafeName -Value $ReleaseFolder -What "release folder"
    $ffmpeg = Resolve-Tool $BinDir 'ffmpeg.exe'
    $scxvid = Resolve-Tool $BinDir 'SCXvid.exe'
    $destDir = Join-Path $ReleaseDir $ReleaseFolder
    $mkv = Join-Path $destDir $OutputName
    $kf = Join-Path $destDir "$Series - $Episode Keyframes.txt"
    if (Test-Path -LiteralPath $kf) {
        Write-Output "keyframes file exists, skipping"
        Write-Output "ENCODE_STEP keyframes 100"
        return
    }
    if (-not (Test-Path -LiteralPath $mkv)) { throw "release MKV not found: $mkv" }

    Write-Output "[keyframes] ffmpeg -> y4m -> SCXvid"
    # No cmd.exe pipe dependency: ffmpeg writes a downscaled y4m to a LOCAL
    # temp file (not the NFS release share), SCXvid reads it. Same output as
    # the legacy pipe, and it works identically on Windows PowerShell and pwsh.
    $y4m = Join-Path ([System.IO.Path]::GetTempPath()) "encode-kf-$PID.y4m"
    try {
        Invoke-Tool -ExePath $ffmpeg -Arguments @(
            '-i', $mkv, '-f', 'yuv4mpegpipe', '-vf', 'scale=640:360',
            '-pix_fmt', 'yuv420p', '-vsync', 'drop', '-loglevel', 'quiet', $y4m
        ) -Label 'ffmpeg'
        Invoke-Tool -ExePath $scxvid -Arguments @($y4m, $kf) -Label 'SCXvid'
    } finally {
        # Best-effort cleanup; a locked temp file must not fail the job.
        try { if (Test-Path -LiteralPath $y4m) { Remove-Item -LiteralPath $y4m -Force } } catch { }
    }
    if (-not (Test-Path -LiteralPath $kf)) { throw "SCXvid finished but $kf was not created" }
    Write-Output "ENCODE_STEP keyframes 100"
}
