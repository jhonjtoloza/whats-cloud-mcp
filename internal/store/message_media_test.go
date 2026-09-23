package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// mediaMessage builds a message carrying the full download reference, the way
// both the live path and the history mapper record one.
func mediaMessage(tenantID, waID string) store.Message {
	mediaType := "image"
	mimeType := "image/jpeg"
	mmsType := "image"
	directPath := "/v/t62.7118-24/12345_678_n.enc"
	length := int64(4096)

	return store.Message{
		ID:            store.NewID(),
		TenantID:      tenantID,
		ChatJID:       "573001234567@s.whatsapp.net",
		SenderJID:     "573001234567@s.whatsapp.net",
		WAMessageID:   waID,
		Direction:     store.DirectionIn,
		Body:          "look at this",
		MediaType:     &mediaType,
		DirectPath:    &directPath,
		MediaKey:      []byte{0x01, 0x02, 0x03},
		FileEncSHA256: []byte{0x04, 0x05},
		FileSHA256:    []byte{0x06, 0x07},
		FileLength:    &length,
		MimeType:      &mimeType,
		MMSType:       &mmsType,
		Timestamp:     time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		CreatedAt:     time.Date(2026, 5, 1, 12, 0, 1, 0, time.UTC),
	}
}

// TestAppendStoresTheDownloadReference is the whole point of work unit 1: the
// row has to carry everything DownloadMediaWithPath needs, because the bytes
// themselves are never fetched on receipt.
func TestAppendStoresTheDownloadReference(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	want := mediaMessage("tenant-a", "WA-MEDIA-1")
	if err := db.Messages().Append(ctx, want); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	got, err := db.Messages().GetByID(ctx, "tenant-a", want.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}

	if got.DirectPath == nil || *got.DirectPath != *want.DirectPath {
		t.Errorf("direct_path = %v, want %q", got.DirectPath, *want.DirectPath)
	}
	if !bytes.Equal(got.MediaKey, want.MediaKey) {
		t.Errorf("media_key = %v, want %v", got.MediaKey, want.MediaKey)
	}
	if !bytes.Equal(got.FileEncSHA256, want.FileEncSHA256) {
		t.Errorf("file_enc_sha256 = %v, want %v", got.FileEncSHA256, want.FileEncSHA256)
	}
	if !bytes.Equal(got.FileSHA256, want.FileSHA256) {
		t.Errorf("file_sha256 = %v, want %v", got.FileSHA256, want.FileSHA256)
	}
	if got.FileLength == nil || *got.FileLength != *want.FileLength {
		t.Errorf("file_length = %v, want %d", got.FileLength, *want.FileLength)
	}
	if got.MimeType == nil || *got.MimeType != *want.MimeType {
		t.Errorf("mime_type = %v, want %q", got.MimeType, *want.MimeType)
	}
	if got.MMSType == nil || *got.MMSType != *want.MMSType {
		t.Errorf("mms_type = %v, want %q", got.MMSType, *want.MMSType)
	}
	// Never requested yet: a fresh reference has no media status at all.
	if got.MediaStatus != nil {
		t.Errorf("media_status = %v, want nil for a reference nobody has asked for", *got.MediaStatus)
	}
	if got.MediaPath != nil {
		t.Errorf("media_path = %v, want nil: media is never downloaded eagerly", *got.MediaPath)
	}
}

// TestGetByIDAcceptsEitherIdentifier keeps the tool surface usable: a caller
// holding the gateway id and a caller holding the WhatsApp id both reach the
// same row, and neither reaches another tenant's.
func TestGetByIDAcceptsEitherIdentifier(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	ctx := context.Background()

	mine := mediaMessage("tenant-a", "WA-MEDIA-1")
	if err := db.Messages().Append(ctx, mine); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	theirs := mediaMessage("tenant-b", "WA-MEDIA-2")
	if err := db.Messages().Append(ctx, theirs); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	tests := []struct {
		name     string
		tenantID string
		id       string
		wantErr  error
		wantRow  string
	}{
		{"the gateway id", "tenant-a", mine.ID, nil, mine.ID},
		{"the whatsapp id", "tenant-a", mine.WAMessageID, nil, mine.ID},
		{"another tenant's gateway id", "tenant-a", theirs.ID, store.ErrNotFound, ""},
		{"another tenant's whatsapp id", "tenant-a", theirs.WAMessageID, store.ErrNotFound, ""},
		{"an unknown id", "tenant-a", "nothing-like-this", store.ErrNotFound, ""},
		{"an empty id", "tenant-a", "", store.ErrNotFound, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.Messages().GetByID(ctx, tc.tenantID, tc.id)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetByID() error = %v", err)
			}
			if got.ID != tc.wantRow {
				t.Errorf("id = %q, want %q", got.ID, tc.wantRow)
			}
		})
	}
}

