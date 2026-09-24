package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// canonicalMigration is the migration under test. Re-running it means deleting
// its ledger row, which is also how its idempotency is proven.
const canonicalMigration = "0003_canonical_addresses.sql"

// messageRow is the part of a row this migration is allowed to touch.
type messageRow struct {
	ID        string
	ChatJID   string
	SenderJID string
	Direction string
}

// seedCorruptedMessages reproduces the four shapes the live database ended up
// in: a conversation split across both addresses, a chat whose LID nothing can
// resolve, a message the tenant sent that was filed as received, and a group
// whose participant is addressed by LID.
func seedCorruptedMessages(t *testing.T, db *store.DB) {
	t.Helper()
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	seeded := []store.Message{
		{
			ID: "m-pn", TenantID: "tenant-a", ChatJID: peerPN, SenderJID: peerPN,
			WAMessageID: "wa-pn", Direction: store.DirectionIn, Body: "older half",
			Timestamp: base,
		},
		{
			ID: "m-lid", TenantID: "tenant-a", ChatJID: peerLID, SenderJID: peerLID,
			WAMessageID: "wa-lid", Direction: store.DirectionIn, Body: "newer half",
			Timestamp: base.Add(time.Hour),
		},
		{
			// Sent by the tenant from their own phone, stored as received, and
			// carrying the device suffix that defeats every string comparison.
			ID: "m-own", TenantID: "tenant-a", ChatJID: peerLID, SenderJID: ownDeviceAD,
			WAMessageID: "wa-own", Direction: store.DirectionIn, Body: "sent by the tenant",
			Timestamp: base.Add(2 * time.Hour),
		},
		{
			ID: "m-orphan", TenantID: "tenant-a", ChatJID: orphanLID, SenderJID: orphanLID,
			WAMessageID: "wa-orphan", Direction: store.DirectionIn, Body: "unmapped",
			Timestamp: base.Add(3 * time.Hour),
		},
		{
			ID: "m-group", TenantID: "tenant-a", ChatJID: groupJID, SenderJID: peerLID,
			WAMessageID: "wa-group", Direction: store.DirectionIn, Body: "in the group",
			Timestamp: base.Add(4 * time.Hour),
		},
	}
	for _, m := range seeded {
		mustAppend(t, db, m)
	}
}

