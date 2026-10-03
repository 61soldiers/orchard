#Requires -Version 5.1
<#
    Interactive first-run setup for Orchard on Windows: starts the server, waits
    for the Apple Music daemon to install, then walks through signing in.

    The Windows counterpart of setup.sh. Needs Docker Desktop with the WSL2
    backend and Linux containers. Safe to re-run at any time.

    Run it from a PowerShell window in the orchard folder:

        powershell -ExecutionPolicy Bypass -File .\setup.ps1
#>

# v1.0 catches typo'd variables but still allows probing for absent JSON fields.
Set-StrictMode -Version 1.0
$ErrorActionPreference = 'Stop'

# Always operate from the repo root, wherever the script was invoked from.
Set-Location -LiteralPath $PSScriptRoot

$BaseUrl = if ($env:ORCHARD_URL) { $env:ORCHARD_URL } else { 'http://127.0.0.1:8080' }
$EnvFile = '.env'
$Key     = ''

# ------------------------------------------------------------------- output --
function Bold($m) { Write-Host $m -ForegroundColor White }
function Info($m) { Write-Host "  $m" }
function Ok($m)   { Write-Host "  " -NoNewline; Write-Host ([char]0x2713) -ForegroundColor Green -NoNewline; Write-Host " $m" }
function Warn($m) { Write-Host "  " -NoNewline; Write-Host "!" -ForegroundColor Yellow -NoNewline; Write-Host " $m" }
function Die($m) {
    Write-Host ''
    Write-Host "  " -NoNewline
    Write-Host ([char]0x2717 + " $m") -ForegroundColor Red
    Write-Host ''
    exit 1
}

# --------------------------------------------------------------- http calls --
function Test-Health {
    try {
        Invoke-WebRequest -Uri "$BaseUrl/healthz" -UseBasicParsing -TimeoutSec 5 | Out-Null
        return $true
    } catch {
        return $false
    }
}

# Returns the parsed JSON body (an object) or $null. A non-2xx response whose
# body is JSON is still parsed and returned, so the caller can read its "error".
function Invoke-Api {
    param(
        [Parameter(Mandatory)] [string] $Method,
        [Parameter(Mandatory)] [string] $Path,
        [object] $Body
    )
    $params = @{
        Method          = $Method
        Uri             = "$BaseUrl$Path"
        Headers         = @{ Authorization = "Bearer $Key" }
        TimeoutSec      = 180
        UseBasicParsing = $true
    }
    if ($PSBoundParameters.ContainsKey('Body') -and $null -ne $Body) {
        $params.Body        = ($Body | ConvertTo-Json -Compress)
        $params.ContentType = 'application/json'
    }
    try {
        return Invoke-RestMethod @params
    } catch {
        # PowerShell 7 keeps the error body here.
        if ($_.ErrorDetails -and $_.ErrorDetails.Message) {
            try { return ($_.ErrorDetails.Message | ConvertFrom-Json) } catch { }
        }
        # Windows PowerShell 5.1 keeps it on the response stream.
        $resp = $_.Exception.Response
        if ($resp -and $resp.PSObject.Methods['GetResponseStream']) {
            try {
                $reader = New-Object System.IO.StreamReader($resp.GetResponseStream())
                $text = $reader.ReadToEnd()
                if ($text) { return ($text | ConvertFrom-Json) }
            } catch { }
        }
        return $null
    }
}

function Field($obj, $name) {
    if ($null -ne $obj -and ($obj.PSObject.Properties.Name -contains $name)) { return $obj.$name }
    return $null
}

# Run docker, swallow its output, and hand back the exit code — without
# $ErrorActionPreference='Stop' turning a nonzero exit into a stack trace on
# PowerShell 7.4+ (where native errors honour that preference by default).
function Invoke-Docker {
    $global:LASTEXITCODE = 0
    try {
        $ErrorActionPreference = 'Continue'
        & docker @args 2>&1 | Out-Null
    } catch {
        return 1
    }
    return $LASTEXITCODE
}

# ---------------------------------------------------------------- preflight --
Write-Host ''
Bold 'Orchard setup'
Write-Host ''

if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    Die 'Docker Desktop is not installed. Get it from https://www.docker.com/products/docker-desktop/'
}

if ((Invoke-Docker compose version) -ne 0) {
    Die 'This Docker is too old — it has no "docker compose" command. Update Docker Desktop.'
}

if ((Invoke-Docker info) -ne 0) {
    Die 'Docker Desktop is not running. Start it, wait for the whale icon to go steady, then run this again.'
}

$osType = ''
try {
    $ErrorActionPreference = 'Continue'
    $osType = (& docker info --format '{{.OSType}}' 2>$null | Out-String).Trim()
} catch { }
$ErrorActionPreference = 'Stop'
if ($osType -and $osType -ne 'linux') {
    Die 'Docker Desktop is in Windows-containers mode. Right-click the Docker tray icon, choose "Switch to Linux containers...", then run this again.'
}

if ($env:PROCESSOR_ARCHITECTURE -notin @('AMD64', 'ARM64')) {
    Die "Orchard needs 64-bit Windows on an Intel/AMD or ARM processor. This one reports $($env:PROCESSOR_ARCHITECTURE)."
}

Ok 'System looks good'

# --------------------------------------------------------------- access key --
if (-not (Test-Path -LiteralPath $EnvFile)) {
    Copy-Item -LiteralPath '.env.example' -Destination $EnvFile
}

