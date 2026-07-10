# Contributing

Thanks for your interest in contributing to usenet-pipeline.

## Development Setup

### Prerequisites

- Go 1.24+
- Docker + Docker Compose (for full integration testing)
- ffmpeg and ffprobe (for media processing)
- parpar or par2cmdline (for PAR2 recovery)
- 7zip (for optional encryption)

### Building

```bash
go build -o indexer-agent .
```

Or with Docker:

```bash
docker compose build
```

### Running locally

```bash
cp .env.example .env
# Edit .env with your credentials
./indexer-agent
```

## Project Structure

```
.
├── main.go              # Entry point: polling loop, task dispatch
├── config/              # Environment variable configuration
├── client/              # HTTP client for site API communication
├── services/
│   ├── torrent.go       # Torrent downloading (anacrolix/torrent)
│   ├── usenet.go        # NNTP posting + yEnc encoding
│   ├── nzb.go           # NZB XML generation
│   ├── par2.go          # PAR2 recovery block generation
│   ├── mediainfo.go     # ffprobe metadata extraction
│   ├── screenshots.go   # ffmpeg screenshot capture
│   ├── archive.go       # 7z encryption
│   ├── obfuscate.go     # Filename obfuscation
│   ├── network.go       # VPN connectivity monitoring
│   └── disk_reserve.go  # Disk space reservation tracking
├── storage/             # JSON state persistence
└── utils/               # Shared helpers
```

## Pull Requests

1. Fork the repo and create your branch from `main`
2. Make your changes
3. Run `go build ./...` and `go vet ./...` to verify
4. Write a clear commit message explaining *why*, not just *what*
5. Open a PR

## Code Style

- Follow standard Go conventions (`gofmt`, `go vet`)
- Keep functions focused and small
- Add comments for non-obvious design decisions, not obvious code
- Error messages should be lowercase, no trailing punctuation

## Reporting Issues

Open an issue with:
- What you expected to happen
- What actually happened
- Steps to reproduce
- Agent logs (redact credentials)
