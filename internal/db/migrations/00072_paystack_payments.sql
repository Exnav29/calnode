-- +goose Up
-- Bulletproof Scheduling: Paystack as a second payment provider beside Stripe.
--
-- Additive only, so the stock Calnode 0.12.0 binary still runs against a migrated
-- database (it ignores columns it does not name) — that is the rollback path.

-- One Paystack account per instance. The secret key is encrypted like Stripe's; its
-- prefix (sk_test_ / sk_live_) is the test/live switch, there is no separate flag.
ALTER TABLE server_settings ADD COLUMN paystack_secret_key_enc TEXT NOT NULL DEFAULT '';
ALTER TABLE server_settings ADD COLUMN paystack_public_key     TEXT NOT NULL DEFAULT '';

-- Which provider a paid event type charges through. 'stripe' keeps every existing
-- paid event type exactly as it was.
--
-- Paystack charges an explicit, admin-set amount in the merchant's currency
-- (e.g. GHS 2,250.00 for an appointment listed at USD 150). The listed price
-- (price_cents/currency) stays what the booking page shows; the Paystack amount is
-- disclosed beside it. Nothing converts currencies automatically.
ALTER TABLE event_types ADD COLUMN payment_provider TEXT NOT NULL DEFAULT 'stripe'
    CHECK (payment_provider IN ('stripe', 'paystack'));
ALTER TABLE event_types ADD COLUMN paystack_amount_minor INTEGER NOT NULL DEFAULT 0;
ALTER TABLE event_types ADD COLUMN paystack_currency     TEXT    NOT NULL DEFAULT 'GHS';

-- Per-booking Paystack state. The expected amount/currency are snapshotted when the
-- checkout starts, so a later price edit can never change what a payment is checked
-- against. payment_status gains one value for Paystack holds: 'expired' (hold
-- released unpaid; a late payment against it is refunded, never confirmed).
ALTER TABLE bookings ADD COLUMN payment_provider           TEXT    NOT NULL DEFAULT '';
ALTER TABLE bookings ADD COLUMN paystack_reference         TEXT    NOT NULL DEFAULT '';
ALTER TABLE bookings ADD COLUMN paystack_expected_amount   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE bookings ADD COLUMN paystack_expected_currency TEXT    NOT NULL DEFAULT '';
ALTER TABLE bookings ADD COLUMN paystack_mode              TEXT    NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_bookings_paystack_reference
    ON bookings (paystack_reference) WHERE paystack_reference <> '';
CREATE INDEX idx_bookings_paystack_pending
    ON bookings (payment_provider, payment_status) WHERE payment_provider = 'paystack';

-- Every Paystack webhook delivery, logged before anything is acted on — including
-- ones that fail signature verification. (event, reference) is the dedupe key.
CREATE TABLE paystack_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    event           TEXT    NOT NULL DEFAULT '',
    reference       TEXT    NOT NULL DEFAULT '',
    signature_valid INTEGER NOT NULL DEFAULT 0,
    applied         INTEGER NOT NULL DEFAULT 0,
    note            TEXT    NOT NULL DEFAULT '',
    raw             TEXT    NOT NULL DEFAULT '',
    received_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE UNIQUE INDEX idx_paystack_events_dedupe ON paystack_events (event, reference)
    WHERE signature_valid = 1;

-- +goose Down
DROP TABLE IF EXISTS paystack_events;
DROP INDEX IF EXISTS idx_bookings_paystack_pending;
DROP INDEX IF EXISTS idx_bookings_paystack_reference;
