package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/httpapi"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

const adminToken = "admin-token-for-tests"

type testEnv struct {
	handler  http.Handler
	db       *store.DB
	sessions *fakeSessionManager
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWithOrigins(t, nil)
}

// newTestEnvWithOrigins builds an environment with an Origin allowlist for the
// MCP endpoint.
func newTestEnvWithOrigins(t *testing.T, allowedOrigins []string) *testEnv {
	t.Helper()

	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}

	sessions := &fakeSessionManager{}
	srv, err := httpapi.New(httpapi.Config{
		AdminToken:        adminToken,
		DB:                db,
		Sessions:          sessions,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPAllowedOrigins: allowedOrigins,
	})
	if err != nil {
		t.Fatalf("httpapi.New() error = %v", err)
	}

	return &testEnv{handler: srv.Handler(), db: db, sessions: sessions}
}

// do issues a request and returns the recorder.
func (e *testEnv) do(t *testing.T, method, path, bearer string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			reader = strings.NewReader(b)
		default:
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatalf("marshal request body: %v", err)
			}
			reader = bytes.NewReader(raw)
		}
	}

	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}

	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

type createTenantResponse struct {
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	APIKey   struct {
		ID        string   `json:"id"`
		Name      string   `json:"name"`
		KeyPrefix string   `json:"key_prefix"`
		Scopes    []string `json:"scopes"`
		Key       string   `json:"key"`
	} `json:"api_key"`
}

type issueKeyResponse struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyPrefix string   `json:"key_prefix"`
	Scopes    []string `json:"scopes"`
	Key       string   `json:"key"`
}

