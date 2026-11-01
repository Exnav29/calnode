package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/paystack"
)

// Paystack paid bookings (Bulletproof Scheduling).
//
// Lifecycle, mirroring the Stripe flow's states on the bookings row:
//
//	CreateBooking ─ hold the slot (status=confirmed, payment_status=pending, side-effects deferred)
//	             └ startPaystackCheckout: mint BPS-<mode>- reference, snapshot expected amount,
//	               initialize the transaction, redirect the booker to Paystack.
//	settlePaystackReference ─ the ONLY place a payment is accepted. It asks Paystack
//	               (GET /transaction/verify) and checks status, reference, amount, currency
//	               and test/live domain against the snapshot. pending→paid is a conditional
//	               UPDATE, so confirmation side-effects run exactly once however many of the
//	               three triggers (browser return, webhook, reconciler) arrive.
//	expirePaystackHold ─ unpaid holds are released (status=cancelled, payment_status=expired).
//	A payment that lands on a released or cancelled hold is refunded, never confirmed:
//	the slot may already belong to someone else.
//
// Nothing from the browser is trusted: the return URL only tells us WHICH reference to go
// and verify. The webhook body is likewise only a trigger; its signature is checked and it
// is logged, but the decision is made on the verify API's answer.

// paystackHoldWindow is how long a Paystack hold keeps the slot. Paystack transactions have
// no expiry of their own, so this is enforced by the reconciler.
const paystackHoldWindow = 30 * time.Minute

// paystackLateWindow is how long a released hold keeps being checked for a late payment
// (one completed on the still-open Paystack page after the hold expired) to refund.
const paystackLateWindow = 48 * time.Hour

// Settlement outcomes.
const (
	psPaid       = "paid"       // verified and confirmed (now or earlier)
	psProcessing = "processing" // Paystack has not finished; try again later
	psUnpaid     = "unpaid"     // failed / abandoned / never started — not paid
	psRefunded   = "refunded"   // paid, but against a released/cancelled hold or wrong amount: refunded
	psRefundDue  = "refund_due" // as above, but the refund call failed; retried by the reconciler
)

// errPaystackUnverifiable means the booking cannot be checked right now (no client, or
// the configured key is for the other environment). Never treated as "unpaid".
var errPaystackUnverifiable = errors.New("paystack: cannot verify with the current configuration")

// paystackBookable returns "" when a Paystack-priced event type can take a booking now,
// else the message for the booker. Checked before a slot is held.
func (h *Handler) paystackBookable(et *bookableEventType, testOptIn bool) string {
	const unavailable = "payments are temporarily unavailable"
	pc := h.getPaystack()
	if pc == nil || et.PaystackAmountMinor <= 0 || len(et.PaystackCurrency) != 3 {
		return unavailable
	}
	// Test keys: only an explicit ?paystack=test page may reach checkout.
	if pc.Mode() == paystack.ModeTest && !testOptIn {
		return unavailable
	}
	return ""
}

// startPaystackCheckout records the reference and the server-side expected charge on the
// held booking, then initializes the Paystack transaction. The row is written FIRST so a
// webhook or return that races the initialize call always finds its booking.
func (h *Handler) startPaystackCheckout(ctx context.Context, bookingID string, et *bookableEventType, slug, email string) (string, error) {
	pc := h.getPaystack()
	if pc == nil {
		return "", errPaystackUnverifiable
	}
	ref, err := paystack.MintReference(pc.Mode(), time.Now())
	if err != nil {
		return "", err
	}
	currency := strings.ToUpper(et.PaystackCurrency)
	if _, err := h.db.ExecContext(ctx, `
		UPDATE bookings SET payment_provider = 'paystack', payment_status = 'pending',
		       paystack_reference = ?, paystack_expected_amount = ?, paystack_expected_currency = ?,
		       paystack_mode = ?
		WHERE id = ?`, ref, et.PaystackAmountMinor, currency, string(pc.Mode()), bookingID); err != nil {
		return "", err
	}
	base := h.publicURL()
	returnURL := base + "/v1/paystack/return"
	init, err := pc.Initialize(ctx, paystack.InitParams{
		Email:       email,
		AmountMinor: int64(et.PaystackAmountMinor),
		Currency:    currency,
		Reference:   ref,
		CallbackURL: returnURL, // Paystack appends ?trxref=&reference=
		Metadata: map[string]any{
			"app":        "bulletproof-scheduling",
			"booking_id": bookingID,
			"event_type": slug,
			// The checkout page's cancel button returns here too, so an abandoned
			// checkout releases its hold at once instead of after the hold window.
			"cancel_action": returnURL + "?reference=" + url.QueryEscape(ref),
			"custom_fields": []map[string]string{
				{"display_name": "Appointment", "variable_name": "appointment", "value": et.Name},
			},
		},
	})
	if err != nil {
		return "", err
	}
	return init.AuthorizationURL, nil
}

