package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// headerTransport injects the credentials an MCP client would carry.
type headerTransport struct {
	base   http.RoundTripper
	bearer string
	origin string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	if t.bearer != "" {
		clone.Header.Set("Authorization", "Bearer "+t.bearer)
	}
	if t.origin != "" {
		clone.Header.Set("Origin", t.origin)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(clone)
}

// connectMCP starts the gateway over real HTTP and returns a connected MCP
// client session, exercising the actual Streamable HTTP transport rather than a
// stand-in.
func connectMCP(t *testing.T, env *testEnv, bearer, origin string) *mcp.ClientSession {
	t.Helper()

	srv := httptest.NewServer(env.handler)
	t.Cleanup(srv.Close)

	httpClient := &http.Client{
		Transport: &headerTransport{bearer: bearer, origin: origin},
		Timeout:   10 * time.Second,
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0.0.1"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: httpClient,
		// The gateway runs the MCP server in stateless mode, where GET returns
		// 405 by design, so the client must not open a standalone SSE stream.
		DisableStandaloneSSE: true,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("MCP Connect() error = %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// callTool invokes a tool and returns the result.
func callTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s) error = %v", name, err)
	}
	return res
}

// resultText flattens a tool result's text content.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()

	var sb strings.Builder
	for _, c := range res.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(text.Text)
		}
	}
	return sb.String()
}

