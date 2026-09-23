package wa

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	waStore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// qrWaitTimeout bounds how long StartPairing waits for the first QR code.
const qrWaitTimeout = 20 * time.Second

// pairClientDisplayName is what the user sees in WhatsApp's linked-devices list.
const pairClientDisplayName = "whats-cloud-mcp"

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

	mu      sync.RWMutex
	clients map[string]*whatsmeow.Client
}

// NewManager builds a Manager on top of the gateway's own database handle, so
// whatsmeow's schema lives in the very same SQLite file.
func NewManager(ctx context.Context, db *store.DB, logger *slog.Logger) (*Manager, error) {
	if logger == nil {
		logger = slog.Default()
	}

	// dbutil recognises any dialect starting with "sqlite"; the driver itself
	// is the pure-Go modernc.org/sqlite registered as "sqlite".
	container := sqlstore.NewWithDB(db.SQL(), "sqlite", waLog.Noop)
	if err := container.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("wa: upgrade whatsmeow schema: %w", err)
	}

	return &Manager{
		container: container,
		sessions:  db.Sessions(),
		messages:  db.Messages(),
		logger:    logger,
		waLogger:  waLog.Noop,
		clients:   make(map[string]*whatsmeow.Client),
	}, nil
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

// SendText sends a plain text message and returns the WhatsApp message id.
func (m *Manager) SendText(ctx context.Context, tenantID, toJID, body string) (string, error) {
	m.mu.RLock()
	client := m.clients[tenantID]
	m.mu.RUnlock()

	if client == nil || !client.IsLoggedIn() {
		return "", ErrNotPaired
	}

	jid, err := parseJID(toJID)
	if err != nil {
		return "", err
	}

	resp, err := client.SendMessage(ctx, jid, &waProto.Message{
		Conversation: proto.String(body),
	})
	if err != nil {
		return "", fmt.Errorf("wa: send message: %w", err)
	}
	return string(resp.ID), nil
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	switch e := evt.(type) {
	case *events.Message:
		m.persistInbound(ctx, tenantID, e)

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

func (m *Manager) persistInbound(ctx context.Context, tenantID string, e *events.Message) {
	body := extractText(e.Message)

	record := store.Message{
		ID:          store.NewID(),
		TenantID:    tenantID,
		ChatJID:     e.Info.Chat.String(),
		SenderJID:   e.Info.Sender.String(),
		WAMessageID: string(e.Info.ID),
		Direction:   store.DirectionIn,
		Body:        body,
		Timestamp:   e.Info.Timestamp.UTC(),
		CreatedAt:   time.Now().UTC(),
	}
	if e.Info.MediaType != "" {
		mediaType := e.Info.MediaType
		record.MediaType = &mediaType
	}

	if err := m.messages.Append(ctx, record); err != nil {
		// Message bodies are never logged.
		m.logger.ErrorContext(ctx, "could not persist inbound message",
			slog.String("tenant_id", tenantID),
			slog.String("wa_message_id", record.WAMessageID),
			slog.String("error", err.Error()))
	}
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

// parseJID accepts both a bare user id and a full JID.
func parseJID(raw string) (types.JID, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return types.JID{}, ErrInvalidJID
	}
	if !strings.ContainsRune(raw, '@') {
		raw += "@" + types.DefaultUserServer
	}

	jid, err := types.ParseJID(raw)
	if err != nil || jid.User == "" {
		return types.JID{}, ErrInvalidJID
	}
	return jid, nil
}
