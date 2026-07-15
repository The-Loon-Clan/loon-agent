package services

import "testing"

// The regression: the online path sweeps the blocklist at Step 3, then the
// Step 4 pre-stage check demanded every file the torrent declared — including
// the ones it had just deleted. Any release shipping a .bat/.exe/.iso failed
// with "torrent declared 24 file(s), 2 missing", naming the agent's own work,
// and blamed a disk_reserve_sweep race and a partial download.
func TestExpectedAfterBlocklist_ExcludesWhatTheSweepDeleted(t *testing.T) {
	// The real ACCA release that failed: 24 declared files, two of them .bat.
	declared := []ExpectedFile{
		{Path: "ACCA/E01.mkv", Size: 100},
		{Path: "ACCA/E02.mkv", Size: 100},
		{Path: "ACCA/Remove-Dub.bat", Size: 1},
		{Path: "ACCA/Switch-to-Sub.bat", Size: 1},
	}

	keep, excluded := ExpectedAfterBlocklist(declared, DefaultBlockedExtensions)
	if excluded != 2 {
		t.Errorf("excluded = %d, want 2 (both .bat files)", excluded)
	}
	if len(keep) != 2 {
		t.Fatalf("keep = %d files, want 2", len(keep))
	}
	for _, f := range keep {
		if f.Path == "ACCA/Remove-Dub.bat" || f.Path == "ACCA/Switch-to-Sub.bat" {
			t.Errorf("kept %q — the sweep deletes it, so the check must not expect it", f.Path)
		}
	}
}

// The filter must be driven by the SAME blocklist the sweep used, or an
// operator override re-opens the gap from the other side: dropping .iso from
// the blocklist keeps .iso on disk, and the check must then expect it.
func TestExpectedAfterBlocklist_HonoursOverride(t *testing.T) {
	declared := []ExpectedFile{
		{Path: "disc.iso", Size: 100},
		{Path: "run.bat", Size: 1},
	}

	// Operator override: block only .bat, allow .iso through (the Bluray remux
	// case the OnlineBlocklist comment calls out).
	keep, excluded := ExpectedAfterBlocklist(declared, EffectiveBlocklist([]string{"bat"}))
	if excluded != 1 {
		t.Errorf("excluded = %d, want 1", excluded)
	}
	if len(keep) != 1 || keep[0].Path != "disc.iso" {
		t.Errorf("keep = %v, want [disc.iso] — .iso is not blocked here, so it must still be expected on disk", keep)
	}
}

func TestExpectedAfterBlocklist_Edges(t *testing.T) {
	// Nil blocklist must fall back to the default, not "block nothing" — a
	// permissive fallback would silently restore the bug.
	keep, excluded := ExpectedAfterBlocklist([]ExpectedFile{{Path: "x.bat"}}, nil)
	if excluded != 1 || len(keep) != 0 {
		t.Errorf("nil blocklist: keep=%v excluded=%d, want the default blocklist to apply", keep, excluded)
	}

	// Case-insensitive: the sweep lowercases, so this must too or a ".BAT"
	// gets deleted and then demanded.
	keep, excluded = ExpectedAfterBlocklist([]ExpectedFile{{Path: "LOUD.BAT"}}, DefaultBlockedExtensions)
	if excluded != 1 || len(keep) != 0 {
		t.Errorf("uppercase ext: keep=%v excluded=%d, want it excluded like the sweep would delete it", keep, excluded)
	}

	// A clean torrent must pass through untouched.
	clean := []ExpectedFile{{Path: "a.mkv"}, {Path: "b.mkv"}}
	keep, excluded = ExpectedAfterBlocklist(clean, DefaultBlockedExtensions)
	if excluded != 0 || len(keep) != 2 {
		t.Errorf("clean torrent: keep=%d excluded=%d, want 2/0", len(keep), excluded)
	}

	// No declared files (single-file torrent edge, nil session) must not panic.
	keep, excluded = ExpectedAfterBlocklist(nil, DefaultBlockedExtensions)
	if len(keep) != 0 || excluded != 0 {
		t.Errorf("nil input: keep=%d excluded=%d, want 0/0", len(keep), excluded)
	}
}
