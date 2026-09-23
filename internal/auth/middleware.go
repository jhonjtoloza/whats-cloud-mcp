package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/apierr"
)

// ErrKeyNotFound is returned by a KeyResolver when no live key matches a hash.
// Revoked keys must also report ErrKeyNotFound.
var ErrKeyNotFound = errors.New("auth: api key not found")

// Principal is the authenticated identity behind a request.
type Principal struct {
	// IsAdmin is true for the shared admin token. Admin principals are not
	// bound to any tenant and must never be accepted on tenant endpoints.
	IsAdmin bool
	// TenantID is the tenant that owns the API key. It comes exclusively from
	// the stored key record, never from the request, which is what makes
	// cross-tenant access impossible.
	TenantID string
	// KeyID identifies the API key record, for auditing.
	KeyID string
	// Scopes are the grants attached to the key.
	Scopes []string
}

// HasScope reports whether this principal satisfies the required scope.
func (p Principal) HasScope(required string) bool {
	return HasScope(p.Scopes, required)
}

// KeyResolver looks up a live API key by its SHA-256 hash.
type KeyResolver interface {
	// ResolveKeyHash returns the principal behind a key hash, or ErrKeyNotFound
	// if the key is unknown or revoked.
	ResolveKeyHash(ctx context.Context, keyHash string) (Principal, error)
}

// KeyUsageRecorder is optionally implemented by a KeyResolver to record when a
// key was last used. Failures are non-fatal.
type KeyUsageRecorder interface {
	MarkKeyUsed(ctx context.Context, keyID string) error
}

type principalCtxKey struct{}

// WithPrincipal returns a context carrying the given principal.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom extracts the principal placed by the middleware.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

// Middleware turns bearer tokens into principals.
type Middleware struct {
	adminToken string
	resolver   KeyResolver
	logger     *slog.Logger
}

// New builds a Middleware. A nil logger falls back to slog.Default.
func New(adminToken string, resolver KeyResolver, logger *slog.Logger) *Middleware {
	if logger == nil {
		logger = slog.Default()
	}
	return &Middleware{adminToken: adminToken, resolver: resolver, logger: logger}
}

// BearerToken extracts the credential from an Authorization header. It returns
// an empty string when the header is absent or does not use the Bearer scheme.
func BearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// RequireAdmin only admits the configured admin token. Tenant API keys are
// rejected here even when they carry a wildcard scope: admin is a separate
// credential class, not a stronger tenant.
func (m *Middleware) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := BearerToken(r)
		if !VerifyAdminToken(m.adminToken, token) {
			m.logger.WarnContext(r.Context(), "admin authentication rejected",
				slog.String("path", r.URL.Path), slog.String("method", r.Method))
			apierr.Unauthorized(w, "admin credentials required")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), Principal{IsAdmin: true})))
	})
}

// RequireScope admits a tenant API key that carries the required scope. The
// admin token is deliberately NOT accepted: admins manage tenants, they do not
// act as them.
func (m *Middleware) RequireScope(required string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, ok := m.authenticateTenant(w, r)
			if !ok {
				return
			}

			if !principal.HasScope(required) {
				m.logger.WarnContext(r.Context(), "scope denied",
					slog.String("tenant_id", principal.TenantID),
					slog.String("key_id", principal.KeyID),
					slog.String("required_scope", required))
				apierr.Forbidden(w, "missing required scope: "+required)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}

// RequireTenant admits any live tenant API key without demanding a particular
// scope.
//
// It exists for endpoints where one URL serves several operations with
// different scope requirements — the MCP endpoint, where the three tools split
// between messages:read and messages:send. Such a handler MUST perform its own
// per-operation scope check against the principal; authentication alone
// authorises nothing.
func (m *Middleware) RequireTenant(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := m.authenticateTenant(w, r)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
	})
}

// authenticateTenant resolves the bearer token into a tenant principal,
// writing the error response itself when it cannot. The returned bool reports
// whether the caller should continue.
func (m *Middleware) authenticateTenant(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	ctx := r.Context()

	token := BearerToken(r)
	if token == "" {
		apierr.Unauthorized(w, "api key required")
		return Principal{}, false
	}

	principal, err := m.resolver.ResolveKeyHash(ctx, HashKey(token))
	switch {
	case errors.Is(err, ErrKeyNotFound):
		m.logger.WarnContext(ctx, "api key rejected",
			slog.String("path", r.URL.Path), slog.String("method", r.Method))
		apierr.Unauthorized(w, "invalid api key")
		return Principal{}, false
	case err != nil:
		m.logger.ErrorContext(ctx, "api key lookup failed", slog.String("error", err.Error()))
		apierr.Internal(w)
		return Principal{}, false
	}

	if recorder, ok := m.resolver.(KeyUsageRecorder); ok {
		if err := recorder.MarkKeyUsed(ctx, principal.KeyID); err != nil {
			m.logger.WarnContext(ctx, "could not record api key usage",
				slog.String("key_id", principal.KeyID), slog.String("error", err.Error()))
		}
	}

	return principal, true
}