type errorResponse struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// createTenant provisions a tenant through the admin API and returns its id and
// plaintext key.
func (e *testEnv) createTenant(t *testing.T, name string, scopes []string) createTenantResponse {
	t.Helper()

	rec := e.do(t, http.MethodPost, "/v1/tenants", adminToken, map[string]any{
		"name":   name,
		"scopes": scopes,
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/tenants status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	return decode[createTenantResponse](t, rec)
}

func TestHealthzIsPublic(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/healthz", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decode[map[string]any](t, rec)
	if got["status"] != "ok" {
		t.Errorf("body = %v, want status ok", got)
	}
}

func TestCreateTenantReturnsPlaintextKeyOnce(t *testing.T) {
	env := newTestEnv(t)

	created := env.createTenant(t, "Acme Corp", []string{"messages:send", "messages:read"})

	if created.TenantID == "" {
		t.Fatal("tenant_id must be returned")
	}
	if !strings.HasPrefix(created.APIKey.Key, "wc_live_") {
		t.Errorf("api key %q does not look like an issued key", created.APIKey.Key)
	}
	if !strings.HasPrefix(created.APIKey.Key, created.APIKey.KeyPrefix) {
		t.Errorf("key_prefix %q is not a prefix of the key", created.APIKey.KeyPrefix)
	}

	// The plaintext must not be recoverable afterwards.
	stored, err := env.db.APIKeys().ListByTenant(context.Background(), created.TenantID)
	if err != nil {
		t.Fatalf("ListByTenant() error = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored %d keys, want 1", len(stored))
	}
	if stored[0].KeyHash == created.APIKey.Key {
		t.Error("the plaintext key was stored instead of its hash")
	}

	raw, err := json.Marshal(stored[0])
	if err != nil {
		t.Fatalf("marshal stored key: %v", err)
	}
	if bytes.Contains(raw, []byte(created.APIKey.Key)) {
		t.Error("the stored key record serialises the plaintext key")
	}
}

func TestCreateTenantValidation(t *testing.T) {
	tests := []struct {
		name       string
		bearer     string
		body       any
		wantStatus int
		wantCode   string
	}{
		{"valid", adminToken, map[string]any{"name": "Acme"}, http.StatusCreated, ""},
		{"missing admin token", "", map[string]any{"name": "Acme"}, http.StatusUnauthorized, "unauthorized"},
		{"wrong admin token", "nope", map[string]any{"name": "Acme"}, http.StatusUnauthorized, "unauthorized"},
		{"missing name", adminToken, map[string]any{}, http.StatusBadRequest, "invalid_request"},
		{"blank name", adminToken, map[string]any{"name": "   "}, http.StatusBadRequest, "invalid_request"},
		{"malformed json", adminToken, "{not json", http.StatusBadRequest, "invalid_request"},
		{"unknown scope", adminToken, map[string]any{"name": "Acme", "scopes": []string{"billing:read"}}, http.StatusBadRequest, "invalid_request"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			rec := env.do(t, http.MethodPost, "/v1/tenants", tc.bearer, tc.body)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantCode == "" {
				return
			}
			got := decode[errorResponse](t, rec)
			if got.Error.Code != tc.wantCode {
				t.Errorf("error code = %q, want %q", got.Error.Code, tc.wantCode)
			}
			if got.Error.Message == "" {
				t.Error("every error response must carry a message")
			}
		})
	}
}

func TestIssueAdditionalKey(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})

	rec := env.do(t, http.MethodPost, "/v1/tenants/"+tenant.TenantID+"/keys", adminToken, map[string]any{
		"name":   "read only",
		"scopes": []string{"messages:read"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	issued := decode[issueKeyResponse](t, rec)
	if issued.Key == tenant.APIKey.Key {
		t.Error("a newly issued key must differ from the first one")
	}
	if len(issued.Scopes) != 1 || issued.Scopes[0] != "messages:read" {
		t.Errorf("scopes = %#v, want [messages:read]", issued.Scopes)
	}

	// Both keys must work, each with its own scopes.
	if got := env.do(t, http.MethodGet, "/v1/chats", issued.Key, nil); got.Code != http.StatusOK {
		t.Errorf("the read key should reach /v1/chats, got %d", got.Code)
	}
	if got := env.do(t, http.MethodGet, "/v1/chats", tenant.APIKey.Key, nil); got.Code != http.StatusForbidden {
		t.Errorf("the send-only key should be forbidden on /v1/chats, got %d", got.Code)
	}
}

func TestIssueKeyForUnknownTenant(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodPost, "/v1/tenants/does-not-exist/keys", adminToken, map[string]any{"name": "x"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestRevokeKey(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	if got := env.do(t, http.MethodGet, "/v1/chats", tenant.APIKey.Key, nil); got.Code != http.StatusOK {
		t.Fatalf("precondition failed: key should work, got %d", got.Code)
	}

	rec := env.do(t, http.MethodDelete, "/v1/tenants/"+tenant.TenantID+"/keys/"+tenant.APIKey.ID, adminToken, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	if got := env.do(t, http.MethodGet, "/v1/chats", tenant.APIKey.Key, nil); got.Code != http.StatusUnauthorized {
		t.Errorf("a revoked key should be unauthorized, got %d", got.Code)
	}

	// Revoking twice reports not found rather than silently succeeding.
	again := env.do(t, http.MethodDelete, "/v1/tenants/"+tenant.TenantID+"/keys/"+tenant.APIKey.ID, adminToken, nil)
	if again.Code != http.StatusNotFound {
		t.Errorf("second revoke status = %d, want 404", again.Code)
	}
}

func TestRevokeKeyRequiresAdmin(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	rec := env.do(t, http.MethodDelete,
		"/v1/tenants/"+tenant.TenantID+"/keys/"+tenant.APIKey.ID, tenant.APIKey.Key, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a tenant key must not revoke keys, got %d", rec.Code)
	}
}

func TestStartPairing(t *testing.T) {
	tests := []struct {
		name         string
		body         any
		wantMode     string
		wantPairCode string
		wantQR       string
		wantPhone    string
	}{
		{
			name:         "phone selects the pair-code flow",
			body:         map[string]any{"phone": "5215550001111"},
			wantMode:     "code",
			wantPairCode: "ABCD1234",
			wantPhone:    "5215550001111",
		},
		{
			name:     "no phone falls back to qr",
			body:     map[string]any{},
			wantMode: "qr",
			wantQR:   "2@qr-payload",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", []string{"sessions:read"})

			rec := env.do(t, http.MethodPost, "/v1/tenants/"+tenant.TenantID+"/sessions/pair", adminToken, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}

			got := decode[wa.PairingResult](t, rec)
			if string(got.Mode) != tc.wantMode {
				t.Errorf("mode = %q, want %q", got.Mode, tc.wantMode)
			}
			if got.PairCode != tc.wantPairCode {
				t.Errorf("pair_code = %q, want %q", got.PairCode, tc.wantPairCode)
			}
			if got.QR != tc.wantQR {
				t.Errorf("qr = %q, want %q", got.QR, tc.wantQR)
			}

			calls := env.sessions.snapshotPairs()
			if len(calls) != 1 {
				t.Fatalf("StartPairing called %d times, want 1", len(calls))
			}
			if calls[0].TenantID != tenant.TenantID {
				t.Errorf("paired tenant = %q, want %q", calls[0].TenantID, tenant.TenantID)
			}
			if calls[0].Phone != tc.wantPhone {
				t.Errorf("phone = %q, want %q", calls[0].Phone, tc.wantPhone)
			}
		})
	}
}

func TestStartPairingRequiresAdmin(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"sessions:read"})

	rec := env.do(t, http.MethodPost,
		"/v1/tenants/"+tenant.TenantID+"/sessions/pair", tenant.APIKey.Key, map[string]any{})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a tenant key must not start pairing, got %d", rec.Code)
	}
}

// TestPairingDoesNotTouchAPIKeys is the HTTP-level restatement of the core
// invariant: keys belong to the tenant, pairing only moves the session.
func TestPairingDoesNotTouchAPIKeys(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	for i := 0; i < 3; i++ {
		rec := env.do(t, http.MethodPost,
			"/v1/tenants/"+tenant.TenantID+"/sessions/pair", adminToken, map[string]any{"phone": "5215550001111"})
		if rec.Code != http.StatusOK {
			t.Fatalf("pair attempt %d status = %d (body %s)", i, rec.Code, rec.Body.String())
		}
	}

	if got := env.do(t, http.MethodGet, "/v1/chats", tenant.APIKey.Key, nil); got.Code != http.StatusOK {
		t.Errorf("the api key stopped working after re-pairing (status %d)", got.Code)
	}
}

func TestGetSessionReportsCallersOwnSession(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"sessions:read"})

	env.sessions.status = wa.SessionStatus{
		Status:    "connected",
		WAJID:     "5215550001111@s.whatsapp.net",
		Connected: true,
		LoggedIn:  true,
	}

	rec := env.do(t, http.MethodGet, "/v1/sessions", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got := decode[wa.SessionStatus](t, rec)
	if got.TenantID != tenant.TenantID {
		t.Errorf("tenant_id = %q, want the caller's own tenant %q", got.TenantID, tenant.TenantID)
	}
	if got.Status != "connected" || !got.Connected {
		t.Errorf("status = %+v, want a connected session", got)
	}
}

