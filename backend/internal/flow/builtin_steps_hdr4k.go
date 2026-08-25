// HDR / Dolby-Vision detection and 4K encoding steps.
//
// hdr_probe writes a sidecar (hdr.json) describing the source's HDR
// characteristics; encode_4k reads it to emit the correct HEVC color
// signaling (bt2020 + PQ/HLG) instead of the legacy bt709 set.
package flow

import (
	"strings"

	"github.com/badskater/encode-system/backend/internal/model"
)

// resolveMediaInfoPS is shared PowerShell that locates MediaInfo.exe on the
// node (bin dir first, then the standard Program Files installs).
const resolveMediaInfoPS = `
    $mi = $null
    foreach ($candidate in @((Join-Path $Job.BinDir 'MediaInfo.exe'), 'C:\Program Files\MediaInfo\MediaInfo.exe', 'C:\Program Files (x86)\MediaInfo\MediaInfo.exe')) {
        if (Test-Path -LiteralPath $candidate) { $mi = $candidate; break }
    }
    if (-not $mi) { throw "MediaInfo.exe not found (install it or copy it to $($Job.BinDir))" }
`

// probeMediaInfoPS runs MediaInfo and parses its JSON output into $info.
// PS 5.1 pipes native output line by line, so the lines are joined before
// parsing (same technique as media_probe).
const probeMediaInfoPS = resolveMediaInfoPS + `
    $rawMi = & $mi --Output=JSON $src
    if ($LASTEXITCODE -ne 0) { throw "MediaInfo exited with code $LASTEXITCODE" }
    $info = ([string]::Join([char]10, $rawMi)) | ConvertFrom-Json
`

func hdrProbeTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "hdr_probe",
		Label:       "HDR/DoVi probe (MediaInfo)",
		Description: "Detect HDR10/HLG/Dolby Vision from MediaInfo and write hdr.json for the encode step. Place before encode/encode_4k; pure detection, no re-encoding.",
		Builtin:     true,
		Params:      []model.ParamDef{},
		PowerShell: `function Invoke-HdrProbe {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP hdr_probe 0"
    $src = Find-SourceFile $Job.EpisodeDir
    $out = Join-Path $Job.EpisodeDir 'hdr.json'
    if ((Test-Path -LiteralPath $out) -and ((Get-Item -LiteralPath $out).LastWriteTime -gt (Get-Item -LiteralPath $src).LastWriteTime)) {
        Write-Output "hdr.json already present and newer than source, reusing"
        Write-Output "ENCODE_STEP hdr_probe 100"
        return
    }
    if (Test-Path -LiteralPath $out) { Remove-Item -LiteralPath $out -Force }
` + probeMediaInfoPS + `
    $tracks = @($info.media.track)
    $video = $tracks | Where-Object { $_.'@type' -eq 'Video' } | Select-Object -First 1
    if (-not $video) { throw "no video track found in $src" }

    $transfer = ''
    if ($video.PSObject.Properties['transfer_characteristics']) { $transfer = [string]$video.transfer_characteristics }
    $primaries = ''
    if ($video.PSObject.Properties['colour_primaries']) { $primaries = [string]$video.colour_primaries }
    $maxCLL = ''
    if ($video.PSObject.Properties['MaxCLL']) { $maxCLL = [string]$video.MaxCLL }
    $maxFALL = ''
    if ($video.PSObject.Properties['MaxFALL']) { $maxFALL = [string]$video.MaxFALL }
    $masterDisplay = ''
    if ($video.PSObject.Properties['MasteringDisplay_Luminance']) { $masterDisplay = [string]$video.MasteringDisplay_Luminance }

    # Dolby Vision markers: MediaInfo reports a Dolby Video track and/or
    # video-level DV fields depending on the container and MediaInfo version.
    $dovi = $false
    foreach ($tk in $tracks) {
        if ([string]$tk.'@type' -eq 'Video') {
            foreach ($p in @('DolbyVision/String', 'DV/String', 'HDR_Format/String')) {
                if ($tk.PSObject.Properties[$p] -and [string]$tk.$p -match 'Dolby') { $dovi = $true }
            }
        }
        if ([string]$tk.'@type' -match '^(Dolby|Other)$' -and [string]$tk.Format -match 'Dolby Vision') { $dovi = $true }
    }

    $hdr = 'SDR'
    if ($transfer -match 'ST 2084|PQ|SMPTE 2084') { $hdr = 'HDR10' }
    elseif ($transfer -match 'HLG') { $hdr = 'HLG' }
    $width = 0
    $height = 0
    if ($video.Width) { $width = [int]$video.Width }
    if ($video.Height) { $height = [int]$video.Height }

    $data = [pscustomobject]@{
        hdr             = $hdr
        dovi            = $dovi
        transfer        = $transfer
        primaries       = $primaries
        max_cll         = $maxCLL
        max_fall        = $maxFALL
        master_display  = $masterDisplay
        width           = $width
        height          = $height
    }
    [System.IO.File]::WriteAllText($out, ($data | ConvertTo-Json -Compress))
    Write-Output "[hdr] $width x $height, transfer '$transfer', primaries '$primaries' -> $hdr (DoVi: $dovi)"
    if ($dovi) {
        Write-Output "[hdr] Dolby Vision detected. HDR10 signaling only; full DoVi RPU passthrough requires dovi_tool (future step)."
    }
    Write-Output "ENCODE_STEP hdr_probe 100"
}`,
	}
}

