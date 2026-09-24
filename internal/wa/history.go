package wa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/config"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// A pushed history sync can carry thousands of messages across hundreds of
// conversations, which is far more than the ten seconds an ordinary event gets.
const historySyncPersistTimeout = 5 * time.Minute

// historyDecision says how much of one conversation a history sync stores.
type historyDecision int

const (
	// historySkipChat stores nothing at all for the conversation.
	historySkipChat historyDecision = iota
	// historyAnchorOnly stores just the most recent message.
	historyAnchorOnly
	// historyPersistAll stores everything the sync delivered.
	historyPersistAll
)

// historyDecisionFor applies HISTORY_SYNC_SCOPE to one chat.
//
// The scope is a volume control, not a filter on existence. A chat type outside
// the scope is reduced to its single most recent message rather than dropped,
// because on-demand backfill asks WhatsApp for the messages immediately BEFORE
// a message we already know: a conversation with zero stored rows has no anchor
// and can never be backfilled. Dropping group history entirely would therefore
// not mean "less group history for now", it would mean "no group history, ever".
//
// Two chat types are skipped outright:
//
//   - Status updates (types.StatusBroadcastJID). They expire after 24 hours and
//     are a one-to-many feed rather than a conversation, so there is nothing to
//     anchor and nothing anybody asks an assistant to read back.
//   - Newsletters, i.e. channels, unless the scope is "all". They are
//     broadcast publications with no reply from this account; keeping their
//     anchor would only invite backfilling a feed the user never wrote in.
func historyDecisionFor(scope config.HistorySyncScope, chat types.JID) historyDecision {
	if chat.Server == types.BroadcastServer && chat.User == types.StatusBroadcastJID.User {
		return historySkipChat
	}

	switch chat.Server {
	case types.NewsletterServer:
		if scope == config.HistorySyncScopeAll {
			return historyPersistAll
		}
		return historySkipChat

	case types.GroupServer:
		if scope == config.HistorySyncScopeDMGroup || scope == config.HistorySyncScopeAll {
			return historyPersistAll
		}
		return historyAnchorOnly

	case types.DefaultUserServer, types.HiddenUserServer:
		// Direct conversations are the point of the gateway and are stored in
		// full under every scope.
		return historyPersistAll

	default:
		// Broadcast lists and anything WhatsApp adds later. Unknown volume, so
		// only "all" takes it wholesale; everything else keeps the anchor and
		// leaves the decision to backfill to the operator.
		if scope == config.HistorySyncScopeAll {
			return historyPersistAll
		}
		return historyAnchorOnly
	}
}

// historyRecords maps the messages of one history-sync conversation onto rows.
//
// History arrives as waWeb.WebMessageInfo, which is a different shape from the
// events.Message the live stream delivers: the addressing lives in a
// waCommon.MessageKey and the timestamp is a Unix second count. That is why
// this is a separate path rather than a detour through persistMessage.
//
// Messages that cannot be addressed or identified are dropped rather than
// failing the batch: one malformed row in a chunk of a thousand must not cost
// the other 999.
func historyRecords(
	tenantID string,
	ownJID types.JID,
	chat types.JID,
	decision historyDecision,
	messages []*waHistorySync.HistorySyncMsg,
) []store.Message {
	if decision == historySkipChat || len(messages) == 0 {
		return nil
	}

	now := time.Now().UTC()
	records := make([]store.Message, 0, len(messages))
	for _, entry := range messages {
		info := entry.GetMessage()
		key := info.GetKey()
		if key == nil || key.GetID() == "" {
			continue
		}

		record := store.Message{
			ID:          store.NewID(),
			TenantID:    tenantID,
			ChatJID:     chat.String(),
			SenderJID:   historySender(ownJID, chat, key.GetFromMe(), key.GetParticipant()),
			WAMessageID: key.GetID(),
			Direction:   store.DirectionIn,
			Body:        extractText(info.GetMessage()),
			Timestamp:   time.Unix(int64(info.GetMessageTimestamp()), 0).UTC(),
			CreatedAt:   now,
		}
		if key.GetFromMe() {
			record.Direction = store.DirectionOut
		}
		// The reference, never the bytes: a history sync can deliver thousands
		// of attachments at once, and downloading them here would be the exact
		// eager fetch this design refuses.
		applyMediaReference(&record, mediaReference(info.GetMessage()))
		records = append(records, record)
	}

	if decision == historyAnchorOnly {
		return newestRecord(records)
	}
	return records
}

