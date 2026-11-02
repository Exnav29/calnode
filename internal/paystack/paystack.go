// Package paystack is a minimal, dependency-free client for the Paystack calls the
// paid-booking flow needs: initialize a transaction, verify one, refund one, and verify
// a webhook signature. One Paystack account per instance, like the Stripe client.
//
// Safety rules carried over from the Bulletproof Automations training integration:
//
//   - The secret key IS the mode switch. sk_test_ keys are "test", sk_live_ keys are
//     "live"; there is no separate flag that could disagree with the key.
//   - References carry their mode in the prefix (BPS-TEST- / BPS-LIVE-), so a test
//     charge can never settle a live booking and vice versa. The prefix is also what
//     distinguishes scheduling payments from training payments (BPA-) on the shared
//     Paystack account.
//   - Nothing the browser sends is proof of payment. A payment is only accepted after
//     a server-side GET /transaction/verify/:reference.
//   - Amounts are in minor units (pesewas, cents) end to end.
package paystack

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const apiBaseDefault = "https://api.paystack.co"

// Mode is the Paystack environment a key, reference or charge belongs to.
type Mode string

const (
	ModeOff  Mode = ""
	ModeTest Mode = "test"
	ModeLive Mode = "live"
)

// KeyMode derives the mode from a secret key's prefix.
func KeyMode(secretKey string) Mode {
	switch {
	case strings.HasPrefix(secretKey, "sk_live_"):
		return ModeLive
	case strings.HasPrefix(secretKey, "sk_test_"):
		return ModeTest
	}
	return ModeOff
}

// Reference prefixes. "BPS" = Bulletproof Scheduling; the training site uses "BPA-".
const (
	RefPrefix     = "BPS-"
	refPrefixLive = "BPS-LIVE-"
	refPrefixTest = "BPS-TEST-"
)

// Crockford base32 (no I, L, O, U) — same alphabet as the training references.
const refAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// MintReference returns a new unique transaction reference for mode, e.g.
// BPS-TEST-2026-7G3KQ0ZP1M4X9D2B. 16 random base32 characters = 80 bits.
func MintReference(mode Mode, now time.Time) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var tail strings.Builder
	for _, x := range b {
		// 256 = 8 × 32, so the modulo is unbiased.
		tail.WriteByte(refAlphabet[x%32])
	}
	prefix := refPrefixTest
	if mode == ModeLive {
		prefix = refPrefixLive
	}
	return prefix + strconv.Itoa(now.UTC().Year()) + "-" + tail.String(), nil
}

// ReferenceMode reports which mode a reference was minted for, or ModeOff for a
// reference this package did not mint.
func ReferenceMode(ref string) Mode {
	switch {
	case strings.HasPrefix(ref, refPrefixLive):
		return ModeLive
	case strings.HasPrefix(ref, refPrefixTest):
		return ModeTest
	}
	return ModeOff
}

// Client talks to one Paystack account using its secret key.
type Client struct {
	secretKey string
	publicKey string
	mode      Mode
	http      *http.Client
	apiBase   string
}

// New builds a client. The key must be an sk_test_ or sk_live_ key.
func New(secretKey, publicKey string) (*Client, error) {
	mode := KeyMode(secretKey)
	if mode == ModeOff {
		return nil, fmt.Errorf("paystack: secret key must start with sk_test_ or sk_live_")
	}
	return &Client{
		secretKey: secretKey,
		publicKey: publicKey,
		mode:      mode,
		http:      &http.Client{Timeout: 20 * time.Second},
		apiBase:   apiBaseDefault,
	}, nil
}

// SetAPIBase points the client at another host. Honoured ONLY under a test key, so no
// configuration mistake can redirect live payments away from api.paystack.co.
func (c *Client) SetAPIBase(base string) {
	if c.mode == ModeTest && base != "" {
		c.apiBase = strings.TrimRight(base, "/")
	}
}

// Mode returns the environment of the configured key.
func (c *Client) Mode() Mode { return c.mode }

// PublicKey returns the configured public key (safe to show).
func (c *Client) PublicKey() string { return c.publicKey }

// InitParams describes a one-off charge.
type InitParams struct {
	Email       string
	AmountMinor int64  // pesewas / cents
	Currency    string // upper-case ISO 4217, e.g. GHS
	Reference   string
	CallbackURL string
	Metadata    map[string]any
}

// InitResult is the hosted checkout to send the customer to.
type InitResult struct {
	AuthorizationURL string `json:"authorization_url"`
	AccessCode       string `json:"access_code"`
	Reference        string `json:"reference"`
}

// Initialize creates a transaction and returns the hosted checkout URL.
func (c *Client) Initialize(ctx context.Context, p InitParams) (*InitResult, error) {
	body := map[string]any{
		"email":        p.Email,
		"amount":       p.AmountMinor,
		"currency":     strings.ToUpper(p.Currency),
		"reference":    p.Reference,
		"callback_url": p.CallbackURL,
	}
	if len(p.Metadata) > 0 {
		body["metadata"] = p.Metadata
	}
	var out InitResult
	if err := c.do(ctx, http.MethodPost, "/transaction/initialize", body, &out); err != nil {
		return nil, err
	}
	if out.AuthorizationURL == "" {
		return nil, fmt.Errorf("paystack: initialize returned no authorization_url")
	}
	return &out, nil
}

