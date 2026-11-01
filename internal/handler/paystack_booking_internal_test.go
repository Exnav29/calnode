package handler

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calnode/calnode/internal/db"
	"github.com/calnode/calnode/internal/i18n"
	"github.com/calnode/calnode/internal/paystack"
)

const psTestKey = "sk_test_fake_bulletproof"

// fakePaystack is a stand-in for api.paystack.co holding per-reference transaction state.
type fakePaystack struct {
	mu       sync.Mutex
	txns     map[string]*paystack.Transaction
	refunds  map[string]int
	verifies int
	failAPI  bool
}

func (f *fakePaystack) set(ref, status string, amount int64, currency, domain string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.txns[ref] = &paystack.Transaction{Reference: ref, Status: status, Amount: amount, Currency: currency, Domain: domain}
}

func (f *fakePaystack) refundCount(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refunds[ref]
}

func (f *fakePaystack) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer "+psTestKey {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"status":false,"message":"Invalid key"}`)
		return
	}
	if f.failAPI {
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"status":false,"message":"upstream down"}`)
		return
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/transaction/initialize":
		var in struct {
			Amount    int64  `json:"amount"`
			Currency  string `json:"currency"`
			Reference string `json:"reference"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.txns[in.Reference] = &paystack.Transaction{Reference: in.Reference, Status: "abandoned", Amount: in.Amount, Currency: in.Currency, Domain: "test"}
		fmt.Fprintf(w, `{"status":true,"data":{"authorization_url":"https://checkout.paystack.com/fake/%s","reference":%q}}`, in.Reference, in.Reference)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/transaction/verify/"):
		f.verifies++
		ref := strings.TrimPrefix(r.URL.Path, "/transaction/verify/")
		t, ok := f.txns[ref]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"status":false,"message":"Transaction reference not found"}`)
			return
		}
		b, _ := json.Marshal(t)
		fmt.Fprintf(w, `{"status":true,"data":%s}`, b)
	case r.Method == http.MethodPost && r.URL.Path == "/refund":
		var in struct {
			Transaction string `json:"transaction"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.refunds[in.Transaction]++
		fmt.Fprint(w, `{"status":true,"data":{"status":"pending"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"status":false,"message":"no route"}`)
	}
}

type psFixture struct {
	h    *Handler
	fake *fakePaystack
	mail *captureMailer
	slug string
	key  string
}

