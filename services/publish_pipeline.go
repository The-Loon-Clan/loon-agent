package services

// The publish pipeline: everything between "a directory of content on disk"
// and "an NZB blob describing it on Usenet", as ONE function both producers
// share.
//
// Two callers:
//   - the task path (cmd/agent: site-dispatched download → publish)
//   - the offer fulfiller (services/offer_fulfill.go: serve a member request
//     from local inventory or a remote tracker)
//
// It exists because the second caller grew by copying fragments of the first:
// the offer path shipped without PAR2, then with PAR2 but without screenshots,
// metadata, obfuscation, the manifest audit, or the upload slot — each gap a
// separate incident report. A release delivered through an offer and one
// produced by a task are the same artefact and must go through the same
// stages; from here on a stage added to the pipeline exists for both.
//
// Stage order (numbers kept from the original task pipeline for log grep):
//   2.  Probe video metadata            (Describe)
//   3.  Screenshots (video or manga)    (Describe)
//   3b. Subtitle extraction             (Describe)
//   3c. Audio track catalog             (Describe)
//   3d. Acoustic fingerprints           (Describe)
//   3e. Dominant colour palette         (Describe)
//   3f. Manga OCR                       (Describe)
//   4.  Stage into a fresh dir (obfuscate or link/copy + allowlist sweep)
//   4.5–4.10. Unpack archives (rar/zip/7z/iso/tar/misc)
//   5.  PAR2 recovery
//   6.  Optional 7z encryption
//   7.  Upload slot → manifest audit → NNTP upload
//   8.  NZB assembly
//
// The pipeline reports through OPTIONAL hooks and returns errors instead of
// terminating the task — what a failure means (fail the lock, release the
// claim) belongs to the caller.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/the-loon-clan/loon-agent/client"
	"github.com/the-loon-clan/loon-agent/config"
	"github.com/the-loon-clan/loon-agent/storage"
)

// PublishJob is one directory's trip to Usenet.
type PublishJob struct {
	Cfg     *config.Config
	JobName string
	// Title is the release name: the upload subject base and the NZB's meta
	// title. RequestID is stamped into the NZB meta when non-zero.
	Title     string
	RequestID int64
	// ContentDir holds the source files. The pipeline never modifies it
	// (staging copies/links out of it), and the description artefacts it
	// creates (_screenshots, _subtitles) live INSIDE it so they share the
	// caller's cleanup — the paths in PublishResult point there and must
	// outlive the pipeline long enough to be uploaded to the site.
	ContentDir string
	// Describe runs the metadata pass (probe, screenshots, subtitles, audio,
	// fingerprints, palette, OCR). Costs ffmpeg/mkvmerge time; a caller that
	// cannot use the artefacts can skip it.
	Describe bool

	// Hooks, all optional. Progress is the human strip ("PAR2", "45%"),
	// FileProgress the dashboard's per-task entry, PostLog the site-visible
	// agent log for failures worth an /admin/errors trail.
	Progress     func(step, detail string)
	FileProgress func(fp *client.FileProgress)
	PostLog      func(level, msg string)
}

// PublishResult is everything the caller needs to report the release.
type PublishResult struct {
	NzbData         []byte
	Password        string
	VideoInfo       *VideoInfo
	Screenshots     []string
	Subtitles       []client.SubtitleUpload
	AudioTracks     []client.AudioTrackUpload
	Fingerprints    []client.AudioFingerprintUpload
	DominantPalette []string
	OCRText         string
	OCRLanguage     string
	Stages          map[string]client.StageRecord
}

// PublishError names the stage that sank the pipeline, so a caller keeping
// per-stage failure labels (the task path's fail()) does not lose them.
type PublishError struct {
	Step string // "Prepare", "Encrypt", "Upload", "NZB", "ManifestMismatch"
	Msg  string
	Err  error
}

func (e *PublishError) Error() string { return fmt.Sprintf("%s: %s: %v", e.Step, e.Msg, e.Err) }
func (e *PublishError) Unwrap() error { return e.Err }

func publishFail(step, msg string, err error) error {
	return &PublishError{Step: step, Msg: msg, Err: err}
}

