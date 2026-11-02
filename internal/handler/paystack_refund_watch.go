package handler

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/calnode/calnode/internal/mailer"
	"github.com/calnode/calnode/internal/paystack"
)

// Refund exception monitoring.
//
// Cancelling a paid Paystack booking requests a refund and marks the booking 'refunded'
// (requested, not settled). Paystack's webhook URL belongs to the training site, so the
// reconciler reads each refund back instead: every refundSweepEvery minutes, for every
// refunded Paystack booking whose refund is not yet processed, it lists the transaction's
// refunds (GET only) and records their status. When a refund ENTERS needs-attention or
// failed, one alert email goes to the admins; the admin banner shows the exception until
// Paystack reports it resolved or an admin acknowledges it.
//
// The monitor never resolves anything itself. A needs-attention refund needs the
// customer's account details (for Mobile Money, the wallet number), which a person enters
// in the Paystack Dashboard; nothing here can retry a refund or send such details.

const (
	refundSweepEvery = 15 // reconciler ticks (minutes) between refund checks
	// A booking whose refunds have all failed (none in flight) is re-read daily, so a
	// retry made in the Paystack Dashboard is noticed without polling it every 15 minutes.
	refundFailedRecheck = 24 * time.Hour
	// paystackRefundsURL is where an admin resolves a refund; the alert names the refund
	// id to search for there.
	paystackRefundsURL = "https://dashboard.paystack.com/#/refunds"
)

func isRefundException(status string) bool {
	return status == "needs-attention" || status == "failed"
}

// paystackRefundReader is all the refund monitor may call. Holding the client only
// through this interface means the monitor cannot request, retry or redirect a refund.
type paystackRefundReader interface {
	Mode() paystack.Mode
	Verify(ctx context.Context, reference string) (*paystack.Transaction, error)
	ListRefunds(ctx context.Context, transactionID int64) ([]paystack.Refund, error)
}

// monitorPaystackRefunds checks unresolved refunds at Paystack, then sends any alert still
// due — including ones whose earlier send failed.
func (h *Handler) monitorPaystackRefunds(ctx context.Context) {
	if pc := h.getPaystack(); pc != nil {
		h.checkPaystackRefunds(ctx, pc)
	}
	h.sendRefundAlerts(ctx)
}

type refundWatchRow struct {
	bookingID, ref string
	txID           int64
	checkedAt      string
	lastErr        string
	allSettled     bool // has refunds, none still in flight (so at least one failed)
}

