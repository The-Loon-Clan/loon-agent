-- Offline jobs: each detected file in a watch folder gets a row here.
-- source_path is UNIQUE so re-scanning a folder doesn't create duplicates
-- (the watcher's "is this new?" check is a SELECT on this column).
-- group_id gets snapshotted into group_name_at_creation so renaming or
-- deleting the group later doesn't lose the audit trail.
--
-- status transitions: queued → processing → (completed | failed | cancelled).
-- The processor claims a row with UPDATE ... WHERE status='queued' so it's
-- safe against a future multi-worker setup.
CREATE TABLE offline_jobs (
    id                        INTEGER PRIMARY KEY AUTOINCREMENT,
    source_path               TEXT NOT NULL UNIQUE,
    title                     TEXT NOT NULL,
    group_id                  INTEGER REFERENCES groups(id) ON DELETE SET NULL,
    group_name_at_creation    TEXT NOT NULL,
    status                    TEXT NOT NULL DEFAULT 'queued'
                                CHECK (status IN ('queued','processing','completed','failed','cancelled')),
    phase                     TEXT NOT NULL DEFAULT '',
    progress                  REAL NOT NULL DEFAULT 0,
    error                     TEXT,
    nzb_path                  TEXT,
    password                  TEXT,
    created_at                TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at                TIMESTAMP,
    completed_at              TIMESTAMP
);
CREATE INDEX idx_offline_jobs_status ON offline_jobs(status);
CREATE INDEX idx_offline_jobs_created_at ON offline_jobs(created_at DESC);