// TestUpdateMediaTouchesOnlyTheMediaColumns is the "a failed fetch must never
// lose the row" guarantee, written as a schema-level constraint: whatever the
// download did, the message itself is untouched.
func TestUpdateMediaTouchesOnlyTheMediaColumns(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	original := mediaMessage("tenant-a", "WA-MEDIA-1")
	if err := db.Messages().Append(ctx, original); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	fetchedAt := time.Date(2026, 5, 2, 8, 30, 0, 0, time.UTC)
	path := "/data/media/tenant-a/abc.jpg"
	err := db.Messages().UpdateMedia(ctx, "tenant-a", original.ID, store.MediaUpdate{
		Path:      &path,
		Status:    store.MediaAvailable,
		FetchedAt: fetchedAt,
	})
	if err != nil {
		t.Fatalf("UpdateMedia() error = %v", err)
	}

	got, err := db.Messages().GetByID(ctx, "tenant-a", original.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}

	if got.MediaPath == nil || *got.MediaPath != path {
		t.Errorf("media_path = %v, want %q", got.MediaPath, path)
	}
	if got.MediaStatus == nil || *got.MediaStatus != string(store.MediaAvailable) {
		t.Errorf("media_status = %v, want %q", got.MediaStatus, store.MediaAvailable)
	}
	if got.MediaFetchedAt == nil || !got.MediaFetchedAt.Equal(fetchedAt) {
		t.Errorf("media_fetched_at = %v, want %v", got.MediaFetchedAt, fetchedAt)
	}
	if got.MediaError != nil {
		t.Errorf("media_error = %q, want nil after a successful fetch", *got.MediaError)
	}

	// The message itself is untouched.
	if got.Body != original.Body {
		t.Errorf("body = %q, want %q", got.Body, original.Body)
	}
	if !got.Timestamp.Equal(original.Timestamp) {
		t.Errorf("timestamp = %v, want %v", got.Timestamp, original.Timestamp)
	}
	if got.Direction != original.Direction {
		t.Errorf("direction = %q, want %q", got.Direction, original.Direction)
	}
	if got.DirectPath == nil || *got.DirectPath != *original.DirectPath {
		t.Errorf("direct_path = %v, want it preserved", got.DirectPath)
	}
	if !bytes.Equal(got.MediaKey, original.MediaKey) {
		t.Error("media_key was lost by a media update")
	}
}

// TestUpdateMediaRecordsAFailure covers the other half: a failure writes the
// status and the reason, and clears no reference.
func TestUpdateMediaRecordsAFailure(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	original := mediaMessage("tenant-a", "WA-MEDIA-1")
	if err := db.Messages().Append(ctx, original); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	err := db.Messages().UpdateMedia(ctx, "tenant-a", original.ID, store.MediaUpdate{
		Status:    store.MediaUnavailable,
		Error:     "download failed with status code 410",
		FetchedAt: time.Date(2026, 5, 2, 8, 30, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("UpdateMedia() error = %v", err)
	}

	got, err := db.Messages().GetByID(ctx, "tenant-a", original.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.MediaStatus == nil || *got.MediaStatus != string(store.MediaUnavailable) {
		t.Errorf("media_status = %v, want %q", got.MediaStatus, store.MediaUnavailable)
	}
	if got.MediaError == nil || *got.MediaError == "" {
		t.Error("media_error must record why the fetch failed")
	}
	if got.MediaPath != nil {
		t.Errorf("media_path = %q, want nil after a failed fetch", *got.MediaPath)
	}
	if got.Body != original.Body {
		t.Errorf("body = %q, want the row intact", got.Body)
	}
}

// TestUpdateMediaIsTenantScoped stops one tenant from writing over another
// tenant's row even when it somehow learns the id.
func TestUpdateMediaIsTenantScoped(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	mustTenant(t, db, "tenant-b", "Globex")
	ctx := context.Background()

	theirs := mediaMessage("tenant-b", "WA-MEDIA-2")
	if err := db.Messages().Append(ctx, theirs); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	err := db.Messages().UpdateMedia(ctx, "tenant-a", theirs.ID, store.MediaUpdate{
		Status:    store.MediaFailed,
		FetchedAt: time.Now().UTC(),
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("error = %v, want %v", err, store.ErrNotFound)
	}

	got, err := db.Messages().GetByID(ctx, "tenant-b", theirs.ID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if got.MediaStatus != nil {
		t.Errorf("media_status = %q, want nil: another tenant wrote to this row", *got.MediaStatus)
	}
}

// TestListByChatCarriesTheMediaReference keeps the listing honest: it reports
// what is known about the attachment without ever fetching it.
func TestListByChatCarriesTheMediaReference(t *testing.T) {
	db := newTestDB(t)
	mustTenant(t, db, "tenant-a", "Acme")
	ctx := context.Background()

	original := mediaMessage("tenant-a", "WA-MEDIA-1")
	if err := db.Messages().Append(ctx, original); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	listed, err := db.Messages().ListByChat(ctx, "tenant-a", original.ChatJID, 10)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d messages, want 1", len(listed))
	}
	if listed[0].MediaType == nil || *listed[0].MediaType != "image" {
		t.Errorf("media_type = %v, want image", listed[0].MediaType)
	}
	if listed[0].MediaStatus != nil {
		t.Errorf("media_status = %q, want nil until somebody asks for the bytes", *listed[0].MediaStatus)
	}
	if listed[0].MimeType == nil || *listed[0].MimeType != "image/jpeg" {
		t.Errorf("mime_type = %v, want image/jpeg", listed[0].MimeType)
	}
}
