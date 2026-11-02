package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRefundAmountLabel(t *testing.T) {
	for _, c := range []struct {
		minor    int
		currency string
		want     string
	}{
		{100, "ghs", "GH₵1.00"},
		{180000, "ghs", "GH₵1,800.00"},
		{300000, "GHS", "GH₵3,000.00"},
		{123456789, "ghs", "GH₵1,234,567.89"},
		{15000, "usd", "$150.00"},
	} {
		if got := refundAmountLabel(c.minor, c.currency); got != c.want {
			t.Errorf("refundAmountLabel(%d, %q) = %q; want %q", c.minor, c.currency, got, c.want)
		}
	}
}

func (f *psFixture) bookPage(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/book/"+f.slug, nil)
	req.SetPathValue("slug", f.slug)
	rec := httptest.NewRecorder()
	f.h.BookPage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("book page: %d", rec.Code)
	}
	return rec.Body.String()
}

func (f *psFixture) makeFree(t *testing.T) {
	t.Helper()
	// Price 0 is what makes an event type free; the provider column keeps its value.
	if _, err := f.h.db.Exec(`UPDATE event_types SET price_cents = 0 WHERE slug = ?`, f.slug); err != nil {
		t.Fatal(err)
	}
}

// Paid Paystack pages say where the button goes and carry the Mobile Money note; free
// pages keep "Confirm booking" and show no payment wording at all.
func TestBookPage_paymentWordingOnlyOnPaidPages(t *testing.T) {
	f := newPaystackFixture(t)
	paid := f.bookPage(t)
	for _, want := range []string{
		`id="submit-btn" class="btn-primary">Continue to payment</button>`,
		"Paying with Mobile Money? Approve the payment request in your MoMo app or approval menu.",
		"If payment appears to be processing, please don&#39;t pay again.",
		"const SUBMIT_KEY = PRICE_CENTS > 0 ? 'continue_to_payment' : 'confirm_booking';",
	} {
		if !strings.Contains(paid, want) {
			t.Errorf("paid page missing %q", want)
		}
	}

	f.makeFree(t)
	free := f.bookPage(t)
	if !strings.Contains(free, `id="submit-btn" class="btn-primary">Confirm booking</button>`) {
		t.Error("free page lost its Confirm booking button")
	}
	if strings.Contains(free, `class="payment-note pay-momo-note"`) {
		t.Error("free page shows the Mobile Money note")
	}
}

func (f *psFixture) cancelMail(t *testing.T, id, to string) string {
	t.Helper()
	b, err := f.h.bookingSvc.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	f.h.cancelSideEffects(*b)
	f.mail.mu.Lock()
	defer f.mail.mu.Unlock()
	for _, m := range f.mail.msgs {
		if len(m.To) > 0 && m.To[0] == to && strings.Contains(m.Subject, "ancel") {
			return m.Text + "\n" + m.HTML
		}
	}
	t.Fatalf("no cancellation email to %s", to)
	return ""
}

// Cancelling a paid booking tells the attendee a refund was REQUESTED, in cedis, and
// never that it has arrived. A free booking's cancellation says nothing about money.
func TestCancellationEmail_refundRequestedOnlyForPaidBookings(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.startPaid(t, slotAt(3, 10), "paid@example.com")
	f.fake.set(ref, "success", 225000, "GHS", "test")
	f.returnFrom(t, ref)

	body := f.cancelMail(t, id, "paid@example.com")
	want := "A refund of GH₵2,250.00 has been requested to your original payment method. Processing times depend on your payment provider."
	if strings.Count(body, want) != 2 { // plain-text and HTML parts
		t.Errorf("paid cancellation email: want the refund sentence in both parts; body:\n%s", body)
	}
	if f.fake.refundCount(ref) != 1 {
		t.Errorf("refunds = %d; want 1", f.fake.refundCount(ref))
	}

	f.makeFree(t)
	code, out := f.book(t, slotAt(4, 10), "free@example.com", false)
	if code != http.StatusCreated && code != http.StatusOK {
		t.Fatalf("free booking: %d %v", code, out)
	}
	freeID, _ := out["id"].(string)
	if freeID == "" {
		freeID, _ = out["booking_id"].(string)
	}
	if body := f.cancelMail(t, freeID, "free@example.com"); strings.Contains(strings.ToLower(body), "refund") {
		t.Errorf("free cancellation email mentions a refund:\n%s", body)
	}
}
