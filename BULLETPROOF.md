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
  applies to Stripe bookings. The attendee's cancellation email says a refund of the amount
  paid "has been requested"; it never says the money has arrived.
- **`refunded` means requested, not settled.** A booking's `payment_status` becomes
  `refunded` when Paystack (or Stripe) *accepts* the refund request. Settlement can take
  days, especially for Mobile Money, and the app does not track it: check the refund's
  status at Paystack (`processed`) before telling anyone the money is back. The API value and
  the `booking.cancelled` webhook keep the name `refunded` for compatibility; the admin
  bookings list shows it as "Refund requested".
- **Mobile Money refunds can stop at `needs-attention`.** In the GH₵1.00 live test
  (2026-11-02) a full refund of an MTN MoMo charge went from `pending` to `needs-attention`
  about 35 minutes after it was created, because Paystack had no destination account; no
  money had moved (`deducted_amount` 0). To finish it, open Paystack Dashboard → Refunds,
  filter *Needs attention*, open the refund and retry it: the retry screen offers **MTN
  Mobile Money** as a destination, using the customer's wallet phone number. After the
  retry it moved to `processing`, and was `processed` (charge `reversed`) about 13 minutes
  later, 1h59m after the refund was created. The app is not told about any of this: the
  booking stays "Refund requested", and Paystack's `refund.needs-attention` webhook goes to
  the training site, which only logs it. Refund exception monitoring (below) is what
  notices it.
- **Refund exception monitoring.** Every 15 minutes the reconciler reads back, from
  Paystack, the refunds of every refunded Paystack booking that is not yet resolved
  (`GET /transaction/verify` once for the numeric transaction id, then
  `GET /refund?transaction=<id>`). A refund is recorded only when Paystack ties it to the
  booking by both that id and the `BPS-` reference. Each time a refund *enters*
  `needs-attention` or `failed`, one alert email goes to `PAYSTACK_REFUND_ALERT_EMAIL`
  (comma-separated; default: every active admin) with the refund ID, booking and payment
  references, amount, status, Paystack's reason, a Paystack Dashboard link and the action
  required. needs-attention → processing → needs-attention alerts twice.
  - *Stops* when every refund on the payment is `processed`. A booking whose refunds have
    all `failed` is re-read once a day (so a Dashboard retry is noticed) and stays in the
    admin banner until an admin acknowledges it.
  - *Duplicates*: `paystack_refunds.exception_seq` / `alerted_seq` are the persistent record;
    the Resend `Idempotency-Key` `bps-refund-<refund id>-<occurrence>` covers a send whose
    response was lost (Resend keeps keys 24 hours).
  - *Failures*: a failed or impossible send (Resend down, email not configured) leaves the
    alert due; it is retried every sweep and logged once until the error changes. A failed
    Paystack check changes nothing and is likewise logged once. Both show in a red banner
    on every admin page (`GET /v1/paystack/refund-exceptions`), which does not depend on
    email.
  - *Read-only*: the monitor holds the Paystack client only through an interface with
    `Verify` and `ListRefunds`; the client has no method to retry a refund or send a
    customer's account details, and a test fails if the monitor makes any non-GET call.
    Resolving an exception is always manual, in the Paystack Dashboard.
- **Customer wording.** Paid event types say "Continue to payment" instead of "Confirm
  booking". Paystack event types show a Mobile Money note before checkout (approve the
  request in the MoMo app; don't pay again while it is processing). A return from checkout
  is shown as "not completed" only when Paystack's API says the charge failed or was
  abandoned, never merely because the browser did not come back.

## Pre-launch requirements for paid appointments

Both paid event types (ads-call GH₵1,800, project-discovery GH₵3,000) stay inactive until
every item below is verified and the owner separately authorizes activation.

- [x] Live GH₵1.00 Paystack charge settled by the reconciler (2026-11-02).
- [x] That test's refund (#18949128) reached `processed` at Paystack (08:53 UTC, after a
      manual MoMo retry), and the training ledger recorded it unmatched with enrollments
      unchanged.
- [x] Customer-facing payment and refund wording deployed (0.12.0-bp.4, 2026-11-02).
- [ ] Email sent through Resend (sending-only key, domain verified via Cloudflare DNS), with
      booking, reschedule, cancellation and paid-cancellation emails delivered and no
      duplicate calendar invites.
- [ ] **Refund exception alerts**: when Paystack reports a refund as `needs-attention` or
      `failed`, one email goes to johnathan@bulletproofautomations.com through Resend with
      the refund ID, booking reference, amount and currency, status, reason, a Paystack
      link and the action required. The app never supplies customer bank or Mobile Money
      details and never retries such a refund; resolving it is manual.

## Building

```bash
docker build --build-arg VERSION=0.12.0-bp.N --build-arg COMMIT=$(git rev-parse HEAD) \
  -t bulletproof-scheduling:0.12.0-bp.N .
```

`PAYSTACK_API_BASE` points the Paystack client at a stand-in API for end-to-end tests. It is
honoured only under an `sk_test_` key. `PAYSTACK_REFUND_ALERT_EMAIL` sets who receives
refund exception alerts (default: every active admin).

## Keeping up with upstream

Merge Calnode releases into this branch. Upstream migrations numbered `00072` or higher
will collide with `00072_paystack_payments.sql`; renumber the upstream file(s) after ours
on merge (never ours — deployed databases have already recorded version 72).
