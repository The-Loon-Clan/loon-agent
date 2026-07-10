# package.ps1 — bundle the user-facing files into a single zip for
# release. Output goes to dist/indexer-agent-<version>.zip and
# always-updated dist/indexer-agent-latest.zip.
#
# Usage:
#   .\package.ps1                    # auto-detect version from git tag
#   .\package.ps1 -Version 1.2.3     # explicit version
#
# What ends up in the zip:
#   docker-compose.yml   - the compose file users actually run
#   .env.example         - template for users to copy → .env
#   README.md            - setup + troubleshooting guide (from dist/)
#
# What's NOT in the zip:
#   - Source code (Go files, Dockerfile)
#   - The image itself (users docker-compose pull it from Docker Hub)
#   - Vendor / build artifacts
#
# This is the "everything an end user needs, nothing they don't" bundle.

[CmdletBinding()]
param(
    [string]$Version = ""
)

$ErrorActionPreference = "Stop"
Set-Location $PSScriptRoot

# Resolve a version string. Order:
#   1. -Version flag if provided
#   2. git describe (uses the most recent tag, falls back to short sha)
#   3. literal "dev" if git isn't available
if (-not $Version) {
    try {
        $Version = (git describe --tags --always 2>$null).Trim()
        if ([string]::IsNullOrEmpty($Version)) { throw }
    } catch {
        $Version = "dev"
    }
}

$DistDir = Join-Path $PSScriptRoot "dist"
$ZipName = "indexer-agent-$Version.zip"
$ZipPath = Join-Path $DistDir $ZipName
$LatestPath = Join-Path $DistDir "indexer-agent-latest.zip"

# Required source files. README lives in dist/ because it's only ever
# bundled — keeping it out of the repo root avoids confusion with
# any future top-level README for the agent source itself.
$Compose  = Join-Path $PSScriptRoot "docker-compose.yml"
$EnvFile  = Join-Path $PSScriptRoot ".env.example"
$Readme   = Join-Path $DistDir       "README.md"

foreach ($f in @($Compose, $EnvFile, $Readme)) {
    if (-not (Test-Path $f)) {
        Write-Error "missing required file: $f"
        exit 1
    }
}

# Ensure dist exists.
New-Item -ItemType Directory -Force -Path $DistDir | Out-Null

# Stage in a temp folder so the zip has clean entries (no parent dirs)
# and so an existing dist/README.md doesn't get bundled with a "dist/"
# prefix.
$Stage = Join-Path $env:TEMP "indexer-agent-pkg-$Version"
if (Test-Path $Stage) { Remove-Item $Stage -Recurse -Force }
New-Item -ItemType Directory -Path $Stage | Out-Null

Copy-Item $Compose (Join-Path $Stage "docker-compose.yml")
Copy-Item $EnvFile (Join-Path $Stage ".env.example")
Copy-Item $Readme  (Join-Path $Stage "README.md")

# Drop a small VERSION file too so users can tell what they downloaded
# without unzipping the whole thing.
"$Version`n" | Set-Content -Path (Join-Path $Stage "VERSION") -NoNewline

if (Test-Path $ZipPath)    { Remove-Item $ZipPath    -Force }
if (Test-Path $LatestPath) { Remove-Item $LatestPath -Force }

Compress-Archive -Path (Join-Path $Stage "*") -DestinationPath $ZipPath -Force
Copy-Item $ZipPath $LatestPath -Force

Remove-Item $Stage -Recurse -Force

$Size = (Get-Item $ZipPath).Length
Write-Host ""
Write-Host "Packaged indexer-agent $Version" -ForegroundColor Green
Write-Host "  $ZipPath"
Write-Host "  $LatestPath"
Write-Host "  size: $([math]::Round($Size / 1KB, 1)) KB"
Write-Host ""
Write-Host "Contents:"
# Use the .NET ZIP API directly — the system `tar` on Windows is Windows-native
# and doesn't translate paths like Git-Bash's tar. The .NET API just works.
Add-Type -AssemblyName System.IO.Compression.FileSystem
$zip = [System.IO.Compression.ZipFile]::OpenRead($ZipPath)
foreach ($e in $zip.Entries) { Write-Host "  $($e.FullName)" }
$zip.Dispose()