$lines   = @(Get-Content -LiteralPath $EnvFile)
$current = ''
foreach ($line in $lines) {
    if ($line -match '^ORCHARD_API_KEY=(.*)$') { $current = $Matches[1]; break }
}

if ($current.Length -lt 24) {
    $bytes = New-Object 'System.Byte[]' 48
    [System.Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
    $generated = ([Convert]::ToBase64String($bytes)) -replace '[=+/]', ''

    if ($lines -match '^ORCHARD_API_KEY=') {
        $lines = $lines | ForEach-Object {
            if ($_ -match '^ORCHARD_API_KEY=') { "ORCHARD_API_KEY=$generated" } else { $_ }
        }
    } else {
        $lines += "ORCHARD_API_KEY=$generated"
    }

    # UTF-8 without BOM, LF line endings — Compose reads .env literally and a BOM
    # or trailing CR would end up inside the key.
    $text = ($lines -join "`n") + "`n"
    [System.IO.File]::WriteAllText(
        (Join-Path (Get-Location) $EnvFile),
        $text,
        (New-Object System.Text.UTF8Encoding($false))
    )
    $Key = $generated
    Ok "Generated an access key and saved it to $EnvFile"
} else {
    $Key = $current
}

if (-not $Key) { Die "Could not read ORCHARD_API_KEY from $EnvFile" }

# ------------------------------------------------------------------- server --
Info 'Starting the server (the first run builds it, which can take a few minutes)...'
$global:LASTEXITCODE = 0
try {
    $ErrorActionPreference = 'Continue'
    & docker compose up -d
} catch { }
$ErrorActionPreference = 'Stop'
if ($LASTEXITCODE -ne 0) {
    Die 'Docker failed to start Orchard. Run "docker compose up" to see why.'
}

$up = $false
for ($i = 0; $i -lt 60; $i++) {
    if (Test-Health) { $up = $true; break }
    Start-Sleep -Seconds 2
}
if (-not $up) { Die 'The server did not come up. Run "docker compose logs" to see why.' }
Ok "Server is running at $BaseUrl"

# ---------------------------------------------------- apple music component --
Info 'Installing the Apple Music component (about 50 MB, one time only)...'
$status = $null
$wrapperState = $null
for ($i = 0; $i -lt 100; $i++) {
    $status = Invoke-Api GET /v1/apple/status
    $wrapperState = Field (Field $status 'wrapper') 'state'
    if ($wrapperState -eq 'ready')       { break }
    if ($wrapperState -eq 'unsupported') { Die "This machine's processor is not supported by the Apple Music component." }
    if ($wrapperState -eq 'failed')      { Die "Download failed: $(Field $status 'error'). Check the internet connection and run this again." }
    Start-Sleep -Seconds 3
}

if ($wrapperState -ne 'ready') {
    Die @'
The Apple Music component did not finish installing. Run "docker compose logs" to see why.
A common cause on Windows is user namespaces being unavailable in the WSL2 VM — make sure
Docker Desktop is using the WSL2 backend (Settings - General - "Use the WSL 2 based engine").
'@
}
Ok 'Apple Music component installed'

# -------------------------------------------------------------------- login --
if ((Field $status 'state') -eq 'ready') {
    Write-Host ''
    Ok 'Already signed in to Apple Music - nothing else to do.'
    Write-Host ''
    Bold 'Your access key'
    Info $Key
    Write-Host ''
    exit 0
}

Write-Host ''
Bold 'Sign in to Apple Music'
Info 'Use the Apple ID with your Apple Music subscription.'
Info 'Your password is sent to Apple and is never saved by Orchard.'
Write-Host ''

$AppleId = (Read-Host '  Apple ID (email)').Trim()
if (-not $AppleId)          { Die 'No Apple ID entered.' }
if ($AppleId.Contains(':')) { Die 'An Apple ID cannot contain a colon.' }

$secure = Read-Host '  Password (hidden)' -AsSecureString
$bstr   = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure)
try {
    $ApplePw = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)
} finally {
    [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr)
}
if (-not $ApplePw) { Die 'No password entered.' }

Write-Host ''
Info 'Signing in. If Apple asks for a code, it will appear on your devices now.'
$resp = Invoke-Api POST /v1/apple/login @{ appleId = $AppleId; password = $ApplePw }
$ApplePw = $null

switch (Field $resp 'state') {
    'ready' {
        Ok 'Signed in'
    }
    'awaiting_2fa' {
        Write-Host ''
        Warn 'Apple sent a verification code to your devices.'
        Warn 'You have 60 seconds to enter it.'
        Write-Host ''
        $code = (Read-Host '  Verification code').Trim()
        if (-not $code) { Die 'No code entered. Run .\setup.ps1 again to retry.' }
        $resp = Invoke-Api POST /v1/apple/2fa @{ code = $code }
        if ((Field $resp 'state') -ne 'ready') {
            Die "Sign-in failed: $(Field $resp 'error'). Run .\setup.ps1 again to retry."
        }
        Ok 'Signed in'
    }
    default {
        Die "Sign-in failed: $(Field $resp 'error'). Check the Apple ID and password, then run .\setup.ps1 again."
    }
}

# ------------------------------------------------------------------- finish --
Write-Host ''
Bold 'All set'
$storefront = Field $resp 'storefront'
if ($storefront) { Info "Apple Music store: $storefront" }
Write-Host ''
Info "Server address:  $BaseUrl"
Info "Access key:      $Key"
Write-Host ''
Info 'Keep the access key private - it grants access to your Apple Music account.'
Info "It is also saved in $EnvFile."
Write-Host ''
