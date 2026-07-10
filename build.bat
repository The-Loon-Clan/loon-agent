@echo off
cd /d "%~dp0"
echo Building Indexer for Windows...

echo go build -o indexer.exe ./cmd/agent

if %ERRORLEVEL% GEQ 1 (
    echo Build failed!
    exit /b %ERRORLEVEL%
)

echo Build successful! Executable is indexer.exe

echo Building Docker image...
docker build -t indexer .
if %ERRORLEVEL% GEQ 1 (
    echo Docker build failed!
    exit /b %ERRORLEVEL%
)
echo Docker build successful!

echo Starting Docker Compose...
docker stop vpn
docker rm vpn
docker stop indexer
docker rm indexer
docker-compose down
docker-compose up -d