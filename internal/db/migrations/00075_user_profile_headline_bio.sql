-- +goose Up
-- Bulletproof Scheduling: an optional headline ("Independent Automation QA") and short bio
-- shown on the person directory (/u/{handle}). Empty strings mean "not shown", so every
-- existing page renders exactly as it would without them. Additive, so the previous image
-- still runs against a migrated database.
ALTER TABLE users ADD COLUMN headline TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN bio TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE users DROP COLUMN bio;
ALTER TABLE users DROP COLUMN headline;
