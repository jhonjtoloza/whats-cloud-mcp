package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// writeMediaFile drops a real file on disk for the fake to point at, so the
// handler is exercised streaming actual bytes.
func writeMediaFile(t *testing.T, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o640); err != nil {
		t.Fatalf("write media fixture: %v", err)
	}
	return path
}

// seedMediaMessage stores a message carrying a media reference, the way the
// live path records one.
func seedMediaMessage(t *testing.T, env *testEnv, tenantID, chatJID, waID, mediaType string, status *string) store.Message {
	t.Helper()

	mimeType := "audio/ogg"
	directPath := "/v/t62.7118-24/1_2_3_n.enc"
	mmsType := "audio"
	length := int64(2048)

	record := store.Message{
		ID:            store.NewID(),
		TenantID:      tenantID,
		ChatJID:       chatJID,
		SenderJID:     chatJID,
		WAMessageID:   waID,
		Direction:     store.DirectionIn,
		MediaType:     &mediaType,
		DirectPath:    &directPath,
		MediaKey:      []byte{0x01, 0x02},
		FileEncSHA256: []byte{0x03},
		FileSHA256:    []byte{0x04},
		FileLength:    &length,
		MimeType:      &mimeType,
		MMSType:       &mmsType,
		Timestamp:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		CreatedAt:     time.Now().UTC(),
	}
	if err := env.db.Messages().Append(context.Background(), record); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	if status != nil {
		path := "/data/media/" + tenantID + "/deadbeef.ogg"
		update := store.MediaUpdate{Status: store.MediaStatus(*status), FetchedAt: time.Now().UTC()}
		if *status == string(store.MediaAvailable) {
			update.Path = &path
		}
		if err := env.db.Messages().UpdateMedia(context.Background(), tenantID, record.ID, update); err != nil {
			t.Fatalf("UpdateMedia() error = %v", err)
		}
	}
	return record
}

// TestGetMessageMediaStreamsTheFile is the lazy fetch seen from outside: one
// request triggers the download and gets the bytes with their stored type.
func TestGetMessageMediaStreamsTheFile(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"media:read"})

	content := []byte("OggS fake voice note bytes")
	env.sessions.mediaRef = wa.MediaRef{
		MessageID: "msg-1",
		MediaType: "ptt",
		MimeType:  "audio/ogg; codecs=opus",
		Status:    "available",
		Path:      writeMediaFile(t, "voice.ogg", content),
		SizeBytes: int64(len(content)),
	}

	rec := env.do(t, http.MethodGet, "/v1/messages/msg-1/media", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "audio/ogg; codecs=opus" {
		t.Errorf("Content-Type = %q, want the stored mime type", got)
	}
	if rec.Body.String() != string(content) {
		t.Errorf("body = %q, want the media bytes", rec.Body.String())
	}

	calls := env.sessions.snapshotMediaCalls()
	if len(calls) != 1 {
		t.Fatalf("FetchMedia called %d times, want 1", len(calls))
	}
	if calls[0].TenantID != tenant.TenantID {
		t.Errorf("fetched as tenant %q, want the caller's own %q", calls[0].TenantID, tenant.TenantID)
	}
	if calls[0].MessageID != "msg-1" {
		t.Errorf("message id = %q, want msg-1", calls[0].MessageID)
	}
}

// TestGetMessageMediaNeverLeaksTheFilesystemPath keeps the host's layout off
// the wire. A client cannot use the path and an attacker should not learn it.
func TestGetMessageMediaNeverLeaksTheFilesystemPath(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"media:read"})

	path := writeMediaFile(t, "voice.ogg", []byte("bytes"))
	env.sessions.mediaRef = wa.MediaRef{
		MessageID: "msg-1", MediaType: "ptt", MimeType: "audio/ogg",
		Status: "available", Path: path, SizeBytes: 5,
	}

	rec := env.do(t, http.MethodGet, "/v1/messages/msg-1/media", tenant.APIKey.Key, nil)
	for name, values := range rec.Header() {
		for _, value := range values {
			if strings.Contains(value, path) {
				t.Errorf("header %s leaked the media path: %q", name, value)
			}
		}
	}
	if strings.Contains(rec.Body.String(), path) {
		t.Error("the response body leaked the media path")
	}
}

// TestGetMessageMediaRequiresMediaReadScope proves media is its own permission:
// reading the text of a conversation is not the same as pulling its files.
func TestGetMessageMediaRequiresMediaReadScope(t *testing.T) {
	tests := []struct {
		name       string
		scopes     []string
		wantStatus int
	}{
		{"a media:read key is let through", []string{"media:read"}, http.StatusOK},
		{"a messages:read key is not", []string{"messages:read"}, http.StatusForbidden},
		{"a messages:send key is not", []string{"messages:send"}, http.StatusForbidden},
		{"a media wildcard is enough", []string{"media:*"}, http.StatusOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", tc.scopes)
			env.sessions.mediaRef = wa.MediaRef{
				MessageID: "msg-1", MimeType: "audio/ogg", Status: "available",
				Path: writeMediaFile(t, "voice.ogg", []byte("bytes")),
			}

			rec := env.do(t, http.MethodGet, "/v1/messages/msg-1/media", tenant.APIKey.Key, nil)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

// TestGetMessageMediaAnswersEachFailureHonestly is the reason these errors are
// distinct types. Expired media is not a server fault and is never going to
// work; telling a caller "500" would invite a retry loop over a file that no
// longer exists.
func TestGetMessageMediaAnswersEachFailureHonestly(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
		wantReason string
	}{
		{
			name:       "expired media is gone, not broken",
			err:        wa.ErrMediaUnavailable,
			wantStatus: http.StatusGone,
			wantCode:   "gone",
			wantReason: "expired",
		},
		{
			name:       "oversize media says so",
			err:        wa.ErrMediaTooLarge,
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   "too_large",
		},
		{
			name:       "a type the gateway does not download says so",
			err:        wa.ErrMediaTypeNotAllowed,
			wantStatus: http.StatusUnsupportedMediaType,
			wantCode:   "unsupported_media_type",
		},
		{
			name:       "a message with no attachment is a 404",
			err:        wa.ErrNoMedia,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "an unknown message is a 404",
			err:        wa.ErrUnknownMessage,
			wantStatus: http.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name:       "an unpaired tenant cannot download anything",
			err:        wa.ErrNotPaired,
			wantStatus: http.StatusConflict,
			wantCode:   "conflict",
		},
		{
			name:       "anything else stays a server error",
			err:        errors.New("the disk is full"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal_error",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", []string{"media:read"})
			env.sessions.mediaErr = tc.err

			rec := env.do(t, http.MethodGet, "/v1/messages/msg-1/media", tenant.APIKey.Key, nil)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}

			got := decode[errorResponse](t, rec)
			if got.Error.Code != tc.wantCode {
				t.Errorf("error code = %q, want %q", got.Error.Code, tc.wantCode)
			}
			if got.Error.Message == "" {
				t.Error("every error response must carry a message")
			}
			if tc.wantReason != "" && !strings.Contains(strings.ToLower(got.Error.Message), tc.wantReason) {
				t.Errorf("message %q should explain %q", got.Error.Message, tc.wantReason)
			}
		})
	}
}

