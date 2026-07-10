-- Watermark text is rendered onto every screenshot generated for a job
-- in this group. Empty string disables watermarking; non-empty draws the
-- text in the bottom-right of the frame via ffmpeg drawtext. Kept on the
-- group row so a single UI edit updates every future job — the
-- "parent group settings" pattern the operator asked for.
ALTER TABLE groups ADD COLUMN watermark_text TEXT NOT NULL DEFAULT '';
