package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	mathrand "math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ameNZB/loon-agent/client"
	"github.com/ameNZB/loon-agent/config"
	"github.com/ameNZB/loon-agent/services"
	"github.com/ameNZB/loon-agent/storage"
	"github.com/ameNZB/loon-agent/utils"
)

// ── Live status: aggregated across all concurrent tasks ─────────────────────

var (
	liveStatusMu sync.RWMutex
	liveStatus   = client.AgentLiveStatus{Phase: "idle"}

	// Per-task progress tracking for the dashboard.
	taskProgressMu sync.RWMutex
	taskProgress   = map[int64]*client.FileProgress{} // keyed by request ID

	// processStart is captured at package-init time so the
	// stop_after_current handler can tell "we just started up" from
	// "we've been running a while". Used to guard against the docker
	// restart loop where the site keeps sending stop_after_current
	// across reboots — without this guard, each new process polls
	// once, sees stop, exits cleanly, docker restarts under its
	// restart-unless-stopped policy, and the cycle repeats every
	// ~60s indefinitely (we hit RestartCount=290 in production
	// before catching it).
	processStart = time.Now()
)

// runWatchdog periodically snapshots taskProgress and emits one line
// per in-flight task to the log. Pair the output with the slot
// ACQUIRED/RELEASED lines and a hung task is grep-pinpointable: same
// rid showing the same phase + percent across several consecutive
// ticks means whichever code path corresponds to that phase is stuck.
//
// Started once at boot from main(); never returns. Read-lock so it
// can't deadlock against per-task progress updates.
//
// Tracks each task's "last-changed-tick" so a task that's been at
// the same phase + percent for >= stallTicks consecutive ticks is
// flagged with a STALLED prefix — surfaces wedges quickly without
// the operator having to compare lines by eye.
func runWatchdog(ctx context.Context, interval time.Duration) {
	const stallTicks = 3 // flag stalled after 3 ticks (default 3 min)
	type prevState struct {
		Phase   string
		Percent float64
		Ticks   int
	}
	prev := map[int64]prevState{}
	// prevGoroutines tracks the goroutine count from the previous tick
	// so we can flag a sudden >50% growth as a likely leak signal — a
	// stuck worker pool or unreleased connection won't OOM the agent
	// for hours, but the goroutine count climbs steadily long before
	// the heap does. Local to this goroutine, so no mutex needed.
	prevGoroutines := 0
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[watchdog] shutdown")
			return
		case <-t.C:
		}

		// Runtime introspection: goroutine count + a small MemStats
		// sample. ReadMemStats does a brief stop-the-world, but the
		// 60s tick interval keeps this well below noise. HeapAlloc /
		// StackInuse are the two numbers that move when a leak is
		// underway; the rest of MemStats is intentionally not
		// surfaced to keep the log line scannable.
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		goroutines := runtime.NumGoroutine()
		heapMiB := ms.HeapAlloc >> 20
		stackMiB := ms.StackInuse >> 20
		if prevGoroutines > 0 && goroutines > (prevGoroutines*3/2) {
			log.Printf("[watchdog] WARNING: goroutine count grew %d -> %d (>50%%) — likely leak", prevGoroutines, goroutines)
		}
		prevGoroutines = goroutines

		taskProgressMu.RLock()
		snap := make(map[int64]*client.FileProgress, len(taskProgress))
		for k, v := range taskProgress {
			if v != nil {
				cp := *v
				snap[k] = &cp
			}
		}
		taskProgressMu.RUnlock()

		// Count stalled tasks (tasks at >= stallTicks consecutive unchanged
		// ticks) so the per-tick header has the stalled count alongside
		// goroutines / heap / stack / tasks_active for one-line scanning.
		stalled := 0
		for rid, fp := range snap {
			p := prev[rid]
			if p.Phase == fp.Phase && p.Percent == fp.Percent && p.Ticks+1 >= stallTicks {
				stalled++
			}
		}
		log.Printf("[watchdog] goroutines=%d heap=%dMiB stack=%dMiB tasks_active=%d stalled=%d",
			goroutines, heapMiB, stackMiB, len(snap), stalled)

		if len(snap) == 0 {
			log.Printf("[watchdog] no tasks in flight")
			continue
		}

		log.Printf("[watchdog] %d task(s) in flight:", len(snap))
		seen := map[int64]bool{}
		for rid, fp := range snap {
			seen[rid] = true
			p := prev[rid]
			tag := ""
			if p.Phase == fp.Phase && p.Percent == fp.Percent {
				p.Ticks++
				if p.Ticks >= stallTicks {
					tag = "  ⚠ STALLED " + (interval * time.Duration(p.Ticks)).Round(time.Second).String() + " in " + fp.Phase
				}
			} else {
				p.Ticks = 0
			}
			p.Phase = fp.Phase
			p.Percent = fp.Percent
			prev[rid] = p
			log.Printf("[watchdog]   request=%d phase=%s percent=%.1f%% speed=%s%s",
				rid, fp.Phase, fp.Percent, fp.Speed, tag)
		}
		// Forget tasks no longer in flight so the prev map doesn't
		// leak across long agent runs.
		for rid := range prev {
			if !seen[rid] {
				delete(prev, rid)
			}
		}
	}
}

// ── Agent-local error backoff ───────────────────────────────────────────────
//
// When the agent hits an error that's clearly about the local environment
// rather than the torrent itself (port bind failure, disk full, VPN proxy
// unreachable), we release the lock back to the pool with status="aborted"
// instead of "failed" — the site treats aborted as non-punitive and the
// request is immediately claimable again, by this agent or any other.
//
// If those errors *keep* happening, though, we don't want to churn through
// the entire queue burning one task after another. After N consecutive
// aborts we pause task polling for a few minutes so the operator has a
// chance to fix the environment before more tasks are touched.
var (
	abortCounterMu    sync.Mutex
	consecutiveAborts int
	abortPauseUntil   time.Time
)

const (
	abortPauseThreshold = 5
	abortPauseDuration  = 5 * time.Minute
	// oversizeSkipTTL is how long we remember "this request doesn't fit on
	// this agent" so we don't re-claim and re-fetch metadata for it every
	// poll. Long enough that the site will usually route it elsewhere or
	// the operator will notice, short enough that a disk-space change
	// (another task finishing) unblocks it.
	oversizeSkipTTL = 30 * time.Minute
)

func recordAbort(reason string) {
	abortCounterMu.Lock()
	defer abortCounterMu.Unlock()
	consecutiveAborts++
	if consecutiveAborts >= abortPauseThreshold {
		abortPauseUntil = time.Now().Add(abortPauseDuration)
		log.Printf("Agent self-pause: %d consecutive agent-local errors (last: %s) — pausing task polls for %s",
			consecutiveAborts, reason, abortPauseDuration)
	}
}

// oversizeSkip tracks request IDs that failed the pre-flight disk check, so
// we can decline to re-download their metadata if the site hands the same
// task back immediately. Persisted to oversizeSkipFile so a restart doesn't
// reset the cooldown — without that, an agent restart turns the
// "30-min skip" into "1 expensive metadata fetch and re-abort."
var (
	oversizeSkipMu   sync.Mutex
	oversizeSkip     = map[int64]time.Time{}
	oversizeSkipFile string
)

// loadOversizeSkip reads the persisted skip set on startup. Anything
// already expired is dropped on load. Best-effort: any read/parse error
// just leaves the in-memory map empty.
func loadOversizeSkip(path string) {
	oversizeSkipFile = path
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var raw map[int64]time.Time
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	now := time.Now()
	oversizeSkipMu.Lock()
	for id, exp := range raw {
		if exp.After(now) {
			oversizeSkip[id] = exp
		}
	}
	oversizeSkipMu.Unlock()
}

// saveOversizeSkip writes the current map. Caller must hold oversizeSkipMu.
// Best-effort: a write failure is logged but doesn't fail the operation
// that triggered the save (the in-memory map is still authoritative for
// the rest of this process's lifetime).
func saveOversizeSkipLocked() {
	if oversizeSkipFile == "" {
		return
	}
	data, err := json.Marshal(oversizeSkip)
	if err != nil {
		return
	}
	if err := os.WriteFile(oversizeSkipFile, data, 0644); err != nil {
		log.Printf("oversizeSkip: persist failed: %v", err)
	}
}

func markOversize(requestID int64) {
	oversizeSkipMu.Lock()
	oversizeSkip[requestID] = time.Now().Add(oversizeSkipTTL)
	saveOversizeSkipLocked()
	oversizeSkipMu.Unlock()
}

func shouldSkipOversize(requestID int64) bool {
	oversizeSkipMu.Lock()
	defer oversizeSkipMu.Unlock()
	exp, ok := oversizeSkip[requestID]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(oversizeSkip, requestID)
		saveOversizeSkipLocked()
		return false
	}
	return true
}

func recordSuccess() {
	abortCounterMu.Lock()
	consecutiveAborts = 0
	abortCounterMu.Unlock()
}

func abortPauseRemaining() time.Duration {
	abortCounterMu.Lock()
	defer abortCounterMu.Unlock()
	if d := time.Until(abortPauseUntil); d > 0 {
		return d
	}
	return 0
}

// isAgentLocalError returns true for errors that are about the agent's own
// environment (port conflict, proxy reachability) rather than anything
// wrong with the torrent. These should release the lock without marking
// the request as failed for this agent.
//
// Disk-full is intentionally NOT in this list: it overlaps with
// isRuntimeDiskFullError and we want callers to check the disk-full path
// first so a single oversized torrent gets the per-request oversize
// cooldown instead of bumping the consecutive-abort counter.
func isAgentLocalError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "address already in use") ||
		strings.Contains(s, "Only one usage of each socket") ||
		strings.Contains(s, "SOCKS5") ||
		strings.Contains(s, "failed to create SOCKS5")
}

// isRuntimeDiskFullError catches "disk filled mid-download" errors that
// don't carry our ErrInsufficientDisk sentinel — typically OS-level write
// failures from anacrolix/torrent or the rar/par2 staging steps. These
// share a fix with the pre-flight oversize case (back off this specific
// request, don't pause the whole agent), so we route them through the
// same oversize-cooldown path.
func isRuntimeDiskFullError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "no space left") ||
		strings.Contains(s, "insufficient disk space") ||
		strings.Contains(s, "There is not enough space on the disk")
}

func setLivePhase(phase string) {
	liveStatusMu.Lock()
	liveStatus.Phase = phase
	liveStatusMu.Unlock()
}

func setLiveTask(title string, requestID int64) {
	liveStatusMu.Lock()
	liveStatus.TaskTitle = title
	liveStatus.RequestID = requestID
	liveStatusMu.Unlock()
}

func clearLiveStatus() {
	liveStatusMu.Lock()
	liveStatus = client.AgentLiveStatus{Phase: "idle"}
	liveStatusMu.Unlock()
}

// updateTaskProgress sets the per-task progress entry for aggregation.
func updateTaskProgress(requestID int64, fp *client.FileProgress) {
	taskProgressMu.Lock()
	if fp == nil {
		delete(taskProgress, requestID)
	} else {
		taskProgress[requestID] = fp
	}
	taskProgressMu.Unlock()
}

// aggregateLiveStatus rebuilds the live status from all active task progress entries.
// Returns the aggregate download speed + the two upload buckets in MB/s so
// the caller can feed them into a separate LiveSnapshot for the local UI
// (strings are what site posts need; floats are what sparkline / sidebar
// JS need). nzbUlMBps is NNTP POST traffic only; seedUlMBps is BT
// seed-back. The site graph and sidebar render these as two distinct
// upload lines so operators can see how much of their bandwidth is
// reaching Usenet vs sharing back to torrent peers.
func aggregateLiveStatus() (dlMBps, nzbUlMBps, seedUlMBps float64) {
	taskProgressMu.RLock()
	files := make([]client.FileProgress, 0, len(taskProgress))
	var dlSpeed, nzbUlSpeed, seedUlSpeed float64
	downloading, uploading := 0, 0
	for _, fp := range taskProgress {
		files = append(files, *fp)
		// FileProgress has separate Speed (download) and UpSpeed (upload)
		// fields — a torrent in "downloading" phase can also be seeding
		// pieces back, and an "uploading" file's throughput lives in
		// UpSpeed (Speed is download-only). Sum both unconditionally so
		// the aggregate honestly reflects what the network is doing,
		// regardless of phase. Phase counters are still used below to
		// pick the displayed status badge ("uploading" / "downloading").
		var ds, us float64
		fmt.Sscanf(fp.Speed, "%f", &ds)
		fmt.Sscanf(fp.UpSpeed, "%f", &us)
		dlSpeed += ds
		// Bucket upload bytes by phase. Phase == "uploading" is NNTP POST;
		// "seeding" is BT seed-back. Any other phase that happens to have
		// a non-zero UpSpeed is BT traffic from a torrent that's also
		// downloading (rare but possible during the swarm-warmup window) —
		// we bucket that as seed too since it's not headed to Usenet.
		if fp.Phase == "uploading" {
			nzbUlSpeed += us
		} else {
			seedUlSpeed += us
		}
		if fp.Phase == "downloading" {
			downloading++
		} else if fp.Phase == "uploading" {
			uploading++
		}
	}
	count := len(taskProgress)
	taskProgressMu.RUnlock()

	ulSpeed := nzbUlSpeed + seedUlSpeed

	liveStatusMu.Lock()
	liveStatus.Files = files
	if count == 0 {
		liveStatus.Phase = "idle"
		liveStatus.DownloadSpeed = ""
		liveStatus.UploadSpeed = ""
		liveStatus.NzbUploadSpeed = ""
		liveStatus.SeedUploadSpeed = ""
	} else {
		if uploading > 0 {
			liveStatus.Phase = "uploading"
		} else if downloading > 0 {
			liveStatus.Phase = "downloading"
		} else {
			liveStatus.Phase = "processing"
		}
		if dlSpeed > 0 {
			liveStatus.DownloadSpeed = fmt.Sprintf("%.2f MB/s", dlSpeed)
		} else {
			liveStatus.DownloadSpeed = ""
		}
		if ulSpeed > 0 {
			liveStatus.UploadSpeed = fmt.Sprintf("%.2f MB/s", ulSpeed)
		} else {
			liveStatus.UploadSpeed = ""
		}
		if nzbUlSpeed > 0 {
			liveStatus.NzbUploadSpeed = fmt.Sprintf("%.2f MB/s", nzbUlSpeed)
		} else {
			liveStatus.NzbUploadSpeed = ""
		}
		if seedUlSpeed > 0 {
			liveStatus.SeedUploadSpeed = fmt.Sprintf("%.2f MB/s", seedUlSpeed)
		} else {
			liveStatus.SeedUploadSpeed = ""
		}
	}
	liveStatusMu.Unlock()
	return dlSpeed, nzbUlSpeed, seedUlSpeed
}