type paystackBookingRow struct {
	ID, Status, PaymentStatus, Slug, Currency, Mode, CreatedAt string
	Amount                                                     int64
}

func (h *Handler) loadPaystackBooking(ctx context.Context, ref string) (*paystackBookingRow, error) {
	var b paystackBookingRow
	err := h.db.QueryRowContext(ctx, `
		SELECT b.id, b.status, b.payment_status, et.slug, b.paystack_expected_currency,
		       b.paystack_mode, b.created_at, b.paystack_expected_amount
		FROM bookings b JOIN event_types et ON et.id = b.event_type_id
		WHERE b.paystack_reference = ? AND b.payment_provider = 'paystack'`, ref).
		Scan(&b.ID, &b.Status, &b.PaymentStatus, &b.Slug, &b.Currency, &b.Mode, &b.CreatedAt, &b.Amount)
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// settlePaystackReference verifies one reference with Paystack and applies the result.
// Idempotent and safe to call concurrently. A returned error is transient (Paystack or
// the DB unreachable) and means nothing was decided.
func (h *Handler) settlePaystackReference(ctx context.Context, ref string) (string, error) {
	b, err := h.loadPaystackBooking(ctx, ref)
	if err != nil {
		return "", err
	}
	if b.PaymentStatus == "paid" {
		return psPaid, nil
	}
	if b.PaymentStatus == "refunded" {
		return psRefunded, nil
	}
	pc := h.getPaystack()
	if pc == nil || string(pc.Mode()) != b.Mode {
		// A test-mode booking can only be verified with the test key and vice versa.
		return "", errPaystackUnverifiable
	}
	txn, err := pc.Verify(ctx, ref)
	if errors.Is(err, paystack.ErrNotFound) {
		return psUnpaid, nil // checkout never started
	}
	if err != nil {
		return "", err
	}
	switch txn.Status {
	case "success":
	case "ongoing", "pending", "processing", "queued":
		return psProcessing, nil
	default: // failed, abandoned, reversed
		return psUnpaid, nil
	}

	// Paystack says the money moved. Check it is exactly the charge we asked for, in the
	// environment we asked for, before it can confirm anything.
	if mismatch := paystackMismatch(b, txn, ref); mismatch != "" {
		h.logger.ErrorContext(ctx, "paystack: successful charge does not match booking; refunding",
			"booking_id", b.ID, "reference", ref, "problem", mismatch)
		h.expirePaystackHold(ctx, b.ID)
		return h.refundLatePaystack(ctx, b.ID, ref), nil
	}

	res, err := h.db.ExecContext(ctx, `
		UPDATE bookings SET payment_status = 'paid', amount_paid_cents = ?, amount_paid_currency = ?
		WHERE id = ? AND status = 'confirmed' AND payment_status = 'pending'`,
		txn.Amount, strings.ToLower(txn.Currency), b.ID)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 1 {
		h.logger.InfoContext(ctx, "paystack: booking paid", "booking_id", b.ID, "reference", ref, "mode", b.Mode)
		h.dispatchPaidConfirmation(ctx, b.ID)
		return psPaid, nil
	}
	// Lost the race to another trigger, or the hold is gone. Re-read to tell which.
	b, err = h.loadPaystackBooking(ctx, ref)
	if err != nil {
		return "", err
	}
	if b.PaymentStatus == "paid" {
		return psPaid, nil // another trigger confirmed it — the duplicate stops here
	}
	h.logger.WarnContext(ctx, "paystack: payment arrived for a released or cancelled hold; refunding",
		"booking_id", b.ID, "reference", ref, "status", b.Status, "payment_status", b.PaymentStatus)
	return h.refundLatePaystack(ctx, b.ID, ref), nil
}

// paystackMismatch returns why a successful transaction cannot confirm this booking, or "".
func paystackMismatch(b *paystackBookingRow, txn *paystack.Transaction, ref string) string {
	switch {
	case txn.Reference != ref:
		return "reference " + txn.Reference
	case txn.Domain != b.Mode || paystack.ReferenceMode(ref) != paystack.Mode(b.Mode):
		return fmt.Sprintf("domain %s on a %s booking", txn.Domain, b.Mode)
	case txn.Amount != b.Amount:
		return fmt.Sprintf("amount %d, expected %d", txn.Amount, b.Amount)
	case !strings.EqualFold(txn.Currency, b.Currency):
		return fmt.Sprintf("currency %s, expected %s", txn.Currency, b.Currency)
	}
	return ""
}

// expirePaystackHold releases a still-unpaid hold. No-op once paid or already resolved.
func (h *Handler) expirePaystackHold(ctx context.Context, bookingID string) {
	res, err := h.db.ExecContext(ctx, `
		UPDATE bookings SET status = 'cancelled', cancellation_reason = 'payment not completed',
		       payment_status = 'expired'
		WHERE id = ? AND status = 'confirmed' AND payment_status = 'pending' AND payment_provider = 'paystack'`,
		bookingID)
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack: release hold", "error", err, "booking_id", bookingID)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		h.logger.InfoContext(ctx, "paystack: released unpaid hold", "booking_id", bookingID)
	}
}