// newestRecord reduces a conversation to its most recent message, which is the
// anchor a later on-demand backfill is built from.
func newestRecord(records []store.Message) []store.Message {
	if len(records) == 0 {
		return nil
	}
	newest := 0
	for i := range records {
		if records[i].Timestamp.After(records[newest].Timestamp) {
			newest = i
		}
	}
	return []store.Message{records[newest]}
}

// historySender resolves who sent a history message.
//
// A message the tenant sent carries no participant, so it is attributed to the
// tenant's own JID. An incoming group message names its participant; an
// incoming direct message does not, and the chat itself is the sender.
func historySender(ownJID, chat types.JID, fromMe bool, participant string) string {
	if fromMe {
		return ownJID.String()
	}
	if participant != "" {
		return participant
	}
	return chat.String()
}

// historyMediaType names the attachment a history message carries, or "" when
// it carries none.
//
// The live path gets this for free in events.Message.Info.MediaType. History
// messages have no such field, so the type is derived from the payload, using
// the same vocabulary whatsmeow puts on the live info ("image", "ptt", "gif",
// ...) so both paths store comparable values.
//
// It is a thin reading of mediaReference on purpose: the type and the download
// reference are decided by one function, which is the only way the two
// persistence paths can be kept from drifting apart.
func historyMediaType(msg *waProto.Message) string {
	return mediaReference(msg).MediaType
}

// persistHistorySync stores a pushed history sync.
//
// Message bodies are never logged; only the shape of the sync is.
func (m *Manager) persistHistorySync(ctx context.Context, tenantID string, e *events.HistorySync) {
	data := e.Data
	if data == nil {
		return
	}

	syncType := data.GetSyncType()
	ownJID := m.ownJID(tenantID)
	lids := m.lidsFor(tenantID)
	conversations := data.GetConversations()

	inserted := 0
	for _, conv := range conversations {
		raw, err := types.ParseJID(conv.GetID())
		if err != nil || raw.User == "" {
			m.logger.WarnContext(ctx, "history sync carried an unusable conversation id",
				slog.String("tenant_id", tenantID),
				slog.String("sync_type", syncType.String()))
			continue
		}
		// History is a write path too. Storing the address the sync happened to
		// use would file the same conversation under a second address all over
		// again, undoing the merge for every chat the phone pushes back.
		chat, chatCanonical := canonicalJID(ctx, lids, raw, types.EmptyJID)
		m.countUnresolved(ctx, tenantID, chatCanonical, true)

		records := historyRecords(tenantID, ownJID, chat,
			historyDecisionFor(m.historyScope, chat), conv.GetMessages())
		if len(records) == 0 {
			continue
		}
		m.canonicaliseSenders(ctx, lids, tenantID, records)

		added, err := m.messages.AppendBatch(ctx, records)
		if err != nil {
			m.logger.ErrorContext(ctx, "could not persist a history sync conversation",
				slog.String("tenant_id", tenantID),
				slog.String("chat_jid", chat.String()),
				slog.String("error", err.Error()))
			continue
		}
		inserted += added

		if syncType == waHistorySync.HistorySync_ON_DEMAND {
			m.deliverOnDemand(ctx, tenantID, chat, added, len(records))
		}
	}

	m.logger.InfoContext(ctx, "history sync stored",
		slog.String("tenant_id", tenantID),
		slog.String("sync_type", syncType.String()),
		slog.Int("conversations", len(conversations)),
		slog.Int("messages_inserted", inserted))
}

