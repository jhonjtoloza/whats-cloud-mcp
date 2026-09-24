// Package wa wraps the WhatsApp engine (go.mau.fi/whatsmeow) behind a narrow
// interface so the HTTP layer can be tested without a live socket.
//
// whatsmeow is MPL-2.0 and is used strictly as a dependency: nothing in this
// repository modifies or vendors its source.
package wa

import (
	"context"
	"errors"
	"time"
)

// Common failures the HTTP layer maps onto status codes.
var (
	// ErrNotPaired means the tenant has no usable WhatsApp session yet.
	ErrNotPaired = errors.New("wa: tenant is not paired")
	// ErrUnknownTenant means no session record exists for the tenant.
	ErrUnknownTenant = errors.New("wa: unknown tenant")
	// ErrInvalidJID means the destination could not be parsed.
	ErrInvalidJID = errors.New("wa: invalid jid")
	// ErrNoAnchorMessage means the chat has no stored message to backfill from.
	//
	// This is a real constraint of WhatsApp's on-demand history, not a gap in
	// this gateway: the request asks for the messages immediately BEFORE a
	// message the phone can identify, so a conversation we hold nothing for
	// cannot be addressed at all. The chat has to show up in a pushed history
	// sync, or receive a live message, before it can be backfilled.
	ErrNoAnchorMessage = errors.New("wa: the chat has no stored message to anchor a history request; it must appear in a pushed history sync or receive a message first")
	// ErrSyncInProgress means another backfill of the same chat is still
	// waiting for its reply.
	ErrSyncInProgress = errors.New("wa: a history sync for this chat is already in progress")
	// ErrSyncTimeout means the phone did not answer the history request in
	// time. The request may still be answered later, and the messages will be
	// stored when it is.
	ErrSyncTimeout = errors.New("wa: timed out waiting for the phone to answer the history request")
)

// Sync request sizing. WhatsApp recommends asking for 50 messages at a time;
// the maximum is ours, to keep one call from pulling an unbounded chunk.
const (
	// DefaultSyncCount is used when a caller asks for no particular number.
	DefaultSyncCount = 50
	// MaxSyncCount caps a single backfill request.
	MaxSyncCount = 200
)

// NormalizeSyncCount clamps a requested backfill size into the supported range.
func NormalizeSyncCount(count int) int {
	switch {
	case count <= 0:
		return DefaultSyncCount
	case count > MaxSyncCount:
		return MaxSyncCount
	default:
		return count
	}
}

// Contact is a candidate destination resolved from the tenant's address book.
type Contact struct {
	JID  string `json:"jid"`
	Name string `json:"name"`
	// HasMessages says whether the gateway already stores messages for this
	// chat. It is what tells a caller whether it can read the conversation
	// straight away or has to backfill it first.
	HasMessages bool `json:"has_messages"`
}

// SyncResult reports what an on-demand backfill brought in.
type SyncResult struct {
	ChatJID  string `json:"chat_jid"`
	Inserted int    `json:"inserted"`
	// OldestTimestamp is the timestamp of the oldest message now stored for the
	// chat, which is where the next backfill would continue from.
	OldestTimestamp time.Time `json:"oldest_timestamp"`
	// MoreAvailable says whether asking again is likely to yield more history.
	MoreAvailable bool `json:"more_available"`
}

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

// SentMessage is what one send actually produced.
//
// It exists because the send is the only place that knows these values. The
// caller holds a string it typed, which may be a bare number or a LID; the
// address the conversation is filed under, the tenant's own JID and the moment
// WhatsApp recorded are all decided inside the send, and persisting anything
// else files the outgoing half of a conversation away from the incoming half.
type SentMessage struct {
	// WAMessageID is the id WhatsApp assigned to the message.
	WAMessageID string
	// ChatJID is the conversation the message belongs to, canonicalised the
	// same way the inbound paths canonicalise theirs and always non-AD. A LID
	// that nothing can resolve keeps its own address rather than being given an
	// invented one.
	ChatJID string
	// SenderJID is the tenant's own address, non-AD. The tenant's JID carries a
	// device suffix ("573114276555:87@s.whatsapp.net"), and a row written with
	// it can never be matched against the plain address.
	SenderJID string
	// Timestamp is the time WhatsApp recorded for the message, not the moment
	// the gateway happened to call.
	Timestamp time.Time
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
	// SendText sends a plain text message and reports what was sent: the
	// WhatsApp message id, the address the conversation is filed under, the
	// tenant's own address and the timestamp WhatsApp recorded.
	SendText(ctx context.Context, tenantID, toJID, body string) (SentMessage, error)
	// Logout unlinks the tenant's device and drops the client.
	Logout(ctx context.Context, tenantID string) error
	// FindContacts resolves a free-text query against the tenant's address
	// book, reporting for each candidate whether messages are already stored.
	FindContacts(ctx context.Context, tenantID, query string) ([]Contact, error)
	// SyncHistory asks the phone for the messages preceding the oldest message
	// stored for a chat, waits for them, and reports what landed.
	//
	// It returns ErrNoAnchorMessage when the chat has nothing stored to anchor
	// the request on.
	SyncHistory(ctx context.Context, tenantID, chatJID string, count int) (SyncResult, error)
	// FetchMedia downloads the attachment of one message, on demand.
	//
	// Media is never downloaded in advance — not on receipt, not on history
	// sync, not in a background job — because eagerly storing every image,
	// video and sticker would fill a small shared server with bytes nobody
	// reads. A message row keeps the reference; this is the only thing that
	// turns one into a file.
	//
	// It is idempotent: media already on disk is returned untouched. The
	// tradeoff of storing a reference is that WhatsApp expires media
	// server-side, so it returns ErrMediaUnavailable for an attachment that is
	// gone, and never retries one.
	FetchMedia(ctx context.Context, tenantID, messageID string) (MediaRef, error)
}
