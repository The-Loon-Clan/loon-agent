-- Migration 007 — remember where a REMOTE offer can be fetched from.
--
-- offer_paths (006) answers "where is this bucket's file on my disk", which
-- is the whole answer for folder sources and no answer at all for tracker
-- sources: the scraper knows the .torrent URL at scan time, threw it away,
-- and the fulfill loop then skipped every tracker-sourced request forever
-- because the hash had no cached path. The request reopened every 15
-- minutes and nothing ever delivered it.
--
-- So the cache stores both routes to the same bucket. A hash may legitimately
-- have EITHER or BOTH (a scraped release that also happens to sit in a
-- declared folder), which is why the upsert fills per column instead of
-- overwriting the row: whichever sync pass runs second must not erase what
-- the first one learned.
--
-- local_path keeps its NOT NULL and carries '' for remote-only rows —
-- rewriting the table to relax the constraint would cost more than the
-- sentinel is worth, and the read path already treats '' as absent.

ALTER TABLE offer_paths ADD COLUMN torrent_url  TEXT NOT NULL DEFAULT '';
ALTER TABLE offer_paths ADD COLUMN source_short TEXT NOT NULL DEFAULT '';

-- The fulfill loop asks "is there anything I can do with this hash", so the
-- useful index is on rows that carry a remote route.
CREATE INDEX IF NOT EXISTS idx_offer_paths_torrent_url
    ON offer_paths (offer_hash) WHERE torrent_url != '';
