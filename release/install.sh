#!/usr/bin/env bash
set -euo pipefail

echo "=== loon-agent installer ==="
echo ""

# Check for Docker
if ! command -v docker &>/dev/null; then
    echo "Error: Docker is not installed."
    echo "Install it from https://docs.docker.com/engine/install/"
    exit 1
fi

if ! docker compose version &>/dev/null; then
    echo "Error: Docker Compose v2 is not available."
    echo "Install it from https://docs.docker.com/compose/install/"
    exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPT_DIR"

# Create data directories
mkdir -p data/agent data/gluetun
echo "Data directory: $SCRIPT_DIR/data"

# Create .env from example if it doesn't exist
if [ ! -f .env ]; then
    cp .env.example .env
    echo ""
    echo "Created .env from .env.example."
    echo "You MUST edit .env before starting — fill in your credentials:"
    echo ""
    echo "  Required:"
    echo "    SITE_URL        — Your indexer site URL"
    echo "    AGENT_TOKEN     — Agent token from Account Settings"
    echo "    VPN_PROVIDER    — Your VPN provider (see gluetun docs)"
    echo "    VPN_USER        — VPN username"
    echo "    VPN_PASS        — VPN password"
    echo "    NNTP_SERVER     — Usenet server (e.g. news.provider.com:563)"
    echo "    NNTP_USER       — Usenet username"
    echo "    NNTP_PASS       — Usenet password"
    echo ""
    echo "  Edit with:  nano $SCRIPT_DIR/.env"
    echo ""
    read -rp "Press Enter after editing .env to continue (or Ctrl+C to exit)..."
else
    echo "Existing .env found, keeping it."
fi

# Pull and start
echo ""
echo "Pulling images..."
docker compose pull

echo ""
echo "Starting containers..."
docker compose up -d

echo ""
echo "=== Installation complete ==="
echo ""
echo "Check logs:    docker compose logs -f agent"
echo "Stop:          docker compose down"
echo "Update:        docker compose pull && docker compose up -d"
