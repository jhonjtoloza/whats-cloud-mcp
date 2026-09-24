package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// lidMapTable is whatsmeow's LID↔phone-number index.
//
// WhatsApp addresses the same person two ways: by phone number
// ("573004725680@s.whatsapp.net") and by LID ("15285906083900@lid"). Storing
// whichever one arrived splits one conversation in two, and a query by phone
// number then silently misses the newest messages.
//
// This table is what undoes that, and the reason the repository can do it at
// all: whatsmeow keeps it in the VERY SAME SQLite file as messages, so folding
// the two addresses together is a JOIN rather than a call into a live WhatsApp
// client. Both of its columns hold the bare user part of an address, never a
// full JID.
//
// whatsmeow owns this table. Nothing here ever writes to it.
const lidMapTable = "whatsmeow_lid_map"

// The two servers a person can be addressed on. A group, a broadcast list and a
// newsletter are not people and have no second address.
const (
	phoneServer = "s.whatsapp.net"
	lidServer   = "lid"
)

// canonicalAddressing carries the SQL that folds the two addresses of one
// person onto the phone-number form the gateway treats as canonical.
//
// On a database whatsmeow has not upgraded yet the LID index is simply absent —
// our own migrations run first — so every field degrades to the raw column and
// the queries keep working, one conversation per stored address.
type canonicalAddressing struct {
	chatJoin   string
	senderJoin string
	chat       string
	sender     string
}

// addressing builds the canonical projection for one query.
func (r *messageRepo) addressing(ctx context.Context) canonicalAddressing {
	if !r.lidMapReady(ctx) {
		return canonicalAddressing{chat: "messages.chat_jid", sender: "messages.sender_jid"}
	}
	return canonicalAddressing{
		chatJoin: ` LEFT JOIN ` + lidMapTable + ` AS chat_lid
		     ON messages.chat_jid = chat_lid.lid || '@` + lidServer + `'`,
		senderJoin: ` LEFT JOIN ` + lidMapTable + ` AS sender_lid
		     ON messages.sender_jid = sender_lid.lid || '@` + lidServer + `'`,
		chat:   `coalesce(chat_lid.pn || '@` + phoneServer + `', messages.chat_jid)`,
		sender: `coalesce(sender_lid.pn || '@` + phoneServer + `', messages.sender_jid)`,
	}
}

// chatAliases returns every address one conversation may be stored under.
//
// A caller holds whatever address it was handed: a phone number out of a
// contact list, a LID copied from an older listing, or either of the two
// carrying a device suffix. All of them have to reach the same rows, including
// rows written before the addresses were canonicalised.
//
// This is a set of exact addresses rather than a JOIN on purpose: an equality
// against the (tenant_id, chat_jid, timestamp) index is what keeps reading one
// conversation cheap, and a rewritten column would give that up for every
// query in order to help the few rows that are still split.
//
// An address nothing can resolve comes back alone, which is exactly right: it
// is a conversation of its own and still answers for its own messages.
func (r *messageRepo) chatAliases(ctx context.Context, chatJID string) []string {
	address := nonADAddress(chatJID)
	aliases := []string{address}

	user, server, ok := splitAddress(address)
	if !ok || !r.lidMapReady(ctx) {
		return aliases
	}

	// The lookup is symmetric: a LID is asked about by lid, a phone number by
	// pn, and anything else is not a person.
	var known, wanted, wantedServer string
	switch server {
	case lidServer:
		known, wanted, wantedServer = "lid", "pn", phoneServer
	case phoneServer:
		known, wanted, wantedServer = "pn", "lid", lidServer
	default:
		return aliases
	}

	var mapped string
	err := r.db.QueryRowContext(ctx,
		`SELECT `+wanted+` FROM `+lidMapTable+` WHERE `+known+` = ?`, user).Scan(&mapped)
	if err != nil || mapped == "" {
		// An unknown mapping is the normal state, not a failure: whatsmeow only
		// learns a pair once it has seen a message carrying both halves.
		return aliases
	}
	return append(aliases, mapped+"@"+wantedServer)
}

// lidMapReady reports whether whatsmeow's LID index exists in this database.
//
// It is re-checked until it is found and remembered afterwards: the gateway
// migrates its own schema BEFORE whatsmeow upgrades its own, so the table
// appears partway through startup, and whatsmeow never drops it again.
func (r *messageRepo) lidMapReady(ctx context.Context) bool {
	if r.lidMap.Load() {
		return true
	}
	present, err := tableExists(ctx, r.db, lidMapTable)
	if err != nil || !present {
		return false
	}
	r.lidMap.Store(true)
	return true
}

// splitAddress takes an address apart into the user it names and the server it
// lives on, dropping any device suffix on the way.
//
// The device is the third way one conversation splits. The tenant's own address
// is stored as "573114276555:87@s.whatsapp.net", so a comparison against the
// plain address misses it every time, which is why "is this my own message" is
// never decided on a raw string.
//
// It reports false for anything that is not an address, such as the bare chat
// keys older rows carry, so callers leave those exactly as they are.
func splitAddress(address string) (user, server string, ok bool) {
	at := strings.IndexByte(address, '@')
	if at <= 0 || at == len(address)-1 {
		return "", "", false
	}
	user, server = address[:at], address[at+1:]
	if device := strings.IndexByte(user, ':'); device >= 0 {
		user = user[:device]
	}
	if user == "" {
		return "", "", false
	}
	return user, server, true
}

// nonADAddress removes the device suffix from an address, leaving anything that
// is not an address untouched.
func nonADAddress(address string) string {
	user, server, ok := splitAddress(address)
	if !ok {
		return address
	}
	return user + "@" + server
}

// placeholders builds the "?, ?, ?" of an IN clause.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// tableExists reports whether a table of that name lives in the main schema.
func tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found int
	err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("store: look up table %s: %w", name, err)
	}
	return found > 0, nil
}
