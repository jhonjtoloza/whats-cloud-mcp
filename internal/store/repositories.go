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
type Messages interface {
	// Append stores a message. A repeated (tenant_id, wa_message_id) pair is a
	// no-op, because WhatsApp redelivers messages.
	Append(ctx context.Context, m Message) error
	// ListByChat returns the most recent messages of a chat, newest first.
	ListByChat(ctx context.Context, tenantID, chatJID string, limit int) ([]Message, error)
	// ListChats returns conversation summaries ordered by most recent activity.
	ListChats(ctx context.Context, tenantID string, limit int) ([]Chat, error)
	// Search performs a substring match over message bodies.
	//
	// This is a LIKE scan on purpose. FTS5 would be the right long-term answer,
	// but its support in the pure-Go driver is unverified and switching to a
	// cgo driver would break the static build. Keeping search behind this
	// method means FTS5 can replace the implementation without touching callers.
	Search(ctx context.Context, tenantID, query string, limit int) ([]Message, error)
}
