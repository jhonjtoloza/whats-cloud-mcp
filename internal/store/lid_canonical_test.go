package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// The two addresses WhatsApp gives one person, plus a LID nobody can resolve.
const (
	peerPN      = "573004725680@s.whatsapp.net"
	peerLID     = "15285906083900@lid"
	orphanLID   = "99999999999999@lid"
	ownPN       = "573114276555@s.whatsapp.net"
	ownDeviceAD = "573114276555:87@s.whatsapp.net"
	groupJID    = "120363000000000000@g.us"
)

// mustLIDMap reproduces whatsmeow's LID index.
//
// The real table is created by whatsmeow's own schema upgrade, which never runs
// in a store test, so the fixture creates it exactly as whatsmeow does. Both
// columns hold the BARE USER PART of an address, never a full JID, and the
// repository has to know that.
func mustLIDMap(t *testing.T, db *store.DB, byLID map[string]string) {
	t.Helper()
	ctx := context.Background()

	if _, err := db.SQL().ExecContext(ctx, `CREATE TABLE IF NOT EXISTS whatsmeow_lid_map (
		lid TEXT PRIMARY KEY,
		pn  TEXT UNIQUE NOT NULL
	)`); err != nil {
		t.Fatalf("create whatsmeow_lid_map: %v", err)
	}
	for lid, pn := range byLID {
		if _, err := db.SQL().ExecContext(ctx,
			`INSERT INTO whatsmeow_lid_map (lid, pn) VALUES (?, ?)`, lid, pn); err != nil {
			t.Fatalf("insert lid mapping %s -> %s: %v", lid, pn, err)
		}
	}
}

// mustAppend stores one message, failing the test rather than the caller.
func mustAppend(t *testing.T, db *store.DB, m store.Message) {
	t.Helper()
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	if err := db.Messages().Append(context.Background(), m); err != nil {
		t.Fatalf("Append(%s) error = %v", m.ID, err)
	}
}

// seedSplitConversation stores the exact shape the bug produced: one
// conversation whose older half is filed under the phone number and whose newer
// half is filed under the LID.
func seedSplitConversation(t *testing.T, db *store.DB) {
	t.Helper()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	mustAppend(t, db, store.Message{
		ID: "m-pn", TenantID: "tenant-a", ChatJID: peerPN, SenderJID: peerPN,
		WAMessageID: "wa-pn", Direction: store.DirectionIn, Body: "older half", Timestamp: base,
	})
	mustAppend(t, db, store.Message{
		ID: "m-lid", TenantID: "tenant-a", ChatJID: peerLID, SenderJID: peerLID,
		WAMessageID: "wa-lid", Direction: store.DirectionIn, Body: "newer half",
		Timestamp: base.Add(time.Hour),
	})
}

// TestListChatsMergesTheTwoAddressesOfOnePerson is the regression test for the
// silent half of the incident: nothing ever errored, a conversation simply
// appeared twice and a query by phone number returned the older row.
//
// whatsmeow_lid_map lives in this very same SQLite file, so the repository can
// fold the two addresses together with a JOIN and needs no live client.
func TestListChatsMergesTheTwoAddressesOfOnePerson(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustLIDMap(t, db, map[string]string{"15285906083900": "573004725680"})
	seedSplitConversation(t, db)

	chats, err := db.Messages().ListChats(context.Background(), "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}

	if len(chats) != 1 {
		t.Fatalf("ListChats() returned %d conversations, want 1: %+v", len(chats), chats)
	}
	if chats[0].ChatJID != peerPN {
		t.Errorf("ChatJID = %q, want the canonical phone-number form %q", chats[0].ChatJID, peerPN)
	}
	if chats[0].MessageCount != 2 {
		t.Errorf("MessageCount = %d, want both halves counted as 2", chats[0].MessageCount)
	}
	if chats[0].LastMessageBody != "newer half" {
		t.Errorf("LastMessageBody = %q, want the newest message of the merged conversation",
			chats[0].LastMessageBody)
	}
}

// TestListByChatFindsBothAddressForms is the other half of the same failure: a
// query by either address must return the whole conversation, never the part
// that happens to be filed under the address that was asked for.
func TestListByChatFindsBothAddressForms(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustLIDMap(t, db, map[string]string{"15285906083900": "573004725680"})
	seedSplitConversation(t, db)

	tests := []struct {
		name  string
		query string
	}{
		{"asked for by phone number", peerPN},
		{"asked for by lid", peerLID},
		{"asked for with a device suffix", "573004725680:12@s.whatsapp.net"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Messages().ListByChat(context.Background(), "tenant-a", tc.query, 10)
			if err != nil {
				t.Fatalf("ListByChat(%q) error = %v", tc.query, err)
			}
			if len(got) != 2 {
				t.Fatalf("ListByChat(%q) returned %d messages, want both halves", tc.query, len(got))
			}
			if got[0].ID != "m-lid" || got[1].ID != "m-pn" {
				t.Errorf("ListByChat(%q) = [%s %s], want [m-lid m-pn] newest first",
					tc.query, got[0].ID, got[1].ID)
			}
		})
	}
}