// structured decodes a tool's structured output.
func structured[T any](t *testing.T, res *mcp.CallToolResult) T {
	t.Helper()

	if res.IsError {
		t.Fatalf("tool returned an error: %s", resultText(t, res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode structured content %s: %v", raw, err)
	}
	return out
}

type mcpChats struct {
	Chats []struct {
		ChatJID         string `json:"chat_jid"`
		LastMessageBody string `json:"last_message_body"`
		MessageCount    int    `json:"message_count"`
	} `json:"chats"`
}

type mcpMessages struct {
	ChatJID  string `json:"chat_jid"`
	Messages []struct {
		ID        string `json:"id"`
		Body      string `json:"body"`
		Direction string `json:"direction"`
	} `json:"messages"`
}

func TestMCPEndpointRequiresAuthentication(t *testing.T) {
	env := newTestEnv(t)
	srv := httptest.NewServer(env.handler)
	defer srv.Close()

	tests := []struct {
		name   string
		bearer string
	}{
		{"no credential", ""},
		{"unknown api key", "wc_live_tenanta_bogus"},
		{"the admin token is not a tenant", adminToken},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("request error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

// TestMCPEndpointValidatesOrigin is the DNS-rebinding defence at the real
// endpoint, not just at the middleware in isolation.
func TestMCPEndpointValidatesOrigin(t *testing.T) {
	env := newTestEnvWithOrigins(t, []string{"https://claude.ai"})
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	srv := httptest.NewServer(env.handler)
	defer srv.Close()

	tests := []struct {
		name       string
		origin     string
		setOrigin  bool
		wantStatus int
	}{
		{"an allowed origin is let through", "https://claude.ai", true, http.StatusOK},
		{"an attacker origin is rejected", "https://evil.example.com", true, http.StatusForbidden},
		{"a local page cannot rebind onto the gateway", "http://localhost:3000", true, http.StatusForbidden},
		{"a non-browser client sending no Origin is let through", "", false, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			req.Header.Set("Authorization", "Bearer "+tenant.APIKey.Key)
			if tc.setOrigin {
				req.Header.Set("Origin", tc.origin)
			}

			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatalf("request error: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestMCPListsTheThreeTools(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read", "messages:send"})
	session := connectMCP(t, env, tenant.APIKey.Key, "")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	res, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}

	got := make(map[string]bool, len(res.Tools))
	for _, tool := range res.Tools {
		got[tool.Name] = true
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema; AddTool should generate one", tool.Name)
		}
	}
	for _, want := range []string{"list_chats", "list_messages", "send_message"} {
		if !got[want] {
			t.Errorf("tool %q is missing; got %v", want, got)
		}
	}
}

func TestMCPListChatsAndMessages(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:read"})

	const chatJID = "5215550001111@s.whatsapp.net"
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	seedChat(t, env, tenant.TenantID, chatJID, "first", base)
	seedChat(t, env, tenant.TenantID, chatJID, "second", base.Add(time.Minute))

	session := connectMCP(t, env, tenant.APIKey.Key, "")

	chats := structured[mcpChats](t, callTool(t, session, "list_chats", map[string]any{}))
	if len(chats.Chats) != 1 {
		t.Fatalf("list_chats returned %d chats, want 1", len(chats.Chats))
	}
	if chats.Chats[0].ChatJID != chatJID {
		t.Errorf("chat_jid = %q, want %q", chats.Chats[0].ChatJID, chatJID)
	}
	if chats.Chats[0].MessageCount != 2 {
		t.Errorf("message_count = %d, want 2", chats.Chats[0].MessageCount)
	}

	messages := structured[mcpMessages](t, callTool(t, session, "list_messages", map[string]any{
		"chat_jid": chatJID,
	}))
	if len(messages.Messages) != 2 {
		t.Fatalf("list_messages returned %d messages, want 2", len(messages.Messages))
	}
	if messages.Messages[0].Body != "second" {
		t.Errorf("messages[0].Body = %q, want %q (newest first)", messages.Messages[0].Body, "second")
	}
}

func TestMCPSendMessage(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})
	session := connectMCP(t, env, tenant.APIKey.Key, "")

	res := callTool(t, session, "send_message", map[string]any{
		"to":   "5215550002222@s.whatsapp.net",
		"body": "sent through mcp",
	})
	if res.IsError {
		t.Fatalf("send_message failed: %s", resultText(t, res))
	}

	calls := env.sessions.snapshotSends()
	if len(calls) != 1 {
		t.Fatalf("SendText called %d times, want 1", len(calls))
	}
	if calls[0].TenantID != tenant.TenantID {
		t.Errorf("sent as tenant %q, want %q", calls[0].TenantID, tenant.TenantID)
	}
	if calls[0].Body != "sent through mcp" {
		t.Errorf("body = %q", calls[0].Body)
	}

	// The outbound message is recorded just as the REST path records it.
	stored, err := env.db.Messages().ListByChat(context.Background(), tenant.TenantID, "5215550002222@s.whatsapp.net", 10)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("persisted %d outbound messages, want 1", len(stored))
	}
}

// TestMCPEnforcesScopePerTool is the reason the endpoint-level middleware only
// authenticates: one URL serves tools with different scope requirements, so
// each tool must check for itself.
func TestMCPEnforcesScopePerTool(t *testing.T) {
	tests := []struct {
		name       string
		scopes     []string
		tool       string
		args       map[string]any
		wantErr    bool
		wantReason string
	}{
		{
			name:       "send-only key cannot list messages",
			scopes:     []string{"messages:send"},
			tool:       "list_messages",
			args:       map[string]any{"chat_jid": "x@s.whatsapp.net"},
			wantErr:    true,
			wantReason: "messages:read",
		},
		{
			name:       "send-only key cannot list chats",
			scopes:     []string{"messages:send"},
			tool:       "list_chats",
			args:       map[string]any{},
			wantErr:    true,
			wantReason: "messages:read",
		},
		{
			name:       "read-only key cannot send",
			scopes:     []string{"messages:read"},
			tool:       "send_message",
			args:       map[string]any{"to": "x@s.whatsapp.net", "body": "hi"},
			wantErr:    true,
			wantReason: "messages:send",
		},
		{
			name:    "read key may list chats",
			scopes:  []string{"messages:read"},
			tool:    "list_chats",
			args:    map[string]any{},
			wantErr: false,
		},
		{
			name:    "send key may send",
			scopes:  []string{"messages:send"},
			tool:    "send_message",
			args:    map[string]any{"to": "x@s.whatsapp.net", "body": "hi"},
			wantErr: false,
		},
		{
			name:    "a wildcard key may do both",
			scopes:  []string{"messages:read", "messages:send"},
			tool:    "list_chats",
			args:    map[string]any{},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", tc.scopes)
			session := connectMCP(t, env, tenant.APIKey.Key, "")

			res := callTool(t, session, tc.tool, tc.args)

			if res.IsError != tc.wantErr {
				t.Fatalf("IsError = %v, want %v (content %q)", res.IsError, tc.wantErr, resultText(t, res))
			}
			if !tc.wantErr {
				return
			}
			text := resultText(t, res)
			if !strings.Contains(text, tc.wantReason) {
				t.Errorf("error %q should name the missing scope %q", text, tc.wantReason)
			}
			// A denied call must not leak data alongside the error.
			if res.StructuredContent != nil {
				if raw, _ := json.Marshal(res.StructuredContent); strings.Contains(string(raw), "chat_jid\":\"5") {
					t.Errorf("a denied tool returned data: %s", raw)
				}
			}
		})
	}
}

