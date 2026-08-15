package services

// Folder scanner — walks a local directory tree and yields one row
// per media file. Output rows feed the sync orchestrator's
// title-resolution + hash + register pipeline.
//
// Title parsing is intentionally minimal: filename without extension,
// stripped of common scene-tag noise. The site's TitleMatcher does
// the heavy lifting on the resolve-titles side (anime + manga with
// progressive-prefix + substring containment fallbacks), so the agent
// doesn't need its own anidb dump.

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// ScannedFile is one row the folder scanner produces. The sync
// orchestrator turns this into a client.OfferEntry after title
// resolution.
type ScannedFile struct {
	RawTitle   string // filename minus extension, lightly cleaned
	Path       string // absolute path on the agent's filesystem
	SizeBytes  int64
	Season     int    // 0 when unparseable
	Episode    int    // 0 when unparseable
	Resolution string // "1080p" / "720p" / "4k" / ""
	SourceTag  string // "bd-remux" / "web-dl" / "" — best-effort
}

// scanRe is a compact set of patterns we extract from the filename.
// More elaborate parsing is the site's job (when it resolves the
// title against the catalog).
var (
	reSeasonEp = regexp.MustCompile(`(?i)S(\d{1,2})E(\d{1,3})`)
	// reResolution / reSourceTag previously used `\b` which is ASCII-only
	// in Go's RE2 — a filename like "アニメ1080p.mkv" has no `\b` before
	// "1080p" because the CJK rune isn't a word character, so the regex
	// silently missed the hint and the release got bucketed without a
	// resolution / source tag. The tokens are distinctive enough that
	// loose matching (no surrounding boundary check) is fine in practice.
	reResolution = regexp.MustCompile(`(?i)(2160p|1080p|720p|480p|4k|uhd)`)
	reSourceTag  = regexp.MustCompile(`(?i)(bd-?remux|bdrip|brrip|web-?dl|webrip|hdtv|dvdrip)`)
	// reEpOnly keeps a non-ASCII-aware boundary on either side (matching
	// either string start/end or a non-alnum byte) so we still match
	// "ep01" inside "Show - ep01.mkv" but also "話01" with CJK adjacent.
	// Dropping `\b` entirely would let it match inside "episode10" → "e10".
	reEpOnly = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])[Ee](?:p(?:isode)?)?\s*(\d{1,3})(?:[^A-Za-z0-9]|$)`)
	// reDashEp is the fansub convention: "[Group] Show Title - 07 (1080p)".
	// It is the most common anime naming there is and nothing above matched
	// it, so season and episode both came back zero.
	//
	// WHY THAT MATTERED MORE THAN A MISSING FIELD. Season and episode are part
	// of the offer bucket identity (ComputeOfferHash). With both zero, every
	// episode of a season hashes to the SAME bucket — so publishing a
	// twelve-episode season created one offer, and a member requesting it got
	// whichever episode the offerer happened to resolve. Found 2026-08-15 by
	// reading what the walker actually emitted rather than what it was meant to.
	//
	// Deliberately strict about what follows the number: a bare " - 2024" in
	// "Some.Movie - 2024" must not read as episode 2024, and a 4-digit run is
	// refused outright by the {1,3} bound. The trailing group also refuses a
	// digit so "- 07p" or a split resolution cannot half-match.
	reDashEp = regexp.MustCompile(`(?:^|\s)-\s*(\d{1,3})(?:v\d)?(?:\s|$|[\(\[])`)
	// reSeasonOnly catches the season when it is stated separately from the
	// episode ("S3 - 07", "Season 2"). Anchored on a non-alphanumeric or
	// string start so "NCIS3" is not season 3.
	reSeasonOnly = regexp.MustCompile(`(?i)(?:^|[^A-Za-z0-9])S(?:eason)?\s*(\d{1,2})(?:[^A-Za-z0-9]|$)`)
	// Scene-tag suffix in []brackets at the start (e.g. "[SubsPlease]
	// Show Title - 03 (1080p) [ABCDEFG].mkv") — drop both leading and
	// trailing bracket groups so the title alone goes to the resolver.
	reBracketGroup = regexp.MustCompile(`\[[^\]]*\]`)
)

// knownSubdirs lists directory names that should be excluded from
// offer registration. These are supplementary content (extras, samples,
// behind-the-scenes) that should not be registered as standalone offers.
// When a multi-file release (main video + extras/) is scanned, only
// top-level files are registered; subdirectory videos are skipped to
// prevent fragmentation across multiple offer hashes.
var knownSubdirs = map[string]bool{
	"extras":            true,
	"extra":             true,
	"bonus":             true,
	"sample":            true,
	"samples":           true,
	"behind the scenes": true,
	"behind_the_scenes": true,
	"behindthescenes":   true,
	"shorts":            true,
	"ovas":              true,
	"ova":               true,
	"specials":          true,
	"special":           true,
	"trailers":          true,
	"credits":           true,
	"intros":            true,
	"outros":            true,
}

// isInSubdirectory returns true if the file path descends into a known
// subdirectory. This is used to filter out supplementary content during
// offer scanning so multi-file releases register as a single offer.
func isInSubdirectory(path, root string) bool {
	relPath, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	// Normalize the path to lowercase and forward slashes for comparison.
	parts := strings.Split(strings.ToLower(relPath), string(filepath.Separator))
	if len(parts) <= 1 {
		// No subdirectory — file is at the root level.
		return false
	}
	// Check if any directory component is in knownSubdirs.
	// parts[0] is the first directory level below root.
	subdir := strings.ToLower(parts[0])
	return knownSubdirs[subdir]
}

// ScanFolder walks `root` and emits ScannedFile rows for every file
// matching the extension allowlist + size floor. Hidden directories
// (.git, .DS_Store containers, etc.) are skipped. Symlinks are not
// followed to avoid infinite loops on poorly-shaped media libraries.
//
// Multi-file releases: only top-level video files are registered as offers.
// Files in known subdirectories (Extras, Sample, etc.) are skipped to
// prevent fragmentation — these are supplementary and better handled outside
// the offer system.
func ScanFolder(root string, extensions []string, sizeMinMB int) ([]ScannedFile, error) {
	if root == "" {
		return nil, nil
	}
	allowedExt := make(map[string]bool, len(extensions))
	for _, e := range extensions {
		allowedExt[strings.ToLower(e)] = true
	}
	if len(allowedExt) == 0 {
		// Sane defaults — most personal collections are mkv/mp4.
		allowedExt[".mkv"] = true
		allowedExt[".mp4"] = true
	}
	sizeMinBytes := int64(sizeMinMB) * 1024 * 1024

	var out []ScannedFile
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// Permissions / vanished file — skip the entry, keep walking.
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			if name != "." && strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !allowedExt[ext] {
			return nil
		}
		if info.Size() < sizeMinBytes {
			return nil
		}
		// Skip files in known subdirectories (Extras, Sample, etc.)
		// to prevent multi-file releases from fragmenting across
		// multiple offer registrations.
		if isInSubdirectory(path, root) {
			return nil
		}
		out = append(out, parseScannedFile(path, info.Size()))
		return nil
	})
	return out, err
}

// parseScannedFile pulls the minimal hints we need out of the
// filename. The site does the real catalog lookup; we just need a
// raw title clean enough that TitleMatcher's punctuation + prefix
// steps catch it.
func parseScannedFile(path string, size int64) ScannedFile {
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	row := ScannedFile{Path: path, SizeBytes: size}

	// Ordered most-specific first. S01E07 states both unambiguously; the
	// dash and ep-only forms state only the episode, and the season is then
	// looked for separately before falling back to 1.
	if m := reSeasonEp.FindStringSubmatch(stem); m != nil {
		row.Season, _ = strconv.Atoi(m[1])
		row.Episode, _ = strconv.Atoi(m[2])
	} else if m := reEpOnly.FindStringSubmatch(stem); m != nil {
		row.Episode, _ = strconv.Atoi(m[1])
		row.Season = seasonFrom(stem)
	} else if m := reDashEp.FindStringSubmatch(stem); m != nil {
		row.Episode, _ = strconv.Atoi(m[1])
		row.Season = seasonFrom(stem)
	}
	if m := reResolution.FindStringSubmatch(stem); m != nil {
		row.Resolution = strings.ToLower(m[1])
		if row.Resolution == "uhd" {
			row.Resolution = "4k"
		}
	}
	if m := reSourceTag.FindStringSubmatch(stem); m != nil {
		row.SourceTag = strings.ToLower(strings.ReplaceAll(m[1], "-", ""))
		// Normalise common variants.
		switch row.SourceTag {
		case "bdremux":
			row.SourceTag = "bd-remux"
		case "webdl":
			row.SourceTag = "web-dl"
		}
	}
	// Title cleanup: drop bracketed groups, collapse spaces.
	clean := reBracketGroup.ReplaceAllString(stem, " ")
	clean = strings.Join(strings.Fields(clean), " ")
	row.RawTitle = clean
	return row
}

// seasonFrom reads a standalone season marker ("S3", "Season 2") out of a
// name whose episode was matched separately.
//
// Falls back to 1 rather than 0. A show with no season stated is season one
// far more often than it is seasonless, and 0 is the value that collapses
// distinct episodes into one offer bucket — so the safer default is the one
// that keeps them apart.
func seasonFrom(stem string) int {
	if m := reSeasonOnly.FindStringSubmatch(stem); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

// SizeBucket maps a byte count to the four allowed buckets the site
// declares in migration 238. Kept in this package (not the client
// one) so the scan-side code path is self-contained.
func SizeBucket(sizeBytes int64) string {
	const (
		mb500 = 500 * 1024 * 1024
		gb1   = 1024 * 1024 * 1024
	)
	var gb2_5 int64 = int64(2.5 * 1024 * 1024 * 1024)
	switch {
	case sizeBytes < mb500:
		return "<500MB"
	case sizeBytes < gb1:
		return "<1GB"
	case sizeBytes < gb2_5:
		return "<2.5GB"
	default:
		return ">=2.5GB"
	}
}
