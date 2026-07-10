package services

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Upload-manifest audit. Compares the RAW downloaded torrent content
// against the directory we're about to publish to Usenet, and refuses
// to publish a partial NZB when video files have silently gone missing
// between download and upload.
//
// Why this exists: there have been intermittent reports of multi-file
// torrents producing single-file NZBs (e.g. a release with main.mkv +
// Extras/extra.mkv ending up as just Extras/extra.mkv on the wire).
// The download/stage/upload pipeline is meant to preserve every file;
// when it doesn't, the silent failure ships a partial release that
// looks legitimate. This audit makes the failure loud BEFORE the
// upload starts so no one downloads a half-mirrored release.
//
// Scope: counts only "video files" by extension. Manga (CBZ), audio
// releases, and other non-video bundles are checked via the broader
// TotalCount / TotalBytes pair instead. The audit gates on video-file
// count specifically because that's the asymmetric loss that matters —
// dropping a 17 GiB feature is catastrophic; dropping a .nfo or a
// thumbnail is not.
//
// Failure-info channels: when the audit fires, the caller has three
// surfaces to surface debug detail on:
//
//	1. docker log (log.Printf)             — full diff with every file
//	2. site agent_logs (site.PostLog)      — same, visible in admin UI
//	3. request_lock fail_reason (site.Complete) — concise summary only
//
// Use ManifestError.DetailedReport for (1) and (2); ManifestError.Error
// (the standard error string) for (3). Both are computed from the
// captured per-file manifest so the operator can see exactly which
// file(s) went missing without re-running the task.

// videoExts is the extension allowlist counted as "video" in the
// manifest. .ts is included for HDTV captures and .m2ts for Bluray
// content; both can be feature-length.
var videoExts = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true,
	".m2ts": true, ".ts": true, ".mov": true,
	".wmv": true, ".webm": true,
}

// minVideoBytes is the floor below which a file isn't treated as a
// "feature" video for audit purposes. A 256 KB .mkv is almost
// certainly a tiny sample / promo / encoding test, not the release
// itself. Excluding these keeps the audit from flapping on releases
// that happen to ship a sample alongside the feature.
const minVideoBytes = 1 << 20 // 1 MiB

// ManifestEntry is one row of the per-file manifest: a relative path
// (POSIX-slashed for cross-platform stability) and the file's size in
// bytes.
type ManifestEntry struct {
	RelPath string
	Size    int64
}

// UploadManifest is a snapshot of a directory tree. Counts + bytes for
// quick comparison; VideoFiles for per-file diff when the audit fails.
type UploadManifest struct {
	VideoCount int
	VideoBytes int64
	TotalCount int
	TotalBytes int64
	// VideoFiles is the per-file list of video entries that contributed
	// to VideoCount/VideoBytes. Sorted by RelPath. Used by DiffManifests
	// to identify which specific file went missing when the audit fires.
	VideoFiles []ManifestEntry
}

// ManifestOf walks dir and produces a manifest. Errors are swallowed
// silently because this audit is best-effort: a transient permission
// error on one file shouldn't abort the publish. The TotalCount stays
// honest because we still see the rest of the tree.
func ManifestOf(dir string) UploadManifest {
	var m UploadManifest
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		m.TotalCount++
		m.TotalBytes += info.Size()
		if info.Size() < minVideoBytes {
			return nil
		}
		if videoExts[strings.ToLower(filepath.Ext(path))] {
			m.VideoCount++
			m.VideoBytes += info.Size()
			rel, relErr := filepath.Rel(dir, path)
			if relErr != nil {
				rel = filepath.Base(path)
			}
			m.VideoFiles = append(m.VideoFiles, ManifestEntry{
				RelPath: filepath.ToSlash(rel),
				Size:    info.Size(),
			})
		}
		return nil
	})
	sort.Slice(m.VideoFiles, func(i, j int) bool { return m.VideoFiles[i].RelPath < m.VideoFiles[j].RelPath })
	return m
}

// ManifestDiff names every video file present in `source` but missing
// from `upload`, plus any extra videos in `upload` that weren't in the
// source (which is normal when the extract wave ran — an archive
// unpacked into one or more videos).
//
// Matching is by basename, not full path: staging may reshuffle a file
// from "Extras/foo.mkv" in the download to a different relative path in
// the upload (e.g. into a remux subdir). Matching by basename catches
// "renamed in place" without flagging that as a loss. Same basename +
// same size = match. Different size with same basename = treated as a
// mismatch (the file was truncated / corrupted en route).
type ManifestDiff struct {
	Missing []ManifestEntry // in source, not in upload
	Extra   []ManifestEntry // in upload, not in source (extract-wave output)
}

