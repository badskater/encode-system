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
#   ENCODE_METRIC <key>=<value>     numeric quality/output metric (vmaf,
#                                   output_bitrate_kbps, duration_sec, ...)
#                                   stored on the job; last value per key wins
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

# Test-SameFile reports whether two EXISTING paths refer to the same file
# on disk: the same resolved path, symlinks pointing at one target, or
# hardlinks (distinct names, one file identity). Plain path-string equality
# misses both link kinds — that let quality_gate's self-compare guard be
# bypassed (a src.mkv symlink/hardlink to the output faked a perfect score).
function Test-SameFile {
    param(
        [Parameter(Mandatory)] [string] $PathA,
        [Parameter(Mandatory)] [string] $PathB
    )
    $a = Resolve-LinkFullPath $PathA
    $b = Resolve-LinkFullPath $PathB
    if ($a -eq $b) { return $true }
    # Different resolved names can still be hardlinks to ONE file: compare
    # the filesystem's file identity, not the names.
    if ([System.Environment]::OSVersion.Platform -eq [System.PlatformID]::Win32NT) {
        $ida = Get-FileIdentity $PathA
        $idb = Get-FileIdentity $PathB
        return ($null -ne $ida -and $null -ne $idb -and $ida -eq $idb)
    }
    # Non-Windows pwsh (Linux nodes / CI): coreutils stat exposes device +
    # inode. Without stat, fall back to resolved-path comparison only
    # (hardlinks stay undetectable — documented limitation, symlinks are
    # still caught above).
    # Get-Command can return MULTIPLE matches (e.g. /usr/bin/stat and
    # /bin/stat when /bin is a symlink) — take the first, or .Source
    # stringifies the whole array into an unrunnable command name.
    $stat = Get-Command stat -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($null -eq $stat) { return $false }
    $prevEAP = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $ida = & $stat.Source -c '%d:%i' -- $PathA 2>$null
        $codeA = $LASTEXITCODE
        $idb = & $stat.Source -c '%d:%i' -- $PathB 2>$null
        $codeB = $LASTEXITCODE
    } finally { $ErrorActionPreference = $prevEAP }
    if ($codeA -ne 0 -or $codeB -ne 0) { return $false }
    $ida = ([string]::Join('', @($ida | ForEach-Object { [string]$_ }))).Trim()
    $idb = ([string]::Join('', @($idb | ForEach-Object { [string]$_ }))).Trim()
    return ($ida -ne '' -and $ida -eq $idb)
}

# Resolve-LinkFullPath follows a symlink chain to the final target path.
# .NET 6+ (pwsh 7.2+) does it in one call; Windows PowerShell 5.1 falls
# back to Get-Item's reparse-point Target, walked with a bounded loop.
function Resolve-LinkFullPath {
    param([Parameter(Mandatory)] [string] $Path)
    try {
        $t = [System.IO.File]::ResolveLinkTarget($Path, $true)
        if ($null -ne $t) { return $t.FullName }
    } catch { }
    $item = Get-Item -LiteralPath $Path -Force
    for ($i = 0; $i -lt 16; $i++) {
        $tp = $item.PSObject.Properties['Target']
        if ($null -eq $tp -or $null -eq $tp.Value -or @($tp.Value).Count -eq 0) { break }
        $target = [string]@($tp.Value)[0]
        if ($target -eq '') { break }
        if (-not [System.IO.Path]::IsPathRooted($target)) {
            $target = Join-Path $item.DirectoryName $target
        }
        $item = Get-Item -LiteralPath $target -Force
    }
    return $item.FullName
}

# Get-FileIdentity returns the Windows file identity (volume serial number
# + 128-bit file index) via GetFileInformationByHandle — the identity NTFS
# hardlinks share. Returns $null when the file cannot be opened/queried.
function Get-FileIdentity {
    param([Parameter(Mandatory)] [string] $Path)
    if (-not ('EncodeLib.FileIdentity' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
namespace EncodeLib {
    public static class FileIdentity {
        [StructLayout(LayoutKind.Sequential)]
        private struct BY_HANDLE_FILE_INFORMATION {
            public uint FileAttributes;
            public System.Runtime.InteropServices.ComTypes.FILETIME CreationTime;
            public System.Runtime.InteropServices.ComTypes.FILETIME LastAccessTime;
            public System.Runtime.InteropServices.ComTypes.FILETIME LastWriteTime;
            public uint VolumeSerialNumber;
            public uint FileSizeHigh;
            public uint FileSizeLow;
            public uint NumberOfLinks;
            public uint FileIndexHigh;
            public uint FileIndexLow;
        }
        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        private static extern IntPtr CreateFileW(string path, uint access, uint share,
            IntPtr security, uint disposition, uint flags, IntPtr template);
        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool GetFileInformationByHandle(IntPtr h, out BY_HANDLE_FILE_INFORMATION info);
        [DllImport("kernel32.dll", SetLastError = true)]
        private static extern bool CloseHandle(IntPtr h);
        public static string Get(string path) {
            // FILE_READ_ATTRIBUTES=0x80; share READ|WRITE|DELETE=7;
            // OPEN_EXISTING=3; FILE_FLAG_BACKUP_SEMANTICS=0x02000000.
            IntPtr h = CreateFileW(path, 0x80, 7, IntPtr.Zero, 3, 0x02000000, IntPtr.Zero);
            if (h == new IntPtr(-1)) { return null; }
            try {
                BY_HANDLE_FILE_INFORMATION info;
                if (!GetFileInformationByHandle(h, out info)) { return null; }
                ulong index = ((ulong)info.FileIndexHigh << 32) | info.FileIndexLow;
                return info.VolumeSerialNumber.ToString("X8") + ":" + index.ToString("X16");
            } finally { CloseHandle(h); }
        }
    }
}
'@
    }
    return [EncodeLib.FileIdentity]::Get($Path)
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

# Write-EncodeMetric emits one ENCODE_METRIC key=value line for the agent to
# capture on the job record (quality/output stats: vmaf, bitrate, duration…).
# The value must be numeric; keys should be snake_case identifiers. Later
# calls with the same key override earlier ones (last wins), so a refined
# second-pass measurement safely supersedes a preview value.
function Write-EncodeMetric {
    param(
        [Parameter(Mandatory)] [string] $Key,
        [Parameter(Mandatory)] [double] $Value
    )
    if ($Key -notmatch '^[A-Za-z_][A-Za-z0-9_.]*$') {
        throw "invalid metric key '$Key' (letters, digits, _ and . only; must start with a letter or _)"
    }
    Write-Output "ENCODE_METRIC $Key=$Value"
}