// PublishDirectory runs the whole pipeline. On success the returned NZB is on
// Usenet and the result carries every sidecar the site's report surfaces
// accept; on failure nothing needs undoing beyond what the caller's own
// cleanup already covers.
func PublishDirectory(ctx context.Context, job PublishJob) (*PublishResult, error) {
	cfg := job.Cfg
	label := job.JobName
	progress := job.Progress
	if progress == nil {
		progress = func(string, string) {}
	}
	fileProgress := job.FileProgress
	if fileProgress == nil {
		fileProgress = func(*client.FileProgress) {}
	}
	postLog := job.PostLog
	if postLog == nil {
		postLog = func(string, string) {}
	}

	res := &PublishResult{Stages: map[string]client.StageRecord{}}
	stageOK := func(name string, count int, note string) {
		res.Stages[name] = client.StageRecord{Status: "ok", Count: count, Note: note}
	}
	stageEmpty := func(name, note string) {
		res.Stages[name] = client.StageRecord{Status: "empty", Note: note}
	}
	stageSkipped := func(name, note string) {
		res.Stages[name] = client.StageRecord{Status: "skipped", Note: note}
	}
	stageFailed := func(name, note string) {
		// Cap reason length so a multi-line exec.Command error doesn't
		// bloat the row or hit the storage layer's 4 KiB JSON cap.
		if len(note) > 200 {
			note = note[:200]
		}
		res.Stages[name] = client.StageRecord{Status: "failed", Note: note}
	}

	if job.Describe {
		describeContent(ctx, job, res, stageOK, stageEmpty, stageSkipped, stageFailed)
	}

	// ── 4. Stage into a fresh directory ────────────────────────────────────
	// "stage-" prefix gives SweepOrphanDownloads a recognizable shape so
	// stage dirs left behind by a force-killed agent get cleaned up like
	// dl-* do. SetJobStagePath publishes the dir to the orphan-sweep
	// keep-set: a job queued behind a slow upload can wait long enough that
	// the 30-min stale-dir sweep would otherwise wipe its stage mid-flight.
	stageDir := filepath.Join(cfg.TempDir, "stage-"+GenerateRandomPassword(12))
	if err := os.MkdirAll(stageDir, 0755); err != nil {
		return nil, publishFail("Prepare", "Stage dir error", err)
	}
	storage.SetJobStagePath(job.JobName, stageDir)
	defer func() {
		os.RemoveAll(stageDir)
		storage.SetJobStagePath(job.JobName, "")
	}()

	if cfg.Obfuscate {
		progress("Preparing", "Obfuscating filenames...")
		if err := ObfuscateFiles(ctx, job.ContentDir, stageDir); err != nil {
			return nil, publishFail("Prepare", "Prepare error", err)
		}
	} else {
		progress("Preparing", "Copying files...")
		if err := CopyFiles(ctx, job.ContentDir, stageDir); err != nil {
			return nil, publishFail("Prepare", "Prepare error", err)
		}
	}
	log.Printf("[%s] Step 4: Staging complete", label)

	// ── 4.5–4.10. Unpack archives ───────────────────────────────────────────
	// The PAR2 step that follows generates recovery for the EXTRACTED media,
	// and the upload posts real content instead of an archive wrapper. Each
	// extractor is a silent no-op when its format (or binary) is absent;
	// partial-success errors are logged and the task continues — anything
	// that did extract is kept.
	type extractor struct {
		step, what string
		run        func(context.Context, string, func(string)) (int, error)
	}
	for _, ex := range []extractor{
		{"4.5", "RAR archives", ExtractRARArchives},
		{"4.6", "ZIP archives", ExtractZIPArchives},
		{"4.7", "7z archives", Extract7zArchives},
		{"4.8", "ISO disc images", ExtractISOArchives},
		{"4.9", "tar archives", ExtractTarArchives},
		{"4.10", "lzh/cab/arj/cpio archives", ExtractMiscArchives},
	} {
		log.Printf("[%s] Step %s: Scanning for %s...", label, ex.step, ex.what)
		if extracted, err := ex.run(ctx, stageDir, func(msg string) {
			progress("Extract", msg)
		}); err != nil {
			log.Printf("[%s] Step %s: extract warning (extracted=%d): %v", label, ex.step, extracted, err)
		} else if extracted > 0 {
			log.Printf("[%s] Step %s: Extracted %d %s", label, ex.step, extracted, ex.what)
		}
	}

	// ── 5. PAR2 recovery ────────────────────────────────────────────────────
	log.Printf("[%s] Step 5: Generating PAR2 recovery data...", label)
	progress("PAR2", "Generating recovery data...")
	fileProgress(&client.FileProgress{Name: job.Title, Phase: "par2"})
	baseName := GenerateRandomPassword(12)
	if !cfg.Obfuscate {
		baseName = SanitizeBaseName(job.Title)
		if baseName == "" {
			baseName = job.JobName
		}
	}
	par2Start := time.Now()
	par2Progress := func(phase string, pct float64) {
		elapsed := time.Since(par2Start).Round(time.Second)
		progress("PAR2", fmt.Sprintf("%s %.0f%% (%s elapsed)", phase, pct, elapsed))
		fileProgress(&client.FileProgress{Name: job.Title, Phase: "par2", Percent: pct})
	}
	par2Files, err := GeneratePAR2(ctx, stageDir, baseName, PAR2Options{
		Redundancy: cfg.PAR2Redundancy,
		BlockSize:  ChunkSize,
		Threads:    cfg.PAR2Threads,
		MemoryMB:   cfg.PAR2Memory,
	}, par2Progress)
	switch {
	case err != nil:
		// Non-fatal, in three places: the agent log (full context), the
		// site's agent_logs (so /admin/errors shows a flapping par2 binary
		// without docker access), and the release checklist as "empty" — an
		// end user sees a release without recovery, not a broken one.
		log.Printf("[%s] PAR2 FAILED (non-fatal) for %q in %q: %v", label, baseName, stageDir, err)
		postLog("error", fmt.Sprintf(
			"PAR2 generation failed for %s (%s): %v — release shipped to Usenet without recovery files (parity loss is now unrecoverable)",
			label, job.Title, err))
		progress("PAR2", "PAR2 failed, uploading without recovery — admin/errors has details")
		stageEmpty("par2", "no recovery files generated (see /admin/errors for the underlying binary error)")
	case len(par2Files) == 0:
		// Defensive: nil error but zero files — the walker looked at the
		// wrong dir or the binary completed without writing output.
		postLog("warn", fmt.Sprintf(
			"PAR2 reported success but produced ZERO .par2 files for %s (%s). Stage dir: %s. Upload continuing without recovery.",
			label, job.Title, stageDir))
		progress("PAR2", "PAR2 produced no files — uploading without recovery")
		stageEmpty("par2", "binary returned success but no files were produced (see /admin/errors)")
	default:
		var par2Size int64
		for _, p := range par2Files {
			if info, statErr := os.Stat(p); statErr == nil {
				par2Size += info.Size()
			}
		}
		log.Printf("[%s] Step 5: PAR2 complete in %s — %d recovery file(s) generated",
			label, time.Since(par2Start).Round(time.Second), len(par2Files))
		stageOK("par2", len(par2Files),
			fmt.Sprintf("%.1f MB recovery at %d%%", float64(par2Size)/(1024*1024), cfg.PAR2Redundancy))
	}

	// ── 6. Optional encryption ──────────────────────────────────────────────
	uploadDir := stageDir
	if cfg.Encrypt {
		res.Password = GenerateRandomPassword(16)
		archiveName := GenerateRandomPassword(16) + ".7z"
		archivePath := filepath.Join(cfg.TempDir, archiveName)
		defer os.Remove(archivePath)

		log.Printf("[%s] Step 6: Encrypting with 7z...", label)
		progress("Encrypting", "Creating password-protected 7z archive...")
		fileProgress(&client.FileProgress{Name: job.Title, Phase: "encrypting"})
		if err := EncryptWith7z(ctx, stageDir, archivePath, res.Password); err != nil {
			return nil, publishFail("Encrypt", "Encryption error", err)
		}
		encDir := filepath.Join(cfg.TempDir, "enc-"+GenerateRandomPassword(8))
		os.MkdirAll(encDir, 0755)
		defer os.RemoveAll(encDir)
		os.Rename(archivePath, filepath.Join(encDir, archiveName))
		uploadDir = encDir
		log.Printf("[%s] Encrypted to %s (%d chars password)", label, archiveName, len(res.Password))
	}

	// ── 7. Upload (serialized — one NNTP upload at a time) ─────────────────
	var totalUploadSize int64
	filepath.Walk(uploadDir, func(_ string, info os.FileInfo, _ error) error {
		if info != nil && !info.IsDir() {
			totalUploadSize += info.Size()
		}
		return nil
	})
	log.Printf("[%s] Step 7: Waiting for upload slot (%.1f MiB to upload)...",
		label, float64(totalUploadSize)/1024/1024)
	progress("Queued", "Waiting for upload slot...")
	fileProgress(&client.FileProgress{Name: job.Title, Phase: "queued", Size: totalUploadSize})

	// The slot covers the NNTP upload only; the offer path historically
	// skipped it entirely and uploaded concurrently with tasks, halving both.
	slotWaitStart := time.Now()
	UploadSlot.Lock()
	slotHeldSince := time.Now()
	if w := time.Since(slotWaitStart); w > 30*time.Second {
		log.Printf("[%s] Slot ACQUIRED (waited %s)", label, w.Round(time.Second))
	} else {
		log.Printf("[%s] Slot ACQUIRED", label)
	}
	var unlockOnce sync.Once
	releaseSlot := func() {
		unlockOnce.Do(func() {
			UploadSlot.Unlock()
			log.Printf("[%s] Slot RELEASED (held %s)", label, time.Since(slotHeldSince).Round(time.Second))
		})
	}
	// Belt for the deferred suspenders: a panic inside the critical section
	// must not strand the slot for every later task.
	defer func() {
		if r := recover(); r != nil {
			releaseSlot()
			panic(r)
		}
	}()
	defer releaseSlot()

	// Manifest audit: compare the source content against the directory about
	// to be published. Catches "multi-file torrent ships as a single-file
	// NZB" BEFORE an upload is burned on a partial release.
	srcManifest := ManifestOf(job.ContentDir)
	upManifest := ManifestOf(uploadDir)
	log.Printf("[%s] %s", label, FormatManifestLine(srcManifest, upManifest, cfg.Encrypt))
	if err := CompareManifest(srcManifest, upManifest, cfg.Encrypt); err != nil {
		var mfErr *ManifestError
		if errors.As(err, &mfErr) {
			report := mfErr.DetailedReport()
			log.Printf("[%s] %s", label, report)
			postLog("error", fmt.Sprintf("[%s] %s\n%s", label, job.Title, report))
		}
		return nil, publishFail("ManifestMismatch", "Manifest check failed — aborting publish", err)
	}

	log.Printf("[%s] Step 7: Uploading to Usenet: %.2f MiB via %d connections...",
		label, float64(totalUploadSize)/1024/1024, cfg.NNTPConnections)
	progress("Uploading", fmt.Sprintf("%.1f MiB via %d NNTP connections...",
		float64(totalUploadSize)/1024/1024, cfg.NNTPConnections))

	uploadStart := time.Now()
	fileSegments, err := UploadDirectory(ctx, cfg, uploadDir, job.Title, job.JobName)
	if err != nil {
		return nil, publishFail("Upload", "Upload error", err)
	}
	uploadDur := time.Since(uploadStart)
	log.Printf("[%s] Step 7: Upload complete: %.2f MiB in %s (%.1f MB/s)",
		label, float64(totalUploadSize)/1024/1024, uploadDur.Round(time.Second),
		float64(totalUploadSize)/1024/1024/uploadDur.Seconds())

	// ── 8. NZB assembly ─────────────────────────────────────────────────────
	log.Printf("[%s] Step 8: Generating NZB...", label)
	progress("Finalizing", "Generating NZB...")
	nzbData, err := CreateMultiFileNZBBytes(cfg, fileSegments, res.Password, NZBMetaInfo{
		Title:     job.Title,
		RequestID: job.RequestID,
	})
	if err != nil {
		return nil, publishFail("NZB", "NZB error", err)
	}
	res.NzbData = nzbData
	return res, nil
}

