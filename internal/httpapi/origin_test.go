package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/httpapi"
)

// TestRequireAllowedOrigin covers the DNS-rebinding defence the MCP Streamable
// HTTP spec requires: a browser page on an attacker's origin must not be able
// to drive a locally bound MCP server.
func TestRequireAllowedOrigin(t *testing.T) {
	allowed := []string{"https://claude.ai", "https://app.example.com"}

	tests := []struct {
		name       string
		allowed    []string
		origin     string
		setHeader  bool
		wantStatus int
	}{
		{
			name:       "an allowed origin passes",
			allowed:    allowed,
			origin:     "https://claude.ai",
			setHeader:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "a second allowed origin passes",
			allowed:    allowed,
			origin:     "https://app.example.com",
			setHeader:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "an unlisted origin is rejected",
			allowed:    allowed,
			origin:     "https://evil.example.com",
			setHeader:  true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a rebinding attempt from a local page is rejected",
			allowed:    allowed,
			origin:     "http://localhost:3000",
			setHeader:  true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "an absent Origin passes: non-browser clients send none",
			allowed:    allowed,
			setHeader:  false,
			wantStatus: http.StatusOK,
		},
		{
			name:       "an empty Origin header is treated as absent",
			allowed:    allowed,
			origin:     "",
			setHeader:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "with no allowlist every browser origin is rejected",
			allowed:    nil,
			origin:     "https://claude.ai",
			setHeader:  true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "with no allowlist a client sending no Origin still passes",
			allowed:    nil,
			setHeader:  false,
			wantStatus: http.StatusOK,
		},
		{
			name:       "matching is exact, not a prefix",
			allowed:    allowed,
			origin:     "https://claude.ai.evil.example.com",
			setHeader:  true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "matching is case insensitive on scheme and host",
			allowed:    allowed,
			origin:     "https://CLAUDE.ai",
			setHeader:  true,
			wantStatus: http.StatusOK,
		},
		{
			name:       "the null origin is rejected",
			allowed:    allowed,
			origin:     "null",
			setHeader:  true,
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := httpapi.RequireAllowedOrigin(tc.allowed)(http.HandlerFunc(
				func(w http.ResponseWriter, _ *http.Request) {
					called = true
					w.WriteHeader(http.StatusOK)
				}))

			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tc.setHeader {
				req.Header.Set("Origin", tc.origin)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if called != (tc.wantStatus == http.StatusOK) {
				t.Errorf("next handler called = %v, want %v", called, tc.wantStatus == http.StatusOK)
			}
			if tc.wantStatus == http.StatusForbidden {
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
			}
		})
	}
}
