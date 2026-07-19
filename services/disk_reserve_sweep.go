package services

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/the-loon-clan/loon-agent/storage"
)

// diskReserved tracks the total bytes reserved by all in-flight tasks.
// Each task reserves space after learning its torrent size and releases
// it on completion (or failure). The polling loop checks
// FreeDiskAfterReservations() before accepting new work.
var diskReserved int64

// reservationsMu protects the per-task map for logging/debugging.
var reservationsMu sync.Mutex
var reservations = map[string]int64{} // jobName → bytes reserved

// ReserveDisk claims bytes for a task. Call once after the torrent size
// is known. The multiplier accounts for download + staging + PAR2:
//
//	downloaded file(s)            = 1.0x
//	staging via hardlink          = ~0x  (CopyFiles tries os.Link first)
//	PAR2 recovery (5%)            = 0.05x
//	safety margin (FS overhead +
//	   PAR2 working files)        = 0.20x
//
// Total: ~1.25x of the torrent size, rounded to 1.3 for headroom.
//
// stageDir lives at filepath.Join(cfg.TempDir, "stage-XXX") (see
// cmd/agent/main.go), which is ALWAYS on the same device as dataDir
// (also under TempDir), so the os.Link call inside CopyFiles
// /ObfuscateFiles succeeds and the staged tree costs ~zero bytes —
// just additional inodes pointing at the same data extents.
//
// User-reported 2026-06-05 ("Always a Catch" / "Versatile Mage" /
// "Megami" all rejected at "torrent 1.4 GB, have 1.4 GB free"):
// the previous 2.1x multiplier was sized for the worst-case cross-
// device stage fallback (full copy) that doesn't happen in this
// agent's layout. Lowering to 1.3 unblocks a 1.4 GB torrent at
// just 1.82 GB free instead of demanding 2.94 GB.
//
// If your stage dir IS on a different device for some reason (rare;
// would require manually moving the stage subdir to a separate
// mount), CopyFiles' fallback path does a full copy and the actual
// usage approaches 2.1x. In that scenario you'll see the staging
// step's auditStaged log fire with the full byte count, and the
// pre-stage size check (1.5.25) catches under-budget cases before
// PAR2 burns more disk.
const DiskMultiplier = 1.3

// ReserveDisk is idempotent on jobName: calling it twice for the same
// task subtracts the previous reservation before adding the new one,
// so the global diskReserved counter tracks the most recent claim and
// never accumulates phantom bytes from re-dispatch / resume / retry.
//
// Pre-1.5.23 the function always added to diskReserved AND
// unconditionally overwrote the per-task map entry — double-calls
// leaked the difference permanently. Hundreds of leaks over agent
// lifetime → "Reserved 159 GB" on a VPS with no in-flight tasks,
// blocking new downloads on free-space gating with no real
// occupancy.
func ReserveDisk(jobName string, torrentBytes int64) {
	reserve := int64(float64(torrentBytes) * DiskMultiplier)

	reservationsMu.Lock()
	prevReserve, hadPrev := reservations[jobName]
	reservations[jobName] = reserve
	reservationsMu.Unlock()

	// Reconcile the global counter against any prior claim for this
	// job. delta = (new - old) when overwriting, = new when fresh.
	delta := reserve - prevReserve
	atomic.AddInt64(&diskReserved, delta)

	if hadPrev {
		log.Printf("Disk: re-reserved %.1f GB for %s (was %.1f GB; torrent=%.1f GB, total reserved=%.1f GB) — double ReserveDisk call without intervening ReleaseDisk, reconciled",
			float64(reserve)/1e9, jobName, float64(prevReserve)/1e9,
			float64(torrentBytes)/1e9,
			float64(atomic.LoadInt64(&diskReserved))/1e9)
		return
	}
	log.Printf("Disk: reserved %.1f GB for %s (torrent=%.1f GB, total reserved=%.1f GB)",
		float64(reserve)/1e9, jobName, float64(torrentBytes)/1e9,
		float64(atomic.LoadInt64(&diskReserved))/1e9)
}

// ReleaseDisk frees the reservation for a completed/failed task.
func ReleaseDisk(jobName string) {
	reservationsMu.Lock()
	reserve, ok := reservations[jobName]
	if ok {
		delete(reservations, jobName)
	}
	reservationsMu.Unlock()

	if ok {
		atomic.AddInt64(&diskReserved, -reserve)
		log.Printf("Disk: released %.1f GB for %s (total reserved=%.1f GB)",
			float64(reserve)/1e9, jobName,
			float64(atomic.LoadInt64(&diskReserved))/1e9)
	}
}

