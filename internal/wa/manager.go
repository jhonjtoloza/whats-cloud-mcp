package wa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	waStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/config"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// qrWaitTimeout bounds how long StartPairing waits for the first QR code.
const qrWaitTimeout = 20 * time.Second

// pairClientDisplayName is what the user sees in WhatsApp's linked-devices list.
//
// It MUST be formatted as "Browser (OS)" and match a browser and OS WhatsApp
// recognises: the server validates this field and rejects anything else with
// "info query returned status 400: bad-request". A product name such as
// "whats-cloud-mcp" fails pairing outright, so this is not a label we are free
// to brand. Keep it consistent with the PairClient* constant passed alongside
// it.
const pairClientDisplayName = "Chrome (Linux)"

// Manager is the whatsmeow-backed SessionManager. It holds one whatsmeow
// client per tenant, guarded by a mutex.
//
// This type is not unit tested on purpose: every meaningful path needs a live
// WhatsApp socket. Callers depend on the SessionManager interface instead, and
// httpapi tests run against a fake.
type Manager struct {
	container *sqlstore.Container
	sessions  store.Sessions
	messages  store.Messages
	logger    *slog.Logger
	waLogger  waLog.Logger

	historyScope   config.HistorySyncScope
	historyTimeout time.Duration
	pending        *pendingHistory
	media          *mediaFetcher

	// unresolvedAddresses counts the LIDs no alternative address and no lookup
	// could turn into a phone number. Those rows keep their raw address, so the
	// counter is the only sign that a conversation may still be split.
	unresolvedAddresses atomic.Uint64

	mu      sync.RWMutex
	clients map[string]*whatsmeow.Client
}

// ManagerOptions carries the history-sync settings the gateway was started
// with. The zero value is the safe one: direct messages only, and the default
// backfill timeout.
type ManagerOptions struct {
	// HistoryScope decides which chat types a pushed history sync is stored in
	// full for.
	HistoryScope config.HistorySyncScope
	// HistoryTimeout bounds how long SyncHistory waits for the phone.
	HistoryTimeout time.Duration
	// MediaDir is the root of the media tree. Each tenant gets a subdirectory.
	MediaDir string
	// MediaMaxBytes refuses attachments above this size.
	MediaMaxBytes int64
	// MediaFetchTypes are the media types FetchMedia may download. References
	// are stored for every type regardless; this only gates the bytes.
	MediaFetchTypes []string
}

// NewManager builds a Manager on top of the gateway's own database handle, so
// whatsmeow's schema lives in the very same SQLite file.
func NewManager(ctx context.Context, db *store.DB, logger *slog.Logger, opts ManagerOptions) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if opts.HistoryScope == "" {
		opts.HistoryScope = config.HistorySyncScopeDM
	}
	if opts.HistoryTimeout <= 0 {
		opts.HistoryTimeout = config.DefaultHistorySyncTimeout
	}
	if opts.MediaDir == "" {
		opts.MediaDir = config.DefaultMediaDir
	}
	if opts.MediaMaxBytes <= 0 {
		opts.MediaMaxBytes = config.DefaultMediaMaxBytes
	}
	if len(opts.MediaFetchTypes) == 0 {
		opts.MediaFetchTypes = config.DefaultMediaFetchTypes()
	}

	// dbutil recognises any dialect starting with "sqlite"; the driver itself
	// is the pure-Go modernc.org/sqlite registered as "sqlite".
	container := sqlstore.NewWithDB(db.SQL(), "sqlite", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("wa: upgrade whatsmeow schema: %w", err)
	}

	manager := &Manager{
		container:      container,
		sessions:       db.Sessions(),
		messages:       db.Messages(),
		logger:         logger,
		waLogger:       waLog.Noop,
		historyScope:   opts.HistoryScope,
		historyTimeout: opts.HistoryTimeout,
		pending:        newPendingHistory(),
		clients:        make(map[string]*whatsmeow.Client),
	}
	manager.media = &mediaFetcher{
		messages:      db.Messages(),
		dir:           opts.MediaDir,
		maxBytes:      opts.MediaMaxBytes,
		allowed:       allowedMediaTypes(opts.MediaFetchTypes),
		logger:        logger,
		downloaderFor: manager.downloaderFor,
	}
	return manager, nil
}

