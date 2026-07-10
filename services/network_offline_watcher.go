package services

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ameNZB/usenet-pipeline/storage"
)

const (
	// watcherTickInterval trades detection latency for CPU/IO cost. 15s is
	// plenty for a watch-folder workflow — users drop a file and expect a
	// job to appear within "a few moments," not sub-second.
	watcherTickInterval = 15 * time.Second

	// watcherQuietPeriod ignores files whose mtime is still recent so we
	// don't claim something that rsync/scp/a torrent client is still
	// writing. Long enough to cover slow transfers on small files, short
	// enough that the user's wait after drop-and-forget feels reasonable.
	watcherQuietPeriod = 30 * time.Second
)

// StartOfflineWatcher scans enabled watch folders on a ticker and queues
// offline_jobs for any new top-level entries it finds. Runs a single
// goroutine — the work per tick is cheap (directory reads + a handful of
// point lookups) and serialising avoids DB contention or double-claim
// races on the same source_path.
//
// Returns immediately if db is nil (the agent falls back gracefully when
// the local DB failed to open — site-polling still works).
func StartOfflineWatcher(ctx context.Context, db *storage.DB) {
	if db == nil {
		return
	}
	log.Printf("offline watcher started (interval=%s)", watcherTickInterval)
	ticker := time.NewTicker(watcherTickInterval)
	defer ticker.Stop()
	// Run once immediately so operators don't wait a full interval on
	// boot to see the first round of detection in the UI.
	scanAllWatches(db)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scanAllWatches(db)
		}
	}
}

func scanAllWatches(db *storage.DB) {
	watches, err := db.ListActiveWatches()
	if err != nil {
		log.Printf("watcher: list active: %v", err)
		return
	}
	for _, w := range watches {
		scanOneWatch(db, w)
	}
}

func scanOneWatch(db *storage.DB, w *storage.WatchFolder) {
	entries, err := os.ReadDir(w.Path)
	if err != nil {
		// Only log once per bad path to avoid spamming — TODO when we add
		// error surfaces in the UI, attach this to the watch row.
		log.Printf("watcher: read %s: %v", w.Path, err)
		return
	}
	for _, e := range entries {
		// Skip hidden/temp files — many tools (rsync, *.download) use
		// leading-dot or trailing-.part names while staging, and we
		// shouldn't claim a half-written file as a job.
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".tmp") {
			continue
		}
		fullPath := filepath.Join(w.Path, name)
		info, err := e.Info()
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) < watcherQuietPeriod {
			continue
		}
		exists, err := db.SourcePathExists(fullPath)
		if err != nil {
			log.Printf("watcher: exists(%s): %v", fullPath, err)
			continue
		}
		if exists {
			continue
		}
		job := &storage.OfflineJob{
			SourcePath:          fullPath,
			Title:               titleFromEntry(name, e.IsDir()),
			GroupID:             w.GroupID,
			GroupNameAtCreation: w.GroupName,
		}
		if err := db.CreateOfflineJob(job); err != nil {
			// UNIQUE violation means another process already claimed this
			// file between our exists check and insert — benign, skip.
			log.Printf("watcher: queue %s: %v", fullPath, err)
			continue
		}
		log.Printf("watcher: queued job %d for %s (group=%s)", job.ID, fullPath, w.GroupName)
	}
}

// titleFromEntry derives a human-readable title from a dropped file or
// directory. Stripping the extension on files keeps NZB subjects clean;
// directories keep their name intact since that's usually the release name.
func titleFromEntry(name string, isDir bool) string {
	if isDir {
		return name
	}
	return strings.TrimSuffix(name, filepath.Ext(name))
}