// refundLatePaystack refunds a successful charge that will not be honoured (hold released,
// booking cancelled, or charge mismatch). Claimed with a conditional UPDATE so concurrent
// triggers refund at most once; a failed refund is left as 'refund_due' for the reconciler.
func (h *Handler) refundLatePaystack(ctx context.Context, bookingID, ref string) string {
	claimed, err := h.db.ExecContext(ctx, `
		UPDATE bookings SET payment_status = 'refunding'
		WHERE id = ? AND status = 'cancelled' AND payment_status IN ('pending', 'expired', 'refund_due')`, bookingID)
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack: claim late refund", "error", err, "booking_id", bookingID)
		return psRefundDue
	}
	if n, _ := claimed.RowsAffected(); n == 0 {
		var ps string
		_ = h.db.QueryRowContext(ctx, `SELECT payment_status FROM bookings WHERE id = ?`, bookingID).Scan(&ps)
		if ps == "paid" {
			return psPaid
		}
		return psRefunded // refunded, or being refunded by another trigger
	}
	return h.finishPaystackRefund(ctx, bookingID, ref, "refund_due")
}

// finishPaystackRefund calls Paystack for a row already claimed as 'refunding' and records
// the result; on failure the row goes back to onFail so it is retried, never lost.
func (h *Handler) finishPaystackRefund(ctx context.Context, bookingID, ref, onFail string) string {
	pc := h.getPaystack()
	var err error
	if pc == nil {
		err = errPaystackUnverifiable
	} else {
		err = pc.Refund(ctx, ref)
	}
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack: refund failed", "error", err, "booking_id", bookingID, "reference", ref)
		_, _ = h.db.ExecContext(ctx,
			`UPDATE bookings SET payment_status = ? WHERE id = ? AND payment_status = 'refunding'`, onFail, bookingID)
		if onFail == "paid" {
			return psPaid
		}
		return psRefundDue
	}
	if _, err := h.db.ExecContext(ctx,
		`UPDATE bookings SET payment_status = 'refunded' WHERE id = ? AND payment_status = 'refunding'`, bookingID); err != nil {
		h.logger.ErrorContext(ctx, "paystack: mark refunded", "error", err, "booking_id", bookingID)
	}
	h.logger.InfoContext(ctx, "paystack: refunded", "booking_id", bookingID, "reference", ref)
	return psRefunded
}