// Transaction is the subset of a verified transaction the booking flow checks.
type Transaction struct {
	ID        int64  `json:"id"`
	Domain    string `json:"domain"`    // "test" | "live"
	Status    string `json:"status"`    // "success" | "failed" | "abandoned" | "ongoing" | "pending" | "reversed" ...
	Reference string `json:"reference"` //
	Amount    int64  `json:"amount"`    // minor units
	Currency  string `json:"currency"`  //
	PaidAt    string `json:"paid_at"`
}

// ErrNotFound means Paystack has no transaction with that reference (e.g. checkout was
// never opened). Distinct from a transport failure, which must not be read as "unpaid".
var ErrNotFound = fmt.Errorf("paystack: transaction not found")

// Verify fetches a transaction by reference from Paystack. This, not a webhook body
// and never a browser redirect, is the evidence a payment is accepted on.
func (c *Client) Verify(ctx context.Context, reference string) (*Transaction, error) {
	var out Transaction
	err := c.do(ctx, http.MethodGet, "/transaction/verify/"+url.PathEscape(reference), nil, &out)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// Refund requests a full refund of a transaction (by reference). Paystack rejects a
// second full refund of the same transaction, so a retry cannot pay out twice.
func (c *Client) Refund(ctx context.Context, reference string) error {
	return c.do(ctx, http.MethodPost, "/refund", map[string]any{"transaction": reference}, nil)
}

// Refund is the subset of a Paystack refund that exception monitoring reads.
type Refund struct {
	ID                   int64  `json:"id"`
	Transaction          int64  `json:"-"`                     // numeric transaction id; 0 if absent or unreadable
	TransactionReference string `json:"transaction_reference"` // not in every response shape; may be ""
	Domain               string `json:"domain"`                //
	Amount               int64  `json:"amount"`                // minor units
	Currency             string `json:"currency"`              //
	Status               string `json:"status"`                // pending | processing | needs-attention | failed | processed
	Reason               string `json:"reason"`                // e.g. "NEEDS-ATTENTION"
}

// UnmarshalJSON reads "transaction" as Paystack's numeric id (as returned today and in the
// published List Refunds examples), a numeric string, or an object {id, reference}. A
// reference found only inside that object fills TransactionReference. Anything else leaves
// Transaction 0, which the monitor treats as unmatched rather than guessing.
func (r *Refund) UnmarshalJSON(b []byte) error {
	type plain Refund
	var aux struct {
		plain
		Transaction json.RawMessage `json:"transaction"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*r = Refund(aux.plain)
	if len(aux.Transaction) == 0 {
		return nil
	}
	var obj struct {
		ID        json.Number `json:"id"`
		Reference string      `json:"reference"`
	}
	var num json.Number
	switch {
	case json.Unmarshal(aux.Transaction, &num) == nil:
	case json.Unmarshal(aux.Transaction, &obj) == nil:
		num = obj.ID
		if r.TransactionReference == "" {
			r.TransactionReference = obj.Reference
		}
	}
	if id, err := strconv.ParseInt(string(num), 10, 64); err == nil && id > 0 {
		r.Transaction = id
	}
	return nil
}

// ListRefunds returns the refunds Paystack holds for a transaction (by numeric id).
// Read-only: there is deliberately no client method to retry a refund or submit a
// customer's account details — refunds needing that are resolved by a person.
func (c *Client) ListRefunds(ctx context.Context, transactionID int64) ([]Refund, error) {
	var out []Refund
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/refund?transaction=%d&perPage=100", transactionID), nil, &out)
	return out, err
}

// SignatureValid reports whether header is the HMAC-SHA512 (hex) of the exact raw body
// under secretKey. Constant-time comparison.
func SignatureValid(raw []byte, header, secretKey string) bool {
	if header == "" || secretKey == "" {
		return false
	}
	m := hmac.New(sha512.New, []byte(secretKey))
	m.Write(raw)
	want := m.Sum(nil)
	got, err := hex.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return false
	}
	return hmac.Equal(want, got)
}

// VerifySignature checks a webhook body against this client's secret key.
func (c *Client) VerifySignature(raw []byte, header string) bool {
	return SignatureValid(raw, header, c.secretKey)
}

// Event is a Paystack webhook envelope.
type Event struct {
	Event string `json:"event"`
	Data  struct {
		Reference            string `json:"reference"`
		Domain               string `json:"domain"`
		Status               string `json:"status"`
		Amount               int64  `json:"amount"`
		Currency             string `json:"currency"`
		TransactionReference string `json:"transaction_reference"`
		Transaction          struct {
			Reference string `json:"reference"`
		} `json:"transaction"`
	} `json:"data"`
}

// TxnReference is the transaction reference an event refers to (refund events nest it).
func (e *Event) TxnReference() string {
	for _, s := range []string{e.Data.TransactionReference, e.Data.Transaction.Reference, e.Data.Reference} {
		if s != "" {
			return s
		}
	}
	return ""
}

// apiError carries Paystack's HTTP status so callers can tell "not found" from outages.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return fmt.Sprintf("paystack: %d: %s", e.status, e.msg) }

func (c *Client) do(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.secretKey)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("paystack: request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env struct {
		Status  bool            `json:"status"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(raw, &env)
	// Paystack answers an unknown reference with 400 "Transaction reference not found"
	// (404 on some endpoints). Either way it is a definite "no such transaction".
	if resp.StatusCode == http.StatusNotFound ||
		(resp.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(env.Message), "not found")) {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 || !env.Status {
		msg := env.Message
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return &apiError{status: resp.StatusCode, msg: msg}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("paystack: decode response: %w", err)
		}
	}
	return nil
}
