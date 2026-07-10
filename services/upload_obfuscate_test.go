package services

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSparse creates a file of the requested size by seeking to size-1 and
// writing a single byte. Tested filesystems (NTFS, ext4, tmpfs) all honour
// this and back the hole with zero-cost extents — a 50 MiB sparse file lands
// in microseconds, which is what we want for a fast regression test.
func writeSparse(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if size > 0 {
		if _, err := f.Seek(size-1, 0); err != nil {
			t.Fatalf("seek %s: %v", path, err)
		}
		if _, err := f.Write([]byte{0}); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// TestCopyFiles_PreservesMultiFileWithSubdirs reproduces the "Extras-only
// upload" topology: a multi-file release with a feature in the root and a
// subdir of extras. Before the parity audit a regression in filepath.Walk
// ordering / filter logic could silently drop the root file and only the
// Extras subtree would make it into the staging dir. This test asserts that
// every non-zero file lands at the expected relative path with the expected
// size, and that zero-byte files in the source are skipped (not staged).
func TestCopyFiles_PreservesMultiFileWithSubdirs(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	const (
		mainSize  int64 = 50 * 1024 * 1024 // 50 MiB
		extraSize int64 = 5 * 1024 * 1024  // 5 MiB
	)

	writeSparse(t, filepath.Join(src, "main.mkv"), mainSize)
	writeSparse(t, filepath.Join(src, "Extras", "extra.mkv"), extraSize)
	// Zero-byte file at the root — the obfuscate layer skips these by design,
	// but the audit must NOT count them on either side or the parity check
	// would fail on every clean download.
	writeSparse(t, filepath.Join(src, "empty.txt"), 0)

	if err := CopyFiles(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyFiles: %v", err)
	}

	// main.mkv at the root
	if fi, err := os.Stat(filepath.Join(dst, "main.mkv")); err != nil {
		t.Fatalf("dst/main.mkv missing: %v", err)
	} else if fi.Size() != mainSize {
		t.Errorf("dst/main.mkv size = %d, want %d", fi.Size(), mainSize)
	}

	// Extras/extra.mkv preserved with its subdir
	if fi, err := os.Stat(filepath.Join(dst, "Extras", "extra.mkv")); err != nil {
		t.Fatalf("dst/Extras/extra.mkv missing: %v", err)
	} else if fi.Size() != extraSize {
		t.Errorf("dst/Extras/extra.mkv size = %d, want %d", fi.Size(), extraSize)
	}

	// Zero-byte file must NOT be present in dst
	if _, err := os.Stat(filepath.Join(dst, "empty.txt")); !os.IsNotExist(err) {
		t.Errorf("dst/empty.txt should be absent (zero-byte skip), stat err = %v", err)
	}
}

// TestCopyFiles_DetectsMissingFile drives the audit directly: stage a tree,
// then delete one file from dst before re-running auditStaged with the
// pre-deletion totals. The audit must return an error whose message names
// the deltas so the agent log captures the silent-drop diagnosis.
func TestCopyFiles_DetectsMissingFile(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	const (
		mainSize  int64 = 8 * 1024 * 1024
		extraSize int64 = 2 * 1024 * 1024
	)
	writeSparse(t, filepath.Join(src, "main.mkv"), mainSize)
	writeSparse(t, filepath.Join(src, "Extras", "extra.mkv"), extraSize)

	if err := CopyFiles(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyFiles: %v", err)
	}

	// Capture the post-stage totals (this is the "want" the real audit
	// computes during Walk).
	want, err := countStagedFiles(dst)
	if err != nil {
		t.Fatalf("countStagedFiles: %v", err)
	}
	if want.files != 2 {
		t.Fatalf("precondition: want 2 staged files, got %d", want.files)
	}

	// Simulate the silent-drop: a downstream step (or a Walk filter bug)
	// removes the feature, leaving only the Extras subtree.
	if err := os.Remove(filepath.Join(dst, "main.mkv")); err != nil {
		t.Fatalf("remove main.mkv: %v", err)
	}

	auditErr := auditStaged(src, dst, want)
	if auditErr == nil {
		t.Fatal("auditStaged returned nil, expected parity mismatch error")
	}
	msg := auditErr.Error()
	for _, fragment := range []string{"parity mismatch", "delta"} {
		if !strings.Contains(msg, fragment) {
			t.Errorf("error message missing %q: %s", fragment, msg)
		}
	}
}

// TestCopyFiles_ExcludesAgentManagedDirs locks in the 1.5.25 fix:
// _screenshots/ and _subtitles/ inside dataDir must NOT be staged
// into the upload. Witnessed 2026-06-04 on "[Poopoo] Another - S01"
// where ~12 MB of _screenshots/ + 150 KB of _subtitles/ shipped on
// Usenet alongside the media. Pre-1.5.25 the stage walker copied
// everything under src; the auditStaged parity check passed because
// src AND dst BOTH included the agent dirs.
func TestCopyFiles_ExcludesAgentManagedDirs(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeSparse(t, filepath.Join(src, "ep01.mkv"), 1_000_000)
	writeSparse(t, filepath.Join(src, "ep02.mkv"), 1_000_000)
	writeSparse(t, filepath.Join(src, "ep03.mkv"), 1_000_000)
	writeSparse(t, filepath.Join(src, "_screenshots", "s1.png"), 4_000_000)
	writeSparse(t, filepath.Join(src, "_screenshots", "s2.png"), 4_000_000)
	writeSparse(t, filepath.Join(src, "_subtitles", "track3.bin"), 32_000)
	writeSparse(t, filepath.Join(src, "_subtitles", "track4.bin"), 7_000)

	if err := CopyFiles(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyFiles: %v", err)
	}

	var landed []string
	_ = filepath.Walk(dst, func(p string, fi os.FileInfo, _ error) error {
		if fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dst, p)
		landed = append(landed, rel)
		return nil
	})
	if len(landed) != 3 {
		t.Fatalf("expected 3 files in stage, got %d: %v", len(landed), landed)
	}
	for _, name := range landed {
		if strings.HasPrefix(name, "_screenshots") || strings.HasPrefix(name, "_subtitles") {
			t.Errorf("agent-managed file leaked into stage: %s", name)
		}
	}
}

// TestCopyFiles_ExcludesAnacrolixDB locks in the second 1.5.25 fix:
// .torrent.bolt.db (anacrolix's internal piece/peer state) MUST NOT
// ship to Usenet. Same incident as the agent-managed dirs above.
func TestCopyFiles_ExcludesAnacrolixDB(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeSparse(t, filepath.Join(src, "movie.mkv"), 1_000_000)
	writeSparse(t, filepath.Join(src, ".torrent.bolt.db"), 50_000)
	writeSparse(t, filepath.Join(src, ".torrent.db"), 50_000)
	writeSparse(t, filepath.Join(src, ".torrent.bolt.db.lock"), 100)

	if err := CopyFiles(context.Background(), src, dst); err != nil {
		t.Fatalf("CopyFiles: %v", err)
	}

	var landed []string
	_ = filepath.Walk(dst, func(p string, fi os.FileInfo, _ error) error {
		if fi.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dst, p)
		landed = append(landed, rel)
		return nil
	})
	if len(landed) != 1 || landed[0] != "movie.mkv" {
		t.Fatalf("expected only movie.mkv in stage, got %v", landed)
	}
}

