package services

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ameNZB/loon-agent/config"
	"github.com/ameNZB/loon-agent/storage"
)

// processorPollInterval is how often we look for a queued job when the
// queue is empty. When work is found we loop immediately, so this only
// bounds the idle-wake latency, not throughput.
const processorPollInterval = 10 * time.Second

// StartOfflineProcessor runs a single goroutine that claims queued jobs
// in FIFO order and runs them end-to-end. One at a time — NNTP uploads
// already serialise on UploadSlot, and running parallel pipelines would
// mostly just contend on disk and VPN bandwidth with no throughput win.
//
// Returns immediately if db is nil (same fallback as the watcher).
func StartOfflineProcessor(ctx context.Context, cfg *config.Config, db *storage.DB) {
	if db == nil {
		return
	}
	log.Printf("offline processor started")
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		job, err := db.ClaimNextJob()
		if err != nil {
			log.Printf("processor: claim: %v", err)
			sleepOrExit(ctx, processorPollInterval)
			continue
		}
		if job == nil {
			sleepOrExit(ctx, processorPollInterval)
			continue
		}
		runOneOfflineJob(ctx, cfg, db, job)
	}
}

func sleepOrExit(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// runOneOfflineJob wraps processOfflineJob with DB status transitions —
// keeps error-path handling (mark failed, log) out of the happy-path
// function so the pipeline itself reads top-to-bottom without every
// step having to remember to update status on failure.
func runOneOfflineJob(ctx context.Context, cfg *config.Config, db *storage.DB, job *storage.OfflineJob) {
	log.Printf("[offline %d] starting: %s", job.ID, job.Title)
	nzbPath, password, err := processOfflineJob(ctx, cfg, db, job)
	if err != nil {
		log.Printf("[offline %d] failed: %v", job.ID, err)
		_ = db.MarkJobFailed(job.ID, err.Error())
		return
	}
	if err := db.MarkJobCompleted(job.ID, nzbPath, password); err != nil {
		log.Printf("[offline %d] post-complete update: %v", job.ID, err)
	}
	log.Printf("[offline %d] completed → %s", job.ID, nzbPath)
}

// processOfflineJob runs the end-to-end pipeline for one job. Phases are
// published through UpdateJobPhase so the /jobs UI shows live progress.
// Returns (nzbPath, password, error); password is "" for unencrypted runs.
//
//nolint:funlen // pipeline is naturally linear; splitting into helpers
// would scatter the error-handling + cleanup defers without aiding reading.
func processOfflineJob(ctx context.Context, cfg *config.Config, db *storage.DB, job *storage.OfflineJob) (string, string, error) {
	// ── 0. Group lookup ───────────────────────────────────────────────────
	// The group decides newsgroups + overrides. If the operator deleted it
	// between queue and claim the job dies here rather than posting to a
	// wrong default — surfaces the problem instead of hiding it.
	if job.GroupID == nil {
		return "", "", errors.New("job has no group assigned")
	}
	group, err := db.GetGroup(*job.GroupID)
	if err != nil {
		return "", "", fmt.Errorf("load group: %w", err)
	}
	if len(group.Newsgroups) == 0 {
		return "", "", fmt.Errorf("group %q has no newsgroups", group.Name)
	}

	// ── 1. Per-job cfg with group overrides ───────────────────────────────
	// Shallow copy of cfg — all fields are scalars except *Layered, which
	// we deliberately share (the layered system doesn't mutate during a job).
	jobCfg := *cfg
	jobCfg.NNTPGroup = strings.Join(group.Newsgroups, ",")
	if group.Par2Redundancy != nil {
		jobCfg.PAR2Redundancy = *group.Par2Redundancy
	}
	if group.Obfuscate != nil {
		jobCfg.Obfuscate = *group.Obfuscate
	}
	sampleCount := 6 // current global default inside processTask
	if group.Screenshots != nil {
		sampleCount = *group.Screenshots
	}
	sampleSeconds := DefaultSampleSeconds
	if group.SampleSeconds != nil {
		sampleSeconds = *group.SampleSeconds
	}

	jobName := fmt.Sprintf("offline-%d", job.ID)

	// Balance the ReserveDisk the .torrent branch claims deep inside
	// DownloadPrivateTorrentBytes -> downloadTorrentFile. Nothing in the
	// offline pipeline released it, so diskReserved grew by ~1.3x the
	// torrent size on every .torrent job and never came back until a
	// process restart — eventually starving the poll loop with a phantom
	// "disk full" (the 1.5.23 regression class, via the offline path).
	// No-op for the directory / single-file branches, which never reserve.
	defer ReleaseDisk(jobName)

	phase := func(p string, pct float64) {
		_ = db.UpdateJobPhase(job.ID, p, pct)
	}

	// ── 2. Locate or fetch source content ─────────────────────────────────
	// Three input shapes supported:
	//   .torrent file  → download via existing torrent services
	//   directory      → treat contents as already-staged content
	//   single file    → wrap in a temp dir so staging sees a folder
	phase("source", 0)
	info, err := os.Stat(job.SourcePath)
	if err != nil {
		return "", "", fmt.Errorf("stat source: %w", err)
	}
	var contentDir string
	var cleanupContent func()

	switch {
	case strings.EqualFold(filepath.Ext(job.SourcePath), ".torrent"):
		phase("downloading", 0)
		blob, err := os.ReadFile(job.SourcePath)
		if err != nil {
			return "", "", fmt.Errorf("read .torrent: %w", err)
		}
		// Use the private-bytes path so we don't push this info hash to
		// public DHT — a user dropping a torrent into their own watch
		// folder hasn't necessarily consented to seeding it to the world.
		session, err := DownloadPrivateTorrentBytes(ctx, blob, &jobCfg, jobName, &DownloadOpts{
			SlowThresholdMBs:    jobCfg.SlowSpeedThresholdMBs,
			SlowTimeoutMins:     jobCfg.SlowSpeedTimeoutMins,
			LowPeersThreshold:   -1,
			LowPeersTimeoutMins: 0,
		})
		if err != nil {
			return "", "", fmt.Errorf("torrent download: %w", err)
		}
		// Offline path doesn't seed — the torrent client gets shut down as
		// soon as we hand the staged content off to the rest of the
		// pipeline. Close before RemoveAll so file handles are released
		// before the data dir is deleted (matters on Windows).
		contentDir = session.Path
		cleanupContent = func() {
			session.Close()
			_ = os.RemoveAll(session.Path)
		}

	case info.IsDir():
		// Use the directory in place. We do NOT delete it on success —
		// the operator's source files aren't ours to clean up. The stage
		// dir (below) is the copy we own and remove.
		contentDir = job.SourcePath
		cleanupContent = func() {}

	default:
		// Single file: fabricate a one-item directory so the rest of the
		// pipeline (staging, PAR2) doesn't need a special case.
		wrap := filepath.Join(cfg.TempDir, "wrap-"+GenerateRandomPassword(8))
		if err := os.MkdirAll(wrap, 0755); err != nil {
			return "", "", fmt.Errorf("wrap dir: %w", err)
		}
		linkDst := filepath.Join(wrap, filepath.Base(job.SourcePath))
		// Hardlink first (free) and fall through to copy on cross-device,
		// same as CopyFiles' strategy — avoids allocating bytes twice for
		// multi-GB single files.
		if err := os.Link(job.SourcePath, linkDst); err != nil {
			if err := CopyFiles(ctx, job.SourcePath, wrap); err != nil {
				_ = os.RemoveAll(wrap)
				return "", "", fmt.Errorf("wrap copy: %w", err)
			}
		}
		contentDir = wrap
		cleanupContent = func() { _ = os.RemoveAll(wrap) }
	}
	defer cleanupContent()

	// ── 2b. Blocklist enforcement ─────────────────────────────────────────
	// Apply the group's per-type override when set; fall through to the
	// hardcoded default when the group leaves the list empty. A music
	// group that legitimately ships .iso files alongside audio can allow
	// them by setting an explicit list that excludes .iso.
	if n, byExt := RemoveBlockedFiles(contentDir, EffectiveBlocklist(group.BannedExtensions)); n > 0 {
		log.Printf("[offline %d] Removed %d blocked file(s) (%s)", job.ID, n, FormatExtCounts(byExt))
	}

	// ── 3. Samples (best-effort, type-aware) ──────────────────────────────
	// Done before staging so we can probe the original filenames — the
	// stage step may obfuscate them, which makes debugging a failed probe
	// harder. Sampling is non-fatal: a failure logs and moves on, same as
	// the online path's behaviour. Branch by group.Type:
	//   video → ffmpeg screenshots of the largest video
	//   manga → cover + N pages from the first archive
	//   music → N audio clips of `sampleSeconds` each
	//   other → skip sampling entirely (posts raw files)
	samples := generateOfflineSamples(ctx, db, job, cfg, contentDir, group, sampleCount, sampleSeconds)

	// The sample temp dir (offline-shots-/pages-/samples-<rand>) has no
	// cleanup of its own, and its prefix isn't reclaimed by the periodic
	// sweep — so any early return below (stage/encrypt/upload/nzb failure)
	// would orphan real PNG/audio bytes indefinitely. moveSamplesInto
	// relocates the files into the release folder first on the success
	// path, so force-removing the now-empty dir here is safe either way.
	if len(samples) > 0 {
		defer os.RemoveAll(filepath.Dir(samples[0]))
	}

	// ── 4. Stage (obfuscate or copy) ──────────────────────────────────────
	stageDir := filepath.Join(cfg.TempDir, "offline-stage-"+GenerateRandomPassword(12))
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return "", "", fmt.Errorf("stage dir: %w", err)
	}
	defer os.RemoveAll(stageDir)

	phase("staging", 0)
	if jobCfg.Obfuscate {
		if err := ObfuscateFiles(ctx, contentDir, stageDir); err != nil {
			return "", "", fmt.Errorf("obfuscate: %w", err)
		}
	} else {
		if err := CopyFiles(ctx, contentDir, stageDir); err != nil {
			return "", "", fmt.Errorf("stage copy: %w", err)
		}
	}

	// ── 4. PAR2 ───────────────────────────────────────────────────────────
	phase("par2", 0)
	baseName := GenerateRandomPassword(12)
	if !jobCfg.Obfuscate {
		baseName = SanitizeBaseName(job.Title)
	}
	_, err = GeneratePAR2(ctx, stageDir, baseName, PAR2Options{
		Redundancy: jobCfg.PAR2Redundancy,
		BlockSize:  ChunkSize,
		Threads:    jobCfg.PAR2Threads,
		MemoryMB:   jobCfg.PAR2Memory,
	}, func(p string, pct float64) {
		_ = db.UpdateJobPhase(job.ID, "par2:"+p, pct)
	})
	if err != nil {
		// Match the online path: PAR2 failure is non-fatal (posts without
		// recovery). We surface the warning in the log but keep going.
		log.Printf("[offline %d] PAR2 warning (non-fatal): %v", job.ID, err)
	}

	// ── 5. Optional encryption ────────────────────────────────────────────
	var password string
	uploadDir := stageDir
	if jobCfg.Encrypt {
		phase("encrypting", 0)
		password = GenerateRandomPassword(16)
		archiveName := GenerateRandomPassword(16) + ".7z"
		archivePath := filepath.Join(cfg.TempDir, archiveName)
		// Register cleanup BEFORE the fallible encrypt: a 7z failure that
		// already wrote a partial archive must not orphan a bare <rand>.7z
		// in TempDir (the online path in cmd/agent/main.go orders it this
		// way too).
		defer os.Remove(archivePath)
		if err := EncryptWith7z(ctx, stageDir, archivePath, password); err != nil {
			return "", "", fmt.Errorf("encrypt: %w", err)
		}
		encDir := filepath.Join(cfg.TempDir, "offline-enc-"+GenerateRandomPassword(8))
		if err := os.MkdirAll(encDir, 0755); err != nil {
			return "", "", fmt.Errorf("enc dir: %w", err)
		}
		defer os.RemoveAll(encDir)
		if err := os.Rename(archivePath, filepath.Join(encDir, archiveName)); err != nil {
			return "", "", fmt.Errorf("enc rename: %w", err)
		}
		uploadDir = encDir
	}

	// ── 6. Upload (serialised with the online pipeline) ───────────────────
	phase("queued-upload", 0)
	UploadSlot.Lock()
	defer UploadSlot.Unlock()

	phase("uploading", 0)
	fileSegments, err := UploadDirectory(ctx, &jobCfg, uploadDir, jobName)
	if err != nil {
		return "", "", fmt.Errorf("upload: %w", err)
	}

	// ── 7. NZB assembly ───────────────────────────────────────────────────
	phase("nzb", 0)
	nzbData, err := CreateMultiFileNZBBytes(&jobCfg, fileSegments, password, NZBMetaInfo{
		Title: job.Title,
	})
	if err != nil {
		return "", "", fmt.Errorf("nzb: %w", err)
	}

	// ── 8. Save NZB + sidecars to output dir ──────────────────────────────
	// Folder-per-release layout keeps each job self-contained — NZB,
	// password (if encrypted), and screenshots live together so the
	// operator can ship the whole folder to another indexer in one copy.
	safeTitle := SanitizeBaseName(job.Title)
	releaseDir := filepath.Join(offlineOutputDir(cfg), sanitizeDirName(group.Name), safeTitle)
	if err := os.MkdirAll(releaseDir, 0755); err != nil {
		return "", "", fmt.Errorf("output dir: %w", err)
	}
	nzbFile := filepath.Join(releaseDir, safeTitle+".nzb")
	if err := os.WriteFile(nzbFile, nzbData, 0644); err != nil {
		return "", "", fmt.Errorf("write nzb: %w", err)
	}
	if password != "" {
		pwFile := filepath.Join(releaseDir, "password.txt")
		_ = os.WriteFile(pwFile, []byte(password+"\n"), 0600)
	}
	if len(samples) > 0 {
		moveSamplesInto(samples, filepath.Join(releaseDir, "samples"))
	}
	return nzbFile, password, nil
}