// encode4kArgBlock is the structured-argument assembly shared between the
// 1080p encode step and encode_4k. The defaults differ per template; the
// block itself is identical by design so both steps stay independently
// editable in the UI without one drifting out from under the other.
const encode4kArgBlock = `    $argList = @()
    $raw = if ($Params.x265_args) { $Params.x265_args } else { '' }
    if ($raw.Trim() -ne '') {
        $argList = @([regex]::Matches($raw, '("[^"]*"|\S+)') | ForEach-Object { $_.Value.Trim('"') })
    } else {
        $preset = if ($Params.preset) { $Params.preset } else { '%PRESET%' }
        $crf = if ($Params.crf) { $Params.crf } else { '%CRF%' }
        $aqMode = if ($Params.aq_mode) { $Params.aq_mode } else { '5' }
        $aqStrength = if ($Params.aq_strength) { $Params.aq_strength } else { '0.80' }
        $aqStrengthEdge = if ($Params.aq_strength_edge) { $Params.aq_strength_edge } else { '0.90' }
        $psyRd = if ($Params.psy_rd) { $Params.psy_rd } else { '2.0' }
        $psyRdoq = if ($Params.psy_rdoq) { $Params.psy_rdoq } else { '2.0' }
        $rd = if ($Params.rd) { $Params.rd } else { '6' }
        $ctu = if ($Params.ctu) { $Params.ctu } else { '%CTU%' }
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
        # Bools: blank means "use the declared default" — these three default
        # to true, so only an explicit 'false' turns them off.
        if ($Params.no_sao -ne 'false') { $argList += @('--no-sao', '--no-sao-non-deblock') }
        if ($Params.b_pyramid -ne 'false') { $argList += '--b-pyramid' }
        if ($Params.open_gop -ne 'false') { $argList += '--open-gop' }
        # HDR color signaling: hdr_probe writes hdr.json; when it reports
        # HDR10/HLG, the bt709 set is replaced with bt2020 + PQ/HLG so the
        # encode carries correct metadata. No sidecar -> legacy bt709.
        $hdrJson = Join-Path $Job.EpisodeDir 'hdr.json'
        if (Test-Path -LiteralPath $hdrJson) {
            $hdrInfo = [System.IO.File]::ReadAllText($hdrJson) | ConvertFrom-Json
            if ($hdrInfo.hdr -eq 'HDR10') {
                for ($i = 0; $i -lt $argList.Count; $i++) {
                    if ($argList[$i] -in @('--colorprim', '--colormatrix')) { $argList[$i + 1] = 'bt2020' }
                    if ($argList[$i] -eq '--transfer') { $argList[$i + 1] = 'smpte-st2084' }
                }
                Write-Output "[encode] hdr.json reports HDR10 -> bt2020/PQ signaling"
            } elseif ($hdrInfo.hdr -eq 'HLG') {
                for ($i = 0; $i -lt $argList.Count; $i++) {
                    if ($argList[$i] -in @('--colorprim', '--colormatrix')) { $argList[$i + 1] = 'bt2020' }
                    if ($argList[$i] -eq '--transfer') { $argList[$i + 1] = 'arib-std-b67' }
                }
                Write-Output "[encode] hdr.json reports HLG -> bt2020/HLG signaling"
            }
        }
    }
`

