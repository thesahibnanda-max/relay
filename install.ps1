#Requires -Version 5.1
<#
Relay installer for native Windows.

    irm https://relay-sahib-nanda.vercel.app/install.ps1 | iex

Downloads the latest release build for this machine, checks its checksum, installs
it to %LOCALAPPDATA%\relay\bin and adds that folder to your user PATH.

Options (environment variables), e.g. `$env:RELAY_VERSION = "v1.2.3"` before running:
  RELAY_VERSION         install this version instead of the latest
  RELAY_INSTALL_DIR     install here instead of %LOCALAPPDATA%\relay\bin
  RELAY_NO_MODIFY_PATH  do not edit the user PATH (any value)
#>

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

function Say($msg) { Write-Host $msg }
function Die($msg) { Write-Host "relay install: $msg" -ForegroundColor Red; exit 1 }

$Repo = if ($env:RELAY_REPO) { $env:RELAY_REPO } else { "thesahibnanda-max/relay" }
$Base = if ($env:RELAY_DOWNLOAD_BASE) { $env:RELAY_DOWNLOAD_BASE } else { "https://github.com/$Repo/releases" }
$Version = if ($env:RELAY_VERSION) { $env:RELAY_VERSION } else { "latest" }
$Dir = if ($env:RELAY_INSTALL_DIR) { $env:RELAY_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "relay\bin" }

# --- which build does this machine need?
switch ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) {
    "X64"   { $arch = "amd64" }
    "Arm64" { $arch = "arm64" }
    default { Die "unsupported CPU: $([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture) (Relay supports amd64 and arm64)" }
}
$os = "windows"

# --- which version?
function Resolve-LatestTag([string]$Url) {
    $req = [System.Net.HttpWebRequest]::Create($Url)
    $req.Method = "HEAD"
    $req.AllowAutoRedirect = $false
    $req.UserAgent = "relay-install.ps1"
    try {
        $resp = $req.GetResponse()
    } catch [System.Net.WebException] {
        $resp = $_.Exception.Response
    }
    if (-not $resp) { Die "could not reach $Url (is the repository public and does it have a release?)" }
    $loc = $resp.Headers["Location"]
    $resp.Close()
    if (-not $loc) { Die "could not work out the latest version from $Url" }
    return $loc.Substring($loc.LastIndexOf("/") + 1)
}

if ($Version -eq "latest") {
    $tag = Resolve-LatestTag "$Base/latest"
    if ($tag -notmatch "^v[0-9]") { Die "could not work out the latest version (got '$tag')" }
} else {
    $tag = "v" + $Version.TrimStart("v")
}
$ver = $tag.TrimStart("v")
$archive = "relay_${ver}_${os}_${arch}.zip"

$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("relay-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Say "Installing relay $tag for $os/$arch"
    $archivePath = Join-Path $tmp $archive
    $checksumsPath = Join-Path $tmp "checksums.txt"
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$Base/download/$tag/$archive" -OutFile $archivePath
    } catch { Die "download failed: $Base/download/$tag/$archive" }
    try {
        Invoke-WebRequest -UseBasicParsing -Uri "$Base/download/$tag/checksums.txt" -OutFile $checksumsPath
    } catch { Die "could not download checksums.txt" }

    # --- verify the download
    $wantLine = Select-String -Path $checksumsPath -Pattern ([regex]::Escape($archive) + '$') | Select-Object -First 1
    if (-not $wantLine) { Die "$archive is not listed in checksums.txt" }
    $want = ($wantLine.Line -split '\s+')[0]
    # Not Get-FileHash: on a machine with PowerShell 7 installed alongside
    # Windows PowerShell 5.1, a stray Microsoft.PowerShell.Utility module
    # from pwsh can end up earlier on 5.1's $PSModulePath and shadow it,
    # breaking autoload (confirmed live) - .NET's own hasher has no module
    # resolution to get shadowed.
    $sha256 = [System.Security.Cryptography.SHA256]::Create()
    $stream = [System.IO.File]::OpenRead($archivePath)
    try {
        $hashBytes = $sha256.ComputeHash($stream)
    } finally {
        $stream.Close()
        $sha256.Dispose()
    }
    $got = [BitConverter]::ToString($hashBytes) -replace '-', ''
    if ($want.ToLower() -ne $got.ToLower()) { Die "checksum mismatch for $archive (expected $want, got $got); nothing was installed" }

    # --- install
    Expand-Archive -Path $archivePath -DestinationPath $tmp -Force
    $newExe = Join-Path $tmp "relay.exe"
    if (-not (Test-Path $newExe)) { Die "could not find relay.exe in $archive" }
    New-Item -ItemType Directory -Path $Dir -Force | Out-Null
    $target = Join-Path $Dir "relay.exe"

    # Safe even if relay is running right now: try moving the new binary into
    # place directly first (works unless something has it locked); if that's
    # blocked, ask the running daemon to stop and retry once. Unlike Unix,
    # Windows will not let a running .exe be replaced out from under itself.
    $moved = $false
    for ($i = 0; $i -lt 2 -and -not $moved; $i++) {
        try {
            Move-Item -Path $newExe -Destination $target -Force
            $moved = $true
        } catch {
            if ($i -eq 0 -and (Test-Path $target)) {
                Say "relay is running; asking it to stop before updating..."
                try { & $target daemon stop 2>$null | Out-Null } catch {}
                Start-Sleep -Milliseconds 500
            }
        }
    }
    if (-not $moved) { Die "could not replace $target - close any running relay processes (relay daemon stop) and try again" }
    Say "Installed: $target"
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# --- make sure `relay` is found in new terminals
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
$alreadyOnPath = ($userPath -split ";") -contains $Dir
if (-not $alreadyOnPath) {
    if ($env:RELAY_NO_MODIFY_PATH) {
        Say "Add $Dir to your PATH to run relay from anywhere."
    } else {
        $newUserPath = if ([string]::IsNullOrEmpty($userPath)) { $Dir } else { "$userPath;$Dir" }
        [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
        Say "Added $Dir to your PATH (user)"
    }
}
# Make it work in this same window without reopening it.
if (($env:Path -split ";") -notcontains $Dir) { $env:Path = "$env:Path;$Dir" }

& $target version
if (-not $alreadyOnPath -and -not $env:RELAY_NO_MODIFY_PATH) {
    Say "Open a new terminal for PATH changes to apply everywhere."
}
Say "Next: run  relay doctor  to check everything is in order."
