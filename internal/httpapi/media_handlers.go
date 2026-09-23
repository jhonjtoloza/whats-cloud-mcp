package httpapi

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/apierr"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/auth"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// defaultMediaContentType is used when a message never declared a mime type.
const defaultMediaContentType = "application/octet-stream"

// handleGetMessageMedia downloads a message's attachment, if it is not already
// on disk, and streams it back.
//
// This is the only thing that ever fetches media. The database always knows a
// media message exists, because its reference was recorded on receipt; the
// bytes arrive when — and only when — somebody asks for them here.
func (s *Server) handleGetMessageMedia(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	principal, ok := auth.PrincipalFrom(ctx)
	if !ok {
		apierr.Unauthorized(w, "api key required")
		return
	}

	messageID := strings.TrimSpace(r.PathValue("id"))
	if messageID == "" {
		apierr.InvalidRequest(w, "a message id is required")
		return
	}

	ref, err := s.sessions.FetchMedia(ctx, principal.TenantID, messageID)
	if err != nil {
		if writeMediaError(w, err) {
			return
		}
		s.logger.ErrorContext(ctx, "could not fetch media",
			slog.String("tenant_id", principal.TenantID),
			slog.String("message_id", messageID),
			slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}

	file, err := os.Open(ref.Path)
	if err != nil {
		// The fetch reported success, so the file disappearing between the two
		// is a real server fault rather than something the caller did.
		s.logger.ErrorContext(ctx, "could not open fetched media",
			slog.String("tenant_id", principal.TenantID),
			slog.String("message_id", messageID),
			slog.String("error", err.Error()))
		apierr.Internal(w)
		return
	}
	defer func() { _ = file.Close() }()

	contentType := strings.TrimSpace(ref.MimeType)
	if contentType == "" {
		contentType = defaultMediaContentType
	}

	w.Header().Set("Content-Type", contentType)
	if ref.SizeBytes > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(ref.SizeBytes, 10))
	}
	// Somebody's private conversation: no shared cache may keep a copy.
	w.Header().Set("Cache-Control", "private, no-store")
	// The stored path is a host detail and never leaves the gateway, so the
	// download name is built from the message id and the media type instead.
	w.Header().Set("Content-Disposition", "inline")
	w.WriteHeader(http.StatusOK)

	if _, err := io.Copy(w, file); err != nil {
		// The status line is already out, so this can only be logged.
		s.logger.WarnContext(ctx, "media stream ended early",
			slog.String("tenant_id", principal.TenantID),
			slog.String("message_id", messageID),
			slog.String("error", err.Error()))
	}
}

// writeMediaError maps a fetch failure onto the status code that tells the
// truth about it.
//
// The distinction that matters is 410: WhatsApp expires media server-side, so a
// reference can outlive its bytes. That is not a server fault and not something
// retrying will fix, and answering 500 would invite a caller to keep asking for
// a file that no longer exists anywhere.
func writeMediaError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, wa.ErrMediaUnavailable):
		apierr.Write(w, http.StatusGone, apierr.CodeGone,
			"WhatsApp no longer has this media; it has expired and cannot be downloaded again")
		return true
	case errors.Is(err, wa.ErrMediaTooLarge):
		apierr.Write(w, http.StatusRequestEntityTooLarge, apierr.CodeTooLarge,
			"this attachment is larger than the gateway is configured to download")
		return true
	case errors.Is(err, wa.ErrMediaTypeNotAllowed):
		apierr.Write(w, http.StatusUnsupportedMediaType, apierr.CodeUnsupportedType,
			"the gateway does not download this media type")
		return true
	case errors.Is(err, wa.ErrNoMedia):
		apierr.NotFound(w, "this message carries no downloadable media")
		return true
	case errors.Is(err, wa.ErrUnknownMessage), errors.Is(err, store.ErrNotFound):
		apierr.NotFound(w, "no such message for this tenant")
		return true
	case errors.Is(err, wa.ErrUnsafeMediaPath):
		apierr.InvalidRequest(w, "the message id cannot address a stored file")
		return true
	default:
		// Everything the messaging routes already map — an unpaired tenant
		// above all — keeps the same meaning here.
		return writeWAError(w, err)
	}
}

// messageView is the wire shape of a stored message.
//
// It exists to add one derived field, media_available, on top of the stored row
// without teaching the store model about HTTP. The filesystem path and the
// decryption keys are excluded by store.Message itself: a client cannot use a
// path on the gateway host, and key material has no business on the wire.
type messageView struct {
	store.Message
	// MediaAvailable says whether the bytes are already on disk, which is what
	// tells a caller whether GET /v1/messages/{id}/media will be instant or
	// will trigger a download.
	MediaAvailable bool `json:"media_available"`
}

// messageViews converts stored rows for the wire.
func messageViews(messages []store.Message) []messageView {
	out := make([]messageView, 0, len(messages))
	for _, m := range messages {
		out = append(out, messageView{
			Message:        m,
			MediaAvailable: m.MediaPath != nil && derefStatus(m.MediaStatus) == string(store.MediaAvailable),
		})
	}
	return out
}

func derefStatus(status *string) string {
	if status == nil {
		return ""
	}
	return *status
}
