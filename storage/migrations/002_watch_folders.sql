-- Watch folders are filesystem paths the offline pipeline monitors for
-- new content. Each folder is tagged with a group, so dropping a file
-- into /data/watch/anime routes through the anime group's newsgroups and
-- override settings. enabled=0 keeps the row but stops the watcher from
-- scanning it — useful for pausing a category without losing config.
--
-- ON DELETE SET NULL on the group FK rather than CASCADE: if the user
-- deletes a group, the watch survives with group_id=NULL and the watcher
-- skips it (logs "unassigned") instead of the row vanishing silently.
CREATE TABLE watch_folders (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    path        TEXT NOT NULL UNIQUE,
    group_id    INTEGER REFERENCES groups(id) ON DELETE SET NULL,
    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX idx_watch_folders_enabled ON watch_folders(enabled);
