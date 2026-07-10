-- Groups are the organizing concept for standalone/offline uploads:
-- each group has a human name, a set of newsgroups to post into, and
-- per-group overrides for the global posting defaults (PAR2, screenshots,
-- obfuscation). The `source` column is 'local' for groups the operator
-- created in the UI; later the main site may push groups down with
-- source='site' so agents can inherit a centrally-managed category set
-- without dropping locally-defined ones.
CREATE TABLE groups (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    name             TEXT NOT NULL UNIQUE,
    newsgroups_json  TEXT NOT NULL DEFAULT '[]',  -- JSON array of newsgroup names
    -- Override knobs. NULL means "inherit from env/yml defaults" so the
    -- operator can keep a group simple by leaving fields blank.
    screenshots      INTEGER,                     -- count, NULL = inherit
    par2_redundancy  INTEGER,                     -- percent, NULL = inherit
    obfuscate        INTEGER,                     -- 0/1, NULL = inherit
    source           TEXT NOT NULL DEFAULT 'local' CHECK (source IN ('local','site')),
    created_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at       TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_groups_source ON groups(source);
