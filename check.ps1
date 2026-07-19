<#
.SYNOPSIS
  Diagnose a running f2f node on Windows.

.DESCRIPTION
  Checks the whole chain bottom-up — adapter, routes, listeners, DNS, trust
  store, UI, portal — and times every API endpoint so a slow one stands out
  instead of just "the portal is slow".

  Read-only: nothing here changes system state.

.EXAMPLE
  .\check.ps1
  .\check.ps1 -Bind 127.0.0.1:2202
#>
[CmdletBinding()]
param(
    # Where the loopback UI listens (must match how f2f was started).
    [string]$Bind = "127.0.0.1:2202",
    # Camp zone label, e.g. "xyz". Auto-detected from /api/status when omitted.
    [string]$Zone = ""
)

$ErrorActionPreference = "Continue"
$ui = "http://$Bind"

function Section($t) { Write-Host "`n=== $t ===" -ForegroundColor Cyan }
function Ok($m)      { Write-Host "  [ok]   $m" -ForegroundColor Green }
function Bad($m)     { Write-Host "  [FAIL] $m" -ForegroundColor Red }
function Warn2($m)   { Write-Host "  [warn] $m" -ForegroundColor Yellow }
function Info($m)    { Write-Host "  $m" -ForegroundColor DarkGray }

# Times one HTTP request and prints status + duration. Slow endpoints are the
# whole point of this script, so anything over a second is called out.
function Probe($label, $url, [switch]$Insecure) {
    $args = @("-sS", "-o", "NUL", "-w", "%{http_code} %{time_total}", "--max-time", "30")
    if ($Insecure) { $args += "-k" }
    $args += $url
    $out = & curl.exe @args 2>&1
    if ($LASTEXITCODE -ne 0) { Bad "$label -> $out"; return }
    $parts = "$out".Trim() -split '\s+'
    $code = $parts[0]; $secs = [double]$parts[1]
    $msg = "{0,-22} {1}  {2:N2}s" -f $label, $code, $secs
    if ($code -ne "200")   { Bad  $msg }
    elseif ($secs -gt 1.0) { Warn2 "$msg  <-- slow" }
    else                   { Ok   $msg }
}

# --- privileges & files ------------------------------------------------------

