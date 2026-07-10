package storage

// Offer-path cache (migration 006) — stores the agent-local hash →
// path mapping the offer-sync service writes during scan and the
// offer-fulfill service reads when a request comes in.
//
// All queries scoped to the single-row PK so concurrency is fine.

import (
	"database/sql"
	"time"
)

// OfferPathRow is one row of the local cache.
type OfferPathRow struct {
	OfferHash  string
	LocalPath  string
	SizeBytes  int64
	LastSeenAt time.Time
}

// UpsertOfferPath records (or refreshes) the hash → path mapping.
// Called per file during the sync scan. Re-running with the same
// hash + a different path moves the cache entry; same hash + same
// path just bumps last_seen_at.
func (db *DB) UpsertOfferPath(hash, path string, size int64) error {
	if hash == "" || path == "" {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO offer_paths (offer_hash, local_path, size_bytes, last_seen_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(offer_hash) DO UPDATE SET
		    local_path   = excluded.local_path,
		    size_bytes   = excluded.size_bytes,
		    last_seen_at = excluded.last_seen_at`,
		hash, path, size, time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetOfferPath returns the local path the offer-fulfill service
// should look at for a given hash. (empty, nil) when not found —
// caller treats absence as "I no longer have this file, fail the
// request so it reopens for another offerer."
func (db *DB) GetOfferPath(hash string) (string, error) {
	if hash == "" {
		return "", nil
	}
	var path string
	err := db.QueryRow(
		`SELECT local_path FROM offer_paths WHERE offer_hash = ?`, hash,
	).Scan(&path)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return path, err
}

// PruneStaleOfferPaths drops rows the sync hasn't touched in a while.
// Default cutoff is 30 days — same library re-walked weekly is fine,
// a library that hasn't been seen in a month is probably gone.
func (db *DB) PruneStaleOfferPaths(olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = 30 * 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-olderThan).Format(time.RFC3339)
	res, err := db.Exec(`DELETE FROM offer_paths WHERE last_seen_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
