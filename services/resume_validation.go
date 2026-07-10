package services

import (
	"log"
	"os"
	"path/filepath"
)

// ValidatePartialDownload sanity-checks a previously-staged download
// directory before the agent resumes from it. Returns true if the
// directory looks structurally usable, false if the caller should
// abandon the resume and start a fresh download.
//
// Checks, in order:
//
//  1. dir must exist and be a directory (caller already did this
//     but cheap to re-verify and lets ValidatePartialDownload be
//     called without a redundant os.Stat at the callsite).
//  2. No symlinks anywhere in the tree. A symlink at this stage
//     would either be left over from a partial extract or an
//     attempt to escape the temp dir; either way it's a sign the
//     prior run did not exit cleanly and the resume is unsafe.
//  3. At least one regular file with non-zero bytes. An empty
//     directory means the previous client never wrote anything
//     before dying — nothing to resume.
//  4. If expectedBytes > 0, the total payload byte count must be
//     within 50%..150% of expected. Lower bound catches a stub
//     directory with only a metadata file; upper bound catches a
//     bizarre case where stale files from an unrelated job remain.
//     expectedBytes == 0 disables the size check (the resume path
//     doesn't always have the original task's torrent size on hand,
//     and torrent size isn't part of AgentTask today).
//
// The heuristic is intentionally lenient — a torrent that's 99%
// downloaded should still resume — but it MUST refuse a directory
// that's obviously broken. The cost of a false negative is one
// re-download; the cost of a false positive is shipping a corrupt
// NZB to the site.
func ValidatePartialDownload(dir string, expectedBytes int64) bool {
	if dir == "" {
		return false
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		log.Printf("[resume-validate] %s: not a directory (%v)", dir, err)
		return false
	}

	var fileCount int
	var totalBytes int64
	walkErr := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// Lstat to detect symlinks — Walk follows directories but
		// reports symlinks via Mode()&os.ModeSymlink so checking
		// here is correct.
		if fi.Mode()&os.ModeSymlink != 0 {
			log.Printf("[resume-validate] %s: symlink at %s — refusing resume", dir, path)
			return errSymlinkFound
		}
		if fi.Mode().IsRegular() && fi.Size() > 0 {
			fileCount++
			totalBytes += fi.Size()
		}
		return nil
	})
	if walkErr == errSymlinkFound {
		return false
	}
	if walkErr != nil {
		log.Printf("[resume-validate] %s: walk error: %v", dir, walkErr)
		return false
	}
	if fileCount == 0 {
		log.Printf("[resume-validate] %s: no non-empty files found — refusing resume", dir)
		return false
	}
	if expectedBytes > 0 {
		minBytes := expectedBytes / 2
		maxBytes := expectedBytes + expectedBytes/2
		if totalBytes < minBytes || totalBytes > maxBytes {
			log.Printf("[resume-validate] %s: size %d outside [%d..%d] (expected %d) — refusing resume",
				dir, totalBytes, minBytes, maxBytes, expectedBytes)
			return false
		}
	}
	log.Printf("[resume-validate] %s: ok (%d files, %d bytes)", dir, fileCount, totalBytes)
	return true
}

// errSymlinkFound is a sentinel used by ValidatePartialDownload to
// short-circuit filepath.Walk on the first symlink. Not exported —
// callers only see the bool return.
var errSymlinkFound = &walkSentinel{"symlink found"}

type walkSentinel struct{ msg string }

func (w *walkSentinel) Error() string { return w.msg }
