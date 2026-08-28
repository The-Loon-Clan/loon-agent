package storage

import (
	"path/filepath"
	"testing"
)

// The cursor's whole job is surviving the process: what one tick writes,
// the next tick's fresh read must return — including the wrap to zero
// that means "walk finished, start over".
func TestScrapeCursorRoundTrip(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	// A source never walked starts at the newest.
	if off, err := db.GetScrapeCursor("animez"); err != nil || off != 0 {
		t.Fatalf("fresh cursor = %d, %v — want 0, nil", off, err)
	}

	if err := db.SetScrapeCursor("animez", 600); err != nil {
		t.Fatalf("set: %v", err)
	}
	if off, _ := db.GetScrapeCursor("animez"); off != 600 {
		t.Errorf("cursor = %d, want 600", off)
	}

	// Upsert, not insert-once: the walk advances every tick.
	if err := db.SetScrapeCursor("animez", 1200); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if off, _ := db.GetScrapeCursor("animez"); off != 1200 {
		t.Errorf("cursor = %d, want 1200", off)
	}

	// Sources do not share a position.
	if off, _ := db.GetScrapeCursor("nyaa"); off != 0 {
		t.Errorf("other source's cursor = %d, want 0", off)
	}

	// A negative offset is a caller bug; store the safe wrap instead.
	if err := db.SetScrapeCursor("animez", -5); err != nil {
		t.Fatalf("negative set: %v", err)
	}
	if off, _ := db.GetScrapeCursor("animez"); off != 0 {
		t.Errorf("cursor after negative = %d, want the 0 wrap", off)
	}
}
