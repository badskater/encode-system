// Package flow's built-in step templates. Every pipeline section owns its
// PowerShell function here; the renderer links each function into the final
// job script. Custom templates created in the UI follow the same convention.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

// EncodeHevcName is the shared encode-output contract between the encode and
// mux steps.
const EncodeHevcName = "1080.hevc"

// AudioWavName / AudioOpusName are the audio intermediate/final contract.
const (
	AudioWavName  = "audio.wav"
	AudioOpusName = "audio.opus"
)

// BuiltinStepTemplates returns the seeded step templates. They are
// upserted on controller boot, so editing one in the UI persists (the store
// overwrites by key) while a factory update re-applies on the next boot.
func BuiltinStepTemplates() []*model.StepTemplate {
	return []*model.StepTemplate{
		sourceRenameTemplate(),
		mediaProbeTemplate(),
		dgindexTemplate(),
		audioTemplate(),
		audioBranchTemplate(),
		encodeTemplate(),
		muxTemplate(),
		crc32RenameTemplate(),
		releaseCopyTemplate(),
		keyframesTemplate(),
	}
}

func sourceRenameTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "source_rename",
		Label:       "Rename source",
		Description: "Rename the raw upload to src.<ext> (legacy rename guard).",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "source_name", Label: "Source name", Placeholder: "src"},
		},
		PowerShell: `function Invoke-SourceRename {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP source_rename 0"
    $sourceName = if ($Params.source_name) { $Params.source_name } else { 'src' }
    Assert-SafeName -Value $sourceName -What "source name"
    foreach ($ext in @('.m2ts', '.ts', '.mkv', '.mp4')) {
        $target = Join-Path $Job.EpisodeDir ($sourceName + $ext)
        if (Test-Path -LiteralPath $target) {
            Write-Output "source already named $($sourceName + $ext)"
            Write-Output "ENCODE_STEP source_rename 100"
            return
        }
    }
    $raws = @(Get-ChildItem -LiteralPath $Job.EpisodeDir -File |
        Where-Object { $_.Extension -in @('.m2ts', '.ts', '.mkv', '.mp4') })
    if ($raws.Count -eq 0) {
        throw "no source media file to rename in $($Job.EpisodeDir)"
    }
    if ($raws.Count -gt 1) {
        $names = ($raws | ForEach-Object { $_.Name }) -join ', '
        throw "multiple source candidates in $($Job.EpisodeDir) ($names) - rename one to $sourceName manually"
    }
    Rename-Item -LiteralPath $raws[0].FullName -NewName ($sourceName + $raws[0].Extension)
    Write-Output "renamed $($raws[0].Name) -> $($sourceName + $raws[0].Extension)"
    Write-Output "ENCODE_STEP source_rename 100"
}`,
	}
}

func dgindexTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "dgindex",
		Label:       "DGIndexNV index",
		Description: "Build src.dgi from the source with DGIndexNV (-h, no UI).",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell: `function Invoke-DgIndex {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP dgindex 0"
    $exe = Resolve-Tool $Job.BinDir 'DGIndexNV.exe'
    $src = Find-SourceFile $Job.EpisodeDir
    $dgi = Join-Path $Job.EpisodeDir 'src.dgi'
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
}`,
	}
}

func audioTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "audio",
		Label:       "Audio (eac3to → Opus)",
		Description: "Demux one track to 16-bit WAV with eac3to, encode Opus with opusenc.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "track", Label: "Track index", Placeholder: "2"},
			{Key: "bitrate", Label: "Opus bitrate (kbps)", Placeholder: "320"},
		},
		PowerShell: `function Invoke-AudioExtract {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP audio 0"
    $track = if ($Params.track) { $Params.track } else { '2' }
    $bitrate = if ($Params.bitrate) { $Params.bitrate } else { '320' }
    $eac3to = Resolve-Tool $Job.BinDir 'eac3to.exe'
    $opusenc = Resolve-Tool $Job.BinDir 'opusenc.exe'
    $src = Find-SourceFile $Job.EpisodeDir
    $wav = $Job.AudioFile
    $opus = Join-Path $Job.EpisodeDir 'audio.opus'  # AudioOpusName contract

    if ((Test-Path -LiteralPath $opus) -and ((Get-Item -LiteralPath $opus).LastWriteTime -gt (Get-Item -LiteralPath $src).LastWriteTime)) {
        Write-Output "audio.opus already present and newer than source, reusing"
        Write-Output "ENCODE_STEP audio 100"
        return
    }
    if (Test-Path -LiteralPath $opus) {
        Write-Output "audio.opus is stale (older than source), re-encoding"
        Remove-Item -LiteralPath $opus -Force
    }

    Write-Output "ENCODE_STEP audio 10"
    Invoke-Tool -ExePath $eac3to -Arguments @($src, "${track}:", $wav, '-down16') -Label 'eac3to'
    if (-not (Test-Path -LiteralPath $wav)) {
        throw "eac3to finished but $wav was not created (check track index $track)"
    }
    Write-Output "ENCODE_STEP audio 60"
    Invoke-Tool -ExePath $opusenc -Arguments @('--bitrate', $bitrate, $wav, $opus) -Label 'opusenc'
    if (-not (Test-Path -LiteralPath $opus)) {
        throw "opusenc finished but $opus was not created"
    }
    Remove-Item -LiteralPath $wav -Force
    Write-Output "removed intermediate WAV"
    Write-Output "ENCODE_STEP audio 100"
}`,
	}
}

func encodeTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "encode",
		Label:       "x265 encode",
		Description: "Encode the .avs/.vpy with the x265 fork. Tune the named fields, or set x265_args to override everything.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "preset", Label: "Preset", Default: "slow"},
			{Key: "crf", Label: "CRF (quality)", Type: "number", Default: "15"},
			{Key: "aq_mode", Label: "AQ mode", Type: "number", Default: "5"},
			{Key: "aq_strength", Label: "AQ strength", Type: "number", Default: "0.80"},
			{Key: "aq_strength_edge", Label: "AQ strength (edge)", Type: "number", Default: "0.90"},
			{Key: "psy_rd", Label: "Psy RD", Type: "number", Default: "2.0"},
			{Key: "psy_rdoq", Label: "Psy RDOQ", Type: "number", Default: "2.0"},
			{Key: "rd", Label: "RD level", Type: "number", Default: "6"},
			{Key: "ctu", Label: "CTU size", Type: "number", Default: "32"},
			{Key: "no_sao", Label: "Disable SAO", Type: "bool", Default: "true"},
			{Key: "b_pyramid", Label: "B-pyramid", Type: "bool", Default: "true"},
			{Key: "open_gop", Label: "Open GOP", Type: "bool", Default: "true"},
			{Key: "x265_args", Label: "Raw x265 arguments (overrides everything above; blank = use the fields)"},
		},
		PowerShell: `function Invoke-VideoEncode {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP encode 0"
    $x265 = Resolve-Tool $Job.BinDir 'x265_x64.exe'
    if (-not (Test-Path -LiteralPath $Job.ScriptFile)) {
        throw "filter script not found: $($Job.ScriptFile)"
    }
    # Two modes: (a) x265_args set -> use that raw argument string verbatim
    # (power users, legacy flows). (b) empty -> assemble from the named
    # fields, each falling back to its documented default when blank.
    $argList = @()
    $raw = if ($Params.x265_args) { $Params.x265_args } else { '' }
    if ($raw.Trim() -ne '') {
        $argList = @([regex]::Matches($raw, '("[^"]*"|\S+)') | ForEach-Object { $_.Value.Trim('"') })
    } else {
        $preset = if ($Params.preset) { $Params.preset } else { 'slow' }
        $crf = if ($Params.crf) { $Params.crf } else { '15' }
        $aqMode = if ($Params.aq_mode) { $Params.aq_mode } else { '5' }
        $aqStrength = if ($Params.aq_strength) { $Params.aq_strength } else { '0.80' }
        $aqStrengthEdge = if ($Params.aq_strength_edge) { $Params.aq_strength_edge } else { '0.90' }
        $psyRd = if ($Params.psy_rd) { $Params.psy_rd } else { '2.0' }
        $psyRdoq = if ($Params.psy_rdoq) { $Params.psy_rdoq } else { '2.0' }
        $rd = if ($Params.rd) { $Params.rd } else { '6' }
        $ctu = if ($Params.ctu) { $Params.ctu } else { '32' }
        # Full legacy argument set (JPSDR/Patman fork flags included); the
        # structured fields substitute their values, everything else is the
        # proven baseline verbatim.
        $argList = @(
            '--frame-threads', '1', '--lookahead-slices', '8',
            '--input-depth', '10', '--output-depth', '10',
            '--videoformat', 'ntsc', '--range', 'limited',
            '--colorprim', 'bt709', '--transfer', 'bt709', '--colormatrix', 'bt709',
            '--allow-non-conformance',
            '--preset', $preset, '--crf', $crf,
            '--deblock=-2:-2', '--min-keyint', '23', '--keyint', '240',
            '--ref', '6', '--bframes', '12', '--b-adapt', '2', '--b-intra', '--fades',
            '--aq-mode', $aqMode, '--aq-strength', $aqStrength,
            '--aq-strength-edge', $aqStrengthEdge,
            '--aq-bias-strength', '0.95', '--aq-bias-strength-edge', '1.00',
            '--subme', '7', '--me', 'star', '--merange', '24',
            '--qcomp', '0.82', '--rc-lookahead', '160',
            '--rd', $rd, '--rdoq-level', '2', '--psy-rd', $psyRd, '--psy-rdoq', $psyRdoq,
            '--cbqpoffs', '-2', '--crqpoffs', '-2', '--qpstep', '2',
            '--ctu', $ctu, '--max-tu-size', '16',
            '--tu-intra-depth', '2', '--tu-inter-depth', '2',
            '--rect', '--amp', '--weightb', '--tskip', '--rskip', '0',
            '--no-strong-intra-smoothing'
        )
        if ($Params.no_sao -eq 'true') { $argList += @('--no-sao', '--no-sao-non-deblock') }
        if ($Params.b_pyramid -eq 'true') { $argList += '--b-pyramid' }
        if ($Params.open_gop -eq 'true') { $argList += '--open-gop' }
    }
    # Input/output always appended last so flow params can never override them.
    $argList += @('--input', $Job.ScriptFile, '-o', $Job.HevcFile)
    Write-Output "[x265] args: $($argList -join ' ')"
    Invoke-Tool -ExePath $x265 -Arguments $argList -Label 'x265'
    if (-not (Test-Path -LiteralPath $Job.HevcFile)) {
        throw "x265 finished but $($Job.HevcFile) was not created"
    }
    Write-Output "ENCODE_STEP encode 100"
}`,
	}
}

func muxTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "mux",
		Label:       "MKV mux",
		Description: "mkvmerge video + Opus audio with the standard track flags.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell: `function Invoke-Mux {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP mux 0"
    $mkvmerge = Resolve-Tool $Job.BinDir 'mkvmerge.exe'
    $hevc = $Job.HevcFile
    $audio = Join-Path $Job.EpisodeDir 'audio.opus'  # AudioOpusName contract
    $out = Join-Path $Job.EpisodeDir $Job.OutputName
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
}`,
	}
}

func releaseCopyTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "release_copy",
		Label:       "Release copy",
		Description: "Copy the finished MKV into [Group] Series - Raws [Tag]/ with size check.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell: `function Invoke-ReleaseCopy {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP release_copy 0"
    Assert-SafeName -Value $Job.ReleaseFolder -What "release folder"
    Assert-SafeName -Value $Job.OutputName -What "output name"
    $src = Join-Path $Job.EpisodeDir $Job.OutputName
    if (-not (Test-Path -LiteralPath $src)) { throw "finished MKV not found: $src" }
    $destDir = Join-Path $Job.ReleaseDir $Job.ReleaseFolder
    if (-not (Test-Path -LiteralPath $destDir)) {
        New-Item -ItemType Directory -Path $destDir -Force | Out-Null
        Write-Output "created release folder: $destDir"
    }
    $dest = Join-Path $destDir $Job.OutputName
    $existed = Test-Path -LiteralPath $dest
    Copy-Item -LiteralPath $src -Destination $dest -Force
    if ($existed) {
        Write-Output "OVERWROTE existing release: $dest"
    } else {
        Write-Output "copied to $dest"
    }
    if ((Get-Item -LiteralPath $src).Length -ne (Get-Item -LiteralPath $dest).Length) {
        throw "release copy size mismatch: $dest"
    }
    Write-Output "ENCODE_STEP release_copy 100"
}`,
	}
}

func keyframesTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "keyframes",
		Label:       "Keyframes",
		Description: "ffmpeg → temp y4m → SCXvid scene-change keyframes file.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell: `function Invoke-Keyframes {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP keyframes 0"
    Assert-SafeName -Value $Job.Series -What "series"
    Assert-SafeName -Value $Job.Episode -What "episode"
    $ffmpeg = Resolve-Tool $Job.BinDir 'ffmpeg.exe'
    $scxvid = Resolve-Tool $Job.BinDir 'SCXvid.exe'
    $destDir = Join-Path $Job.ReleaseDir $Job.ReleaseFolder
    $mkv = Join-Path $destDir $Job.OutputName
    $kf = Join-Path $destDir "$($Job.Series) - $($Job.Episode) Keyframes.txt"
    if (-not (Test-Path -LiteralPath $mkv)) { throw "release MKV not found: $mkv" }
    if ((Test-Path -LiteralPath $kf) -and ((Get-Item -LiteralPath $kf).LastWriteTime -gt (Get-Item -LiteralPath $mkv).LastWriteTime)) {
        Write-Output "keyframes file exists and is newer than the MKV, skipping"
        Write-Output "ENCODE_STEP keyframes 100"
        return
    }
    if (Test-Path -LiteralPath $kf) {
        Write-Output "keyframes file is stale (older than MKV), regenerating"
        Remove-Item -LiteralPath $kf -Force
    }

    Write-Output "[keyframes] ffmpeg -> y4m -> SCXvid"
    # SCXvid's documented contract (verified on a real node): the output log
    # file is its ONLY argument and the y4m arrives on STDIN. ffmpeg writes a
    # temp y4m, then a .NET process pipes it into SCXvid - cross-platform
    # (works on PS 5.1 nodes and pwsh alike, no cmd.exe dependency).
    $y4m = Join-Path ([System.IO.Path]::GetTempPath()) "encode-kf-$PID.y4m"
    try {
        Invoke-Tool -ExePath $ffmpeg -Arguments @(
            '-i', $mkv, '-f', 'yuv4mpegpipe', '-vf', 'scale=640:360',
            '-pix_fmt', 'yuv420p', '-vsync', 'drop', '-loglevel', 'quiet', $y4m
        ) -Label 'ffmpeg'

        $psi = New-Object System.Diagnostics.ProcessStartInfo
        $psi.FileName = $scxvid
        $psi.Arguments = '"' + $kf + '"'
        $psi.RedirectStandardInput = $true
        $psi.RedirectStandardOutput = $true
        $psi.RedirectStandardError = $true
        $psi.UseShellExecute = $false
        $proc = [System.Diagnostics.Process]::Start($psi)
        $outTask = $proc.StandardOutput.ReadToEndAsync()
        $errTask = $proc.StandardError.ReadToEndAsync()
        $src = [System.IO.File]::OpenRead($y4m)
        try { $src.CopyTo($proc.StandardInput.BaseStream) } finally { $src.Close() }
        $proc.StandardInput.Close()
        $proc.WaitForExit()
        foreach ($line in (($outTask.Result + [Environment]::NewLine + $errTask.Result) -split [Environment]::NewLine)) {
            if ($line.Trim()) { Write-Output "[SCXvid] $($line.Trim())" }
        }
        if ($proc.ExitCode -ne 0) { throw "SCXvid exited with code $($proc.ExitCode)" }
    } finally {
        try { if (Test-Path -LiteralPath $y4m) { Remove-Item -LiteralPath $y4m -Force } } catch { }
    }
    if (-not (Test-Path -LiteralPath $kf)) { throw "SCXvid finished but $kf was not created" }
    Write-Output "ENCODE_STEP keyframes 100"
}`,
	}
}
