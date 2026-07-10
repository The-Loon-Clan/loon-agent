# Stage 1: Build the Go application
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git || true
WORKDIR /app

# Copy module manifests first and download dependencies so the
# layer can be cached across rebuilds whenever only source changes
# (.go files churn far more often than go.mod/go.sum).
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build without -mod=vendor: the vendor/ directory is .gitignored so
# a fresh `git clone && docker build` doesn't have one, and the
# module cache populated by `go mod download` above is all the
# compiler needs. This was the original "vendor out of sync" error
# operators saw when cloning the public repo.
RUN CGO_ENABLED=0 GOOS=linux go build -o indexer ./cmd/agent

# Stage 2: Build parpar native C++ addon (needs python3 + build tools for node-gyp)
FROM node:22-alpine AS parpar-builder
RUN apk add --no-cache python3 make g++ && \
    npm install -g @animetosho/parpar && \
    npm cache clean --force && \
    parpar --version

# Stage 3: Create the runtime container — use the SAME node base so the
# native addon's NODE_MODULE_VERSION matches the runtime exactly.
FROM node:22-alpine

# UTF-8 locale so spawned binaries (mediainfo / ffmpeg / mkvmerge / 7z /
# par2 / chromaprint / tesseract) handle non-ASCII filenames + paths
# (Chinese, Japanese, accented characters) correctly. The default POSIX
# locale causes mediainfo to silently skip multibyte-named files and
# ffmpeg to corrupt path output. musl 1.2.4+ ships C.UTF-8 natively, so
# no extra apk package is required.
ENV LANG=C.UTF-8
ENV LC_ALL=C.UTF-8

RUN apk --no-cache add ca-certificates ffmpeg par2cmdline mkvtoolnix \
        chromaprint \
        tesseract-ocr tesseract-ocr-data-eng tesseract-ocr-data-jpn && \
    (apk --no-cache add 7zip || apk --no-cache add p7zip) && \
    (apk --no-cache add unrar 2>/dev/null || true)
# unrar (Alpine community repo) is the preferred backend for the
# agent's services.ExtractRARArchives step; 7z is the fallback when
# unrar isn't available. Both are tried at startup and the first one
# found wins — see services/extract_rar.go:detectRARBinary.
# The `chromaprint` package (NOT `chromaprint-tools` — that name only
# exists on Debian/Ubuntu) ships `/usr/bin/fpcalc` for the Phase G
# acoustic-fingerprint extractor. tesseract-ocr + the eng/jpn data
# files back the Phase I manga OCR step. Both are optional in the
# agent code (LookPath miss logs and skips), but baking them into
# the image is what actually turns those features on for stock
# deployments — without them every release skips both phases
# silently.
WORKDIR /app

# parpar: copy the full package tree from the builder. The npm symlink at
# /usr/local/bin/parpar is relative (../lib/node_modules/@animetosho/parpar/bin/parpar.js)
# so we copy both the modules and recreate the link.
COPY --from=parpar-builder /usr/local/lib/node_modules/@animetosho /usr/local/lib/node_modules/@animetosho
RUN ln -sf /usr/local/lib/node_modules/@animetosho/parpar/bin/parpar.js /usr/local/bin/parpar
COPY --from=builder /app/indexer .

CMD ["./indexer"]
