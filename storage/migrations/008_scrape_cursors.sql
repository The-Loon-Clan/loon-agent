-- Scrape cursors: where each paging tracker scrape resumes.
--
-- A Torznab catalog walk covers a large tracker across many sync ticks
-- (max_pages_per_tick pages each), so the position has to survive the
-- process. One row per source short_name; next_offset 0 means "start
-- over from the newest", which is also what a finished walk resets to.
CREATE TABLE IF NOT EXISTS scrape_cursors (
    source_short TEXT PRIMARY KEY,
    next_offset  INTEGER NOT NULL DEFAULT 0,
    updated_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
