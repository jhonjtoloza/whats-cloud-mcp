// Package wa wraps the WhatsApp engine (go.mau.fi/whatsmeow) behind a narrow
// interface so the HTTP layer can be tested without a live socket.
//
// whatsmeow is MPL-2.0 and is used strictly as a dependency: nothing in this
// repository modifies or vendors its source.
package wa

import (
	"context"
	"errors"
)

// Common failures the HTTP layer maps onto status codes.
var (
	// ErrNotPaired means the tenant has no usable WhatsApp session yet.
	ErrNotPaired = errors.New("wa: tenant is not paired")
	// ErrUnknownTenant means no session record exists for the tenant.
	ErrUnknownTenant = errors.New("wa: unknown tenant")
	// ErrInvalidJID means the destination could not be parsed.
	ErrInvalidJID = errors.New("wa: invalid jid")
)

// PairingMode says which of the two pairing flows produced a result.
type PairingMode string

const (
	// PairingModeCode is the preferred flow: the user types an 8-character
	// code on their phone.
	PairingModeCode PairingMode = "code"
	// PairingModeQR is the fallback: the user scans a QR string.
	PairingModeQR PairingMode = "qr"
)

// PairingResult carries whichever credential the chosen flow produced. Exactly
// one of PairCode or QR is populated.
type PairingResult struct {
	Mode     PairingMode `json:"mode"`
	PairCode string      `json:"pair_code,omitempty"`
	QR       string      `json:"qr,omitempty"`
}

// SessionStatus is the connection state reported for a tenant.
type SessionStatus struct {
	TenantID  string `json:"tenant_id"`
	Status    string `json:"status"`
	WAJID     string `json:"wa_jid,omitempty"`
	Connected bool   `json:"connected"`
	LoggedIn  bool   `json:"logged_in"`
}

// SessionManager owns the WhatsApp side of a tenant.
//
// Implementations hold one whatsmeow client per tenant. The HTTP layer only
// ever talks to this interface, which is what lets handler tests run against a
// fake instead of a real socket.
type SessionManager interface {
	// StartPairing begins pairing for a tenant. A non-empty phone (E.164,
	// digits only) uses the pair-code flow; an empty phone falls back to QR.
	StartPairing(ctx context.Context, tenantID string, phone string) (PairingResult, error)
	// Status reports the tenant's current connection state.
	Status(ctx context.Context, tenantID string) (SessionStatus, error)
	// SendText sends a plain text message and returns the WhatsApp message id.
	SendText(ctx context.Context, tenantID, toJID, body string) (string, error)
	// Logout unlinks the tenant's device and drops the client.
	Logout(ctx context.Context, tenantID string) error
}
