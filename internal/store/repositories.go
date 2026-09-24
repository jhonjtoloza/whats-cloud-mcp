package store

import "context"

// Tenants persists tenants.
type Tenants interface {
	Create(ctx context.Context, t Tenant) error
	Get(ctx context.Context, id string) (Tenant, error)
	List(ctx context.Context) ([]Tenant, error)
}

// APIKeys persists tenant credentials.
type APIKeys interface {
	Create(ctx context.Context, k APIKey) error
	// GetByHash returns the live key with the given SHA-256 hash, or
	// ErrNotFound when it is unknown or revoked.
	GetByHash(ctx context.Context, keyHash string) (APIKey, error)
	// ListByTenant returns every key of a tenant, revoked ones included, so an
	// operator can audit what was issued.
	ListByTenant(ctx context.Context, tenantID string) ([]APIKey, error)
	// Revoke marks a key revoked. tenantID scopes the operation so a key can
	// only ever be revoked through its owning tenant.
	Revoke(ctx context.Context, tenantID, keyID string) error
	MarkUsed(ctx context.Context, keyID string) error
}

// Sessions persists WhatsApp pairing state, one row per tenant.
type Sessions interface {
	// EnsureForTenant returns the tenant's session, creating a pending one on
	// first call.
	EnsureForTenant(ctx context.Context, tenantID string) (Session, error)
	GetByTenant(ctx context.Context, tenantID string) (Session, error)
	// UpdateStatus changes only the session lifecycle. An empty waJID leaves
	// the currently known JID untouched. This never touches api_keys.
	UpdateStatus(ctx context.Context, tenantID string, status SessionStatus, waJID string) error
	List(ctx context.Context) ([]Session, error)
}

// Messages persists the message log.
//
// Every method that takes or reports a chat address speaks ONE address per
// conversation. WhatsApp addresses the same person two ways, by phone number
// and by LID, and a repository that passed those through would let list_chats,
// list_messages and the contact lookup disagree about how many conversations
// exist. Addresses are folded together through whatsmeow's LID index, which
// lives in this same database file, so no live WhatsApp client is involved; an
// address the index does not know keeps its own identity rather than being
// guessed at.
type Messages interface {
	// Append stores a message. A repeated (tenant_id, wa_message_id) pair is a
	// no-op, because WhatsApp redelivers messages.
	Append(ctx context.Context, m Message) error
	// AppendBatch stores many messages in one transaction and reports how many
	// rows were actually new.
	//
	// It exists for history sync, which delivers whole conversations at once
	// and overlaps heavily with what the live event stream already stored. The
	// same (tenant_id, wa_message_id) uniqueness makes a replay a no-op, so the
	// returned count is the honest "new history" number.
	AppendBatch(ctx context.Context, messages []Message) (int, error)
	// GetByID returns one message of a tenant, addressed by the gateway's own
	// id or by the WhatsApp message id, or ErrNotFound when neither matches.
	//
	// The tenant is part of the lookup rather than a check afterwards, so an id
	// belonging to another tenant is indistinguishable from one that never
	// existed.
	GetByID(ctx context.Context, tenantID, id string) (Message, error)
	// UpdateMedia records the outcome of a media fetch. It writes the media
	// bookkeeping columns and nothing else: a failed download must never lose
	// the message or its download reference.
	UpdateMedia(ctx context.Context, tenantID, id string, upd MediaUpdate) error
	// OldestByChat returns the oldest stored message of a chat, or ErrNotFound
	// when the chat has none.
	//
	// This is the anchor an on-demand history request is built from: WhatsApp
	// backfills the messages immediately BEFORE a known message, so a chat with
	// nothing stored cannot be backfilled at all. The anchor is taken from the
	// whole conversation, not from the half filed under the address that was
	// passed, or the gateway would keep asking for history it already holds.
	OldestByChat(ctx context.Context, tenantID, chatJID string) (Message, error)
	// SearchChats returns the chat JIDs whose address or sender matches the
	// query, most recent activity first.
	//
	// It answers "do we already hold messages for this contact?", which is what
	// the contact lookup tool needs before deciding whether to backfill.
	SearchChats(ctx context.Context, tenantID, query string, limit int) ([]string, error)
	// ListByChat returns the most recent messages of a chat, newest first.
	//
	// The chat may be named by either of its addresses, with or without a
	// device suffix; all of them read the same conversation.
	ListByChat(ctx context.Context, tenantID, chatJID string, limit int) ([]Message, error)
	// ListChats returns conversation summaries ordered by most recent activity,
	// one per person rather than one per address.
	ListChats(ctx context.Context, tenantID string, limit int) ([]Chat, error)
	// Search performs a substring match over message bodies.
	//
	// This is a LIKE scan on purpose. FTS5 would be the right long-term answer,
	// but its support in the pure-Go driver is unverified and switching to a
	// cgo driver would break the static build. Keeping search behind this
	// method means FTS5 can replace the implementation without touching callers.
	Search(ctx context.Context, tenantID, query string, limit int) ([]Message, error)
}

// Chats is the display name of a conversation, cached from WhatsApp.
//
// It exists because a group is only addressable by a numeric JID: WhatsApp
// keeps the subject in the group metadata, whatsmeow fetches it live and
// persists none of it, so without this table nothing could answer "the group
// called obd2ip". Names for people are NOT written here — the address book
// already carries those, and duplicating them would mean two answers to the
// same question.
//
// Every name is a cache of what WhatsApp owns. It is replaced wholesale on
// every reading rather than merged, and a conversation with no row simply has
// no name, which leaves the caller with the JID it already had.
type Chats interface {
	// Upsert stores the current name of one conversation, replacing whatever
	// was there. A blank name is ignored rather than stored.
	Upsert(ctx context.Context, tenantID string, chat NamedChat) error
	// UpsertBatch stores many names in one transaction. It is the shape the
	// reconnect backfill needs, which reads every joined group at once.
	UpsertBatch(ctx context.Context, tenantID string, chats []NamedChat) error
	// Search returns the conversations of one tenant whose name contains the
	// query, matched case-insensitively and literally, ordered by name.
	Search(ctx context.Context, tenantID, query string, limit int) ([]NamedChat, error)
}
