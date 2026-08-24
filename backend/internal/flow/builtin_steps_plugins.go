// Plugin-style builtin steps added for the FileFlows-inspired extension set:
// media_probe (MediaInfo diagnostics), audio_branch (lossy/lossless-aware
// Opus budgeting), crc32_rename (release checksum naming).
package flow

import "github.com/badskater/encode-system/backend/internal/model"

// mediainfoResolver is shared PowerShell that locates MediaInfo on the node:
// the bin dir first, then the standard Program Files installs.
const mediainfoResolverPS = `
    $mi = $null
    foreach ($candidate in @((Join-Path $Job.BinDir 'MediaInfo.exe'), 'C:\Program Files\MediaInfo\MediaInfo.exe', 'C:\Program Files (x86)\MediaInfo\MediaInfo.exe')) {
        if (Test-Path -LiteralPath $candidate) { $mi = $candidate; break }
    }
    if (-not $mi) { throw "MediaInfo.exe not found (install it or copy it to $($Job.BinDir))" }
`

func mediaProbeTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "media_probe",
		Label:       "Media probe (MediaInfo)",
		Description: "Report container, video codec/resolution and every audio track with its presumed eac3to track index. Pure diagnostics — place it early to make failures self-explanatory.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell: `function Invoke-MediaProbe {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP media_probe 0"