// TestMCPTenantIsolation is the multi-tenant guarantee through MCP, mirroring
// the REST test. It runs both directions so it cannot pass by accident.
func TestMCPTenantIsolation(t *testing.T) {
	env := newTestEnv(t)

	tenantA := env.createTenant(t, "Acme", []string{"messages:read"})
	tenantB := env.createTenant(t, "Globex", []string{"messages:read"})

	const sharedChat = "5215550009999@s.whatsapp.net"
	at := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	seedChat(t, env, tenantA.TenantID, sharedChat, "message for acme", at)
	seedChat(t, env, tenantB.TenantID, sharedChat, "message for globex", at.Add(time.Minute))

	cases := []struct {
		name     string
		key      string
		wantBody string
		notBody  string
	}{
		{"tenant A sees only its own", tenantA.APIKey.Key, "message for acme", "message for globex"},
		{"tenant B sees only its own", tenantB.APIKey.Key, "message for globex", "message for acme"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := connectMCP(t, env, tc.key, "")

			res := callTool(t, session, "list_messages", map[string]any{"chat_jid": sharedChat})
			if res.IsError {
				t.Fatalf("list_messages failed: %s", resultText(t, res))
			}

			raw, err := json.Marshal(res.StructuredContent)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(raw), tc.notBody) {
				t.Errorf("MCP leaked the other tenant's message: %s", raw)
			}

			messages := structured[mcpMessages](t, res)
			if len(messages.Messages) != 1 {
				t.Fatalf("got %d messages, want exactly 1 (its own)", len(messages.Messages))
			}
			if messages.Messages[0].Body != tc.wantBody {
				t.Errorf("body = %q, want %q", messages.Messages[0].Body, tc.wantBody)
			}
		})
	}
}

// TestMCPToolsRejectATenantArgument proves the tenant cannot be chosen by the
// caller: an unexpected tenant_id argument must not redirect the read.
func TestMCPToolsIgnoreClientSuppliedTenant(t *testing.T) {
	env := newTestEnv(t)

	tenantA := env.createTenant(t, "Acme", []string{"messages:read"})
	tenantB := env.createTenant(t, "Globex", []string{"messages:read"})

	const sharedChat = "5215550009999@s.whatsapp.net"
	at := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	seedChat(t, env, tenantB.TenantID, sharedChat, "message for globex", at)

	session := connectMCP(t, env, tenantA.APIKey.Key, "")

	res := callTool(t, session, "list_messages", map[string]any{
		"chat_jid":  sharedChat,
		"tenant_id": tenantB.TenantID,
	})

	// Either the schema rejects the unknown argument or the tool ignores it.
	// What must never happen is tenant B's data coming back.
	if raw, err := json.Marshal(res.StructuredContent); err == nil {
		if strings.Contains(string(raw), "message for globex") {
			t.Fatalf("a client-supplied tenant_id redirected the read: %s", raw)
		}
	}
	if strings.Contains(resultText(t, res), "message for globex") {
		t.Fatal("a client-supplied tenant_id redirected the read")
	}
}