// generateOfflineSamples routes to the right primitive for the group's
// type. "Samples" here is the generic term for what a pipeline generates
// before upload to preview the release — PNG stills, manga pages, or
// audio clips depending on content. All three variants are non-fatal:
// an empty return just means "no previews this run" and the pipeline
// continues to stage + post the raw files, matching the online path's
// "warning (non-fatal)" behaviour.
//
// An unknown type (custom label introduced by site admins for some new
// category) simply skips sampling — posting still works; operators can
// add a preview pipeline later without breaking the current release.
func generateOfflineSamples(ctx context.Context, db *storage.DB, job *storage.OfflineJob, cfg *config.Config, contentDir string, group *storage.Group, count, seconds int) []string {
	switch group.Type {
	case "video", "":
		return offlineVideoScreenshots(ctx, db, job, cfg, contentDir, count, group.WatermarkText)
	case "manga":
		return offlineMangaPages(ctx, db, job, cfg, contentDir, count)
	case "music":
		return offlineAudioSamples(ctx, db, job, cfg, contentDir, count, seconds)
	default:
		log.Printf("[offline %d] type %q has no sampling strategy, skipping", job.ID, group.Type)
		return nil
	}
}

func offlineVideoScreenshots(ctx context.Context, db *storage.DB, job *storage.OfflineJob, cfg *config.Config, contentDir string, count int, watermark string) []string {
	_ = db.UpdateJobPhase(job.ID, "screenshots", 0)
	videos := FindVideoFiles(contentDir)
	if len(videos) == 0 {
		return nil
	}
	main := videos[0]
	info, err := ProbeVideo(ctx, main)
	if err != nil {
		log.Printf("[offline %d] probe warning (non-fatal): %v", job.ID, err)
		return nil
	}
	if info.Duration < 10 {
		return nil
	}
	outDir := filepath.Join(cfg.TempDir, "offline-shots-"+GenerateRandomPassword(8))
	paths, err := GenerateScreenshotsWatermarked(ctx, main, outDir, info.Duration, count, watermark)
	if err != nil {
		log.Printf("[offline %d] screenshot warning (non-fatal): %v", job.ID, err)
		_ = os.RemoveAll(outDir)
		return nil
	}
	return paths
}

