package paystack

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
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
