package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// newTestDB opens a throwaway database on disk and runs the migrations.
func newTestDB(t *testing.T) *store.DB {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("Close() error = %v", err)
		}
	})

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return db
}

func mustTenant(t *testing.T, db *store.DB, id, name string) store.Tenant {
	t.Helper()
	tenant := store.Tenant{ID: id, Name: name, CreatedAt: time.Now().UTC()}
	if err := db.Tenants().Create(context.Background(), tenant); err != nil {
		t.Fatalf("Tenants().Create(%q) error = %v", id, err)
	}
	return tenant
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
}

// TestMigrationsDoNotCollideWithWhatsmeow guards the shared database file: our
// bookkeeping table must not be named like whatsmeow's own schema table.
func TestMigrationsUseOwnNamespace(t *testing.T) {
	db := newTestDB(t)

	var name string
	err := db.SQL().QueryRowContext(context.Background(),
		`SELECT name FROM sqlite_master WHERE type='table' AND name='wc_schema_migrations'`).Scan(&name)
	if err != nil {
		t.Fatalf("expected the wc_schema_migrations table to exist: %v", err)
	}

	var count int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'whatsmeow%'`).Scan(&count); err != nil {
		t.Fatalf("query error = %v", err)
	}
	if count != 0 {
		t.Errorf("our migrations created %d whatsmeow_* tables; whatsmeow must own its own schema", count)
	}
}

func TestTenantCreateAndGet(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()

	created := mustTenant(t, db, "tenant-a", "Acme Corp")

	got, err := db.Tenants().Get(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ID != created.ID || got.Name != created.Name {
		t.Errorf("Get() = %+v, want id %q name %q", got, created.ID, created.Name)
	}
	if got.CreatedAt.IsZero() {
		t.Error("CreatedAt should be persisted")
	}
}

func TestTenantGetUnknownReturnsNotFound(t *testing.T) {
	db := newTestDB(t)

	_, err := db.Tenants().Get(context.Background(), "missing")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

func TestTenantCreateDuplicateIDFails(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme Corp")

	err := db.Tenants().Create(context.Background(), store.Tenant{ID: "tenant-a", Name: "Other", CreatedAt: time.Now()})
	if err == nil {
		t.Fatal("creating a duplicate tenant id should fail")
	}
}

func TestTenantList(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")

	got, err := db.Tenants().List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("List() returned %d tenants, want 2", len(got))
	}
}

func TestAPIKeyCreateLookupAndRevoke(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")

	key := store.APIKey{
		ID:        "key-1",
		TenantID:  "tenant-a",
		Name:      "web platform",
		KeyHash:   "hash-1",
		KeyPrefix: "wc_live_tenanta_abc",
		Scopes:    []string{"messages:send"},
		CreatedAt: time.Now().UTC(),
	}
	if err := db.APIKeys().Create(ctx, key); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	got, err := db.APIKeys().GetByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetByHash() error = %v", err)
	}
	if got.ID != "key-1" || got.TenantID != "tenant-a" {
		t.Errorf("GetByHash() = %+v, want key-1 / tenant-a", got)
	}
	if len(got.Scopes) != 1 || got.Scopes[0] != "messages:send" {
		t.Errorf("Scopes = %#v, want [messages:send]", got.Scopes)
	}
	if got.LastUsedAt != nil {
		t.Error("a fresh key should have no LastUsedAt")
	}

	if err := db.APIKeys().Revoke(ctx, "tenant-a", "key-1"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	if _, err := db.APIKeys().GetByHash(ctx, "hash-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByHash() after revoke error = %v, want ErrNotFound", err)
	}
}

func TestAPIKeyRevokeIsScopedToOwningTenant(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")

	if err := db.APIKeys().Create(ctx, store.APIKey{
		ID: "key-1", TenantID: "tenant-a", Name: "n", KeyHash: "hash-1",
		KeyPrefix: "p", Scopes: []string{"messages:send"}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Tenant B must not be able to revoke tenant A's key.
	if err := db.APIKeys().Revoke(ctx, "tenant-b", "key-1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("cross-tenant Revoke() error = %v, want ErrNotFound", err)
	}
	if _, err := db.APIKeys().GetByHash(ctx, "hash-1"); err != nil {
		t.Errorf("the key must still be live after a cross-tenant revoke attempt: %v", err)
	}
}

func TestAPIKeyMarkUsed(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")

	if err := db.APIKeys().Create(ctx, store.APIKey{
		ID: "key-1", TenantID: "tenant-a", Name: "n", KeyHash: "hash-1",
		KeyPrefix: "p", Scopes: []string{"messages:send"}, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	if err := db.APIKeys().MarkUsed(ctx, "key-1"); err != nil {
		t.Fatalf("MarkUsed() error = %v", err)
	}

	got, err := db.APIKeys().GetByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetByHash() error = %v", err)
	}
	if got.LastUsedAt == nil {
		t.Fatal("LastUsedAt should be set after MarkUsed")
	}
}

func TestAPIKeyListByTenantIncludesRevoked(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")

	for _, k := range []store.APIKey{
		{ID: "key-1", TenantID: "tenant-a", Name: "live", KeyHash: "h1", KeyPrefix: "p1", Scopes: []string{"messages:send"}, CreatedAt: time.Now().UTC()},
		{ID: "key-2", TenantID: "tenant-a", Name: "dead", KeyHash: "h2", KeyPrefix: "p2", Scopes: []string{"messages:read"}, CreatedAt: time.Now().UTC()},
		{ID: "key-3", TenantID: "tenant-b", Name: "other", KeyHash: "h3", KeyPrefix: "p3", Scopes: []string{"messages:send"}, CreatedAt: time.Now().UTC()},
	} {
		if err := db.APIKeys().Create(ctx, k); err != nil {
			t.Fatalf("Create(%s) error = %v", k.ID, err)
		}
	}
	if err := db.APIKeys().Revoke(ctx, "tenant-a", "key-2"); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}

	got, err := db.APIKeys().ListByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListByTenant(tenant-a) returned %d keys, want 2 (a revoked key stays visible for auditing)", len(got))
	}
	for _, k := range got {
		if k.TenantID != "tenant-a" {
			t.Errorf("ListByTenant leaked a key of tenant %q", k.TenantID)
		}
	}
}

// TestAPIKeySurvivesSessionRePairing is the invariant the whole design rests on:
// API keys belong to the TENANT, not to the WhatsApp session. Re-pairing a
// number changes only sessions.status and must never touch a key.
func TestAPIKeySurvivesSessionRePairing(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")

	key := store.APIKey{
		ID: "key-1", TenantID: "tenant-a", Name: "web platform", KeyHash: "hash-1",
		KeyPrefix: "wc_live_tenanta_abc", Scopes: []string{"messages:send"}, CreatedAt: time.Now().UTC(),
	}
	if err := db.APIKeys().Create(ctx, key); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	before, err := db.APIKeys().GetByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("GetByHash() error = %v", err)
	}

	if _, err := db.Sessions().EnsureForTenant(ctx, "tenant-a"); err != nil {
		t.Fatalf("EnsureForTenant() error = %v", err)
	}

	// A full pair / logout / re-pair cycle.
	transitions := []struct {
		status store.SessionStatus
		waJID  string
	}{
		{store.SessionConnected, "5215550001111@s.whatsapp.net"},
		{store.SessionLoggedOut, ""},
		{store.SessionPending, ""},
		{store.SessionConnected, "5215550002222@s.whatsapp.net"},
	}
	for _, tr := range transitions {
		if err := db.Sessions().UpdateStatus(ctx, "tenant-a", tr.status, tr.waJID); err != nil {
			t.Fatalf("UpdateStatus(%s) error = %v", tr.status, err)
		}
	}

	after, err := db.APIKeys().GetByHash(ctx, "hash-1")
	if err != nil {
		t.Fatalf("the api key must still resolve after re-pairing: %v", err)
	}
	if after.ID != before.ID || after.KeyHash != before.KeyHash || after.TenantID != before.TenantID {
		t.Errorf("api key changed across re-pairing: before %+v, after %+v", before, after)
	}
	if after.RevokedAt != nil {
		t.Error("re-pairing must never revoke an api key")
	}

	session, err := db.Sessions().GetByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("GetByTenant() error = %v", err)
	}
	if session.Status != store.SessionConnected {
		t.Errorf("session status = %q, want %q", session.Status, store.SessionConnected)
	}
	if session.WAJID == nil || *session.WAJID != "5215550002222@s.whatsapp.net" {
		t.Errorf("session wa_jid = %v, want the newly paired jid", session.WAJID)
	}
}

func TestSessionEnsureForTenantIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")

	first, err := db.Sessions().EnsureForTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("EnsureForTenant() error = %v", err)
	}
	if first.Status != store.SessionPending {
		t.Errorf("a new session starts as %q, got %q", store.SessionPending, first.Status)
	}

	second, err := db.Sessions().EnsureForTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("second EnsureForTenant() error = %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("EnsureForTenant created a second session: %q then %q", first.ID, second.ID)
	}
}

func TestSessionStatusTransitions(t *testing.T) {
	tests := []struct {
		name    string
		status  store.SessionStatus
		waJID   string
		wantErr bool
	}{
		{"pending", store.SessionPending, "", false},
		{"connected with jid", store.SessionConnected, "5215550001111@s.whatsapp.net", false},
		{"disconnected", store.SessionDisconnected, "", false},
		{"logged out", store.SessionLoggedOut, "", false},
		{"unknown status is rejected", store.SessionStatus("exploded"), "", true},
		{"empty status is rejected", store.SessionStatus(""), "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			ctx := context.Background()
			mustTenant(t, db, "tenant-a", "Acme")
			if _, err := db.Sessions().EnsureForTenant(ctx, "tenant-a"); err != nil {
				t.Fatalf("EnsureForTenant() error = %v", err)
			}

			err := db.Sessions().UpdateStatus(ctx, "tenant-a", tc.status, tc.waJID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("UpdateStatus(%q) error = %v, wantErr %v", tc.status, err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}

			got, err := db.Sessions().GetByTenant(ctx, "tenant-a")
			if err != nil {
				t.Fatalf("GetByTenant() error = %v", err)
			}
			if got.Status != tc.status {
				t.Errorf("status = %q, want %q", got.Status, tc.status)
			}
		})
	}
}

// TestSessionUpdateStatusKeepsKnownJIDWhenBlank verifies a disconnect does not
// erase which number the tenant is paired to.
func TestSessionUpdateStatusKeepsKnownJIDWhenBlank(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")
	if _, err := db.Sessions().EnsureForTenant(ctx, "tenant-a"); err != nil {
		t.Fatalf("EnsureForTenant() error = %v", err)
	}

	const jid = "5215550001111@s.whatsapp.net"
	if err := db.Sessions().UpdateStatus(ctx, "tenant-a", store.SessionConnected, jid); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}
	if err := db.Sessions().UpdateStatus(ctx, "tenant-a", store.SessionDisconnected, ""); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	got, err := db.Sessions().GetByTenant(ctx, "tenant-a")
	if err != nil {
		t.Fatalf("GetByTenant() error = %v", err)
	}
	if got.WAJID == nil || *got.WAJID != jid {
		t.Errorf("wa_jid = %v, want it preserved as %q", got.WAJID, jid)
	}
}

func TestSessionGetUnknownTenantReturnsNotFound(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Sessions().GetByTenant(context.Background(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetByTenant() error = %v, want ErrNotFound", err)
	}
}

func seedMessages(t *testing.T, db *store.DB) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	messages := []store.Message{
		{ID: "m1", TenantID: "tenant-a", ChatJID: "chat-1", SenderJID: "chat-1", WAMessageID: "wa1", Direction: store.DirectionIn, Body: "hello from one", Timestamp: base},
		{ID: "m2", TenantID: "tenant-a", ChatJID: "chat-1", SenderJID: "me", WAMessageID: "wa2", Direction: store.DirectionOut, Body: "reply to one", Timestamp: base.Add(1 * time.Minute)},
		{ID: "m3", TenantID: "tenant-a", ChatJID: "chat-1", SenderJID: "chat-1", WAMessageID: "wa3", Direction: store.DirectionIn, Body: "latest in one", Timestamp: base.Add(2 * time.Minute)},
		{ID: "m4", TenantID: "tenant-a", ChatJID: "chat-2", SenderJID: "chat-2", WAMessageID: "wa4", Direction: store.DirectionIn, Body: "hello from two", Timestamp: base.Add(30 * time.Second)},
		{ID: "m5", TenantID: "tenant-b", ChatJID: "chat-1", SenderJID: "chat-1", WAMessageID: "wa5", Direction: store.DirectionIn, Body: "secret of tenant b", Timestamp: base.Add(3 * time.Minute)},
	}
	for _, m := range messages {
		m.CreatedAt = time.Now().UTC()
		if err := db.Messages().Append(ctx, m); err != nil {
			t.Fatalf("Append(%s) error = %v", m.ID, err)
		}
	}
}

func TestMessageListByChatIsNewestFirst(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)

	got, err := db.Messages().ListByChat(context.Background(), "tenant-a", "chat-1", 10)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}

	want := []string{"m3", "m2", "m1"}
	if len(got) != len(want) {
		t.Fatalf("ListByChat() returned %d messages, want %d", len(got), len(want))
	}
	for i, id := range want {
		if got[i].ID != id {
			t.Errorf("message[%d] = %q, want %q (newest first)", i, got[i].ID, id)
		}
	}
}

func TestMessageListByChatLimit(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)

	tests := []struct {
		name  string
		limit int
		want  int
	}{
		{"limit below total", 2, 2},
		{"limit above total", 50, 3},
		{"zero limit falls back to the default", 0, 3},
		{"negative limit falls back to the default", -1, 3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Messages().ListByChat(context.Background(), "tenant-a", "chat-1", tc.limit)
			if err != nil {
				t.Fatalf("ListByChat() error = %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("ListByChat(limit=%d) returned %d, want %d", tc.limit, len(got), tc.want)
			}
		})
	}
}

// TestMessageListByChatIsTenantIsolated is the storage half of the
// multi-tenant guarantee: tenant A and tenant B share a chat_jid but never see
// each other's rows.
func TestMessageListByChatIsTenantIsolated(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)

	got, err := db.Messages().ListByChat(context.Background(), "tenant-a", "chat-1", 100)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}
	for _, m := range got {
		if m.TenantID != "tenant-a" {
			t.Errorf("tenant-a query returned a row of tenant %q", m.TenantID)
		}
		if m.Body == "secret of tenant b" {
			t.Error("tenant-a read a message belonging to tenant-b")
		}
	}
}

func TestMessageListChats(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)

	got, err := db.Messages().ListChats(context.Background(), "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListChats() returned %d chats, want 2", len(got))
	}
	if got[0].ChatJID != "chat-1" {
		t.Errorf("chats[0] = %q, want chat-1 (most recent activity first)", got[0].ChatJID)
	}
	if got[0].LastMessageBody != "latest in one" {
		t.Errorf("chats[0].LastMessageBody = %q, want %q", got[0].LastMessageBody, "latest in one")
	}
	if got[0].MessageCount != 3 {
		t.Errorf("chats[0].MessageCount = %d, want 3", got[0].MessageCount)
	}
	if got[1].ChatJID != "chat-2" {
		t.Errorf("chats[1] = %q, want chat-2", got[1].ChatJID)
	}
}

func TestMessageAppendValidatesDirection(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")

	err := db.Messages().Append(context.Background(), store.Message{
		ID: "bad", TenantID: "tenant-a", ChatJID: "chat-1", SenderJID: "chat-1",
		WAMessageID: "wax", Direction: store.Direction("sideways"), Body: "x",
		Timestamp: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Fatal("Append() with an unknown direction should fail")
	}
}

func TestMessageAppendIsIdempotentPerWAMessageID(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")

	m := store.Message{
		ID: "m1", TenantID: "tenant-a", ChatJID: "chat-1", SenderJID: "chat-1",
		WAMessageID: "wa1", Direction: store.DirectionIn, Body: "hello",
		Timestamp: time.Now().UTC(), CreatedAt: time.Now().UTC(),
	}
	if err := db.Messages().Append(ctx, m); err != nil {
		t.Fatalf("first Append() error = %v", err)
	}

	// WhatsApp can redeliver the same message; a retry must not duplicate it.
	m.ID = "m1-retry"
	if err := db.Messages().Append(ctx, m); err != nil {
		t.Fatalf("duplicate Append() should be a no-op, got error = %v", err)
	}

	got, err := db.Messages().ListByChat(ctx, "tenant-a", "chat-1", 10)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}
	if len(got) != 1 {
		t.Errorf("stored %d copies of the same wa_message_id, want 1", len(got))
	}
}

func TestMessageSearch(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)

	tests := []struct {
		name     string
		tenantID string
		query    string
		wantIDs  []string
	}{
		{"matches a single message", "tenant-a", "latest", []string{"m3"}},
		{"case insensitive", "tenant-a", "LATEST", []string{"m3"}},
		{"matches several, newest first", "tenant-a", "hello", []string{"m4", "m1"}},
		{"no match", "tenant-a", "zzzz", nil},
		{"never crosses tenants", "tenant-a", "secret of tenant b", nil},
		{"empty query matches nothing", "tenant-a", "", nil},
		{"wildcard characters are treated literally", "tenant-a", "%", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Messages().Search(context.Background(), tc.tenantID, tc.query, 10)
			if err != nil {
				t.Fatalf("Search() error = %v", err)
			}
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("Search(%q) returned %d results, want %d", tc.query, len(got), len(tc.wantIDs))
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id {
					t.Errorf("result[%d] = %q, want %q", i, got[i].ID, id)
				}
			}
		})
	}
}

func TestNewIDIsUnique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := store.NewID()
		if id == "" {
			t.Fatal("NewID() returned an empty id")
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("NewID() collided after %d iterations", i)
		}
		seen[id] = struct{}{}
	}
}

// TestMessageAppendBatchIsIdempotent is the dedup contract between a pushed
// history sync and the live message stream: both deliver the same
// wa_message_id, and the unique (tenant_id, wa_message_id) index must make the
// second delivery a no-op.
func TestMessageAppendBatchIsIdempotent(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	base := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	batch := []store.Message{
		{TenantID: "tenant-a", ChatJID: "chat-h", SenderJID: "chat-h", WAMessageID: "h1", Direction: store.DirectionIn, Body: "one", Timestamp: base},
		{TenantID: "tenant-a", ChatJID: "chat-h", SenderJID: "me", WAMessageID: "h2", Direction: store.DirectionOut, Body: "two", Timestamp: base.Add(time.Minute)},
	}

	inserted, err := db.Messages().AppendBatch(ctx, batch)
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	if inserted != 2 {
		t.Fatalf("first AppendBatch() inserted %d, want 2", inserted)
	}

	countRows := func() int {
		t.Helper()
		var n int
		if err := db.SQL().QueryRowContext(ctx,
			`SELECT count(*) FROM messages WHERE tenant_id = 'tenant-a'`).Scan(&n); err != nil {
			t.Fatalf("count error = %v", err)
		}
		return n
	}
	afterFirst := countRows()

	inserted, err = db.Messages().AppendBatch(ctx, batch)
	if err != nil {
		t.Fatalf("second AppendBatch() error = %v", err)
	}
	if inserted != 0 {
		t.Errorf("second AppendBatch() inserted %d, want 0", inserted)
	}
	if got := countRows(); got != afterFirst {
		t.Errorf("row count = %d after replaying the batch, want %d", got, afterFirst)
	}
}

// TestMessageAppendBatchDedupsAgainstLiveMessages proves a history row cannot
// duplicate a message the live event stream already stored.
func TestMessageAppendBatchDedupsAgainstLiveMessages(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	at := time.Date(2026, 3, 1, 8, 0, 0, 0, time.UTC)
	if err := db.Messages().Append(ctx, store.Message{
		TenantID: "tenant-a", ChatJID: "chat-h", SenderJID: "chat-h",
		WAMessageID: "live-1", Direction: store.DirectionIn, Body: "live", Timestamp: at,
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	inserted, err := db.Messages().AppendBatch(ctx, []store.Message{
		{TenantID: "tenant-a", ChatJID: "chat-h", SenderJID: "chat-h", WAMessageID: "live-1", Direction: store.DirectionIn, Body: "live", Timestamp: at},
		{TenantID: "tenant-a", ChatJID: "chat-h", SenderJID: "chat-h", WAMessageID: "hist-1", Direction: store.DirectionIn, Body: "older", Timestamp: at.Add(-time.Hour)},
	})
	if err != nil {
		t.Fatalf("AppendBatch() error = %v", err)
	}
	if inserted != 1 {
		t.Errorf("AppendBatch() inserted %d, want 1 (the live message was already stored)", inserted)
	}
}

func TestMessageAppendBatchRejectsInvalidRows(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	tests := []struct {
		name string
		msg  store.Message
	}{
		{"no tenant", store.Message{ChatJID: "c", WAMessageID: "w", Direction: store.DirectionIn}},
		{"no chat", store.Message{TenantID: "tenant-a", WAMessageID: "w", Direction: store.DirectionIn}},
		{"no wa message id", store.Message{TenantID: "tenant-a", ChatJID: "c", Direction: store.DirectionIn}},
		{"bad direction", store.Message{TenantID: "tenant-a", ChatJID: "c", WAMessageID: "w", Direction: "sideways"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Messages().AppendBatch(ctx, []store.Message{tc.msg}); err == nil {
				t.Error("AppendBatch() accepted an invalid row")
			}
		})
	}
}

// TestMessageOldestByChat covers the backfill anchor: on-demand history is
// requested relative to the OLDEST message we already hold for the chat.
func TestMessageOldestByChat(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)
	ctx := context.Background()

	got, err := db.Messages().OldestByChat(ctx, "tenant-a", "chat-1")
	if err != nil {
		t.Fatalf("OldestByChat() error = %v", err)
	}
	if got.ID != "m1" {
		t.Errorf("OldestByChat() = %q, want m1 (the oldest of chat-1)", got.ID)
	}

	if _, err := db.Messages().OldestByChat(ctx, "tenant-a", "chat-unknown"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OldestByChat() on an empty chat error = %v, want ErrNotFound", err)
	}

	// A chat another tenant owns is an empty chat as far as this tenant knows.
	if _, err := db.Messages().OldestByChat(ctx, "tenant-b", "chat-2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OldestByChat() across tenants error = %v, want ErrNotFound", err)
	}
}

// TestMessageSearchChats is the helper behind the contact lookup tool: it maps
// a free-text query onto the chats we already store something for.
func TestMessageSearchChats(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)
	ctx := context.Background()

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"matches a chat jid", "chat-1", []string{"chat-1"}},
		{"matches every chat of the tenant", "chat", []string{"chat-1", "chat-2"}},
		{"matches a sender jid", "me", []string{"chat-1"}},
		{"no match", "nobody", nil},
		{"empty query returns nothing", "", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Messages().SearchChats(ctx, "tenant-a", tc.query, 0)
			if err != nil {
				t.Fatalf("SearchChats() error = %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("SearchChats(%q) = %v, want %v", tc.query, got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("SearchChats(%q)[%d] = %q, want %q", tc.query, i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestMessageSearchChatsIsTenantIsolated keeps the contact helper inside the
// same tenant boundary as every other read.
func TestMessageSearchChatsIsTenantIsolated(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)

	got, err := db.Messages().SearchChats(context.Background(), "tenant-b", "chat", 0)
	if err != nil {
		t.Fatalf("SearchChats() error = %v", err)
	}
	if len(got) != 1 || got[0] != "chat-1" {
		t.Errorf("SearchChats() for tenant-b = %v, want only its own chat-1", got)
	}
}
