package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

type mcpMedia struct {
	MessageID string `json:"message_id"`
	Path      string `json:"path"`
	MimeType  string `json:"mime_type"`
	MediaType string `json:"media_type"`
	Status    string `json:"status"`
	SizeBytes int64  `json:"size_bytes"`
}

// TestMCPServesGetMedia is the tool an agent reaches for once a listing has
// told it a media message exists.
func TestMCPServesGetMedia(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"media:read"})

	path := writeMediaFile(t, "voice.ogg", []byte("bytes"))
	env.sessions.mediaRef = wa.MediaRef{
		MessageID: "msg-1",
		MediaType: "ptt",
		MimeType:  "audio/ogg; codecs=opus",
		Status:    string(store.MediaAvailable),
		Path:      path,
		SizeBytes: 5,
	}

	session := connectMCP(t, env, tenant.APIKey.Key, "")
	out := structured[mcpMedia](t, callTool(t, session, "get_media", map[string]any{"message_id": "msg-1"}))

	if out.Path != path {
		t.Errorf("path = %q, want %q", out.Path, path)
	}
	if out.MimeType != "audio/ogg; codecs=opus" {
		t.Errorf("mime_type = %q", out.MimeType)
	}
	if out.MediaType != "ptt" {
		t.Errorf("media_type = %q, want ptt", out.MediaType)
	}
	if out.Status != string(store.MediaAvailable) {
		t.Errorf("status = %q, want %q", out.Status, store.MediaAvailable)
	}

	calls := env.sessions.snapshotMediaCalls()
	if len(calls) != 1 {
		t.Fatalf("FetchMedia called %d times, want 1", len(calls))
	}
	if calls[0].TenantID != tenant.TenantID {
		t.Errorf("fetched as tenant %q, want %q", calls[0].TenantID, tenant.TenantID)
	}
}

// TestMCPGetMediaEnforcesItsOwnScope mirrors TestMCPEnforcesScopePerTool for
// the media tool: one URL, tools with different requirements, so the tool
// checks for itself.
func TestMCPGetMediaEnforcesItsOwnScope(t *testing.T) {
	tests := []struct {
		name       string
		scopes     []string
		wantErr    bool
		wantReason string
	}{
		{name: "a media:read key may fetch", scopes: []string{"media:read"}},
		{name: "a media wildcard may fetch", scopes: []string{"media:*"}},
		{
			name:       "a messages:read key may not",
			scopes:     []string{"messages:read"},
			wantErr:    true,
			wantReason: "media:read",
		},
		{
			name:       "a messages:send key may not",
			scopes:     []string{"messages:send"},
			wantErr:    true,
			wantReason: "media:read",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", tc.scopes)
			env.sessions.mediaRef = wa.MediaRef{
				MessageID: "msg-1", Status: string(store.MediaAvailable),
				Path: writeMediaFile(t, "voice.ogg", []byte("bytes")),
			}

			session := connectMCP(t, env, tenant.APIKey.Key, "")
			res := callTool(t, session, "get_media", map[string]any{"message_id": "msg-1"})

			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v (content %q)", res.IsError, tc.wantErr, resultText(t, res))
			}
			if !tc.wantErr {
				return
			}
			if !strings.Contains(resultText(t, res), tc.wantReason) {
				t.Errorf("error %q should name the missing scope %q", resultText(t, res), tc.wantReason)
			}
			// A denied call must not reach the engine at all.
			if calls := env.sessions.snapshotMediaCalls(); len(calls) != 0 {
				t.Errorf("a denied get_media still fetched: %+v", calls)
			}
		})
	}
}

// TestMCPGetMediaIgnoresClientSuppliedTenant extends the guarantee of
// TestMCPToolsIgnoreClientSuppliedTenant: get_media takes no tenant, so it
// cannot be pointed at somebody else's files.
func TestMCPGetMediaIgnoresClientSuppliedTenant(t *testing.T) {
	env := newTestEnv(t)
	tenantA := env.createTenant(t, "Acme", []string{"media:read"})
	tenantB := env.createTenant(t, "Globex", []string{"media:read"})

	env.sessions.mediaRef = wa.MediaRef{
		MessageID: "msg-1", Status: string(store.MediaAvailable),
		Path: writeMediaFile(t, "voice.ogg", []byte("bytes")),
	}

	session := connectMCP(t, env, tenantA.APIKey.Key, "")
	callTool(t, session, "get_media", map[string]any{
		"message_id": "msg-1",
		"tenant_id":  tenantB.TenantID,
	})

	for _, call := range env.sessions.snapshotMediaCalls() {
		if call.TenantID != tenantA.TenantID {
			t.Errorf("a client-supplied tenant_id redirected get_media to tenant %q", call.TenantID)
		}
	}
}

