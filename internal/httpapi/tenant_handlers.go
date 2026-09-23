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
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// handleGetSession reports the status of the caller's own session. The tenant
// comes from the API key, so a caller can never ask about somebody else.
func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		apierr.Unauthorized(w, "api key required")
		return
	}

	status, err := s.sessions.Status(ctx, principal.TenantID)
	if err != nil {
		if writeWAError(w, err) {
			return
		}
		s.logger.ErrorContext(ctx, "could not read session status",
			slog.String("tenant_id", principal.TenantID), slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	writeJSON(w, http.StatusOK, status)
}

type sendMessageRequest struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

type sendMessageResponse struct {
	WAMessageID string    `json:"wa_message_id"`
	To          string    `json:"to"`
	SentAt      time.Time `json:"sent_at"`
}

// handleSendMessage sends a text message as the calling tenant and records it.
func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		apierr.Unauthorized(w, "api key required")
		return
	}

	var req sendMessageRequest
	if err := decodeJSON(w, r, &req); err != nil {
		apierr.InvalidRequest(w, "the request body must be a JSON object")
		return
	}

	to := strings.TrimSpace(req.To)
	body := strings.TrimSpace(req.Body)
	switch {
	case to == "":
		apierr.InvalidRequest(w, "to is required")
		return
	case body == "":
		apierr.InvalidRequest(w, "body is required")
		return
	}

	waMessageID, err := s.sessions.SendText(ctx, principal.TenantID, to, body)
	if err != nil {
		if writeWAError(w, err) {
			return
		}
		// The message body is never logged; only routing metadata is.
		s.logger.ErrorContext(ctx, "could not send message",
			slog.String("tenant_id", principal.TenantID),
			slog.String("to", to),
			slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	sentAt := time.Now().UTC()
	record := store.Message{
		ID:          store.NewID(),
		TenantID:    principal.TenantID,
		ChatJID:     to,
		SenderJID:   "",
		WAMessageID: waMessageID,
		Direction:   store.DirectionOut,
		Body:        body,
		Timestamp:   sentAt,
		CreatedAt:   sentAt,
	}
	if err := s.db.Messages().Append(ctx, record); err != nil {
		// The message did leave, so this is logged but not reported as a
		// failure to the caller.
		s.logger.ErrorContext(ctx, "could not persist outbound message",
			slog.String("tenant_id", principal.TenantID),
			slog.String("wa_message_id", waMessageID),
			slog.String("error", err.Error()))
	}

	s.logger.InfoContext(ctx, "message sent",
		slog.String("tenant_id", principal.TenantID),
		slog.String("to", to),
		slog.String("wa_message_id", waMessageID))

	writeJSON(w, http.StatusCreated, sendMessageResponse{
		WAMessageID: waMessageID,
		To:          to,
		SentAt:      sentAt,
	})
}

type listChatsResponse struct {
	Chats []store.Chat `json:"chats"`
}

// handleListChats returns the caller's own conversations.
func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		apierr.Unauthorized(w, "api key required")
		return
	}

	chats, err := s.db.Messages().ListChats(ctx, principal.TenantID, queryLimit(r))
	if err != nil {
		s.logger.ErrorContext(ctx, "could not list chats",
			slog.String("tenant_id", principal.TenantID), slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}
	if chats == nil {
		chats = []store.Chat{}
	}

	writeJSON(w, http.StatusOK, listChatsResponse{Chats: chats})
}

type listMessagesResponse struct {
	ChatJID  string          `json:"chat_jid"`
	Messages []store.Message `json:"messages"`
}

// handleListChatMessages returns a chat's history, newest first, always scoped
// to the tenant that owns the API key.
func (s *Server) handleListChatMessages(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		apierr.Unauthorized(w, "api key required")
		return
	}

	chatJID := strings.TrimSpace(r.PathValue("jid"))
	if chatJID == "" {
		apierr.InvalidRequest(w, "a chat jid is required")
		return
	}

	messages, err := s.db.Messages().ListByChat(ctx, principal.TenantID, chatJID, queryLimit(r))
	if err != nil {
		s.logger.ErrorContext(ctx, "could not list messages",
			slog.String("tenant_id", principal.TenantID), slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}
	if messages == nil {
		messages = []store.Message{}
	}

	writeJSON(w, http.StatusOK, listMessagesResponse{ChatJID: chatJID, Messages: messages})
}

type syncHistoryRequest struct {
	Count int `json:"count"`
}

// handleSyncChatHistory asks the tenant's phone for older messages of one chat.
//
// The call blocks until the phone answers or the configured timeout passes,
// because the messages are only useful once they are stored: returning early
// would hand the caller a promise it cannot check.
func (s *Server) handleSyncChatHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		apierr.Unauthorized(w, "api key required")
		return
	}

	chatJID := strings.TrimSpace(r.PathValue("jid"))
	if chatJID == "" {
		apierr.InvalidRequest(w, "a chat jid is required")
		return
	}

	// The body is optional: a bare POST means "the default number of messages".
	var req syncHistoryRequest
	if err := decodeJSON(w, r, &req); err != nil && !errors.Is(err, errEmptyBody) {
		apierr.InvalidRequest(w, "the request body must be a JSON object")
		return
	}

	result, err := s.sessions.SyncHistory(ctx, principal.TenantID, chatJID, wa.NormalizeSyncCount(req.Count))
	if err != nil {
		if writeWAError(w, err) {
			return
		}
		s.logger.ErrorContext(ctx, "could not sync chat history",
			slog.String("tenant_id", principal.TenantID),
			slog.String("chat_jid", chatJID),
			slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	s.logger.InfoContext(ctx, "chat history synced",
		slog.String("tenant_id", principal.TenantID),
		slog.String("chat_jid", chatJID),
		slog.Int("inserted", result.Inserted))

	writeJSON(w, http.StatusOK, result)
}

type findContactsResponse struct {
	Contacts []wa.Contact `json:"contacts"`
}

// handleFindContacts resolves a query against the caller's own address book.
func (s *Server) handleFindContacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		apierr.Unauthorized(w, "api key required")
		return
	}

	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		apierr.InvalidRequest(w, "q is required")
		return
	}

	contacts, err := s.sessions.FindContacts(ctx, principal.TenantID, query)
	if err != nil {
		if writeWAError(w, err) {
			return
		}
		s.logger.ErrorContext(ctx, "could not look up contacts",
			slog.String("tenant_id", principal.TenantID), slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}
	if contacts == nil {
		contacts = []wa.Contact{}
	}

	writeJSON(w, http.StatusOK, findContactsResponse{Contacts: contacts})
}
