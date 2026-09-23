package httpapi_test

import (
	"context"
	"sync"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// fakeSessionManager is the test double for wa.SessionManager. The real
// whatsmeow-backed implementation needs a live socket, so every handler test
// runs against this instead.
type fakeSessionManager struct {
	mu sync.Mutex

	pairCalls []pairCall
	sendCalls []sendCall

	pairResult  wa.PairingResult
	pairErr     error
	status      wa.SessionStatus
	statusErr   error
	sendID      string
	sendErr     error
	logoutCalls []string
	logoutErr   error
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

func (f *fakeSessionManager) SendText(_ context.Context, tenantID, toJID, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.sendCalls = append(f.sendCalls, sendCall{TenantID: tenantID, To: toJID, Body: body})
	if f.sendErr != nil {
		return "", f.sendErr
	}
	if f.sendID != "" {
		return f.sendID, nil
	}
	return "WA-MSG-1", nil
}

func (f *fakeSessionManager) Logout(_ context.Context, tenantID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.logoutCalls = append(f.logoutCalls, tenantID)
	return f.logoutErr
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