// diskMaxBytes is the user-configured cap (0 = no cap, use all available).
// Set once at startup via InitDiskLimit.
var diskMaxBytes uint64

// InitDiskLimit sets the maximum disk usage. Call once at startup.
// maxGB=0 means no limit.
func InitDiskLimit(maxGB float64) {
	if maxGB > 0 {
		diskMaxBytes = uint64(maxGB * 1024 * 1024 * 1024)
		log.Printf("Disk: usage capped at %.0f GB", maxGB)
	}
}

// FreeDiskAfterReservations returns the effective free space: the lesser of
// actual free space and the user-configured budget, minus what's already
// reserved by in-flight tasks.
func FreeDiskAfterReservations(path string) (uint64, error) {
	free, err := FreeDiskSpace(path)
	if err != nil {
		return 0, err
	}

	// If a max disk cap is set, compute remaining budget from actual usage.
	if diskMaxBytes > 0 {
		used := diskUsage(path)
		if used >= diskMaxBytes {
			free = 0
		} else if remaining := diskMaxBytes - used; remaining < free {
			free = remaining
		}
	}

	reserved := atomic.LoadInt64(&diskReserved)
	if reserved <= 0 {
		return free, nil
	}
	if int64(free) <= reserved {
		return 0, nil
	}
	return free - uint64(reserved), nil
}

// diskUsage walks path and sums file sizes (how much the agent is currently using).
func diskUsage(path string) uint64 {
	var total uint64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total
}

// TotalReservedBytes returns the current total reservation for logging.
func TotalReservedBytes() int64 {
	return atomic.LoadInt64(&diskReserved)
}

// SweepOrphanDownloads removes stale temp entries in tempDir left behind by
// crashed, aborted, or force-killed tasks. Called once at startup after
// storage.LoadState (so the in-memory job map is populated) and then on a
// ticker every hour so long-running agents don't accumulate over weeks.
//
// Sweeps four categories:
//
//	dl-{jobName}/        — torrent download dirs. Preserved only if a job's
//	                       DownloadedPath still references them, since that's
//	                       the resume path's input. Anything else is
//	                       guaranteed-unreachable.
//	dl-{jobName}.torrent — .torrent staging files for the file-based download
//	                       path. defer os.Remove handles the happy path; the
//	                       sweep catches force-kill leftovers.
//	screens-*/           — screenshot working dirs from the metadata phase.
//	                       Never resumable; always safe to remove.
//	stage-*/             — upload staging dirs (obfuscated copies). Only the
//	                       dirs we created with the explicit "stage-" prefix
//	                       — pre-existing random-named stage dirs are out of
//	                       scope so we don't accidentally take user data.
//
// minAge filters out very fresh entries on the periodic ticker so we never
// rip out a dir an active task is mid-write to. The startup call uses
// minAge=0 since by then no task can be running yet.
func SweepOrphanDownloads(tempDir string) {
	sweepWithMinAge(tempDir, 0, false)
}

// SweepOrphanDownloadsAged is the periodic-tick variant: only entries
// older than minAge get swept. Keeps in-flight task working dirs safe.
func SweepOrphanDownloadsAged(tempDir string, minAge time.Duration) {
	sweepWithMinAge(tempDir, minAge, false)
}

// SweepOrphanTempStartup is the boot-time variant. On top of the core
// dl-/screens-/stage- families it also reclaims the background-pipeline
// temp families that the online sweep deliberately ignores: offline-*
// (offline-stage-/enc-/shots-/pages-/samples-), enc-, wrap-, offer-, and
// bare *.7z encrypt archives. Those prefixes are created by the offline
// processor and offer fulfiller, whose jobs aren't in GlobalState — so
// the periodic sweep can't build a keep-set for them and never touches
// them. That is only safe at STARTUP, before those goroutines are
// launched (cmd/agent/main.go dispatches them after this call), so
// anything matching is a guaranteed-dead orphan from a previous run that
// no other sweep would ever reclaim. Force-kill leftovers therefore
// accumulate until the next boot rather than forever.
func SweepOrphanTempStartup(tempDir string) {
	sweepWithMinAge(tempDir, 0, true)
}