` + mediainfoResolverPS + `
    $src = Find-SourceFile $Job.EpisodeDir
    Write-Output "[probe] analyzing $src"
    $raw = & $mi --Output=JSON $src
    if ($LASTEXITCODE -ne 0) { throw "MediaInfo exited with code $LASTEXITCODE" }
    # Join before parsing: PS 5.1 pipes native output line-by-line and would
    # parse each line as separate JSON; joining works on 5.1 and pwsh alike.
    $info = ([string]::Join([char]10, $raw)) | ConvertFrom-Json
    $tracks = @($info.media.track)
    $general = $tracks | Where-Object { $_.'@type' -eq 'General' } | Select-Object -First 1
    $video = $tracks | Where-Object { $_.'@type' -eq 'Video' } | Select-Object -First 1
    $audios = @($tracks | Where-Object { $_.'@type' -eq 'Audio' })

    if ($general) {
        Write-Output "[probe] container: $($general.Format), duration: $($general.'Duration/String3')"
    }
    if ($video) {
        Write-Output "[probe] video: $($video.Format) $($video.Width)x$($video.Height), $($video.BitDepth)-bit, $($video.FrameRate) fps"
    } else {
        Write-Output "[probe] WARNING: no video track found"
    }
    if ($audios.Count -eq 0) {
        Write-Output "[probe] WARNING: no audio tracks found"
    }
    $i = 0
    foreach ($a in $audios) {
        $i++
        $br = ''
        if ($a.BitRate) { $br = ", $([math]::Round([double]$a.BitRate / 1000)) kbps" }
        $sr = if ($a.'SamplingRate/String') { $a.'SamplingRate/String' } else { $a.SamplingRate }
        # Presumed eac3to index: track 1 is the video, audios follow in file
        # order. Subtitle/chapter tracks on M2TS sources can shift this -
        # verify against the eac3to listing in the audio step's log.
        $eac = $i + 1
        Write-Output "[probe] AUDIO #$i -> eac3to track $eac : $($a.Format), $($a.'Channel(s)') ch, $sr$br"
    }
    if ($audios.Count -gt 0) {
        Write-Output "[probe] SUGGESTED audio step 'track' param: 2 (first audio track)"
    }
    Write-Output "ENCODE_STEP media_probe 100"
}`,
	}
}

func audioBranchTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "audio_branch",
		Label:       "Audio branch (lossy-aware Opus)",
		Description: "Demux one track (eac3to) and encode Opus with a bitrate chosen by the source codec: lossy sources get the compact budget, lossless (FLAC/PCM/TrueHD) get the full budget.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "track", Label: "eac3to track index", Type: "number", Default: "2"},
			{Key: "lossy_bitrate", Label: "Opus bitrate for lossy sources (kbps)", Type: "number", Default: "192"},
			{Key: "lossless_bitrate", Label: "Opus bitrate for lossless sources (kbps)", Type: "number", Default: "320"},
			{Key: "lossy_codecs", Label: "Codecs treated as lossy (comma-separated)", Default: "AC-3,EAC-3,DTS,AAC,MPEG Audio,Vorbis,Opus,MP3"},
			{Key: "threshold_kbps", Label: "Lossy sources at/above this kbps also get the lossless budget (0 = off)", Type: "number", Default: "0"},
		},
		PowerShell: `function Invoke-AudioBranch {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP audio_branch 0"
    $track = if ($Params.track) { $Params.track } else { '2' }
    $lossyBitrate = if ($Params.lossy_bitrate) { $Params.lossy_bitrate } else { '192' }
    $losslessBitrate = if ($Params.lossless_bitrate) { $Params.lossless_bitrate } else { '320' }
    $lossyCodecs = if ($Params.lossy_codecs) { $Params.lossy_codecs } else { 'AC-3,EAC-3,DTS,AAC,MPEG Audio,Vorbis,Opus,MP3' }
    $threshold = 0
    if ($Params.threshold_kbps) { $threshold = [int]$Params.threshold_kbps }

    $src = Find-SourceFile $Job.EpisodeDir
    $wav = $Job.AudioFile
    $opus = Join-Path $Job.EpisodeDir 'audio.opus'  # AudioOpusName contract

    if ((Test-Path -LiteralPath $opus) -and ((Get-Item -LiteralPath $opus).LastWriteTime -gt (Get-Item -LiteralPath $src).LastWriteTime)) {
        Write-Output "audio.opus already present and newer than source, reusing"
        Write-Output "ENCODE_STEP audio_branch 100"
        return
    }
    if (Test-Path -LiteralPath $opus) {
        Write-Output "audio.opus is stale (older than source), re-encoding"
        Remove-Item -LiteralPath $opus -Force
    }

    # Identify the selected track's codec via MediaInfo to pick the budget.
    $codec = ''
    $profile = ''
    $hiRes = $false
    $trackKbps = 0
    try {
` + mediainfoResolverPS + `
        $rawMi = & $mi --Output=JSON $src
        if ($LASTEXITCODE -ne 0) { throw "MediaInfo exited with code $LASTEXITCODE" }
        $info = ([string]::Join([char]10, $rawMi)) | ConvertFrom-Json
        $audios = @($info.media.track | Where-Object { $_.'@type' -eq 'Audio' })
        $audioPos = [int]$track - 2  # eac3to track 2 = first audio stream
        if ($audioPos -lt 0 -or $audioPos -ge $audios.Count) {
            throw "track $track does not map to an audio stream (found $($audios.Count) audio streams)"
        }
        $sel = $audios[$audioPos]
        $codec = [string]$sel.Format
        if ($sel.PSObject.Properties['Format_Profile']) { $profile = [string]$sel.Format_Profile }
        # DTS-HD Master Audio reports Format 'DTS' with profile 'MA / Core' -
        # the profile, not the codec family, decides lossy vs lossless here.
        # Set-StrictMode throws on missing properties, hence the guard above.
        $hiRes = ($profile -ne '') -and ($profile -match 'Master Audio|HRA|\bMA\b')
        if ($sel.BitRate) { $trackKbps = [math]::Round([double]$sel.BitRate / 1000) }
    } catch {
        Write-Output "[branch] MediaInfo unavailable/failed ($($_.Exception.Message)); falling back to the lossless budget"
    }

    $bitrate = $losslessBitrate
    $why = "lossless or unknown codec"
    if ($codec -ne '') {
        $isLossy = $false
        if (-not $hiRes) {
            foreach ($c in ($lossyCodecs -split ',')) {
                if ($c.Trim() -and ($codec -like "*$($c.Trim())*")) { $isLossy = $true; break }
            }
        } else {
            Write-Output "[branch] profile '$profile' marks this track lossless despite codec family"
        }
        if ($isLossy) {
            if ($threshold -gt 0 -and $trackKbps -ge $threshold) {
                $why = "lossy but high-bitrate ($trackKbps kbps >= $threshold)"
            } else {
                $bitrate = $lossyBitrate
                $why = "lossy codec ($trackKbps kbps)"
            }
        }
    }
    Write-Output "[branch] track $track : $codec -> Opus @ $bitrate kbps ($why)"

    $eac3to = Resolve-Tool $Job.BinDir 'eac3to.exe'
    $opusenc = Resolve-Tool $Job.BinDir 'opusenc.exe'
    Write-Output "ENCODE_STEP audio_branch 10"
    Invoke-Tool -ExePath $eac3to -Arguments @($src, "${track}:", $wav, '-down16') -Label 'eac3to'
    if (-not (Test-Path -LiteralPath $wav)) {
        throw "eac3to finished but $wav was not created (check track index $track)"
    }
    Write-Output "ENCODE_STEP audio_branch 60"
    Invoke-Tool -ExePath $opusenc -Arguments @('--bitrate', $bitrate, $wav, $opus) -Label 'opusenc'
    if (-not (Test-Path -LiteralPath $opus)) {
        throw "opusenc finished but $opus was not created"
    }
    Remove-Item -LiteralPath $wav -Force
    Write-Output "removed intermediate WAV"
    Write-Output "ENCODE_STEP audio_branch 100"
}`,
	}
}

func crc32RenameTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "crc32_rename",
		Label:       "CRC32 checksum rename",
		Description: "Compute the final MKV's CRC32 and append it to the name ([ABC1234D]), the standard release convention. Place AFTER mux, BEFORE release copy so downstream steps use the checksummed name.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "uppercase", Label: "Uppercase hex", Type: "bool", Default: "true"},
		},
		PowerShell: `function Invoke-Crc32Rename {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP crc32_rename 0"
    $uppercase = $true
    if ($Params.uppercase -eq 'false') { $uppercase = $false }
    $mkv = Join-Path $Job.EpisodeDir $Job.OutputName
    if (-not (Test-Path -LiteralPath $mkv)) { throw "finished MKV not found: $mkv (run after the mux step)" }

    # Idempotency: if the name already ends with an 8-hex checksum, replace it.
    $base = [System.IO.Path]::GetFileNameWithoutExtension($Job.OutputName)
    $ext = [System.IO.Path]::GetExtension($Job.OutputName)
    if ($base -match '\s\[[0-9A-Fa-f]{8}\]$') {
        $base = $base -replace '\s\[[0-9A-Fa-f]{8}\]$', ''
        Write-Output "existing checksum tag found, replacing"
    }

    # Streaming CRC32 (standard reflected polynomial, same as fansub tooling).
    # Compiled via Add-Type for native speed: a byte-wise PowerShell loop over
    # a multi-GB release would take tens of minutes, this runs in seconds.
    if (-not ('EncodeCrc32' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.IO;
public static class EncodeCrc32 {
    private static readonly uint[] Table = new uint[256];
    static EncodeCrc32() {
        for (uint i = 0; i < 256; i++) {
            uint c = i;
            for (int k = 0; k < 8; k++)
                c = (c & 1) != 0 ? 0xEDB88320u ^ (c >> 1) : c >> 1;
            Table[i] = c;
        }
    }
    public static uint Compute(string path) {
        uint crc = 0xFFFFFFFFu;
        using (var fs = File.OpenRead(path)) {
            byte[] buf = new byte[1 << 16];
            int n;
            while ((n = fs.Read(buf, 0, buf.Length)) > 0)
                for (int j = 0; j < n; j++)
                    crc = Table[(crc ^ buf[j]) & 0xFF] ^ (crc >> 8);
        }
        return crc ^ 0xFFFFFFFFu;
    }
}
'@
    }
    $hex = '{0:X8}' -f ([EncodeCrc32]::Compute($mkv))
    if (-not $uppercase) { $hex = $hex.ToLower() }

    $newName = "$base [$hex]$ext"
    Rename-Item -LiteralPath $mkv -NewName $newName
    # Propagate to the shared job context so release_copy / keyframes pick up
    # the checksummed name automatically.
    $Job.OutputName = $newName
    Write-Output "renamed -> $newName (CRC32 $hex)"
    Write-Output "ENCODE_STEP crc32_rename 100"
}`,
	}
}