func TestGetSessionRequiresSessionsReadScope(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})

	rec := env.do(t, http.MethodGet, "/v1/sessions", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

func TestSendMessage(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})

	rec := env.do(t, http.MethodPost, "/v1/messages", tenant.APIKey.Key, map[string]any{
		"to":   "5215550001111@s.whatsapp.net",
		"body": "hello from the gateway",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	got := decode[map[string]any](t, rec)
	if got["wa_message_id"] != "WA-MSG-1" {
		t.Errorf("wa_message_id = %v, want WA-MSG-1", got["wa_message_id"])
	}

	calls := env.sessions.snapshotSends()
	if len(calls) != 1 {
		t.Fatalf("SendText called %d times, want 1", len(calls))
	}
	if calls[0].TenantID != tenant.TenantID {
		t.Errorf("sent as tenant %q, want %q", calls[0].TenantID, tenant.TenantID)
	}
	if calls[0].Body != "hello from the gateway" {
		t.Errorf("body = %q", calls[0].Body)
	}

	// The outbound message must be persisted so it shows up in the history.
	stored, err := env.db.Messages().ListByChat(context.Background(), tenant.TenantID, "5215550001111@s.whatsapp.net", 10)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("persisted %d outbound messages, want 1", len(stored))
	}
	if stored[0].Direction != store.DirectionOut {
		t.Errorf("direction = %q, want %q", stored[0].Direction, store.DirectionOut)
	}
}

