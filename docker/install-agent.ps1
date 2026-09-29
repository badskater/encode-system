<#
.SYNOPSIS
  One-command encode-system node installer for Windows Server.

.DESCRIPTION
  Downloads encode-agent.exe from a GitHub release, installs it as a Windows
  service ("encode-agent"), and writes agent.json with either a one-shot
  pairing code (recommended) or a permanent node token. The controller pushes
  agent updates automatically after install.

  Run from an ELEVATED PowerShell on the node:

    irm https://github.com/badskater/encode-system/releases/latest/download/install-agent.ps1 | iex
    # or with arguments:
    .\install-agent.ps1 -ControllerUrl http://192.168.1.10:8080 -PairingCode ABC123

  Toolchain: encode tools (x265, mkvmerge, ffmpeg, eac3to, opusenc, ...) are
  expected in C:\bin — provision them separately (UI provisioning or the
  Ansible bin-tools play). This script only installs the agent itself.

.PARAMETER ControllerUrl
  Base URL of the controller, e.g. http://192.168.1.10:8080

.PARAMETER PairingCode
  One-shot pairing code from UI -> Nodes -> Issue pairing code (preferred).

.PARAMETER Token
  Permanent node token from UI -> Nodes -> register node (alternative).

.PARAMETER NodeName
  Node name to register as (default: hostname).

.PARAMETER Version
  Release tag to install (default: latest).

.PARAMETER Repo
  GitHub repo (default: badskater/encode-system). For private repos set
  $env:GITHUB_TOKEN to a PAT with repo read access before running.
#>
[CmdletBinding()]
param(
    [string]$ControllerUrl,
    [string]$PairingCode,
    [string]$Token,
    [string]$NodeName = $env:COMPUTERNAME,
    [string]$Version = "latest",
    [string]$Repo = "badskater/encode-system",
    [string]$InstallDir = "C:\encode-agent"
)

$ErrorActionPreference = "Stop"

# --- preflight ---------------------------------------------------------------
$isAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
           ).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
if (-not $isAdmin) { throw "Run from an ELEVATED PowerShell (service install requires admin)." }

if (-not $ControllerUrl) { $ControllerUrl = Read-Host "Controller URL (e.g. http://192.168.1.10:8080)" }
if (-not $PairingCode -and -not $Token) { $PairingCode = Read-Host "Pairing code (UI -> Nodes -> Issue pairing code)" }
if (-not $ControllerUrl) { throw "ControllerUrl is required." }
if (-not $PairingCode -and -not $Token) { throw "Either -PairingCode or -Token is required." }
$ControllerUrl = $ControllerUrl.TrimEnd('/')

$Headers = @{}
if ($env:GITHUB_TOKEN) { $Headers["Authorization"] = "Bearer $env:GITHUB_TOKEN" }

# --- resolve version ---------------------------------------------------------
if ($Version -eq "latest") {
    $rel = Invoke-RestMethod -Headers $Headers "https://api.github.com/repos/$Repo/releases/latest"
    $Version = $rel.tag_name
}
Write-Host "[install] version: $Version"

# --- download agent -----------------------------------------------------------
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$exe = Join-Path $InstallDir "encode-agent.exe"
$asset = "encode-agent.exe"
$url = "https://github.com/$Repo/releases/download/$Version/$asset"

# Stop the service before replacing the binary (upgrade case).
$svc = Get-Service encode-agent -ErrorAction SilentlyContinue
if ($svc) { Stop-Service encode-agent -Force; Start-Sleep 2 }

try {
    Invoke-WebRequest -Headers $Headers -Uri $url -OutFile "$exe.new" -UseBasicParsing
} catch {
    # Private repo: resolve the asset ID and download through the API.
    $relInfo = Invoke-RestMethod -Headers $Headers "https://api.github.com/repos/$Repo/releases/tags/$Version"
    $assetObj = $relInfo.assets | Where-Object { $_.name -eq $asset }
    if (-not $assetObj) { throw "asset $asset not found in release $Version" }
    Invoke-WebRequest -Headers ($Headers + @{Accept = "application/octet-stream"}) `
        -Uri $assetObj.url -OutFile "$exe.new" -UseBasicParsing
}
Move-Item -Force "$exe.new" $exe
Write-Host "[install] agent binary installed: $exe"

# --- agent.json ----------------------------------------------------------------
# Preserve an existing persisted token (node.token) across reinstalls: the
# agent prefers the persisted credential, so re-pairing is not needed.
$cfg = [ordered]@{
    controller_url    = $ControllerUrl
    node_name         = $NodeName
    data_dir          = $InstallDir
    lib_path          = (Join-Path $InstallDir "EncodeLib.ps1")
    heartbeat_seconds = 5
}
if ($PairingCode) { $cfg["pairing_code"] = $PairingCode }
if ($Token)       { $cfg["token"] = $Token }
$cfg | ConvertTo-Json | Set-Content -Path (Join-Path $InstallDir "agent.json") -Encoding UTF8
Write-Host "[install] wrote agent.json (controller=$ControllerUrl node=$NodeName)"

# --- service --------------------------------------------------------------------
$binPath = "`"$exe`" -config `"$InstallDir\agent.json`""
if ($svc) {
    # Upgrade: repoint the service in case paths changed.
    & sc.exe config encode-agent binPath= $binPath start= auto | Out-Null
} else {
    New-Service -Name encode-agent -DisplayName "encode-system agent" `
        -Description "Encode farm worker: heartbeats to the controller and runs encode jobs." `
        -BinaryPathName $binPath -StartupType Automatic | Out-Null
}
Start-Service encode-agent
Write-Host "[install] service encode-agent started"

# --- verify ---------------------------------------------------------------------
Start-Sleep 6
$proc = Get-Process encode-agent -ErrorAction SilentlyContinue
if (-not $proc) {
    Get-Content (Join-Path $InstallDir "agent.log") -Tail 10 -ErrorAction SilentlyContinue
    throw "agent process not running — check $InstallDir\agent.log"
}
try {
    $nodes = Invoke-RestMethod "$ControllerUrl/api/health" -TimeoutSec 10
    Write-Host "[install] controller reachable: $($nodes | ConvertTo-Json -Compress)"
} catch {
    Write-Warning "[install] controller not reachable from this node ($ControllerUrl) — agent will retry."
}
Write-Host "[install] done. The node appears in the UI within one heartbeat (~5s)."
Write-Host "[install] Next: provision C:\bin tools (UI Provision page or Ansible bin-tools)."
