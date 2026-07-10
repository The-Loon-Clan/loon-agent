package services

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// describeMissingPath returns a verbose error explaining what's actually on
// disk where the caller expected `src`. Used when Prepare phase stat fails so
// the agent log shows the real filename(s) under the parent dir instead of
// the bare "no such file or directory" — which is useless when the torrent
// client wrote the file under a sanitized / different-encoded name. The
// originalErr is preserved as the wrapped error so callers checking
// errors.Is can still match os.IsNotExist.
func describeMissingPath(src string, originalErr error) error {
	parent := filepath.Dir(src)
	want := filepath.Base(src)
	entries, dirErr := os.ReadDir(parent)
	if dirErr != nil {
		return fmt.Errorf("%w (parent dir %q unreadable: %v)", originalErr, parent, dirErr)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) == 0 {
		return fmt.Errorf("%w (expected %q under %q; directory is empty — download may not have flushed yet)",
			originalErr, want, parent)
	}
	return fmt.Errorf("%w (expected %q under %q; found: %v)",
		originalErr, want, parent, names)
}

// agentManagedDirs are subdirectories the agent creates INSIDE dataDir
// for its own bookkeeping (extracted subtitles + generated screenshots).
// They MUST NOT land in the upload — they're for site-side ingestion only,
// surfaced through separate /api/agent/{subtitles,screenshots} endpoints.
//
// The 1.5.22 disk-sweep fix moved these from <tempDir>/screens-XXX to
// <dataDir>/_screenshots so they inherit the dl-XXX keep-set protection.
// That solved the sweep race but introduced a SECOND bug: the stage
// walker, which copies everything under dataDir into the upload, also
// picked up _screenshots/ and _subtitles/. The .par2 set then referenced
// the resulting tree, and donors who downloaded the NZB got the
// screenshots+subtitles as on-Usenet files instead of (or in addition
// to) the actual media. Witnessed 2026-06-04 on
// "[Poopoo] Another - S01" upload (12 MB of _screenshots/, 150 KB of
// _subtitles/ shipped to Usenet alongside three of the twelve episodes).
//
// Skipping these directories at Walk time keeps them on disk (for the
// post-stage upload to the site), while excluding them from the NZB.
var agentManagedDirs = map[string]bool{
	"_screenshots": true,
	"_subtitles":   true,
}

// isAgentManagedFile returns true for files the anacrolix torrent client
// writes INSIDE dataDir as its own bookkeeping. These also leaked into
// uploads (same 2026-06-04 incident) — `.torrent.bolt.db` shipped as a
// real Usenet article alongside the .mkv set.
//
// Known artifacts as of anacrolix v1.x:
//   .torrent.bolt.db        bolt-backed peer/piece state
//   .torrent.db             alternate storage backend
//   .torrent.bolt.db.lock   bolt lockfile
//
// Matching the `.torrent.` prefix catches future variants too — anacrolix
// has stuck to this convention across versions. Doesn't match a real
// `.torrent` file (no dot after) so user-supplied .torrent files in the
// data dir would still upload normally.
func isAgentManagedFile(name string) bool {
	return strings.HasPrefix(name, ".torrent.")
}

// stageCounts is the result of walking a tree and tallying non-zero-byte
// regular files. Used by CopyFiles + ObfuscateFiles to compare what was on
// the source side against what landed in the staging directory.
type stageCounts struct {
	files int
	bytes int64
}

// CountUploadableBytes walks `root` and returns the total bytes that
// would end up in the NZB — i.e. non-zero-byte regular files outside
// the agent-managed sub-trees and excluding anacrolix-internal files.
// Used by cmd/agent/main.go right before staging to compare against
// the torrent's expected length; if actual bytes are wildly below
// expected, we abort instead of shipping a half-empty upload.
//
// Exported wrapper around the package-private countStagedFiles so the
// audit logic stays in one place (any future skip rule lands here and
// applies to both the audit and the pre-stage check).
func CountUploadableBytes(root string) (int64, int, error) {
	c, err := countStagedFiles(root)
	return c.bytes, c.files, err
}

// countStagedFiles walks `root` and tallies non-zero-byte regular files. It
// mirrors the filter used inside the Walk callbacks of CopyFiles /
// ObfuscateFiles (skip dirs, skip zero-byte, skip agent-managed subdirs)
// so the post-stage audit compares apples to apples.
//
// Skipping agentManagedDirs here matters for two reasons:
//   1. Apples-to-apples with the source walk in CopyFiles / ObfuscateFiles
//      which also skips them — without symmetry the audit would falsely
//      detect a mismatch.
//   2. If a future code path leaks _screenshots / _subtitles into the
//      stage dir, this counter would silently absorb them into "bytes
//      staged". Skipping at audit time keeps the metric honest.
func countStagedFiles(root string) (stageCounts, error) {
	var c stageCounts
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			if agentManagedDirs[filepath.Base(path)] {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Size() == 0 || isAgentManagedFile(fi.Name()) {
			return nil
		}
		c.files++
		c.bytes += fi.Size()
		return nil
	})
	return c, err
}

