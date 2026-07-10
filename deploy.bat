@echo off
REM Deploy the agent to a remote VPS.
REM Usage: deploy.bat [user@host] [remote_dir]
REM
REM The target host comes from the first argument or the DEPLOY_HOST
REM env var; the SSH port from DEPLOY_SSH_PORT (default 22). Nothing
REM operator-specific is committed here.
REM
REM Example:
REM   deploy.bat agent@my-server ~/indexer-agent

setlocal

set REMOTE_HOST=%1
if "%REMOTE_HOST%"=="" set REMOTE_HOST=%DEPLOY_HOST%
if "%REMOTE_HOST%"=="" (
    echo ERROR: no deploy target. Pass user@host as the first argument or set DEPLOY_HOST.
    exit /b 1
)

set REMOTE_DIR=%2
if "%REMOTE_DIR%"=="" set REMOTE_DIR=~/indexer-agent

set SSH_PORT=%DEPLOY_SSH_PORT%
if "%SSH_PORT%"=="" set SSH_PORT=22

echo === Deploying indexer-agent to %REMOTE_HOST%:%REMOTE_DIR% ===

echo Creating remote directory...
echo ssh -p %SSH_PORT% %REMOTE_HOST% "mkdir -p %REMOTE_DIR%"

echo Syncing source files via scp...
scp -P %SSH_PORT% -r ^
    client ^
    config ^
    services ^
    storage ^
    utils ^
    main.go ^
    go.mod ^
    go.sum ^
    Dockerfile ^
    docker-compose.yml ^
    Makefile ^
    .env.example ^
    .gitignore ^
    %REMOTE_HOST%:%REMOTE_DIR%/

if errorlevel 1 (
    echo ERROR: scp failed. Make sure SSH access works.
    exit /b 1
)

echo.
echo Building and starting on VPS...
echo ssh -p %SSH_PORT% %REMOTE_HOST% "cd %REMOTE_DIR% && if [ ! -f .env ]; then echo 'ERROR: No .env file. Copy .env.example to .env and fill in credentials.'; exit 1; fi && docker compose build --no-cache && docker compose up -d && echo '=== Deployment complete ===' && docker compose ps"

echo.
echo Done. View logs with:
echo ssh -p %SSH_PORT% %REMOTE_HOST% "cd %REMOTE_DIR% && docker compose logs -f agent"
