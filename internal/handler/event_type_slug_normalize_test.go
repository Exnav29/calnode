package handler_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calnode/calnode/internal/handler"
)

// The production incident: a slug pasted with a leading tab was stored verbatim. The
// list showed it, but browsers strip tabs from URLs, so the dashboard's settings link
// asked for "ai-receptionist-onboarding" and the editor reported "event type not found".
const pastedSlug = "\tai-receptionist-onboarding"

// slugMux routes the event type endpoints the way the server does, so a request path
// is decoded into the {slug} wildcard exactly as it is in production.
func slugMux(h *handler.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/event-types/{slug}", h.RequireAuth(h.GetEventType))
	mux.HandleFunc("PATCH /v1/event-types/{slug}", h.RequireAuth(h.PatchEventType))
	mux.HandleFunc("POST /v1/event-types/{slug}/duplicate", h.RequireAuth(h.DuplicateEventType))
	return mux
}

func serveSlug(t *testing.T, mux *http.ServeMux, method, path, body, apiKey string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, authReq(method, path, body, apiKey))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func createRaw(t *testing.T, h *handler.Handler, apiKey, slug string) (int, map[string]any) {
	t.Helper()
	body := fmt.Sprintf(`{"slug":%q,"name":"AI Receptionist Onboarding","duration_minutes":60}`, slug)
	rec := httptest.NewRecorder()
	h.RequireAuth(h.CreateEventType)(rec, authReq(http.MethodPost, "/v1/event-types", body, apiKey))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// Create must store the slug the editor's route can reach: normalised the same way a
// rename through PATCH is.
func TestCreateEventType_normalisesSlug(t *testing.T) {
	cases := map[string]string{
		pastedSlug:                      "ai-receptionist-onboarding",
		"ai-receptionist-onboarding\n":  "ai-receptionist-onboarding",
		"  AI Receptionist Onboarding ": "ai-receptionist-onboarding",
		"Intro_Call":                    "intro-call",
	}
	for in, want := range cases {
		t.Run(fmt.Sprintf("%q", in), func(t *testing.T) {
			h, apiKey, _ := setupWorkspace(t)
			code, body := createRaw(t, h, apiKey, in)
			if code != http.StatusCreated {
				t.Fatalf("create: %d - %v", code, body)
			}
			if got := body["slug"]; got != want {
				t.Fatalf("stored slug = %q, want %q", got, want)
			}
			// The settings page fetches by the slug in its URL; it must find the row.
			code, got := serveSlug(t, slugMux(h), http.MethodGet, "/v1/event-types/"+want, "", apiKey)
			if code != http.StatusOK {
				t.Fatalf("GET by stored slug: %d - %v", code, got)
			}
			if got["duration_minutes"] != float64(60) {
				t.Errorf("duration = %v, want 60", got["duration_minutes"])
			}
		})
	}
}

func TestCreateEventType_rejectsSlugWithNothingUsable(t *testing.T) {
	h, apiKey, _ := setupWorkspace(t)
	for _, in := range []string{"\t", "   ", "!!!", "\t\n"} {
		if code, body := createRaw(t, h, apiKey, in); code != http.StatusBadRequest {
			t.Errorf("create %q: %d, want 400 - %v", in, code, body)
		}
	}
}

// A row stored before the fix keeps its tab. With the slug percent-encoded in the URL
// (as the dashboard now sends it) the editor can load it, and saving from the editor
// renames it to the clean slug without touching anything else on the row.
func TestLegacyUnsafeSlug_editorCanOpenAndRepair(t *testing.T) {
	h, database, apiKey, _ := setupWorkspaceWithDB(t)
	code, created := createRaw(t, h, apiKey, "placeholder")
	if code != http.StatusCreated {
		t.Fatalf("create: %d - %v", code, created)
	}
	if _, err := database.Exec(`UPDATE event_types SET slug = ? WHERE id = ?`, pastedSlug, created["id"]); err != nil {
		t.Fatalf("simulate legacy row: %v", err)
	}
	mux := slugMux(h)

	// What the browser produced from the unencoded link: the tab is gone, so no match.
	if code, _ := serveSlug(t, mux, http.MethodGet, "/v1/event-types/ai-receptionist-onboarding", "", apiKey); code != http.StatusNotFound {
		t.Fatalf("stripped slug: %d, want 404 (the reported symptom)", code)
	}

	// Encoded, it resolves.
	code, got := serveSlug(t, mux, http.MethodGet, "/v1/event-types/%09ai-receptionist-onboarding", "", apiKey)
	if code != http.StatusOK {
		t.Fatalf("GET encoded legacy slug: %d - %v", code, got)
	}
	if got["id"] != created["id"] {
		t.Fatalf("resolved a different event type: %v", got["id"])
	}

	// The editor saves with slug: form.slug.trim(); the description edit stands in for
	// any other change made in the same save.
	code, got = serveSlug(t, mux, http.MethodPatch, "/v1/event-types/%09ai-receptionist-onboarding",
		`{"slug":"ai-receptionist-onboarding","description":"Kickoff for new AI receptionist clients"}`, apiKey)
	if code != http.StatusOK {
		t.Fatalf("save from editor: %d - %v", code, got)
	}
	if got["slug"] != "ai-receptionist-onboarding" || got["id"] != created["id"] {
		t.Fatalf("after save: slug=%q id=%v", got["slug"], got["id"])
	}

	// Reopening the settings page by the clean slug works, and the row is the same one
	// with its duration and the edit intact.
	code, got = serveSlug(t, mux, http.MethodGet, "/v1/event-types/ai-receptionist-onboarding", "", apiKey)
	if code != http.StatusOK {
		t.Fatalf("reopen: %d - %v", code, got)
	}
	if got["id"] != created["id"] || got["duration_minutes"] != float64(60) ||
		got["description"] != "Kickoff for new AI receptionist clients" {
		t.Fatalf("reopened row changed unexpectedly: %v", got)
	}
	var n int
	if err := database.QueryRow(`SELECT count(*) FROM event_types WHERE name = 'AI Receptionist Onboarding'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("expected exactly one onboarding event type, got %d (%v)", n, err)
	}
}

// Duplicating a legacy row must not copy the unsafe characters into the new slug.
func TestDuplicateEventType_normalisesLegacySourceSlug(t *testing.T) {
	h, database, apiKey, _ := setupWorkspaceWithDB(t)
	_, created := createRaw(t, h, apiKey, "placeholder")
	if _, err := database.Exec(`UPDATE event_types SET slug = ? WHERE id = ?`, pastedSlug, created["id"]); err != nil {
		t.Fatalf("simulate legacy row: %v", err)
	}
	code, got := serveSlug(t, slugMux(h), http.MethodPost, "/v1/event-types/%09ai-receptionist-onboarding/duplicate", "", apiKey)
	if code != http.StatusCreated {
		t.Fatalf("duplicate: %d - %v", code, got)
	}
	if got["slug"] != "ai-receptionist-onboarding-copy" {
		t.Fatalf("copy slug = %q, want ai-receptionist-onboarding-copy", got["slug"])
	}
}
