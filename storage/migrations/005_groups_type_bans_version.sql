-- Groups gain four columns to support type-aware sampling and site sync:
--
--   type                    — 'video' | 'manga' | 'music' | future. No CHECK
--                             constraint so site admins can introduce new
--                             types (audiobook, software, ...) without an
--                             agent migration. Unknown types skip sampling
--                             in the offline processor rather than erroring.
--   banned_extensions_json  — per-group override for the file-type blocklist
--                             the pipeline strips before staging. Empty
--                             array means "inherit the hardcoded default"
--                             (services.DefaultBlockedExtensions); a
--                             non-empty list replaces the default outright
--                             so music groups can allow .iso, video groups
--                             can block .html, etc.
--   sample_seconds          — duration (seconds) of each audio sample clip.
--                             Only meaningful when type='music'. NULL falls
--                             back to services.DefaultSampleSeconds.
--   version                 — monotonically increasing, bumped on every
--                             UPDATE via a trigger below. Site-pushed groups
--                             carry the site's version directly so the
--                             agent can poll "since_version=N" efficiently.
ALTER TABLE groups ADD COLUMN type TEXT NOT NULL DEFAULT 'video';
ALTER TABLE groups ADD COLUMN banned_extensions_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE groups ADD COLUMN sample_seconds INTEGER;
ALTER TABLE groups ADD COLUMN version INTEGER NOT NULL DEFAULT 1;

CREATE INDEX idx_groups_type ON groups(type);

-- Auto-bump version on every UPDATE so the UI / site never has to remember
-- to set it. Uses a trigger rather than application code so direct-SQL
-- edits during debugging or admin-tooling paths also bump correctly.
-- WHEN clause guards against recursion: the trigger's own UPDATE would
-- otherwise fire it again, incrementing version twice per write.
CREATE TRIGGER groups_bump_version
AFTER UPDATE ON groups
WHEN NEW.version = OLD.version
BEGIN
    UPDATE groups SET version = OLD.version + 1 WHERE id = OLD.id;
END;
