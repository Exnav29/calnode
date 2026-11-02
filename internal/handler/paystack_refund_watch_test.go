package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/calnode/calnode/internal/mailer"
)

const rwTxID = 5550001

// refundedBooking makes a paid Paystack booking, cancels it (refund requested), and gives
// its transaction a Paystack id, as a real verify response would.
func (f *psFixture) refundedBooking(t *testing.T) (id, ref string) {
	t.Helper()
	id, ref = f.startPaid(t, slotAt(3, 10), "payer@example.com")
	f.fake.set(ref, "success", 225000, "GHS", "test")
	f.returnFrom(t, ref)
	f.h.refundBookingPayment(context.Background(), id)
	f.fake.mu.Lock()
	f.fake.txns[ref].ID = rwTxID
	f.fake.mu.Unlock()
	var ps string
	if err := f.h.db.QueryRow(`SELECT payment_status FROM bookings WHERE id = ?`, id).Scan(&ps); err != nil || ps != "refunded" {
		t.Fatalf("payment_status = %q, %v; want refunded", ps, err)
	}
	return id, ref
}

// setRefund makes Paystack report refund rid on the booking's transaction with status.
func (f *psFixture) setRefund(ref string, rid int64, status, reason string) {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	if f.fake.refundList == nil {
		f.fake.refundList = map[int64][]map[string]any{}
	}
	rec := map[string]any{"id": rid, "transaction": rwTxID, "transaction_reference": ref, "domain": "test",
		"amount": 225000, "currency": "GHS", "status": status, "reason": reason}
	list := f.fake.refundList[rwTxID]
	for i := range list {
		if list[i]["id"] == rid {
			list[i] = rec
			return
		}
	}
	f.fake.refundList[rwTxID] = append(list, rec)
}

func (f *psFixture) alerts() []mailer.Message {
	f.mail.mu.Lock()
	defer f.mail.mu.Unlock()
	var out []mailer.Message
	for _, m := range f.mail.msgs {
		if strings.HasPrefix(m.Subject, "Action needed: Paystack refund") {
			out = append(out, m)
		}
	}
	return out
}

type refundExceptionsBody struct {
	Exceptions  []RefundException  `json:"exceptions"`
	CheckErrors []RefundCheckError `json:"check_errors"`
}

func (f *psFixture) exceptions(t *testing.T) refundExceptionsBody {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/paystack/refund-exceptions", nil)
	req.Header.Set("X-API-Key", f.key)
	rec := httptest.NewRecorder()
	f.h.RequireAuth(f.h.GetRefundExceptions)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("refund exceptions: %d %s", rec.Code, rec.Body.String())
	}
	var out refundExceptionsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (f *psFixture) callsSince(n int) []string {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	return append([]string(nil), f.fake.calls[n:]...)
}

func (f *psFixture) callCount() int {
	f.fake.mu.Lock()
	defer f.fake.mu.Unlock()
	return len(f.fake.calls)
}

// captureLogs routes the handler's logger into a buffer.
func captureLogs(h *Handler) *bytes.Buffer {
	var buf bytes.Buffer
	var mu sync.Mutex
	h.logger = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}), nil))
	return &buf
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

