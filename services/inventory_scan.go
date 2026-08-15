package services

// Inventory walker — reports the tree, decides nothing.
//
// HOW THIS DIFFERS FROM ScanFolder, and why both exist. ScanFolder feeds the
// register path, so it filters hard: an extension allowlist, a size floor, and
// knownSubdirs (Extras/, Sample/, Specials/ …). Every one of those is a
// judgement about what is worth OFFERING, and each is invisible afterwards —
// a file it skipped simply never existed as far as the site was concerned.
//
// Under the staging flow the operator makes that judgement from a rendered
// tree, which only works if the tree is complete. So this walker reports what
// is on disk: every subdirectory including the ones ScanFolder drops, and every
// file above a small noise floor. The site holds it as staging rows, publishes
// none of it, and the human picks.
//
// The floor exists because a media library is mostly not media by file count —
// .nfo, .srt, .jpg, .sfv, Thumbs.db — and shipping fifty thousand 2 kB
// sidecars buries the twelve episodes the operator came to find. It is a
// legibility filter, not a policy one, and it is configurable.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// InventoryOptions tunes one walk.
type InventoryOptions struct {
	// MinSizeBytes drops sidecar noise. 0 means report everything.
	MinSizeBytes int64
	// ExcludeExts is a lowercase set (".nfo") always dropped regardless of
	// size. Empty means exclude nothing.
	ExcludeExts map[string]bool
	// MaxFiles bounds one walk. A runaway root (a mount that turned into "/")
	// should stop rather than stream a million rows at the site; the caller is
	// told it was truncated so it can say so out loud.
	MaxFiles int
}

// InventoryResult is one completed walk.
type InventoryResult struct {
	Files []InventoryFile
	// Truncated reports that MaxFiles stopped the walk early. The caller MUST
	// NOT close the generation when this is set: a partial walk marked final
	// would prune every file the walk never reached, which for a promoted row
	// means flagging a live offer as missing on the basis of not having looked.
	Truncated bool
	// Skipped counts entries the walk could not read (permissions, a file that
	// vanished mid-walk). Reported rather than silently absent.
	Skipped int
}

// InventoryFile is one file, relative to the scan root.
type InventoryFile struct {
	RelPath    string
	SizeBytes  int64
	RawTitle   string
	Season     int
	Episode    int
	Resolution string
	SourceTag  string
}

// DefaultInventoryExcludes are the extensions that are never worth a row.
// Deliberately short: this is a noise list, not a media allowlist. Anything not
// named here is reported and the site decides.
var DefaultInventoryExcludes = map[string]bool{
	".nfo": true, ".sfv": true, ".md5": true, ".txt": true,
	".url": true, ".lnk": true, ".db": true, ".ds_store": true,
	".part": true, ".!qb": true, ".tmp": true,
}

// ScanInventory walks root and returns one row per file.
//
// Symlinks are not followed — a library with a self-referential link would
// otherwise walk forever, and filepath.WalkDir reports the link itself so
// nothing is silently lost.
func ScanInventory(root string, opts InventoryOptions) (InventoryResult, error) {
	var res InventoryResult
	if strings.TrimSpace(root) == "" {
		return res, fmt.Errorf("inventory: empty root")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return res, fmt.Errorf("inventory: resolving %q: %w", root, err)
	}
	if fi, err := os.Stat(absRoot); err != nil {
		return res, fmt.Errorf("inventory: %q: %w", root, err)
	} else if !fi.IsDir() {
		return res, fmt.Errorf("inventory: %q is not a directory", root)
	}

	err = filepath.Walk(absRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			// A permission error on one subtree must not abandon the rest of
			// the library. Counted so the total is explainable.
			res.Skipped++
			if info != nil && info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			// Hidden and tooling directories. @eaDir is Synology's thumbnail
			// store and is present in most NAS-hosted libraries; it holds a
			// copy of every file's artwork and would double the row count.
			if path != absRoot && (strings.HasPrefix(name, ".") || name == "@eaDir" || name == "lost+found") {
				return filepath.SkipDir
			}
			return nil
		}
		// Symlinks: reported only if they resolve to a regular file we can
		// stat. Not followed as directories.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if res.MaxReached(opts) {
			res.Truncated = true
			return filepath.SkipAll
		}

		ext := strings.ToLower(filepath.Ext(path))
		if opts.ExcludeExts != nil && opts.ExcludeExts[ext] {
			return nil
		}
		if opts.MinSizeBytes > 0 && info.Size() < opts.MinSizeBytes {
			return nil
		}

		rel, relErr := filepath.Rel(absRoot, path)
		if relErr != nil {
			res.Skipped++
			return nil
		}
		// Forward slashes on the wire regardless of host OS, so a Windows agent
		// and a Linux agent describe the same library identically. The site
		// keys inventory rows on this string.
		rel = filepath.ToSlash(rel)

		parsed := parseScannedFile(path, info.Size())
		res.Files = append(res.Files, InventoryFile{
			RelPath:    rel,
			SizeBytes:  info.Size(),
			RawTitle:   parsed.RawTitle,
			Season:     parsed.Season,
			Episode:    parsed.Episode,
			Resolution: parsed.Resolution,
			SourceTag:  parsed.SourceTag,
		})
		return nil
	})
	if err != nil {
		return res, err
	}
	// Stable order so successive scans of an unchanged library produce
	// identical batches — which is what makes a re-scan cheap to eyeball and
	// keeps the site's upsert doing no work.
	sort.Slice(res.Files, func(i, j int) bool { return res.Files[i].RelPath < res.Files[j].RelPath })
	return res, nil
}

// MaxReached reports whether the walk has hit its ceiling.
func (r *InventoryResult) MaxReached(opts InventoryOptions) bool {
	return opts.MaxFiles > 0 && len(r.Files) >= opts.MaxFiles
}

// TotalBytes sums the walk. Used for the log line, so an operator can tell a
// library apart from a mount that pointed somewhere unintended.
func (r *InventoryResult) TotalBytes() int64 {
	var n int64
	for _, f := range r.Files {
		n += f.SizeBytes
	}
	return n
}

// NewScanID mints the generation marker for one walk.
//
// Time-based plus the root, so two roots scanned in the same second cannot
// collide into one generation — which would make each one's prune delete the
// other's rows.
func NewScanID(root string, now time.Time) string {
	base := filepath.Base(strings.TrimRight(filepath.ToSlash(root), "/"))
	if base == "" || base == "." || base == "/" {
		base = "root"
	}
	return fmt.Sprintf("%s-%s", now.UTC().Format("20060102T150405Z"), sanitiseScanTag(base))
}

// sanitiseScanTag keeps the id readable in a settings table and free of
// anything that would need escaping.
func sanitiseScanTag(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= 24 {
			break
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "root"
	}
	return out
}

// BatchInventory splits a walk into request-sized chunks.
//
// Returned as slices of the input rather than copies — the caller converts each
// batch to client.InventoryEntry on the way out.
func BatchInventory(files []InventoryFile, size int) [][]InventoryFile {
	if size <= 0 {
		size = 1
	}
	var out [][]InventoryFile
	for i := 0; i < len(files); i += size {
		end := i + size
		if end > len(files) {
			end = len(files)
		}
		out = append(out, files[i:end])
	}
	return out
}
