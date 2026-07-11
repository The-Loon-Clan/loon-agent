@echo off
REM Multi-arch publish to Docker Hub (linux/amd64 + linux/arm64).
REM
REM Replaces the previous single-arch `docker build && docker push`
REM so ARM users (Raspberry Pi 4/5, Apple Silicon servers, AWS
REM Graviton, etc.) don't have to build from source.
REM
REM Requires `docker buildx` which ships with Docker Desktop and
REM modern Docker Engine. The named builder is created on first
REM run and reused after that.

setlocal

set IMAGE=amenzb/loon-agent:latest

REM Create a buildx builder once. The `|| ver > nul` swallow keeps
REM the script from aborting if the builder already exists.
docker buildx create --name loon-agent-multiarch --use 2>nul || ver > nul

REM `--push` is required for multi-arch builds — buildx can't load
REM a multi-platform manifest into the local `docker images` store,
REM so the only valid output target is a registry. Make sure you've
REM already run `docker login` before invoking this.
docker buildx build ^
    --platform linux/amd64,linux/arm64 ^
    --tag %IMAGE% ^
    --push ^
    .

endlocal