func (h *Handler) checkPaystackRefunds(ctx context.Context, pr paystackRefundReader) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT b.id, b.paystack_reference, COALESCE(w.transaction_id, 0), COALESCE(w.checked_at, ''),
		       COALESCE(w.last_error, ''),
		       EXISTS (SELECT 1 FROM paystack_refunds r WHERE r.booking_id = b.id) AND NOT EXISTS (
		           SELECT 1 FROM paystack_refunds r WHERE r.booking_id = b.id AND r.status NOT IN ('failed', 'processed'))
		FROM bookings b LEFT JOIN paystack_refund_watch w ON w.booking_id = b.id
		WHERE b.payment_provider = 'paystack' AND b.paystack_reference <> ''
		  AND b.payment_status = 'refunded' AND w.resolved_at IS NULL
		ORDER BY b.created_at`)
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack refunds: query", "error", err)
		return
	}
	var todo []refundWatchRow
	for rows.Next() {
		var x refundWatchRow
		if rows.Scan(&x.bookingID, &x.ref, &x.txID, &x.checkedAt, &x.lastErr, &x.allSettled) == nil {
			todo = append(todo, x)
		}
	}
	rows.Close() // read fully first: the single DB connection is needed below

	for _, x := range todo {
		if paystack.ReferenceMode(x.ref) != pr.Mode() {
			continue // a test booking under a live key (or the reverse) cannot be looked up
		}
		if checked, _ := time.Parse(time.RFC3339Nano, x.checkedAt); x.allSettled && time.Since(checked) < refundFailedRecheck {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := h.checkBookingRefunds(cctx, pr, x)
		cancel()
		h.recordRefundCheck(ctx, x, err)
	}
}

// checkBookingRefunds reads one booking's refunds from Paystack and records them. The anchor
// is the transaction id Paystack returns when verifying this booking's own BPS- reference
// (the same verify used for payments). A refund is accepted only if it carries exactly that
// id; a refund without a readable id is rejected. transaction_reference is not in every
// Paystack response shape, so it is a cross-check: when present it must equal the booking's
// reference. Any refund that fails these checks rejects the whole list and nothing is recorded.
func (h *Handler) checkBookingRefunds(ctx context.Context, pr paystackRefundReader, x refundWatchRow) error {
	txID := x.txID
	if txID == 0 {
		tx, err := pr.Verify(ctx, x.ref)
		if err != nil {
			return fmt.Errorf("look up transaction: %w", err)
		}
		if tx.ID == 0 || tx.Reference != x.ref {
			return fmt.Errorf("transaction lookup returned id %d for reference %q", tx.ID, tx.Reference)
		}
		txID = tx.ID
		if _, err := h.db.ExecContext(ctx, `
			INSERT INTO paystack_refund_watch (booking_id, transaction_id) VALUES (?, ?)
			ON CONFLICT (booking_id) DO UPDATE SET transaction_id = excluded.transaction_id`, x.bookingID, txID); err != nil {
			return fmt.Errorf("store transaction id: %w", err)
		}
	}
	refunds, err := pr.ListRefunds(ctx, txID)
	if err != nil {
		return fmt.Errorf("list refunds: %w", err)
	}
	if len(refunds) == 0 {
		return errors.New("paystack lists no refund for this payment")
	}
	for _, rf := range refunds {
		if rf.Transaction != txID || (rf.TransactionReference != "" && rf.TransactionReference != x.ref) {
			return fmt.Errorf("refund %d is for transaction %d (%q), not %d (%q); nothing recorded",
				rf.ID, rf.Transaction, rf.TransactionReference, txID, x.ref)
		}
	}
	allProcessed := true
	for _, rf := range refunds {
		if err := h.recordRefund(ctx, x.bookingID, txID, x.ref, rf); err != nil {
			return err
		}
		allProcessed = allProcessed && rf.Status == "processed"
	}
	if allProcessed {
		if _, err := h.db.ExecContext(ctx, `
			UPDATE paystack_refund_watch SET resolved_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE booking_id = ?`, x.bookingID); err != nil {
			return fmt.Errorf("mark resolved: %w", err)
		}
		h.logger.InfoContext(ctx, "paystack refunds: processed", "booking_id", x.bookingID, "reference", x.ref)
	}
	return nil
}

// recordRefund stores a refund's latest status. Each entry into needs-attention or failed
// (first sighting included) bumps exception_seq, which is what makes an alert due.
func (h *Handler) recordRefund(ctx context.Context, bookingID string, txID int64, ref string, rf paystack.Refund) error {
	exc := boolInt(isRefundException(rf.Status))
	res, err := h.db.ExecContext(ctx, `
		INSERT INTO paystack_refunds (refund_id, booking_id, transaction_id, transaction_reference,
		    amount_minor, currency, status, reason, exception_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (refund_id) DO NOTHING`,
		rf.ID, bookingID, txID, ref, rf.Amount, strings.ToUpper(rf.Currency), rf.Status, rf.Reason, exc)
	if err != nil {
		return fmt.Errorf("record refund %d: %w", rf.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		h.logger.InfoContext(ctx, "paystack refunds: seen", "refund_id", rf.ID, "booking_id", bookingID, "status", rf.Status)
		return nil
	}
	// SET expressions read the row's previous values, so the CASEs compare against the old status.
	res, err = h.db.ExecContext(ctx, `
		UPDATE paystack_refunds SET
		    exception_seq     = exception_seq + CASE WHEN status <> ? THEN ? ELSE 0 END,
		    status_changed_at = CASE WHEN status <> ? THEN strftime('%Y-%m-%dT%H:%M:%fZ', 'now') ELSE status_changed_at END,
		    status = ?, reason = ?
		WHERE refund_id = ? AND booking_id = ? AND (status <> ? OR reason <> ?)`,
		rf.Status, exc, rf.Status, rf.Status, rf.Reason, rf.ID, bookingID, rf.Status, rf.Reason)
	if err != nil {
		return fmt.Errorf("update refund %d: %w", rf.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 1 {
		h.logger.InfoContext(ctx, "paystack refunds: status", "refund_id", rf.ID, "booking_id", bookingID, "status", rf.Status)
	}
	return nil
}

// recordRefundCheck notes the check and logs a failure only when it differs from the last
// one for this booking, so an outage is one log line, not one every 15 minutes.
func (h *Handler) recordRefundCheck(ctx context.Context, x refundWatchRow, checkErr error) {
	msg := ""
	if checkErr != nil {
		msg = checkErr.Error()
	}
	if _, err := h.db.ExecContext(ctx, `
		INSERT INTO paystack_refund_watch (booking_id, checked_at, last_error)
		VALUES (?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), ?)
		ON CONFLICT (booking_id) DO UPDATE SET checked_at = excluded.checked_at, last_error = excluded.last_error`,
		x.bookingID, msg); err != nil {
		h.logger.ErrorContext(ctx, "paystack refunds: record check", "error", err, "booking_id", x.bookingID)
	}
	switch {
	case msg != "" && msg != x.lastErr:
		h.logger.WarnContext(ctx, "paystack refunds: check failed (logged once until it changes)",
			"error", msg, "booking_id", x.bookingID, "reference", x.ref)
	case msg == "" && x.lastErr != "":
		h.logger.InfoContext(ctx, "paystack refunds: check recovered", "booking_id", x.bookingID)
	}
}

type refundAlert struct {
	refundID                 int64
	bookingID, ref           string
	amount                   int
	currency, status, reason string
	seq                      int
	prevErr                  string
	eventType, name, email   string
}

// sendRefundAlerts emails every alert that is due. A send that fails leaves the alert due
// for the next sweep; the Resend idempotency key (refund id + exception number) stops a
// retry after a lost response from delivering it twice.
func (h *Handler) sendRefundAlerts(ctx context.Context) {
	rows, err := h.db.QueryContext(ctx, `
		SELECT r.refund_id, r.booking_id, r.transaction_reference, r.amount_minor, r.currency, r.status,
		       r.reason, r.exception_seq, r.alert_error, COALESCE(et.name, ''),
		       COALESCE((SELECT name FROM booking_attendees WHERE booking_id = b.id ORDER BY rowid LIMIT 1), ''),
		       COALESCE((SELECT email FROM booking_attendees WHERE booking_id = b.id ORDER BY rowid LIMIT 1), '')
		FROM paystack_refunds r
		JOIN bookings b ON b.id = r.booking_id
		LEFT JOIN event_types et ON et.id = b.event_type_id
		WHERE r.alerted_seq < r.exception_seq AND r.status IN ('needs-attention', 'failed')
		ORDER BY r.status_changed_at`)
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack refunds: alert query", "error", err)
		return
	}
	var due []refundAlert
	for rows.Next() {
		var a refundAlert
		if rows.Scan(&a.refundID, &a.bookingID, &a.ref, &a.amount, &a.currency, &a.status, &a.reason,
			&a.seq, &a.prevErr, &a.eventType, &a.name, &a.email) == nil {
			due = append(due, a)
		}
	}
	rows.Close()
	if len(due) == 0 {
		return
	}

	to := h.refundAlertRecipients(ctx)
	for _, a := range due {
		var sendErr error
		switch {
		case !h.isEmailEnabled():
			sendErr = errors.New("email is not configured")
		case len(to) == 0:
			sendErr = errors.New("no admin email address to alert")
		default:
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			sendErr = h.mailer.Send(cctx, h.refundAlertMessage(a, to))
			cancel()
		}
		if sendErr == nil {
			_, err := h.db.ExecContext(ctx, `
				UPDATE paystack_refunds SET alerted_seq = ?, alert_error = '',
				    alerted_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
				WHERE refund_id = ? AND alerted_seq < ?`, a.seq, a.refundID, a.seq)
			if err != nil {
				h.logger.ErrorContext(ctx, "paystack refunds: mark alerted", "error", err, "refund_id", a.refundID)
			}
			h.logger.InfoContext(ctx, "paystack refunds: alert sent", "refund_id", a.refundID, "status", a.status, "to", len(to))
			continue
		}
		msg := sendErr.Error()
		if _, err := h.db.ExecContext(ctx, `UPDATE paystack_refunds SET alert_error = ? WHERE refund_id = ?`,
			msg, a.refundID); err != nil {
			h.logger.ErrorContext(ctx, "paystack refunds: record alert failure", "error", err, "refund_id", a.refundID)
		}
		if msg != a.prevErr {
			h.logger.ErrorContext(ctx, "paystack refunds: alert not sent; will retry (logged once until it changes)",
				"error", msg, "refund_id", a.refundID, "status", a.status)
		}
	}
}

// refundAlertRecipients is PAYSTACK_REFUND_ALERT_EMAIL (comma-separated) when set,
// otherwise every active admin.
func (h *Handler) refundAlertRecipients(ctx context.Context) []string {
	var to []string
	for _, s := range strings.Split(os.Getenv("PAYSTACK_REFUND_ALERT_EMAIL"), ",") {
		if s = strings.TrimSpace(s); s != "" {
			to = append(to, s)
		}
	}
	if len(to) > 0 {
		return to
	}
	rows, err := h.db.QueryContext(ctx, `
		SELECT email FROM users WHERE (is_admin = 1 OR is_owner = 1) AND archived_at IS NULL
		ORDER BY is_owner DESC, created_at`)
	if err != nil {
		h.logger.ErrorContext(ctx, "paystack refunds: admin query", "error", err)
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var e string
		if rows.Scan(&e) == nil && e != "" {
			to = append(to, e)
		}
	}
	return to
}

func (h *Handler) refundAlertMessage(a refundAlert, to []string) mailer.Message {
	amount := refundAmountLabel(a.amount, a.currency)
	subject := fmt.Sprintf("Action needed: Paystack refund %d needs attention (%s)", a.refundID, amount)
	action := `Paystack needs the account to send this refund to.
  1. Contact the customer for it. For Mobile Money: the network and wallet phone number.
  2. In the Paystack Dashboard, open Refunds, filter "Needs attention", and open refund ` + strconv.FormatInt(a.refundID, 10) + `.
  3. Choose the destination (e.g. MTN Mobile Money), enter the details, and retry.`
	if a.status == "failed" {
		subject = fmt.Sprintf("Action needed: Paystack refund %d failed (%s)", a.refundID, amount)
		action = `Paystack could not complete this refund.
  1. In the Paystack Dashboard, open Refunds and open refund ` + strconv.FormatInt(a.refundID, 10) + ` to see why.
  2. Retry it there, or refund the customer another way and tell them.
  3. Once it is dealt with, acknowledge it in the Bulletproof Scheduling admin.`
	}
	reason := a.reason
	if reason == "" {
		reason = "(none given)"
	}
	customer := a.name
	if a.email != "" {
		customer = strings.TrimSpace(customer + " <" + a.email + ">")
	}
	text := fmt.Sprintf(`Paystack reports a refund that needs manual action.

