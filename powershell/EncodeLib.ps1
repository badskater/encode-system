# EncodeLib.ps1 — shared helper library for encode-system job scripts.
#
# Pushed to nodes by the agent's auto-update system. Do NOT hand-edit copies
# on nodes. Job scripts dot-source this file for the helpers below; the actual
# pipeline step functions travel WITH each rendered job (each flow step owns
# its own PowerShell, linked at render time).
#
# Progress protocol (parsed by the agent):
#   ENCODE_STEP <name> <pct>        progress update (0-100)
#   ENCODE_STEP_FAILED <name> <msg>
#   ENCODE_JOB_DONE
#
# Step function convention (see Docs/md/Architecture.md):
#   function Invoke-Whatever {
#       param(
#           [Parameter(Mandatory=$true)] [pscustomobject] $Job,
#           [pscustomobject] $Params
#       )
#       ...
#   }
# $Job fields: Series, Episode, EpisodeDir, ScriptFile, BinDir, ScriptsDir,
#   ReleaseDir, Group, Tag, OutputName, ReleaseFolder, HevcFile, AudioFile.
# $Params holds the step's flow-parameter values.

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# Resolve-Tool returns the full path to a tool in the bin dir, failing fast
# when the binary is missing so jobs fail with a clear message instead of a
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
        # Windows PowerShell 5.1 wraps native stderr lines as ErrorRecords;
        # stringifying via .Exception.Message keeps the original text and
        # avoids 'RemoteException' noise in the job log.
        & $ExePath @Arguments 2>&1 | ForEach-Object {
            if ($_ -is [System.Management.Automation.ErrorRecord]) {
                Write-Output ("[$Label] $($_.Exception.Message)")
            } else {
                Write-Output ("[$Label] $_")
            }
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