// TestObfuscateFiles_ExcludesAgentManagedDirs same shape as the
// CopyFiles test but for the obfuscation code path (cfg.Obfuscate=true).
func TestObfuscateFiles_ExcludesAgentManagedDirs(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	writeSparse(t, filepath.Join(src, "ep01.mkv"), 1_000_000)
	writeSparse(t, filepath.Join(src, "_screenshots", "s.png"), 4_000_000)
	writeSparse(t, filepath.Join(src, "_subtitles", "t.bin"), 32_000)
	writeSparse(t, filepath.Join(src, ".torrent.bolt.db"), 50_000)

	if err := ObfuscateFiles(context.Background(), src, dst); err != nil {
		t.Fatalf("ObfuscateFiles: %v", err)
	}

	entries, _ := os.ReadDir(dst)
	if len(entries) != 1 {
		t.Fatalf("expected 1 obfuscated file in stage, got %d", len(entries))
	}
	if filepath.Ext(entries[0].Name()) != ".mkv" {
		t.Errorf("expected .mkv extension, got %s", entries[0].Name())
	}
}

// TestCountUploadableBytes_ExcludesAgentArtifacts covers the helper
// used by cmd/agent/main.go for the pre-stage size sanity check.
// The check compares this against TorrentSession.ExpectedBytes();
// a delta > 20% aborts the pipeline.
func TestCountUploadableBytes_ExcludesAgentArtifacts(t *testing.T) {
	root := t.TempDir()

	writeSparse(t, filepath.Join(root, "ep01.mkv"), 1_000_000)
	writeSparse(t, filepath.Join(root, "ep02.mkv"), 1_000_000)
	writeSparse(t, filepath.Join(root, "_screenshots", "s.png"), 5_000_000)
	writeSparse(t, filepath.Join(root, "_subtitles", "t.bin"), 100_000)
	writeSparse(t, filepath.Join(root, ".torrent.bolt.db"), 50_000)

	bytes, files, err := CountUploadableBytes(root)
	if err != nil {
		t.Fatalf("CountUploadableBytes: %v", err)
	}
	if files != 2 {
		t.Errorf("expected 2 uploadable files, got %d", files)
	}
	if bytes != 2_000_000 {
		t.Errorf("expected 2,000,000 uploadable bytes, got %d", bytes)
	}
}

