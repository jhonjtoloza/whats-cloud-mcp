package wa

import (
	"context"
	"log/slog"
	"time"

	"go.mau.fi/whatsmeow/types"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// groupNameSyncTimeout bounds the reconnect backfill. It is a single round trip
// to WhatsApp plus one transaction, and it runs off the event goroutine, so a
// server that never answers must not hold a goroutine open for the life of the
// process.
const groupNameSyncTimeout = 30 * time.Second

// groupLister is the slice of the whatsmeow client the name sync needs.
//
// The manager takes the interface rather than *whatsmeow.Client so this path
// is testable: every other route into group metadata needs a live socket.
type groupLister interface {
	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
}

// syncGroupNames caches the current subject of every group the tenant is in.
//
// It runs on every successful connection rather than once at pairing, because
// the subject can change while the gateway is down and nothing would tell it
// afterwards. A name that silently goes stale is worse than no name at all:
// the caller cannot tell it is reading a group that has since been renamed.
func (m *Manager) syncGroupNames(ctx context.Context, tenantID string, client groupLister) error {
	groups, err := client.GetJoinedGroups(ctx)
	if err != nil {
		// Deliberately not swallowed into "no groups": that would wipe nothing
		// but would also leave the caller believing the cache is current.
		return err
	}

	named := make([]store.NamedChat, 0, len(groups))
	for _, group := range groups {
		if group == nil {
			continue
		}
		named = append(named, store.NamedChat{
			ChatJID: group.JID.ToNonAD().String(),
			Name:    group.Name,
		})
	}
	return m.chats.UpsertBatch(ctx, tenantID, named)
}

// syncGroupNamesInBackground runs the backfill off the event goroutine.
//
// whatsmeow delivers every event on one goroutine, so a round trip taken
// inline would stall the messages arriving behind it.
func (m *Manager) syncGroupNamesInBackground(tenantID string, client groupLister) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), groupNameSyncTimeout)
		defer cancel()

		if err := m.syncGroupNames(ctx, tenantID, client); err != nil {
			// A failure here costs readability, never data: the names already
			// cached stay, and the next reconnect tries again.
			m.logger.WarnContext(ctx, "could not refresh group names",
				slog.String("tenant_id", tenantID), slog.String("error", err.Error()))
		}
	}()
}

// rememberGroupName caches the name of one group as WhatsApp just reported it.
func (m *Manager) rememberGroupName(ctx context.Context, tenantID string, jid types.JID, name string) {
	chat := store.NamedChat{ChatJID: jid.ToNonAD().String(), Name: name}
	if err := m.chats.Upsert(ctx, tenantID, chat); err != nil {
		m.logger.ErrorContext(ctx, "could not store a group name",
			slog.String("tenant_id", tenantID),
			slog.String("chat_jid", chat.ChatJID),
			slog.String("error", err.Error()))
	}
}

// mergeNamedChats folds cached chat names into the contact candidates.
//
// The address book is the better source for a PERSON, so a real contact name
// is never overwritten. What does get replaced is the numeric placeholder the
// lookup falls back to when it knows an address but no name — which is exactly
// every group, and the reason this cache exists.
func mergeNamedChats(found map[string]Contact, named []store.NamedChat) {
	for _, chat := range named {
		existing, ok := found[chat.ChatJID]
		if ok && !isPlaceholderName(existing) {
			continue
		}
		existing.JID = chat.ChatJID
		existing.Name = chat.Name
		found[chat.ChatJID] = existing
	}
}

// isPlaceholderName reports whether a candidate carries the address as its
// name, which is what the lookup falls back to when no name is known.
func isPlaceholderName(contact Contact) bool {
	if contact.Name == contact.JID {
		return true
	}
	jid, err := types.ParseJID(contact.JID)
	if err != nil {
		return false
	}
	return contact.Name == jid.User
}