// startStatusReporter posts the agent's live status to the site every 5 seconds.
func startStatusReporter(site client.Site, tempDir string) {
	ticker := time.NewTicker(5 * time.Second)
	var lastErrMsg string
	for range ticker.C {
		dlMBps, nzbUlMBps, seedUlMBps := aggregateLiveStatus()
		ulMBps := nzbUlMBps + seedUlMBps

		liveStatusMu.RLock()
		storage.GlobalState.RLock()
		snap := liveStatus
		snap.VPNStatus = storage.GlobalState.VPNStatus
		snap.PublicIP = storage.GlobalState.PublicIP
		storage.GlobalState.RUnlock()
		liveStatusMu.RUnlock()

		// Disk usage for dashboard display.
		if free, err := services.FreeDiskSpace(tempDir); err == nil {
			snap.DiskFreeGB = float64(free) / 1024 / 1024 / 1024
		}
		snap.DiskReservedGB = float64(services.TotalReservedBytes()) / 1024 / 1024 / 1024
		var diskTotalGB float64
		if total, err := services.TotalDiskSpace(tempDir); err == nil && total > 0 {
			diskTotalGB = float64(total) / 1024 / 1024 / 1024
		}

		// Publish the local-UI snapshot (typed floats, minimal fields) for
		// the SSE endpoint. Separate from the site post so a site outage
		// doesn't freeze the sidebar, and vice versa.
		services.SetLiveSnapshot(services.LiveSnapshot{
			Phase:          snap.Phase,
			TaskTitle:      snap.TaskTitle,
			DownloadMBps:   dlMBps,
			UploadMBps:     ulMBps,
			NzbUploadMBps:  nzbUlMBps,
			SeedUploadMBps: seedUlMBps,
			VPNStatus:      snap.VPNStatus,
			PublicIP:       snap.PublicIP,
			DiskFreeGB:     snap.DiskFreeGB,
			DiskReservedGB: snap.DiskReservedGB,
			DiskTotalGB:    diskTotalGB,
		})

		resp, err := site.PostStatus(snap)
		// Log the first occurrence of each new error — previously these were
		// swallowed silently, leaving no trace in agent logs when the site
		// saw us as offline. Dedup by error string so a sustained outage
		// doesn't spam every 5s.
		if err != nil {
			if msg := err.Error(); msg != lastErrMsg {
				log.Printf("status post failed: %v", err)
				lastErrMsg = msg
			}
		} else if lastErrMsg != "" {
			log.Printf("status post recovered")
			lastErrMsg = ""
		}
		if resp != nil && resp.CancelRequestID > 0 {
			jobName := fmt.Sprintf("request-%d", resp.CancelRequestID)
			if cancelFn, ok := storage.JobCancels.Load(jobName); ok {
				log.Printf("[skip] Cancelling task %s (request %d) by user request", jobName, resp.CancelRequestID)
				cancelFn.(context.CancelFunc)()
			}
		}
	}
}

// startWatchdog self-heals from sustained site-unreachability. Runs every
// 30s and consults site.LastOK():
//
//   - after watchdogVPNRestartAfter minutes: ask gluetun to reconnect (common
//     cause is the VPN tunnel going stale, which takes down the shared
//     netns's DNS resolver). Respects watchdogVPNCooldown so we don't hammer
//     the control server if the restart itself doesn't help.
//   - after watchdogHardExitAfter minutes: exit(1) so the supervisor
//     (docker restart: unless-stopped, systemd, etc.) can take over. This
//     only helps if the agent is running under a supervisor — bare
//     `./indexer-agent` users get the log and a dead process.
func startWatchdog(site client.Site) {
	const (
		watchdogVPNRestartAfter = 5 * time.Minute
		watchdogHardExitAfter   = 10 * time.Minute
		watchdogVPNCooldown     = 3 * time.Minute
	)
	ticker := time.NewTicker(30 * time.Second)
	var lastVPNRestart time.Time
	for range ticker.C {
		age := time.Since(site.LastOK())
		if age < watchdogVPNRestartAfter {
			continue
		}
		if age >= watchdogHardExitAfter {
			log.Printf("watchdog: no successful site contact for %v — exiting for supervisor to restart", age.Round(time.Second))
			os.Exit(1)
		}
		if time.Since(lastVPNRestart) < watchdogVPNCooldown {
			continue
		}
		log.Printf("watchdog: no site contact for %v — asking gluetun to reconnect", age.Round(time.Second))
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := services.RestartVPN(ctx)
		cancel()
		lastVPNRestart = time.Now()
		if err != nil {
			log.Printf("watchdog: VPN restart failed: %v", err)
		} else {
			log.Printf("watchdog: VPN restart requested")
		}
	}
}

// startSpeedLogger periodically logs download/upload speeds for all active tasks.
func startSpeedLogger() {
	ticker := time.NewTicker(30 * time.Second)
	for range ticker.C {
		taskProgressMu.RLock()
		if len(taskProgress) == 0 {
			taskProgressMu.RUnlock()
			continue
		}
		for rid, fp := range taskProgress {
			log.Printf("[speed] request=%d phase=%s speed=%s percent=%.1f%% title=%s",
				rid, fp.Phase, fp.Speed, fp.Percent, fp.Name)
		}
		taskProgressMu.RUnlock()
	}
}

// ── Upload serialization ────────────────────────────────────────────────────

// Upload serialization lives in services.UploadSlot so the offline
// pipeline can share the same mutex — NNTP connection limits apply
// regardless of whether the job came from site polling or a watch folder.

// ── Active task tracking (prevents double-dispatch) ─────────────────────────

var (
	activeTasks   = map[int64]bool{}
	activeTasksMu sync.Mutex
)

func claimTask(id int64) bool {
	activeTasksMu.Lock()
	defer activeTasksMu.Unlock()
	if activeTasks[id] {
		return false
	}
	activeTasks[id] = true
	return true
}

func releaseTask(id int64) {
	activeTasksMu.Lock()
	delete(activeTasks, id)
	activeTasksMu.Unlock()
}

func activeTaskCount() int {
	activeTasksMu.Lock()
	defer activeTasksMu.Unlock()
	return len(activeTasks)
}

// applyDirective runs a queued site directive (currently only write_config,
// which edits config.yml on disk) and acks the outcome. Errors are reported
// via ack so the site can surface them in the settings UI; the agent keeps
// polling regardless.
func applyDirective(cfg *config.Config, site client.Site, d client.Directive) {
	switch d.Kind {
	case "write_config":
		var p client.WriteConfigPayload
		if err := json.Unmarshal(d.Payload, &p); err != nil {
			site.AckDirective(d.ID, "bad payload: "+err.Error())
			return
		}
		if cfg.Layered == nil {
			site.AckDirective(d.ID, "layered config not initialised")
			return
		}
		written, err := cfg.Layered.WriteYml(p.Updates)
		if err != nil {
			site.AckDirective(d.ID, err.Error())
			return
		}
		// Re-derive effective values so env/yml changes take effect now.
		cfg.Refresh()
		log.Printf("write_config directive %d applied: %v", d.ID, written)
		site.AckDirective(d.ID, "")
	default:
		site.AckDirective(d.ID, "unknown directive kind: "+d.Kind)
	}
}

// runBackfill walks tempDir for backup-request-*.nzb files and re-submits
// each one to the site via /api/agent/backfill. The site does the same
// hash/dedup/insert/fulfill that Complete does. On success the local backup
// file is deleted; on failure it stays put so the next agent restart will
// retry. Designed to be safe to run repeatedly: dedup on the site means
// re-submitting an already-ingested NZB just fulfills the request again
// (idempotent).
func runBackfill(site client.Site, tempDir string) {
	matches, err := filepath.Glob(filepath.Join(tempDir, "backup-request-*.nzb"))
	if err != nil || len(matches) == 0 {
		return
	}
	log.Printf("Backfill: found %d local NZB backup(s) to re-submit", len(matches))

	var ok, fail int
	for _, path := range matches {
		base := filepath.Base(path)
		// Filename format: backup-request-<id>.nzb
		idStr := strings.TrimSuffix(strings.TrimPrefix(base, "backup-request-"), ".nzb")
		requestID, parseErr := strconv.ParseInt(idStr, 10, 64)
		if parseErr != nil {
			log.Printf("Backfill: skipping %s — cannot parse request ID: %v", base, parseErr)
			continue
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			log.Printf("Backfill: cannot read %s: %v", base, readErr)
			fail++
			continue
		}
		nzbID, subErr := site.Backfill(requestID, data, "")
		if subErr != nil {
			log.Printf("Backfill: request=%d failed: %v (will retry on next start)", requestID, subErr)
			fail++
			continue
		}
		log.Printf("Backfill: request=%d → nzb=%d, removing %s", requestID, nzbID, base)
		if rmErr := os.Remove(path); rmErr != nil {
			log.Printf("Backfill: warn — could not remove %s: %v", path, rmErr)
		}
		ok++
	}
	log.Printf("Backfill: complete — %d ok, %d failed", ok, fail)
}

// ── Main ────────────────────────────────────────────────────────────────────