// auditStaged compares the destination tree against the (files, bytes) totals
// captured while walking the source. A mismatch means a file was silently
// dropped (or grew/shrank) between Walk and copy — this is the regression we
// want to fail loudly on, not paper over.
func auditStaged(src, dst string, want stageCounts) error {
	got, err := countStagedFiles(dst)
	if err != nil {
		return fmt.Errorf("stage: audit walk of %q failed: %w", dst, err)
	}
	if got.files != want.files || got.bytes != want.bytes {
		return fmt.Errorf(
			"stage: parity mismatch between %q and %q: src=%d file(s)/%d byte(s), dst=%d file(s)/%d byte(s) (delta: %d file(s)/%d byte(s))",
			src, dst,
			want.files, want.bytes,
			got.files, got.bytes,
			got.files-want.files, got.bytes-want.bytes,
		)
	}
	return nil
}

// ObfuscateFiles copies all files from src into dstDir with randomized
// filenames, preserving the original extension. src can be a file or directory.
//
// The post-walk parity audit exists because of the "Extras-only upload"
// regression: a multi-file torrent staged only its Extras/ subdir into the
// NZB and the main feature silently disappeared, producing a partial upload
// that nobody noticed until users complained. Counting files + bytes on both
// sides of the copy turns that class of silent drop into a hard error with
// the offending paths named in the log.
func ObfuscateFiles(ctx context.Context, src, dstDir string) error {
	info, err := os.Stat(src)
	if err != nil {
		return describeMissingPath(src, err)
	}
	if !info.IsDir() {
		ext := filepath.Ext(info.Name())
		if info.Size() == 0 {
			log.Printf("stage: skipping zero-byte file %s", src)
			return nil
		}
		dst := filepath.Join(dstDir, GenerateRandomPassword(12)+ext)
		if err := copyFile(src, dst); err != nil {
			return fmt.Errorf("stage: copy %q failed: %w", info.Name(), err)
		}
		log.Printf("stage: copied 1 file(s), %.2f MiB from %s to %s",
			float64(info.Size())/(1024*1024), src, dstDir)
		return nil
	}

	var want stageCounts
	walkErr := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			log.Printf("stage: walk error at %s: %v", path, err)
			return err
		}
		if fi.IsDir() {
			// Skip agent-managed sub-trees (_screenshots, _subtitles)
			// — these go to the site via separate APIs, never to
			// Usenet. See the comment on agentManagedDirs.
			if agentManagedDirs[filepath.Base(path)] {
				log.Printf("stage: skipping agent-managed dir %s (site-only, not uploaded)", path)
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Size() == 0 {
			log.Printf("stage: skipping zero-byte file %s", path)
			return nil
		}
		if isAgentManagedFile(fi.Name()) {
			log.Printf("stage: skipping anacrolix-internal file %s (site-only, not uploaded)", path)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		ext := filepath.Ext(fi.Name())
		obfName := GenerateRandomPassword(12) + ext
		dst := filepath.Join(dstDir, obfName)

		rel, _ := filepath.Rel(src, path)
		if err := copyFile(path, dst); err != nil {
			return fmt.Errorf("stage: copy %q failed: %w", rel, err)
		}
		want.files++
		want.bytes += fi.Size()
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if err := auditStaged(src, dstDir, want); err != nil {
		return err
	}
	log.Printf("stage: copied %d file(s), %.2f MiB from %s to %s",
		want.files, float64(want.bytes)/(1024*1024), src, dstDir)
	return nil
}

// CopyFiles stages files from src into dstDir preserving original filenames
// and directory structure. Prefers hardlinks (zero I/O) and falls back to a
// full copy when hardlinking fails (e.g. cross-device mounts in Docker).
//
// The post-walk parity audit exists because of the "Extras-only upload"
// regression: a multi-file torrent staged only its Extras/ subdir into the
// NZB and the main feature silently disappeared, producing a partial upload
// that nobody noticed until users complained. Counting files + bytes on both
// sides of the copy turns that class of silent drop into a hard error with
// the offending paths named in the log.
func CopyFiles(ctx context.Context, src, dstDir string) error {
	info, err := os.Stat(src)
	if err != nil {
		return describeMissingPath(src, err)
	}
	if !info.IsDir() {
		if info.Size() == 0 {
			log.Printf("stage: skipping zero-byte file %s", src)
			return nil
		}
		dst := filepath.Join(dstDir, info.Name())
		if err := linkOrCopy(src, dst); err != nil {
			return fmt.Errorf("stage: link/copy %q failed: %w", info.Name(), err)
		}
		log.Printf("stage: copied 1 file(s), %.2f MiB from %s to %s",
			float64(info.Size())/(1024*1024), src, dstDir)
		return nil
	}

	var want stageCounts
	walkErr := filepath.Walk(src, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			log.Printf("stage: walk error at %s: %v", path, err)
			return err
		}
		if fi.IsDir() {
			// Skip agent-managed sub-trees (_screenshots, _subtitles)
			// — these go to the site via separate APIs, never to
			// Usenet. See the comment on agentManagedDirs.
			if agentManagedDirs[filepath.Base(path)] {
				log.Printf("stage: skipping agent-managed dir %s (site-only, not uploaded)", path)
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Size() == 0 {
			log.Printf("stage: skipping zero-byte file %s", path)
			return nil
		}
		if isAgentManagedFile(fi.Name()) {
			log.Printf("stage: skipping anacrolix-internal file %s (site-only, not uploaded)", path)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		rel, _ := filepath.Rel(src, path)
		dst := filepath.Join(dstDir, rel)
		os.MkdirAll(filepath.Dir(dst), 0755)
		if err := linkOrCopy(path, dst); err != nil {
			return fmt.Errorf("stage: link/copy %q failed: %w", rel, err)
		}
		want.files++
		want.bytes += fi.Size()
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if err := auditStaged(src, dstDir, want); err != nil {
		return err
	}
	log.Printf("stage: copied %d file(s), %.2f MiB from %s to %s",
		want.files, float64(want.bytes)/(1024*1024), src, dstDir)
	return nil
}

// linkOrCopy tries os.Link first (instant, zero I/O); falls back to copyFile
// when src and dst are on different devices or the filesystem doesn't support
// hardlinks.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst)
}

// SanitizeBaseName turns a title string into a safe filename base for PAR2
// and other outputs. Removes characters that are illegal on common filesystems.
//
// Replaces both ASCII filesystem-reserved chars AND their fullwidth Unicode
// equivalents commonly seen in CJK release titles (U+FF1A FULLWIDTH COLON,
// U+FF1F FULLWIDTH QUESTION MARK, etc.) — those are visually-distinct from
// the ASCII forms but most filesystems still treat them fine; we map them to
// underscore for consistency with the ASCII set.
//
// Truncates by rune count, not byte count: a 200-byte slice in the middle of
// a 3-byte CJK codepoint produces invalid UTF-8, which json.Marshal then
// replaces with U+FFFD on the round-trip — silent name corruption. Counting
// runes keeps the cut on a codepoint boundary.
func SanitizeBaseName(title string) string {
	replacer := strings.NewReplacer(
		// ASCII reserved on Windows / common filesystems
		"/", "_", "\\", "_", ":", "_", "*", "_",
		"?", "_", "\"", "_", "<", "_", ">", "_", "|", "_",
		// Fullwidth Unicode equivalents (common in CJK release titles)
		"：", "_", // FULLWIDTH COLON ：
		"？", "_", // FULLWIDTH QUESTION MARK ?
		"｜", "_", // FULLWIDTH VERTICAL LINE |
		"＂", "_", // FULLWIDTH QUOTATION MARK ”
		"＜", "_", // FULLWIDTH LESS-THAN SIGN <
		"＞", "_", // FULLWIDTH GREATER-THAN SIGN >
		"＊", "_", // FULLWIDTH ASTERISK *
		"＼", "_", // FULLWIDTH REVERSE SOLIDUS \
		"／", "_", // FULLWIDTH SOLIDUS /
	)
	name := replacer.Replace(strings.TrimSpace(title))
	if name == "" {
		return GenerateRandomPassword(12)
	}
	if r := []rune(name); len(r) > 200 {
		name = string(r[:200])
	}
	return name
}

// copyBufSize is used for file copies. 256 KB cuts syscall overhead ~8x
// vs. the default 32 KB io.Copy buffer for large media files.
const copyBufSize = 256 * 1024

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	// Pre-allocate so the filesystem can lay out contiguous blocks.
	if info, err := in.Stat(); err == nil && info.Size() > 0 {
		if err := out.Truncate(info.Size()); err != nil {
			return fmt.Errorf("preallocate %s: %w", dst, err)
		}
	}

	buf := make([]byte, copyBufSize)
	_, err = io.CopyBuffer(out, in, buf)
	return err
}
