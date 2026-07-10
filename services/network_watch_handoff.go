package services

// Watch-folder hand-off mode. When the agent runs with AGENT_MODE=watch_folder
// it doesn't drive a torrent client itself — instead it drops a magnet URI
// (or for private trackers, the raw .torrent bytes) into WATCH_DIR and waits
// for the user's BT client of choice to deposit the completed download into
// DONE_DIR/<info_hash>/. The rest of the assembly+upload pipeline runs
// unchanged once the download lands.
//
// This file is just the I/O glue — main.go's processTask branches on the
// agent mode and either runs the existing internal-client path or calls
// these helpers.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HandoffSpec is everything the user's BT client needs to start the
// download. Either Magnet (public torrents) or TorrentBytes (private
// trackers — the site delivers the .torrent file because we don't want to
// hit DHT with a private hash).
type HandoffSpec struct {
	InfoHash     string
	Magnet       string
	TorrentBytes []byte // optional; non-nil for private uploads
}

// WriteHandoff places the work spec in watchDir so the user's monitored-
// folder workflow picks it up.
//
// Naming: <infohash>.torrent for private, <infohash>.magnet for public.
// The .magnet file is a single-line text file containing the magnet URI;
// some clients (qBittorrent, Deluge, rTorrent) treat .magnet as a watch-
// folder input out of the box. Clients that don't (Transmission) accept
// magnets via a small wrapper script — documented in the agent README.
//
// Atomic write: stage as <name>.tmp, fsync, rename. Without this the BT
// client's monitored-folder watcher can race and try to load a half-
// written file.
func WriteHandoff(watchDir string, spec HandoffSpec) (string, error) {
	if spec.InfoHash == "" {
		return "", fmt.Errorf("watch_handoff: empty info_hash")
	}
	if err := os.MkdirAll(watchDir, 0o755); err != nil {
		return "", fmt.Errorf("watch_handoff: mkdir %s: %w", watchDir, err)
	}
	var (
		finalName string
		body      []byte
	)
	if len(spec.TorrentBytes) > 0 {
		finalName = spec.InfoHash + ".torrent"
		body = spec.TorrentBytes
	} else {
		if spec.Magnet == "" {
			// Same normalize step as cmd/agent/main.go — the user's
			// BT client may be more permissive than anacrolix, but
			// uppercase base32 is the canonical form per BEP 9 and
			// works everywhere. Hex hashes pass through.
			spec.Magnet = "magnet:?xt=urn:btih:" + NormalizeInfoHash(spec.InfoHash)
		}
		finalName = spec.InfoHash + ".magnet"
		body = []byte(spec.Magnet)
	}
	finalPath := filepath.Join(watchDir, finalName)
	tmpPath := finalPath + ".tmp"
	if err := os.WriteFile(tmpPath, body, 0o644); err != nil {
		return "", fmt.Errorf("watch_handoff: write tmp: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("watch_handoff: rename: %w", err)
	}
	return finalPath, nil
}

// CleanupHandoff removes both possible spec filenames for an info hash.
// Idempotent — non-existence is not an error. Called after the BT client
// has obviously taken the file (or after we give up); we don't want
// orphan handoff files piling up in WATCH_DIR.
func CleanupHandoff(watchDir, infoHash string) {
	for _, name := range []string{infoHash + ".torrent", infoHash + ".magnet"} {
		_ = os.Remove(filepath.Join(watchDir, name))
	}
}

// HandoffDoneOpts tunes how WaitForHandoffDone polls the done dir.
type HandoffDoneOpts struct {
	// Tick is the polling interval. Default 15s — fine grained enough that a
	// small download lands in the pipeline within a quarter-minute, gentle
	// enough on FTP-mounted shares (where every directory listing is a
	// network round trip).
	Tick time.Duration
	// QuietPeriod requires the directory's mtime / total size to stop
	// changing for this long before we accept it as "done." Default 30s —
	// matches the offline watcher's behaviour and covers slow finalisation
	// in qBittorrent (move-completed, fastresume write, etc).
	QuietPeriod time.Duration
	// Timeout caps the total wait. Default 6h. The user's BT client may
	// take real-world time on rare/poorly-seeded torrents; we don't want
	// the lock to expire just because someone's home connection is slow
	// at 3am. Returns context.DeadlineExceeded when hit.
	Timeout time.Duration
	// OnTick fires on each poll iteration with the current elapsed time.
	// Used by the agent to push periodic /api/agent/progress heartbeats so
	// the site doesn't reap the lock as stale during a long handoff.
	OnTick func(elapsed time.Duration)
}

// WaitForHandoffDone blocks until <doneDir>/<infoHash>/ exists AND is
// quiet for QuietPeriod. Returns the absolute path to that directory on
// success, or an error if the timeout fires / context is cancelled.
//
// Detection rule: directory present AND (mtime older than QuietPeriod
// OR total size unchanged across two consecutive polls QuietPeriod
// apart). Either signal works alone — qBittorrent updates mtime when
// it moves files in, but some FTP backends preserve original mtimes,
// in which case we fall back to size-stability.
func WaitForHandoffDone(ctx context.Context, doneDir, infoHash string, opts HandoffDoneOpts) (string, error) {
	tick := opts.Tick
	if tick <= 0 {
		tick = 15 * time.Second
	}
	quiet := opts.QuietPeriod
	if quiet <= 0 {
		quiet = 30 * time.Second
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 6 * time.Hour
	}

	target := filepath.Join(doneDir, strings.ToLower(infoHash))
	deadline := time.Now().Add(timeout)
	start := time.Now()

	var prevSize int64 = -1
	var prevMtime time.Time

	t := time.NewTicker(tick)
	defer t.Stop()

	for {
		// Honour ctx cancel (admin clicks "skip" or agent shutdown).
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("watch_handoff: %s did not appear within %s", target, timeout)
		}

		info, err := os.Stat(target)
		switch {
		case err == nil && info.IsDir():
			size := dirSize(target)
			mtime := info.ModTime()
			// "Quiet" check: mtime older than quiet period AND size hasn't
			// changed since last poll. The size check has to be against the
			// previous poll specifically (not "older than quiet"), because
			// a torrent that's been moving files in for 5 minutes will have
			// a fresh mtime but a stable-for-now size.
			mtimeQuiet := time.Since(mtime) >= quiet
			sizeQuiet := prevSize >= 0 && size == prevSize && time.Since(prevMtime.Add(quiet)) >= 0
			if mtimeQuiet || sizeQuiet {
				return target, nil
			}
			prevSize = size
			prevMtime = mtime
		case err == nil && !info.IsDir():
			// A file with the info-hash name appeared instead of a dir.
			// That's not what the BT client should produce, but happens
			// when someone configures their client to keep files in the
			// root of DONE_DIR rather than in a per-torrent subfolder.
			// Treat the whole DONE_DIR as the result… no, too risky;
			// surface a clear error instead.
			return "", fmt.Errorf("watch_handoff: %s is a file, not a directory; configure your BT client's 'create subfolder per torrent' option", target)
		case os.IsNotExist(err):
			// Still waiting. Heartbeat caller so the site lock stays warm.
			if opts.OnTick != nil {
				opts.OnTick(time.Since(start))
			}
		default:
			return "", fmt.Errorf("watch_handoff: stat %s: %w", target, err)
		}

		// Sleep until next tick. Honour ctx.Done in the wait too so a
		// cancellation doesn't have to wait a full tick to take effect.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

// dirSize sums the byte sizes of every regular file under root. Errors
// are silently swallowed — a transient permission glitch on a single
// file shouldn't make the size signal unusable; we still get the
// mtime-based "quiet" check.
func dirSize(root string) int64 {
	var total int64
	_ = filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}