// RestoreSessions reconnects every tenant that is already paired. Call it once
// on boot, before serving traffic.
func (m *Manager) RestoreSessions(ctx context.Context) error {
	sessions, err := m.sessions.List(ctx)
	if err != nil {
		return fmt.Errorf("wa: list sessions: %w", err)
	}

	devices, err := m.container.GetAllDevices(ctx)
	if err != nil {
		return fmt.Errorf("wa: load whatsmeow devices: %w", err)
	}

	byJID := make(map[string]*waStore.Device, len(devices))
	for _, device := range devices {
		if device.ID != nil {
			byJID[device.ID.String()] = device
		}
	}

	for _, session := range sessions {
		if session.WAJID == nil || *session.WAJID == "" {
			continue
		}
		device, ok := byJID[*session.WAJID]
		if !ok {
			m.logger.WarnContext(ctx, "paired session has no whatsmeow device; it needs re-pairing",
				slog.String("tenant_id", session.TenantID), slog.String("wa_jid", *session.WAJID))
			continue
		}

		client := m.newClient(session.TenantID, device)
		m.mu.Lock()
		m.clients[session.TenantID] = client
		m.mu.Unlock()

		if err := client.Connect(); err != nil {
			m.logger.ErrorContext(ctx, "could not reconnect tenant",
				slog.String("tenant_id", session.TenantID), slog.String("error", err.Error()))
			continue
		}
		m.logger.InfoContext(ctx, "tenant session restored", slog.String("tenant_id", session.TenantID))
	}
	return nil
}

// StartPairing begins pairing for a tenant.
//
// A non-empty phone uses the pair-code flow, which is the preferred one: the
// user types an 8-character code on their phone. An empty phone falls back to
// the QR flow.
func (m *Manager) StartPairing(ctx context.Context, tenantID, phone string) (PairingResult, error) {
	client, err := m.clientFor(ctx, tenantID)
	if err != nil {
		return PairingResult{}, err
	}

	if client.IsLoggedIn() {
		return PairingResult{}, fmt.Errorf("wa: tenant %s is already paired; log out first", tenantID)
	}

	// Pairing only ever moves the session forward. API keys are untouched.
	if err := m.sessions.UpdateStatus(ctx, tenantID, store.SessionPending, ""); err != nil && !errors.Is(err, store.ErrNotFound) {
		return PairingResult{}, err
	}

	if phone != "" {
		return m.pairWithCode(ctx, client, phone)
	}
	return m.pairWithQR(ctx, client)
}

func (m *Manager) pairWithCode(ctx context.Context, client *whatsmeow.Client, phone string) (PairingResult, error) {
	if !client.IsConnected() {
		if err := client.Connect(); err != nil {
			return PairingResult{}, fmt.Errorf("wa: connect for pair code: %w", err)
		}
	}

	code, err := client.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, pairClientDisplayName)
	if err != nil {
		return PairingResult{}, fmt.Errorf("wa: request pair code: %w", err)
	}
	return PairingResult{Mode: PairingModeCode, PairCode: code}, nil
}

func (m *Manager) pairWithQR(ctx context.Context, client *whatsmeow.Client) (PairingResult, error) {
	// The QR channel must be obtained before connecting.
	qrChan, err := client.GetQRChannel(ctx)
	if err != nil {
		return PairingResult{}, fmt.Errorf("wa: open qr channel: %w", err)
	}
	if !client.IsConnected() {
		if err := client.Connect(); err != nil {
			return PairingResult{}, fmt.Errorf("wa: connect for qr: %w", err)
		}
	}

	timeout := time.NewTimer(qrWaitTimeout)
	defer timeout.Stop()

	for {
		select {
		case item, ok := <-qrChan:
			if !ok {
				return PairingResult{}, errors.New("wa: qr channel closed before a code arrived")
			}
			switch item.Event {
			case "code":
				return PairingResult{Mode: PairingModeQR, QR: item.Code}, nil
			case "error":
				return PairingResult{}, fmt.Errorf("wa: qr pairing failed: %w", item.Error)
			}
		case <-timeout.C:
			return PairingResult{}, errors.New("wa: timed out waiting for a qr code")
		case <-ctx.Done():
			return PairingResult{}, ctx.Err()
		}
	}
}

