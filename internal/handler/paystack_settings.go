package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/calnode/calnode/internal/paystack"
	"github.com/calnode/calnode/internal/secret"
)

// PaystackConfig holds decrypted Paystack settings loaded from the DB.
type PaystackConfig struct {
	SecretKey string
	PublicKey string
}

// LoadPaystackSettingsFromDB reads and decrypts the Paystack credentials. Returns nil (not an
// error) when no secret key is stored.
func LoadPaystackSettingsFromDB(db *sql.DB, encKey [32]byte) (*PaystackConfig, error) {
	var secretEnc, pubKey string
	err := db.QueryRow(`SELECT paystack_secret_key_enc, paystack_public_key FROM server_settings WHERE id = 1`).
		Scan(&secretEnc, &pubKey)
	if err == sql.ErrNoRows || secretEnc == "" {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	cfg := &PaystackConfig{PublicKey: pubKey}
	if cfg.SecretKey, err = secret.Decrypt(encKey, secretEnc); err != nil {
		return nil, fmt.Errorf("decrypt paystack secret key: %w", err)
	}
	return cfg, nil
}

// NewPaystackClient builds the client for cfg. PAYSTACK_API_BASE points it at a stand-in
// API for end-to-end tests; the client honours it only under a TEST key, so it can never
// redirect live payments.
func NewPaystackClient(cfg *PaystackConfig) (*paystack.Client, error) {
	pc, err := paystack.New(cfg.SecretKey, cfg.PublicKey)
	if err != nil {
		return nil, err
	}
	pc.SetAPIBase(os.Getenv("PAYSTACK_API_BASE"))
	return pc, nil
}

// GetPaystackSettings handles GET /v1/settings/paystack (admin only). Never returns the key.
func (h *Handler) GetPaystackSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	var secretEnc, pubKey string
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT paystack_secret_key_enc, paystack_public_key FROM server_settings WHERE id = 1`).
		Scan(&secretEnc, &pubKey); err != nil && err != sql.ErrNoRows {
		h.logger.ErrorContext(r.Context(), "paystack settings: query", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	mode := ""
	if pc := h.getPaystack(); pc != nil {
		mode = string(pc.Mode())
	}
	h.writeJSON(w, http.StatusOK, map[string]any{
		"public_key":     pubKey,
		"secret_key_set": secretEnc != "",
		"configured":     secretEnc != "",
		"mode":           mode, // "test" | "live" | "" — from the key prefix
		"webhook_url":    h.baseURL + "/v1/paystack/webhook",
		"callback_url":   h.publicURL() + "/v1/paystack/return",
	})
}

// PatchPaystackSettings handles PATCH /v1/settings/paystack (admin only). Saves credentials
// and hot-reloads the client. A blank secret_key keeps the stored one.
func (h *Handler) PatchPaystackSettings(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireAdmin(w, r); !ok {
		return
	}
	if h.demoMode {
		h.writeError(w, http.StatusServiceUnavailable, "not available in the demo")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		SecretKey string  `json:"secret_key"`
		PublicKey *string `json:"public_key"`
		Clear     bool    `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.Clear {
		if _, err := h.db.ExecContext(r.Context(), `
			UPDATE server_settings SET paystack_secret_key_enc = '', paystack_public_key = '',
			       updated_at = datetime('now') WHERE id = 1`); err != nil {
			h.logger.ErrorContext(r.Context(), "paystack settings: clear", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		h.SetPaystack(nil)
		h.GetPaystackSettings(w, r)
		return
	}
	req.SecretKey = strings.TrimSpace(req.SecretKey)
	if req.SecretKey != "" && paystack.KeyMode(req.SecretKey) == paystack.ModeOff {
		h.writeError(w, http.StatusBadRequest, "secret key must start with sk_test_ or sk_live_")
		return
	}
	if req.PublicKey != nil {
		pk := strings.TrimSpace(*req.PublicKey)
		if pk != "" && !strings.HasPrefix(pk, "pk_test_") && !strings.HasPrefix(pk, "pk_live_") {
			h.writeError(w, http.StatusBadRequest, "public key must start with pk_test_ or pk_live_")
			return
		}
		if _, err := h.db.ExecContext(r.Context(),
			`UPDATE server_settings SET paystack_public_key = ?, updated_at = datetime('now') WHERE id = 1`, pk); err != nil {
			h.logger.ErrorContext(r.Context(), "paystack settings: update public key", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if req.SecretKey != "" {
		enc, err := secret.Encrypt(h.encKey, req.SecretKey)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "paystack settings: encrypt", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if _, err := h.db.ExecContext(r.Context(),
			`UPDATE server_settings SET paystack_secret_key_enc = ?, updated_at = datetime('now') WHERE id = 1`, enc); err != nil {
			h.logger.ErrorContext(r.Context(), "paystack settings: update secret key", "error", err)
			h.writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	cfg, err := LoadPaystackSettingsFromDB(h.db, h.encKey)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "paystack settings: reload", "error", err)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if cfg == nil {
		h.SetPaystack(nil)
	} else if pc, err := NewPaystackClient(cfg); err != nil {
		h.writeError(w, http.StatusBadRequest, err.Error())
		return
	} else {
		h.SetPaystack(pc)
		h.logger.Info("paystack settings: credentials updated", "mode", pc.Mode())
	}
	h.GetPaystackSettings(w, r)
}
