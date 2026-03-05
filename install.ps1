# OpenPoet installer for Windows
# Usage: irm https://raw.githubusercontent.com/openpoet/openpoet/main/install.ps1 | iex
#
# Uninstall:
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/openpoet/openpoet/main/install.ps1))) --uninstall
#
# Custom install directory:
#   $env:OPENPOET_INSTALL = "C:\custom\path"; irm https://raw.githubusercontent.com/openpoet/openpoet/main/install.ps1 | iex
#
# Specific version:
#   $env:OPENPOET_VERSION = "0.1.0"; irm https://raw.githubusercontent.com/openpoet/openpoet/main/install.ps1 | iex

param(
    [switch]$Uninstall
)

$ErrorActionPreference = "Stop"

$Repo = "openpoet/openpoet"
$InstallDir = if ($env:OPENPOET_INSTALL) { $env:OPENPOET_INSTALL } else { Join-Path $env:USERPROFILE ".local\bin" }
$Version = $env:OPENPOET_VERSION

function Write-Info($msg)  { Write-Host "[openpoet] $msg" -ForegroundColor Green }
function Write-Warn($msg)  { Write-Host "[openpoet] $msg" -ForegroundColor Yellow }
function Write-Err($msg)   { Write-Host "[openpoet] $msg" -ForegroundColor Red; exit 1 }

function Get-Arch {
    $arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture
    switch ($arch) {
        "X64"   { return "amd64" }
        "Arm64" { return "arm64" }
        default { Write-Err "Unsupported architecture: $arch" }
    }
}

function Get-LatestVersion {
    try {
        $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -UseBasicParsing
        return $release.tag_name -replace '^v', ''
    } catch {
        Write-Err "Could not fetch latest version from GitHub. Check your internet connection."
    }
}

function Test-ExistingInstall {
    $existing = Get-Command openpoet -ErrorAction SilentlyContinue
    if ($existing) {
        $existingPath = $existing.Source
        $existingVer = try { & openpoet version 2>$null } catch { "unknown" }
        Write-Warn "OpenPoet is already installed at $existingPath ($existingVer)."
        $reply = Read-Host "[openpoet] Proceed with reinstall/update? [y/N]"
        if ($reply -notmatch '^[yY]') {
            Write-Info "Aborted."
            exit 0
        }
    }
}

function Add-ToUserPath($dir) {
    $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if ($currentPath -split ";" | Where-Object { $_ -eq $dir }) {
        return  # already in PATH
    }

    $reply = Read-Host "[openpoet] $dir is not in your PATH. Add it? [Y/n]"
    if ($reply -match '^[nN]') {
        Write-Warn "Skipped. Add it manually to use 'openpoet' from any terminal:"
        Write-Warn "  [Environment]::SetEnvironmentVariable('Path', '$dir;' + [Environment]::GetEnvironmentVariable('Path', 'User'), 'User')"
        return
    }

    [Environment]::SetEnvironmentVariable("Path", "$dir;$currentPath", "User")
    $env:Path = "$dir;$env:Path"
    Write-Info "Added $dir to your user PATH."
    Write-Info "Restart your terminal for the change to take effect in new windows."
}

function Invoke-Uninstall {
    $binary = Join-Path $InstallDir "openpoet.exe"
    $dataDir = Join-Path $env:USERPROFILE ".openpoet"

    if (Test-Path $binary) {
        Remove-Item $binary -Force
        Write-Info "Removed $binary"
    } else {
        Write-Warn "Binary not found at $binary"
    }

    if (Test-Path $dataDir) {
        $reply = Read-Host "[openpoet] Remove data directory $dataDir? This deletes your database and settings. [y/N]"
        if ($reply -match '^[yY]') {
            Remove-Item $dataDir -Recurse -Force
            Write-Info "Removed $dataDir"
        } else {
            Write-Info "Kept $dataDir"
        }
    }

    # Remove from PATH
    $currentPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $newPath = ($currentPath -split ";" | Where-Object { $_ -ne $InstallDir }) -join ";"
    if ($newPath -ne $currentPath) {
        [Environment]::SetEnvironmentVariable("Path", $newPath, "User")
        Write-Info "Removed $InstallDir from user PATH."
    }

    Write-Info "OpenPoet has been uninstalled."
}

function Invoke-Install {
    Test-ExistingInstall

    $arch = Get-Arch

    if (-not $Version) {
        Write-Info "Fetching latest version..."
        $Version = Get-LatestVersion
    }

    if (-not $Version) {
        Write-Err "Could not determine latest version. Set `$env:OPENPOET_VERSION manually."
    }

    Write-Info "Installing openpoet v$Version (windows/$arch)..."

    $archive = "openpoet_${Version}_windows_${arch}.zip"
    $url = "https://github.com/$Repo/releases/download/v$Version/$archive"
    $checksumUrl = "https://github.com/$Repo/releases/download/v$Version/checksums.txt"

    # Download to temp directory
    $tmpDir = Join-Path ([System.IO.Path]::GetTempPath()) "openpoet-install-$(Get-Random)"
    New-Item -ItemType Directory -Path $tmpDir -Force | Out-Null

    try {
        $archivePath = Join-Path $tmpDir $archive

        Write-Info "Downloading $url..."
        try {
            Invoke-WebRequest -Uri $url -OutFile $archivePath -UseBasicParsing
        } catch {
            Write-Err "Download failed. Check that version v$Version exists at https://github.com/$Repo/releases"
        }

        # Verify checksum
        $checksumPath = Join-Path $tmpDir "checksums.txt"
        try {
            Invoke-WebRequest -Uri $checksumUrl -OutFile $checksumPath -UseBasicParsing
            $checksumLine = Get-Content $checksumPath | Where-Object { $_ -match $archive }
            if ($checksumLine) {
                $expected = ($checksumLine -split "\s+")[0]
                $actual = (Get-FileHash $archivePath -Algorithm SHA256).Hash.ToLower()
                if ($expected -ne $actual) {
                    Write-Err "Checksum mismatch! Expected: $expected, Got: $actual"
                }
                Write-Info "Checksum verified."
            }
        } catch {
            Write-Warn "Could not verify checksum (non-fatal)."
        }

        # Extract
        $extractDir = Join-Path $tmpDir "extracted"
        Expand-Archive -Path $archivePath -DestinationPath $extractDir -Force

        # Create install directory
        if (-not (Test-Path $InstallDir)) {
            New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
        }

        # Find and move binary
        $binary = Get-ChildItem -Path $extractDir -Filter "openpoet.exe" -Recurse | Select-Object -First 1
        if (-not $binary) {
            Write-Err "openpoet.exe not found in the downloaded archive."
        }

        $destBinary = Join-Path $InstallDir "openpoet.exe"
        Copy-Item $binary.FullName $destBinary -Force

        Write-Info "Installed openpoet to $destBinary"

        # Ensure install dir is in PATH
        Add-ToUserPath $InstallDir

        Write-Host ""
        Write-Info "Run 'openpoet' to start. Web UI: http://localhost:8080"
    } finally {
        # Cleanup temp directory
        Remove-Item $tmpDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}

# Entry point — handle both param and positional arg for --uninstall
if ($Uninstall -or ($args -contains "--uninstall")) {
    Invoke-Uninstall
} else {
    Invoke-Install
}
