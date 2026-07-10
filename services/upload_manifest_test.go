package services

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Helper: create a sparse file of exactly size bytes. Avoids actually
// allocating multi-GB to disk during tests — we Seek+Write to fake a
// large file's apparent length without writing the data.
func sparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if _, err := f.Seek(size-1, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// TestManifestOf_CountsVideoFiles ensures the manifest walker picks up
// the two video files in a download tree shaped like the user's
// problem torrent (main.mkv at root + Extras/extra.mkv) and ignores
// sub-MiB junk (samples, nfo, par2 sidecars).
func TestManifestOf_CountsVideoFiles(t *testing.T) {
	dir := t.TempDir()
	sparseFile(t, filepath.Join(dir, "main.mkv"), 50<<20)           // 50 MiB
	sparseFile(t, filepath.Join(dir, "Extras", "extra.mkv"), 5<<20) // 5 MiB
	sparseFile(t, filepath.Join(dir, "tiny.mkv"), 100)              // ignored: < 1 MiB
	sparseFile(t, filepath.Join(dir, "readme.nfo"), 4096)           // ignored: not video
	sparseFile(t, filepath.Join(dir, "release.par2"), 2<<20)        // ignored: not video

	m := ManifestOf(dir)
	if m.VideoCount != 2 {
		t.Errorf("VideoCount=%d, want 2", m.VideoCount)
	}
	if m.VideoBytes != (50<<20)+(5<<20) {
		t.Errorf("VideoBytes=%d, want %d", m.VideoBytes, (50<<20)+(5<<20))
	}
	if m.TotalCount != 5 {
		t.Errorf("TotalCount=%d, want 5", m.TotalCount)
	}
	if len(m.VideoFiles) != 2 {
		t.Fatalf("len(VideoFiles)=%d, want 2", len(m.VideoFiles))
	}
	// Sort guarantees Extras/extra.mkv (E) sorts BEFORE main.mkv (m, lowercase) in ASCII.
	// Verify the per-file slice is populated.
	wantBases := map[string]int64{"extra.mkv": 5 << 20, "main.mkv": 50 << 20}
	for _, f := range m.VideoFiles {
		base := filepath.Base(f.RelPath)
		if wantSize, ok := wantBases[base]; !ok {
			t.Errorf("unexpected VideoFile: %s", f.RelPath)
		} else if f.Size != wantSize {
			t.Errorf("VideoFile %s: size=%d, want %d", base, f.Size, wantSize)
		}
	}
}

// TestCompareManifest_CatchesMultiFileBug is the regression test for
// the user-reported symptom: 2-file torrent producing a 1-file NZB.
// The audit MUST refuse the publish AND return a *ManifestError that
// names the missing file.
func TestCompareManifest_CatchesMultiFileBug(t *testing.T) {
	source := UploadManifest{
		VideoCount: 2, VideoBytes: (17 << 30) + (1 << 30),
		TotalCount: 2, TotalBytes: (17 << 30) + (1 << 30),
		VideoFiles: []ManifestEntry{
			{RelPath: "Extras/extra.mkv", Size: 1 << 30},
			{RelPath: "main.mkv", Size: 17 << 30},
		},
	}
	upload := UploadManifest{
		VideoCount: 1, VideoBytes: 1 << 30,
		TotalCount: 1, TotalBytes: 1 << 30,
		VideoFiles: []ManifestEntry{
			{RelPath: "Extras/extra.mkv", Size: 1 << 30},
		},
	}

	err := CompareManifest(source, upload, false /* encrypted */)
	if err == nil {
		t.Fatal("expected manifest mismatch error, got nil")
	}

	// Concise error string lands on the request_lock — must mention the
	// first missing file by path so quick triage is possible.
	if !strings.Contains(err.Error(), "refusing to publish partial NZB") {
		t.Errorf("Error() missing partial-NZB refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "main.mkv") {
		t.Errorf("Error() should name main.mkv as the missing file: %v", err)
	}

	// Structured ManifestError must be extractable for the agent's
	// PostLog detailed-report path.
	var mfErr *ManifestError
	if !errors.As(err, &mfErr) {
		t.Fatalf("expected *ManifestError, got %T", err)
	}
	if len(mfErr.Diff.Missing) != 1 || filepath.Base(mfErr.Diff.Missing[0].RelPath) != "main.mkv" {
		t.Errorf("Diff.Missing should name main.mkv, got %+v", mfErr.Diff.Missing)
	}

	// DetailedReport must list every missing file by relpath + size so
	// the operator can see what dropped without re-running the task.
	report := mfErr.DetailedReport()
	if !strings.Contains(report, "main.mkv") {
		t.Errorf("DetailedReport missing main.mkv:\n%s", report)
	}
	if !strings.Contains(report, "MISSING from upload") {
		t.Errorf("DetailedReport missing section header:\n%s", report)
	}
}

// TestCompareManifest_AllowsEqualCount: when staging preserves every
// video, the audit passes.
func TestCompareManifest_AllowsEqualCount(t *testing.T) {
	source := UploadManifest{
		VideoCount: 2, VideoBytes: 1000, TotalCount: 2, TotalBytes: 1000,
		VideoFiles: []ManifestEntry{{RelPath: "a.mkv", Size: 500}, {RelPath: "b.mkv", Size: 500}},
	}
	upload := UploadManifest{
		VideoCount: 2, VideoBytes: 1000, TotalCount: 3, TotalBytes: 1100, // +1 par2 sidecar
		VideoFiles: []ManifestEntry{{RelPath: "a.mkv", Size: 500}, {RelPath: "b.mkv", Size: 500}},
	}
	if err := CompareManifest(source, upload, false); err != nil {
		t.Errorf("unexpected error for matched counts: %v", err)
	}
}

// TestCompareManifest_AllowsHigherCount: extract-wave run produces
// MORE video files than the raw download (a .rar containing two .mkvs,
// or a .iso unpacked to BDMV with several .m2ts). Audit must pass.
func TestCompareManifest_AllowsHigherCount(t *testing.T) {
	source := UploadManifest{
		VideoCount: 0, VideoBytes: 0, TotalCount: 1, TotalBytes: 5 << 20, // just a .rar
	}
	upload := UploadManifest{
		VideoCount: 3, VideoBytes: 50 << 20, TotalCount: 4, TotalBytes: 55 << 20,
		VideoFiles: []ManifestEntry{
			{RelPath: "01.mkv", Size: 20 << 20},
			{RelPath: "02.mkv", Size: 20 << 20},
			{RelPath: "03.mkv", Size: 10 << 20},
		},
	}
	if err := CompareManifest(source, upload, false); err != nil {
		t.Errorf("extract-wave higher count flagged as mismatch: %v", err)
	}
}

// TestCompareManifest_SkipsWhenEncrypted: encryption collapses the
// whole stage to one .7z, so per-file comparison is meaningless and
// the audit must short-circuit.
func TestCompareManifest_SkipsWhenEncrypted(t *testing.T) {
	source := UploadManifest{VideoCount: 5, VideoBytes: 5 << 30, TotalCount: 5, TotalBytes: 5 << 30}
	upload := UploadManifest{VideoCount: 0, VideoBytes: 0, TotalCount: 1, TotalBytes: 5 << 30}
	if err := CompareManifest(source, upload, true /* encrypted */); err != nil {
		t.Errorf("encryption case incorrectly flagged: %v", err)
	}
}

// TestCompareManifest_SkipsWhenNoSourceVideo: manga / audio / data
// releases legitimately have zero video files. Audit must not fire.
func TestCompareManifest_SkipsWhenNoSourceVideo(t *testing.T) {
	source := UploadManifest{VideoCount: 0, VideoBytes: 0, TotalCount: 30, TotalBytes: 100 << 20} // CBZ chapter
	upload := UploadManifest{VideoCount: 0, VideoBytes: 0, TotalCount: 30, TotalBytes: 100 << 20}
	if err := CompareManifest(source, upload, false); err != nil {
		t.Errorf("non-video release incorrectly flagged: %v", err)
	}
}

// TestDiffManifests_MatchesByBasename ensures DiffManifests considers a
// file "preserved" even if its parent directory changed between source
// and upload (e.g. staging moved "Extras/foo.mkv" up to "foo.mkv").
// Loss is keyed on basename + size matching, not full path.
func TestDiffManifests_MatchesByBasename(t *testing.T) {
	source := UploadManifest{
		VideoFiles: []ManifestEntry{
			{RelPath: "Extras/foo.mkv", Size: 100},
			{RelPath: "bar.mkv", Size: 200},
		},
	}
	upload := UploadManifest{
		VideoFiles: []ManifestEntry{
			{RelPath: "foo.mkv", Size: 100}, // moved up — same basename + size
			{RelPath: "bar.mkv", Size: 200},
		},
	}
	d := DiffManifests(source, upload)
	if len(d.Missing) != 0 {
		t.Errorf("expected zero missing on rename-only, got: %+v", d.Missing)
	}
}

// TestDiffManifests_DetectsSizeMismatch catches the "same name, wrong
// size" case — a truncated or corrupted file that staged but came out
// at a different byte count.
func TestDiffManifests_DetectsSizeMismatch(t *testing.T) {
	source := UploadManifest{
		VideoFiles: []ManifestEntry{{RelPath: "feature.mkv", Size: 17 << 30}},
	}
	upload := UploadManifest{
		VideoFiles: []ManifestEntry{{RelPath: "feature.mkv", Size: 1 << 30}}, // truncated
	}
	d := DiffManifests(source, upload)
	if len(d.Missing) != 1 || d.Missing[0].Size != 17<<30 {
		t.Errorf("size-mismatch should be flagged as missing, got: %+v", d.Missing)
	}
}