// rerunCanonicalMigration drops the migration's ledger row and migrates again,
// which is the only way to replay a data migration over a fixture.
func rerunCanonicalMigration(t *testing.T, db *store.DB) error {
	t.Helper()
	ctx := context.Background()

	res, err := db.SQL().ExecContext(ctx,
		`DELETE FROM wc_schema_migrations WHERE name = ?`, canonicalMigration)
	if err != nil {
		t.Fatalf("delete migration ledger row: %v", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		t.Fatalf("rows affected: %v", err)
	}
	if affected != 1 {
		t.Fatalf("expected %s to be recorded in wc_schema_migrations", canonicalMigration)
	}
	return db.Migrate(ctx)
}

// readMessageRows returns every row's addressing, ordered so two runs compare.
func readMessageRows(t *testing.T, db *store.DB) map[string]messageRow {
	t.Helper()

	rows, err := db.SQL().QueryContext(context.Background(),
		`SELECT id, chat_jid, sender_jid, direction FROM messages ORDER BY id`)
	if err != nil {
		t.Fatalf("read message rows: %v", err)
	}
	defer func() { _ = rows.Close() }()

	out := make(map[string]messageRow)
	for rows.Next() {
		var r messageRow
		if err := rows.Scan(&r.ID, &r.ChatJID, &r.SenderJID, &r.Direction); err != nil {
			t.Fatalf("scan message row: %v", err)
		}
		out[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read message rows: %v", err)
	}
	return out
}

// newCorruptedDB builds the database exactly as the bug left it, with the
// migration rolled back so the test can watch it run.
func newCorruptedDB(t *testing.T) *store.DB {
	t.Helper()
	ctx := context.Background()

	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustLIDMap(t, db, map[string]string{"15285906083900": "573004725680"})

	if _, err := db.Sessions().EnsureForTenant(ctx, "tenant-a"); err != nil {
		t.Fatalf("EnsureForTenant() error = %v", err)
	}
	// The tenant's own address carries a device: "573114276555:87@...".
	if err := db.Sessions().UpdateStatus(ctx, "tenant-a", store.SessionConnected, ownDeviceAD); err != nil {
		t.Fatalf("UpdateStatus() error = %v", err)
	}

	seedCorruptedMessages(t, db)
	return db
}

// TestCanonicalMigrationRepointsAddressesAndDirection is the repair half of the
// fix. The write path stops new rows from splitting; this is what puts the rows
// already written back onto one address.
func TestCanonicalMigrationRepointsAddressesAndDirection(t *testing.T) {
	db := newCorruptedDB(t)

	if err := rerunCanonicalMigration(t, db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	want := map[string]messageRow{
		"m-pn":     {ID: "m-pn", ChatJID: peerPN, SenderJID: peerPN, Direction: "in"},
		"m-lid":    {ID: "m-lid", ChatJID: peerPN, SenderJID: peerPN, Direction: "in"},
		"m-own":    {ID: "m-own", ChatJID: peerPN, SenderJID: ownPN, Direction: "out"},
		"m-orphan": {ID: "m-orphan", ChatJID: orphanLID, SenderJID: orphanLID, Direction: "in"},
		"m-group":  {ID: "m-group", ChatJID: groupJID, SenderJID: peerPN, Direction: "in"},
	}

	got := readMessageRows(t, db)
	if len(got) != len(want) {
		t.Fatalf("migration left %d rows, want %d", len(got), len(want))
	}
	for id, expected := range want {
		if got[id] != expected {
			t.Errorf("row %s = %+v, want %+v", id, got[id], expected)
		}
	}
}

// TestCanonicalMigrationMergesTheSplitConversation states the outcome the
// operator actually cares about, rather than the columns it took to get there.
func TestCanonicalMigrationMergesTheSplitConversation(t *testing.T) {
	db := newCorruptedDB(t)

	if err := rerunCanonicalMigration(t, db); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	chats, err := db.Messages().ListChats(context.Background(), "tenant-a", 10)
	if err != nil {
		t.Fatalf("ListChats() error = %v", err)
	}

	counts := make(map[string]int, len(chats))
	for _, c := range chats {
		counts[c.ChatJID] = c.MessageCount
	}
	if counts[peerPN] != 3 {
		t.Errorf("conversation %q holds %d messages, want the 3 halves merged: %+v",
			peerPN, counts[peerPN], chats)
	}
	if counts[orphanLID] != 1 {
		t.Errorf("conversation %q holds %d messages, want its single message kept",
			orphanLID, counts[orphanLID])
	}
	if counts[groupJID] != 1 {
		t.Errorf("conversation %q holds %d messages, want the group untouched",
			groupJID, counts[groupJID])
	}
}

// TestCanonicalMigrationIsIdempotent is what makes the migration safe to ship:
// a second run must change nothing, because a half-finished deploy, a restore
// or a manual replay all end up running it twice.
func TestCanonicalMigrationIsIdempotent(t *testing.T) {
	db := newCorruptedDB(t)

	if err := rerunCanonicalMigration(t, db); err != nil {
		t.Fatalf("first Migrate() error = %v", err)
	}
	first := readMessageRows(t, db)

	if err := rerunCanonicalMigration(t, db); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	second := readMessageRows(t, db)

	if len(first) != len(second) {
		t.Fatalf("second run left %d rows, want %d", len(second), len(first))
	}
	for id, row := range first {
		if second[id] != row {
			t.Errorf("row %s changed on the second run: %+v, want %+v", id, second[id], row)
		}
	}
}

// TestCanonicalMigrationRunsOnADatabaseWithoutTheLIDIndex covers a fresh
// install: our migrations run before whatsmeow upgrades its own schema, so
// whatsmeow_lid_map does not exist yet. A fresh database has nothing to
// canonicalise, and the migration must say so rather than fail.
func TestCanonicalMigrationRunsOnADatabaseWithoutTheLIDIndex(t *testing.T) {
	db := newTestDB(t)

	var applied int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT count(*) FROM wc_schema_migrations WHERE name = ?`, canonicalMigration).Scan(&applied); err != nil {
		t.Fatalf("query error = %v", err)
	}
	if applied != 1 {
		t.Fatalf("%s did not run on a database without the lid index", canonicalMigration)
	}

	var stand int
	if err := db.SQL().QueryRowContext(context.Background(),
		`SELECT count(*) FROM sqlite_master WHERE name LIKE 'whatsmeow%'`).Scan(&stand); err != nil {
		t.Fatalf("query error = %v", err)
	}
	if stand != 0 {
		t.Errorf("our migrations left %d whatsmeow_* objects behind; whatsmeow owns its own schema", stand)
	}
}

// TestCanonicalMigrationRefusesDuplicateMessageIDs pins the guard.
//
// Repointing chat_jid is only safe while every WhatsApp message exists once per
// tenant, which the unique index on (tenant_id, wa_message_id) guarantees. A
// database where that index is gone could merge two rows into one address and
// silently lose one, so the migration checks the assumption itself and aborts
// loudly instead of mutating.
func TestCanonicalMigrationRefusesDuplicateMessageIDs(t *testing.T) {
	db := newCorruptedDB(t)
	ctx := context.Background()

	if _, err := db.SQL().ExecContext(ctx, `DROP INDEX idx_messages_tenant_wa_id`); err != nil {
		t.Fatalf("drop unique index: %v", err)
	}
	if _, err := db.SQL().ExecContext(ctx,
		`INSERT INTO messages (id, tenant_id, chat_jid, sender_jid, wa_message_id, direction, body, timestamp, created_at)
		 VALUES ('m-dup', 'tenant-a', ?, ?, 'wa-pn', 'in', 'duplicate', ?, ?)`,
		peerLID, peerLID, time.Now().UTC(), time.Now().UTC()); err != nil {
		t.Fatalf("insert duplicate message: %v", err)
	}

	before := readMessageRows(t, db)

	err := rerunCanonicalMigration(t, db)
	if err == nil {
		t.Fatal("Migrate() succeeded over duplicate wa_message_ids, want it to abort")
	}
	if !strings.Contains(err.Error(), canonicalMigration) {
		t.Errorf("Migrate() error = %v, want it to name %s", err, canonicalMigration)
	}

	after := readMessageRows(t, db)
	for id, row := range before {
		if after[id] != row {
			t.Errorf("row %s was mutated by an aborted migration: %+v, want %+v", id, after[id], row)
		}
	}
}
