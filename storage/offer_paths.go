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

// OfferSourceRow is everything the fulfill loop can use to get hold of a
// bucket's bytes: a local file, a remote .torrent, or both. Empty strings
// mean "no route of this kind".
type OfferSourceRow struct {
	LocalPath   string
	TorrentURL  string
	SourceShort string // tracker short_name — picks the cookie jar at fetch time
	SizeBytes   int64
}

// UpsertOfferPath records (or refreshes) the hash → local-path mapping.
func (db *DB) UpsertOfferPath(hash, path string, size int64) error {
	return db.UpsertOfferSource(hash, path, "", "", size)
}

// UpsertOfferRemote records (or refreshes) the hash → .torrent-URL mapping
// for a scraped tracker release with no local copy.
func (db *DB) UpsertOfferRemote(hash, torrentURL, sourceShort string, size int64) error {
	return db.UpsertOfferSource(hash, "", torrentURL, sourceShort, size)
}

// UpsertOfferSource is the full form. Per-column fill rather than row
// overwrite: one bucket can be reachable both locally and remotely, and the
// two facts are learned by different passes — a plain overwrite would mean
// whichever ran last erased the other route. An empty argument therefore
// means "I learned nothing about this route", never "clear it".
func (db *DB) UpsertOfferSource(hash, path, torrentURL, sourceShort string, size int64) error {
	if hash == "" || (path == "" && torrentURL == "") {
		return nil
	}
	_, err := db.Exec(`
		INSERT INTO offer_paths (offer_hash, local_path, torrent_url, source_short, size_bytes, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(offer_hash) DO UPDATE SET
		    local_path   = CASE WHEN excluded.local_path   != '' THEN excluded.local_path   ELSE offer_paths.local_path   END,
		    torrent_url  = CASE WHEN excluded.torrent_url  != '' THEN excluded.torrent_url  ELSE offer_paths.torrent_url  END,
		    source_short = CASE WHEN excluded.source_short != '' THEN excluded.source_short ELSE offer_paths.source_short END,
		    size_bytes   = excluded.size_bytes,
		    last_seen_at = excluded.last_seen_at`,
		hash, path, torrentURL, sourceShort, size, time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetOfferSource returns every route known for a hash. A zero-value row with
// a nil error means the agent has no way to serve this bucket.
func (db *DB) GetOfferSource(hash string) (OfferSourceRow, error) {
	var row OfferSourceRow
	if hash == "" {
		return row, nil
	}
	err := db.QueryRow(
		`SELECT local_path, torrent_url, source_short, size_bytes
		   FROM offer_paths WHERE offer_hash = ?`, hash,
	).Scan(&row.LocalPath, &row.TorrentURL, &row.SourceShort, &row.SizeBytes)
	if err == sql.ErrNoRows {
		return OfferSourceRow{}, nil
	}
	return row, err
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