Section "environment"
$id = [Security.Principal.WindowsIdentity]::GetCurrent()
if ((New-Object Security.Principal.WindowsPrincipal $id).IsInRole(
        [Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Ok "running elevated"
} else {
    Warn2 "not elevated - adapter/route/DNS checks may read as missing"
}
foreach ($f in @("wintun.dll", "f2f.exe")) {
    if (Test-Path (Join-Path $PSScriptRoot $f)) { Ok "$f present" } else { Bad "$f missing" }
}
$proc = Get-Process f2f -ErrorAction SilentlyContinue
if ($proc) { Ok "f2f.exe running (pid $($proc.Id))" } else { Bad "f2f.exe is not running" }

# --- tunnel ------------------------------------------------------------------

Section "tunnel"
$ifc = Get-NetAdapter -Name "f2f*" -ErrorAction SilentlyContinue
if ($ifc) {
    Ok "adapter $($ifc.Name) status=$($ifc.Status)"
    $addr = Get-NetIPAddress -InterfaceAlias $ifc.Name -AddressFamily IPv4 -ErrorAction SilentlyContinue
    if ($addr) { Ok "overlay IP $($addr.IPAddress)" } else { Bad "adapter has no IPv4 address" }
    $rt = Get-NetRoute -InterfaceAlias $ifc.Name -ErrorAction SilentlyContinue |
          Where-Object { $_.DestinationPrefix -like "100.*" }
    if ($rt) { $rt | ForEach-Object { Ok "route $($_.DestinationPrefix)" } }
    else     { Bad "no 100.64.0.0/10 route on the adapter" }
} else {
    Bad "no f2f adapter (tunnel down)"
}

# --- listeners ---------------------------------------------------------------

Section "listeners"
$want = @{ 53 = "DNS"; 80 = "proxy HTTP"; 443 = "proxy HTTPS"; 2202 = "UI"; 2203 = "bus QUIC"; 6881 = "torrent" }
$tcp = (Get-NetTCPConnection -State Listen -ErrorAction SilentlyContinue).LocalPort
$udp = (Get-NetUDPEndpoint -ErrorAction SilentlyContinue).LocalPort
foreach ($p in ($want.Keys | Sort-Object)) {
    if ($tcp -contains $p -or $udp -contains $p) { Ok "$p ($($want[$p]))" }
    else { Bad "$p ($($want[$p])) not listening" }
}

# --- UI + camp ---------------------------------------------------------------

Section "local UI"
Probe "GET /"          "$ui/"
Probe "GET /api/status" "$ui/api/status"

$status = $null
try { $status = Invoke-RestMethod -Uri "$ui/api/status" -TimeoutSec 15 } catch { Bad "cannot read /api/status: $_" }
if ($status) {
    if (-not $Zone) { $Zone = $status.camp_label }
    Info "camp=$($status.camp_label) running=$($status.running) local_ip=$($status.local_ip) reflex=$($status.camp_reflex)"
}

# --- API timings -------------------------------------------------------------
# The portal being "slow to start" is almost always one endpoint the SPA waits
# on, so time them all rather than guessing which.

Section "API timings"
foreach ($ep in @("/api/channels", "/api/profile", "/api/oidc", "/api/camp/peers",
                  "/api/shell/peers", "/api/vnc/peers")) {
    Probe "GET $ep" "$ui$ep"
}

# --- peers -------------------------------------------------------------------

Section "peers"
if ($status -and $status.peers) {
    foreach ($p in $status.peers) {
        if ($p.self) { Info "self  $($p.name) $($p.overlay_v4)"; continue }
        $line = "{0,-16} paired={1,-5} reachable={2,-5} online={3,-5} rtt={4} ip={5}" -f `
                $p.name, $p.paired, $p.reachable, $p.online, $p.last_rtt_ms, $p.overlay_v4
        if ($p.paired -and $p.last_rtt_ms) { Ok $line }
        elseif ($p.paired)                 { Warn2 "$line  <-- paired but no bus RTT" }
        else                               { Info "  $line" }
    }
} else { Warn2 "no peer list" }

# --- intercepts --------------------------------------------------------------
# An intercept only works if the OS actually sends that address into f2f. Ask
# Windows the same question the stack asks when it picks a route, per address.

Section "intercepts"
if ($status -and $status.intercepts -and $status.intercepts.Count -gt 0) {
    foreach ($ic in $status.intercepts) {
        Info "$($ic.spec) via $($ic.peer)"
        if (-not $ic.prefixes -or $ic.prefixes.Count -eq 0) {
            Bad "  no resolved prefixes (route install failed?)"
            continue
        }
        foreach ($pfx in $ic.prefixes) {
            $ip = ($pfx -split '/')[0]
            $rt = Find-NetRoute -RemoteIPAddress $ip -ErrorAction SilentlyContinue | Select-Object -First 1
            if (-not $rt)                          { Bad   "  $pfx -> no route" }
            elseif ($rt.InterfaceAlias -like "f2f*") { Ok  "  $pfx -> $($rt.InterfaceAlias)" }
            else                                   { Bad   "  $pfx -> $($rt.InterfaceAlias)  <-- BYPASSES the tunnel" }
        }
        # What the name resolves to right now may differ from what we routed:
        # a CDN hands out different addresses over time, and anything not in the
        # list above leaves through the normal interface.
        $live = Resolve-DnsName $ic.spec -Type A -ErrorAction SilentlyContinue |
                Where-Object { $_.IPAddress } | Select-Object -Expand IPAddress
        foreach ($a in $live) {
            $routed = $ic.prefixes | Where-Object { $_ -like "$a/*" }
            if ($routed) { Ok "  resolves to $a (routed)" }
            else         { Bad "  resolves to $a  <-- NOT in the routed set" }
        }
    }
} else {
    Info "no intercepts configured"
}

# --- IPv6 leak ---------------------------------------------------------------
# The intercept routes IPv4. If the host also has working IPv6 and the target
# publishes AAAA records, the browser prefers v6 and never touches the tunnel —
# which looks exactly like "the intercept does nothing".

Section "IPv6 leak"
$v6 = Get-NetIPAddress -AddressFamily IPv6 -ErrorAction SilentlyContinue |
      Where-Object { $_.IPAddress -notlike "fe80*" -and $_.IPAddress -ne "::1" -and
                     $_.InterfaceAlias -notlike "f2f*" }
if ($v6) {
    Warn2 "host has global IPv6: $(($v6.IPAddress | Select-Object -First 3) -join ', ')"
    if ($status -and $status.intercepts) {
        foreach ($ic in $status.intercepts) {
            $aaaa = Resolve-DnsName $ic.spec -Type AAAA -ErrorAction SilentlyContinue |
                    Where-Object { $_.IPAddress }
            if ($aaaa) {
                Bad "  $($ic.spec) has AAAA -> browsers will prefer IPv6 and skip the tunnel"
                Info "  (f2f installs reject routes for these; if they failed above, that's the cause)"
            }
        }
    }
} else {
    Ok "no global IPv6 on this host - nothing to leak through"
}

# --- egress ------------------------------------------------------------------
# The end-to-end answer: what the outside world sees. Over IPv4 this should be
# the exit peer's address when an intercept is active.

Section "egress address"
$v4ip = & curl.exe -4 -sS --max-time 15 https://api.ipify.org 2>$null
if ($LASTEXITCODE -eq 0 -and $v4ip) { Info "public IPv4: $v4ip" } else { Warn2 "could not determine public IPv4" }
$v6ip = & curl.exe -6 -sS --max-time 10 https://api64.ipify.org 2>$null
if ($LASTEXITCODE -eq 0 -and $v6ip) { Warn2 "public IPv6: $v6ip  (traffic can leave this way)" }

# --- DNS ---------------------------------------------------------------------

Section "DNS"
if ($Zone) {
    $ns = ".$Zone.f2f"
    $rule = Get-DnsClientNrptRule -ErrorAction SilentlyContinue | Where-Object { $_.Namespace -eq $ns }
    if ($rule) { Ok "NRPT rule $ns -> $($rule.NameServers -join ',')" }
    else       { Bad "no NRPT rule for $ns" }

    # Ask our resolver directly first: separates "server is broken" from
    # "queries never reach it".
    $direct = Resolve-DnsName "portal.$Zone.f2f" -Server 127.0.0.1 -Type A -ErrorAction SilentlyContinue
    if ($direct) { Ok "resolver answers portal.$Zone.f2f -> $($direct.IPAddress -join ',')" }
    else         { Bad "our resolver on 127.0.0.1:53 did not answer" }

    $viaSystem = Resolve-DnsName "portal.$Zone.f2f" -Type A -ErrorAction SilentlyContinue
    if ($viaSystem) { Ok "system resolves portal.$Zone.f2f -> $($viaSystem.IPAddress -join ',')" }
    else            { Bad "system does NOT resolve portal.$Zone.f2f (NRPT not applied?)" }
} else {
    Warn2 "zone unknown - pass -Zone <label>"
}

# --- trust store -------------------------------------------------------------

Section "trust store"
$ca = Get-ChildItem Cert:\LocalMachine\Root -ErrorAction SilentlyContinue |
      Where-Object { $_.Subject -like "*f2f Local CA*" }
if ($ca) { $ca | ForEach-Object { Ok "trusted: $($_.Subject)" } }
else     { Bad "f2f CA not in LocalMachine\Root (HTTPS will warn)" }

# --- portal ------------------------------------------------------------------

Section "portal"
if ($Zone) {
    Probe "GET portal /"        "https://portal.$Zone.f2f/" -Insecure
    Probe "GET portal /api/status" "https://portal.$Zone.f2f/api/status" -Insecure
    Info "if these are fast but the browser is slow, the browser is the problem"
    Info "(check Secure DNS / DoH in chrome://settings/security - it bypasses NRPT)"
}

Write-Host ""