func sweepWithMinAge(tempDir string, minAge time.Duration, includeExtended bool) {
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		return
	}

	// Build the keep-set from in-flight job state. We protect two
	// categories:
	//
	//   dl-XXX  — sourced from DownloadedPath (the file inside the dir);
	//             needed because a resume after restart re-uses the file.
	//   stage-XXX — sourced from StagePath, set as soon as the stage dir
	//             is created. A task can sit here for an unbounded time
	//             waiting for the upload slot, and previously the 30-min
	//             sweep would wipe its stage out from under it while it
	//             waited, leading to "Complete (0 articles)" finishes.
	keep := map[string]bool{}
	storage.GlobalState.RLock()
	for jobName, job := range storage.GlobalState.Jobs {
		if job == nil {
			continue
		}
		// CRITICAL: Always protect "dl-<jobName>" for every job in
		// the state map, regardless of whether DownloadedPath has
		// been stamped yet. DownloadedPath is set by
		// storage.UpdateJobMeta only AFTER the download completes
		// (cmd/agent/main.go ~line 1530). During the active
		// download — which can run for minutes — DownloadedPath is
		// "" and the old code skipped the job entirely. The
		// periodic 30-min ticker then deleted the dl-XXX directory
		// out from under anacrolix mid-write, producing
		// "expected output missing after WaitAll" with the whole
		// dataDir gone (witnessed in prod 2026-06-04 on
		// request-21856, where peak-peers=5 and last-progress=100%
		// but the file vanished by WaitAll return).
		//
		// jobName matches the lock identifier used to build the
		// dir name ("request-NNNN" → "dl-request-NNNN"), so we
		// protect it directly.
		if jobName != "" {
			keep["dl-"+jobName] = true
		}
		if job.Name != "" && job.Name != jobName {
			keep["dl-"+job.Name] = true
		}
		// DownloadedPath has TWO possible shapes depending on agent
		// version + torrent layout:
		//   1. <tempDir>/dl-XXX/<file>     pre-1.5.18, file inside dl-XXX
		//   2. <tempDir>/dl-XXX            1.5.18+, dl-XXX directly
		// Walk up the path looking for a "dl-" / "stage-" / "screens-"
		// prefix and protect that name. Without this loop, the 1.5.18
		// behaviour change (always-return-dataDir) inverted the
		// Base(Dir(...)) computation here from "dl-request-NNN" to
		// "temp" — the sweep then wiped active downloads mid-flight,
		// producing "expected output missing after WaitAll" with the
		// whole dataDir gone (witnessed in prod 2026-06-03).
		//
		// This block is kept as a belt-and-braces complement to the
		// unconditional dl-<jobName> protection above — it also
		// catches stage-XXX / screens-XXX dirs that downstream
		// stages embed in DownloadedPath.
		if job.DownloadedPath != "" {
			p := job.DownloadedPath
			for p != "" && p != "/" && p != "." {
				b := filepath.Base(p)
				if strings.HasPrefix(b, "dl-") || strings.HasPrefix(b, "stage-") || strings.HasPrefix(b, "screens-") {
					keep[b] = true
					break
				}
				next := filepath.Dir(p)
				if next == p {
					break
				}
				p = next
			}
		}
		if job.StagePath != "" {
			keep[filepath.Base(job.StagePath)] = true
		}
	}
	storage.GlobalState.RUnlock()

	cutoff := time.Now().Add(-minAge)

	var removed int
	var freedBytes uint64
	for _, e := range entries {
		name := e.Name()
		matches := strings.HasPrefix(name, "dl-") ||
			strings.HasPrefix(name, "screens-") ||
			strings.HasPrefix(name, "stage-")
		if !matches && includeExtended {
			// Boot-only families (see SweepOrphanTempStartup). "offline-"
			// covers offline-stage-/enc-/shots-/pages-/samples-; the bare
			// *.7z catches encrypt archives that were renamed away from a
			// recognizable prefix.
			matches = strings.HasPrefix(name, "offline-") ||
				strings.HasPrefix(name, "enc-") ||
				strings.HasPrefix(name, "wrap-") ||
				strings.HasPrefix(name, "offer-") ||
				strings.HasSuffix(name, ".7z")
		}
		if !matches {
			continue
		}
		if keep[name] {
			continue
		}
		// On the ticker pass, skip anything modified recently — a brand-
		// new dl- dir that just got created seconds ago belongs to an
		// active task we don't want to nuke.
		if minAge > 0 {
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
		}
		full := filepath.Join(tempDir, name)
		if e.IsDir() {
			freedBytes += diskUsage(full)
		} else if info, err := e.Info(); err == nil {
			freedBytes += uint64(info.Size())
		}
		if err := os.RemoveAll(full); err != nil {
			log.Printf("Sweep: failed to remove %s: %v", full, err)
			continue
		}
		removed++
	}
	if removed > 0 {
		log.Printf("Sweep: removed %d orphan entries (%.1f GB freed)",
			removed, float64(freedBytes)/1e9)
	}
}
