package storage

// Scrape cursors (migration 008) — where each paging tracker scrape
// resumes. Written by the offer-sync orchestrator after every scrape
// tick, read before the next; one row per source short_name.

import "database/sql"

// GetScrapeCursor returns the persisted resume offset for a source, or 0
// when the source has never been walked (a fresh walk starts at the
// newest, which is offset 0).
func (db *DB) GetScrapeCursor(sourceShort string) (int, error) {
	var off int
	err := db.QueryRow(
		`SELECT next_offset FROM scrape_cursors WHERE source_short = ?`,
		sourceShort).Scan(&off)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return off, err
}

// SetScrapeCursor records where the next tick resumes. 0 means the walk
// completed and starts over from the newest.
func (db *DB) SetScrapeCursor(sourceShort string, nextOffset int) error {
	if nextOffset < 0 {
		nextOffset = 0
	}
	_, err := db.Exec(`
		INSERT INTO scrape_cursors (source_short, next_offset, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT (source_short) DO UPDATE SET
		    next_offset = excluded.next_offset,
		    updated_at  = CURRENT_TIMESTAMP`,
		sourceShort, nextOffset)
	return err
}