func offlineMangaPages(ctx context.Context, db *storage.DB, job *storage.OfflineJob, cfg *config.Config, contentDir string, count int) []string {
	_ = db.UpdateJobPhase(job.ID, "pages", 0)
	archive := FindMangaArchive(contentDir)
	if archive == "" {
		return nil
	}
	outDir := filepath.Join(cfg.TempDir, "offline-pages-"+GenerateRandomPassword(8))
	paths, err := GenerateMangaScreenshots(ctx, archive, outDir, count)
	if err != nil {
		log.Printf("[offline %d] manga pages warning (non-fatal): %v", job.ID, err)
		_ = os.RemoveAll(outDir)
		return nil
	}
	return paths
}

func offlineAudioSamples(ctx context.Context, db *storage.DB, job *storage.OfflineJob, cfg *config.Config, contentDir string, count, seconds int) []string {
	_ = db.UpdateJobPhase(job.ID, "samples", 0)
	audio := FindAudioFiles(contentDir)
	if len(audio) == 0 {
		return nil
	}
	main := audio[0]
	// ffprobe works on audio containers too — Duration comes from the
	// same field as video files do.
	info, err := ProbeVideo(ctx, main)
	if err != nil {
		log.Printf("[offline %d] audio probe warning (non-fatal): %v", job.ID, err)
		return nil
	}
	if info.Duration < float64(seconds) {
		log.Printf("[offline %d] audio track too short (%.1fs) for %ds samples", job.ID, info.Duration, seconds)
		return nil
	}
	outDir := filepath.Join(cfg.TempDir, "offline-samples-"+GenerateRandomPassword(8))
	paths, err := GenerateAudioSamples(ctx, main, outDir, info.Duration, count, seconds)
	if err != nil {
		log.Printf("[offline %d] audio sample warning (non-fatal): %v", job.ID, err)
		_ = os.RemoveAll(outDir)
		return nil
	}
	return paths
}