// refundPaystackBooking refunds a PAID Paystack booking that is being cancelled — the same
// refund-on-cancel policy as Stripe bookings. Concurrent cancels refund once (claim), and a
// failed call puts the row back to 'paid' so a later cancel retries.
func (h *Handler) refundPaystackBooking(ctx context.Context, bookingID string) {
	var ref string
	if err := h.db.QueryRowContext(ctx,
		`SELECT paystack_reference FROM bookings WHERE id = ?`, bookingID).Scan(&ref); err != nil || ref == "" {
		return
	}
	claimed, err := h.db.ExecContext(ctx,
		`UPDATE bookings SET payment_status = 'refunding' WHERE id = ? AND payment_status = 'paid'`, bookingID)
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack: claim refund", "error", err, "booking_id", bookingID)
		return
	}
	if n, _ := claimed.RowsAffected(); n == 0 {
		return // not paid (a pending hold has nothing to refund), or already refunding
	}
	h.finishPaystackRefund(ctx, bookingID, ref, "paid")
}

// PaystackReturn handles GET /v1/paystack/return — where Paystack sends the booker after
// checkout (and where its cancel button leads). The query string is typed by whoever holds
// the browser, so it is used only to pick WHICH reference to verify server-side.
func (h *Handler) PaystackReturn(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.URL.Query().Get("reference"))
	if ref == "" {
		ref = strings.TrimSpace(r.URL.Query().Get("trxref"))
	}
	if !strings.HasPrefix(ref, paystack.RefPrefix) || len(ref) > 64 {
		http.Error(w, "Unknown payment reference", http.StatusNotFound)
		return
	}
	b, err := h.loadPaystackBooking(r.Context(), ref)
	if err != nil {
		http.Error(w, "Unknown payment reference", http.StatusNotFound)
		return
	}
	outcome, err := h.settlePaystackReference(r.Context(), ref)
	if err != nil {
		h.logger.WarnContext(r.Context(), "paystack return: could not verify yet", "error", err, "reference", ref)
		outcome = psProcessing // the reconciler finishes it
	}
	q := url.Values{}
	switch outcome {
	case psPaid:
		q.Set("paid", "1")
		q.Set("session_id", ref)
	case psProcessing:
		q.Set("payment", "processing")
	default:
		// Not paid: free the slot now so the booker can pick again straight away. A
		// payment that still lands on this reference later is refunded by the reconciler.
		h.expirePaystackHold(r.Context(), b.ID)
		q.Set("payment", "failed")
	}
	http.Redirect(w, r, h.publicURL()+"/book/"+url.PathEscape(b.Slug)+"?"+q.Encode(), http.StatusSeeOther)
}

