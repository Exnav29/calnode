-- +goose Up
-- Bulletproof Scheduling: refund exception monitoring. Paystack's webhook belongs to the
-- training site, so the reconciler reads refund state back from Paystack (GET only) for
-- every refunded Paystack booking until its refund is processed, and emails the admin
-- once each time a refund ENTERS needs-attention or failed. Additive, so the previous
-- image still runs against a migrated database.

-- One row per refunded Paystack booking being watched. transaction_id is Paystack's
-- numeric id for the booking's own reference (from GET /transaction/verify), cached so a
-- refund is only ever matched to its booking by that id AND the reference.
CREATE TABLE paystack_refund_watch (
    booking_id     TEXT    PRIMARY KEY REFERENCES bookings(id) ON DELETE CASCADE,
    transaction_id INTEGER NOT NULL DEFAULT 0,
    resolved_at    TEXT,                       -- every refund on the transaction is processed
    checked_at     TEXT,
    last_error     TEXT    NOT NULL DEFAULT '' -- last check failure; logged only when it changes
);

-- Every refund Paystack reports for a watched booking. exception_seq counts entries into
-- needs-attention/failed; an alert is due while alerted_seq < exception_seq, so a refund
-- that goes needs-attention -> processing -> needs-attention alerts twice, and a failed
-- send stays due until it succeeds. acknowledged_seq hides the admin banner for an
-- exception the admin has dealt with outside Paystack.
CREATE TABLE paystack_refunds (
    refund_id             INTEGER PRIMARY KEY, -- Paystack refund id
    booking_id            TEXT    NOT NULL REFERENCES bookings(id) ON DELETE CASCADE,
    transaction_id        INTEGER NOT NULL,
    transaction_reference TEXT    NOT NULL,
    amount_minor          INTEGER NOT NULL,
    currency              TEXT    NOT NULL,
    status                TEXT    NOT NULL,
    reason                TEXT    NOT NULL DEFAULT '',
    exception_seq         INTEGER NOT NULL DEFAULT 0,
    alerted_seq           INTEGER NOT NULL DEFAULT 0,
    acknowledged_seq      INTEGER NOT NULL DEFAULT 0,
    alert_error           TEXT    NOT NULL DEFAULT '', -- last send failure, shown in the admin banner
    alerted_at            TEXT,
    first_seen_at         TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    status_changed_at     TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);
CREATE INDEX idx_paystack_refunds_booking ON paystack_refunds (booking_id);

-- +goose Down
DROP TABLE IF EXISTS paystack_refunds;
DROP TABLE IF EXISTS paystack_refund_watch;
