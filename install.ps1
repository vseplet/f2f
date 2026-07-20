<#
.SYNOPSIS
  f2f installer for Windows.

  irm https://raw.githubusercontent.com/vseplet/f2f/main/install.ps1 | iex

  Downloads the release zip (f2f.exe + the wintun.dll driver it needs) from
  GitHub Releases, installs it to %LOCALAPPDATA%\f2f, and puts that on the user
  PATH. Override with -Version to pin a release.

.PARAMETER Version
  Release tag to install, e.g. v0.1.0. Default: the latest release.
#>
[CmdletBinding()]
param(
    [string]$Version = "latest"
)

$ErrorActionPreference = "Stop"
$repo    = "vseplet/f2f"
$asset   = "f2f-windows-amd64.zip"
$dest    = Join-Path $env:LOCALAPPDATA "f2f"

# curl.exe rather than Invoke-WebRequest: IWR stalls on hosts that advertise a
# broken AAAA, and GitHub redirects to a CDN we want to follow (-L) cleanly.
function Fetch($url, $out) {
    & curl.exe -4 -fSL --proto "=https" --connect-timeout 20 $url -o $out
    if ($LASTEXITCODE -ne 0) { throw "download failed: $url" }
}

if ($Version -eq "latest") {
    $url = "https://github.com/$repo/releases/latest/download/$asset"
} else {
    $url = "https://github.com/$repo/releases/download/$Version/$asset"
}

Write-Host "downloading $asset ($Version)..." -ForegroundColor Cyan
$tmpZip = Join-Path $env:TEMP $asset
Fetch $url $tmpZip

New-Item -ItemType Directory -Force -Path $dest | Out-Null
# Expand-Archive won't overwrite; clear the old copy first so upgrades work.
Get-ChildItem $dest -Filter "f2f.exe"    -ErrorAction SilentlyContinue | Remove-Item -Force
Get-ChildItem $dest -Filter "wintun.dll" -ErrorAction SilentlyContinue | Remove-Item -Force
Expand-Archive -LiteralPath $tmpZip -DestinationPath $dest -Force
Remove-Item -Force $tmpZip

$exe = Join-Path $dest "f2f.exe"
if (-not (Test-Path $exe)) { throw "f2f.exe missing after extract" }
Write-Host "installed: $exe" -ForegroundColor Green
& $exe version

# Add the install dir to the user PATH (persists; no admin needed) if absent.
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if (($userPath -split ';') -notcontains $dest) {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dest", "User")
    $env:Path += ";$dest"
    Write-Host "added $dest to your PATH (open a new terminal to pick it up)" -ForegroundColor DarkGray
}

Write-Host ""
Write-Host "f2f needs Administrator to create the tunnel adapter and routes." -ForegroundColor Yellow
Write-Host "run it from an elevated PowerShell:  f2f" -ForegroundColor Yellow