// canonicaliseSenders resolves the participant addresses of a history batch.
//
// A history message key carries one participant and no alternative address, so
// the LID index is the only source here; an address it does not know is kept
// exactly as it arrived.
//
// The addresses are re-parsed rather than resolved inside historyRecords so
// that mapping a history message onto a row stays a pure function of the event,
// which is what makes it testable without a client.
func (m *Manager) canonicaliseSenders(
	ctx context.Context, lids lidResolver, tenantID string, records []store.Message,
) {
	for i := range records {
		sender, err := types.ParseJID(records[i].SenderJID)
		if err != nil || sender.IsEmpty() {
			continue
		}
		canonical, ok := canonicalJID(ctx, lids, sender, types.EmptyJID)
		records[i].SenderJID = canonical.String()
		m.countUnresolved(ctx, tenantID, true, ok)
	}
}

// deliverOnDemand wakes the SyncHistory call that asked for this chat, if one
// is still waiting.
func (m *Manager) deliverOnDemand(ctx context.Context, tenantID string, chat types.JID, inserted, delivered int) {
	delivery := historyDelivery{Inserted: inserted, Delivered: delivered}
	if oldest, err := m.messages.OldestByChat(ctx, tenantID, chat.String()); err == nil {
		delivery.Oldest = oldest.Timestamp
	}
	m.pending.deliver(historyKey(tenantID, chat.String()), delivery)
}

// SyncHistory asks the user's phone for the messages that precede the oldest
// message we already store for a chat, then waits for them to arrive.
//
// The wait is unavoidable: the request is a peer message and its answer comes
// back later as an unrelated *events.HistorySync, so the reply is matched to
// this call through the pending registry.
func (m *Manager) SyncHistory(ctx context.Context, tenantID, chatJID string, count int) (SyncResult, error) {
	m.mu.RLock()
	client := m.clients[tenantID]
	m.mu.RUnlock()

	if client == nil || !client.IsLoggedIn() {
		return SyncResult{}, ErrNotPaired
	}

	chat, err := parseJID(chatJID)
	if err != nil {
		return SyncResult{}, err
	}
	count = NormalizeSyncCount(count)

	// The reply arrives as an unrelated event and is matched back to this call
	// by chat, so both ends have to name the conversation the same way. The
	// reply is canonicalised when it is stored, and a caller may hold either of
	// the two addresses, so the canonical form is the only one they can meet
	// on. The REQUEST keeps the address it was given: that one goes to
	// WhatsApp, which addressed the conversation in the first place.
	canonical, _ := canonicalJID(ctx, m.lidsFor(tenantID), chat, types.EmptyJID)

	anchor, err := m.messages.OldestByChat(ctx, tenantID, canonical.String())
	if errors.Is(err, store.ErrNotFound) {
		return SyncResult{}, ErrNoAnchorMessage
	}
	if err != nil {
		return SyncResult{}, err
	}

	key := historyKey(tenantID, canonical.String())
	replies, release, err := m.pending.register(key)
	if err != nil {
		return SyncResult{}, err
	}
	// Whatever ends this call — a reply, the deadline or a cancelled context —
	// takes the registry entry with it.
	defer release()

	request := client.BuildHistorySyncRequest(&types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			IsFromMe: anchor.Direction == store.DirectionOut,
		},
		ID:        anchor.WAMessageID,
		Timestamp: anchor.Timestamp,
	}, count)

	if _, err := client.SendPeerMessage(ctx, request); err != nil {
		return SyncResult{}, fmt.Errorf("wa: request history sync: %w", err)
	}

	timeout := time.NewTimer(m.historyTimeout)
	defer timeout.Stop()

	select {
	case delivery := <-replies:
		return SyncResult{
			ChatJID:         canonical.String(),
			Inserted:        delivery.Inserted,
			OldestTimestamp: delivery.Oldest,
			// The phone answers with at most the requested number of messages,
			// so a full batch is the signal that older history remains.
			MoreAvailable: delivery.Delivered >= count,
		}, nil
	case <-timeout.C:
		return SyncResult{}, ErrSyncTimeout
	case <-ctx.Done():
		return SyncResult{}, ctx.Err()
	}
}

// ownJID returns the tenant's own WhatsApp address, or the empty JID when the
// tenant has no connected client.
func (m *Manager) ownJID(tenantID string) types.JID {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if client := m.clients[tenantID]; client != nil && client.Store.ID != nil {
		return client.Store.ID.ToNonAD()
	}
	return types.EmptyJID
}