// TestGetMessageMediaRejectsAnEmptyID stops a bare path from reaching the
// engine at all.
func TestGetMessageMediaRejectsAnEmptyID(t *testing.T) {
	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"media:read"})

	rec := env.do(t, http.MethodGet, "/v1/messages/%20/media", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if calls := env.sessions.snapshotMediaCalls(); len(calls) != 0 {
		t.Errorf("a blank id reached the engine: %+v", calls)
	}
}

// TestGetMessageMediaIsTenantScoped restates the isolation guarantee on the
// media route: the tenant comes from the key, never from the path.
func TestGetMessageMediaIsTenantScoped(t *testing.T) {
	env := newTestEnv(t)
	tenantA := env.createTenant(t, "Acme", []string{"media:read"})
	tenantB := env.createTenant(t, "Globex", []string{"media:read"})

	env.sessions.mediaErr = wa.ErrUnknownMessage

	if rec := env.do(t, http.MethodGet, "/v1/messages/msg-of-b/media", tenantA.APIKey.Key, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}

	calls := env.sessions.snapshotMediaCalls()
	if len(calls) != 1 {
		t.Fatalf("FetchMedia called %d times, want 1", len(calls))
	}
	if calls[0].TenantID != tenantA.TenantID {
		t.Errorf("fetched as tenant %q, want %q", calls[0].TenantID, tenantA.TenantID)
	}
	if calls[0].TenantID == tenantB.TenantID {
		t.Error("the request was served as the other tenant")
	}
}

// TestListChatMessagesReportsMediaWithoutPaths is what makes the lazy model
// visible to a caller: the listing always knows a media message exists and what
// state its bytes are in, and never hands out a path.
func TestListChatMessagesReportsMediaWithoutPaths(t *testing.T) {
	const chatJID = "573001234567@s.whatsapp.net"
	available := string(store.MediaAvailable)
	unavailable := string(store.MediaUnavailable)

	tests := []struct {
		name          string
		status        *string
		wantStatus    string
		wantAvailable bool
	}{
		{name: "never requested", status: nil, wantStatus: "", wantAvailable: false},
		{name: "already fetched", status: &available, wantStatus: available, wantAvailable: true},
		{name: "expired", status: &unavailable, wantStatus: unavailable, wantAvailable: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			tenant := env.createTenant(t, "Acme", []string{"messages:read"})
			seedMediaMessage(t, env, tenant.TenantID, chatJID, "WA-1", "ptt", tc.status)

			rec := env.do(t, http.MethodGet,
				"/v1/chats/"+url.PathEscape(chatJID)+"/messages", tenant.APIKey.Key, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}

			got := decode[struct {
				Messages []struct {
					MediaType      string `json:"media_type"`
					MediaStatus    string `json:"media_status"`
					MediaAvailable bool   `json:"media_available"`
					MimeType       string `json:"mime_type"`
					MediaPath      string `json:"media_path"`
					DirectPath     string `json:"direct_path"`
					MediaKey       string `json:"media_key"`
				} `json:"messages"`
			}](t, rec)

			if len(got.Messages) != 1 {
				t.Fatalf("returned %d messages, want 1", len(got.Messages))
			}
			message := got.Messages[0]

			if message.MediaType != "ptt" {
				t.Errorf("media_type = %q, want ptt", message.MediaType)
			}
			if message.MediaStatus != tc.wantStatus {
				t.Errorf("media_status = %q, want %q", message.MediaStatus, tc.wantStatus)
			}
			if message.MediaAvailable != tc.wantAvailable {
				t.Errorf("media_available = %v, want %v", message.MediaAvailable, tc.wantAvailable)
			}
			if message.MimeType != "audio/ogg" {
				t.Errorf("mime_type = %q, want audio/ogg", message.MimeType)
			}

			// Neither a filesystem path nor any key material crosses the wire.
			if message.MediaPath != "" || message.DirectPath != "" || message.MediaKey != "" {
				t.Errorf("the listing leaked media internals: %+v", message)
			}
			for _, secret := range []string{"media_path", "direct_path", "media_key", "file_enc_sha256", "file_sha256", "/data/media"} {
				if strings.Contains(rec.Body.String(), secret) {
					t.Errorf("the listing body contains %q", secret)
				}
			}
		})
	}
}
