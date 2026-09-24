package wa

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// TestBothPersistencePathsCaptureTheReference is the work-unit-1 guarantee.
//
// The live stream and history sync are separate mappings over different wire
// shapes, and before this they both recorded a media type and dropped the
// payload. A reference that only one of them captures means media that can be
// fetched from a conversation you received today and not from the same
// conversation after a re-pair.
func TestBothPersistencePathsCaptureTheReference(t *testing.T) {
	var (
		tenantID = "tenant-a"
		ownJID   = types.NewJID("573001111111", types.DefaultUserServer)
		dm       = types.NewJID("573001234567", types.DefaultUserServer)
		at       = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	)

	// One and the same voice note, wrapped as view-once, delivered both ways.
	payload := &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{
		Message: &waE2E.Message{AudioMessage: refAudio(true)},
	}}

	t.Run("the live path", func(t *testing.T) {
		messages := newFakeMessages()
		manager := &Manager{
			messages: messages,
			logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		}

		manager.persistMessage(context.Background(), tenantID, &events.Message{
			Info: types.MessageInfo{
				MessageSource: types.MessageSource{Chat: dm, Sender: dm},
				ID:            "LIVE-1",
				Timestamp:     at,
				MediaType:     "ptt",
			},
			Message: payload,
		})

		stored, err := messages.GetByID(context.Background(), tenantID, "LIVE-1")
		if err != nil {
			t.Fatalf("the live message was not stored: %v", err)
		}
		assertReferenceCaptured(t, stored, "ptt")
	})

	t.Run("the history path", func(t *testing.T) {
		records := historyRecords(tenantID, ownJID, dm, historyPersistAll,
			[]*waHistorySync.HistorySyncMsg{historyMsg("HIST-1", false, dm.String(), "", at, payload)})

		if len(records) != 1 {
			t.Fatalf("historyRecords() returned %d records, want 1", len(records))
		}
		assertReferenceCaptured(t, records[0], "ptt")
	})
}

// assertReferenceCaptured checks that a row carries everything a later download
// needs, and nothing it should not.
func assertReferenceCaptured(t *testing.T, got store.Message, wantMediaType string) {
	t.Helper()

	if got.MediaType == nil || *got.MediaType != wantMediaType {
		t.Errorf("media_type = %v, want %q", got.MediaType, wantMediaType)
	}
	if !got.HasMediaReference() {
		t.Fatal("the row carries no download reference")
	}
	if *got.DirectPath != refDirectPath {
		t.Errorf("direct_path = %q, want %q", *got.DirectPath, refDirectPath)
	}
	if !bytes.Equal(got.MediaKey, refMediaKey) {
		t.Errorf("media_key = %v, want %v", got.MediaKey, refMediaKey)
	}
	if !bytes.Equal(got.FileEncSHA256, refEncSHA) {
		t.Errorf("file_enc_sha256 = %v, want %v", got.FileEncSHA256, refEncSHA)
	}
	if !bytes.Equal(got.FileSHA256, refSHA) {
		t.Errorf("file_sha256 = %v, want %v", got.FileSHA256, refSHA)
	}
	if got.MMSType == nil || *got.MMSType != "audio" {
		t.Errorf("mms_type = %v, want audio", got.MMSType)
	}
	if got.MimeType == nil || *got.MimeType != "audio/ogg; codecs=opus" {
		t.Errorf("mime_type = %v, want the declared one", got.MimeType)
	}
	if got.FileLength == nil || *got.FileLength != 2048 {
		t.Errorf("file_length = %v, want 2048", got.FileLength)
	}

	// Nothing was downloaded: that is the whole point.
	if got.MediaPath != nil {
		t.Errorf("media_path = %q, want nil: media is fetched lazily", *got.MediaPath)
	}
	if got.MediaStatus != nil {
		t.Errorf("media_status = %q, want nil until somebody asks", *got.MediaStatus)
	}
}

// TestPersistedTextMessagesCarryNoMediaColumns keeps the reference columns from
// filling up with empty values for ordinary conversation.
func TestPersistedTextMessagesCarryNoMediaColumns(t *testing.T) {
	messages := newFakeMessages()
	manager := &Manager{
		messages: messages,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	dm := types.NewJID("573001234567", types.DefaultUserServer)

	manager.persistMessage(context.Background(), "tenant-a", &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: dm, Sender: dm},
			ID:            "LIVE-TEXT",
			Timestamp:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		},
		Message: textMsg("just talking"),
	})

	stored, err := messages.GetByID(context.Background(), "tenant-a", "LIVE-TEXT")
	if err != nil {
		t.Fatalf("the message was not stored: %v", err)
	}
	if stored.Body != "just talking" {
		t.Errorf("body = %q, want %q", stored.Body, "just talking")
	}
	if stored.MediaType != nil {
		t.Errorf("media_type = %q, want nil for a text message", *stored.MediaType)
	}
	if stored.HasMediaReference() {
		t.Error("a text message must carry no download reference")
	}
	if stored.MediaKey != nil {
		t.Error("a text message must carry no media key")
	}
}