func main() {
	// SECURITY: kill core dumps before anything else runs. A dump is
	// full process memory (NNTP password, agent token, env secrets);
	// if one is ever written into a content tree the upload stage
	// would post it to Usenet. See coredump_unix.go.
	disableCoreDumps()

	cfg := config.NewConfig()

	if cfg.SiteURL == "" || cfg.AgentToken == "" {
		log.Fatal("SITE_URL and AGENT_TOKEN must be set")
	}

	// Shutdown-aware root context. signal.NotifyContext wires SIGINT
	// (Ctrl-C in the foreground) and SIGTERM (docker stop, systemd
	// stop) to ctx cancellation; long-running goroutines that derive
	// from this context exit cleanly instead of being killed mid-
	// syscall. SetRootContext publishes it for services that don't
	// receive a ctx in their constructor (parallel to the site's
	// services.SetRootContext pattern). Calling stop() on shutdown
	// releases the signal handler so a second SIGTERM kills the
	// process immediately rather than being swallowed.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	services.SetRootContext(ctx)

	for _, dir := range []string{cfg.TempDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Fatalf("Failed to create directory %s: %v", dir, err)
		}
	}
	services.InitDiskLimit(cfg.MaxDiskUsageGB)

	storage.StateFile = filepath.Join(cfg.TempDir, "state.json")
	storage.LoadState()

	// Open the agent's local SQLite DB. Used by the local UI for
	// user-defined groups + watch folders + offline job history. A
	// failure here is non-fatal: the existing site-polling path doesn't
	// touch the DB, so we log and leave it nil. The local UI handlers
	// detect db==nil and return 503 on the DB-backed routes.
	db, err := storage.OpenDB(filepath.Join(cfg.TempDir, "agent.db"))
	if err != nil {
		log.Printf("WARNING: local DB failed to open (%v) — groups/watch-folders UI disabled", err)
		db = nil
	} else {
		// A crash or kill during processing would leave jobs stranded in
		// the 'processing' state forever. Put them back in the queue on
		// boot so the watcher/processor can pick them up again.
		if n, err := db.RequeueStuckJobs(); err == nil && n > 0 {
			log.Printf("requeued %d stuck offline job(s) from previous run", n)
		}
	}

	// Restore the per-request oversize cooldown set so a restart doesn't
	// turn the cooldown into a single expensive metadata fetch. Sibling of
	// state.json so the operator only has to know about TempDir.
	loadOversizeSkip(filepath.Join(cfg.TempDir, "oversize-skip.json"))

	// Remove any dl-* / screens-* / stage-* / dl-*.torrent left behind by
	// crashed/aborted tasks. Must run after LoadState so the resumable set
	// is known, and before the polling loop so free-space checks see an
	// accurate disk picture. The startup variant ALSO reclaims the
	// offline-/enc-/wrap-/offer-/*.7z families the periodic sweep leaves
	// alone — safe here because the offline processor + offer fulfiller
	// (which own those dirs) haven't been launched yet.
	services.SweepOrphanTempStartup(cfg.TempDir)

	// Re-sweep hourly so long-running agents that never restart still get
	// cleanup. The ticker variant skips entries newer than its minAge so
	// an active task's working dir is never removed mid-write. Jitter
	// the first tick by up to 5 minutes so multiple agents on the same
	// host (a common dev/test setup) don't all hit disk on the same
	// second every hour.
	go func() {
		jitter := time.Duration(mathrand.Int63n(int64(5 * time.Minute)))
		time.Sleep(jitter)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for range t.C {
			services.SweepOrphanDownloadsAged(cfg.TempDir, 30*time.Minute)
		}
	}()

	go services.MonitorNetworkConnection(cfg)

	// Secrets + optional local UI. Both are no-ops when the operator hasn't
	// enabled them (LOCAL_UI_PORT unset, secrets.yml missing). The local UI
	// needs the site client so its "Agent settings" panel can PUT web-tier
	// overrides — see services.LocalUI.SetSite.
	secrets := services.LoadSecrets()
	site := client.New(cfg)
	// Forward child-tool crashes (ffmpeg/par2/… signal kills) to the
	// site's agent_logs so they're visible in the admin dashboard
	// instead of only in container stdout. See services/exec_tool.go.
	services.ToolFailureSink = func(level, message string) { _ = site.PostLog(level, message) }
	localUI := services.StartLocalUI(cfg, secrets, db)
	if localUI != nil {
		localUI.SetSite(site)
	}
	pollInterval := time.Duration(cfg.PollInterval) * time.Second

	// On startup, clear any stale locks from a previous crash.
	if cleared, err := site.ClearMyLocks(); err != nil {
		log.Printf("Warning: could not clear stale locks: %v", err)
	} else if cleared > 0 {
		log.Printf("Cleared %d stale lock(s) from previous run", cleared)
	}

	// Also on startup: scan for backup-request-*.nzb files left by previous
	// failed Complete calls and re-submit them via /api/agent/backfill. Done
	// before the polling loop starts so the user doesn't waste bandwidth
	// re-downloading content the agent already has on disk.
	go runBackfill(site, cfg.TempDir)

	go startStatusReporter(site, cfg.TempDir)
	go startSpeedLogger()
	go startWatchdog(site)
	// Offline watcher + processor pair: watcher queues new files, processor
	// runs them through the pipeline and writes NZBs to OFFLINE_OUTPUT_DIR.
	// Both no-op when db is nil (DB failed to open on startup). All four
	// background services now derive from the shutdown-aware root ctx so
	// a SIGTERM unblocks them within one tick instead of being killed
	// mid-poll.
	go services.StartOfflineWatcher(ctx, db)
	go services.StartOfflineProcessor(ctx, cfg, db)
	// Site-groups sync: periodically pull the site's curated catalog of
	// posting groups into the local DB so watch folders can target them.
	// Preserves locally-edited groups (rows with source='local'); only
	// source='site' rows get upserted or reconciled.
	go services.StartSiteGroupsSync(ctx, site, db)

	// Offer-system sync (mig 241 + 242). No-op when OFFER_ENABLED is
	// false or the offer.yml file is missing — agent runs unchanged
	// for users who don't opt into the feature. A bad parse of
	// offer.yml is a fatal startup error so the operator sees the
	// misconfiguration immediately rather than after the first tick.
	if offerSync, err := services.NewOfferSyncService(cfg, site, db); err != nil {
		log.Fatalf("offer config: %v", err)
	} else if offerSync != nil {
		offerSync.Start(ctx)
		log.Printf("[offer] sync service started (interval=%dm config=%s)",
			cfg.OfferSyncIntervalMin, cfg.OfferConfigPath)
	}
	// Fulfill loop — paired with sync. Polls /api/agent/offer/
	// requests/pending and walks claim→deliver/fail. Phase 3a stubs
	// the deliver step; 3b plugs the upload pipeline in.
	if offerFulfill := services.NewOfferFulfillService(cfg, site, db); offerFulfill != nil {
		offerFulfill.Start(ctx)
		log.Printf("[offer] fulfill service started")
	}

	maxDL := cfg.MaxConcurrentDownloads
	if maxDL < 1 {
		maxDL = 1
	}
	log.Printf("Agent started — polling %s every %ds (max %d concurrent downloads)",
		cfg.SiteURL, cfg.PollInterval, maxDL)

	// Watchdog goroutine: every 60s, emit a snapshot of every in-flight
	// task with its current phase + last-known progress. Catches stuck-
	// in-disk-IO, stuck-in-subprocess, stuck-on-channel-send — anything
	// that doesn't go through a code path that times out itself. If
	// the next operator sees a task with the same phase + same percent
	// for several consecutive ticks, they know exactly which task to
	// dig into without paging through 10k lines of speed logs.
	go runWatchdog(ctx, 60*time.Second)

	const minFreeSpaceGB = 1 // don't accept new tasks below this threshold
	// lastReason dedupes repeated poll-error log lines in the single-
	// poller world we live in today. Single-goroutine access only —
	// if a future change spawns parallel pollers, promote this to a
	// struct field or atomic.Pointer[string] before they share it,
	// or you get a data race that the Go runtime will yell about
	// the first time -race is used in CI.
	var lastReason string

	for {
		// Honour SIGTERM/SIGINT — bail out at the top of every iteration
		// so a graceful shutdown takes at most one poll iteration to
		// notice instead of waiting for the next sleep to expire.
		if ctx.Err() != nil {
			log.Printf("Shutdown signal received — exiting poll loop")
			return
		}
		// Fetch remote config from server (falls back to local env vars).
		// We do this BEFORE the capacity check so a max_concurrent change
		// from the dashboard takes effect within one poll interval instead
		// of waiting for the next restart.
		cpuThreshold := cfg.CPUMaxPercent
		var remoteCfg *client.RemoteConfig
		if rc, err := site.GetConfig(); err == nil {
			remoteCfg = rc
			if rc.CpuMaxPercent > 0 {
				cpuThreshold = float64(rc.CpuMaxPercent)
			}
			// Apply remote max-concurrent override. 0 means "use local default".
			if rc.MaxConcurrent > 0 && rc.MaxConcurrent != maxDL {
				log.Printf("max concurrent downloads updated by site: %d -> %d", maxDL, rc.MaxConcurrent)
				maxDL = rc.MaxConcurrent
			}
			// Apply remote disk limit override.
			if rc.MaxDiskUsageGB > 0 {
				services.InitDiskLimit(rc.MaxDiskUsageGB)
			}
			// Operator-configured blocklist for the post-download sweep
			// (migration 215). Non-empty REPLACES DefaultBlockedExtensions
			// — typically used to allow .iso through when remux_bluray
			// is on. Published every poll so dashboard edits take effect
			// within one cycle.
			services.SetRemoteBannedExtensions(rc.BannedExtensions)
			// Layered web-override tier: only applied when the site provided
			// an explicit WebOverrides map. Empty/nil leaves existing tier in
			// place rather than clobbering it on every poll.
			if rc.WebOverrides != nil && cfg.Layered != nil {
				cfg.Layered.ApplyWeb(rc.WebOverrides)
				cfg.Refresh()
			}
		}

		// Best-effort: post our local config snapshot so the settings UI can
		// render state badges, and drain any pending write_config directives
		// the user queued from the site. Failures are non-fatal — the agent
		// keeps polling for work regardless.
		if cfg.Layered != nil {
			gpuInfo, upscaleModels := services.GPUCapabilities()
			extras := []config.ReportExtra{
				config.WithPrivateTrackers(secrets.Has()),
				config.WithLocalUIURL(localUI.URL()),
				config.WithGpuInfo(gpuInfo),
				config.WithUpscaleModels(upscaleModels),
			}
			if err := site.PostLocalConfig(cfg.Layered.Report(extras...)); err != nil {
				// Only log once per reason to avoid spamming on older sites
				// that don't yet implement /api/agent/local-config.
				if lastReason != "local_config_post" {
					log.Printf("local-config post: %v (ok if site not yet upgraded)", err)
					lastReason = "local_config_post"
				}
			}
			if dirs, err := site.FetchDirectives(); err == nil {
				for _, d := range dirs {
					applyDirective(cfg, site, d)
				}
			}
		}

		// Operator-toggled pause via the local UI (storage.GlobalState.
		// QueuePaused). When set, the poll loop skips claim attempts so
		// no new work starts; in-flight tasks keep running. The flag
		// flips via /mirror/pause on the local UI.
		storage.GlobalState.RLock()
		paused := storage.GlobalState.QueuePaused
		storage.GlobalState.RUnlock()
		if paused {
			if lastReason != "queue_paused" {
				log.Printf("Queue paused via local UI — not claiming new tasks")
				lastReason = "queue_paused"
			}
			time.Sleep(pollInterval)
			continue
		}
		if lastReason == "queue_paused" {
			lastReason = ""
		}

		// Only poll for new work if we have capacity.
		if activeTaskCount() >= maxDL {
			time.Sleep(pollInterval)
			continue
		}

		// Skip polling if CPU usage is too high.
		if cpuThreshold > 0 {
			if cpuPct, err := services.CPUUsagePercent(); err == nil && cpuPct > cpuThreshold {
				if lastReason != "cpu_high" {
					log.Printf("CPU usage %.0f%% > %.0f%% threshold — pausing new tasks", cpuPct, cpuThreshold)
					site.PostLog("info", fmt.Sprintf("CPU usage %.0f%% exceeds %.0f%% threshold — pausing new tasks", cpuPct, cpuThreshold))
					lastReason = "cpu_high"
				}
				time.Sleep(pollInterval)
				continue
			}
		}

		// Skip polling if disk space (minus reservations for in-flight tasks) is too low.
		if effective, err := services.FreeDiskAfterReservations(cfg.TempDir); err == nil {
			effectiveGB := float64(effective) / 1024 / 1024 / 1024
			if effectiveGB < minFreeSpaceGB {
				if lastReason != "disk_low" {
					reserved := float64(services.TotalReservedBytes()) / 1024 / 1024 / 1024
					log.Printf("Low disk space: %.1f GB effective free (%.1f GB reserved by active tasks), need %d GB — waiting",
						effectiveGB, reserved, minFreeSpaceGB)
					lastReason = "disk_low"
				}
				time.Sleep(pollInterval)
				continue
			}
		}

		// Self-pause after repeated agent-local errors (see recordAbort).
		if wait := abortPauseRemaining(); wait > 0 {
			if lastReason != "abort_pause" {
				log.Printf("Skipping task polls for %s (self-pause after repeated local errors)", wait.Round(time.Second))
				site.PostLog("warn", fmt.Sprintf("Self-pause: %s remaining after repeated agent-local errors", wait.Round(time.Second)))
				lastReason = "abort_pause"
			}
			time.Sleep(pollInterval)
			continue
		}
		if lastReason == "abort_pause" {
			lastReason = ""
		}

		result, err := site.Poll()
		if err != nil {
			// Upgrade required: this agent's protocol is below the site's
			// minimum. No amount of retrying will fix this — log loudly,
			// tell the user, and back off so we're not spamming a clearly
			// broken endpoint.
			if ue, ok := client.IsUpgradeRequired(err); ok {
				if lastReason != "upgrade" {
					log.Printf("AGENT UPGRADE REQUIRED: %s", ue.Error())
					site.PostLog("error", "Agent upgrade required: "+ue.Error())
					lastReason = "upgrade"
				}
				time.Sleep(10 * time.Minute)
				continue
			}
			// Maintenance: sleep the ETA + a small buffer, don't spam logs.
			if me, ok := client.IsMaintenanceError(err); ok {
				wait := time.Duration(me.Info.ETASeconds+15) * time.Second
				if wait < 30*time.Second {
					wait = 30 * time.Second
				}
				if wait > 10*time.Minute {
					wait = 10 * time.Minute
				}
				if lastReason != "maintenance" {
					log.Printf("Site in maintenance: %s — waiting %s", me.Info.Reason, wait.Round(time.Second))
					lastReason = "maintenance"
				}
				time.Sleep(wait)
				continue
			}
			log.Printf("Poll error: %v", err)
			site.PostLog("error", "Poll error: "+err.Error())
			time.Sleep(pollInterval)
			continue
		}
		if lastReason == "maintenance" {
			lastReason = ""
		}

		if result.Command == "stop" {
			log.Printf("Received stop command — idling")
			time.Sleep(pollInterval)
			continue
		}

		// "stop_after_current" is a graceful shutdown: the user clicked
		// "Finish & Stop" in the dashboard, so we drain any in-flight
		// downloads and then exit cleanly. Until activeTaskCount() drops
		// to zero, behave exactly like "stop" — accept no new work but
		// keep the process alive so existing goroutines can finish.
		//
		// Startup guard (stopGuardDuration below): if we receive
		// stop_after_current within the guard window of process start
		// AND we have no active tasks, refuse to exit and continue
		// polling instead. Without this guard, an operator-set "stop"
		// that never got cleared on the site keeps the agent in a
		// docker restart loop — each new process polls once, sees
		// stop, exits cleanly, docker restart-unless-stopped kicks
		// in, the cycle repeats every ~60s indefinitely. We caught
		// this in production at RestartCount=290 across ~5 hours of
		// agent downtime where the operator thought the agent had
		// crashed. The guard window of 10 minutes is long enough for
		// a deliberate "Finish & Stop" click on an idle agent to
		// eventually take effect (after the window expires it'll
		// honour the stop normally), but short enough that the
		// operator clearing the stop on the site is the dominant
		// recovery path.
		const stopGuardDuration = 10 * time.Minute
		if result.Command == "stop_after_current" {
			active := activeTaskCount()
			if active == 0 {
				if uptime := time.Since(processStart); uptime < stopGuardDuration {
					remaining := (stopGuardDuration - uptime).Round(time.Second)
					log.Printf("stop_after_current: received within %s of startup with no active tasks — refusing to exit (would risk docker-restart loop). Clear the stop on the site to keep the agent idle, or wait %s for the guard window to expire and exit will proceed.",
						uptime.Round(time.Second), remaining)
					time.Sleep(pollInterval)
					continue
				}
				log.Printf("stop_after_current: no active tasks — shutting down (uptime %s past guard window)", time.Since(processStart).Round(time.Second))
				site.PostLog("info", "Graceful shutdown: no active tasks remaining")
				return
			}
			log.Printf("stop_after_current: waiting for %d active task(s) to finish", active)
			time.Sleep(pollInterval)
			continue
		}

		if result.Task == nil {
			if result.Reason != "" {
				log.Printf("No task: %s", result.Reason)
				// Log to site only occasionally to avoid spam (use a simple throttle).
				if lastReason != result.Reason {
					site.PostLog("info", "Poll: "+result.Reason)
					lastReason = result.Reason
				}
			}
			time.Sleep(pollInterval)
			continue
		}
		lastReason = "" // reset when we get a task

		task := result.Task
		// Prevent double-dispatch if site returns same task.
		if !claimTask(task.RequestID) {
			time.Sleep(pollInterval)
			continue
		}

		log.Printf("Received task: request=%d title=%q hash=%s", task.RequestID, task.Title, task.InfoHash)
		site.PostLog("info", fmt.Sprintf("Picked up request #%d: %s", task.RequestID, task.Title))
		go func(t *client.AgentTask, rc *client.RemoteConfig) {
			defer releaseTask(t.RequestID)
			processTask(cfg, site, t, rc)
			// Post-task cleanup: drop the GlobalState entry for this
			// job (so the orphan sweep's keep[] no longer protects
			// its working dir), then run an immediate sweep to
			// reclaim anything left behind by this task OR earlier
			// crashed/aborted ones. Catches the "files accumulate
			// over time" leak — without RemoveJob, a failed task's
			// entry would shield its dir forever and the hourly
			// sweep would skip it. minAge=0 here is safe because
			// the only protected dirs are the ones still in
			// GlobalState (any in-flight task) — this task's entry
			// was just removed so its leftovers ARE eligible.
			jobName := fmt.Sprintf("request-%d", t.RequestID)
			storage.RemoveJob(jobName)
			services.SweepOrphanDownloads(cfg.TempDir)
		}(task, remoteCfg)

		time.Sleep(2 * time.Second) // brief pause before polling for next
	}
}

