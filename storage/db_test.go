package storage

import (
	"path/filepath"
	"testing"
)

// TestOpenDB_AppliesAllMigrations opens a fresh SQLite file and makes
// sure every embedded migration applies without error — the whole
// point of the migration runner is to catch SQL syntax mistakes at
// build/test time rather than at first-boot time in production.
//
// After open we look at schema_migrations to confirm all five
// embedded files were recorded. If someone adds a migration with bad
// syntax this test fails at the exact file that breaks.
func TestOpenDB_AppliesAllMigrations(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "agent.db")
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name FROM schema_migrations ORDER BY name`)
	if err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	defer rows.Close()
	var applied []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		applied = append(applied, n)
	}
	// Expect at least the five migrations we've shipped so far. Future
	// migrations will just grow this floor; an explicit list would be
	// annoying to maintain each time.
	if len(applied) < 5 {
		t.Fatalf("want >= 5 migrations applied, got %d: %v", len(applied), applied)
	}
	// Sanity-check that the groups schema evolution landed end-to-end:
	// the columns added across 004 + 005 should all exist, INSERT-able
	// with NULLs for overrides and proper defaults for NOT NULL cols.
	if _, err := db.Exec(`
		INSERT INTO groups (name, newsgroups_json, watermark_text, source)
		VALUES ('test', '["alt.bin.test"]', '', 'local')`); err != nil {
		t.Fatalf("insert into groups: %v", err)
	}
	var gotType string
	var gotVersion int
	if err := db.QueryRow(`SELECT type, version FROM groups WHERE name='test'`).Scan(&gotType, &gotVersion); err != nil {
		t.Fatalf("select groups: %v", err)
	}
	if gotType != "video" {
		t.Errorf("type default = %q, want video", gotType)
	}
	if gotVersion != 1 {
		t.Errorf("version default = %d, want 1", gotVersion)
	}
	// The auto-bump trigger: any UPDATE that doesn't set version should
	// push it from 1 → 2.
	if _, err := db.Exec(`UPDATE groups SET watermark_text='x' WHERE name='test'`); err != nil {
		t.Fatalf("update groups: %v", err)
	}
	if err := db.QueryRow(`SELECT version FROM groups WHERE name='test'`).Scan(&gotVersion); err != nil {
		t.Fatalf("re-select: %v", err)
	}
	if gotVersion != 2 {
		t.Errorf("version after update = %d, want 2 (auto-bump trigger)", gotVersion)
	}
}