Refund ID:         %d
Booking reference: %s
Payment reference: %s
Appointment:       %s
Customer:          %s
Amount:            %s
Paystack status:   %s
Reason:            %s
Paystack:          %s (search for refund %d)

Action required:
%s

Bulletproof Scheduling never retries a refund or sends customer account details itself.
It keeps checking: the admin banner clears once Paystack reports the refund processed,
and you will be emailed again if it returns to needs-attention or fails.
Admin: %s/admin/
`, a.refundID, a.bookingID, a.ref, a.eventType, customer, amount, a.status, reason,
		paystackRefundsURL, a.refundID, action, strings.TrimRight(h.baseURL, "/"))
	return mailer.Message{
		To:             to,
		Subject:        subject,
		Text:           text,
		IdempotencyKey: fmt.Sprintf("bps-refund-%d-%d", a.refundID, a.seq),
	}
}

// RefundException is an unresolved refund the admin banner shows.
type RefundException struct {
	RefundID        int64  `json:"refund_id"`
	BookingID       string `json:"booking_id"`
	Reference       string `json:"reference"`
	Amount          string `json:"amount"`
	Status          string `json:"status"`
	Reason          string `json:"reason"`
	StatusChangedAt string `json:"status_changed_at"`
	AlertedAt       string `json:"alerted_at,omitempty"`
	AlertError      string `json:"alert_error,omitempty"`
}

// RefundCheckError is a refunded booking the monitor could not check.
type RefundCheckError struct {
	BookingID string `json:"booking_id"`
	Reference string `json:"reference"`
	Error     string `json:"error"`
	CheckedAt string `json:"checked_at"`
}

// GetRefundExceptions lists unresolved refund exceptions (needs-attention or failed, not
// acknowledged) and refunded bookings the monitor could not check. Admin only; drives the
// admin banner, which works whether or not the alert email was sent.
func (h *Handler) GetRefundExceptions(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	out := struct {
		Exceptions  []RefundException  `json:"exceptions"`
		CheckErrors []RefundCheckError `json:"check_errors"`
	}{Exceptions: []RefundException{}, CheckErrors: []RefundCheckError{}}

	rows, err := h.db.QueryContext(r.Context(), `
		SELECT refund_id, booking_id, transaction_reference, amount_minor, currency, status, reason,
		       status_changed_at, COALESCE(alerted_at, ''), alert_error
		FROM paystack_refunds
		WHERE status IN ('needs-attention', 'failed') AND acknowledged_seq < exception_seq
		ORDER BY status_changed_at`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "refund exceptions: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for rows.Next() {
		var e RefundException
		var amount int
		var currency, alertedAt string
		if rows.Scan(&e.RefundID, &e.BookingID, &e.Reference, &amount, &currency, &e.Status, &e.Reason,
			&e.StatusChangedAt, &alertedAt, &e.AlertError) == nil {
			e.Amount = refundAmountLabel(amount, currency)
			if e.AlertError == "" {
				e.AlertedAt = alertedAt
			}
			out.Exceptions = append(out.Exceptions, e)
		}
	}
	rows.Close()

	rows, err = h.db.QueryContext(r.Context(), `
		SELECT w.booking_id, b.paystack_reference, w.last_error, COALESCE(w.checked_at, '')
		FROM paystack_refund_watch w JOIN bookings b ON b.id = w.booking_id
		WHERE w.resolved_at IS NULL AND w.last_error <> ''
		ORDER BY w.checked_at`)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "refund exceptions: check errors", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	for rows.Next() {
		var e RefundCheckError
		if rows.Scan(&e.BookingID, &e.Reference, &e.Error, &e.CheckedAt) == nil {
			out.CheckErrors = append(out.CheckErrors, e)
		}
	}
	rows.Close()
	h.writeJSON(w, http.StatusOK, out)
}

// AcknowledgeRefundException hides one exception from the admin banner — for a refund the
// admin has resolved outside Paystack. Monitoring continues: if the refund changes status
// and later re-enters needs-attention or failed, it alerts and shows again. Admin only.
func (h *Handler) AcknowledgeRefundException(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid refund id")
		return
	}
	res, err := h.db.ExecContext(r.Context(),
		`UPDATE paystack_refunds SET acknowledged_seq = exception_seq WHERE refund_id = ?`, id)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "refund exceptions: acknowledge", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		h.writeError(w, http.StatusNotFound, "refund not found")
		return
	}
	h.logger.InfoContext(r.Context(), "paystack refunds: acknowledged", "refund_id", id)
	w.WriteHeader(http.StatusNoContent)
}
