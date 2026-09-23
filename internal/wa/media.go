package wa

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// Media failures the HTTP and MCP layers answer with distinct, honest replies.
var (
	// ErrUnknownMessage means no message of this tenant carries that id. A
	// message belonging to somebody else reports the same thing, because from
	// this tenant's side there is no difference.
	ErrUnknownMessage = errors.New("wa: unknown message")
	// ErrNoMedia means the message carries no attachment to fetch. A contact
	// card and a location name a media type and still land here: they have a
	// type, not bytes.
	ErrNoMedia = errors.New("wa: the message carries no downloadable media")
	// ErrMediaUnavailable means WhatsApp no longer serves the file.
	//
	// This is the cost of storing a reference instead of the bytes: media
	// expires server-side, so a message we still know about can point at
	// something that is gone. It is final — retrying can only fail again — and
	// it is reported as its own error rather than a generic failure so callers
	// stop asking.
	ErrMediaUnavailable = errors.New("wa: WhatsApp no longer has this media; it has expired")
	// ErrMediaTooLarge means the attachment is above MEDIA_MAX_BYTES. The
	// gateway shares a small server with other things; one video must not be
	// able to fill it.
	ErrMediaTooLarge = errors.New("wa: the media is larger than the configured limit")
	// ErrMediaTypeNotAllowed means the media type is outside MEDIA_FETCH_TYPES.
	// References are stored for every type because they are cheap; the
	// allowlist decides which ones may be turned into bytes on disk.
	ErrMediaTypeNotAllowed = errors.New("wa: this media type is not one the gateway downloads")
	// ErrUnsafeMediaPath means the identifiers could not produce a path inside
	// the tenant's own media directory.
	ErrUnsafeMediaPath = errors.New("wa: refusing to build a media path from these identifiers")
)

