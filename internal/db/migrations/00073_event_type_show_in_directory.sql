-- +goose Up
-- Bulletproof Scheduling: "Show in directory". A public, active event type stays bookable
-- at /book/{slug}; this only decides whether the person (/u/{handle}) and team
-- (/team/{slug}) directories list it. 1 keeps every existing event type listed exactly as
-- before. Additive, so the previous image still runs against a migrated database.
ALTER TABLE event_types ADD COLUMN show_in_directory INTEGER NOT NULL DEFAULT 1
    CHECK (show_in_directory IN (0, 1));

-- +goose Down
ALTER TABLE event_types DROP COLUMN show_in_directory;