// One email when a refund enters needs-attention — never again for the same occurrence —
// carrying everything the admin needs to resolve it.
func TestRefundMonitor_alertsOnceOnNeedsAttention(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.refundedBooking(t)
	ctx := context.Background()

	f.setRefund(ref, 18949128, "pending", "")
	f.h.monitorPaystackRefunds(ctx)
	if n := len(f.alerts()); n != 0 {
		t.Fatalf("pending refund alerted %d times", n)
	}

	f.setRefund(ref, 18949128, "needs-attention", "NEEDS-ATTENTION")
	for i := 0; i < 4; i++ {
		f.h.monitorPaystackRefunds(ctx)
	}
	got := f.alerts()
	if len(got) != 1 {
		t.Fatalf("alerts = %d; want exactly 1", len(got))
	}
	m := got[0]
	if len(m.To) != 1 || m.To[0] != "host@example.com" {
		t.Errorf("To = %v; want the admin", m.To)
	}
	if m.IdempotencyKey != "bps-refund-18949128-1" {
		t.Errorf("IdempotencyKey = %q", m.IdempotencyKey)
	}
	for _, want := range []string{
		"Refund ID:         18949128",
		"Booking reference: " + id,
		"Payment reference: " + ref,
		"Amount:            GH₵2,250.00",
		"Paystack status:   needs-attention",
		"Reason:            NEEDS-ATTENTION",
		"https://dashboard.paystack.com/#/refunds",
		"Mobile Money: the network and wallet phone number",
		"filter \"Needs attention\"",
		"never retries a refund or sends customer account details",
	} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("alert missing %q:\n%s", want, m.Text)
		}
	}
	if !strings.Contains(m.Subject, "18949128 needs attention") {
		t.Errorf("Subject = %q", m.Subject)
	}

	ex := f.exceptions(t)
	if len(ex.Exceptions) != 1 || ex.Exceptions[0].RefundID != 18949128 || ex.Exceptions[0].AlertedAt == "" {
		t.Errorf("banner = %+v; want the alerted exception", ex.Exceptions)
	}
}

// needs-attention -> processing -> needs-attention is two occurrences: two alerts. Once
// processed the booking is resolved, the banner clears and Paystack is not asked again.
func TestRefundMonitor_reAlertsOnReentryAndStopsWhenProcessed(t *testing.T) {
	f := newPaystackFixture(t)
	_, ref := f.refundedBooking(t)
	ctx := context.Background()

	for _, st := range []string{"needs-attention", "processing", "processing", "needs-attention", "needs-attention"} {
		f.setRefund(ref, 77, st, strings.ToUpper(st))
		f.h.monitorPaystackRefunds(ctx)
	}
	got := f.alerts()
	if len(got) != 2 {
		t.Fatalf("alerts = %d; want 2 (one per entry into needs-attention)", len(got))
	}
	if got[1].IdempotencyKey != "bps-refund-77-2" {
		t.Errorf("second alert key = %q; want bps-refund-77-2", got[1].IdempotencyKey)
	}

	f.setRefund(ref, 77, "processed", "PROCESSED")
	f.h.monitorPaystackRefunds(ctx)
	if ex := f.exceptions(t); len(ex.Exceptions) != 0 {
		t.Errorf("banner after processed = %+v; want empty", ex.Exceptions)
	}
	n := f.callCount()
	f.h.monitorPaystackRefunds(ctx)
	if calls := f.callsSince(n); len(calls) != 0 {
		t.Errorf("resolved refund still polled: %v", calls)
	}
	if len(f.alerts()) != 2 {
		t.Errorf("processed refund alerted")
	}
}

// flakyMailer fails while down is set.
type flakyMailer struct {
	captureMailer
	down bool
}

func (m *flakyMailer) Send(ctx context.Context, msg mailer.Message) error {
	if m.down {
		return errors.New("mailer: resend api 503: service unavailable")
	}
	return m.captureMailer.Send(ctx, msg)
}