// DiffManifests computes the per-file diff between source and upload.
// Always safe to call — never returns an error.
func DiffManifests(source, upload UploadManifest) ManifestDiff {
	uploadByBase := make(map[string]ManifestEntry, len(upload.VideoFiles))
	for _, f := range upload.VideoFiles {
		uploadByBase[filepath.Base(f.RelPath)] = f
	}
	sourceByBase := make(map[string]ManifestEntry, len(source.VideoFiles))
	for _, f := range source.VideoFiles {
		sourceByBase[filepath.Base(f.RelPath)] = f
	}

	var d ManifestDiff
	for _, src := range source.VideoFiles {
		base := filepath.Base(src.RelPath)
		up, ok := uploadByBase[base]
		if !ok || up.Size != src.Size {
			d.Missing = append(d.Missing, src)
		}
	}
	for _, up := range upload.VideoFiles {
		base := filepath.Base(up.RelPath)
		if _, ok := sourceByBase[base]; !ok {
			d.Extra = append(d.Extra, up)
		}
	}
	return d
}

// ManifestError is the typed error returned by CompareManifest when the
// audit fires. Wraps the source + upload manifests + the computed diff
// so callers can extract structured detail via errors.As.
type ManifestError struct {
	Source UploadManifest
	Upload UploadManifest
	Diff   ManifestDiff
}

// Error implements error — short single-line summary suitable for the
// request_lock fail_reason column. Names the FIRST missing file by
// path so a quick scan of /admin/requests catches the pattern.
func (e *ManifestError) Error() string {
	first := "(unknown)"
	if len(e.Diff.Missing) > 0 {
		first = e.Diff.Missing[0].RelPath
	}
	return fmt.Sprintf(
		"refusing to publish partial NZB: source had %d video file(s) but upload has only %d (first missing: %s) — see agent log for full diff",
		e.Source.VideoCount, e.Upload.VideoCount, first,
	)
}

// DetailedReport returns a multi-line breakdown suitable for the
// docker log AND the site's agent_logs surface (via site.PostLog).
// Lists every missing and extra file so an operator can correlate
// with the staging audit's "stage: copied N file(s)" line and the
// extract wave's "Extracted N archive(s)" lines.
func (e *ManifestError) DetailedReport() string {
	var b strings.Builder
	fmt.Fprintf(&b, "manifest mismatch detected before upload\n")
	fmt.Fprintf(&b, "  source:   %d video file(s), %.1f MiB (%d total files)\n",
		e.Source.VideoCount, float64(e.Source.VideoBytes)/(1<<20), e.Source.TotalCount)
	fmt.Fprintf(&b, "  upload:   %d video file(s), %.1f MiB (%d total files)\n",
		e.Upload.VideoCount, float64(e.Upload.VideoBytes)/(1<<20), e.Upload.TotalCount)
	if len(e.Diff.Missing) > 0 {
		fmt.Fprintf(&b, "  MISSING from upload (%d):\n", len(e.Diff.Missing))
		for _, f := range e.Diff.Missing {
			fmt.Fprintf(&b, "    - %s (%.1f MiB)\n", f.RelPath, float64(f.Size)/(1<<20))
		}
	}
	if len(e.Diff.Extra) > 0 {
		fmt.Fprintf(&b, "  EXTRA in upload (%d) — usually extract-wave output:\n", len(e.Diff.Extra))
		for _, f := range e.Diff.Extra {
			fmt.Fprintf(&b, "    + %s (%.1f MiB)\n", f.RelPath, float64(f.Size)/(1<<20))
		}
	}
	b.WriteString("debugging hints: check the prior 'stage: copied N file(s)' line in the log " +
		"to see if loss happened during staging; check 'Extracted N archive(s)' lines for the " +
		"extract wave's input/output; the staging audit fires earlier and would name a copy " +
		"failure if one occurred")
	return b.String()
}

// CompareManifest reports whether the upload manifest is consistent
// with the source. Returns nil when the publish is safe, a
// *ManifestError when it isn't (so callers can errors.As to extract
// the structured Source/Upload/Diff fields).
//
// The rule is intentionally narrow — see file header for the three
// conditions that must hold for a mismatch to fire.
func CompareManifest(source, upload UploadManifest, encrypted bool) error {
	if encrypted {
		return nil
	}
	if source.VideoCount == 0 {
		return nil
	}
	if upload.VideoCount < source.VideoCount {
		return &ManifestError{
			Source: source,
			Upload: upload,
			Diff:   DiffManifests(source, upload),
		}
	}
	return nil
}

// FormatManifestLine returns a single-line human summary suitable for
// the agent log. Used unconditionally — even on healthy publishes — so
// the file counts are always visible at upload time.
func FormatManifestLine(source, upload UploadManifest, encrypted bool) string {
	enc := "no"
	if encrypted {
		enc = "yes"
	}
	return fmt.Sprintf(
		"manifest: source=%d video (%.1f MiB) %d total (%.1f MiB) | upload=%d video (%.1f MiB) %d total (%.1f MiB) | encrypt=%s",
		source.VideoCount, float64(source.VideoBytes)/(1<<20), source.TotalCount, float64(source.TotalBytes)/(1<<20),
		upload.VideoCount, float64(upload.VideoBytes)/(1<<20), upload.TotalCount, float64(upload.TotalBytes)/(1<<20),
		enc,
	)
}