func encode4kArgAssembly(preset, crf, ctu string) string {
	s := encode4kArgBlock
	s = strings.ReplaceAll(s, "%PRESET%", preset)
	s = strings.ReplaceAll(s, "%CRF%", crf)
	s = strings.ReplaceAll(s, "%CTU%", ctu)
	return s
}

func encode4kTemplate() *model.StepTemplate {
	return &model.StepTemplate{
		Key:         "encode_4k",
		Label:       "x265 encode (4K)",
		Description: "Encode a 2160p .avs/.vpy with the x265 fork. Same structured fields as the 1080p step with 4K defaults (CTU 64). Reads hdr.json when present and switches to bt2020/PQ signaling for HDR10 sources.",
		Builtin:     true,
		Params: []model.ParamDef{
			{Key: "preset", Label: "Preset", Default: "slow"},
			{Key: "crf", Label: "CRF (quality)", Type: "number", Default: "16"},
			{Key: "aq_mode", Label: "AQ mode", Type: "number", Default: "5"},
			{Key: "aq_strength", Label: "AQ strength", Type: "number", Default: "0.80"},
			{Key: "aq_strength_edge", Label: "AQ strength (edge)", Type: "number", Default: "0.90"},
			{Key: "psy_rd", Label: "Psy RD", Type: "number", Default: "2.0"},
			{Key: "psy_rdoq", Label: "Psy RDOQ", Type: "number", Default: "2.0"},
			{Key: "rd", Label: "RD level", Type: "number", Default: "6"},
			{Key: "ctu", Label: "CTU size", Type: "number", Default: "64"},
			{Key: "no_sao", Label: "Disable SAO", Type: "bool", Default: "true"},
			{Key: "b_pyramid", Label: "B-pyramid", Type: "bool", Default: "true"},
			{Key: "open_gop", Label: "Open GOP", Type: "bool", Default: "true"},
			{Key: "x265_args", Label: "Raw x265 arguments (overrides everything above; blank = use the fields)"},
		},
		PowerShell: `function Invoke-VideoEncode4K {
    param(
        [Parameter(Mandatory=$true)] [pscustomobject] $Job,
        [pscustomobject] $Params
    )
    Write-Output "ENCODE_STEP encode_4k 0"
    $x265 = Resolve-Tool $Job.BinDir 'x265_x64.exe'
    if (-not (Test-Path -LiteralPath $Job.ScriptFile)) {
        throw "filter script not found: $($Job.ScriptFile)"
    }
` + encode4kArgAssembly("slow", "16", "64") + `    # Input/output always appended last so flow params can never override them.
    $argList += @('--input', $Job.ScriptFile, '-o', $Job.HevcFile)
    Write-Output "[x265] args: $($argList -join ' ')"
    Invoke-Tool -ExePath $x265 -Arguments $argList -Label 'x265'
    if (-not (Test-Path -LiteralPath $Job.HevcFile)) {
        throw "x265 finished but $($Job.HevcFile) was not created"
    }
    Write-Output "ENCODE_STEP encode_4k 100"
}`,
	}
}
