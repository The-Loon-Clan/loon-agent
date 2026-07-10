# usenet-pipeline

A self-hosted agent that automates the torrent-to-Usenet pipeline: downloads torrents, extracts media metadata, generates PAR2 recovery, optionally encrypts, uploads to Usenet via NNTP, and reports back to a companion indexer site with a complete NZB.

Designed to run on a VPS behind a VPN with zero manual intervention.

## Features

- Torrent downloading via magnet links (anacrolix/torrent, pure Go)
- Usenet uploading with parallel NNTP connections and yEnc encoding
- PAR2 recovery block generation (parpar or par2cmdline)
- Optional 7z encryption with header obfuscation
- Optional filename obfuscation (random hex subjects)
- Media analysis via ffprobe (codec, resolution, HDR detection)
- Automatic screenshot capture via ffmpeg
- VPN integration via gluetun (full-tunnel or split-tunnel)
- Disk space reservation and CPU throttling
- Slow/dead download detection and automatic rejection
- Live status reporting to companion site (speed, phase, VPN IP)
- Web-configurable settings (no restart needed for most changes)

## Quick Start

### 1. Prerequisites

- Docker + Docker Compose
- A running indexer site instance with an agent token
- Usenet provider credentials
- VPN credentials (recommended)

### 2. Configure

```bash
cp .env.example .env
nano .env
```

**Required settings:**

| Variable | Description |
|----------|-------------|
| `SITE_URL` | Your indexer site URL |
| `AGENT_TOKEN` | Agent token from Account Settings |
| `NNTP_SERVER` | Usenet server address (e.g. `news.provider.com:563`) |
| `NNTP_SSL` | `true` for TLS connections |
| `NNTP_USER` / `NNTP_PASS` | Usenet credentials |
| `NNTP_GROUP` | Target newsgroup |
| `NNTP_POSTER` | Poster identity (e.g. `agent <agent@your-domain.com>`) |

See [.env.example](.env.example) for the full list with descriptions.

### 3. Build and start

```bash
docker compose build --no-cache
docker compose up -d
```

### 4. Verify

```bash
docker compose logs -f agent
```

You should see:
```
Polling for work...
No task available, sleeping 30s
```

## VPN Modes

### Full tunnel (default)

All agent traffic routes through gluetun. Acts as an automatic kill-switch — if the VPN drops, nothing leaks.

```env
VPN_DOWNLOAD_ONLY=false
```

### Split tunnel

Only torrent downloads go through the VPN (via SOCKS5 proxy). NNTP uploads and site API calls go direct for full speed.

```env
VPN_DOWNLOAD_ONLY=true
AGENT_NETWORK_MODE=bridge
```

Gluetun exposes a SOCKS5 proxy on port 1080. The torrent client is automatically configured to use it.

## How It Works

```
Poll site for task
  |
  v
Download torrent (VPN-protected)
  |
  v
Analyze: ffprobe metadata + screenshots
  |
  v
Generate PAR2 recovery blocks
  |
  v
Optional: 7z encryption
  |
  v
Upload to Usenet via NNTP
  |
  v
Report NZB + metadata to site
  |
  v
Poll for next task
```

## Architecture

```
+----------+     +----------+     +-----------+
| Gluetun  | <-- |  Agent   | --> | NNTP      |
| (VPN)    |     |          |     | (Usenet)  |
+----------+     +----+-----+     +-----------+
                      |
                      | HTTPS
                      v
                +----------+
                | Indexer   |
                | Site      |
                +----------+
```

In full-tunnel mode, all traffic goes through gluetun. In split-tunnel mode, only torrent traffic uses the VPN's SOCKS5 proxy.

## Web Dashboard Configuration

Most settings can be changed from the site UI without restarting:

- **Mode**: Auto (picks requests) or Manual (only assigned tasks)
- **Speed limits**: Max download/upload KB/s
- **Pool**: Filter by category
- **Shutdown rules**: After N downloads, minutes, or points
- **Torrent watch folder**: Monitor a directory for completed downloads
- **Built-in FTP server**: Accept uploads directly

## Processing Options

| Variable | Default | Description |
|----------|---------|-------------|
| `ENCRYPT` | `false` | Wrap files in password-protected 7z |
| `OBFUSCATE` | `false` | Rename files to random hex on Usenet |
| `PAR2_REDUNDANCY` | `5` | Recovery block percentage |
| `PAR2_THREADS` | `0` | CPU threads for PAR2 (0 = all) |
| `MAX_CONCURRENT_DOWNLOADS` | `3` | Parallel torrent downloads |
| `MAX_DISK_USAGE_GB` | `0` | Disk cap (0 = no limit) |
| `CPU_MAX_PERCENT` | `85` | Pause new tasks above this CPU % |
| `SLOW_SPEED_THRESHOLD_MBS` | `0.05` | Reject downloads below this speed |
| `SLOW_SPEED_TIMEOUT_MINS` | `10` | Minutes before slow rejection |

## Updating

```bash
docker compose build --no-cache
docker compose up -d
```

## Troubleshooting

**Agent can't connect to site**
- Verify `SITE_URL` is reachable from the VPS
- Check `AGENT_TOKEN` matches a token in Account Settings
- If using VPN: confirm VPN is up first (`docker compose logs vpn`)

**VPN won't connect**
- Check credentials in `.env`
- Try a different country/server
- See [gluetun docs](https://github.com/qdm12/gluetun-wiki) for your provider

**Upload failures**
- Verify NNTP credentials and newsgroup
- Check connection count doesn't exceed provider limits
- Look for retry errors in logs

**No tasks picked up**
- Set mode to "auto" on the dashboard
- Check pool filter isn't excluding available requests
- Verify there are unfulfilled requests on the site

## License

[MIT](LICENSE)
