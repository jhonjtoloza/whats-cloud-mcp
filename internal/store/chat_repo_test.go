package store_test

import (
	"context"
	"testing"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

func TestChatNameIsFoundByAPartialCaseInsensitiveQuery(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	chats := []store.NamedChat{
		{ChatJID: "120363424550223300@g.us", Name: "obd2ip"},
		{ChatJID: "120363406613747745@g.us", Name: "Weekly planning"},
	}
	if err := db.Chats().UpsertBatch(ctx, "tenant-a", chats); err != nil {
		t.Fatalf("UpsertBatch() error = %v", err)
	}

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "exact", query: "obd2ip", want: "120363424550223300@g.us"},
		{name: "partial", query: "obd", want: "120363424550223300@g.us"},
		{name: "different case", query: "OBD2IP", want: "120363424550223300@g.us"},
		{name: "middle of the name", query: "planning", want: "120363406613747745@g.us"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Chats().Search(ctx, "tenant-a", tc.query, 10)
			if err != nil {
				t.Fatalf("Search(%q) error = %v", tc.query, err)
			}
			if len(got) != 1 {
				t.Fatalf("Search(%q) returned %d chats, want 1", tc.query, len(got))
			}
			if got[0].ChatJID != tc.want {
				t.Errorf("Search(%q) jid = %q, want %q", tc.query, got[0].ChatJID, tc.want)
			}
		})
	}
}

// TestChatNameSearchMatchesLiterally keeps a query carrying LIKE
// metacharacters from turning into a pattern that matches everything.
func TestChatNameSearchMatchesLiterally(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	if err := db.Chats().Upsert(ctx, "tenant-a", store.NamedChat{ChatJID: "1@g.us", Name: "obd2ip"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	got, err := db.Chats().Search(ctx, "tenant-a", "%", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Search(%q) returned %d chats, want 0", "%", len(got))
	}
}

// TestChatNameIsReplacedWhenTheGroupIsRenamed is why the name is refreshed on
// every reconnect and on every rename event: a stale name is worse than no
// name, because the caller has no way of telling that it is stale.
func TestChatNameIsReplacedWhenTheGroupIsRenamed(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	chat := store.NamedChat{ChatJID: "1@g.us", Name: "obd2ip"}
	if err := db.Chats().Upsert(ctx, "tenant-a", chat); err != nil {
		t.Fatalf("first Upsert() error = %v", err)
	}
	chat.Name = "obd2ip colombia"
	if err := db.Chats().Upsert(ctx, "tenant-a", chat); err != nil {
		t.Fatalf("second Upsert() error = %v", err)
	}

	got, err := db.Chats().Search(ctx, "tenant-a", "obd2ip", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search() returned %d chats, want 1", len(got))
	}
	if got[0].Name != "obd2ip colombia" {
		t.Errorf("Search() name = %q, want %q", got[0].Name, "obd2ip colombia")
	}
}

// TestChatNameSearchIsTenantIsolated is security invariant 2 applied to the
// new table: the name of another tenant's group must not even be visible.
func TestChatNameSearchIsTenantIsolated(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	ctx := context.Background()

	if err := db.Chats().Upsert(ctx, "tenant-b", store.NamedChat{ChatJID: "1@g.us", Name: "obd2ip"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	got, err := db.Chats().Search(ctx, "tenant-a", "obd2ip", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Search() returned %d chats of another tenant, want 0", len(got))
	}
}

// TestBlankChatNamesAreNotStored keeps a nameless group out of the table. An
// empty name stored is indistinguishable from a name that is genuinely empty,
// and it would replace the JID the reader falls back to.
func TestBlankChatNamesAreNotStored(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	named := store.NamedChat{ChatJID: "1@g.us", Name: "obd2ip"}
	if err := db.Chats().Upsert(ctx, "tenant-a", named); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	batch := []store.NamedChat{
		{ChatJID: "1@g.us", Name: "   "},
		{ChatJID: "2@g.us", Name: ""},
		{ChatJID: "3@g.us", Name: "kept"},
	}
	if err := db.Chats().UpsertBatch(ctx, "tenant-a", batch); err != nil {
		t.Fatalf("UpsertBatch() error = %v", err)
	}

	got, err := db.Chats().Search(ctx, "tenant-a", "obd2ip", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 || got[0].Name != "obd2ip" {
		t.Errorf("Search() = %+v, want the name to survive a blank upsert", got)
	}

	kept, err := db.Chats().Search(ctx, "tenant-a", "kept", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(kept) != 1 {
		t.Errorf("Search() returned %d chats, want the named one of the batch", len(kept))
	}
}

// TestChatNamesAreTrimmed keeps the stored name free of the padding WhatsApp
// sometimes carries, so a search for the visible name matches.
func TestChatNamesAreTrimmed(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	if err := db.Chats().Upsert(ctx, "tenant-a", store.NamedChat{ChatJID: "1@g.us", Name: "  obd2ip  "}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	got, err := db.Chats().Search(ctx, "tenant-a", "obd2ip", 10)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Search() returned %d chats, want 1", len(got))
	}
	if got[0].Name != "obd2ip" {
		t.Errorf("Search() name = %q, want %q", got[0].Name, "obd2ip")
	}
}

// TestListChatsReportsTheStoredName is the whole point of the table: the
// conversation listing stops being a wall of numeric JIDs.
func TestListChatsReportsTheStoredName(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)
	ctx := context.Background()

	if err := db.Chats().Upsert(ctx, "tenant-a", store.NamedChat{ChatJID: "chat-1", Name: "obd2ip"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	got, err := db.Messages().ListChats(ctx, "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}

	names := make(map[string]string, len(got))
	for _, chat := range got {
		names[chat.ChatJID] = chat.Name
	}
	if names["chat-1"] != "obd2ip" {
		t.Errorf("ListChats() name of chat-1 = %q, want %q", names["chat-1"], "obd2ip")
	}
	// A chat nobody named keeps an empty name so the caller can fall back to
	// the JID rather than being handed an invented one.
	if names["chat-2"] != "" {
		t.Errorf("ListChats() name of chat-2 = %q, want it empty", names["chat-2"])
	}
}

// TestListChatsIgnoresTheNameOfAnotherTenant is security invariant 2 on the
// read path: the join must be scoped by tenant, not by chat address alone.
func TestListChatsIgnoresTheNameOfAnotherTenant(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	seedMessages(t, db)
	ctx := context.Background()

	if err := db.Chats().Upsert(ctx, "tenant-b", store.NamedChat{ChatJID: "chat-1", Name: "secret of tenant b"}); err != nil {
		t.Fatalf("Upsert() error = %v", err)
	}

	got, err := db.Messages().ListChats(ctx, "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}
	for _, chat := range got {
		if chat.Name != "" {
			t.Errorf("ListChats() leaked the name %q of another tenant", chat.Name)
		}
	}
}