// PaystackWebhook handles POST /v1/paystack/webhook. Public; authenticated by the
// x-paystack-signature HMAC-SHA512 of the exact raw body under the secret key. Every
// delivery is logged first (including bad signatures); a valid charge.success for one of
// OUR references (BPS-) only triggers settlePaystackReference, which re-verifies with the
// API. Training payments (BPA-) and anything else on the shared account are ignored.
func (h *Handler) PaystackWebhook(w http.ResponseWriter, r *http.Request) {
	pc := h.getPaystack()
	if pc == nil {
		h.writeError(w, http.StatusServiceUnavailable, "payments not configured")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "could not read body")
		return
	}
	valid := pc.VerifySignature(raw, r.Header.Get("x-paystack-signature"))
	var ev paystack.Event
	_ = json.Unmarshal(raw, &ev)
	ref := ev.TxnReference()

	if _, err := h.db.ExecContext(r.Context(), `
		INSERT INTO paystack_events (event, reference, signature_valid, raw)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (event, reference) WHERE signature_valid = 1 DO NOTHING`,
		ev.Event, ref, boolInt(valid), string(raw)); err != nil {
		// Not logged means not safe to acknowledge: a non-2xx makes Paystack retry.
		h.logger.ErrorContext(r.Context(), "paystack webhook: log", "error", err)
		h.writeError(w, http.StatusServiceUnavailable, "try again")
		return
	}
	if !valid {
		h.logger.WarnContext(r.Context(), "paystack webhook: bad signature", "reference", ref)
		h.writeError(w, http.StatusUnauthorized, "signature verification failed")
		return
	}
	note := func(s string, applied bool) {
		_, _ = h.db.ExecContext(r.Context(), `
			UPDATE paystack_events SET note = ?, applied = ?
			WHERE event = ? AND reference = ? AND signature_valid = 1`, s, boolInt(applied), ev.Event, ref)
	}
	switch {
	case !strings.HasPrefix(ref, paystack.RefPrefix):
		note("not a scheduling reference; ignored", false)
	case paystack.ReferenceMode(ref) != pc.Mode() || ev.Data.Domain != string(pc.Mode()):
		note(fmt.Sprintf("refused: %s reference, %s domain, %s key", paystack.ReferenceMode(ref), ev.Data.Domain, pc.Mode()), false)
	case ev.Event == "charge.success":
		outcome, err := h.settlePaystackReference(r.Context(), ref)
		if errors.Is(err, sql.ErrNoRows) {
			note("no booking with this reference", false)
			break
		}
		if err != nil {
			h.logger.ErrorContext(r.Context(), "paystack webhook: settle", "error", err, "reference", ref)
			h.writeError(w, http.StatusServiceUnavailable, "try again")
			return
		}
		note("settled: "+outcome, outcome == psPaid)
	default:
		note("recorded", false) // refund.* etc: refund state is driven by our own calls
	}
	w.WriteHeader(http.StatusOK)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RunPaystackReconciler settles Paystack holds whose browser never came back and whose
// webhook never arrived (the shared Paystack account's webhook URL belongs to the training
// site), releases holds past the window, refunds late payments, and retries failed
// refunds. Runs until ctx is cancelled.
func (h *Handler) RunPaystackReconciler(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for tick := 0; ; tick++ {
		if h.getPaystack() != nil {
			h.reconcilePaystack(ctx, tick%10 == 0)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (h *Handler) reconcilePaystack(ctx context.Context, includeLate bool) {
	type row struct{ id, ref, ps, created string }
	q := `SELECT id, paystack_reference, payment_status, created_at FROM bookings
	      WHERE payment_provider = 'paystack' AND paystack_reference <> '' AND (
	        (status = 'confirmed' AND payment_status = 'pending') OR payment_status = 'refund_due'`
	args := []any{}
	if includeLate {
		q += ` OR (payment_status = 'expired' AND created_at > ?)`
		args = append(args, time.Now().UTC().Add(-paystackLateWindow).Format(time.RFC3339Nano))
	}
	q += `) ORDER BY created_at LIMIT 100`
	rows, err := h.db.QueryContext(ctx, q, args...)
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack reconcile: query", "error", err)
		return
	}
	var todo []row
	for rows.Next() {
		var x row
		if rows.Scan(&x.id, &x.ref, &x.ps, &x.created) == nil {
			todo = append(todo, x)
		}
	}
	rows.Close() // read fully first: the single DB connection is needed below

	for _, x := range todo {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		switch x.ps {
		case "refund_due":
			if c, err := h.db.ExecContext(cctx,
				`UPDATE bookings SET payment_status = 'refunding' WHERE id = ? AND payment_status = 'refund_due'`, x.id); err == nil {
				if n, _ := c.RowsAffected(); n == 1 {
					h.finishPaystackRefund(cctx, x.id, x.ref, "refund_due")
				}
			}
		default:
			outcome, err := h.settlePaystackReference(cctx, x.ref)
			created, _ := time.Parse(time.RFC3339Nano, x.created)
			expired := !created.IsZero() && time.Since(created) > paystackHoldWindow
			if x.ps == "pending" && expired && (err == nil && outcome != psPaid && outcome != psRefunded && outcome != psRefundDue ||
				err != nil && time.Since(created) > paystackHoldWindow+time.Hour) {
				h.expirePaystackHold(cctx, x.id)
			}
			if err != nil && !errors.Is(err, errPaystackUnverifiable) {
				h.logger.WarnContext(cctx, "paystack reconcile: verify", "error", err, "reference", x.ref)
			}
		}
		cancel()
	}
}
