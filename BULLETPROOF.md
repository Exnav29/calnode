# Bulletproof Scheduling

**Bulletproof Scheduling** is the appointment scheduler of Bulletproof Automations,
served at <https://book.bulletproofautomations.com>. It is a fork of
[Calnode](https://github.com/Calnode/calnode) (Apache-2.0), branched from release
**v0.12.0**. It is not an official Calnode product and is not endorsed by the
Calnode project; see [TRADEMARK.md](TRADEMARK.md). The original [LICENSE](LICENSE)
and copyright notices are unchanged.

## What this fork changes

| Area | Change |
|---|---|
| Branding | Bulletproof palette (navy `#091a36`, royal `#2366e9`, bright `#55abf7`, mist `#f3f5f8`) and fonts (Oswald headings, Inter body, self-hosted under the SIL OFL in `frontend/static/fonts/`) across the admin SPA, public booking/manage/directory pages and emails. Product name in titles, sign-in, setup, email subjects. Favicon and icon from bulletproofautomations.com. Public footer reads "Bulletproof Scheduling by Bulletproof Automations · built on Calnode". |
| Payments | **Paystack** as a second provider beside Stripe, chosen per event type. Stripe is unchanged. |

Everything else is upstream Calnode v0.12.0.

## Paystack design

Code: `internal/paystack/` (client), `internal/handler/paystack_booking.go` (lifecycle),
`internal/handler/paystack_settings.go`, migration `00072_paystack_payments.sql`.
Patterns are adapted from the Bulletproof training site's Paystack integration.

- **One settle path.** A payment is accepted only by `settlePaystackReference`, which calls
  Paystack's `GET /transaction/verify/:reference` and checks status, reference, amount,
  currency and test/live domain against the values snapshotted on the booking when checkout
  started. The browser return URL and webhook body are only triggers.
- **Exactly once.** `pending → paid` is a conditional UPDATE; only its winner dispatches the
  confirmation (calendar event, emails, meeting link, webhooks).
- **Holds.** The slot is held (status `confirmed`, payment `pending`) for 30 minutes. A failed
  or cancelled checkout releases it immediately; an abandoned one is released by the
  reconciler. Released holds become `cancelled` / `expired`.
- **Late or wrong payments are refunded, never confirmed**: a charge that lands on a released
  or cancelled hold, or that does not match the expected amount, currency or mode.
- **Mode safety.** The secret key prefix is the mode switch. References are
  `BPS-TEST-…` / `BPS-LIVE-…`; a test charge cannot settle a live booking. Under a test key,
  bookings are only accepted from a page opened with `?paystack=test`.
- **Shared Paystack account.** Paystack allows one webhook URL per mode, and it belongs to
  the training site. Scheduling does not need it: a reconciler (every minute) verifies
  pending holds, releases expired ones, refunds late payments and retries failed refunds.
  `POST /v1/paystack/webhook` (HMAC-SHA512 over the raw body, logged to `paystack_events`)
  is available for a relay from the training receiver; it ignores non-`BPS-` references.
- **Currency.** Each Paystack event type has an explicit, admin-set charge
  (`paystack_amount_minor` + `paystack_currency`, e.g. GHS 2,250.00), shown on the booking
  page beside the listed price ("$150.00 · charged as 2250.00 GHS via Paystack"). Nothing is
  converted automatically.
- **Cancellation.** Cancelling a paid booking refunds it in full, the same policy Calnode
  applies to Stripe bookings.

## Building

```bash
docker build --build-arg VERSION=0.12.0-bp.N --build-arg COMMIT=$(git rev-parse HEAD) \
  -t bulletproof-scheduling:0.12.0-bp.N .
```

`PAYSTACK_API_BASE` points the Paystack client at a stand-in API for end-to-end tests. It is
honoured only under an `sk_test_` key.

## Keeping up with upstream

Merge Calnode releases into this branch. Upstream migrations numbered `00072` or higher
will collide with `00072_paystack_payments.sql`; renumber the upstream file(s) after ours
on merge (never ours — deployed databases have already recorded version 72).
