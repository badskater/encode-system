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
		dgindexTemplate(),
		audioTemplate(),
		encodeTemplate(),
		muxTemplate(),
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
		Description: "Encode the .avs/.vpy with the x265 fork and the flow's argument set.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "x265_args", Label: "x265 arguments"},
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
    $argString = if ($Params.x265_args) { $Params.x265_args } else { $Job.DefaultX265Args }
    # Quote-aware argv split; input/output appended last so flow params can
    # never override them. Note: $args is a PS automatic variable - never use it.
    $x265ArgList = @([regex]::Matches($argString, '("([^"]*)"|\S+)') | ForEach-Object { $_.Value.Trim('"') })
    $x265ArgList += @('--input', $Job.ScriptFile, '-o', $Job.HevcFile)
    Invoke-Tool -ExePath $x265 -Arguments $x265ArgList -Label 'x265'
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
    # Local temp file (not the NFS release share); best-effort cleanup.
    $y4m = Join-Path ([System.IO.Path]::GetTempPath()) "encode-kf-$PID.y4m"
    try {
        Invoke-Tool -ExePath $ffmpeg -Arguments @(
            '-i', $mkv, '-f', 'yuv4mpegpipe', '-vf', 'scale=640:360',
            '-pix_fmt', 'yuv420p', '-vsync', 'drop', '-loglevel', 'quiet', $y4m
        ) -Label 'ffmpeg'
        Invoke-Tool -ExePath $scxvid -Arguments @($y4m, $kf) -Label 'SCXvid'
    } finally {
        try { if (Test-Path -LiteralPath $y4m) { Remove-Item -LiteralPath $y4m -Force } } catch { }
    }
    if (-not (Test-Path -LiteralPath $kf)) { throw "SCXvid finished but $kf was not created" }
    Write-Output "ENCODE_STEP keyframes 100"
}`,
	}
}
