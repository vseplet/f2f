<#
.SYNOPSIS
  Windows equivalent of `make dev`: fetch the Wintun driver, build the helper,
  run it elevated.

.DESCRIPTION
  Three things differ from the Unix path and are why this script exists:

  * Wintun. The tunnel adapter is driven by wintun.dll, which our dependency
    loads lazily from disk at runtime — it is NOT linked into the binary. The
    DLL has to sit next to the .exe, so we fetch it once from the official
    WireGuard site and cache it in the repo root.
  * Administrator. Creating a network adapter and writing routes needs it, so
    the script re-launches itself elevated instead of failing halfway through.
  * `go build`, not `go run`. go run drops the executable in a temp directory
    where wintun.dll would not be beside it.

.EXAMPLE
  .\dev.ps1
  .\dev.ps1 -LogLevel debug
  .\dev.ps1 -Bind 0.0.0.0:2202
#>
[CmdletBinding()]
param(
    # HTTP bind address for the loopback UI; empty = the helper's own default.
    [string]$Bind = "",
    # F2F_LOG level: info (default) or debug.
    [string]$LogLevel = "info",
    # Reuse an existing f2f.exe instead of rebuilding.
    [switch]$SkipBuild,
    # Run unelevated. The UI, config and local DNS come up, but creating the
    # wintun adapter and writing routes need Administrator — so there is no
    # tunnel, and with it no peers, mesh or calls. Useful when the UAC prompt
    # can't be answered (some remote-desktop tools don't show the secure
    # desktop) and you only need to exercise the UI or the build.
    [switch]$NoElevate,
    # Anything else is forwarded to the helper verbatim (e.g. `up`).
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$ExtraArgs
)

$ErrorActionPreference = "Stop"
Set-Location -LiteralPath $PSScriptRoot

# Wintun 0.14.1 is the current (and final) upstream release. www.wintun.net is
# the WireGuard project's own site; the DLL there is signed by WireGuard LLC.
$WintunVersion = "0.14.1"
$WintunUrl     = "https://www.wintun.net/builds/wintun-$WintunVersion.zip"
$WintunDll     = Join-Path $PSScriptRoot "wintun.dll"
$ExePath       = Join-Path $PSScriptRoot "f2f.exe"

function Test-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    (New-Object Security.Principal.WindowsPrincipal $id).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)
}

