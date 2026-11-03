package paystack

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestKeyMode(t *testing.T) {
	cases := map[string]Mode{
		"sk_live_abc": ModeLive,
		"sk_test_abc": ModeTest,
		"pk_test_abc": ModeOff,
		"":            ModeOff,
		"sk_abc":      ModeOff,
	}
	for key, want := range cases {
		if got := KeyMode(key); got != want {
			t.Errorf("KeyMode(%q) = %q; want %q", key, got, want)
		}
	}
	if _, err := New("pk_live_x", ""); err == nil {
		t.Error("New accepted a non-secret key")
	}
}

func TestMintReference_modePrefixAndUniqueness(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		ref, err := MintReference(ModeTest, now)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(ref, "BPS-TEST-2026-") || len(ref) != len("BPS-TEST-2026-")+16 {
			t.Fatalf("bad test reference %q", ref)
		}
		if seen[ref] {
			t.Fatalf("duplicate reference %q", ref)
		}
		seen[ref] = true
		if ReferenceMode(ref) != ModeTest {
			t.Fatalf("ReferenceMode(%q) != test", ref)
		}
	}
	live, _ := MintReference(ModeLive, now)
	if !strings.HasPrefix(live, "BPS-LIVE-") || ReferenceMode(live) != ModeLive {
		t.Fatalf("bad live reference %q", live)
	}
	// Training-site references and foreign ones are not ours.
	for _, ref := range []string{"BPA-TEST-2026-ABC", "BPA-CAB-2026-ABC", "T123456", ""} {
		if ReferenceMode(ref) != ModeOff {
			t.Errorf("ReferenceMode(%q) should be off", ref)
		}
	}
}

func TestSignatureValid(t *testing.T) {
	body := []byte(`{"event":"charge.success","data":{"reference":"BPS-TEST-2026-X"}}`)
	m := hmac.New(sha512.New, []byte("sk_test_secret"))
	m.Write(body)
	sig := hex.EncodeToString(m.Sum(nil))

	if !SignatureValid(body, sig, "sk_test_secret") {
		t.Fatal("valid signature rejected")
	}
	if SignatureValid(body, sig, "sk_test_other") {
		t.Fatal("signature under another key accepted")
	}
	// The signature is over the exact raw bytes: re-serialised JSON must not verify.
	if SignatureValid([]byte(`{"data":{"reference":"BPS-TEST-2026-X"},"event":"charge.success"}`), sig, "sk_test_secret") {
		t.Fatal("reordered body accepted")
	}
	if SignatureValid(body, "", "sk_test_secret") || SignatureValid(body, "zz", "sk_test_secret") {
		t.Fatal("empty/garbage signature accepted")
	}
}

func TestSetAPIBase_ignoredForLiveKeys(t *testing.T) {
	live, _ := New("sk_live_x", "")
	live.SetAPIBase("http://evil.example")
	if live.apiBase != apiBaseDefault {
		t.Fatalf("live client api base = %q; must stay %q", live.apiBase, apiBaseDefault)
	}
	test, _ := New("sk_test_x", "")
	test.SetAPIBase("http://127.0.0.1:9/")
	if test.apiBase != "http://127.0.0.1:9" {
		t.Fatalf("test client api base = %q", test.apiBase)
	}
}

// Refund objects arrive in more than one shape. Live (refund 18949128, 2026-11-02): numeric
// "transaction" plus "transaction_reference". Paystack's published List Refunds examples:
// numeric "transaction" and no "transaction_reference". Some endpoints nest the
// transaction as an object. Unreadable ids must come out as 0, never as a guess.
func TestRefund_unmarshalShapes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantTx  int64
		wantRef string
	}{
		{"live: id and reference", `{"id":18949128,"transaction":6721157889,"transaction_reference":"BPS-LIVE-2026-X","domain":"live","amount":100,"currency":"GHS","status":"processed","reason":"PROCESSED","retriable":false,"refund_channel":"Original Payment","customer":{"id":1}}`, 6721157889, "BPS-LIVE-2026-X"},
		{"documented: id only", `{"id":1,"transaction":6721157889,"domain":"live","amount":100,"currency":"GHS","status":"needs-attention"}`, 6721157889, ""},
		{"object with id and reference", `{"id":1,"transaction":{"id":6721157889,"reference":"BPS-LIVE-2026-X"},"status":"failed"}`, 6721157889, "BPS-LIVE-2026-X"},
		{"object with id only", `{"id":1,"transaction":{"id":6721157889},"status":"failed"}`, 6721157889, ""},
		{"top-level reference wins over object", `{"id":1,"transaction":{"id":5,"reference":"B"},"transaction_reference":"A"}`, 5, "A"},
		{"numeric string id", `{"id":1,"transaction":"6721157889"}`, 6721157889, ""},
		{"missing transaction", `{"id":1,"transaction_reference":"BPS-LIVE-2026-X"}`, 0, "BPS-LIVE-2026-X"},
		{"null transaction", `{"id":1,"transaction":null}`, 0, ""},
		{"reference string in place of id", `{"id":1,"transaction":"BPS-LIVE-2026-X"}`, 0, ""},
		{"object without id", `{"id":1,"transaction":{"reference":"BPS-LIVE-2026-X"}}`, 0, "BPS-LIVE-2026-X"},
		{"negative id", `{"id":1,"transaction":-3}`, 0, ""},
		{"fractional id", `{"id":1,"transaction":3.5}`, 0, ""},
	}
	for _, c := range cases {
		var r Refund
		if err := json.Unmarshal([]byte(c.body), &r); err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if r.Transaction != c.wantTx || r.TransactionReference != c.wantRef {
			t.Errorf("%s: transaction = %d, reference = %q; want %d, %q", c.name, r.Transaction, r.TransactionReference, c.wantTx, c.wantRef)
		}
	}
	var r Refund
	_ = json.Unmarshal([]byte(`{"id":18949128,"transaction":6721157889,"amount":100,"currency":"GHS","status":"processed","reason":"PROCESSED"}`), &r)
	if r.ID != 18949128 || r.Amount != 100 || r.Currency != "GHS" || r.Status != "processed" || r.Reason != "PROCESSED" {
		t.Errorf("other fields = %+v", r)
	}
}

// ListRefunds is one GET with the numeric transaction id, and parses a list mixing shapes.
func TestListRefunds_getByTransactionID(t *testing.T) {
	var method, query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		method, query = req.Method, req.URL.Path+"?"+req.URL.RawQuery
		w.Write([]byte(`{"status":true,"data":[{"id":1,"transaction":42,"status":"needs-attention"},{"id":2,"transaction":{"id":42},"transaction_reference":"BPS-TEST-2026-A","status":"processed"}]}`))
	}))
	defer srv.Close()
	c, err := New("sk_test_x", "")
	if err != nil {
		t.Fatal(err)
	}
	c.SetAPIBase(srv.URL)
	got, err := c.ListRefunds(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet || query != "/refund?transaction=42&perPage=100" {
		t.Errorf("request = %s %s", method, query)
	}
	if len(got) != 2 || got[0].Transaction != 42 || got[1].Transaction != 42 || got[1].TransactionReference != "BPS-TEST-2026-A" {
		t.Errorf("refunds = %+v", got)
	}
}