// Blocklist enforcement lives in services.RemoveBlockedFiles — both the
// online pipeline (this file) and the offline pipeline call it with a
// per-job effective list so a music group can allow .iso, a video group
// can ban .html, etc. See services/blocklist.go.

// findVideoFiles moved to services.FindVideoFiles so the offline pipeline
// shares the same list of extensions — divergence would mean screenshots
// work on one path and not the other.

// dirSize walks a directory and sums file sizes.
func dirSize(path string) int64 {
	var total int64
	filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

func processTask(cfg *config.Config, site client.Site, task *client.AgentTask, remoteCfg *client.RemoteConfig) {
	jobName := fmt.Sprintf("request-%d", task.RequestID)

	// If we just rejected this exact request for insufficient disk, the
	// site may hand it right back. Fail fast: releasing the lock without
	// fetching metadata saves a 2-minute DHT round-trip per re-dispatch.
	if shouldSkipOversize(task.RequestID) {
		log.Printf("[%d] Skipping re-dispatched oversized task (still within cooldown)", task.RequestID)
		if err := site.Complete(client.CompleteResult{
			LockID:     task.LockID,
			RequestID:  task.RequestID,
			Status:     "aborted",
			FailReason: "too large for this agent (cooldown)",
		}); err != nil {
			log.Printf("[%d] site.Complete (oversize cooldown abort) failed: %v", task.RequestID, err)
		}
		return
	}

	// Parent the per-task ctx on the shutdown-aware root context so
	// SIGTERM (docker stop / systemd stop) cancels every in-flight
	// task in addition to the explicit /mirror/cancel path. The
	// pipeline checkpoints (ctx.Err() in nntpWorker, runSeedPhase,
	// downloadAndWaitSeed, etc) honour the same ctx and exit
	// cleanly within the shutdown budget instead of being killed
	// mid-syscall.
	ctx, cancel := context.WithCancel(services.RootContext())
	storage.JobCancels.Store(jobName, cancel)
	defer storage.JobCancels.Delete(jobName)
	defer cancel()

	// Clean up per-task progress and disk reservation on exit.
	defer updateTaskProgress(task.RequestID, nil)
	defer services.ReleaseDisk(jobName)

	// Seed an initial JobState entry so the local UI /mirror page
	// renders this task immediately + carries the release title for
	// display (jobName is `request-N`; Title is the human-legible
	// label from the AgentTask payload).
	storage.UpdateState(jobName, "claimed", "", 0)
	storage.SetJobTitle(jobName, task.Title)

	reportProgress := func(phase, details string) {
		storage.UpdateState(jobName, phase, details, 0)
		_ = site.ReportProgress(task.LockID, phase+": "+details, "", nil)
	}

	fail := func(phase, msg string, err error) {
		reason := msg + ": " + err.Error()
		log.Printf("[%d] %s: %v", task.RequestID, phase, err)
		// Pre-flight disk shortfall is per-torrent, not environmental: a
		// 90 GB torrent not fitting tells us nothing about whether a 5 GB
		// one will. Abort the task back to the pool but skip the
		// self-pause counter, and remember the request ID briefly so we
		// don't re-fetch its metadata if the site hands it right back.
		if errors.Is(err, services.ErrInsufficientDisk) || isRuntimeDiskFullError(err) {
			reportProgress("Aborted", reason)
			// If it was our pre-flight that fired (the typed variant),
			// pass the discovered torrent size to the site so future
			// polls from smaller-disk agents skip this request without
			// spending 2 minutes re-resolving metadata. OS-level ENOSPC
			// doesn't carry a size, so in that branch we stay silent.
			var total int64
			var ds *services.DiskShortfallError
			if errors.As(err, &ds) {
				total = ds.TorrentBytes
			}
			if err := site.Complete(client.CompleteResult{
				LockID:         task.LockID,
				RequestID:      task.RequestID,
				Status:         "aborted",
				FailReason:     reason,
				TotalSizeBytes: total,
			}); err != nil {
				log.Printf("[%d] site.Complete (disk-shortfall abort) failed: %v", task.RequestID, err)
			}
			markOversize(task.RequestID)
			return
		}
		// Agent-local errors (port bind, proxy down) aren't the torrent's
		// fault — release the lock without burning a cooldown, and
		// increment the self-pause counter so repeated failures
		// eventually back off instead of churning the whole queue.
		if isAgentLocalError(err) {
			reportProgress("Aborted", reason)
			if cerr := site.Complete(client.CompleteResult{
				LockID:     task.LockID,
				RequestID:  task.RequestID,
				Status:     "aborted",
				FailReason: reason,
			}); cerr != nil {
				log.Printf("[%d] site.Complete (agent-local abort) failed: %v", task.RequestID, cerr)
			}
			recordAbort(reason)
			return
		}
		reportProgress("Failed", reason)
		if cerr := site.Complete(client.CompleteResult{
			LockID:     task.LockID,
			RequestID:  task.RequestID,
			Status:     "failed",
			FailReason: reason,
		}); cerr != nil {
			log.Printf("[%d] site.Complete (task-failed) failed: %v", task.RequestID, cerr)
		}
	}

	// Per-task progress callback for live dashboard.
	var lastLockUpdate time.Time
	progressCb := func(downMBs float64, upMBs float64, percent float64, phase string, peers int, totalBytes int64, transferredBytes int64, etaSeconds float64, warnings []services.ProgressWarning) {
		// The "headline" speed for a single-string display is the dominant
		// direction for the phase: upload speed during seeding/uploading,
		// download speed otherwise.
		headline := downMBs
		if phase == "seeding" || phase == "uploading" {
			headline = upMBs
		}
		// Translate the service-layer warning shape into the client/site
		// shape (same fields, decouples the two packages).
		var clientWarnings []client.LockWarning
		for _, w := range warnings {
			clientWarnings = append(clientWarnings, client.LockWarning{
				Kind:      w.Kind,
				Label:     w.Label,
				Icon:      w.Icon,
				ExpiresAt: w.ExpiresAt,
			})
		}
		fp := &client.FileProgress{
			Name:        task.Title,
			Size:        totalBytes,
			Transferred: transferredBytes,
			Percent:     percent,
			Speed:       fmt.Sprintf("%.2f MB/s", downMBs),
			Phase:       phase,
			Peers:       peers,
			Warnings:    clientWarnings,
		}
		if upMBs > 0 || phase == "seeding" || phase == "uploading" {
			fp.UpSpeed = fmt.Sprintf("%.2f MB/s", upMBs)
		}
		updateTaskProgress(task.RequestID, fp)
		// Throttle DB lock updates to every 10 seconds so the admin Active
		// Tasks table stays current without hammering the site API. When
		// warnings are active we always push (so the countdown doesn't go
		// stale up to 10s late) but cap that to once per second.
		shouldPush := time.Since(lastLockUpdate) >= 10*time.Second
		if !shouldPush && len(clientWarnings) > 0 && time.Since(lastLockUpdate) >= 1*time.Second {
			shouldPush = true
		}
		if shouldPush {
			lastLockUpdate = time.Now()
			label := "Downloading"
			if phase == "uploading" {
				label = "Uploading"
			} else if phase == "seeding" {
				label = "Seeding"
			}
			progress := fmt.Sprintf("%s: %.0f%% (%.1f MB/s)", label, percent, headline)
			if peers > 0 {
				progress += fmt.Sprintf(" [%d peers]", peers)
			}
			if etaSeconds > 0 {
				progress += " - ETA " + utils.FormatETA(etaSeconds)
			}
			_ = site.ReportProgress(task.LockID, progress, fmt.Sprintf("%.1f MB/s", headline), clientWarnings)
		}
	}

	rid := task.RequestID
	taskStart := time.Now()
	log.Printf("[%d] ── Pipeline start: %s ──", rid, task.Title)
	reportProgress("Initializing", "Preparing pipeline...")

	// ── 1. Download torrent (or resume from previous download) ────────────
	// Normalize the infohash for anacrolix's strict-case base32 parser.
	// Site sometimes hands us lowercase base32 hashes (32-char magnet
	// shape) which fail with "error decoding xt: illegal base32 data
	// at input byte 0". Hex (40-char) hashes pass through unchanged.
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s", services.NormalizeInfoHash(task.InfoHash))

	// Check for existing download from a previous interrupted run.
	var downloadedPath string
	// session is non-nil only on the fresh-download path; the resume-from-
	// disk branch can't seed (the torrent client died with the previous
	// run), and that's accepted — the rare interrupted-run case isn't
	// worth re-adding the torrent just to seed.
	var session *services.TorrentSession
	storage.GlobalState.RLock()
	if prev, ok := storage.GlobalState.Jobs[jobName]; ok && prev.DownloadedPath != "" {
		if info, err := os.Stat(prev.DownloadedPath); err == nil && info.IsDir() {
			// Validate that the prior download is structurally usable
			// before trusting it. A previous run that died mid-write
			// can leave an empty dir, a symlink, or a stub metadata
			// file — none of which we want to feed into the rest of
			// the pipeline. ValidatePartialDownload returns false if
			// the dir looks broken; we then fall through to fresh
			// download. AgentTask doesn't carry the expected torrent
			// size today so we pass 0 to skip the size-range check —
			// the file-count and symlink checks alone catch the
			// known-bad shapes.
			if services.ValidatePartialDownload(prev.DownloadedPath, 0) {
				downloadedPath = prev.DownloadedPath
				log.Printf("[%d] Resuming from previous download: %s", task.RequestID, downloadedPath)
			} else {
				log.Printf("[%d] Resume: prior download at %s failed validation — starting fresh", task.RequestID, prev.DownloadedPath)
			}
		}
	}
	storage.GlobalState.RUnlock()

	// AGENT_MODE=watch_folder skips the internal torrent client. Drop a
	// magnet (or .torrent for private uploads) into WATCH_DIR and wait
	// for the user's BT client to deposit the completed download into
	// DONE_DIR/<info_hash>/. Heartbeat /api/agent/progress while waiting
	// so the site doesn't reap the lock as stale during a multi-hour
	// download. Once the dir lands, fall through to the normal post-
	// download pipeline (PAR2/screenshots/upload) — that code doesn't
	// care how the bytes got there.
	if downloadedPath == "" && cfg.AgentMode == "watch_folder" {
		log.Printf("[%d] watch_folder: handing off to user's BT client (watch=%s done=%s)",
			task.RequestID, cfg.WatchDir, cfg.DoneDir)
		reportProgress("Handing off", "Writing magnet to watch folder; waiting for user's BT client...")

		spec := services.HandoffSpec{
			InfoHash: task.InfoHash,
			Magnet:   magnet,
		}
		// Private torrents: prefer the .torrent bytes the site provides
		// over the magnet so the user's client doesn't hit DHT and leak
		// the hash off the private tracker.
		if task.Private && task.TorrentFileURL != "" {
			if blob, err := site.FetchTorrentFile(task.TorrentFileURL); err == nil && len(blob) > 0 {
				spec.TorrentBytes = blob
			} else if err != nil {
				fail("Handoff", "fetch private .torrent", err)
				return
			}
		}
		handoffFile, err := services.WriteHandoff(cfg.WatchDir, spec)
		if err != nil {
			fail("Handoff", "write handoff", err)
			return
		}
		log.Printf("[%d] watch_folder: dropped %s", task.RequestID, handoffFile)

		timeout := time.Duration(cfg.WatchHandoffTimeoutMin) * time.Minute
		if timeout <= 0 {
			timeout = 6 * time.Hour
		}
		donePath, err := services.WaitForHandoffDone(ctx, cfg.DoneDir, task.InfoHash,
			services.HandoffDoneOpts{
				Timeout: timeout,
				OnTick: func(elapsed time.Duration) {
					// Site reaps locks with no progress for ~10 min; ticking
					// every 30s of elapsed time keeps the lock warm with
					// margin. Using the OnTick callback rather than a
					// separate goroutine keeps the pulse synced with the
					// poll cadence (avoids racing).
					if elapsed > 0 && int(elapsed.Seconds())%30 < 15 {
						_ = site.ReportProgress(task.LockID,
							fmt.Sprintf("Waiting on user's BT client (%s elapsed)", utils.FormatETA(elapsed.Seconds())),
							"", nil)
					}
				},
			})
		if err != nil {
			services.CleanupHandoff(cfg.WatchDir, task.InfoHash)
			if errors.Is(err, context.Canceled) || ctx.Err() != nil {
				log.Printf("[%d] watch_folder: cancelled by user", task.RequestID)
				reportProgress("Skipped", "Task skipped by user")
				if cerr := site.Complete(client.CompleteResult{
					LockID:     task.LockID,
					RequestID:  task.RequestID,
					Status:     "failed",
					FailReason: "Task skipped by user",
				}); cerr != nil {
					log.Printf("[%d] site.Complete (watch_folder user-cancel) failed: %v", task.RequestID, cerr)
				}
				return
			}
			// Timeout / read errors / unexpected file shapes — release the
			// lock as aborted (non-punitive) so another agent or a retry
			// can take it.
			log.Printf("[%d] watch_folder: %v", task.RequestID, err)
			reportProgress("Aborted", err.Error())
			if cerr := site.Complete(client.CompleteResult{
				LockID:     task.LockID,
				RequestID:  task.RequestID,
				Status:     "aborted",
				FailReason: err.Error(),
			}); cerr != nil {
				log.Printf("[%d] site.Complete (watch_folder abort) failed: %v", task.RequestID, cerr)
			}
			return
		}
		downloadedPath = donePath
		// Stamp it on the job state so a mid-pipeline crash can resume
		// from the same path on the next run instead of re-handing off.
		storage.UpdateJobMeta(jobName, downloadedPath, "", "")
		log.Printf("[%d] watch_folder: completion detected at %s", task.RequestID, downloadedPath)
		// Best-effort handoff cleanup. The user's BT client should have
		// either consumed or moved the .magnet/.torrent by now; leaving
		// it would just clutter the watch folder.
		services.CleanupHandoff(cfg.WatchDir, task.InfoHash)
	}

	if downloadedPath == "" {
		log.Printf("[%d] Downloading: %s", task.RequestID, task.Title)
		reportProgress("Downloading", "Fetching torrent metadata...")

		// Set per-task callback for download progress.
		services.SetProgressCallbackForJob(jobName, progressCb)

		dlOpts := &services.DownloadOpts{
			SlowThresholdMBs:    cfg.SlowSpeedThresholdMBs,
			SlowTimeoutMins:     cfg.SlowSpeedTimeoutMins,
			LowPeersThreshold:   -1, // disabled by default
			LowPeersTimeoutMins: 0,
			IsBoosted:           task.BoostCount > 0,
		}
		// Override from remote config if available.
		if remoteCfg != nil {
			if remoteCfg.SlowSpeedTimeout > 0 {
				dlOpts.SlowThresholdMBs = remoteCfg.SlowSpeedThreshold
				dlOpts.SlowTimeoutMins = remoteCfg.SlowSpeedTimeout
			}
			if remoteCfg.LowPeersTimeout > 0 {
				dlOpts.LowPeersThreshold = remoteCfg.LowPeersThreshold
				dlOpts.LowPeersTimeoutMins = remoteCfg.LowPeersTimeout
			}
		}
		var err error
		if task.Private && task.TorrentFileURL != "" {
			// Private upload: the site has the .torrent bytes. Fetch them
			// over HTTPS and run the file-based download path so we never
			// hit DHT with this info hash (which would leak the release
			// off the user's private tracker).
			log.Printf("[%d] Fetching private .torrent from %s", task.RequestID, task.TorrentFileURL)
			var blob []byte
			blob, err = site.FetchTorrentFile(task.TorrentFileURL)
			if err == nil && len(blob) > 0 {
				session, err = services.DownloadPrivateTorrentBytes(ctx, blob, cfg, jobName, dlOpts)
			}
		} else {
			// Public torrent: try the site's metadata cache first so we
			// can skip the 2-minute DHT round-trip when the prefetch
			// worker has already resolved this hash. Cache miss (404 →
			// nil, nil) silently falls through to DownloadMagnet, so
			// older sites without the cache endpoint still work.
			var cached []byte
			if task.InfoHash != "" {
				if c, cerr := site.FetchCachedTorrentByInfoHash(task.InfoHash); cerr == nil {
					cached = c
				}
				// A real fetch error (not 404) is logged but doesn't
				// abort — DHT fallback below still has a shot.
			}
			if len(cached) > 0 {
				log.Printf("[%d] Cache hit (%d bytes) — skipping DHT", task.RequestID, len(cached))
				session, err = services.DownloadCachedTorrentBytes(ctx, cached, cfg, jobName, dlOpts)
			} else {
				session, err = services.DownloadMagnet(ctx, magnet, cfg, jobName, dlOpts)
			}
		}

		services.ClearProgressCallbackForJob(jobName)

		if err != nil {
			if errors.Is(err, services.ErrSlowDownload) {
				log.Printf("[%d] Slow download rejected — skipping", task.RequestID)
				reportProgress("Failed", "Download too slow — skipping")
				if cerr := site.Complete(client.CompleteResult{
					LockID:     task.LockID,
					RequestID:  task.RequestID,
					Status:     "failed",
					FailReason: "Download too slow — skipping",
				}); cerr != nil {
					log.Printf("[%d] site.Complete (slow-download skip) failed: %v", task.RequestID, cerr)
				}
			} else if ctx.Err() != nil {
				log.Printf("[%d] Task skipped by user", task.RequestID)
				reportProgress("Skipped", "Task skipped by user")
				if cerr := site.Complete(client.CompleteResult{
					LockID:     task.LockID,
					RequestID:  task.RequestID,
					Status:     "failed",
					FailReason: "Task skipped by user",
				}); cerr != nil {
					log.Printf("[%d] site.Complete (user-skipped download) failed: %v", task.RequestID, cerr)
				}
			} else {
				fail("Download", "Download error", err)
			}
			return
		}
		downloadedPath = session.Path
		storage.UpdateJobMeta(jobName, downloadedPath, "", "")
		log.Printf("[%d] Download complete: %s", task.RequestID, downloadedPath)
	}
	// LIFO defers: Close runs first (stops the torrent client, releases
	// file handles), then RemoveAll deletes the data dir. Reversing this
	// would try to delete the data dir while the torrent client still has
	// the files open — fine on Linux but fails on Windows.
	//
	// In watch_folder mode the bytes belong to the user's BT client (it's
	// still seeding from that path); session is nil and we MUST NOT
	// delete the data dir or we'd nuke the user's own torrent storage.
	if cfg.AgentMode != "watch_folder" {
		defer os.RemoveAll(downloadedPath)
		defer session.Close()
	}

	// ── 1b. Optional Bluray remux ─────────────────────────────────────────
	// When the request opted into remux (remux_option = "remux" or
	// "both") AND this agent advertised remux_bluray=true, run the
	// mkvmerge stream-copy pipeline before any other post-processing.
	// The dispatcher already gates incapable agents from receiving
	// remux-required jobs (migration 214), so reaching this code means
	// the operator has opted in. RunRemux is a no-op for
	// remux_option=none/"" so the call is safe to leave unconditional.
	//
	// We run remux BEFORE the blocked-file sweep because Bluray rips
	// often carry .iso payloads — those would be deleted by the
	// blocklist before we got a chance to extract MKV(s) from them.
	// (ISO content currently bails inside RunRemux with a clear
	// reason; full MakeMKV support is a future image upgrade.)
	if task.RemuxOption != "" && task.RemuxOption != "none" {
		log.Printf("[%d] Step 1b: Bluray remux (mode=%s)...", rid, task.RemuxOption)
		reportProgress("Remuxing", "Running mkvmerge stream-copy on Bluray content...")
		if res, err := services.RunRemux(ctx, downloadedPath, task.RemuxOption); err != nil {
			// Non-fatal: log the failure and let the rest of the
			// pipeline run on whatever the download left behind.
			// Operator can investigate via the agent log.
			log.Printf("[%d] Remux failed (continuing without): %v", rid, err)
		} else if res.Skipped {
			log.Printf("[%d] Remux skipped: %s", rid, res.Reason)
		} else {
			log.Printf("[%d] Remux emitted %d MKV(s) under %s/remux/", rid, len(res.EmittedMKVs), downloadedPath)
		}
	}

	// ── Step 1c. AI upscale ────────────────────────────────────────────────
	// Runs after remux (the upscale path operates on whatever video the
	// previous step left behind — original or freshly-remuxed MKVs).
	// Dispatcher gates this on agent_config.ai_upscale, so a non-capable
	// agent never sees a non-empty UpscaleOption.
	if task.UpscaleOption != "" {
		log.Printf("[%d] Step 1c: AI upscale (model=%s)...", rid, task.UpscaleOption)
		reportProgress("Upscaling", "Running GPU upscale pipeline...")
		if res, err := services.RunUpscale(ctx, downloadedPath, task.UpscaleOption); err != nil {
			// Non-fatal: log + continue, mirroring the remux path. Phase
			// 3's catalog logic decides whether to mark the resulting
			// release as a failed-upscale variant.
			log.Printf("[%d] Upscale failed (continuing without): %v", rid, err)
		} else if res.Skipped {
			log.Printf("[%d] Upscale skipped: %s", rid, res.Reason)
		} else {
			log.Printf("[%d] Upscale emitted %d file(s) under %s/upscale/", rid, len(res.EmittedFiles), downloadedPath)
		}
	}

	// Remove any dangerous file types before processing. Uses
	// services.OnlineBlocklist() so the operator-configured override
	// from /agent/<id> (migration 215) takes effect when set — e.g.
	// dropping iso so Bluray remux content passes through. Empty
	// override falls back to DefaultBlockedExtensions.
	n, blockedByExt := services.RemoveBlockedFiles(downloadedPath, services.OnlineBlocklist())
	if n > 0 {
		log.Printf("[%d] Removed %d blocked file(s) from download (%s, using %s)",
			task.RequestID, n, services.FormatExtCounts(blockedByExt), services.OnlineBlocklistSource())
	}

	// If the blocklist sweep left nothing behind, the rest of the
	// pipeline will fail confusingly six steps from now ("no files to
	// upload in stage-XXX") because staging copies an empty source.
	// Most common cause: DVD_ISO / single-iso releases hitting the
	// default blocklist that includes .iso. Abort cleanly so the
	// failure surface is informative — the operator can either add
	// the blocked extensions to their /agent/<id> override list or
	// the dispatcher can route the request to a different agent.
	// One-line POST-MORTEM dump RIGHT before the abort gate so a future
	// failure surfaces every relevant fact (path, dir contents, byte
	// counts on disk) in a single grep-friendly line. Without this the
	// operator has to scroll up through dozens of unrelated log entries
	// to find the 1.5.13 / 1.5.17 diagnostics.
	postMortemEntries := []string{}
	if entries, err := os.ReadDir(downloadedPath); err == nil {
		for _, e := range entries {
			info, ierr := e.Info()
			sz := int64(-1)
			if ierr == nil {
				sz = info.Size()
			}
			postMortemEntries = append(postMortemEntries, fmt.Sprintf("%s(%dB)", e.Name(), sz))
			if len(postMortemEntries) >= 8 {
				postMortemEntries = append(postMortemEntries, "...")
				break
			}
		}
	} else {
		postMortemEntries = append(postMortemEntries, fmt.Sprintf("ReadDir-err=%v", err))
	}
	log.Printf("[%d] POST-MORTEM: downloadedPath=%q blockedExtCounts=%v dirContents=%v",
		rid, downloadedPath, blockedByExt, postMortemEntries)

	if !services.DirHasUsableFiles(downloadedPath) {
		// Two distinct failure shapes share this abort gate; word the
		// reason so the operator can tell which one fired:
		//
		//  - Blocklist stripped everything: the override removed the
		//    only files in the release (DVD_ISO / single-iso releases
		//    against a list that includes .iso are the canonical case).
		//    Operator-actionable: edit /agent/<id> banned_extensions to
		//    let those extensions through.
		//
		//  - Nothing arrived in the first place: the download completed
		//    according to anacrolix, but no usable files landed at
		//    downloadedPath. Dead swarm / zero-byte info / path-mismatch
		//    after WaitAll. The DirHasUsableFiles diagnostic added in
		//    1.5.13 logs the actual walker inventory at the same time
		//    as this abort fires, so the operator can read both lines
		//    side by side. Editing banned_extensions does NOT help here
		//    — the suggestion only makes sense in the blocklist-stripped
		//    branch.
		var reason string
		if blockedDesc := services.FormatExtCounts(blockedByExt); blockedDesc != "" {
			reason = fmt.Sprintf("nothing left after blocklist sweep (using %s) — blocked: %s. Set agent banned_extensions on /agent/<id> to a non-empty list without those extensions to allow them through.",
				services.OnlineBlocklistSource(), blockedDesc)
		} else {
			reason = fmt.Sprintf("download produced no usable files at %s — blocklist sweep removed nothing (using %s); torrent likely had no peers or anacrolix signalled complete on an empty swarm. Check the DirHasUsableFiles diagnostic above for the walker inventory.",
				downloadedPath, services.OnlineBlocklistSource())
		}
		log.Printf("[%d] %s", rid, reason)
		reportProgress("Aborted", reason)
		if cerr := site.Complete(client.CompleteResult{
			LockID:     task.LockID,
			RequestID:  task.RequestID,
			Status:     "aborted",
			FailReason: reason,
		}); cerr != nil {
			log.Printf("[%d] site.Complete (blocklist-empty abort) failed: %v", rid, cerr)
		}
		return
	}

	// ── 2. Extract video metadata ──────────────────────────────────────────
	log.Printf("[%d] Step 2: Analyzing video metadata...", rid)
	reportProgress("Analyzing", "Extracting video metadata...")
	updateTaskProgress(task.RequestID, &client.FileProgress{
		Name: task.Title, Phase: "processing", Percent: 0,
	})

	// pipelineStages collects one entry per post-download stage so the
	// site can render a per-release checklist (migration 227). Each
	// stage call site below appends its outcome. Keeps the diagnosis
	// visible without needing docker access on the agent host.
	pipelineStages := map[string]client.StageRecord{}
	stageOK := func(name string, count int, note string) {
		pipelineStages[name] = client.StageRecord{Status: "ok", Count: count, Note: note}
	}
	stageEmpty := func(name, note string) {
		pipelineStages[name] = client.StageRecord{Status: "empty", Note: note}
	}
	stageSkipped := func(name, note string) {
		pipelineStages[name] = client.StageRecord{Status: "skipped", Note: note}
	}
	stageFailed := func(name, note string) {
		// Cap reason length so a multi-line exec.Command error doesn't
		// bloat the row or hit the storage layer's 4 KiB JSON cap.
		if len(note) > 200 {
			note = note[:200]
		}
		pipelineStages[name] = client.StageRecord{Status: "failed", Note: note}
	}
	summarizeSubtitleLangs := func(tracks []services.SubtitleTrack) string {
		seen := map[string]struct{}{}
		var order []string
		for _, t := range tracks {
			lang := t.Language
			if lang == "" {
				lang = "und"
			}
			if _, ok := seen[lang]; ok {
				continue
			}
			seen[lang] = struct{}{}
			order = append(order, lang)
		}
		if len(order) <= 6 {
			return strings.Join(order, ",")
		}
		return strings.Join(order[:6], ",") + ",…"
	}
	summarizeAudioLangs := func(tracks []services.AudioCatalogTrack) string {
		seen := map[string]struct{}{}
		var order []string
		for _, t := range tracks {
			lang := t.Language
			if lang == "" {
				lang = "und"
			}
			if _, ok := seen[lang]; ok {
				continue
			}
			seen[lang] = struct{}{}
			order = append(order, lang)
		}
		if len(order) <= 6 {
			return strings.Join(order, ",")
		}
		return strings.Join(order[:6], ",") + ",…"
	}

	var videoInfo *services.VideoInfo
	var screenshots []string
	// isManga is set on the CBZ/EPUB branch below so the OCR step
	// (Step 3f) knows to run tesseract — anime screenshots aren't
	// worth OCRing.
	var isManga bool
	videoFiles := services.FindVideoFiles(downloadedPath)

	if len(videoFiles) > 0 {
		mainVideo := videoFiles[0] // largest video file

		info, err := services.ProbeVideo(ctx, mainVideo)
		if err != nil {
			log.Printf("[%d] Probe warning (non-fatal): %v", rid, err)
			stageFailed("mediainfo", err.Error())
		} else {
			videoInfo = info
			log.Printf("[%d] Video: %s %dx%d %s %s %s",
				rid, info.VideoCodec, info.Width, info.Height,
				info.ResolutionLabel(), info.HDR, info.DurationStr())
			stageOK("mediainfo", 1, fmt.Sprintf("%s %s %s", info.VideoCodec, info.ResolutionLabel(), info.HDR))
		}

		// ── 3. Generate screenshots ────────────────────────────────────────
		if videoInfo != nil && videoInfo.Duration > 10 {
			log.Printf("[%d] Step 3: Generating screenshots...", rid)
			reportProgress("Screenshots", "Capturing preview images...")
			updateTaskProgress(task.RequestID, &client.FileProgress{
				Name: task.Title, Phase: "screenshots",
			})
			// 1.5.22: screen dir moved inside dataDir as a sibling of
			// _subtitles. Previously a top-level <tempDir>/screens-XXX
			// dir, which was NOT in the disk_reserve_sweep keep-set
			// (only dl-XXX and stage-XXX were). The sweep's 30-min
			// minAge was the only protection; a slow upload past that
			// window and the screenshot dir got nuked mid-use,
			// producing the same metadata-without-files symptom as the
			// 1.5.20 dl-XXX race. Inside dataDir, the dl-XXX keep-set
			// protection covers it automatically.
			screenDir := filepath.Join(downloadedPath, "_screenshots")
			defer os.RemoveAll(screenDir)

			shots, err := services.GenerateScreenshots(ctx, mainVideo, screenDir, videoInfo.Duration, 6)
			if err != nil {
				log.Printf("[%d] Screenshot warning (non-fatal): %v", rid, err)
				stageFailed("screenshots", err.Error())
			} else if len(shots) > 0 {
				screenshots = shots
				log.Printf("[%d] Generated %d screenshots", rid, len(shots))
				stageOK("screenshots", len(shots), "")
			} else {
				stageEmpty("screenshots", "ffmpeg returned no frames")
			}
		} else if videoInfo != nil {
			stageSkipped("screenshots", fmt.Sprintf("video too short (%s)", videoInfo.DurationStr()))
		}
	} else if archive := services.FindMangaArchive(downloadedPath); archive != "" {
		// Manga path: no video file, but there's a CBZ/EPUB. Extract 6
		// sample pages using the same screenshot upload pipeline — the
		// site stores/renders them identically to video stills.
		isManga = true
		log.Printf("[%d] Step 2: Found manga archive: %s", rid, filepath.Base(archive))
		stageSkipped("mediainfo", "manga archive (no video file)")
		reportProgress("Screenshots", "Extracting preview pages...")
		updateTaskProgress(task.RequestID, &client.FileProgress{
			Name: task.Title, Phase: "screenshots",
		})
		// 1.5.22: see comment on the video path above — screen dir now
		// inside dataDir so the dl-XXX keep-set protects it from the
		// disk_reserve_sweep.
		screenDir := filepath.Join(downloadedPath, "_screenshots")
		defer os.RemoveAll(screenDir)

		shots, err := services.GenerateMangaScreenshots(ctx, archive, screenDir, 6)
		if err != nil {
			log.Printf("[%d] Manga screenshot warning (non-fatal): %v", rid, err)
			stageFailed("screenshots", err.Error())
		} else if len(shots) > 0 {
			screenshots = shots
			log.Printf("[%d] Extracted %d manga pages", rid, len(shots))
			stageOK("screenshots", len(shots), "manga pages")
		} else {
			stageEmpty("screenshots", "manga archive yielded no extractable pages")
		}
	} else {
		stageSkipped("mediainfo", "no video file or manga archive found")
		stageSkipped("screenshots", "no video file or manga archive found")
	}

	// ── 4. Prepare upload directory with obfuscated filenames ───────────────
	// "stage-" prefix gives SweepOrphanDownloads a recognizable shape so
	// stage dirs left behind by a force-killed agent get cleaned up like
	// dl-* and screens-* do. defer handles the happy path.
	//
	// SetJobStagePath publishes this dir to the orphan-sweep keep-set: a
	// task that sits queued behind a slow upload can wait long enough that
	// the 30-min stale-dir sweep would otherwise wipe its stage and the
	// upload would "complete" with zero articles posted. We clear it back
	// to "" in the defer alongside RemoveAll.
	stageDir := filepath.Join(cfg.TempDir, "stage-"+services.GenerateRandomPassword(12))
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		fail("Prepare", "Stage dir error", err)
		return
	}
	storage.SetJobStagePath(jobName, stageDir)
	defer func() {
		os.RemoveAll(stageDir)
		storage.SetJobStagePath(jobName, "")
	}()

	// ── 3b. Extract subtitle tracks ────────────────────────────────────────
	// Run before staging so the extraction directory shares the same
	// download root (gets cleaned up by the same os.RemoveAll defer).
	// Failure here is non-fatal: the upload + screenshot pipeline runs
	// regardless. Empty result for releases with no subtitle tracks.
	subtitleDir := filepath.Join(downloadedPath, "_subtitles")
	subStatus := services.SubtitleToolStatus()
	subtitleTracks, subErr := services.ExtractSubtitles(ctx, downloadedPath, subtitleDir)
	switch {
	case subStatus != "":
		// Pre-flight told us why the stage will silently return nil —
		// surface that in the checklist instead of an unattributed "empty".
		stageSkipped("subtitles", subStatus)
	case subErr != nil:
		log.Printf("[%d] subtitles: extraction failed (continuing): %v", rid, subErr)
		stageFailed("subtitles", subErr.Error())
	case len(subtitleTracks) > 0:
		log.Printf("[%d] subtitles: extracted %d track(s)", rid, len(subtitleTracks))
		stageOK("subtitles", len(subtitleTracks), summarizeSubtitleLangs(subtitleTracks))
	default:
		stageEmpty("subtitles", "no subtitle tracks found in MKV containers")
	}
	// Convert the agent-side tracks into the client-side upload
	// payload. The NzbID stays zero — Complete fills it in from
	// the site response.
	var subtitleUploads []client.SubtitleUpload
	for _, t := range subtitleTracks {
		subtitleUploads = append(subtitleUploads, client.SubtitleUpload{
			TrackIndex:   t.TrackIndex,
			Language:     t.Language,
			TrackName:    t.TrackName,
			Codec:        t.Codec,
			Forced:       t.Forced,
			DefaultTrack: t.DefaultTrack,
			Path:         t.File,
		})
	}

	// ── 3c. Probe audio tracks ─────────────────────────────────────────────
	// Metadata-only catalog (see migration 217). No files to extract,
	// no disk usage, just mkvmerge -J per video. Same forward-compat
	// rule as subtitles: missing mkvmerge ⇒ skip silently.
	audioTracks, audioErr := services.ProbeAudioTracks(ctx, downloadedPath)
	switch {
	case audioErr != nil:
		log.Printf("[%d] audio: probe failed (continuing): %v", rid, audioErr)
		stageFailed("audio_tracks", audioErr.Error())
	case len(audioTracks) > 0:
		log.Printf("[%d] audio: cataloged %d track(s)", rid, len(audioTracks))
		stageOK("audio_tracks", len(audioTracks), summarizeAudioLangs(audioTracks))
	default:
		// Could be: mkvmerge missing, no video files, or videos without
		// audio streams. We don't reach in to distinguish today — the
		// site-side aggregate stat ("X% of completed releases have an
		// audio_tracks=empty entry") makes the macro problem visible.
		stageEmpty("audio_tracks", "no audio tracks cataloged")
	}
	var audioUploads []client.AudioTrackUpload
	for _, t := range audioTracks {
		audioUploads = append(audioUploads, client.AudioTrackUpload{
			TrackIndex:   t.TrackIndex,
			Language:     t.Language,
			TrackName:    t.TrackName,
			Codec:        t.Codec,
			Channels:     t.Channels,
			SampleRateHz: t.SampleRateHz,
			BitrateKbps:  t.BitrateKbps,
			DefaultTrack: t.DefaultTrack,
			Forced:       t.Forced,
		})
	}

	// ── 3d. Acoustic fingerprint (Chromaprint via fpcalc) ──────────────────
	// One row per video file. Used by the site to detect duplicate
	// audio across releases (same Japanese dub, different rips). No
	// files to extract — just a base32 string per video.
	fingerprints, fpErr := services.FingerprintAudio(ctx, downloadedPath)
	switch {
	case fpErr != nil:
		log.Printf("[%d] fingerprint: failed (continuing): %v", rid, fpErr)
		stageFailed("audio_fingerprints", fpErr.Error())
	case len(fingerprints) > 0:
		log.Printf("[%d] fingerprint: generated for %d file(s)", rid, len(fingerprints))
		stageOK("audio_fingerprints", len(fingerprints), "")
	default:
		stageEmpty("audio_fingerprints", "fpcalc produced no fingerprints (missing binary or no audio)")
	}
	var fingerprintUploads []client.AudioFingerprintUpload
	for _, f := range fingerprints {
		fingerprintUploads = append(fingerprintUploads, client.AudioFingerprintUpload{
			SourceFilename:   f.SourceFilename,
			DurationSeconds:  f.DurationSeconds,
			AlgorithmVersion: f.AlgorithmVersion,
			Fingerprint:      f.Fingerprint,
		})
	}

	// ── 3e. Dominant color palette ─────────────────────────────────────────
	// Bucket-histograms the screenshots we already generated. Pure
	// in-process work, no external binaries, ~50ms for 8 screenshots.
	// Empty for releases without screenshots (manga only / failed
	// screenshot pass) — the site hides the swatch strip in that case.
	dominantPalette := services.ExtractDominantPalette(screenshots, 8)
	switch {
	case len(screenshots) == 0:
		stageSkipped("dominant_palette", "no screenshots to sample")
	case len(dominantPalette) > 0:
		log.Printf("[%d] palette: %d colours from %d screenshot(s)", rid, len(dominantPalette), len(screenshots))
		stageOK("dominant_palette", len(dominantPalette), "")
	default:
		stageEmpty("dominant_palette", "bucket-histogram returned no colours")
	}

	// ── 3f. Manga OCR ──────────────────────────────────────────────────────
	// Only runs on the manga branch — anime screenshots aren't worth
	// OCRing (subtitles + scene cuts produce noise, not useful text).
	// Tesseract is optional; missing binary or missing language data
	// logs once and skips.
	var ocrResult services.OCRResult
	switch {
	case !isManga:
		stageSkipped("ocr", "anime release (OCR runs only on manga)")
	case len(screenshots) == 0:
		stageSkipped("ocr", "no manga pages available to OCR")
	default:
		ocrResult = services.OCRMangaPages(ctx, screenshots, "eng+jpn")
		switch {
		case ocrResult.Text != "":
			log.Printf("[%d] ocr: extracted %d chars from %d page(s) (%s)", rid, len(ocrResult.Text), len(screenshots), ocrResult.Language)
			stageOK("ocr", len(ocrResult.Text), ocrResult.Language)
		default:
			stageEmpty("ocr", "tesseract produced no recognisable text")
		}
	}

	log.Printf("[%d] Step 4: Staging files...", rid)
	// Pre-stage size-parity check (1.5.25). Before the staging walk
	// runs, compare what's ACTUALLY on disk in downloadedPath against
	// what the torrent SAID we should have downloaded. If the delta
	// is large, something nuked files between download-complete and
	// stage-start — historically the disk-sweep race (1.5.20 / 1.5.24)
	// but defensive against any future bug that loses files mid-
	// pipeline.
	//
	// auditStaged inside CopyFiles only catches src↔dst mismatches
	// AT COPY TIME — if both src and dst agree on "3 of 14 files",
	// the audit passes and a partial NZB ships silently. This check
	// guards against that.
	//
	// Tolerance: 80% of expected bytes. The blocklist run earlier
	// legitimately deletes some files (executables, archives on a
	// media-only group), so a tight 95% threshold would false-positive.
	// 80% is loose enough that legitimate filtering passes but tight
	// enough to catch "11 of 14 episodes vanished" (uploads at 21%).
	if session != nil {
		// Per-file check: walk every file the torrent's metainfo
		// declared and verify it exists on disk at the expected
		// path with the expected byte length. Stricter than the
		// gross byte-total check below — catches "specific file
		// missing while total bytes look mostly right" failures,
		// like the 2026-06-04 Another S01 incident where 11 of 14
		// episodes vanished but the byte total wasn't enough to
		// trip the threshold alone if E10-E12 happened to be very
		// large (in fact it WAS enough — but on subtler losses,
		// per-file would catch what bytes wouldn't).
		expectedFiles := session.ExpectedFiles()
		var missing []string
		var truncated []string
		for _, ef := range expectedFiles {
			full := filepath.Join(downloadedPath, ef.Path)
			fi, statErr := os.Stat(full)
			switch {
			case os.IsNotExist(statErr):
				missing = append(missing, ef.Path)
			case statErr != nil:
				log.Printf("[%d] Step 4: stat %s: %v (treating as missing)", rid, ef.Path, statErr)
				missing = append(missing, ef.Path)
			case fi.Size() < ef.Size:
				truncated = append(truncated,
					fmt.Sprintf("%s (%.1f%% of %d bytes)",
						ef.Path, float64(fi.Size())/float64(ef.Size)*100, ef.Size))
			}
		}
		if len(expectedFiles) > 0 {
			log.Printf("[%d] Step 4: pre-stage file check — torrent declared %d file(s); on disk: %d ok, %d missing, %d truncated",
				rid, len(expectedFiles),
				len(expectedFiles)-len(missing)-len(truncated),
				len(missing), len(truncated))
		}
		if len(missing) > 0 || len(truncated) > 0 {
			// Format a useful error. Cap each list at 5 names so a
			// torrent with 100 missing files doesn't blow the log
			// line — operator can see the rest in the on-disk list.
			capList := func(items []string) string {
				if len(items) <= 5 {
					return strings.Join(items, ", ")
				}
				return strings.Join(items[:5], ", ") + fmt.Sprintf(", … +%d more", len(items)-5)
			}
			err := fmt.Errorf(
				"pre-stage file check failed: torrent declared %d file(s), %d missing + %d truncated. Missing: [%s]. Truncated: [%s]. Likely disk_reserve_sweep race (fixed in 1.5.24) or partial download — re-poll the task or check /admin/errors",
				len(expectedFiles), len(missing), len(truncated),
				capList(missing), capList(truncated))
			fail("Prepare", "Pre-stage file check failed", err)
			return
		}

		// Byte-total check: defence in depth. The per-file check
		// above catches missing/truncated individual files; this
		// catches "everything's there but the bytes are wrong"
		// (e.g. an inflated zero-byte file that os.Stat counts as
		// present). 80% threshold leaves headroom for blocklist
		// deletions; a >20% delta is always a bug.
		expected := session.ExpectedBytes()
		actual, fileCount, walkErr := services.CountUploadableBytes(downloadedPath)
		if walkErr != nil {
			log.Printf("[%d] Step 4: pre-stage size check walk failed: %v (proceeding)", rid, walkErr)
		} else if expected > 0 {
			ratio := float64(actual) / float64(expected)
			log.Printf("[%d] Step 4: pre-stage size check — torrent expected %.2f GB, on-disk %.2f GB across %d file(s), ratio %.1f%%",
				rid, float64(expected)/(1<<30), float64(actual)/(1<<30), fileCount, ratio*100)
			if ratio < 0.80 {
				err := fmt.Errorf("pre-stage size check failed: torrent expected %.2f GB but only %.2f GB across %d file(s) on disk (%.0f%% — under 80%% threshold). Something deleted files between download and stage; check for disk_reserve_sweep race or manual rm",
					float64(expected)/(1<<30), float64(actual)/(1<<30), fileCount, ratio*100)
				fail("Prepare", "Pre-stage size mismatch", err)
				return
			}
		}
	}
	if cfg.Obfuscate {
		reportProgress("Preparing", "Obfuscating filenames...")
		if err := services.ObfuscateFiles(ctx, downloadedPath, stageDir); err != nil {
			fail("Prepare", "Prepare error", err)
			return
		}
	} else {
		reportProgress("Preparing", "Copying files...")
		if err := services.CopyFiles(ctx, downloadedPath, stageDir); err != nil {
			fail("Prepare", "Prepare error", err)
			return
		}
	}
	log.Printf("[%d] Step 4: Staging complete", rid)

	// ── 4.5. Extract RAR archives ──────────────────────────────────────────
	// If the staged content contains RAR archives (common shape for
	// torrent → Usenet republishes and for some offer-shared
	// folders), unpack them in place. The PAR2 step that follows
	// generates recovery data for the *extracted* media files, and
	// the upload step posts the real content instead of a RAR
	// wrapper. Original .rar volumes + their .par2 recovery files
	// are deleted after a successful extract so they don't double-
	// upload. Partial-success errors are logged but don't abort
	// the task — anything that did extract is kept.
	log.Printf("[%d] Step 4.5: Scanning for RAR archives...", rid)
	if extracted, err := services.ExtractRARArchives(ctx, stageDir, func(msg string) {
		reportProgress("Extract", msg)
	}); err != nil {
		log.Printf("[%d] Step 4.5: extract warning (extracted=%d): %v", rid, extracted, err)
	} else if extracted > 0 {
		log.Printf("[%d] Step 4.5: Extracted %d RAR archive(s)", rid, extracted)
	} else {
		log.Printf("[%d] Step 4.5: No RAR archives found", rid)
	}

	// ── 4.6. Extract compressed ZIP archives ───────────────────────────────
	// Same rationale as RAR, with a store-mode exception: a zip whose
	// entries are all stored (no compression) is already streamable in
	// place, so we leave it; a zip with any compressed entry locks the
	// media behind Deflate, so we unpack it before PAR2 + upload. Runs
	// after RAR so a RAR-inside-zip (rare) still gets the RAR pass on a
	// later task if needed. Native archive/zip — no external binary.
	log.Printf("[%d] Step 4.6: Scanning for ZIP archives...", rid)
	if extracted, err := services.ExtractZIPArchives(ctx, stageDir, func(msg string) {
		reportProgress("Extract", msg)
	}); err != nil {
		log.Printf("[%d] Step 4.6: extract warning (extracted=%d): %v", rid, extracted, err)
	} else if extracted > 0 {
		log.Printf("[%d] Step 4.6: Extracted %d ZIP archive(s)", rid, extracted)
	} else {
		log.Printf("[%d] Step 4.6: No compressed ZIP archives found", rid)
	}

	// ── 4.7. Extract 7z archives ────────────────────────────────────────────
	// Same rationale as RAR (always compressed, no store-mode exception):
	// 7z locks media behind LZMA, so we always unpack single + split
	// (.7z / .7z.001) sets before PAR2 + upload. Shells to the same
	// 7z/7za/7zr family the RAR fallback already requires.
	log.Printf("[%d] Step 4.7: Scanning for 7z archives...", rid)
	if extracted, err := services.Extract7zArchives(ctx, stageDir, func(msg string) {
		reportProgress("Extract", msg)
	}); err != nil {
		log.Printf("[%d] Step 4.7: extract warning (extracted=%d): %v", rid, extracted, err)
	} else if extracted > 0 {
		log.Printf("[%d] Step 4.7: Extracted %d 7z archive(s)", rid, extracted)
	} else {
		log.Printf("[%d] Step 4.7: No 7z archives found", rid)
	}

	// ── 4.8. Extract ISO disc images ────────────────────────────────────────
	// p7zip reads ISO9660 + UDF, so a data ISO or a BD/DVD disc image
	// is unpacked into its real file tree (BDMV / VIDEO_TS / files)
	// before PAR2 + upload. Reuses the same 7z binary as Step 4.7 — no
	// extra dependency, silent no-op when 7z is absent.
	log.Printf("[%d] Step 4.8: Scanning for ISO disc images...", rid)
	if extracted, err := services.ExtractISOArchives(ctx, stageDir, func(msg string) {
		reportProgress("Extract", msg)
	}); err != nil {
		log.Printf("[%d] Step 4.8: extract warning (extracted=%d): %v", rid, extracted, err)
	} else if extracted > 0 {
		log.Printf("[%d] Step 4.8: Extracted %d ISO image(s)", rid, extracted)
	} else {
		log.Printf("[%d] Step 4.8: No ISO disc images found", rid)
	}

	// ── 4.9. Extract tarballs ───────────────────────────────────────────────
	// Linux-origin releases occasionally ship media inside a (compressed)
	// tarball. gzip/bzip2/plain decode in pure Go; xz/zstd lean on the 7z
	// binary. Unpacks before PAR2 so the NZB carries the real files.
	log.Printf("[%d] Step 4.9: Scanning for tar archives...", rid)
	if extracted, err := services.ExtractTarArchives(ctx, stageDir, func(msg string) {
		reportProgress("Extract", msg)
	}); err != nil {
		log.Printf("[%d] Step 4.9: extract warning (extracted=%d): %v", rid, extracted, err)
	} else if extracted > 0 {
		log.Printf("[%d] Step 4.9: Extracted %d tar archive(s)", rid, extracted)
	} else {
		log.Printf("[%d] Step 4.9: No tar archives found", rid)
	}

	// ── 4.10. Extract legacy/odd formats (lzh, cab, arj, cpio) ──────────────
	// Rare for anime media, but cheap to cover via the 7z binary already
	// required for Steps 4.7/4.8. Silent no-op when 7z is absent.
	log.Printf("[%d] Step 4.10: Scanning for lzh/cab/arj/cpio archives...", rid)
	if extracted, err := services.ExtractMiscArchives(ctx, stageDir, func(msg string) {
		reportProgress("Extract", msg)
	}); err != nil {
		log.Printf("[%d] Step 4.10: extract warning (extracted=%d): %v", rid, extracted, err)
	} else if extracted > 0 {
		log.Printf("[%d] Step 4.10: Extracted %d legacy archive(s)", rid, extracted)
	} else {
		log.Printf("[%d] Step 4.10: No legacy archives found", rid)
	}

	// ── 5. Generate PAR2 recovery files ────────────────────────────────────
	log.Printf("[%d] Step 5: Generating PAR2 recovery data...", rid)
	reportProgress("PAR2", "Generating recovery data...")
	updateTaskProgress(task.RequestID, &client.FileProgress{
		Name: task.Title, Phase: "par2",
	})
	baseName := services.GenerateRandomPassword(12)
	if !cfg.Obfuscate {
		baseName = services.SanitizeBaseName(task.Title)
	}
	par2Start := time.Now()

	// Stream PAR2 progress to the dashboard so users can see it's not stuck.
	// par2create emits lines like "Processing: 12.3%" and "Creating recovery
	// file(s): 45.6%" that we parse and forward via the live status channel.
	par2Progress := func(phase string, pct float64) {
		elapsed := time.Since(par2Start).Round(time.Second)
		detail := fmt.Sprintf("%s %.0f%% (%s elapsed)", phase, pct, elapsed)
		reportProgress("PAR2", detail)
		updateTaskProgress(task.RequestID, &client.FileProgress{
			Name:    task.Title,
			Phase:   "par2",
			Percent: pct,
		})
	}

	par2Files, err := services.GeneratePAR2(ctx, stageDir, baseName, services.PAR2Options{
		Redundancy: cfg.PAR2Redundancy,
		BlockSize:  services.ChunkSize,
		Threads:    cfg.PAR2Threads,
		MemoryMB:   cfg.PAR2Memory,
	}, par2Progress)
	if err != nil {
		// PAR2 failure visibility (1.5.26).
		//
		// Three places it shows up:
		//   1. Agent log: full context (baseName + stageDir + err)
		//      so docker logs grep tells the operator everything.
		//   2. /admin/errors: site.PostLog ships an 'error' entry
		//      tagged with request_id so admin can spot a flapping
		//      par2 binary without ssh'ing into the agent.
		//   3. Release page pipeline checklist: stageEmpty (NOT
		//      stageFailed) per operator's policy — the release
		//      shows "par2: empty" on the migration 227 checklist
		//      but doesn't surface the gory error there; that lives
		//      in /admin/errors.
		//
		// stageEmpty (not stageFailed) is deliberate: from the
		// end-user's POV, the release simply has no recovery; it's
		// not "broken". The site renders empty stages as neutral
		// rather than red. Operator triage goes through /admin/errors.
		log.Printf("[%d] PAR2 FAILED (non-fatal) for %q in %q: %v",
			rid, baseName, stageDir, err)
		site.PostLog("error", fmt.Sprintf(
			"PAR2 generation failed for request=%d (%s): %v — release shipped to Usenet without recovery files (parity loss is now unrecoverable)",
			rid, task.Title, err))
		reportProgress("PAR2", "PAR2 failed, uploading without recovery — admin/errors has details")
		stageEmpty("par2", "no recovery files generated (see /admin/errors for the underlying binary error)")
	} else {
		log.Printf("[%d] Step 5: PAR2 complete in %s — %d recovery file(s) generated",
			rid, time.Since(par2Start).Round(time.Second), len(par2Files))
		if len(par2Files) == 0 {
			// Defensive: GeneratePAR2 returned nil error but zero
			// files. The walker may have looked at the wrong dir,
			// or the binary completed without writing output. Ship
			// the warning so it doesn't go silent.
			site.PostLog("warn", fmt.Sprintf(
				"PAR2 reported success but produced ZERO .par2 files for request=%d (%s). Stage dir: %s. Upload continuing without recovery — investigate the par2 binary.",
				rid, task.Title, stageDir))
			reportProgress("PAR2", "PAR2 produced no files — uploading without recovery")
			stageEmpty("par2", "binary returned success but no files were produced (see /admin/errors)")
		} else {
			// Successful PAR2 run: record the count + total recovery
			// MB so the release page checklist can show "par2: 13
			// files / 1.3 GB" instead of just a green checkmark.
			var par2Size int64
			for _, p := range par2Files {
				if info, statErr := os.Stat(p); statErr == nil {
					par2Size += info.Size()
				}
			}
			stageOK("par2", len(par2Files),
				fmt.Sprintf("%.1f MB recovery at %d%%",
					float64(par2Size)/(1024*1024), cfg.PAR2Redundancy))
		}
	}

	// ── 6. Optional encryption ─────────────────────────────────────────────
	var password string
	uploadDir := stageDir
	if cfg.Encrypt {
		password = services.GenerateRandomPassword(16)
		archiveName := services.GenerateRandomPassword(16) + ".7z"
		archivePath := filepath.Join(cfg.TempDir, archiveName)
		defer os.Remove(archivePath)

		log.Printf("[%d] Step 6: Encrypting with 7z...", rid)
		reportProgress("Encrypting", "Creating password-protected 7z archive...")
		updateTaskProgress(task.RequestID, &client.FileProgress{
			Name: task.Title, Phase: "encrypting",
		})
		if err := services.EncryptWith7z(ctx, stageDir, archivePath, password); err != nil {
			fail("Encrypt", "Encryption error", err)
			return
		}

		encDir := filepath.Join(cfg.TempDir, "enc-"+services.GenerateRandomPassword(8))
		os.MkdirAll(encDir, 0755)
		defer os.RemoveAll(encDir)
		os.Rename(archivePath, filepath.Join(encDir, archiveName))
		uploadDir = encDir
		log.Printf("Encrypted to %s (%d chars password)", archiveName, len(password))
	}

	// ── 7. Upload all files to Usenet (serialized — one upload at a time) ──
	var totalUploadSize int64
	filepath.Walk(uploadDir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			totalUploadSize += info.Size()
		}
		return nil
	})

	log.Printf("[%d] Step 7: Waiting for upload slot (%.1f MiB to upload)...", rid, float64(totalUploadSize)/1024/1024)
	reportProgress("Queued", "Waiting for upload slot...")
	updateTaskProgress(task.RequestID, &client.FileProgress{
		Name: task.Title, Phase: "queued", Size: totalUploadSize,
	})

	// Wait-time tracking around the slot: if the prior task wedges,
	// the wait-for-slot duration grows unbounded. Logging the actual
	// wait makes "30 tasks stacked behind a stuck upload" visible
	// from the agent log alone — operators don't need the dashboard
	// to spot the pile-up.
	//
	// The slot covers the NNTP upload + site report ONLY. Seeding at
	// the very end of processTask is BitTorrent traffic, unrelated to
	// the upload mutex — releasing the slot before session.Seed()
	// lets the next task start uploading while this one seeds. sync.Once
	// makes the explicit release idempotent: the deferred path also
	// fires on error / panic returns that skip the manual release.
	slotWaitStart := time.Now()
	services.UploadSlot.Lock()
	slotHeldSince := time.Now()
	// Always log slot ownership transitions — pairs with the Released
	// line so any future slot-held-too-long bug is one grep away.
	// Format: "[N] Slot ACQUIRED (waited Xs)" → matching
	// "[N] Slot RELEASED (held Xs)" later. Search the agent log for
	// gaps where ACQUIRED isn't followed by RELEASED for the same rid
	// to surface a hung task.
	if w := time.Since(slotWaitStart); w > 30*time.Second {
		log.Printf("[%d] Slot ACQUIRED (waited %s)", rid, w.Round(time.Second))
	} else {
		log.Printf("[%d] Slot ACQUIRED", rid)
	}
	var unlockOnce sync.Once
	releaseSlot := func() {
		unlockOnce.Do(func() {
			services.UploadSlot.Unlock()
			log.Printf("[%d] Slot RELEASED (held %s)", rid, time.Since(slotHeldSince).Round(time.Second))
		})
	}
	// BELT for the existing SUSPENDERS of defer releaseSlot(): if anything
	// inside the critical section panics, ensure the slot is released and
	// then re-panic so the agent crash-restarts cleanly.
	defer func() {
		if r := recover(); r != nil {
			releaseSlot()
			panic(r)
		}
	}()
	defer releaseSlot()

	// Manifest audit: compare the raw downloaded torrent content against
	// the directory we're about to publish. Catches the "multi-file
	// torrent ships as a single-file NZB" symptom BEFORE we burn an
	// upload on a partial release. See services/upload_manifest.go for
	// the rule (briefly: refuse to publish when upload has fewer video
	// files than the source and encryption isn't masking the comparison).
	//
	// On failure, route the FULL per-file diff to three places so the
	// operator can debug without re-running:
	//   1. docker log — log.Printf the DetailedReport (every missing file)
	//   2. site agent_logs — site.PostLog the same, visible on the agent
	//      dashboard in the admin UI
	//   3. request_lock fail_reason — fail()'s wrap of err.Error() gives
	//      the concise single-line summary on the request detail page
	srcManifest := services.ManifestOf(downloadedPath)
	upManifest := services.ManifestOf(uploadDir)
	log.Printf("[%d] %s", rid, services.FormatManifestLine(srcManifest, upManifest, cfg.Encrypt))
	if err := services.CompareManifest(srcManifest, upManifest, cfg.Encrypt); err != nil {
		var mfErr *services.ManifestError
		if errors.As(err, &mfErr) {
			report := mfErr.DetailedReport()
			log.Printf("[%d] %s", rid, report)
			// Best-effort: push the full diff to the site so it shows up
			// on the agent dashboard without anyone needing docker access.
			// Failure here is non-fatal — the bug still gets surfaced via
			// FailReason on the request — but log on failure so a silent
			// PostLog drop doesn't hide the diagnostic from the operator.
			if perr := site.PostLog("error",
				fmt.Sprintf("[req %d] %s\n%s", task.RequestID, task.Title, report)); perr != nil {
				log.Printf("[%d] site.PostLog (manifest-mismatch report) failed: %v", task.RequestID, perr)
			}
		}
		fail("ManifestMismatch", "Manifest check failed — aborting publish", err)
		return
	}

	log.Printf("[%d] Step 7: Uploading to Usenet: %.2f MiB via %d connections...",
		rid, float64(totalUploadSize)/1024/1024, cfg.NNTPConnections)
	reportProgress("Uploading", fmt.Sprintf("%.1f MiB via %d NNTP connections...",
		float64(totalUploadSize)/1024/1024, cfg.NNTPConnections))

	services.SetProgressCallbackForJob(jobName, progressCb)

	uploadStart := time.Now()
	fileSegments, err := services.UploadDirectory(ctx, cfg, uploadDir, task.Title, jobName)

	services.ClearProgressCallbackForJob(jobName)

	if err != nil {
		fail("Upload", "Upload error", err)
		return
	}
	uploadDur := time.Since(uploadStart)
	speedMBs := float64(totalUploadSize) / 1024 / 1024 / uploadDur.Seconds()
	log.Printf("[%d] Step 7: Upload complete: %.2f MiB in %s (%.1f MB/s)",
		rid, float64(totalUploadSize)/1024/1024, uploadDur.Round(time.Second), speedMBs)

	// ── 8. Generate NZB ────────────────────────────────────────────────────
	log.Printf("[%d] Step 8: Generating NZB and reporting to site...", rid)
	reportProgress("Finalizing", "Generating NZB...")

	nzbData, err := services.CreateMultiFileNZBBytes(cfg, fileSegments, password, services.NZBMetaInfo{
		Title:     task.Title,
		RequestID: task.RequestID,
	})
	if err != nil {
		fail("NZB", "NZB error", err)
		return
	}

	// ── 9. Report completion with NZB + metadata + screenshots ─────────────
	reportProgress("Reporting", "Sending results to site...")

	completeResult := client.CompleteResult{
		LockID:            task.LockID,
		RequestID:         task.RequestID,
		Status:            "completed",
		NzbData:           nzbData,
		Password:          password,
		MediaInfo:         videoInfo,
		Screenshots:       screenshots,
		Subtitles:         subtitleUploads,
		AudioTracks:       audioUploads,
		AudioFingerprints: fingerprintUploads,
		DominantPalette:   dominantPalette,
		OCRText:           ocrResult.Text,
		OCRLanguage:       ocrResult.Language,
		PipelineStages:    pipelineStages,
	}

	// Retry completion with smart handling for maintenance mode. Normal
	// errors retry up to 3 times with short backoff. If the site returns
	// a MaintenanceError (503 with {"maintenance":true,...}), we wait out
	// the reported ETA and keep retrying indefinitely — the agent has
	// already done the expensive upload work and we don't want to lose it.
	var completeErr error
	normalAttempt := 0
	for {
		completeErr = site.Complete(completeResult)
		if completeErr == nil {
			break
		}

		// Maintenance: wait out the ETA and keep going.
		if me, ok := client.IsMaintenanceError(completeErr); ok {
			wait := time.Duration(me.Info.ETASeconds+15) * time.Second
			if wait < 30*time.Second {
				wait = 30 * time.Second
			}
			if wait > 10*time.Minute {
				wait = 10 * time.Minute
			}
			log.Printf("[%d] Site in maintenance: %s — waiting %s before retry",
				rid, me.Info.Reason, wait.Round(time.Second))
			reportProgress("Waiting", fmt.Sprintf("Site maintenance: %s", me.Info.Reason))
			if ctx.Err() != nil {
				break
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				completeErr = ctx.Err()
			}
			continue
		}

		// Normal transient error: 3 quick retries then give up.
		normalAttempt++
		log.Printf("[%d] Completion attempt %d/3 failed: %v", rid, normalAttempt, completeErr)
		if normalAttempt >= 3 {
			break
		}
		time.Sleep(time.Duration(normalAttempt*10) * time.Second)
	}
	if completeErr != nil {
		// Save NZB locally as last resort so it's not lost.
		backupPath := filepath.Join(cfg.TempDir, fmt.Sprintf("backup-request-%d.nzb", task.RequestID))
		if err := os.WriteFile(backupPath, nzbData, 0644); err == nil {
			log.Printf("[%d] NZB saved to %s — upload manually if needed", rid, backupPath)
			site.PostLog("error", fmt.Sprintf("Completion failed for request #%d. NZB saved locally at %s", task.RequestID, backupPath))
		}
		reportProgress("Failed", "Report error: "+completeErr.Error())
		return
	}

	log.Printf("[%d] ── Pipeline complete: %s (total %s) ──", rid, task.Title, time.Since(taskStart).Round(time.Second))
	storage.UpdateState(jobName, "Completed", "Uploaded and reported to site.", 100)
	recordSuccess()

	// Release the upload slot BEFORE seeding so the next task in the
	// queue can start uploading immediately. Seeding is BitTorrent
	// traffic with its own ratio/time budget (up to 1h per task by
	// default) — holding the NNTP upload mutex through it would
	// throttle the whole queue to one task per hour. releaseSlot is
	// idempotent via sync.Once so the deferred path (error returns /
	// panic / no-seed path) still works without a double-Unlock panic.
	releaseSlot()

	// Clear the in-flight progress entry now that the pipeline is
	// done. The deferred updateTaskProgress(rid, nil) at function entry
	// only fires after the (up to 1h) seed window completes, which
	// would leave the dashboard showing this task as "uploading 100%"
	// for the entire seed duration. The NZB is already on the site,
	// the upload is done — from a queue/dashboard perspective the task
	// is finished, even though we're still seeding for BitTorrent ratio.
	updateTaskProgress(task.RequestID, nil)

	// Seed AFTER the request is fulfilled — the NZB is now visible to
	// users, so the seed phase shares back to the swarm without holding
	// the request in the unfulfilled state. No-op when the user hasn't
	// configured torrent_seed_ratio / torrent_seed_hours, and skipped
	// entirely on the resume-from-disk path (session is nil).
	if session != nil {
		reportProgress("Seeding", "Sharing back to swarm...")
		session.Seed(ctx)
	}
}