// TestMCPGetMediaReportsExpiredMediaHonestly is what stops an agent retrying a
// file WhatsApp no longer has. It has to read as a final answer, not a glitch.
func TestMCPGetMediaReportsExpiredMedia(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantReason string
	}{
		{"expired", wa.ErrMediaUnavailable, "expired"},
		{"too large", wa.ErrMediaTooLarge, "large"},
		{"a type the gateway skips", wa.ErrMediaTypeNotAllowed, "does not download"},
		{"no attachment", wa.ErrNoMedia, "no downloadable media"},
		{"an unknown message", wa.ErrUnknownMessage, "no message"},
		{"an unpaired tenant", wa.ErrNotPaired, "pair"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", []string{"media:read"})
			env.sessions.mediaErr = tc.err

			session := connectMCP(t, env, tenant.APIKey.Key, "")
			res := callTool(t, session, "get_media", map[string]any{"message_id": "msg-1"})

			if !res.IsError {
				t.Fatalf("get_media should report %v as a tool error", tc.err)
			}
			text := strings.ToLower(resultText(t, res))
			if !strings.Contains(text, tc.wantReason) {
				t.Errorf("error %q should explain %q", text, tc.wantReason)
			}
		})
	}
}

// TestMCPGetMediaRequiresAMessageID keeps a bare call from reaching the engine.
func TestMCPGetMediaRequiresAMessageID(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"media:read"})

	session := connectMCP(t, env, tenant.APIKey.Key, "")
	res := callTool(t, session, "get_media", map[string]any{"message_id": ""})

	if !res.IsError {
		t.Fatal("get_media without a message id should be a tool error")
	}
	if calls := env.sessions.snapshotMediaCalls(); len(calls) != 0 {
		t.Errorf("an empty message id reached the engine: %+v", calls)
	}
}

// TestMCPListMessagesNeverFetchesMedia is the promise the tool description
// makes: a listing stays fast and predictable, so it reports what is known
// about an attachment and never downloads one.
func TestMCPListMessagesNeverFetchesMedia(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read", "media:read"})

	const chatJID = "573001234567@s.whatsapp.net"
	seedMediaMessage(t, env, tenant.TenantID, chatJID, "WA-1", "ptt", nil)
	seedChat(t, env, tenant.TenantID, chatJID, "and some text", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC))

	session := connectMCP(t, env, tenant.APIKey.Key, "")
	res := callTool(t, session, "list_messages", map[string]any{"chat_jid": chatJID})
	if res.IsError {
		t.Fatalf("list_messages failed: %s", resultText(t, res))
	}

	if calls := env.sessions.snapshotMediaCalls(); len(calls) != 0 {
		t.Fatalf("list_messages triggered %d media fetches; a listing must never download", len(calls))
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"media_type":"ptt"`) {
		t.Errorf("list_messages did not report the attachment: %s", raw)
	}
	// The listing must not hand out host paths or key material either.
	for _, secret := range []string{"media_path", "direct_path", "media_key", "/data/media"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("list_messages leaked %q: %s", secret, raw)
		}
	}
}

// TestMCPListsTheMediaTool keeps get_media discoverable alongside the five it
// joins.
func TestMCPListsTheMediaTool(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"media:read"})
	session := connectMCP(t, env, tenant.APIKey.Key, "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}

	names := make(map[string]string, len(res.Tools))
	for _, tool := range res.Tools {
		names[tool.Name] = strings.ToLower(tool.Description)
	}

	want := []string{"list_chats", "list_messages", "send_message", "find_contact", "sync_history", "get_media"}
	if len(names) != len(want) {
		t.Errorf("the server exposes %d tools, want %d: %v", len(names), len(want), names)
	}
	for _, name := range want {
		if _, ok := names[name]; !ok {
			t.Errorf("tool %q is missing", name)
		}
	}

	// The descriptions have to teach the model the lazy model itself: the
	// database always knows the message exists, the bytes arrive only when
	// asked, and asking may legitimately answer "gone".
	for _, phrase := range []string{"expired", "list_messages"} {
		if !strings.Contains(names["get_media"], phrase) {
			t.Errorf("get_media description does not mention %q: %q", phrase, names["get_media"])
		}
	}
	for _, phrase := range []string{"get_media", "never"} {
		if !strings.Contains(names["list_messages"], phrase) {
			t.Errorf("list_messages description does not mention %q: %q", phrase, names["list_messages"])
		}
	}
}

// TestMCPMediaToolDoesNotWeakenTheRESTSurface is a guard against the media
// route being reachable with the wrong credential class.
func TestMediaRouteRejectsTheAdminToken(t *testing.T) {
	env := newTestEnv(t)
	env.createTenant(t, "Acme", []string{"media:read"})

	rec := env.do(t, http.MethodGet, "/v1/messages/msg-1/media", adminToken, nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401: the admin token is not a tenant", rec.Code)
	}
}
