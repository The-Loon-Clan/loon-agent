@echo off
REM package.bat — bundle the user-facing files into a single zip for
REM release. cmd.exe equivalent of package.ps1 / package.sh.
REM
REM Usage:
REM   package.bat              auto-detect version from git tag
REM   package.bat 1.2.3        explicit version
REM
REM Output:
REM   dist\indexer-agent-<version>.zip
REM   dist\indexer-agent-latest.zip
REM
REM Requires: Windows 10+ (uses tar and powershell built-in).
REM This script exists because the .ps1 version trips on the default
REM RemoteSigned execution policy on most fresh Windows installs.

setlocal enabledelayedexpansion
cd /d "%~dp0"

REM ── Resolve version: arg 1 if provided, else `git describe`, else "dev".
set "VERSION=%~1"
if "%VERSION%"=="" (
    for /f "delims=" %%v in ('git describe --tags --always 2^>nul') do set "VERSION=%%v"
)
if "%VERSION%"=="" set "VERSION=dev"

set "DIST_DIR=dist"
set "ZIP_NAME=indexer-agent-%VERSION%.zip"
set "ZIP_PATH=%DIST_DIR%\%ZIP_NAME%"
set "LATEST_PATH=%DIST_DIR%\indexer-agent-latest.zip"

REM ── Required source files. README lives in dist\ because it's only
REM    bundled — keeps it out of the way of any future top-level README.
for %%f in ("docker-compose.yml" ".env.example" "dist\README.md") do (
    if not exist "%%~f" (
        echo missing required file: %%~f 1>&2
        exit /b 1
    )
)

if not exist "%DIST_DIR%" mkdir "%DIST_DIR%"

REM ── Stage in a temp folder so the zip has clean entries (no parent
REM    paths) and dist\README.md doesn't get bundled with a "dist\" prefix.
set "STAGE=%TEMP%\indexer-agent-pkg-%VERSION%"
if exist "%STAGE%" rmdir /s /q "%STAGE%"
mkdir "%STAGE%"

copy /y "docker-compose.yml" "%STAGE%\docker-compose.yml" >nul
copy /y ".env.example"       "%STAGE%\.env.example"       >nul
copy /y "dist\README.md"     "%STAGE%\README.md"          >nul

REM Drop a small VERSION file so users can tell what they downloaded
REM without unzipping the whole thing.
> "%STAGE%\VERSION" echo %VERSION%

if exist "%ZIP_PATH%"    del /q "%ZIP_PATH%"
if exist "%LATEST_PATH%" del /q "%LATEST_PATH%"

REM ── Build the zip with PowerShell's Compress-Archive. We invoke it
REM    as a single inline command (-Command) which bypasses the script
REM    execution policy that blocks .ps1 files on default Windows.
REM    This is the same trick the Contents listing uses below.
REM
REM    Why not Windows tar.exe? On Windows tar -a -c -f file.zip
REM    produces a tar-formatted file with a .zip extension, not a real
REM    pkzip archive. Compress-Archive produces a real one.
powershell -NoProfile -Command "Compress-Archive -Path '%STAGE%\*' -DestinationPath '%ZIP_PATH%' -Force"
if errorlevel 1 (
    echo zip creation failed 1>&2
    rmdir /s /q "%STAGE%"
    exit /b 1
)

copy /y "%ZIP_PATH%" "%LATEST_PATH%" >nul

rmdir /s /q "%STAGE%"

REM ── Print summary + contents using PowerShell's .NET ZIP API
REM    (works regardless of execution policy because we run inline).
echo.
echo Packaged indexer-agent %VERSION%
echo   %CD%\%ZIP_PATH%
echo   %CD%\%LATEST_PATH%
for %%I in ("%ZIP_PATH%") do echo   size: %%~zI bytes
echo.
echo Contents:
powershell -NoProfile -Command "Add-Type -AssemblyName System.IO.Compression.FileSystem; $z = [System.IO.Compression.ZipFile]::OpenRead('%CD%\%ZIP_PATH%'); foreach ($e in $z.Entries) { Write-Host ('  ' + $e.FullName) }; $z.Dispose()"

endlocal