// Status reports the tenant's connection state, combining the persisted
// session row with the live client.
func (m *Manager) Status(ctx context.Context, tenantID string) (SessionStatus, error) {
	session, err := m.sessions.GetByTenant(ctx, tenantID)
	if errors.Is(err, store.ErrNotFound) {
		return SessionStatus{}, ErrUnknownTenant
	}
	if err != nil {
		return SessionStatus{}, err
	}

	status := SessionStatus{
		TenantID: tenantID,
		Status:   string(session.Status),
	}
	if session.WAJID != nil {
		status.WAJID = *session.WAJID
	}

	m.mu.RLock()
	client := m.clients[tenantID]
	m.mu.RUnlock()

	if client != nil {
		status.Connected = client.IsConnected()
		status.LoggedIn = client.IsLoggedIn()
	}
	return status, nil
}

// SendText sends a plain text message and reports what was sent.
//
// The destination is canonicalised BEFORE the message leaves and the very same
// address is what the caller persists, so the two directions of one
// conversation can never be filed apart.
func (m *Manager) SendText(ctx context.Context, tenantID, toJID, body string) (SentMessage, error) {
	m.mu.RLock()
	client := m.clients[tenantID]
	m.mu.RUnlock()

	if client == nil || !client.IsLoggedIn() {
		return SentMessage{}, ErrNotPaired
	}

	chat, canonical, err := sendDestination(ctx, m.lidsFor(tenantID), toJID)
	if err != nil {
		return SentMessage{}, err
	}
	// The sender is the tenant's own address, which is canonical by
	// construction; only the destination can stay a LID.
	m.countUnresolved(ctx, tenantID, canonical, true)

	resp, err := client.SendMessage(ctx, chat, &waProto.Message{
		Conversation: proto.String(body),
	})
	if err != nil {
		return SentMessage{}, fmt.Errorf("wa: send message: %w", err)
	}
	return sentRecord(resp, chat, m.ownJID(tenantID)), nil
}

// sentRecord maps one send response onto the values a row is written from.
//
// It deliberately ignores two fields the response carries. SendResponse.Chat is
// the address whatsmeow actually sent on, which it may have swapped for a LID on
// the way out, so storing it would undo the canonicalisation the send just did.
// SendResponse.Sender is documented as "currently not reliable in all cases"
// and is the own LID on that same path, so the tenant's own address is taken
// from the client store instead, exactly as the history path takes it.
//
// The timestamp comes off the server's acknowledgement. A response that carries
// none leaves a zero time, which would file the message at the start of the
// epoch, so the send time stands in — the one value here that is ours.
func sentRecord(resp whatsmeow.SendResponse, chat, own types.JID) SentMessage {
	timestamp := resp.Timestamp.UTC()
	if resp.Timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	return SentMessage{
		WAMessageID: string(resp.ID),
		ChatJID:     chat.ToNonAD().String(),
		SenderJID:   own.ToNonAD().String(),
		Timestamp:   timestamp,
	}
}

// Logout unlinks the tenant's device. The tenant's API keys stay valid: they
// belong to the tenant, not to the session.
func (m *Manager) Logout(ctx context.Context, tenantID string) error {
	m.mu.Lock()
	client := m.clients[tenantID]
	delete(m.clients, tenantID)
	m.mu.Unlock()

	if client == nil {
		return ErrNotPaired
	}
	if err := client.Logout(ctx); err != nil {
		return fmt.Errorf("wa: logout: %w", err)
	}

	if err := m.sessions.UpdateStatus(ctx, tenantID, store.SessionLoggedOut, ""); err != nil &&
		!errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// Close disconnects every client. It never returns an error; disconnecting is
// best effort during shutdown.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for tenantID, client := range m.clients {
		client.Disconnect()
		delete(m.clients, tenantID)
	}
}

// clientFor returns the tenant's client, creating a fresh device when the
// tenant has never been paired.
func (m *Manager) clientFor(ctx context.Context, tenantID string) (*whatsmeow.Client, error) {
	m.mu.RLock()
	existing := m.clients[tenantID]
	m.mu.RUnlock()
	if existing != nil {
		return existing, nil
	}

	session, err := m.sessions.GetByTenant(ctx, tenantID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrUnknownTenant
	}
	if err != nil {
		return nil, err
	}

	device, err := m.deviceFor(ctx, session)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	// Another goroutine may have won the race while we were loading.
	if existing := m.clients[tenantID]; existing != nil {
		return existing, nil
	}
	client := m.newClient(tenantID, device)
	m.clients[tenantID] = client
	return client, nil
}