// A send that fails stays due and is retried each sweep, logging once, not every sweep;
// the banner shows the exception and the delivery error meanwhile.
func TestRefundMonitor_failedSendIsRetriedAndLoggedOnce(t *testing.T) {
	f := newPaystackFixture(t)
	fm := &flakyMailer{down: true}
	f.h.SetMailer(fm, "https://book.example.com")
	logs := captureLogs(f.h)
	_, ref := f.refundedBooking(t)
	ctx := context.Background()

	f.setRefund(ref, 91, "failed", "FAILED")
	for i := 0; i < 3; i++ {
		f.h.monitorPaystackRefunds(ctx)
	}
	if n := strings.Count(logs.String(), "alert not sent"); n != 1 {
		t.Errorf("alert failure logged %d times over 3 sweeps; want 1\n%s", n, logs.String())
	}
	ex := f.exceptions(t)
	if len(ex.Exceptions) != 1 || !strings.Contains(ex.Exceptions[0].AlertError, "503") || ex.Exceptions[0].AlertedAt != "" {
		t.Fatalf("banner = %+v; want the exception with its delivery error", ex.Exceptions)
	}

	fm.down = false
	f.h.monitorPaystackRefunds(ctx) // failed-only booking is not re-read for a day, but the alert still goes
	f.h.monitorPaystackRefunds(ctx)
	fm.mu.Lock()
	sent := len(fm.msgs)
	var key, subject string
	if sent > 0 {
		key, subject = fm.msgs[0].IdempotencyKey, fm.msgs[0].Subject
	}
	fm.mu.Unlock()
	if sent != 1 || key != "bps-refund-91-1" || !strings.Contains(subject, "91 failed") {
		t.Fatalf("after recovery: sent=%d key=%q subject=%q; want one failed-refund alert", sent, key, subject)
	}
	if ex := f.exceptions(t); len(ex.Exceptions) != 1 || ex.Exceptions[0].AlertError != "" || ex.Exceptions[0].AlertedAt == "" {
		t.Errorf("banner after send = %+v; failed refunds stay listed until acknowledged", ex.Exceptions)
	}
}

// With no email configured the exception is still recorded and shown, and the alert goes
// out once email works.
func TestRefundMonitor_noEmailStillShowsException(t *testing.T) {
	f := newPaystackFixture(t)
	f.h.SetMailer(&mailer.Noop{}, "https://book.example.com")
	_, ref := f.refundedBooking(t)
	f.setRefund(ref, 12, "needs-attention", "NEEDS-ATTENTION")
	f.h.monitorPaystackRefunds(context.Background())

	ex := f.exceptions(t)
	if len(ex.Exceptions) != 1 || ex.Exceptions[0].AlertError != "email is not configured" {
		t.Fatalf("banner = %+v", ex.Exceptions)
	}
	f.h.SetMailer(f.mail, "https://book.example.com")
	f.h.monitorPaystackRefunds(context.Background())
	if n := len(f.alerts()); n != 1 {
		t.Errorf("alerts after email configured = %d; want 1", n)
	}
}

// The monitor only reads from Paystack: no refund request, retry, or account details.
func TestRefundMonitor_readOnly(t *testing.T) {
	f := newPaystackFixture(t)
	_, ref := f.refundedBooking(t)
	refunds := f.fake.refundCount(ref)
	n := f.callCount()
	for _, st := range []string{"pending", "needs-attention", "failed", "processing", "processed"} {
		f.setRefund(ref, 5, st, "")
		f.h.monitorPaystackRefunds(context.Background())
	}
	calls := f.callsSince(n)
	if len(calls) == 0 {
		t.Fatal("monitor made no Paystack calls")
	}
	for _, c := range calls {
		if !strings.HasPrefix(c, "GET ") {
			t.Errorf("monitor made a non-GET Paystack call: %s", c)
		}
	}
	if f.fake.refundCount(ref) != refunds {
		t.Errorf("monitor requested a refund")
	}
}

// A refund is recorded only when Paystack ties it to the booking's own transaction id AND
// reference; anything else is reported as a check error and nothing is stored or emailed.
func TestRefundMonitor_matchesByTransactionIDAndReference(t *testing.T) {
	f := newPaystackFixture(t)
	_, ref := f.refundedBooking(t)
	f.setRefund(ref, 31, "needs-attention", "NEEDS-ATTENTION")
	f.fake.mu.Lock()
	f.fake.refundList[rwTxID][0]["transaction_reference"] = "BPS-TEST-SOMEONE-ELSE"
	f.fake.mu.Unlock()

	f.h.monitorPaystackRefunds(context.Background())
	var n int
	_ = f.h.db.QueryRow(`SELECT COUNT(*) FROM paystack_refunds`).Scan(&n)
	if n != 0 || len(f.alerts()) != 0 {
		t.Fatalf("mismatched refund recorded (%d rows, %d alerts)", n, len(f.alerts()))
	}
	ex := f.exceptions(t)
	if len(ex.CheckErrors) != 1 || !strings.Contains(ex.CheckErrors[0].Error, "nothing recorded") {
		t.Errorf("check errors = %+v", ex.CheckErrors)
	}
}