func newPaystackFixture(t *testing.T) *psFixture {
	t.Helper()
	database, err := db.Open("sqlite://:memory:")
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := db.Migrate(database); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	h := New(database, slog.Default())
	mail := &captureMailer{}
	h.SetMailer(mail, "https://book.example.com")

	rec := httptest.NewRecorder()
	h.Setup(rec, httptest.NewRequest(http.MethodPost, "/v1/setup",
		strings.NewReader(`{"name":"Host","email":"host@example.com","timezone":"UTC"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup: %d %s", rec.Code, rec.Body.String())
	}
	var setup struct {
		APIKey string `json:"api_key"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &setup)

	fake := &fakePaystack{txns: map[string]*paystack.Transaction{}, refunds: map[string]int{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	pc, err := paystack.New(psTestKey, "pk_test_fake")
	if err != nil {
		t.Fatal(err)
	}
	pc.SetAPIBase(srv.URL)
	h.SetPaystack(pc)

	f := &psFixture{h: h, fake: fake, mail: mail, slug: "consult", key: setup.APIKey}
	f.api(t, h.CreateEventType, "/v1/event-types",
		`{"slug":"consult","name":"Consultation","duration_minutes":30,"max_active_bookings":0,"max_future_days":0}`, http.StatusCreated)
	for d := 0; d < 7; d++ {
		f.api(t, h.CreateAvailabilityRule, "/v1/availability-rules",
			fmt.Sprintf(`{"day_of_week":%d,"start_time":"00:00","end_time":"23:59"}`, d), http.StatusCreated)
	}
	// $150 listed, charged as GHS 2,250.00 through Paystack.
	if _, err := database.Exec(`UPDATE event_types SET price_cents = 15000, currency = 'usd',
		payment_provider = 'paystack', paystack_amount_minor = 225000, paystack_currency = 'GHS' WHERE slug = 'consult'`); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *psFixture) api(t *testing.T, fn http.HandlerFunc, path, body string, want int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", f.key)
	rec := httptest.NewRecorder()
	f.h.RequireAuth(fn)(rec, req)
	if rec.Code != want {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
}

func slotAt(daysAhead, hour int) string {
	d := time.Now().UTC().AddDate(0, 0, daysAhead)
	return time.Date(d.Year(), d.Month(), d.Day(), hour, 0, 0, 0, time.UTC).Format(time.RFC3339)
}

// book posts to CreateBooking and returns the status and decoded body.
func (f *psFixture) book(t *testing.T, start, email string, optIn bool) (int, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"event_type_slug":%q,"start_at":%q,"name":"Booker","email":%q,"timezone":"UTC","paystack_test":%t}`,
		f.slug, start, email, optIn)
	req := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.CreateBooking(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// startPaid creates a Paystack hold and returns its booking id and reference.
func (f *psFixture) startPaid(t *testing.T, start, email string) (string, string) {
	t.Helper()
	code, out := f.book(t, start, email, true)
	if code != http.StatusOK || out["checkout_url"] == nil {
		t.Fatalf("paid booking: %d %v", code, out)
	}
	id := out["booking_id"].(string)
	var ref string
	if err := f.h.db.QueryRow(`SELECT paystack_reference FROM bookings WHERE id = ?`, id).Scan(&ref); err != nil || ref == "" {
		t.Fatalf("reference not stored: %v", err)
	}
	return id, ref
}

func (f *psFixture) state(t *testing.T, id string) (status, payment string) {
	t.Helper()
	if err := f.h.db.QueryRow(`SELECT status, payment_status FROM bookings WHERE id = ?`, id).Scan(&status, &payment); err != nil {
		t.Fatal(err)
	}
	return
}

func (f *psFixture) returnFrom(t *testing.T, ref string) string {
	t.Helper()
	rec := httptest.NewRecorder()
	f.h.PaystackReturn(rec, httptest.NewRequest(http.MethodGet, "/v1/paystack/return?trxref="+ref+"&reference="+ref, nil))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("return: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Header().Get("Location")
}

func (f *psFixture) webhook(t *testing.T, body string, key string) int {
	t.Helper()
	m := hmac.New(sha512.New, []byte(key))
	m.Write([]byte(body))
	req := httptest.NewRequest(http.MethodPost, "/v1/paystack/webhook", strings.NewReader(body))
	req.Header.Set("x-paystack-signature", hex.EncodeToString(m.Sum(nil)))
	rec := httptest.NewRecorder()
	f.h.PaystackWebhook(rec, req)
	return rec.Code
}

// mailsTo counts captured messages addressed to `to`.
func (f *psFixture) mailsTo(to string) int {
	f.mail.mu.Lock()
	defer f.mail.mu.Unlock()
	n := 0
	for _, m := range f.mail.msgs {
		if len(m.To) > 0 && m.To[0] == to {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Success: the return URL, then duplicate webhooks and a reconciler pass, confirm the
// booking exactly once and send exactly one confirmation to the booker.
func TestPaystack_successConfirmsExactlyOnce(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.startPaid(t, slotAt(3, 10), "payer@example.com")

	if st, ps := f.state(t, id); st != "confirmed" || ps != "pending" {
		t.Fatalf("hold = %s/%s; want confirmed/pending", st, ps)
	}
	// Expected charge is the server-side Paystack amount, not the listed USD price.
	var amt int64
	var cur string
	f.h.db.QueryRow(`SELECT paystack_expected_amount, paystack_expected_currency FROM bookings WHERE id=?`, id).Scan(&amt, &cur)
	if amt != 225000 || cur != "GHS" {
		t.Fatalf("expected charge = %d %s; want 225000 GHS", amt, cur)
	}
	if f.mailsTo("payer@example.com") != 0 {
		t.Fatal("confirmation sent before payment")
	}

	f.fake.set(ref, "success", 225000, "GHS", "test")
	loc := f.returnFrom(t, ref)
	if !strings.Contains(loc, "/book/consult?") || !strings.Contains(loc, "paid=1") {
		t.Fatalf("return redirect = %q; want paid=1", loc)
	}
	ev := fmt.Sprintf(`{"event":"charge.success","data":{"reference":%q,"domain":"test","status":"success","amount":225000,"currency":"GHS"}}`, ref)
	for i := 0; i < 3; i++ {
		if code := f.webhook(t, ev, psTestKey); code != http.StatusOK {
			t.Fatalf("webhook delivery %d: %d", i, code)
		}
	}
	f.h.reconcilePaystack(context.Background(), true)

	if st, ps := f.state(t, id); st != "confirmed" || ps != "paid" {
		t.Fatalf("after payment = %s/%s; want confirmed/paid", st, ps)
	}
	waitFor(t, "booker confirmation", func() bool { return f.mailsTo("payer@example.com") >= 1 })
	time.Sleep(200 * time.Millisecond) // let any duplicate dispatch surface
	if n := f.mailsTo("payer@example.com"); n != 1 {
		t.Fatalf("booker got %d confirmations; want exactly 1", n)
	}
	var amountPaid int
	f.h.db.QueryRow(`SELECT amount_paid_cents FROM bookings WHERE id=?`, id).Scan(&amountPaid)
	if amountPaid != 225000 {
		t.Fatalf("amount_paid = %d; want 225000", amountPaid)
	}
	var logged int
	f.h.db.QueryRow(`SELECT COUNT(*) FROM paystack_events WHERE reference=? AND signature_valid=1`, ref).Scan(&logged)
	if logged != 1 {
		t.Fatalf("duplicate deliveries logged as %d rows; want 1 (deduped)", logged)
	}
}

// A test key without the explicit ?paystack=test opt-in never reaches checkout and never
// holds a slot.
func TestPaystack_testKeyRequiresOptIn(t *testing.T) {
	f := newPaystackFixture(t)
	code, _ := f.book(t, slotAt(3, 11), "visitor@example.com", false)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("booking without opt-in: %d; want 503", code)
	}
	var n int
	f.h.db.QueryRow(`SELECT COUNT(*) FROM bookings`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d bookings created; want none", n)
	}
}

// A failed payment returns the booker to the page, releases the slot at once, and never
// confirms; the same slot can be booked again.
func TestPaystack_failedPaymentReleasesSlot(t *testing.T) {
	f := newPaystackFixture(t)
	start := slotAt(4, 10)
	id, ref := f.startPaid(t, start, "declined@example.com")
	f.fake.set(ref, "failed", 225000, "GHS", "test")

	if loc := f.returnFrom(t, ref); !strings.Contains(loc, "payment=failed") {
		t.Fatalf("redirect = %q; want payment=failed", loc)
	}
	if st, ps := f.state(t, id); st != "cancelled" || ps != "expired" {
		t.Fatalf("after failure = %s/%s; want cancelled/expired", st, ps)
	}
	if code, _ := f.book(t, start, "next@example.com", true); code != http.StatusOK {
		t.Fatalf("rebooking released slot: %d; want 200", code)
	}
	time.Sleep(100 * time.Millisecond)
	if f.mailsTo("declined@example.com") != 0 {
		t.Fatal("confirmation sent for a failed payment")
	}
}

// Abandoned checkout (browser never returns): the reconciler keeps the hold inside the
// window and releases it after.
func TestPaystack_abandonedCheckoutExpires(t *testing.T) {
	f := newPaystackFixture(t)
	id, _ := f.startPaid(t, slotAt(5, 10), "gone@example.com")

	f.h.reconcilePaystack(context.Background(), false)
	if st, ps := f.state(t, id); st != "confirmed" || ps != "pending" {
		t.Fatalf("inside window = %s/%s; want hold kept", st, ps)
	}
	old := time.Now().UTC().Add(-paystackHoldWindow - time.Minute).Format(time.RFC3339Nano)
	f.h.db.Exec(`UPDATE bookings SET created_at = ? WHERE id = ?`, old, id)
	f.h.reconcilePaystack(context.Background(), false)
	if st, ps := f.state(t, id); st != "cancelled" || ps != "expired" {
		t.Fatalf("after window = %s/%s; want cancelled/expired", st, ps)
	}
}

// A missed webhook and no browser return: the reconciler alone confirms a paid hold.
func TestPaystack_reconcilerConfirmsMissedWebhook(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.startPaid(t, slotAt(5, 12), "quiet@example.com")
	f.fake.set(ref, "success", 225000, "GHS", "test")
	f.h.reconcilePaystack(context.Background(), false)
	if _, ps := f.state(t, id); ps != "paid" {
		t.Fatalf("payment_status = %s; want paid", ps)
	}
	waitFor(t, "confirmation", func() bool { return f.mailsTo("quiet@example.com") == 1 })
}

// A payment completed after the hold was released is refunded, never confirmed.
func TestPaystack_latePaymentRefunded(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.startPaid(t, slotAt(6, 10), "late@example.com")
	f.h.expirePaystackHold(context.Background(), id)

	f.fake.set(ref, "success", 225000, "GHS", "test")
	f.h.reconcilePaystack(context.Background(), true)
	if st, ps := f.state(t, id); st != "cancelled" || ps != "refunded" {
		t.Fatalf("late payment = %s/%s; want cancelled/refunded", st, ps)
	}
	if f.fake.refundCount(ref) != 1 {
		t.Fatalf("refund calls = %d; want 1", f.fake.refundCount(ref))
	}
	f.h.reconcilePaystack(context.Background(), true)
	if f.fake.refundCount(ref) != 1 {
		t.Fatal("late payment refunded twice")
	}
	time.Sleep(100 * time.Millisecond)
	if f.mailsTo("late@example.com") != 0 {
		t.Fatal("late payment produced a confirmation")
	}
}

// Wrong amount, wrong currency or wrong environment never confirms; the charge is refunded.
func TestPaystack_mismatchedChargeNeverConfirms(t *testing.T) {
	cases := []struct {
		name, currency, domain string
		amount                 int64
	}{
		{"short amount", "GHS", "test", 15000},
		{"USD treated as GHS", "USD", "test", 225000},
		{"live charge on test booking", "GHS", "live", 225000},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newPaystackFixture(t)
			id, ref := f.startPaid(t, slotAt(7+i, 10), "mm@example.com")
			f.fake.set(ref, "success", c.amount, c.currency, c.domain)
			f.returnFrom(t, ref)
			if st, ps := f.state(t, id); st != "cancelled" || ps != "refunded" {
				t.Fatalf("= %s/%s; want cancelled/refunded", st, ps)
			}
			if f.fake.refundCount(ref) != 1 {
				t.Fatalf("refunds = %d; want 1", f.fake.refundCount(ref))
			}
		})
	}
}

// Webhooks: a bad signature is rejected and logged; a signed training-site (BPA-) event is
// acknowledged and ignored without touching any booking or calling Paystack.
func TestPaystack_webhookSignatureAndForeignReferences(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.startPaid(t, slotAt(3, 15), "sig@example.com")
	f.fake.set(ref, "success", 225000, "GHS", "test")

	forged := fmt.Sprintf(`{"event":"charge.success","data":{"reference":%q,"domain":"test"}}`, ref)
	if code := f.webhook(t, forged, "sk_test_wrong"); code != http.StatusUnauthorized {
		t.Fatalf("forged webhook: %d; want 401", code)
	}
	if _, ps := f.state(t, id); ps != "pending" {
		t.Fatalf("forged webhook changed payment to %s", ps)
	}
	var bad int
	f.h.db.QueryRow(`SELECT COUNT(*) FROM paystack_events WHERE signature_valid = 0`).Scan(&bad)
	if bad != 1 {
		t.Fatalf("bad-signature deliveries logged = %d; want 1", bad)
	}

	before := f.fake.verifies
	training := `{"event":"charge.success","data":{"reference":"BPA-TEST-2026-ABCDEFGHJK","domain":"test","amount":75000,"currency":"GHS"}}`
	if code := f.webhook(t, training, psTestKey); code != http.StatusOK {
		t.Fatalf("training webhook: %d; want 200", code)
	}
	if f.fake.verifies != before {
		t.Fatal("a training reference triggered a Paystack verify")
	}
	var note string
	f.h.db.QueryRow(`SELECT note FROM paystack_events WHERE reference = 'BPA-TEST-2026-ABCDEFGHJK'`).Scan(&note)
	if !strings.Contains(note, "not a scheduling reference") {
		t.Fatalf("training event note = %q", note)
	}
}

// Cancelling a paid Paystack booking refunds it once, however many times cancel runs.
func TestPaystack_cancelRefundsOnce(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.startPaid(t, slotAt(3, 16), "cancel@example.com")
	f.fake.set(ref, "success", 225000, "GHS", "test")
	f.returnFrom(t, ref)

	f.h.refundBookingPayment(context.Background(), id)
	f.h.refundBookingPayment(context.Background(), id)
	if f.fake.refundCount(ref) != 1 {
		t.Fatalf("refunds = %d; want 1", f.fake.refundCount(ref))
	}
	if _, ps := f.state(t, id); ps != "refunded" {
		t.Fatalf("payment_status = %s; want refunded", ps)
	}
}

// A Paystack outage is never read as "unpaid": the hold survives and nothing is decided.
func TestPaystack_outageDecidesNothing(t *testing.T) {
	f := newPaystackFixture(t)
	id, ref := f.startPaid(t, slotAt(3, 17), "outage@example.com")
	f.fake.set(ref, "success", 225000, "GHS", "test")
	f.fake.mu.Lock()
	f.fake.failAPI = true
	f.fake.mu.Unlock()

	if loc := f.returnFrom(t, ref); !strings.Contains(loc, "payment=processing") {
		t.Fatalf("redirect during outage = %q; want payment=processing", loc)
	}
	if st, ps := f.state(t, id); st != "confirmed" || ps != "pending" {
		t.Fatalf("during outage = %s/%s; want hold kept", st, ps)
	}
	f.fake.mu.Lock()
	f.fake.failAPI = false
	f.fake.mu.Unlock()
	f.h.reconcilePaystack(context.Background(), false)
	if _, ps := f.state(t, id); ps != "paid" {
		t.Fatalf("after recovery = %s; want paid", ps)
	}
}

// Two bookers racing for the same paid slot: exactly one gets a checkout.
func TestPaystack_concurrentSameSlot(t *testing.T) {
	f := newPaystackFixture(t)
	start := slotAt(9, 10)
	const n = 8
	codes := make([]int, n)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			codes[i], _ = f.book(t, start, fmt.Sprintf("racer%d@example.com", i), true)
		}(i)
	}
	close(gate)
	wg.Wait()
	ok, conflict := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusConflict:
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("ok=%d conflict=%d codes=%v; want 1 and %d", ok, conflict, codes, n-1)
	}
}

// The booking page discloses the Paystack charge beside the listed price.
func TestPaidPriceLabel_disclosesPaystackCharge(t *testing.T) {
	en := i18n.Get("en")
	if got := paidPriceLabel(15000, "usd", "paystack", 225000, "GHS", en); got != "$150.00 · charged as 2250.00 GHS via Paystack" {
		t.Fatalf("label = %q", got)
	}
	if got := paidPriceLabel(15000, "usd", "stripe", 0, "GHS", en); got != "$150.00" {
		t.Fatalf("stripe label = %q", got)
	}
	if got := paidPriceLabel(15000, "usd", "paystack", 15000, "USD", en); got != "$150.00" {
		t.Fatalf("same-currency label = %q", got)
	}
}
