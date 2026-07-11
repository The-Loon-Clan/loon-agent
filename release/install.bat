@echo off
setlocal enabledelayedexpansion

echo === loon-agent installer ===
echo.

:: Check for Docker
where docker >nul 2>&1
if %errorlevel% neq 0 (
    echo Error: Docker is not installed.
    echo Install Docker Desktop from https://www.docker.com/products/docker-desktop/
    exit /b 1
)

docker compose version >nul 2>&1
if %errorlevel% neq 0 (
    echo Error: Docker Compose v2 is not available.
    echo It should be included with Docker Desktop.
    exit /b 1
)

:: Work from the script's directory
cd /d "%~dp0"

:: Create data directories
if not exist "data\agent" mkdir "data\agent"
if not exist "data\gluetun" mkdir "data\gluetun"
echo Data directory: %cd%\data

:: Create .env from example if it doesn't exist
if not exist .env (
    copy .env.example .env >nul
    echo.
    echo Created .env from .env.example.
    echo You MUST edit .env before starting — fill in your credentials:
    echo.
    echo   Required:
    echo     SITE_URL        — Your indexer site URL
    echo     AGENT_TOKEN     — Agent token from Account Settings
    echo     VPN_PROVIDER    — Your VPN provider ^(see gluetun docs^)
    echo     VPN_USER        — VPN username
    echo     VPN_PASS        — VPN password
    echo     NNTP_SERVER     — Usenet server ^(e.g. news.provider.com:563^)
    echo     NNTP_USER       — Usenet username
    echo     NNTP_PASS       — Usenet password
    echo.
    echo   Edit with:  notepad "%cd%\.env"
    echo.
    pause
) else (
    echo Existing .env found, keeping it.
)

:: Pull and start
echo.
echo Pulling images...
docker compose pull

echo.
echo Starting containers...
docker compose up -d

echo.
echo === Installation complete ===
echo.
echo Check logs:    docker compose logs -f agent
echo Stop:          docker compose down
echo Update:        docker compose pull ^&^& docker compose up -d
