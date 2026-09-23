package store

import (
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ErrNotFound is returned whenever a lookup matches no live row. Revoked API
// keys are reported as not found on purpose, so callers cannot distinguish a
// revoked key from a key that never existed.
var ErrNotFound = errors.New("store: not found")

// NewID returns a fresh identifier for a persisted entity.
func NewID() string { return uuid.NewString() }

// Tenant is the unit of isolation. Everything else hangs off a tenant id.
type Tenant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// APIKey is a tenant credential. Only the SHA-256 hash and a display prefix are
// persisted; the plaintext is shown to the caller once at creation time.
//
// An APIKey belongs to a TENANT, never to a session. Re-pairing a WhatsApp
// number must not revoke, rotate or otherwise touch any key.
type APIKey struct {
	ID         string     `json:"id"`
	TenantID   string     `json:"tenant_id"`
	Name       string     `json:"name"`
	KeyHash    string     `json:"-"`
	KeyPrefix  string     `json:"key_prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// SessionStatus is the lifecycle state of a tenant's WhatsApp session.
type SessionStatus string

const (
	SessionPending      SessionStatus = "pending"
	SessionConnected    SessionStatus = "connected"
	SessionDisconnected SessionStatus = "disconnected"
	SessionLoggedOut    SessionStatus = "logged_out"
)

// Valid reports whether the status is one the schema accepts.
func (s SessionStatus) Valid() bool {
	switch s {
	case SessionPending, SessionConnected, SessionDisconnected, SessionLoggedOut:
		return true
	default:
		return false
	}
}

func (s SessionStatus) validate() error {
	if !s.Valid() {
		return fmt.Errorf("store: invalid session status %q", string(s))
	}
	return nil
}

// Session is a tenant's WhatsApp pairing state. There is at most one per tenant.
type Session struct {
	ID        string        `json:"id"`
	TenantID  string        `json:"tenant_id"`
	WAJID     *string       `json:"wa_jid,omitempty"`
	Status    SessionStatus `json:"status"`
	CreatedAt time.Time     `json:"created_at"`
	UpdatedAt time.Time     `json:"updated_at"`
}

// Direction says whether a message was received or sent by the gateway.
type Direction string

const (
	DirectionIn  Direction = "in"
	DirectionOut Direction = "out"
)

// Valid reports whether the direction is one the schema accepts.
func (d Direction) Valid() bool {
	return d == DirectionIn || d == DirectionOut
}

// Message is one persisted WhatsApp message.
type Message struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	ChatJID     string    `json:"chat_jid"`
	SenderJID   string    `json:"sender_jid"`
	WAMessageID string    `json:"wa_message_id"`
	Direction   Direction `json:"direction"`
	Body        string    `json:"body"`
	MediaType   *string   `json:"media_type,omitempty"`
	MediaPath   *string   `json:"media_path,omitempty"`
	Timestamp   time.Time `json:"timestamp"`
	CreatedAt   time.Time `json:"created_at"`
}

// Chat is a conversation summary derived from the messages table.
type Chat struct {
	ChatJID         string    `json:"chat_jid"`
	LastMessageAt   time.Time `json:"last_message_at"`
	LastMessageBody string    `json:"last_message_body"`
	LastDirection   Direction `json:"last_direction"`
	MessageCount    int       `json:"message_count"`
}