// MediaRef is what a completed fetch hands back.
//
// Path is a location on the gateway host. It is useful to an MCP client running
// beside the gateway and meaningless to an HTTP client, which is served the
// bytes instead and never the path.
type MediaRef struct {
	// MessageID is the gateway's own id for the message, whichever id was used
	// to ask.
	MessageID string `json:"message_id"`
	MediaType string `json:"media_type,omitempty"`
	MimeType  string `json:"mime_type,omitempty"`
	Status    string `json:"status"`
	Path      string `json:"path,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
}

// mediaDownloader is the one thing FetchMedia needs a live WhatsApp connection
// for. *whatsmeow.Client satisfies it.
//
// It exists so the fetch logic — the allowlist, the size guard, the path
// safety, the expiry handling and the bookkeeping — is testable without a
// socket, which is the rule the rest of this package already follows.
type mediaDownloader interface {
	DownloadMediaWithPath(
		ctx context.Context,
		directPath string,
		encFileHash, fileHash, mediaKey []byte,
		mediaType whatsmeow.MediaType,
		mmsType string,
		allowNoHash bool,
	) ([]byte, error)
}

// mediaFetcher turns a stored reference into bytes on disk, on demand.
type mediaFetcher struct {
	messages store.Messages
	// dir is the root of the media tree. Each tenant gets a subdirectory of it
	// so one tenant's media is never reachable under another's.
	dir      string
	maxBytes int64
	allowed  map[string]bool
	logger   *slog.Logger
	// downloaderFor resolves the tenant's live client.
	downloaderFor func(tenantID string) (mediaDownloader, error)
}

// allowedMediaTypes turns the configured list into a set.
func allowedMediaTypes(types []string) map[string]bool {
	allowed := make(map[string]bool, len(types))
	for _, t := range types {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" {
			allowed[t] = true
		}
	}
	return allowed
}

// FetchMedia downloads a message's attachment, once, on demand.
//
// Nothing downloads media before this is called: not the live event handler,
// not history sync, not a background job. A message row holds the reference
// whatsmeow needs and the bytes are fetched only when an MCP tool or an HTTP
// route asks for them.
//
// The call is idempotent. Media already on disk is returned without touching
// the network, and media WhatsApp has expired reports ErrMediaUnavailable
// without ever being retried.
func (m *Manager) FetchMedia(ctx context.Context, tenantID, messageID string) (MediaRef, error) {
	return m.media.fetch(ctx, tenantID, messageID)
}

// downloaderFor returns the tenant's connected client.
func (m *Manager) downloaderFor(tenantID string) (mediaDownloader, error) {
	m.mu.RLock()
	client := m.clients[tenantID]
	m.mu.RUnlock()

	if client == nil || !client.IsLoggedIn() {
		return nil, ErrNotPaired
	}
	return client, nil
}

// fetch runs the whole on-demand path. Its steps are ordered so that the cheap
// refusals happen before anything reaches the network.
func (f *mediaFetcher) fetch(ctx context.Context, tenantID, messageID string) (MediaRef, error) {
	message, err := f.messages.GetByID(ctx, tenantID, messageID)
	if errors.Is(err, store.ErrNotFound) {
		return MediaRef{}, ErrUnknownMessage
	}
	if err != nil {
		return MediaRef{}, err
	}
	if !message.HasMediaReference() {
		return MediaRef{}, ErrNoMedia
	}

	ref := MediaRef{
		MessageID: message.ID,
		MediaType: derefString(message.MediaType),
		MimeType:  derefString(message.MimeType),
	}

	// Expired is final. Asking WhatsApp again for a file it has already dropped
	// is a request that can only ever fail, so the stored verdict answers.
	if derefString(message.MediaStatus) == string(store.MediaUnavailable) {
		ref.Status = string(store.MediaUnavailable)
		return ref, ErrMediaUnavailable
	}

	// Already on disk: hand it back without downloading anything. A row that
	// claims to be available while the file is gone — a cleaned volume, a
	// restored backup — falls through and is fetched again.
	if path := derefString(message.MediaPath); path != "" {
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			ref.Status = string(store.MediaAvailable)
			ref.Path = path
			ref.SizeBytes = info.Size()
			return ref, nil
		}
	}

	if !f.allowed[strings.ToLower(ref.MediaType)] {
		return ref, ErrMediaTypeNotAllowed
	}
	// The declared length is checked first so an oversize file is refused
	// before a byte of it crosses the network.
	if message.FileLength != nil && *message.FileLength > f.maxBytes {
		return ref, ErrMediaTooLarge
	}

	path, err := mediaPathFor(f.dir, tenantID, message.ID, ref.MimeType)
	if err != nil {
		return ref, err
	}

	downloader, err := f.downloaderFor(tenantID)
	if err != nil {
		return ref, err
	}

	data, err := downloader.DownloadMediaWithPath(ctx,
		derefString(message.DirectPath),
		message.FileEncSHA256,
		message.FileSHA256,
		message.MediaKey,
		mediaTypeFor(derefString(message.MMSType)),
		derefString(message.MMSType),
		false,
	)
	if err != nil {
		return ref, f.recordFailure(ctx, tenantID, message.ID, ref, err)
	}

	// The declared length is what WhatsApp said, not a promise, so the real
	// size is checked too. Nothing is written when it is over the limit.
	if int64(len(data)) > f.maxBytes {
		return ref, ErrMediaTooLarge
	}

	if err := writeMediaFile(path, data); err != nil {
		return ref, f.recordFailure(ctx, tenantID, message.ID, ref, err)
	}

	fetchedAt := time.Now().UTC()
	if err := f.messages.UpdateMedia(ctx, tenantID, message.ID, store.MediaUpdate{
		Path:      &path,
		Status:    store.MediaAvailable,
		FetchedAt: fetchedAt,
	}); err != nil {
		return ref, err
	}

	// The media type and size are logged; the bytes and the keys never are.
	f.logger.InfoContext(ctx, "media fetched",
		slog.String("tenant_id", tenantID),
		slog.String("message_id", message.ID),
		slog.String("media_type", ref.MediaType),
		slog.Int("size_bytes", len(data)))

	ref.Status = string(store.MediaAvailable)
	ref.Path = path
	ref.SizeBytes = int64(len(data))
	return ref, nil
}

// recordFailure writes the outcome of a failed download and returns what the
// caller should report.
//
// The two classes are kept apart on purpose. 403, 404 and 410 mean WhatsApp
// itself no longer has the file: that is permanent, it is recorded as
// "unavailable" and it is never attempted again. Everything else — a refused
// media connection, a decryption mismatch, a full disk — is recorded as
// "failed" and may be retried. Only the media_* columns move either way.
func (f *mediaFetcher) recordFailure(ctx context.Context, tenantID, messageID string, ref MediaRef, cause error) error {
	status := store.MediaFailed
	outcome := fmt.Errorf("wa: download media: %w", cause)

	if isExpiredMedia(cause) {
		status = store.MediaUnavailable
		outcome = ErrMediaUnavailable
	}

	if err := f.messages.UpdateMedia(ctx, tenantID, messageID, store.MediaUpdate{
		Status:    status,
		Error:     cause.Error(),
		FetchedAt: time.Now().UTC(),
	}); err != nil {
		f.logger.ErrorContext(ctx, "could not record a media fetch failure",
			slog.String("tenant_id", tenantID),
			slog.String("message_id", messageID),
			slog.String("error", err.Error()))
	}

	f.logger.WarnContext(ctx, "media fetch failed",
		slog.String("tenant_id", tenantID),
		slog.String("message_id", messageID),
		slog.String("media_type", ref.MediaType),
		slog.String("media_status", string(status)),
		slog.String("error", cause.Error()))

	return outcome
}

// isExpiredMedia reports whether WhatsApp answered that the file is gone.
//
// whatsmeow returns these as typed DownloadHTTPError values whose Is compares
// the status code, so errors.Is is the correct test even after wrapping.
func isExpiredMedia(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

// writeMediaFile creates the tenant directory and writes the attachment.
//
// The directory keeps the 0o750 the gateway already creates MEDIA_DIR with, and
// the file is not world-readable: it is somebody's private conversation.
func writeMediaFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("wa: create media directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o640); err != nil {
		return fmt.Errorf("wa: write media file: %w", err)
	}
	return nil
}

// mediaPathFor builds the on-disk location of one message's attachment.
//
// The file name is the SHA-256 of the message id rather than the id itself.
// WhatsApp message ids arrive from the network and are not ours to trust: a
// name built from one verbatim could carry "../", an absolute path or a NUL and
// walk straight out of the media directory. Hashing removes the whole class of
// attack instead of trying to filter it, and the extension comes from a fixed
// table rather than from the message's own mime type.
//
// The tenant id is a path segment, so it is validated against a strict charset,
// and the finished path is checked to be inside the tenant's directory as a
// second line of defence.
func mediaPathFor(root, tenantID, messageID, mimeType string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", ErrUnsafeMediaPath
	}
	if !safeTenantSegment(tenantID) {
		return "", ErrUnsafeMediaPath
	}
	if strings.TrimSpace(messageID) == "" {
		return "", ErrUnsafeMediaPath
	}

	sum := sha256.Sum256([]byte(messageID))
	name := hex.EncodeToString(sum[:]) + mediaExtension(mimeType)

	dir := filepath.Join(root, tenantID)
	path := filepath.Join(dir, name)
	if !withinDir(dir, path) {
		return "", ErrUnsafeMediaPath
	}
	return path, nil
}

// maxTenantSegment bounds the directory name. Tenant ids are UUIDs the gateway
// generates itself, so anything long is a sign the caller is not what it claims.
const maxTenantSegment = 64

// safeTenantSegment accepts only what can be one harmless directory name.
func safeTenantSegment(tenantID string) bool {
	if tenantID == "" || len(tenantID) > maxTenantSegment {
		return false
	}
	// "." and ".." pass a bare charset check and are exactly the two names that
	// must not.
	if tenantID == "." || tenantID == ".." || strings.Contains(tenantID, "..") {
		return false
	}
	for _, r := range tenantID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// withinDir reports whether path sits inside dir.
func withinDir(dir, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(path))
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// mediaExtensions maps the mime types WhatsApp actually sends onto a file
// suffix. It is a fixed table on purpose: the mime type is attacker-influenced
// and must never reach a file name unfiltered.
var mediaExtensions = map[string]string{
	"image/jpeg":      ".jpg",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"image/gif":       ".gif",
	"audio/ogg":       ".ogg",
	"audio/opus":      ".opus",
	"audio/mpeg":      ".mp3",
	"audio/mp4":       ".m4a",
	"audio/aac":       ".aac",
	"audio/amr":       ".amr",
	"audio/wav":       ".wav",
	"video/mp4":       ".mp4",
	"video/3gpp":      ".3gp",
	"video/quicktime": ".mov",
	"video/webm":      ".webm",
	"application/pdf": ".pdf",
}

// mediaExtension picks a suffix for a mime type. An unknown one gets ".bin",
// which is honest and harmless.
func mediaExtension(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")
	if ext, ok := mediaExtensions[strings.ToLower(strings.TrimSpace(base))]; ok {
		return ext
	}
	return ".bin"
}

// mediaTypeFor maps the stored CDN type back onto whatsmeow's key constant.
//
// The constants are the key-derivation labels, so getting this wrong does not
// fail the request, it fails the decryption.
func mediaTypeFor(mmsType string) whatsmeow.MediaType {
	switch strings.ToLower(strings.TrimSpace(mmsType)) {
	case "image":
		return whatsmeow.MediaImage
	case "audio":
		return whatsmeow.MediaAudio
	case "video":
		return whatsmeow.MediaVideo
	case "document":
		return whatsmeow.MediaDocument
	default:
		return ""
	}
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
