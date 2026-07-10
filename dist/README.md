# usenet-pipeline — Pre-built Distribution

This directory is used for distributing pre-built Docker images to end users who don't need the source code.

## What this package contains

- `docker-compose.yml` — defines the VPN tunnel and agent containers
- `.env.example` — all configurable variables with descriptions
- `README.md` — this file

## Setup

See the [main README](../README.md) for full documentation.

Quick version:

```bash
cp .env.example .env
nano .env                          # fill in your credentials
docker compose pull
docker compose up -d
docker compose logs -f agent       # verify it's running
```

## Updating

```bash
docker compose pull
docker compose up -d
```
