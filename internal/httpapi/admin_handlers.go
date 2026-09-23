package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/apierr"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/auth"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

type createTenantRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// issuedKey is the only representation that ever carries the plaintext key, and
// it is returned exactly once, at creation time.
type issuedKey struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	KeyPrefix string   `json:"key_prefix"`
	Scopes    []string `json:"scopes"`
	Key       string   `json:"key"`
}

type createTenantResponse struct {
	TenantID  string    `json:"tenant_id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	APIKey    issuedKey `json:"api_key"`
}

// handleCreateTenant provisions a tenant plus its first API key.
func (s *Server) handleCreateTenant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req createTenantRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.InvalidRequest(w, "the request body must be a JSON object")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		apierr.InvalidRequest(w, "name is required")
		return
	}

	scopes := s.resolveScopes(req.Scopes)
	if err := auth.ValidateScopes(scopes); err != nil {
		apierr.InvalidRequest(w, err.Error())
		return
	}

	tenant := store.Tenant{ID: store.NewID(), Name: name, CreatedAt: time.Now().UTC()}
	if err := s.db.Tenants().Create(ctx, tenant); err != nil {
		s.logger.ErrorContext(ctx, "could not create tenant", slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	// Every tenant gets a session row up front so pairing only ever updates it.
	if _, err := s.db.Sessions().EnsureForTenant(ctx, tenant.ID); err != nil {
		s.logger.ErrorContext(ctx, "could not create session row",
			slog.String("tenant_id", tenant.ID), slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	issued, err := s.issueKey(r, tenant.ID, "default", scopes)
	if err != nil {
		apierr.Internal(w)
		return
	}

	s.logger.InfoContext(ctx, "tenant created",
		slog.String("tenant_id", tenant.ID), slog.String("key_prefix", issued.KeyPrefix))

	writeJSON(w, http.StatusCreated, createTenantResponse{
		TenantID:  tenant.ID,
		Name:      tenant.Name,
		CreatedAt: tenant.CreatedAt,
		APIKey:    issued,
	})
}

type issueKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// handleIssueKey mints an additional scoped key for an existing tenant.
func (s *Server) handleIssueKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := r.PathValue("id")

	if _, err := s.db.Tenants().Get(ctx, tenantID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apierr.NotFound(w, "tenant not found")
			return
		}
		s.logger.ErrorContext(ctx, "could not load tenant", slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	var req issueKeyRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		apierr.InvalidRequest(w, "the request body must be a JSON object")
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		apierr.InvalidRequest(w, "name is required")
		return
	}

	scopes := s.resolveScopes(req.Scopes)
	if err := auth.ValidateScopes(scopes); err != nil {
		apierr.InvalidRequest(w, err.Error())
		return
	}

	issued, err := s.issueKey(r, tenantID, name, scopes)
	if err != nil {
		apierr.Internal(w)
		return
	}

	s.logger.InfoContext(ctx, "api key issued",
		slog.String("tenant_id", tenantID), slog.String("key_prefix", issued.KeyPrefix))

	writeJSON(w, http.StatusCreated, issued)
}

// handleRevokeKey revokes a key through its owning tenant.
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := r.PathValue("id")
	keyID := r.PathValue("keyID")

	err := s.db.APIKeys().Revoke(ctx, tenantID, keyID)
	if errors.Is(err, store.ErrNotFound) {
		apierr.NotFound(w, "api key not found for this tenant")
		return
	}
	if err != nil {
		s.logger.ErrorContext(ctx, "could not revoke api key", slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	s.logger.InfoContext(ctx, "api key revoked",
		slog.String("tenant_id", tenantID), slog.String("key_id", keyID))
	w.WriteHeader(http.StatusNoContent)
}

type startPairingRequest struct {
	// Phone is an E.164 number without a leading plus. When present the
	// pair-code flow is used, which is the preferred one; leaving it empty
	// falls back to QR.
	Phone string `json:"phone"`
}

// handleStartPairing begins pairing for a tenant.
//
// Pairing only ever moves sessions.status. It never touches api_keys: keys
// belong to the tenant, so re-pairing a number keeps every credential valid.
func (s *Server) handleStartPairing(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tenantID := r.PathValue("id")

	if _, err := s.db.Tenants().Get(ctx, tenantID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			apierr.NotFound(w, "tenant not found")
			return
		}
		s.logger.ErrorContext(ctx, "could not load tenant", slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	var req startPairingRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		apierr.InvalidRequest(w, "the request body must be a JSON object")
		return
	}

	if _, err := s.db.Sessions().EnsureForTenant(ctx, tenantID); err != nil {
		s.logger.ErrorContext(ctx, "could not ensure session row", slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	result, err := s.sessions.StartPairing(ctx, tenantID, strings.TrimSpace(req.Phone))
	if err != nil {
		if writeWAError(w, err) {
			return
		}
		s.logger.ErrorContext(ctx, "could not start pairing",
			slog.String("tenant_id", tenantID), slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	s.logger.InfoContext(ctx, "pairing started",
		slog.String("tenant_id", tenantID), slog.String("mode", string(result.Mode)))

	writeJSON(w, http.StatusOK, result)
}

// resolveScopes falls back to the configured defaults when none were requested.
func (s *Server) resolveScopes(requested []string) []string {
	if len(requested) == 0 {
		return s.defaultScopes
	}
	return requested
}

// issueKey generates, stores and returns a key. The plaintext leaves this
// function once and is never logged or persisted.
func (s *Server) issueKey(r *http.Request, tenantID, name string, scopes []string) (issuedKey, error) {
	ctx := r.Context()

	generated, err := auth.GenerateKey(tenantID)
	if err != nil {
		s.logger.ErrorContext(ctx, "could not generate api key", slog.String("error", err.Error()))
		return issuedKey{}, err
	}

	record := store.APIKey{
		ID:        store.NewID(),
		TenantID:  tenantID,
		Name:      name,
		KeyHash:   generated.Hash,
		KeyPrefix: generated.Prefix,
		Scopes:    scopes,
		CreatedAt: time.Now().UTC(),
	}
	if err := s.db.APIKeys().Create(ctx, record); err != nil {
		s.logger.ErrorContext(ctx, "could not store api key", slog.String("error", err.Error()))
		return issuedKey{}, err
	}

	return issuedKey{
		ID:        record.ID,
		Name:      record.Name,
		KeyPrefix: record.KeyPrefix,
		Scopes:    record.Scopes,
		Key:       generated.Plaintext,
	}, nil
}
