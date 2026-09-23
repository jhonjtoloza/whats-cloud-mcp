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

// MediaStatus says what happened the last time somebody asked for a message's
// attachment. A message whose media nobody has ever requested has no status at
// all, which is the normal state: media is fetched lazily.
type MediaStatus string

const (
	// MediaAvailable means the bytes are on disk at MediaPath.
	MediaAvailable MediaStatus = "available"
	// MediaUnavailable means WhatsApp no longer serves the media. It expires
	// server-side, so a stored reference can outlive its bytes. This is final:
	// retrying can only fail again.
	MediaUnavailable MediaStatus = "unavailable"
	// MediaFailed means the fetch failed for some other reason and may be
	// retried.
	MediaFailed MediaStatus = "failed"
)

// Message is one persisted WhatsApp message.
//
// The media_* fields are a REFERENCE, not the attachment. Media is downloaded
// only when an MCP tool or an HTTP route explicitly asks for it; see
// migrations/0002_media_reference.sql for why.
//
// MediaKey, FileEncSHA256 and FileSHA256 are secrets: they decrypt the
// attachment on WhatsApp's CDN. They are never serialised over HTTP, and
// neither is MediaPath, which is a filesystem path on the gateway host.
type Message struct {
	ID          string    `json:"id"`
	TenantID    string    `json:"tenant_id"`
	ChatJID     string    `json:"chat_jid"`
	SenderJID   string    `json:"sender_jid"`
	WAMessageID string    `json:"wa_message_id"`
	Direction   Direction `json:"direction"`
	Body        string    `json:"body"`
	MediaType   *string   `json:"media_type,omitempty"`
	// MediaPath is where the bytes live on the gateway host once fetched. It is
	// never returned over HTTP: a client has no use for a path it cannot reach,
	// and publishing the layout of the media directory helps nobody but an
	// attacker.
	MediaPath *string `json:"-"`

	// The download reference, captured on receipt and on history sync.
	DirectPath    *string `json:"-"`
	MediaKey      []byte  `json:"-"`
	FileEncSHA256 []byte  `json:"-"`
	FileSHA256    []byte  `json:"-"`
	MMSType       *string `json:"-"`
	FileLength    *int64  `json:"file_length,omitempty"`
	MimeType      *string `json:"mime_type,omitempty"`

	// The bookkeeping of the last fetch attempt.
	MediaStatus *string `json:"media_status,omitempty"`
	// MediaError is the reason the last fetch failed. It is kept for operators
	// and is not published over HTTP, where the status code carries the answer.
	MediaError     *string    `json:"-"`
	MediaFetchedAt *time.Time `json:"media_fetched_at,omitempty"`

	Timestamp time.Time `json:"timestamp"`
	CreatedAt time.Time `json:"created_at"`
}

// HasMediaReference reports whether the row carries enough to fetch the bytes.
//
// A direct path is the one part no download can do without; whatsmeow rejects
// an empty one outright.
func (m Message) HasMediaReference() bool {
	return m.DirectPath != nil && *m.DirectPath != ""
}

// MediaUpdate carries the media_* columns a fetch attempt rewrites.
//
// It is deliberately narrow: a failed download must never lose or corrupt the
// message, so nothing else in the row is writable through this path.
type MediaUpdate struct {
	// Path is the on-disk location, set only by a successful fetch.
	Path      *string
	Status    MediaStatus
	Error     string
	FetchedAt time.Time
}

// Chat is a conversation summary derived from the messages table.
type Chat struct {
	ChatJID         string    `json:"chat_jid"`
	LastMessageAt   time.Time `json:"last_message_at"`
	LastMessageBody string    `json:"last_message_body"`
	LastDirection   Direction `json:"last_direction"`
	MessageCount    int       `json:"message_count"`
}
