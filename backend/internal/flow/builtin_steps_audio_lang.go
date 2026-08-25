// Language-aware audio track selection.
//
// audio_lang picks the audio track by a language priority list (MediaInfo
// Language/String3 codes) instead of a hard-coded eac3to index, extracts it
// through the standard eac3to → WAV → opusenc path, and records the chosen
// language in audio.json so the mux step tags the track correctly.
package flow

import "github.com/badskater/encode-system/backend/internal/model"

func audioLangTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "audio_lang",
		Label:       "Audio (auto-select by language → Opus)",
		Description: "Pick the audio track by a MediaInfo language priority list (e.g. jpn,eng), demux it with eac3to and encode Opus. Writes audio.json so the mux step tags the track with the selected language.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "languages", Label: "Language priority (comma-separated ISO3)", Default: "jpn,eng"},
			{Key: "bitrate", Label: "Opus bitrate (kbps)", Type: "number", Default: "320"},
		},
		PowerShell: `function Invoke-AudioLang {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP audio_lang 0"
    $languages = if ($Params.languages) { $Params.languages } else { 'jpn,eng' }
    $bitrate = if ($Params.bitrate) { $Params.bitrate } else { '320' }
    $eac3to = Resolve-Tool $Job.BinDir 'eac3to.exe'
    $opusenc = Resolve-Tool $Job.BinDir 'opusenc.exe'
    $src = Find-SourceFile $Job.EpisodeDir
    $wav = $Job.AudioFile
    $opus = Join-Path $Job.EpisodeDir 'audio.opus'  # AudioOpusName contract
    $audioJson = Join-Path $Job.EpisodeDir 'audio.json'

    if ((Test-Path -LiteralPath $opus) -and ((Get-Item -LiteralPath $opus).LastWriteTime -gt (Get-Item -LiteralPath $src).LastWriteTime)) {
        Write-Output "audio.opus already present and newer than source, reusing"
        Write-Output "ENCODE_STEP audio_lang 100"
        return
    }
    if (Test-Path -LiteralPath $opus) { Remove-Item -LiteralPath $opus -Force }
    if (Test-Path -LiteralPath $audioJson) { Remove-Item -LiteralPath $audioJson -Force }

    # --- Select the track by language via MediaInfo -------------------------
    Write-Output "ENCODE_STEP audio_lang 10"
` + probeMediaInfoPS + `
    $tracks = @($info.media.track)
    $audios = @($tracks | Where-Object { $_.'@type' -eq 'Audio' })
    if ($audios.Count -eq 0) { throw "no audio tracks found in $src" }

    # Normalise a MediaInfo language value to a lowercase ISO3 code.
    # MediaInfo exposes Language/String3 (3-letter) and Language (full name);
    # prefer String3 and fall back to mapping common full names.
    function Get-TrackLang {
        param($a)
        if ($a.PSObject.Properties['Language/String3'] -and $a.'Language/String3') {
            return ([string]$a.'Language/String3').Trim().ToLower()
        }
        $name = ''
        if ($a.PSObject.Properties['Language']) { $name = ([string]$a.Language).Trim().ToLower() }
        switch ($name) {
            'japanese'  { return 'jpn' }
            'english'   { return 'eng' }
            'french'    { return 'fra' }
            'german'    { return 'deu' }
            'spanish'   { return 'spa' }
            'italian'   { return 'ita' }
            'portuguese'{ return 'por' }
            'russian'   { return 'rus' }
            'chinese'   { return 'zho' }
            'korean'    { return 'kor' }
            default     { return $name }
        }
    }

    $want = @($languages -split ',' | ForEach-Object { $_.Trim().ToLower() } | Where-Object { $_ -ne '' })
    $audioPos = -1
    $chosenLang = ''
    foreach ($lang in $want) {
        for ($i = 0; $i -lt $audios.Count; $i++) {
            $al = Get-TrackLang $audios[$i]
            if ($al -eq $lang) { $audioPos = $i; $chosenLang = $al; break }
        }
        if ($audioPos -ge 0) { break }
    }
    if ($audioPos -lt 0) {
        # No priority language matched: fall back to the first audio track so
        # the job still completes, but make the fallback loud in the log.
        $audioPos = 0
        $chosenLang = Get-TrackLang $audios[0]
        Write-Output "[audio_lang] WARNING: none of [$languages] matched; falling back to audio #1 ($chosenLang)"
    } else {
        Write-Output "[audio_lang] selected audio #$($audioPos + 1) (language '$chosenLang') from priority [$languages]"
    }
    # eac3to indexes tracks with the video first, so the Nth audio stream is
    # track (N + 1).
    $track = $audioPos + 2

    # --- Extract + encode (same path as the standard audio step) ------------
    Write-Output "ENCODE_STEP audio_lang 40"
    Invoke-Tool -ExePath $eac3to -Arguments @($src, "${track}:", $wav, '-down16') -Label 'eac3to'
    if (-not (Test-Path -LiteralPath $wav)) {
        throw "eac3to finished but $wav was not created (track $track, language '$chosenLang')"
    }
    Write-Output "ENCODE_STEP audio_lang 70"
    Invoke-Tool -ExePath $opusenc -Arguments @('--bitrate', $bitrate, $wav, $opus) -Label 'opusenc'
    if (-not (Test-Path -LiteralPath $opus)) {
        throw "opusenc finished but $opus was not created"
    }
    Remove-Item -LiteralPath $wav -Force
    Write-Output "removed intermediate WAV"

    # Record the selection for the mux step (track language tag).
    $sel = [pscustomobject]@{
        language = $chosenLang
        track    = $track
        codec    = 'opus'
        bitrate  = $bitrate
    }
    [System.IO.File]::WriteAllText($audioJson, ($sel | ConvertTo-Json -Compress))
    Write-Output "[audio_lang] wrote audio.json (language '$chosenLang')"
    Write-Output "ENCODE_STEP audio_lang 100"
}`,
	}
}
