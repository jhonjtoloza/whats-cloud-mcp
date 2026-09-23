package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// stubResolver resolves API key hashes without touching a database.
type stubResolver struct {
	byHash    map[string]Principal
	lastUsed  []string
	failWith  error
	callCount int
}

func (s *stubResolver) ResolveKeyHash(_ context.Context, keyHash string) (Principal, error) {
	s.callCount++
	if s.failWith != nil {
		return Principal{}, s.failWith
	}
	p, ok := s.byHash[keyHash]
	if !ok {
		return Principal{}, ErrKeyNotFound
	}
	return p, nil
}

func (s *stubResolver) MarkKeyUsed(_ context.Context, keyID string) error {
	s.lastUsed = append(s.lastUsed, keyID)
	return nil
}

func okHandler(t *testing.T, onCall func(*http.Request)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if onCall != nil {
			onCall(r)
		}
		w.WriteHeader(http.StatusOK)
	})
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"standard bearer", "Bearer abc123", "abc123"},
		{"lowercase scheme", "bearer abc123", "abc123"},
		{"mixed case scheme", "BeArEr abc123", "abc123"},
		{"extra whitespace", "Bearer    abc123   ", "abc123"},
		{"missing header", "", ""},
		{"wrong scheme", "Basic abc123", ""},
		{"scheme only", "Bearer", ""},
		{"scheme with no token", "Bearer   ", ""},
		{"raw token without scheme", "abc123", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			if got := BearerToken(req); got != tc.want {
				t.Errorf("BearerToken() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRequireAdmin(t *testing.T) {
	const adminToken = "admin-token-value"

	tenantKey, err := GenerateKey("tenant-a")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	resolver := &stubResolver{byHash: map[string]Principal{
		tenantKey.Hash: {TenantID: "tenant-a", KeyID: "key-a", Scopes: []string{"*"}},
	}}

	tests := []struct {
		name       string
		adminToken string
		header     string
		wantStatus int
	}{
		{"valid admin token", adminToken, "Bearer " + adminToken, http.StatusOK},
		{"wrong admin token", adminToken, "Bearer nope", http.StatusUnauthorized},
		{"missing header", adminToken, "", http.StatusUnauthorized},
		{"tenant key is not admin even with wildcard scope", adminToken, "Bearer " + tenantKey.Plaintext, http.StatusUnauthorized},
		{"unset admin token locks the admin surface", "", "Bearer " + adminToken, http.StatusUnauthorized},
		{"unset admin token rejects empty credential", "", "Bearer ", http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			mw := New(tc.adminToken, resolver, nil)
			h := mw.RequireAdmin(okHandler(t, func(*http.Request) { called = true }))

			req := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if called != (tc.wantStatus == http.StatusOK) {
				t.Errorf("next handler called = %v, want %v", called, tc.wantStatus == http.StatusOK)
			}
			if rec.Code != http.StatusOK {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
			}
		})
	}
}

func TestRequireAdminMarksRequestAsAdminPrincipal(t *testing.T) {
	mw := New("admin-token", &stubResolver{}, nil)

	var got Principal
	var ok bool
	h := mw.RequireAdmin(okHandler(t, func(r *http.Request) {
		got, ok = PrincipalFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/tenants", nil)
	req.Header.Set("Authorization", "Bearer admin-token")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !ok {
		t.Fatal("expected a principal in the request context")
	}
	if !got.IsAdmin {
		t.Error("admin principal should have IsAdmin = true")
	}
	if got.TenantID != "" {
		t.Errorf("admin principal TenantID = %q, want empty: admins are not scoped to a tenant", got.TenantID)
	}
}

func TestRequireScope(t *testing.T) {
	const adminToken = "admin-token-value"

	sendKey, err := GenerateKey("tenant-a")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	mediaKey, err := GenerateKey("tenant-b")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	resolver := &stubResolver{byHash: map[string]Principal{
		sendKey.Hash:  {TenantID: "tenant-a", KeyID: "key-a", Scopes: []string{"messages:send"}},
		mediaKey.Hash: {TenantID: "tenant-b", KeyID: "key-b", Scopes: []string{"media:*"}},
	}}

	tests := []struct {
		name       string
		required   string
		header     string
		wantStatus int
	}{
		{"granted scope passes", ScopeMessagesSend, "Bearer " + sendKey.Plaintext, http.StatusOK},
		{"missing scope is forbidden", ScopeMessagesRead, "Bearer " + sendKey.Plaintext, http.StatusForbidden},
		{"wildcard scope passes", ScopeMediaRead, "Bearer " + mediaKey.Plaintext, http.StatusOK},
		{"wildcard does not cross resources", ScopeMessagesSend, "Bearer " + mediaKey.Plaintext, http.StatusForbidden},
		{"unknown key is unauthorized", ScopeMessagesSend, "Bearer wc_live_tenanta_unknown", http.StatusUnauthorized},
		{"missing credential is unauthorized", ScopeMessagesSend, "", http.StatusUnauthorized},
		{"admin token cannot use tenant endpoints", ScopeMessagesSend, "Bearer " + adminToken, http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mw := New(adminToken, resolver, nil)
			h := mw.RequireScope(tc.required)(okHandler(t, nil))

			req := httptest.NewRequest(http.MethodGet, "/v1/chats", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestRequireScopeBindsPrincipalToItsOwnTenant is the multi-tenant isolation
// guarantee: a key always resolves to the tenant that owns it, so a handler can
// never be tricked into serving another tenant's data.
func TestRequireScopeBindsPrincipalToItsOwnTenant(t *testing.T) {
	keyA, err := GenerateKey("tenant-a")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	keyB, err := GenerateKey("tenant-b")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	resolver := &stubResolver{byHash: map[string]Principal{
		keyA.Hash: {TenantID: "tenant-a", KeyID: "key-a", Scopes: []string{"messages:read"}},
		keyB.Hash: {TenantID: "tenant-b", KeyID: "key-b", Scopes: []string{"messages:read"}},
	}}

	tests := []struct {
		name       string
		plaintext  string
		wantTenant string
	}{
		{"key a resolves to tenant a", keyA.Plaintext, "tenant-a"},
		{"key b resolves to tenant b", keyB.Plaintext, "tenant-b"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mw := New("admin-token", resolver, nil)

			var got Principal
			h := mw.RequireScope(ScopeMessagesRead)(okHandler(t, func(r *http.Request) {
				got, _ = PrincipalFrom(r.Context())
			}))

			req := httptest.NewRequest(http.MethodGet, "/v1/chats?tenant_id=tenant-b", nil)
			req.Header.Set("Authorization", "Bearer "+tc.plaintext)
			req.Header.Set("X-Tenant-Id", "tenant-b")
			h.ServeHTTP(httptest.NewRecorder(), req)

			if got.TenantID != tc.wantTenant {
				t.Errorf("principal tenant = %q, want %q: the tenant must come from the key, never from the request", got.TenantID, tc.wantTenant)
			}
			if got.IsAdmin {
				t.Error("a tenant key must never yield an admin principal")
			}
		})
	}
}

func TestRequireScopeReportsResolverFailureAsServerError(t *testing.T) {
	key, err := GenerateKey("tenant-a")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	resolver := &stubResolver{failWith: context.DeadlineExceeded}

	mw := New("admin-token", resolver, nil)
	h := mw.RequireScope(ScopeMessagesSend)(okHandler(t, nil))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestRequireScopeMarksKeyUsed(t *testing.T) {
	key, err := GenerateKey("tenant-a")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	resolver := &stubResolver{byHash: map[string]Principal{
		key.Hash: {TenantID: "tenant-a", KeyID: "key-a", Scopes: []string{"messages:send"}},
	}}

	mw := New("admin-token", resolver, nil)
	h := mw.RequireScope(ScopeMessagesSend)(okHandler(t, nil))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if len(resolver.lastUsed) != 1 || resolver.lastUsed[0] != "key-a" {
		t.Errorf("MarkKeyUsed calls = %#v, want exactly [key-a]", resolver.lastUsed)
	}
}

func TestPrincipalFromEmptyContext(t *testing.T) {
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Error("PrincipalFrom(background) should report no principal")
	}
}

// RequireTenant authenticates without demanding a particular scope. It exists
// for the MCP endpoint, where one URL serves several tools whose scope
// requirements differ, so the scope check has to happen per tool instead.
func TestRequireTenant(t *testing.T) {
	const adminToken = "admin-token-value"

	key, err := GenerateKey("tenant-a")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	scopeless, err := GenerateKey("tenant-c")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}

	resolver := &stubResolver{byHash: map[string]Principal{
		key.Hash:       {TenantID: "tenant-a", KeyID: "key-a", Scopes: []string{"messages:send"}},
		scopeless.Hash: {TenantID: "tenant-c", KeyID: "key-c", Scopes: nil},
	}}

	tests := []struct {
		name       string
		header     string
		wantStatus int
	}{
		{"a valid tenant key passes whatever its scopes", "Bearer " + key.Plaintext, http.StatusOK},
		{"a key with no scopes still authenticates", "Bearer " + scopeless.Plaintext, http.StatusOK},
		{"an unknown key is unauthorized", "Bearer wc_live_tenanta_unknown", http.StatusUnauthorized},
		{"a missing credential is unauthorized", "", http.StatusUnauthorized},
		{"the admin token is not a tenant", "Bearer " + adminToken, http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mw := New(adminToken, resolver, nil)
			h := mw.RequireTenant(okHandler(t, nil))

			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestRequireTenantBindsPrincipalToItsOwnTenant(t *testing.T) {
	key, err := GenerateKey("tenant-a")
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	resolver := &stubResolver{byHash: map[string]Principal{
		key.Hash: {TenantID: "tenant-a", KeyID: "key-a", Scopes: []string{"messages:read"}},
	}}

	mw := New("admin-token", resolver, nil)

	var got Principal
	var ok bool
	h := mw.RequireTenant(okHandler(t, func(r *http.Request) {
		got, ok = PrincipalFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+key.Plaintext)
	req.Header.Set("X-Tenant-Id", "tenant-b")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !ok {
		t.Fatal("expected a principal in the request context")
	}
	if got.TenantID != "tenant-a" {
		t.Errorf("principal tenant = %q, want tenant-a: it must come from the key", got.TenantID)
	}
	if !got.HasScope(ScopeMessagesRead) || got.HasScope(ScopeMessagesSend) {
		t.Errorf("scopes = %#v, want exactly the key's own grants", got.Scopes)
	}
}
