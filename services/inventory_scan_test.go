package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildTree materialises a library on disk. Sizes are in bytes.
func buildTree(t *testing.T, files map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for rel, size := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", p, err)
		}
		if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	return root
}

func paths(res InventoryResult) []string {
	out := make([]string, 0, len(res.Files))
	for _, f := range res.Files {
		out = append(out, f.RelPath)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// THE POINT OF THE WHOLE WALKER. ScanFolder drops Extras/, Specials/ and
// anything not on a media allowlist, because it decides what is worth
// offering. This one decides nothing — if it filtered the same way, the
// operator would be choosing from a tree that had already been censored and
// would have no way to tell.
func TestScanInventoryReportsWhatScanFolderWouldDrop(t *testing.T) {
	root := buildTree(t, map[string]int{
		"Show/S01/ep01.mkv":         4 << 20,
		"Show/Extras/making-of.mkv": 4 << 20,
		"Show/Specials/ova1.mkv":    4 << 20,
		"Show/Sample/sample.mkv":    4 << 20,
		"Show/S01/cover.jpg":        4 << 20,
		"Music/track.flac":          4 << 20,
		"Archive/release.rar":       4 << 20,
	})
	res, err := ScanInventory(root, InventoryOptions{ExcludeExts: DefaultInventoryExcludes})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := paths(res)
	for _, want := range []string{
		"Show/S01/ep01.mkv",
		"Show/Extras/making-of.mkv",
		"Show/Specials/ova1.mkv",
		"Show/Sample/sample.mkv",
		"Show/S01/cover.jpg",
		"Music/track.flac",
		"Archive/release.rar",
	} {
		if !has(got, want) {
			t.Errorf("%q missing — the walker is filtering, which defeats the staging flow", want)
		}
	}
	if len(got) != 7 {
		t.Errorf("reported %d files, want 7: %v", len(got), got)
	}
}

// Paths are the site's unique key and must not vary by host OS, or the same
// library re-reports as an entirely new set of rows when the agent moves.
func TestScanInventoryUsesForwardSlashRelativePaths(t *testing.T) {
	root := buildTree(t, map[string]int{"Anime/Show/S01/ep01.mkv": 2 << 20})
	res, err := ScanInventory(root, InventoryOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(res.Files))
	}
	got := res.Files[0].RelPath
	if got != "Anime/Show/S01/ep01.mkv" {
		t.Errorf("RelPath = %q, want a forward-slashed relative path", got)
	}
	if filepath.IsAbs(got) || strings.Contains(got, `\`) {
		t.Errorf("RelPath %q is absolute or backslashed — the site refuses both", got)
	}
}

func TestScanInventoryNoiseFloorAndExcludes(t *testing.T) {
	root := buildTree(t, map[string]int{
		"Show/ep01.mkv": 4 << 20,
		"Show/ep01.srt": 2 << 10, // tiny, but .srt is NOT in the exclude list
		"Show/ep01.nfo": 4 << 20, // big, but .nfo always goes
		"Show/tiny.mkv": 1 << 10, // under the floor
	})
	res, err := ScanInventory(root, InventoryOptions{
		MinSizeBytes: 1 << 20,
		ExcludeExts:  DefaultInventoryExcludes,
	})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	got := paths(res)
	if !has(got, "Show/ep01.mkv") {
		t.Error("the episode was dropped")
	}
	if has(got, "Show/ep01.nfo") {
		t.Error(".nfo survived the exclude list")
	}
	if has(got, "Show/ep01.srt") {
		t.Error("a sub-floor file survived")
	}
	if has(got, "Show/tiny.mkv") {
		t.Error("a sub-floor .mkv survived")
	}
	// And with no options at all, everything comes back.
	all, _ := ScanInventory(root, InventoryOptions{})
	if len(all.Files) != 4 {
		t.Errorf("unfiltered scan returned %d files, want 4", len(all.Files))
	}
}

func TestScanInventorySkipsHiddenAndToolingDirs(t *testing.T) {
	root := buildTree(t, map[string]int{
		"Show/ep01.mkv":         2 << 20,
		".hidden/secret.mkv":    2 << 20,
		"@eaDir/Show/thumb.mkv": 2 << 20,
		"lost+found/orphan.mkv": 2 << 20,
	})
	res, err := ScanInventory(root, InventoryOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Files) != 1 || res.Files[0].RelPath != "Show/ep01.mkv" {
		t.Fatalf("got %v, want only Show/ep01.mkv — @eaDir alone would double a NAS library", paths(res))
	}
}

// THE DANGEROUS ONE. A truncated walk must be reported as truncated, because
// the service uses that flag to decide whether to close the generation. If it
// were lost, a mis-pointed root would prune the operator's real inventory and
// flag live offers as missing on the basis of a walk that never reached them.
func TestScanInventoryReportsTruncation(t *testing.T) {
	files := map[string]int{}
	for i := 0; i < 20; i++ {
		files[filepath.ToSlash(filepath.Join("Show", string(rune('a'+i))+".mkv"))] = 2 << 20
	}
	root := buildTree(t, files)

	res, err := ScanInventory(root, InventoryOptions{MaxFiles: 5})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if !res.Truncated {
		t.Fatal("MaxFiles was hit and Truncated is false — the service would prune on a partial walk")
	}
	if len(res.Files) > 5 {
		t.Errorf("returned %d files past a cap of 5", len(res.Files))
	}

	// And an un-truncated walk must not claim it was.
	full, _ := ScanInventory(root, InventoryOptions{MaxFiles: 1000})
	if full.Truncated {
		t.Error("a complete walk reported itself truncated — the generation would never close")
	}
	if len(full.Files) != 20 {
		t.Errorf("complete walk returned %d files, want 20", len(full.Files))
	}
}

// Successive scans of an unchanged library must produce identical batches, or
// every re-scan rewrites every row for no reason.
func TestScanInventoryIsStablyOrdered(t *testing.T) {
	root := buildTree(t, map[string]int{
		"b/2.mkv": 1 << 20, "a/1.mkv": 1 << 20, "c/3.mkv": 1 << 20, "a/0.mkv": 1 << 20,
	})
	first, _ := ScanInventory(root, InventoryOptions{})
	second, _ := ScanInventory(root, InventoryOptions{})
	p1, p2 := paths(first), paths(second)
	if strings.Join(p1, "|") != strings.Join(p2, "|") {
		t.Fatalf("two scans disagreed:\n  %v\n  %v", p1, p2)
	}
	want := []string{"a/0.mkv", "a/1.mkv", "b/2.mkv", "c/3.mkv"}
	if strings.Join(p1, "|") != strings.Join(want, "|") {
		t.Errorf("order = %v, want %v", p1, want)
	}
}

func TestScanInventoryParsesHints(t *testing.T) {
	root := buildTree(t, map[string]int{
		"Show/[SubsPlease] Show Name - S01E03 (1080p) [ABCD].mkv": 4 << 20,
	})
	res, err := ScanInventory(root, InventoryOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	f := res.Files[0]
	if f.Season != 1 || f.Episode != 3 {
		t.Errorf("season/episode = %d/%d, want 1/3", f.Season, f.Episode)
	}
	if f.Resolution != "1080p" {
		t.Errorf("resolution = %q, want 1080p", f.Resolution)
	}
	if strings.Contains(f.RawTitle, "[") {
		t.Errorf("RawTitle %q still carries bracket groups", f.RawTitle)
	}
}

func TestScanInventoryRejectsBadRoots(t *testing.T) {
	if _, err := ScanInventory("", InventoryOptions{}); err == nil {
		t.Error("an empty root was accepted")
	}
	if _, err := ScanInventory(filepath.Join(t.TempDir(), "nope"), InventoryOptions{}); err == nil {
		t.Error("a missing root was accepted")
	}
	// A file rather than a directory: silently walking it would report one
	// row with an empty relative path.
	f := filepath.Join(t.TempDir(), "a.mkv")
	os.WriteFile(f, []byte("x"), 0o644)
	if _, err := ScanInventory(f, InventoryOptions{}); err == nil {
		t.Error("a plain file was accepted as a root")
	}
}

// ─── scan ids ────────────────────────────────────────────────────────────────

// Two roots walked in the same second must not share a generation: each root's
// prune would then delete the other's rows.
func TestNewScanIDDistinguishesRoots(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	a := NewScanID("/media/anime", now)
	b := NewScanID("/media/movies", now)
	if a == b {
		t.Fatalf("both roots produced %q — one prune would delete the other's inventory", a)
	}
	if !strings.HasPrefix(a, "20260815T100000Z") {
		t.Errorf("scan id %q does not carry the timestamp", a)
	}
	// Successive walks of the SAME root must differ, or the prune never fires.
	later := NewScanID("/media/anime", now.Add(time.Second))
	if later == a {
		t.Error("two walks a second apart share a generation")
	}
}

func TestNewScanIDIsSafeForOddRoots(t *testing.T) {
	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	for _, root := range []string{"/", ".", "", `C:\media\ア ニメ!`, "/mnt/very-long-directory-name-that-keeps-going-and-going"} {
		id := NewScanID(root, now)
		if id == "" {
			t.Errorf("root %q produced an empty scan id — the site refuses those", root)
		}
		if strings.ContainsAny(id, ` \/"'`) {
			t.Errorf("scan id %q for root %q contains characters that need escaping", id, root)
		}
	}
}

// ─── batching ────────────────────────────────────────────────────────────────

func TestBatchInventory(t *testing.T) {
	mk := func(n int) []InventoryFile {
		out := make([]InventoryFile, n)
		for i := range out {
			out[i] = InventoryFile{RelPath: string(rune('a' + i%26))}
		}
		return out
	}
	cases := []struct{ n, size, want int }{
		{0, 100, 0},
		{1, 100, 1},
		{100, 100, 1}, // exactly one full batch, not two
		{101, 100, 2},
		{250, 100, 3},
	}
	for _, tc := range cases {
		got := BatchInventory(mk(tc.n), tc.size)
		if len(got) != tc.want {
			t.Errorf("BatchInventory(%d files, size %d) = %d batches, want %d",
				tc.n, tc.size, len(got), tc.want)
		}
		total := 0
		for _, b := range got {
			if len(b) > tc.size {
				t.Errorf("a batch held %d > %d", len(b), tc.size)
			}
			total += len(b)
		}
		if total != tc.n {
			t.Errorf("batching lost files: %d in, %d out", tc.n, total)
		}
	}
}
