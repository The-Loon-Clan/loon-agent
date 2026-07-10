-- Migration 006 — Offer hash → local path cache.
--
-- The offer-sync service walks declared folder sources, computes a
-- canonical offer_hash per file (sha1 of pipe-joined identity), and
-- registers the metadata with the site. The site has no idea where
-- the file lives on the agent's disk — that's intentional, anti-
-- fingerprint. But when a request comes in for one of those buckets,
-- the agent needs to find the file again WITHOUT re-walking the
-- entire library.
--
-- offer_paths is that local index. Keyed by offer_hash so the fulfill
-- loop's lookup is one indexed query: bucket_id → site hash → local
-- path. Rows are overwritten on re-sync (path may move), and pruned
-- when the file no longer exists (Phase 3b housekeeping).

CREATE TABLE IF NOT EXISTS offer_paths (
    offer_hash   TEXT    PRIMARY KEY,
    local_path   TEXT    NOT NULL,
    size_bytes   INTEGER NOT NULL,
    last_seen_at TEXT    NOT NULL  -- RFC3339 timestamp
);

CREATE INDEX IF NOT EXISTS idx_offer_paths_last_seen
    ON offer_paths (last_seen_at);