// TestOldestByChatFindsBothAddressForms keeps backfill anchored on the merged
// conversation. An anchor taken from only half of a split chat would ask
// WhatsApp for history the gateway already holds.
func TestOldestByChatFindsBothAddressForms(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustLIDMap(t, db, map[string]string{"15285906083900": "573004725680"})
	seedSplitConversation(t, db)

	for _, query := range []string{peerPN, peerLID} {
		got, err := db.Messages().OldestByChat(context.Background(), "tenant-a", query)
		if err != nil {
			t.Fatalf("OldestByChat(%q) error = %v", query, err)
		}
		if got.ID != "m-pn" {
			t.Errorf("OldestByChat(%q) = %q, want the oldest of the merged conversation m-pn",
				query, got.ID)
		}
	}
}

// TestSearchChatsMergesTheTwoAddresses covers the contact lookup: find_contact
// searches by phone number, and a conversation whose rows are filed under a LID
// must still be found, once, under its canonical address.
func TestSearchChatsMergesTheTwoAddresses(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustLIDMap(t, db, map[string]string{"15285906083900": "573004725680"})
	seedSplitConversation(t, db)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"by phone number", "573004725680", []string{peerPN}},
		{"by lid", "15285906083900", []string{peerPN}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Messages().SearchChats(context.Background(), "tenant-a", tc.query, 0)
			if err != nil {
				t.Fatalf("SearchChats(%q) error = %v", tc.query, err)
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

// TestUnresolvableLIDKeepsItsOwnConversation is the no-data-loss guarantee.
//
// One chat in the live database has no mapping at all. An address that cannot
// be resolved must be left exactly as it is: a chat of its own that still
// answers for its own messages. Inventing a phone number for it, or dropping
// it, would both be worse than the split it came from.
func TestUnresolvableLIDKeepsItsOwnConversation(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")
	mustLIDMap(t, db, map[string]string{"15285906083900": "573004725680"})
	seedSplitConversation(t, db)
	mustAppend(t, db, store.Message{
		ID: "m-orphan", TenantID: "tenant-a", ChatJID: orphanLID, SenderJID: orphanLID,
		WAMessageID: "wa-orphan", Direction: store.DirectionIn, Body: "unmapped",
		Timestamp: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC),
	})

	chats, err := db.Messages().ListChats(ctx, "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("ListChats() returned %d conversations, want the merged one plus the unmapped one", len(chats))
	}

	messages, err := db.Messages().ListByChat(ctx, "tenant-a", orphanLID, 10)
	if err != nil {
		t.Fatalf("ListByChat(%q) error = %v", orphanLID, err)
	}
	if len(messages) != 1 || messages[0].ID != "m-orphan" {
		t.Fatalf("ListByChat(%q) = %+v, want only m-orphan", orphanLID, messages)
	}
	if messages[0].ChatJID != orphanLID {
		t.Errorf("ChatJID = %q, want the raw lid kept as it is", messages[0].ChatJID)
	}
}

// TestGroupChatKeepsItsOwnAddress guards the one address that must never be
// rewritten: a group is not a person and has no phone-number form.
func TestGroupChatKeepsItsOwnAddress(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")
	mustLIDMap(t, db, map[string]string{"15285906083900": "573004725680"})
	mustAppend(t, db, store.Message{
		ID: "m-group", TenantID: "tenant-a", ChatJID: groupJID, SenderJID: peerPN,
		WAMessageID: "wa-group", Direction: store.DirectionIn, Body: "in the group",
		Timestamp: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
	})

	chats, err := db.Messages().ListChats(ctx, "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}
	if len(chats) != 1 || chats[0].ChatJID != groupJID {
		t.Fatalf("ListChats() = %+v, want the group kept at %q", chats, groupJID)
	}

	messages, err := db.Messages().ListByChat(ctx, "tenant-a", groupJID, 10)
	if err != nil {
		t.Fatalf("ListByChat(%q) error = %v", groupJID, err)
	}
	if len(messages) != 1 {
		t.Fatalf("ListByChat(%q) returned %d messages, want 1", groupJID, len(messages))
	}
}

// TestCanonicalLookupsWorkWithoutTheLIDIndex keeps the repository honest on a
// database whatsmeow has not upgraded yet: our own migrations run first, so the
// LID index may simply not be there, and every lookup must still answer.
func TestCanonicalLookupsWorkWithoutTheLIDIndex(t *testing.T) {
	db := newTestDB(t)
	ctx := context.Background()
	mustTenant(t, db, "tenant-a", "Acme")
	seedSplitConversation(t, db)

	chats, err := db.Messages().ListChats(ctx, "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}
	if len(chats) != 2 {
		t.Fatalf("ListChats() returned %d conversations, want the two unmerged halves", len(chats))
	}

	messages, err := db.Messages().ListByChat(ctx, "tenant-a", peerPN, 10)
	if err != nil {
		t.Fatalf("ListByChat(%q) error = %v", peerPN, err)
	}
	if len(messages) != 1 || messages[0].ID != "m-pn" {
		t.Errorf("ListByChat(%q) = %+v, want only m-pn", peerPN, messages)
	}

	if _, err := db.Messages().OldestByChat(ctx, "tenant-a", orphanLID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("OldestByChat(%q) error = %v, want ErrNotFound", orphanLID, err)
	}
}
