package httpapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

type syncHistoryResponse struct {
	ChatJID         string    `json:"chat_jid"`
	Inserted        int       `json:"inserted"`
	OldestTimestamp time.Time `json:"oldest_timestamp"`
	MoreAvailable   bool      `json:"more_available"`
}

type findContactsResponse struct {
	Contacts []struct {
		JID         string `json:"jid"`
		Name        string `json:"name"`
		HasMessages bool   `json:"has_messages"`
	} `json:"contacts"`
}

func TestSyncChatHistoryRequiresTheReadScope(t *testing.T) {
	tests := []struct {
		name       string
		scopes     []string
		wantStatus int
	}{
		{"a read key may backfill", []string{"messages:read"}, http.StatusOK},
		{"a send-only key may not", []string{"messages:send"}, http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", tc.scopes)

			rec := env.do(t, http.MethodPost, "/v1/chats/5215550001111@s.whatsapp.net/sync",
				tenant.APIKey.Key, map[string]any{"count": 50})

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestSyncChatHistoryClampsTheCount(t *testing.T) {
	tests := []struct {
		name string
		body any
		want int
	}{
		{"an explicit count is used", map[string]any{"count": 25}, 25},
		{"no count falls back to the default", map[string]any{}, wa.DefaultSyncCount},
		{"an empty body falls back to the default", nil, wa.DefaultSyncCount},
		{"zero falls back to the default", map[string]any{"count": 0}, wa.DefaultSyncCount},
		{"a negative count falls back to the default", map[string]any{"count": -5}, wa.DefaultSyncCount},
		{"an absurd count is capped", map[string]any{"count": 100000}, wa.MaxSyncCount},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", []string{"messages:read"})

			rec := env.do(t, http.MethodPost, "/v1/chats/5215550001111@s.whatsapp.net/sync",
				tenant.APIKey.Key, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}

			calls := env.sessions.snapshotSyncCalls()
			if len(calls) != 1 {
				t.Fatalf("SyncHistory called %d times, want 1", len(calls))
			}
			if calls[0].Count != tc.want {
				t.Errorf("count = %d, want %d", calls[0].Count, tc.want)
			}
			if calls[0].TenantID != tenant.TenantID {
				t.Errorf("backfilled as tenant %q, want %q", calls[0].TenantID, tenant.TenantID)
			}
		})
	}
}

func TestSyncChatHistoryReportsWhatLanded(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	oldest := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	env.sessions.syncResult = wa.SyncResult{Inserted: 37, OldestTimestamp: oldest, MoreAvailable: true}

	rec := env.do(t, http.MethodPost, "/v1/chats/5215550001111@s.whatsapp.net/sync",
		tenant.APIKey.Key, map[string]any{"count": 50})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got := decode[syncHistoryResponse](t, rec)
	if got.Inserted != 37 {
		t.Errorf("inserted = %d, want 37", got.Inserted)
	}
	if !got.MoreAvailable {
		t.Error("more_available = false, want true")
	}
	if !got.OldestTimestamp.Equal(oldest) {
		t.Errorf("oldest_timestamp = %v, want %v", got.OldestTimestamp, oldest)
	}
	if got.ChatJID != "5215550001111@s.whatsapp.net" {
		t.Errorf("chat_jid = %q", got.ChatJID)
	}
}

// TestSyncChatHistoryWithoutAnAnchor surfaces the product constraint rather
// than hiding it: a conversation the gateway holds nothing for cannot be
// backfilled, and the caller has to be told why.
func TestSyncChatHistoryWithoutAnAnchor(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})
	env.sessions.syncErr = wa.ErrNoAnchorMessage

	rec := env.do(t, http.MethodPost, "/v1/chats/5215550001111@s.whatsapp.net/sync",
		tenant.APIKey.Key, map[string]any{"count": 50})

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	body := decode[errorResponse](t, rec)
	if !strings.Contains(strings.ToLower(body.Error.Message), "anchor") {
		t.Errorf("message = %q, should explain that the chat has no message to anchor from", body.Error.Message)
	}
}

func TestSyncChatHistoryRejectsAnEmptyJID(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	rec := env.do(t, http.MethodPost, "/v1/chats/%20/sync", tenant.APIKey.Key, map[string]any{"count": 10})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestFindContactsRequiresTheReadScope(t *testing.T) {
	tests := []struct {
		name       string
		scopes     []string
		wantStatus int
	}{
		{"a read key may look up contacts", []string{"messages:read"}, http.StatusOK},
		{"a send-only key may not", []string{"messages:send"}, http.StatusForbidden},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", tc.scopes)

			rec := env.do(t, http.MethodGet, "/v1/contacts?q=ana", tenant.APIKey.Key, nil)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestFindContactsReturnsCandidates(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})
	env.sessions.contacts = []wa.Contact{
		{JID: "573001234567@s.whatsapp.net", Name: "Ana Torres", HasMessages: true},
		{JID: "573007654321@s.whatsapp.net", Name: "Ana Maria", HasMessages: false},
	}

	rec := env.do(t, http.MethodGet, "/v1/contacts?q=ana", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	got := decode[findContactsResponse](t, rec)
	if len(got.Contacts) != 2 {
		t.Fatalf("returned %d contacts, want 2", len(got.Contacts))
	}
	if got.Contacts[0].Name != "Ana Torres" || !got.Contacts[0].HasMessages {
		t.Errorf("contacts[0] = %+v", got.Contacts[0])
	}

	calls := env.sessions.snapshotContactCalls()
	if len(calls) != 1 {
		t.Fatalf("FindContacts called %d times, want 1", len(calls))
	}
	if calls[0].Query != "ana" {
		t.Errorf("query = %q, want %q", calls[0].Query, "ana")
	}
	// The tenant comes from the key, never from the request.
	if calls[0].TenantID != tenant.TenantID {
		t.Errorf("looked up contacts of tenant %q, want %q", calls[0].TenantID, tenant.TenantID)
	}
}

func TestFindContactsRequiresAQuery(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	rec := env.do(t, http.MethodGet, "/v1/contacts", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestFindContactsWithoutASession reports the missing pairing as a conflict,
// exactly as sending does.
func TestFindContactsWithoutASession(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})
	env.sessions.contactsErr = wa.ErrNotPaired

	rec := env.do(t, http.MethodGet, "/v1/contacts?q=ana", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
}
