#!/bin/bash
# Deploy the agent to a remote VPS.
# Usage: ./deploy.sh [user@host] [remote_dir]
#
# The target host comes from the first argument or the DEPLOY_HOST
# env var; the SSH port from DEPLOY_SSH_PORT (default 22). Nothing
# operator-specific is committed here — export the values in your
# shell profile or an untracked wrapper.
#
# Example:
#   DEPLOY_HOST=agent@my-server ./deploy.sh
#   ./deploy.sh agent@my-server ~/indexer-agent
#
# Prerequisites:
#   - SSH access to the VPS
#   - Docker + Docker Compose on the VPS
#   - A .env file on the VPS (copy from .env.example and fill in)

set -euo pipefail

REMOTE_HOST="${1:-${DEPLOY_HOST:-}}"
REMOTE_DIR="${2:-~/indexer-agent}"
SSH_PORT="${DEPLOY_SSH_PORT:-22}"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

if [ -z "${REMOTE_HOST}" ]; then
    echo "ERROR: no deploy target. Pass user@host as the first argument or set DEPLOY_HOST." >&2
    exit 1
fi

echo "=== Deploying indexer-agent to ${REMOTE_HOST}:${REMOTE_DIR} ==="

# Files to sync (excludes .env, data/, temp/, .git/)
echo "Syncing source files..."
rsync -avz --delete -e "ssh -p ${SSH_PORT}" \
    --exclude '.env' \
    --exclude 'data/' \
    --exclude 'temp/' \
    --exclude '.git/' \
    --exclude '*.exe' \
    --exclude 'indexer-linux-*' \
    --exclude 'indexer-windows-*' \
    "${SCRIPT_DIR}/" "${REMOTE_HOST}:${REMOTE_DIR}/"

echo ""
echo "Building and starting on VPS..."
ssh -p "${SSH_PORT}" "${REMOTE_HOST}" bash -s "${REMOTE_DIR}" << 'REMOTE_SCRIPT'
    REMOTE_DIR="$1"
    cd "${REMOTE_DIR}"

    # Check .env exists
    if [ ! -f .env ]; then
        echo "ERROR: No .env file found at ${REMOTE_DIR}/.env"
        echo "Copy .env.example to .env and fill in your credentials:"
        echo "  cp .env.example .env"
        echo "  nano .env"
        exit 1
    fi

    # Build and restart
    echo "Building Docker image..."
    docker compose build --no-cache

    echo "Starting services..."
    docker compose up -d

    echo ""
    echo "=== Deployment complete ==="
    docker compose ps
    echo ""
    echo "Logs: docker compose -f ${REMOTE_DIR}/docker-compose.yml logs -f agent"
REMOTE_SCRIPT