func TestSendMessageValidation(t *testing.T) {
	tests := []struct {
		name       string
		scopes     []string
		useAdmin   bool
		body       any
		wantStatus int
	}{
		{"valid", []string{"messages:send"}, false, map[string]any{"to": "x@s.whatsapp.net", "body": "hi"}, http.StatusCreated},
		{"missing scope", []string{"messages:read"}, false, map[string]any{"to": "x@s.whatsapp.net", "body": "hi"}, http.StatusForbidden},
		{"admin token cannot send", []string{"messages:send"}, true, map[string]any{"to": "x@s.whatsapp.net", "body": "hi"}, http.StatusUnauthorized},
		{"missing to", []string{"messages:send"}, false, map[string]any{"body": "hi"}, http.StatusBadRequest},
		{"missing body", []string{"messages:send"}, false, map[string]any{"to": "x@s.whatsapp.net"}, http.StatusBadRequest},
		{"blank body", []string{"messages:send"}, false, map[string]any{"to": "x@s.whatsapp.net", "body": "  "}, http.StatusBadRequest},
		{"malformed json", []string{"messages:send"}, false, "{", http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", tc.scopes)

			bearer := tenant.APIKey.Key
			if tc.useAdmin {
				bearer = adminToken
			}

			rec := env.do(t, http.MethodPost, "/v1/messages", bearer, tc.body)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestSendMessageWhenNotPaired(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})
	env.sessions.sendErr = wa.ErrNotPaired

	rec := env.do(t, http.MethodPost, "/v1/messages", tenant.APIKey.Key, map[string]any{
		"to": "x@s.whatsapp.net", "body": "hi",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an unpaired tenant (body %s)", rec.Code, rec.Body.String())
	}
	got := decode[errorResponse](t, rec)
	if got.Error.Code != "conflict" {
		t.Errorf("error code = %q, want conflict", got.Error.Code)
	}
}

func TestSendMessageWithInvalidJID(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})
	env.sessions.sendErr = wa.ErrInvalidJID

	rec := env.do(t, http.MethodPost, "/v1/messages", tenant.APIKey.Key, map[string]any{
		"to": "not-a-jid", "body": "hi",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func seedChat(t *testing.T, env *testEnv, tenantID, chatJID, body string, at time.Time) {
	t.Helper()
	err := env.db.Messages().Append(context.Background(), store.Message{
		ID:          store.NewID(),
		TenantID:    tenantID,
		ChatJID:     chatJID,
		SenderJID:   chatJID,
		WAMessageID: store.NewID(),
		Direction:   store.DirectionIn,
		Body:        body,
		Timestamp:   at,
		CreatedAt:   time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Append() error = %v", err)
	}
}

func TestListChats(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	seedChat(t, env, tenant.TenantID, "chat-1@s.whatsapp.net", "older", base)
	seedChat(t, env, tenant.TenantID, "chat-2@s.whatsapp.net", "newer", base.Add(time.Minute))

	rec := env.do(t, http.MethodGet, "/v1/chats", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got := decode[struct {
		Chats []store.Chat `json:"chats"`
	}](t, rec)
	if len(got.Chats) != 2 {
		t.Fatalf("returned %d chats, want 2", len(got.Chats))
	}
	if got.Chats[0].ChatJID != "chat-2@s.whatsapp.net" {
		t.Errorf("chats[0] = %q, want the most recent chat first", got.Chats[0].ChatJID)
	}
}

func TestListChatMessages(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	const chatJID = "5215550001111@s.whatsapp.net"
	base := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	for i, body := range []string{"first", "second", "third"} {
		seedChat(t, env, tenant.TenantID, chatJID, body, base.Add(time.Duration(i)*time.Minute))
	}

	tests := []struct {
		name      string
		query     string
		wantCount int
		wantFirst string
	}{
		{"default limit", "", 3, "third"},
		{"explicit limit", "?limit=2", 2, "third"},
		{"limit above the cap is clamped", "?limit=100000", 3, "third"},
		{"non numeric limit falls back to the default", "?limit=abc", 3, "third"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := "/v1/chats/" + url.PathEscape(chatJID) + "/messages" + tc.query
			rec := env.do(t, http.MethodGet, path, tenant.APIKey.Key, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}

			got := decode[struct {
				Messages []store.Message `json:"messages"`
			}](t, rec)
			if len(got.Messages) != tc.wantCount {
				t.Fatalf("returned %d messages, want %d", len(got.Messages), tc.wantCount)
			}
			if got.Messages[0].Body != tc.wantFirst {
				t.Errorf("messages[0].Body = %q, want %q (newest first)", got.Messages[0].Body, tc.wantFirst)
			}
		})
	}
}

func TestListChatMessagesRequiresReadScope(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})

	rec := env.do(t, http.MethodGet, "/v1/chats/"+url.PathEscape("x@s.whatsapp.net")+"/messages", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
}

// TestTenantCannotReadAnotherTenantsMessages is the end-to-end multi-tenant
// isolation guarantee.
func TestTenantCannotReadAnotherTenantsMessages(t *testing.T) {
	env := newTestEnv(t)

	tenantA := env.createTenant(t, "Acme", []string{"messages:read"})
	tenantB := env.createTenant(t, "Globex", []string{"messages:read"})

	const sharedChat = "5215550009999@s.whatsapp.net"
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	seedChat(t, env, tenantA.TenantID, sharedChat, "message for acme", at)
	seedChat(t, env, tenantB.TenantID, sharedChat, "message for globex", at.Add(time.Minute))

	path := "/v1/chats/" + url.PathEscape(sharedChat) + "/messages"

	rec := env.do(t, http.MethodGet, path, tenantA.APIKey.Key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "message for globex") {
		t.Error("tenant A read tenant B's message")
	}

	got := decode[struct {
		Messages []store.Message `json:"messages"`
	}](t, rec)
	if len(got.Messages) != 1 || got.Messages[0].Body != "message for acme" {
		t.Errorf("tenant A saw %#v, want only its own message", got.Messages)
	}
	for _, m := range got.Messages {
		if m.TenantID != tenantA.TenantID {
			t.Errorf("response leaked a row of tenant %q", m.TenantID)
		}
	}

	// And the mirror image, so the test cannot pass by accident.
	recB := env.do(t, http.MethodGet, path, tenantB.APIKey.Key, nil)
	if strings.Contains(recB.Body.String(), "message for acme") {
		t.Error("tenant B read tenant A's message")
	}
}

func TestUnknownRouteReturnsJSONError(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/v1/nope", adminToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	got := decode[errorResponse](t, rec)
	if got.Error.Code != "not_found" {
		t.Errorf("error code = %q, want not_found", got.Error.Code)
	}
}

func TestMethodNotAllowed(t *testing.T) {
	env := newTestEnv(t)

	rec := env.do(t, http.MethodGet, "/v1/messages", adminToken, nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

func TestNewRejectsEmptyAdminToken(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer func() { _ = db.Close() }()

	if _, err := httpapi.New(httpapi.Config{DB: db, Sessions: &fakeSessionManager{}}); err == nil {
		t.Error("New() without an admin token should fail: it would leave the admin surface open")
	}
}
