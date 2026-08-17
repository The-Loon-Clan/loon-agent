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

# Every release must also tell the SITE what the newest build is, or the
# dashboard's "update available" nudge compares against a stale number —
# the hardcoded-constant era froze at 1.5.2 while the agent walked to
# 1.5.33 and nobody was ever nudged. The value is the agent_latest_version
# site setting.
VERSION=$(sed -n 's/^const AgentVersion = "\(.*\)"$/\1/p' client/version.go)
echo
echo "==> Pushed $IMAGE (agent version $VERSION)"
echo "==> NOW BUMP THE SITE'S UPDATE NUDGE (site setting agent_latest_version):"
echo "    write + commit indexer-tools/sql/<date>_agent_version.sql containing"
echo "      INSERT INTO site_settings (key, value) VALUES ('agent_latest_version', '$VERSION')"
echo "      ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value"
echo "    then: python3 opssql.py --file <that file> --expect 1 --reason 'agent $VERSION released'"
