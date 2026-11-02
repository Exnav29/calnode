package handler_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Bulletproof Scheduling "Private Desk" directory: sessions grouped by intent, the
// profile headline/bio, facts stated once, and the self-hosted brand fonts.

func TestPatchMe_headlineAndBio(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	code, resp := patchMeHandle(t, h, key, `{"headline":"  Independent Automation QA ","bio":" I review the automations your business depends on. "}`)
	if code != http.StatusOK {
		t.Fatalf("set: status = %d — %v", code, resp)
	}
	if resp["headline"] != "Independent Automation QA" || resp["bio"] != "I review the automations your business depends on." {
		t.Errorf("not trimmed and returned: %v", resp)
	}
	// Leaving the fields out keeps them; an empty string clears.
	if code, resp := patchMeHandle(t, h, key, `{"name":"Test User"}`); code != http.StatusOK || resp["headline"] != "Independent Automation QA" {
		t.Errorf("omitted headline changed: %d %v", code, resp)
	}
	if code, resp := patchMeHandle(t, h, key, `{"headline":""}`); code != http.StatusOK || resp["headline"] != "" {
		t.Errorf("clear: %d %v", code, resp)
	}
	for _, body := range []string{
		`{"headline":"` + strings.Repeat("é", 81) + `"}`,
		`{"bio":"` + strings.Repeat("x", 301) + `"}`,
	} {
		if code, _ := patchMeHandle(t, h, key, body); code != http.StatusBadRequest {
			t.Errorf("too long: status = %d; want 400", code)
		}
	}
	// 80 accented characters are 160 bytes; the limit counts characters.
	if code, _ := patchMeHandle(t, h, key, `{"headline":"`+strings.Repeat("é", 80)+`"}`); code != http.StatusOK {
		t.Errorf("80 characters: status = %d; want 200", code)
	}
}

func TestPersonPage_privateDeskGroups(t *testing.T) {
	h, database, _, ownerID := setupWorkspaceWithDB(t)
	mustExec(t, database, `UPDATE users SET handle = 'desk', headline = 'Independent Automation QA', bio = 'Bio <b>text</b>' WHERE id = ?`, ownerID)
	for _, et := range []struct {
		slug, name, desc string
		order, price     int
		provider         string
	}{
		{"chat", "General Conversation", "", 30, 0, "stripe"},
		{"fit", "Initial Fit Check", "A short call.\n\nSecond paragraph stays on the booking page.", 10, 0, "stripe"},
		{"diag", "Automation Diagnostic", "One **live** workflow, see [notes](https://example.com).", 20, 180000, "paystack"},
		{"scope", "Discovery", "", 40, 300050, "paystack"},
	} {
		mustExec(t, database, `INSERT INTO event_types (id,user_id,slug,name,description,duration_minutes,display_order,price_cents,currency,payment_provider,location_type)
			VALUES (?,?,?,?,?,60,?,?,'GHS',?,'google_meet')`, et.slug, ownerID, et.slug, et.name, et.desc, et.order, et.price, et.provider)
	}
	code, body := personPage(t, h, "desk")
	if code != http.StatusOK {
		t.Fatalf("page: %d", code)
	}
	for _, want := range []string{
		`<p class="dir-eyebrow">Independent Automation QA</p>`,
		`<p class="dir-bio">Bio &lt;b&gt;text&lt;/b&gt;</p>`,
		`All sessions are on Google Meet.`,
		`Paid sessions are secured by Paystack`,
		`<a class="dir-book" href="/book/fit">`,
		`<div class="dir-summary">A short call.</div>`,
		`<div class="dir-summary">One live workflow, see notes.</div>`,
		`<span class="dir-price">GH₵1,800</span>`,
		`<span class="dir-price">GH₵3,000.50</span>`,
		`<a class="dir-row" href="/book/chat">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	for _, unwanted := range []string{"Second paragraph", "example.com", "Book time with"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page contains %q", unwanted)
		}
	}
	// Featured, then engagements in display order, then the other free sessions.
	previous := -1
	for _, slug := range []string{"fit", "diag", "scope", "chat"} {
		i := strings.Index(body, `href="/book/`+slug+`"`)
		if i <= previous {
			t.Fatalf("missing or out-of-order %s", slug)
		}
		previous = i
	}
	// The location is said once, not on each card.
	if n := strings.Count(body, "Google Meet"); n != 1 {
		t.Errorf("Google Meet appears %d times; want 1", n)
	}
}

func TestPersonPage_mixedLocationsAndNoPaystack(t *testing.T) {
	h, database, _, ownerID := setupWorkspaceWithDB(t)
	mustExec(t, database, `UPDATE users SET handle = 'mixed' WHERE id = ?`, ownerID)
	mustExec(t, database, `INSERT INTO event_types (id,user_id,slug,name,duration_minutes,location_type) VALUES ('a',?,'a','A',30,'google_meet')`, ownerID)
	mustExec(t, database, `INSERT INTO event_types (id,user_id,slug,name,duration_minutes,location_type,price_cents,currency) VALUES ('b',?,'b','B',30,'zoom',15000,'usd')`, ownerID)
	_, body := personPage(t, h, "mixed")
	for _, unwanted := range []string{"All sessions are on", "Paystack", `class="dir-eyebrow"`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("page contains %q", unwanted)
		}
	}
	for _, want := range []string{"Google Meet", "Zoom", `<span class="dir-price">$150</span>`, `<p class="dir-bio">Book time with Test Host</p>`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// Only paid sessions: no "start here" group.
	mustExec(t, database, `UPDATE event_types SET price_cents = 5000 WHERE slug = 'a'`)
	if _, body := personPage(t, h, "mixed"); strings.Contains(body, `class="dir-book"`) || strings.Contains(body, "Start here") {
		t.Error("paid-only page rendered a featured session")
	}
}

func TestServeBrandFont(t *testing.T) {
	h, _, _ := setupWorkspace(t)
	dir := t.TempDir()
	h.SetDataDir(dir)
	get := func(name string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/brand-fonts/"+name, nil)
		req.SetPathValue("file", name)
		h.ServeBrandFont(rec, req)
		return rec
	}
	if rec := get("satoshi.woff2"); rec.Code != http.StatusNotFound {
		t.Errorf("missing file: %d; want 404", rec.Code)
	}
	if err := os.MkdirAll(filepath.Join(dir, "fonts"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fonts", "Satoshi-Variable.woff2"), []byte("wOF2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("no"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := get("satoshi.woff2")
	if rec.Code != http.StatusOK || rec.Body.String() != "wOF2" || rec.Header().Get("Content-Type") != "font/woff2" {
		t.Errorf("font: %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
	}
	for _, name := range []string{"Satoshi-Variable.woff2", "../secret.txt", "secret.txt"} {
		if rec := get(name); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d; want 404", name, rec.Code)
		}
	}
}