// A Paystack outage changes nothing, is logged once across sweeps, and is visible.
func TestRefundMonitor_paystackOutage(t *testing.T) {
	f := newPaystackFixture(t)
	logs := captureLogs(f.h)
	_, ref := f.refundedBooking(t)
	f.setRefund(ref, 44, "needs-attention", "NEEDS-ATTENTION")
	f.fake.mu.Lock()
	f.fake.failAPI = true
	f.fake.mu.Unlock()
	for i := 0; i < 3; i++ {
		f.h.monitorPaystackRefunds(context.Background())
	}
	if n := strings.Count(logs.String(), "check failed"); n != 1 {
		t.Errorf("outage logged %d times; want 1", n)
	}
	if len(f.alerts()) != 0 {
		t.Error("alert sent without a confirmed status")
	}
	if ex := f.exceptions(t); len(ex.CheckErrors) != 1 {
		t.Errorf("check errors = %+v; want the outage", ex.CheckErrors)
	}

	f.fake.mu.Lock()
	f.fake.failAPI = false
	f.fake.mu.Unlock()
	f.h.monitorPaystackRefunds(context.Background())
	if len(f.alerts()) != 1 {
		t.Errorf("alerts after recovery = %d; want 1", len(f.alerts()))
	}
	if ex := f.exceptions(t); len(ex.CheckErrors) != 0 {
		t.Errorf("check errors after recovery = %+v", ex.CheckErrors)
	}
}

// A failed refund stays in the banner until acknowledged; it is re-read daily, not every
// sweep; acknowledging hides it, and a later re-entry shows and alerts again.
func TestRefundMonitor_failedRefundRetainedUntilAcknowledged(t *testing.T) {
	f := newPaystackFixture(t)
	_, ref := f.refundedBooking(t)
	ctx := context.Background()
	f.setRefund(ref, 66, "failed", "FAILED")
	f.h.monitorPaystackRefunds(ctx)

	n := f.callCount()
	f.h.monitorPaystackRefunds(ctx)
	if calls := f.callsSince(n); len(calls) != 0 {
		t.Errorf("failed-only refund re-read within a day: %v", calls)
	}
	if len(f.exceptions(t).Exceptions) != 1 {
		t.Fatal("failed refund not shown")
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/paystack/refund-exceptions/66/acknowledge", nil)
	req.SetPathValue("id", strconv.Itoa(66))
	req.Header.Set("X-API-Key", f.key)
	rec := httptest.NewRecorder()
	f.h.RequireAuth(f.h.AcknowledgeRefundException)(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("acknowledge: %d %s", rec.Code, rec.Body.String())
	}
	if ex := f.exceptions(t); len(ex.Exceptions) != 0 {
		t.Errorf("acknowledged refund still shown: %+v", ex.Exceptions)
	}

	// A dashboard retry that fails again is a new occurrence. Force the daily re-read.
	_, _ = f.h.db.Exec(`UPDATE paystack_refund_watch SET checked_at = '2000-01-01T00:00:00Z'`)
	f.setRefund(ref, 66, "processing", "")
	f.h.monitorPaystackRefunds(ctx)
	f.setRefund(ref, 66, "failed", "FAILED")
	f.h.monitorPaystackRefunds(ctx)
	if len(f.exceptions(t).Exceptions) != 1 || len(f.alerts()) != 2 {
		t.Errorf("re-failed refund: banner=%d alerts=%d; want 1 and 2", len(f.exceptions(t).Exceptions), len(f.alerts()))
	}
}