// moveSamplesInto relocates sample files from their temp dir into the
// release folder under samples/, keeping the numbered filenames so they
// render in natural order (timeline, page, clip index) when viewed in a
// file browser. Best-effort — the pipeline has already succeeded by
// this point, so a move failure logs and leaves files where they are.
func moveSamplesInto(paths []string, dst string) {
	if err := os.MkdirAll(dst, 0755); err != nil {
		log.Printf("samples: mkdir %s: %v", dst, err)
		return
	}
	for _, p := range paths {
		target := filepath.Join(dst, filepath.Base(p))
		if err := os.Rename(p, target); err != nil {
			// Cross-device rename fails on some Docker volume setups;
			// fall back to a copy so the samples still reach the
			// release folder.
			if data, rerr := os.ReadFile(p); rerr == nil {
				if werr := os.WriteFile(target, data, 0644); werr == nil {
					_ = os.Remove(p)
					continue
				}
			}
			log.Printf("samples: move %s → %s: %v", p, target, err)
		}
	}
	// The source temp dir is force-removed by processOfflineJob's deferred
	// os.RemoveAll (covers both this success path and every failure path),
	// so there's nothing to clean up here.
}

// offlineOutputDir resolves the base directory for written NZBs. Env
// OFFLINE_OUTPUT_DIR wins; otherwise the default sits next to TempDir
// (so a Docker /data volume gets /data/offline-output cleanly).
func offlineOutputDir(cfg *config.Config) string {
	if p := os.Getenv("OFFLINE_OUTPUT_DIR"); p != "" {
		return p
	}
	// filepath.Dir of /data/temp → /data → /data/offline-output.
	return filepath.Join(filepath.Dir(cfg.TempDir), "offline-output")
}

// sanitizeDirName strips characters that are iffy on Windows volumes
// and collapses whitespace so "My Anime!" becomes "My_Anime". Keep it
// conservative — group names are operator-picked, so this is a safety
// net rather than a first line of defence.
func sanitizeDirName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unnamed"
	}
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '<', '>', ':', '"', '/', '\\', '|', '?', '*':
			b.WriteByte('_')
		case ' ':
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
