package services

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanFolderSkipsSubdirectories verifies that multi-file releases
// register only their top-level video files, preventing offer
// fragmentation when Extras/Sample subdirectories exist.
func TestScanFolderSkipsSubdirectories(t *testing.T) {
	// Create a temporary directory structure mirroring the reported bug:
	// .
	// ├── Show.S01E01.1080p.mkv (17 GiB equivalent for test)
	// └── Extras/
	//     └── Extra.1080p.mkv (1.4 GiB equivalent)
	tmpDir := t.TempDir()

	// Create main file (50 MB to meet size threshold).
	mainFile := filepath.Join(tmpDir, "Show.S01E01.1080p.mkv")
	if err := os.WriteFile(mainFile, make([]byte, 50*1024*1024), 0644); err != nil {
		t.Fatalf("create main file: %v", err)
	}

	// Create Extras subdirectory and file.
	extrasDir := filepath.Join(tmpDir, "Extras")
	if err := os.Mkdir(extrasDir, 0755); err != nil {
		t.Fatalf("create extras dir: %v", err)
	}
	extraFile := filepath.Join(extrasDir, "Extra.1080p.mkv")
	if err := os.WriteFile(extraFile, make([]byte, 50*1024*1024), 0644); err != nil {
		t.Fatalf("create extra file: %v", err)
	}

	// Scan the folder.
	rows, err := ScanFolder(tmpDir, []string{".mkv"}, 40) // 40 MB threshold
	if err != nil {
		t.Fatalf("ScanFolder: %v", err)
	}

	// Verify only the main file is returned.
	if len(rows) != 1 {
		t.Errorf("expected 1 file, got %d", len(rows))
		for i, r := range rows {
			t.Logf("  [%d] %s", i, r.Path)
		}
		return
	}

	if !filepath.HasPrefix(rows[0].Path, mainFile) && rows[0].Path != mainFile {
		t.Errorf("expected main file %s, got %s", mainFile, rows[0].Path)
	}
}

// TestScanFolderIncludesTopLevelFiles verifies that top-level video files
// are still scanned and registered (regression test for the fix).
func TestScanFolderIncludesTopLevelFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create two top-level files.
	file1 := filepath.Join(tmpDir, "Movie1.1080p.mkv")
	file2 := filepath.Join(tmpDir, "Movie2.1080p.mkv")
	for _, f := range []string{file1, file2} {
		if err := os.WriteFile(f, make([]byte, 50*1024*1024), 0644); err != nil {
			t.Fatalf("create file %s: %v", f, err)
		}
	}

	rows, err := ScanFolder(tmpDir, []string{".mkv"}, 40)
	if err != nil {
		t.Fatalf("ScanFolder: %v", err)
	}

	if len(rows) != 2 {
		t.Errorf("expected 2 files, got %d", len(rows))
	}
}

// TestIsInSubdirectory verifies subdirectory detection for various
// known supplementary directory names.
//
// Each row gets its own t.TempDir() because some rows differ only in
// case (e.g. "Extras" vs "extras") — on Windows' case-insensitive
// filesystem the second os.Mkdir would collide with the first. The
// production check is case-insensitive on every OS, so coverage of
// both spellings is still meaningful; we just need them isolated on
// disk so the test runs on any platform the agent might be built on.
func TestIsInSubdirectory(t *testing.T) {
	tests := []struct {
		subdir string
		want   bool
	}{
		{"Extras", true},
		{"extras", true},
		{"Sample", true},
		{"samples", true},
		{"Behind The Scenes", true},
		{"OVAs", true},
		{"Specials", true},
		{".", false},
		{"", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.subdir, func(t *testing.T) {
			tmpDir := t.TempDir()
			if tt.subdir != "." && tt.subdir != "" {
				if err := os.Mkdir(filepath.Join(tmpDir, tt.subdir), 0755); err != nil {
					t.Fatalf("mkdir %s: %v", tt.subdir, err)
				}
			}

			path := filepath.Join(tmpDir, tt.subdir, "file.mkv")
			got := isInSubdirectory(path, tmpDir)
			if got != tt.want {
				t.Errorf("isInSubdirectory(%s): got %v, want %v", path, got, tt.want)
			}
		})
	}
}

// TestOfferSyncMultiFileRelease verifies the full sync flow for a
// multi-file release: only the top-level file produces an offer hash.
func TestOfferSyncMultiFileRelease(t *testing.T) {
	tmpDir := t.TempDir()

	// Set up release structure.
	main := filepath.Join(tmpDir, "Movie.2024.1080p.webrip.mkv")
	if err := os.WriteFile(main, make([]byte, 50*1024*1024), 0644); err != nil {
		t.Fatalf("create main: %v", err)
	}

	extrasDir := filepath.Join(tmpDir, "Extras")
	if err := os.Mkdir(extrasDir, 0755); err != nil {
		t.Fatalf("mkdir extras: %v", err)
	}
	extra := filepath.Join(extrasDir, "Behind.The.Scenes.1080p.mkv")
	if err := os.WriteFile(extra, make([]byte, 50*1024*1024), 0644); err != nil {
		t.Fatalf("create extra: %v", err)
	}

	// Scan with realistic size threshold.
	rows, err := ScanFolder(tmpDir, []string{".mkv"}, 40)
	if err != nil {
		t.Fatalf("ScanFolder: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("expected 1 scanned file for multi-file release, got %d", len(rows))
	}

	// Verify metadata is parsed correctly from the main file.
	if rows[0].Season != 0 || rows[0].Episode != 0 {
		t.Errorf("expected no season/episode for movie, got S%dE%d", rows[0].Season, rows[0].Episode)
	}
	if rows[0].Resolution != "1080p" {
		t.Errorf("expected resolution 1080p, got %s", rows[0].Resolution)
	}
	if rows[0].SourceTag != "webrip" {
		t.Errorf("expected source webrip, got %s", rows[0].SourceTag)
	}
}
