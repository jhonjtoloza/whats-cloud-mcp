// Package httpapi exposes the gateway over HTTP using the standard library
// only: net/http with Go 1.22 ServeMux pattern routing. No framework.
package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/apierr"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/auth"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/mcpserver"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// Config wires the server's dependencies.
type Config struct {
	// AdminToken is the shared secret for the admin credential class. It is
	// required: an empty token would leave the admin surface open.
	AdminToken string
	// DB is the persistence layer.
	DB *store.DB
	// Sessions is the WhatsApp engine, behind an interface so tests can use a fake.
	Sessions wa.SessionManager
	// Logger defaults to slog.Default when nil.
	Logger *slog.Logger
	// DefaultScopes are granted to a key created without an explicit scope list.
	DefaultScopes []string
	// MCPAllowedOrigins is the Origin allowlist for /mcp. Empty means no
	// browser origin is accepted, which is the safe default for a server that
	// non-browser MCP clients reach without an Origin header at all.
	MCPAllowedOrigins []string
}

// Server holds the routed HTTP surface.
type Server struct {
	db            *store.DB
	sessions      wa.SessionManager
	mw            *auth.Middleware
	logger        *slog.Logger
	defaultScopes []string
	mcpOrigins    []string
	handler       http.Handler
}

// New validates the configuration and builds the server.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.AdminToken == "":
		return nil, errors.New("httpapi: admin token is required")
	case cfg.DB == nil:
		return nil, errors.New("httpapi: database is required")
	case cfg.Sessions == nil:
		return nil, errors.New("httpapi: session manager is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	defaultScopes := cfg.DefaultScopes
	if len(defaultScopes) == 0 {
		defaultScopes = []string{auth.ScopeMessagesSend}
	}
	if err := auth.ValidateScopes(defaultScopes); err != nil {
		return nil, err
	}

	s := &Server{
		db:            cfg.DB,
		sessions:      cfg.Sessions,
		mw:            auth.New(cfg.AdminToken, keyResolver{keys: cfg.DB.APIKeys()}, logger),
		logger:        logger,
		defaultScopes: defaultScopes,
		mcpOrigins:    cfg.MCPAllowedOrigins,
	}
	s.handler = jsonErrors(s.routes())
	return s, nil
}

// Handler returns the configured http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	// Public.
	mux.HandleFunc("GET /healthz", s.handleHealthz)

	// Admin surface: manages tenants, keys and pairing. It can never send or
	// read messages.
	admin := s.mw.RequireAdmin
	mux.Handle("POST /v1/tenants", admin(http.HandlerFunc(s.handleCreateTenant)))
	mux.Handle("POST /v1/tenants/{id}/keys", admin(http.HandlerFunc(s.handleIssueKey)))
	mux.Handle("DELETE /v1/tenants/{id}/keys/{keyID}", admin(http.HandlerFunc(s.handleRevokeKey)))
	mux.Handle("POST /v1/tenants/{id}/sessions/pair", admin(http.HandlerFunc(s.handleStartPairing)))

	// Tenant surface: scoped to the caller's own tenant, always.
	scope := s.mw.RequireScope
	mux.Handle("GET /v1/sessions", scope(auth.ScopeSessionsRead)(http.HandlerFunc(s.handleGetSession)))
	mux.Handle("POST /v1/messages", scope(auth.ScopeMessagesSend)(http.HandlerFunc(s.handleSendMessage)))
	mux.Handle("GET /v1/chats", scope(auth.ScopeMessagesRead)(http.HandlerFunc(s.handleListChats)))
	mux.Handle("GET /v1/chats/{jid}/messages", scope(auth.ScopeMessagesRead)(http.HandlerFunc(s.handleListChatMessages)))

	// MCP over Streamable HTTP, in this same process.
	//
	// Layered outermost first: Origin validation (DNS-rebinding defence),
	// then tenant authentication, then the SDK handler. The endpoint cannot
	// demand a single scope because its three tools differ, so each tool
	// performs its own scope check against the principal.
	mux.Handle("/mcp", RequireAllowedOrigin(s.mcpOrigins)(s.mw.RequireTenant(s.mcpHandler())))

	return mux
}

// mcpHandler builds the Streamable HTTP handler for the in-process MCP server.
func (s *Server) mcpHandler() http.Handler {
	server := mcpserver.New(mcpserver.Deps{
		Messages: s.db.Messages(),
		Sessions: s.sessions,
		Logger:   s.logger,
	})

	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		&mcp.StreamableHTTPOptions{
			// Stateless mode matters for correctness here, not just for
			// memory. A stateful session captures the context of the request
			// that created it, so every later tool call in that session would
			// see the principal of whoever ran `initialize`. Stateless rebuilds
			// the session from each request's own context, so a tool always
			// sees the tenant that authenticated THIS call and a replayed
			// Mcp-Session-Id cannot cross tenants.
			Stateless: true,
			Logger:    s.logger,
		},
	)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// keyResolver adapts the API key repository to the auth middleware.
type keyResolver struct{ keys store.APIKeys }

func (r keyResolver) ResolveKeyHash(ctx context.Context, keyHash string) (auth.Principal, error) {
	key, err := r.keys.GetByHash(ctx, keyHash)
	if errors.Is(err, store.ErrNotFound) {
		return auth.Principal{}, auth.ErrKeyNotFound
	}
	if err != nil {
		return auth.Principal{}, err
	}
	return auth.Principal{
		TenantID: key.TenantID,
		KeyID:    key.ID,
		Scopes:   key.Scopes,
	}, nil
}

func (r keyResolver) MarkKeyUsed(ctx context.Context, keyID string) error {
	return r.keys.MarkUsed(ctx, keyID)
}

// jsonErrors converts the router's own plain-text 404 and 405 responses into
// the gateway's JSON error shape, so clients only ever parse one format.
func jsonErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&errorRewriter{ResponseWriter: w}, r)
	})
}

type errorRewriter struct {
	http.ResponseWriter
	rewritten bool
}

func (w *errorRewriter) WriteHeader(status int) {
	// Handlers that already produce JSON set Content-Type first, so they are
	// left alone; only the router's own text/plain replies are rewritten.
	if w.Header().Get("Content-Type") == "application/json" {
		w.ResponseWriter.WriteHeader(status)
		return
	}

	switch status {
	case http.StatusNotFound:
		w.rewritten = true
		apierr.Write(w.ResponseWriter, status, apierr.CodeNotFound, "no such endpoint")
	case http.StatusMethodNotAllowed:
		w.rewritten = true
		apierr.Write(w.ResponseWriter, status, apierr.CodeInvalidRequest, "method not allowed for this endpoint")
	default:
		w.ResponseWriter.WriteHeader(status)
	}
}

func (w *errorRewriter) Write(b []byte) (int, error) {
	if w.rewritten {
		// Swallow the router's plain-text body; ours is already written.
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}