# Re-launch elevated, preserving the arguments we were given. -NoExit keeps the
# new window open so the helper's console output stays readable after it exits.
if (-not (Test-Admin) -and $NoElevate) {
    Write-Host "running WITHOUT Administrator: UI and config only." -ForegroundColor Yellow
    Write-Host "the tunnel will fail to start (wintun adapter + routes need elevation)." -ForegroundColor Yellow
} elseif (-not (Test-Admin)) {
    Write-Host "not elevated - relaunching as Administrator..." -ForegroundColor Yellow
    $argList = @("-NoExit", "-ExecutionPolicy", "Bypass", "-File", "`"$PSCommandPath`"")
    if ($Bind)      { $argList += @("-Bind", $Bind) }
    if ($LogLevel)  { $argList += @("-LogLevel", $LogLevel) }
    if ($SkipBuild) { $argList += "-SkipBuild" }
    if ($ExtraArgs) { $argList += $ExtraArgs }
    try {
        Start-Process -FilePath "powershell.exe" -Verb RunAs -ArgumentList $argList
    } catch {
        # Nothing was elevated. The usual cause on a remote session is that UAC
        # draws its prompt on the secure desktop, which some remote-desktop
        # tools don't render — so it is dismissed without ever being seen.
        Write-Host ""
        Write-Host "elevation was cancelled or never appeared." -ForegroundColor Red
        Write-Host "over a remote session UAC prompts on the secure desktop, which not every" -ForegroundColor DarkGray
        Write-Host "remote-desktop tool shows. Options: connect with mstsc (plain RDP), or run" -ForegroundColor DarkGray
        Write-Host "'.\dev.ps1 -NoElevate' for UI-only (no tunnel)." -ForegroundColor DarkGray
        exit 1
    }
    exit
}

# --- toolchain ---------------------------------------------------------------

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw "go not found in PATH. Install Go from https://go.dev/dl/ and reopen the shell."
}

# --- wintun.dll --------------------------------------------------------------

if ($NoElevate -and -not (Test-Path $WintunDll)) {
    # The DLL only matters when the tunnel starts, and unelevated it can't.
    # Skipping the download keeps UI-only runs working on a flaky connection.
    Write-Host "skipping wintun download (-NoElevate: no tunnel anyway)" -ForegroundColor DarkGray
} elseif (-not (Test-Path $WintunDll)) {
    # Map the process architecture onto the layout inside the zip.
    $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
        "AMD64" { "amd64" }
        "ARM64" { "arm64" }
        "x86"   { "x86" }
        default { throw "unsupported architecture: $env:PROCESSOR_ARCHITECTURE" }
    }

    Write-Host "fetching wintun $WintunVersion ($arch)..." -ForegroundColor Cyan
    $tmpZip = Join-Path $env:TEMP "wintun-$WintunVersion.zip"
    $tmpDir = Join-Path $env:TEMP "wintun-$WintunVersion"

    # curl.exe (shipped since Windows 10 1803) rather than Invoke-WebRequest:
    # wintun.net publishes an AAAA record, and IWR stalls until timeout on
    # networks that advertise IPv6 without working connectivity. -4 pins IPv4;
    # curl also ignores the system proxy settings IWR silently inherits.
    try {
        & curl.exe -4 -fsSL --connect-timeout 15 --max-time 120 $WintunUrl -o $tmpZip
        if ($LASTEXITCODE -ne 0) { throw "curl exited with $LASTEXITCODE" }
    } catch {
        Write-Host ""
        Write-Host "could not download wintun: $($_.Exception.Message)" -ForegroundColor Red
        Write-Host "fetch it by hand instead:" -ForegroundColor DarkGray
        Write-Host "  1. download $WintunUrl" -ForegroundColor DarkGray
        Write-Host "  2. extract wintun\bin\$arch\wintun.dll" -ForegroundColor DarkGray
        Write-Host "  3. drop it next to this script: $WintunDll" -ForegroundColor DarkGray
        Write-Host "(the same DLL ships inside the WireGuard for Windows installer)" -ForegroundColor DarkGray
        throw "wintun.dll missing"
    }

    if (Test-Path $tmpDir) { Remove-Item -Recurse -Force $tmpDir }
    Expand-Archive -LiteralPath $tmpZip -DestinationPath $tmpDir -Force

    $src = Join-Path $tmpDir "wintun\bin\$arch\wintun.dll"
    if (-not (Test-Path $src)) { throw "wintun.dll for $arch not found in the archive" }
    Copy-Item $src $WintunDll -Force

    Remove-Item -Force $tmpZip
    Remove-Item -Recurse -Force $tmpDir
    Write-Host "wintun.dll -> $WintunDll" -ForegroundColor Green
} else {
    Write-Host "wintun.dll already present" -ForegroundColor DarkGray
}

# --- build -------------------------------------------------------------------

if (-not $SkipBuild) {
    Write-Host "building f2f.exe..." -ForegroundColor Cyan
    & go build -o $ExePath ./source/helper
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}
if (-not (Test-Path $ExePath)) { throw "f2f.exe not found - run without -SkipBuild" }

# --- run ---------------------------------------------------------------------

# Serve the UI from the working tree so edits show up on reload, same as the
# Makefile's dev target.
$env:F2F_LOG        = $LogLevel
$env:F2F_DEV_ASSETS = Join-Path $PSScriptRoot "source\helper\ui\web\assets"

$runArgs = @("--console")
if ($Bind)      { $runArgs += @("--bind", $Bind) }
if ($ExtraArgs) { $runArgs += $ExtraArgs }

Write-Host "starting: f2f.exe $($runArgs -join ' ')" -ForegroundColor Cyan
& $ExePath @runArgs
