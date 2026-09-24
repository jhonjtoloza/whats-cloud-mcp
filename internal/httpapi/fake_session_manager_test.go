package httpapi_test

import (
	"context"
	"sync"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// What a send reports back when a test does not say otherwise.
//
// The timestamp is deliberately far from now: an outbound row used to be filed
// at time.Now() instead of the moment WhatsApp recorded, and a fixed, distant
// value is what makes the difference visible in a stored row.
const fakeOwnJID = "573114276555@s.whatsapp.net"

var fakeSentAt = time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)

// fakeSessionManager is the test double for wa.SessionManager. The real
// whatsmeow-backed implementation needs a live socket, so every handler test
// runs against this instead.
type fakeSessionManager struct {
	mu sync.Mutex

	pairCalls    []pairCall
	sendCalls    []sendCall
	contactCalls []contactCall
	syncCalls    []syncCall
	mediaCalls   []mediaCall

	pairResult  wa.PairingResult
	pairErr     error
	status      wa.SessionStatus
	statusErr   error
	sendResult  wa.SentMessage
	sendResults map[string]wa.SentMessage
	sendErr     error
	logoutCalls []string
	logoutErr   error
	contacts    []wa.Contact
	contactsErr error
	syncResult  wa.SyncResult
	syncErr     error
	mediaRef    wa.MediaRef
	mediaErr    error
}

type pairCall struct {
	TenantID string
	Phone    string
}

type sendCall struct {
	TenantID string
	To       string
	Body     string
}

type contactCall struct {
	TenantID string
	Query    string
}

type syncCall struct {
	TenantID string
	ChatJID  string
	Count    int
}

type mediaCall struct {
	TenantID  string
	MessageID string
}

func (f *fakeSessionManager) StartPairing(_ context.Context, tenantID, phone string) (wa.PairingResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pairCalls = append(f.pairCalls, pairCall{TenantID: tenantID, Phone: phone})
	if f.pairErr != nil {
		return wa.PairingResult{}, f.pairErr
	}
	if f.pairResult != (wa.PairingResult{}) {
		return f.pairResult, nil
	}
	// Mirror the real manager: a phone number selects the pair-code flow, an
	// empty phone falls back to QR.
	if phone != "" {
		return wa.PairingResult{Mode: wa.PairingModeCode, PairCode: "ABCD1234"}, nil
	}
	return wa.PairingResult{Mode: wa.PairingModeQR, QR: "2@qr-payload"}, nil
}

func (f *fakeSessionManager) Status(_ context.Context, tenantID string) (wa.SessionStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.statusErr != nil {
		return wa.SessionStatus{}, f.statusErr
	}
	status := f.status
	status.TenantID = tenantID
	if status.Status == "" {
		status.Status = "pending"
	}
	return status, nil
}

// SendText stands in for the real send. Only the manager knows the address the
// message was resolved to, the tenant's own JID and the timestamp WhatsApp
// recorded, so here the test decides what the send would have reported — per
// destination when it needs several, and a default otherwise.
func (f *fakeSessionManager) SendText(_ context.Context, tenantID, toJID, body string) (wa.SentMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendCalls = append(f.sendCalls, sendCall{TenantID: tenantID, To: toJID, Body: body})
	if f.sendErr != nil {
		return wa.SentMessage{}, f.sendErr
	}
	if sent, ok := f.sendResults[toJID]; ok {
		return sent, nil
	}

	sent := f.sendResult
	if sent.WAMessageID == "" {
		sent.WAMessageID = "WA-MSG-1"
	}
	if sent.ChatJID == "" {
		// The real manager canonicalises; the default here only has to be an
		// address, so it echoes the destination.
		sent.ChatJID = toJID
	}
	if sent.SenderJID == "" {
		sent.SenderJID = fakeOwnJID
	}
	if sent.Timestamp.IsZero() {
		sent.Timestamp = fakeSentAt
	}
	return sent, nil
}

func (f *fakeSessionManager) Logout(_ context.Context, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.logoutCalls = append(f.logoutCalls, tenantID)
	return f.logoutErr
}

func (f *fakeSessionManager) FindContacts(_ context.Context, tenantID, query string) ([]wa.Contact, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.contactCalls = append(f.contactCalls, contactCall{TenantID: tenantID, Query: query})
	if f.contactsErr != nil {
		return nil, f.contactsErr
	}
	return f.contacts, nil
}

func (f *fakeSessionManager) SyncHistory(_ context.Context, tenantID, chatJID string, count int) (wa.SyncResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.syncCalls = append(f.syncCalls, syncCall{TenantID: tenantID, ChatJID: chatJID, Count: count})
	if f.syncErr != nil {
		return wa.SyncResult{}, f.syncErr
	}
	result := f.syncResult
	if result.ChatJID == "" {
		result.ChatJID = chatJID
	}
	return result, nil
}

// FetchMedia stands in for the lazy download. The real one needs a socket and a
// live media connection; here the test decides what the fetch would have found.
func (f *fakeSessionManager) FetchMedia(_ context.Context, tenantID, messageID string) (wa.MediaRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.mediaCalls = append(f.mediaCalls, mediaCall{TenantID: tenantID, MessageID: messageID})
	if f.mediaErr != nil {
		return wa.MediaRef{}, f.mediaErr
	}
	ref := f.mediaRef
	if ref.MessageID == "" {
		ref.MessageID = messageID
	}
	return ref, nil
}

func (f *fakeSessionManager) snapshotMediaCalls() []mediaCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]mediaCall, len(f.mediaCalls))
	copy(out, f.mediaCalls)
	return out
}

func (f *fakeSessionManager) snapshotContactCalls() []contactCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]contactCall, len(f.contactCalls))
	copy(out, f.contactCalls)
	return out
}

func (f *fakeSessionManager) snapshotSyncCalls() []syncCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]syncCall, len(f.syncCalls))
	copy(out, f.syncCalls)
	return out
}

func (f *fakeSessionManager) snapshotSends() []sendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sendCall, len(f.sendCalls))
	copy(out, f.sendCalls)
	return out
}

func (f *fakeSessionManager) snapshotPairs() []pairCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pairCall, len(f.pairCalls))
	copy(out, f.pairCalls)
	return out
}