// describeContent is the metadata pass (steps 2–3f): probe, screenshots,
// subtitles, audio catalog, fingerprints, palette, OCR. Everything in it is
// non-fatal — a release that cannot be described is still worth publishing —
// so it records outcomes on the checklist and returns nothing.
func describeContent(ctx context.Context, job PublishJob, res *PublishResult,
	stageOK func(string, int, string), stageEmpty, stageSkipped, stageFailed func(string, string)) {
	label := job.JobName
	progress := job.Progress
	if progress == nil {
		progress = func(string, string) {}
	}
	fileProgress := job.FileProgress
	if fileProgress == nil {
		fileProgress = func(*client.FileProgress) {}
	}

	log.Printf("[%s] Step 2: Analyzing video metadata...", label)
	progress("Analyzing", "Extracting video metadata...")
	fileProgress(&client.FileProgress{Name: job.Title, Phase: "processing", Percent: 0})

	// isManga is set on the CBZ/EPUB branch so the OCR step knows to run
	// tesseract — anime screenshots aren't worth OCRing.
	var isManga bool
	videoFiles := FindVideoFiles(job.ContentDir)

	if len(videoFiles) > 0 {
		mainVideo := videoFiles[0] // largest video file
		info, err := ProbeVideo(ctx, mainVideo)
		if err != nil {
			log.Printf("[%s] Probe warning (non-fatal): %v", label, err)
			stageFailed("mediainfo", err.Error())
		} else {
			res.VideoInfo = info
			log.Printf("[%s] Video: %s %dx%d %s %s %s",
				label, info.VideoCodec, info.Width, info.Height,
				info.ResolutionLabel(), info.HDR, info.DurationStr())
			stageOK("mediainfo", 1, fmt.Sprintf("%s %s %s", info.VideoCodec, info.ResolutionLabel(), info.HDR))
		}

		// ── 3. Screenshots ─────────────────────────────────────────────
		if res.VideoInfo != nil && res.VideoInfo.Duration > 10 {
			log.Printf("[%s] Step 3: Generating screenshots...", label)
			progress("Screenshots", "Capturing preview images...")
			fileProgress(&client.FileProgress{Name: job.Title, Phase: "screenshots"})
			// Inside ContentDir so the dl-* keep-set protects it from the
			// disk_reserve_sweep (1.5.22), and so the caller's cleanup
			// removes it — the paths must outlive this pipeline: they are
			// uploaded to the site when the caller reports.
			screenDir := filepath.Join(job.ContentDir, "_screenshots")
			shots, err := GenerateScreenshots(ctx, mainVideo, screenDir, res.VideoInfo.Duration, 6)
			if err != nil {
				log.Printf("[%s] Screenshot warning (non-fatal): %v", label, err)
				stageFailed("screenshots", err.Error())
			} else if len(shots) > 0 {
				res.Screenshots = shots
				log.Printf("[%s] Generated %d screenshots", label, len(shots))
				stageOK("screenshots", len(shots), "")
			} else {
				stageEmpty("screenshots", "ffmpeg returned no frames")
			}
		} else if res.VideoInfo != nil {
			stageSkipped("screenshots", fmt.Sprintf("video too short (%s)", res.VideoInfo.DurationStr()))
		}
	} else if archive := FindMangaArchive(job.ContentDir); archive != "" {
		// Manga path: no video file, but there's a CBZ/EPUB. Extract sample
		// pages through the same screenshot pipeline.
		isManga = true
		log.Printf("[%s] Step 2: Found manga archive: %s", label, filepath.Base(archive))
		stageSkipped("mediainfo", "manga archive (no video file)")
		progress("Screenshots", "Extracting preview pages...")
		fileProgress(&client.FileProgress{Name: job.Title, Phase: "screenshots"})
		screenDir := filepath.Join(job.ContentDir, "_screenshots")
		shots, err := GenerateMangaScreenshots(ctx, archive, screenDir, 6)
		if err != nil {
			log.Printf("[%s] Manga screenshot warning (non-fatal): %v", label, err)
			stageFailed("screenshots", err.Error())
		} else if len(shots) > 0 {
			res.Screenshots = shots
			log.Printf("[%s] Extracted %d manga pages", label, len(shots))
			stageOK("screenshots", len(shots), "manga pages")
		} else {
			stageEmpty("screenshots", "manga archive yielded no extractable pages")
		}
	} else {
		stageSkipped("mediainfo", "no video file or manga archive found")
		stageSkipped("screenshots", "no video file or manga archive found")
	}

	// ── 3b. Subtitles ───────────────────────────────────────────────────
	subtitleDir := filepath.Join(job.ContentDir, "_subtitles")
	subStatus := SubtitleToolStatus()
	subtitleTracks, subErr := ExtractSubtitles(ctx, job.ContentDir, subtitleDir)
	switch {
	case subStatus != "":
		stageSkipped("subtitles", subStatus)
	case subErr != nil:
		log.Printf("[%s] subtitles: extraction failed (continuing): %v", label, subErr)
		stageFailed("subtitles", subErr.Error())
	case len(subtitleTracks) > 0:
		log.Printf("[%s] subtitles: extracted %d track(s)", label, len(subtitleTracks))
		stageOK("subtitles", len(subtitleTracks), summarizeTrackLangs(subtitleLangsOf(subtitleTracks)))
	default:
		stageEmpty("subtitles", "no subtitle tracks found in MKV containers")
	}
	for _, t := range subtitleTracks {
		res.Subtitles = append(res.Subtitles, client.SubtitleUpload{
			TrackIndex:   t.TrackIndex,
			Language:     t.Language,
			TrackName:    t.TrackName,
			Codec:        t.Codec,
			Forced:       t.Forced,
			DefaultTrack: t.DefaultTrack,
			Path:         t.File,
		})
	}

	// ── 3c. Audio track catalog ─────────────────────────────────────────
	audioTracks, audioErr := ProbeAudioTracks(ctx, job.ContentDir)
	switch {
	case audioErr != nil:
		log.Printf("[%s] audio: probe failed (continuing): %v", label, audioErr)
		stageFailed("audio_tracks", audioErr.Error())
	case len(audioTracks) > 0:
		log.Printf("[%s] audio: cataloged %d track(s)", label, len(audioTracks))
		stageOK("audio_tracks", len(audioTracks), summarizeTrackLangs(audioLangsOf(audioTracks)))
	default:
		stageEmpty("audio_tracks", "no audio tracks cataloged")
	}
	for _, t := range audioTracks {
		res.AudioTracks = append(res.AudioTracks, client.AudioTrackUpload{
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

	// ── 3d. Acoustic fingerprints ───────────────────────────────────────
	fingerprints, fpErr := FingerprintAudio(ctx, job.ContentDir)
	switch {
	case fpErr != nil:
		log.Printf("[%s] fingerprint: failed (continuing): %v", label, fpErr)
		stageFailed("audio_fingerprints", fpErr.Error())
	case len(fingerprints) > 0:
		log.Printf("[%s] fingerprint: generated for %d file(s)", label, len(fingerprints))
		stageOK("audio_fingerprints", len(fingerprints), "")
	default:
		stageEmpty("audio_fingerprints", "fpcalc produced no fingerprints (missing binary or no audio)")
	}
	for _, f := range fingerprints {
		res.Fingerprints = append(res.Fingerprints, client.AudioFingerprintUpload{
			SourceFilename:   f.SourceFilename,
			DurationSeconds:  f.DurationSeconds,
			AlgorithmVersion: f.AlgorithmVersion,
			Fingerprint:      f.Fingerprint,
		})
	}

	// ── 3e. Dominant palette ────────────────────────────────────────────
	res.DominantPalette = ExtractDominantPalette(res.Screenshots, 8)
	switch {
	case len(res.Screenshots) == 0:
		stageSkipped("dominant_palette", "no screenshots to sample")
	case len(res.DominantPalette) > 0:
		log.Printf("[%s] palette: %d colours from %d screenshot(s)", label, len(res.DominantPalette), len(res.Screenshots))
		stageOK("dominant_palette", len(res.DominantPalette), "")
	default:
		stageEmpty("dominant_palette", "bucket-histogram returned no colours")
	}

	// ── 3f. Manga OCR ───────────────────────────────────────────────────
	switch {
	case !isManga:
		stageSkipped("ocr", "anime release (OCR runs only on manga)")
	case len(res.Screenshots) == 0:
		stageSkipped("ocr", "no manga pages available to OCR")
	default:
		ocr := OCRMangaPages(ctx, res.Screenshots, "eng+jpn")
		res.OCRText, res.OCRLanguage = ocr.Text, ocr.Language
		if ocr.Text != "" {
			log.Printf("[%s] ocr: extracted %d chars from %d page(s) (%s)",
				label, len(ocr.Text), len(res.Screenshots), ocr.Language)
			stageOK("ocr", len(ocr.Text), ocr.Language)
		} else {
			stageEmpty("ocr", "tesseract produced no recognisable text")
		}
	}
}

// summarizeTrackLangs joins the first distinct languages for a checklist note.
func summarizeTrackLangs(langs []string) string {
	seen := map[string]struct{}{}
	var order []string
	for _, lang := range langs {
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

func subtitleLangsOf(tracks []SubtitleTrack) []string {
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, t.Language)
	}
	return out
}

func audioLangsOf(tracks []AudioCatalogTrack) []string {
	out := make([]string, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, t.Language)
	}
	return out
}
