#!/usr/bin/env bash
# Multi-arch publish to Docker Hub (linux/amd64 + linux/arm64).
# Bash equivalent of push_docker.bat for operators on macOS / Linux.
set -euo pipefail

IMAGE="${IMAGE:-amenzb/loon-agent:latest}"

# Create the buildx builder once; reuse if it already exists.
docker buildx create --name loon-agent-multiarch --use 2>/dev/null || true

# `--push` is required for multi-arch builds — buildx can't load a
# multi-platform manifest into the local docker images store, so
# the only valid output target is a registry. Run `docker login`
# first.
docker buildx build \
    --platform linux/amd64,linux/arm64 \
    --tag "$IMAGE" \
    --push \
    .
