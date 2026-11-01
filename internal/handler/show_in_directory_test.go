package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// "Show in directory" (Bulletproof Scheduling): an active, public event type with the flag
// off is left out of the person/team directories but stays bookable at its direct URL. The
// existing Active and Public switches keep their meaning.

func patchShowDir(t *testing.T, h interface {
	RequireAuth(http.HandlerFunc) http.HandlerFunc
	PatchEventType(http.ResponseWriter, *http.Request)
}, key, slug, body string) map[string]any {
	t.Helper()
	req := authReq(http.MethodPatch, "/v1/event-types/"+slug, body, key)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.PatchEventType)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("patch %s %s: %d — %s", slug, body, rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out) //nolint:errcheck
	return out
}

func bookPageCode(h interface {
	BookPage(http.ResponseWriter, *http.Request)
}, slug string) int {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/book/"+slug, nil)
	req.SetPathValue("slug", slug)
	h.BookPage(rec, req)
	return rec.Code
}

func TestShowInDirectory_unlistedHiddenButBookableByLink(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	if code, _ := patchMeHandle(t, h, key, `{"handle":"listed-host"}`); code != http.StatusOK {
		t.Fatalf("set handle: %d", code)
	}
	listed, _ := seedEventTypeHTTP(t, h, key)
	unlisted, _ := seedEventTypeHTTP(t, h, key)

	if got := patchShowDir(t, h, key, unlisted, `{"show_in_directory":false}`); got["show_in_directory"] != false {
		t.Fatalf("show_in_directory after patch = %v; want false", got["show_in_directory"])
	}

	// Directory: lists the listed one only.
	code, body := personPage(t, h, "listed-host")
	if code != http.StatusOK {
		t.Fatalf("directory: %d", code)
	}
	if !strings.Contains(body, "/book/"+listed) {
		t.Errorf("directory is missing the listed event type %s", listed)
	}
	if strings.Contains(body, "/book/"+unlisted) {
		t.Errorf("directory shows the unlisted event type %s", unlisted)
	}

	// Direct link: page, slots, questions and booking all still work.
	if c := bookPageCode(h, unlisted); c != http.StatusOK {
		t.Errorf("unlisted booking page: %d; want 200", c)
	}
	slotsReq := httptest.NewRequest(http.MethodGet,
		"/v1/event-types/"+unlisted+"/slots?start=2026-06-15&end=2026-06-15&timezone=UTC", nil)
	slotsReq.SetPathValue("slug", unlisted)
	slotsRec := httptest.NewRecorder()
	h.GetSlots(slotsRec, slotsReq)
	if slotsRec.Code != http.StatusOK {
		t.Errorf("unlisted slots: %d; want 200", slotsRec.Code)
	}
	qReq := httptest.NewRequest(http.MethodGet, "/v1/event-types/"+unlisted+"/questions", nil)
	qReq.SetPathValue("slug", unlisted)
	qRec := httptest.NewRecorder()
	h.ListQuestions(qRec, qReq)
	if qRec.Code != http.StatusOK {
		t.Errorf("unlisted questions: %d; want 200", qRec.Code)
	}
	bookReq := httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(fmt.Sprintf(
		`{"event_type_slug":%q,"start_at":"2026-06-15T09:00:00Z","name":"Client","email":"client@example.com"}`, unlisted)))
	bookReq.Header.Set("Content-Type", "application/json")
	bookRec := httptest.NewRecorder()
	h.CreateBooking(bookRec, bookReq)
	if bookRec.Code != http.StatusCreated {
		t.Errorf("booking the unlisted event type: %d — %s; want 201", bookRec.Code, bookRec.Body.String())
	}
}

func TestShowInDirectory_activeAndPublicStillGate(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	if code, _ := patchMeHandle(t, h, key, `{"handle":"gate-host"}`); code != http.StatusOK {
		t.Fatalf("set handle: %d", code)
	}
	inactive, _ := seedEventTypeHTTP(t, h, key)
	private, _ := seedEventTypeHTTP(t, h, key)
	patchShowDir(t, h, key, inactive, `{"is_active":false}`)
	patchShowDir(t, h, key, private, `{"is_public":false}`)

	_, body := personPage(t, h, "gate-host")
	for _, slug := range []string{inactive, private} {
		if strings.Contains(body, "/book/"+slug) {
			t.Errorf("directory lists %s", slug)
		}
		if c := bookPageCode(h, slug); c != http.StatusNotFound {
			t.Errorf("booking page for %s: %d; want 404", slug, c)
		}
		rec := httptest.NewRecorder()
		h.CreateBooking(rec, httptest.NewRequest(http.MethodPost, "/v1/bookings", strings.NewReader(fmt.Sprintf(
			`{"event_type_slug":%q,"start_at":"2026-06-15T09:00:00Z","name":"X","email":"x@example.com"}`, slug))))
		if rec.Code != http.StatusNotFound {
			t.Errorf("booking %s: %d; want 404", slug, rec.Code)
		}
	}
}

func TestShowInDirectory_defaultsOnAndSurvivesDuplicate(t *testing.T) {
	h, key, _ := setupWorkspace(t)
	slug, _ := seedEventTypeHTTP(t, h, key)

	if got := patchShowDir(t, h, key, slug, `{"name":"Renamed"}`); got["show_in_directory"] != true {
		t.Fatalf("new event type show_in_directory = %v; want true (default)", got["show_in_directory"])
	}
	patchShowDir(t, h, key, slug, `{"show_in_directory":false}`)

	req := authReq(http.MethodPost, "/v1/event-types/"+slug+"/duplicate", "", key)
	req.SetPathValue("slug", slug)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.DuplicateEventType)(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("duplicate: %d — %s", rec.Code, rec.Body.String())
	}
	var dup map[string]any
	json.Unmarshal(rec.Body.Bytes(), &dup) //nolint:errcheck
	if dup["show_in_directory"] != false || dup["is_active"] != false {
		t.Errorf("duplicate show_in_directory=%v is_active=%v; want false/false", dup["show_in_directory"], dup["is_active"])
	}
}