func (m *Manager) deviceFor(ctx context.Context, session store.Session) (*waStore.Device, error) {
	if session.WAJID == nil || *session.WAJID == "" {
		return m.container.NewDevice(), nil
	}

	jid, err := parseJID(*session.WAJID)
	if err != nil {
		return m.container.NewDevice(), nil
	}

	device, err := m.container.GetDevice(ctx, jid)
	if err != nil {
		return nil, fmt.Errorf("wa: load device: %w", err)
	}
	if device == nil {
		return m.container.NewDevice(), nil
	}
	return device, nil
}

func (m *Manager) newClient(tenantID string, device *waStore.Device) *whatsmeow.Client {
	client := whatsmeow.NewClient(device, m.waLogger)
	client.AddEventHandler(func(evt any) { m.handleEvent(tenantID, evt) })
	return client
}

// handleEvent persists inbound messages and mirrors connection events onto the
// sessions table. It never touches api_keys.
func (m *Manager) handleEvent(tenantID string, evt any) {
	// A history sync is the one event that can carry thousands of messages, so
	// it gets a deadline of its own rather than the one a single message needs.
	timeout := 10 * time.Second
	if _, isHistory := evt.(*events.HistorySync); isHistory {
		timeout = historySyncPersistTimeout
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	switch e := evt.(type) {
	case *events.Message:
		m.persistMessage(ctx, tenantID, e)

	case *events.HistorySync:
		m.persistHistorySync(ctx, tenantID, e)

	case *events.Connected:
		waJID := ""
		m.mu.RLock()
		if client := m.clients[tenantID]; client != nil && client.Store.ID != nil {
			waJID = client.Store.ID.String()
		}
		m.mu.RUnlock()
		m.updateStatus(ctx, tenantID, store.SessionConnected, waJID)

	case *events.Disconnected:
		m.updateStatus(ctx, tenantID, store.SessionDisconnected, "")

	case *events.LoggedOut:
		m.updateStatus(ctx, tenantID, store.SessionLoggedOut, "")
	}
}

// persistMessage stores one live message, in either direction.
//
// It handles BOTH directions on purpose. WhatsApp delivers the tenant's own
// messages — the ones they typed on their phone — through the very same event,
// distinguished only by Info.IsFromMe. This function used to be called
// persistInbound and hardcoded store.DirectionIn, and the name is what made
// that look correct; the history path has always read the direction off the
// message key.
//
// Both addresses are canonicalised before the row is written, so the live path
// can never file one conversation under two addresses. A group keeps its own
// @g.us address and only its participant is resolved.
func (m *Manager) persistMessage(ctx context.Context, tenantID string, e *events.Message) {
	lids := m.lidsFor(tenantID)

	chat, chatCanonical := canonicalJID(ctx, lids, e.Info.Chat, chatAlt(e.Info.MessageSource))
	sender, senderCanonical := canonicalJID(ctx, lids, e.Info.Sender, e.Info.SenderAlt)
	m.countUnresolved(ctx, tenantID, chatCanonical, senderCanonical)

	record := liveRecord(tenantID, e, chat, sender)

	if err := m.messages.Append(ctx, record); err != nil {
		// Message bodies are never logged.
		m.logger.ErrorContext(ctx, "could not persist message",
			slog.String("tenant_id", tenantID),
			slog.String("wa_message_id", record.WAMessageID),
			slog.String("direction", string(record.Direction)),
			slog.String("error", err.Error()))
	}
}

// liveRecord maps one live message onto a row.
//
// chat and sender arrive already resolved because resolving a LID may need the
// tenant's client, and this mapping deliberately needs nothing but the event.
func liveRecord(tenantID string, e *events.Message, chat, sender types.JID) store.Message {
	direction := store.DirectionIn
	if e.Info.IsFromMe {
		direction = store.DirectionOut
	}

	record := store.Message{
		ID:          store.NewID(),
		TenantID:    tenantID,
		ChatJID:     chat.ToNonAD().String(),
		SenderJID:   sender.ToNonAD().String(),
		WAMessageID: string(e.Info.ID),
		Direction:   direction,
		Body:        extractText(e.Message),
		Timestamp:   e.Info.Timestamp.UTC(),
		CreatedAt:   time.Now().UTC(),
	}
	// Store the reference the attachment could later be fetched with, and
	// nothing else: no image, video or voice note is downloaded on receipt.
	// whatsmeow has already unwrapped the live message, but mediaReference is
	// used all the same so the live and history paths cannot disagree about
	// what a message carries.
	applyMediaReference(&record, mediaReference(e.Message))
	// whatsmeow's own naming on the live info wins when it has one: it is the
	// value historyMediaType was written to mirror.
	if e.Info.MediaType != "" {
		mediaType := e.Info.MediaType
		record.MediaType = &mediaType
	}
	return record
}

// lidsFor returns the tenant's LID index, or nil when the tenant has no client.
//
// A nil index is a normal state rather than a failure: an address that cannot
// be resolved is stored as it arrived.
func (m *Manager) lidsFor(tenantID string) lidResolver {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if client := m.clients[tenantID]; client != nil && client.Store != nil && client.Store.LIDs != nil {
		return client.Store.LIDs
	}
	return nil
}

// countUnresolved records addresses that stayed LIDs.
//
// It is a counter rather than an alert: a handful of unresolvable LIDs is the
// normal state of a live database, and the number is what says whether that is
// still true. It is logged at debug level only, because an address identifies a
// person as surely as a body identifies a conversation.
func (m *Manager) countUnresolved(ctx context.Context, tenantID string, chatCanonical, senderCanonical bool) {
	unresolved := 0
	if !chatCanonical {
		unresolved++
	}
	if !senderCanonical {
		unresolved++
	}
	if unresolved == 0 {
		return
	}

	total := m.unresolvedAddresses.Add(uint64(unresolved))
	m.logger.DebugContext(ctx, "could not resolve a lid to a phone number",
		slog.String("tenant_id", tenantID),
		slog.Uint64("unresolved_addresses_total", total))
}

func (m *Manager) updateStatus(ctx context.Context, tenantID string, status store.SessionStatus, waJID string) {
	if err := m.sessions.UpdateStatus(ctx, tenantID, status, waJID); err != nil {
		m.logger.ErrorContext(ctx, "could not update session status",
			slog.String("tenant_id", tenantID),
			slog.String("status", string(status)),
			slog.String("error", err.Error()))
		return
	}
	m.logger.InfoContext(ctx, "session status changed",
		slog.String("tenant_id", tenantID), slog.String("status", string(status)))
}

// extractText pulls the readable body out of the message variants that carry
// one. Anything else is stored with an empty body plus its media type.
func extractText(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}
	if text := msg.GetConversation(); text != "" {
		return text
	}
	if ext := msg.GetExtendedTextMessage(); ext != nil {
		return ext.GetText()
	}
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	}
	if vid := msg.GetVideoMessage(); vid != nil {
		return vid.GetCaption()
	}
	if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}
	return ""
}

// notDigits matches everything that cannot appear in a bare phone number.
var notDigits = regexp.MustCompile(`\D`)

// parseJID accepts both a bare phone number and a full JID.
//
// A bare number is user input: it reaches us from web forms and chat messages
// carrying "+", spaces and dashes, so it is reduced to digits before the
// default user server is appended. whatsmeow's own PairPhone normalises the
// same way, and sending had no business being stricter than pairing.
//
// A full JID is an address, not user input, and is passed through untouched.
// The user part of a group JID legitimately contains a dash
// ("123456789-987654@g.us"), so stripping non-digits there would corrupt a
// valid address.
func parseJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, ErrInvalidJID
	}
	if !strings.ContainsRune(raw, '@') {
		raw = notDigits.ReplaceAllString(raw, "")
		// types.ParseJID accepts any user part, so a string that held no
		// digits at all would otherwise become a valid-looking recipient.
		if raw == "" {
			return types.JID{}, ErrInvalidJID
		}
		raw += "@" + types.DefaultUserServer
	}

	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" {
		return types.JID{}, ErrInvalidJID
	}
	return jid, nil
}
