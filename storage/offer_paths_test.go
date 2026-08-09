package storage

import (
	"path/filepath"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := OpenDB(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// The cache holds two independent routes to one bucket — a local file and a
// remote .torrent — learned by different passes. A row-level overwrite would
// mean whichever pass ran second erased the other route, which is precisely
// how tracker-sourced offers ended up unfulfillable. Per-column fill is the
// behaviour, so this asserts both orders.
func TestUpsertOfferSourcePreservesTheOtherRoute(t *testing.T) {
	db := openTestDB(t)

	t.Run("remote then local", func(t *testing.T) {
		const h = "hash-remote-then-local"
		if err := db.UpsertOfferRemote(h, "https://t/x.torrent", "nyaa", 100); err != nil {
			t.Fatalf("upsert remote: %v", err)
		}
		if err := db.UpsertOfferPath(h, "/data/x.mkv", 200); err != nil {
			t.Fatalf("upsert local: %v", err)
		}
		got, err := db.GetOfferSource(h)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.LocalPath != "/data/x.mkv" {
			t.Errorf("LocalPath = %q, want the local pass's value", got.LocalPath)
		}
		if got.TorrentURL != "https://t/x.torrent" {
			t.Errorf("TorrentURL = %q — the local pass erased the remote route", got.TorrentURL)
		}
		if got.SourceShort != "nyaa" {
			t.Errorf("SourceShort = %q — lost the tracker identity, so the fetch cannot pick a cookie jar", got.SourceShort)
		}
		if got.SizeBytes != 200 {
			t.Errorf("SizeBytes = %d, want the latest write's 200", got.SizeBytes)
		}
	})

	t.Run("local then remote", func(t *testing.T) {
		const h = "hash-local-then-remote"
		if err := db.UpsertOfferPath(h, "/data/y.mkv", 300); err != nil {
			t.Fatalf("upsert local: %v", err)
		}
		if err := db.UpsertOfferRemote(h, "https://t/y.torrent", "ab", 400); err != nil {
			t.Fatalf("upsert remote: %v", err)
		}
		got, err := db.GetOfferSource(h)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.LocalPath != "/data/y.mkv" {
			t.Errorf("LocalPath = %q — the remote pass erased the local route", got.LocalPath)
		}
		if got.TorrentURL != "https://t/y.torrent" {
			t.Errorf("TorrentURL = %q, want the remote pass's value", got.TorrentURL)
		}
	})
}

// GetOfferPath is the pre-existing reader; it must keep answering exactly as
// before now that the row has more columns.
func TestGetOfferPathStillReadsLocalOnly(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertOfferRemote("remote-only", "https://t/z.torrent", "nyaa", 10); err != nil {
		t.Fatalf("upsert remote: %v", err)
	}
	// A remote-only bucket has no local path, and the legacy reader must say
	// so rather than returning the sentinel row's empty string as a path.
	if p, err := db.GetOfferPath("remote-only"); err != nil || p != "" {
		t.Errorf("GetOfferPath(remote-only) = %q, %v; want empty", p, err)
	}
	if err := db.UpsertOfferPath("with-file", "/data/a.mkv", 10); err != nil {
		t.Fatalf("upsert local: %v", err)
	}
	if p, err := db.GetOfferPath("with-file"); err != nil || p != "/data/a.mkv" {
		t.Errorf("GetOfferPath(with-file) = %q, %v", p, err)
	}
}

func TestOfferSourceMissAndNoOpWrites(t *testing.T) {
	db := openTestDB(t)
	// A miss is a zero row and a nil error, never an error — "I cannot serve
	// this" is an ordinary answer for the fulfill loop.
	got, err := db.GetOfferSource("never-seen")
	if err != nil {
		t.Fatalf("miss returned an error: %v", err)
	}
	if got != (OfferSourceRow{}) {
		t.Errorf("miss returned %+v, want the zero row", got)
	}
	// Writes with nothing to say are no-ops, not errors or empty rows.
	if err := db.UpsertOfferSource("", "/x", "", "", 1); err != nil {
		t.Errorf("empty hash should be a no-op, got %v", err)
	}
	if err := db.UpsertOfferSource("h", "", "", "", 1); err != nil {
		t.Errorf("no routes should be a no-op, got %v", err)
	}
	if got, _ := db.GetOfferSource("h"); got != (OfferSourceRow{}) {
		t.Errorf("routeless write created a row: %+v", got)
	}
}
