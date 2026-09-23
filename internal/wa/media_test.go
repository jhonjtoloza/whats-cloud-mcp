package wa

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// fakeMessages is an in-memory store.Messages. FetchMedia is the one part of
// the manager that can be tested without a socket, and this is what keeps it
// that way.
type fakeMessages struct {
	mu   sync.Mutex
	rows map[string]store.Message // keyed by tenant id + "/" + message id
}

func newFakeMessages(rows ...store.Message) *fakeMessages {
	f := &fakeMessages{rows: make(map[string]store.Message, len(rows))}
	for _, row := range rows {
		f.rows[row.TenantID+"/"+row.ID] = row
	}
	return f
}

func (f *fakeMessages) get(tenantID, id string) (store.Message, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[tenantID+"/"+id]
	return row, ok
}

func (f *fakeMessages) GetByID(_ context.Context, tenantID, id string) (store.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if row, ok := f.rows[tenantID+"/"+id]; ok {
		return row, nil
	}
	for _, row := range f.rows {
		if row.TenantID == tenantID && row.WAMessageID == id && id != "" {
			return row, nil
		}
	}
	return store.Message{}, store.ErrNotFound
}

func (f *fakeMessages) UpdateMedia(_ context.Context, tenantID, id string, upd store.MediaUpdate) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	key := tenantID + "/" + id
	row, ok := f.rows[key]
	if !ok {
		return store.ErrNotFound
	}
	if upd.Path != nil {
		path := *upd.Path
		row.MediaPath = &path
	}
	status := string(upd.Status)
	row.MediaStatus = &status
	row.MediaError = nil
	if upd.Error != "" {
		reason := upd.Error
		row.MediaError = &reason
	}
	at := upd.FetchedAt.UTC()
	row.MediaFetchedAt = &at
	f.rows[key] = row
	return nil
}

func (f *fakeMessages) Append(_ context.Context, m store.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[m.TenantID+"/"+m.ID] = m
	return nil
}

func (f *fakeMessages) AppendBatch(_ context.Context, messages []store.Message) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range messages {
		f.rows[m.TenantID+"/"+m.ID] = m
	}
	return len(messages), nil
}

func (f *fakeMessages) OldestByChat(context.Context, string, string) (store.Message, error) {
	return store.Message{}, store.ErrNotFound
}

func (f *fakeMessages) SearchChats(context.Context, string, string, int) ([]string, error) {
	return nil, nil
}

func (f *fakeMessages) ListByChat(context.Context, string, string, int) ([]store.Message, error) {
	return nil, nil
}

func (f *fakeMessages) ListChats(context.Context, string, int) ([]store.Chat, error) {
	return nil, nil
}

func (f *fakeMessages) Search(context.Context, string, string, int) ([]store.Message, error) {
	return nil, nil
}

// stubDownloader stands in for the whatsmeow client. No socket, no network.
type stubDownloader struct {
	mu    sync.Mutex
	calls int
	data  []byte
	err   error
}

func (s *stubDownloader) DownloadMediaWithPath(
	_ context.Context, _ string, _, _, _ []byte, _ whatsmeow.MediaType, _ string, _ bool,
) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.data, nil
}

func (s *stubDownloader) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// mediaRow builds a stored message carrying a complete download reference.
func mediaRow(tenantID, id, mediaType, mimeType string, length int64) store.Message {
	directPath := "/v/t62.7118-24/1_2_3_n.enc"
	mmsType := map[string]string{
		"image": "image", "sticker": "image", "ptt": "audio",
		"audio": "audio", "gif": "video", "video": "video", "document": "document",
	}[mediaType]

	return store.Message{
		ID:            id,
		TenantID:      tenantID,
		ChatJID:       "573001234567@s.whatsapp.net",
		SenderJID:     "573001234567@s.whatsapp.net",
		WAMessageID:   "WA-" + id,
		Direction:     store.DirectionIn,
		Body:          "the body must survive every fetch",
		MediaType:     &mediaType,
		DirectPath:    &directPath,
		MediaKey:      []byte{0x01, 0x02},
		FileEncSHA256: []byte{0x03},
		FileSHA256:    []byte{0x04},
		FileLength:    &length,
		MimeType:      &mimeType,
		MMSType:       &mmsType,
		Timestamp:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		CreatedAt:     time.Date(2026, 5, 1, 10, 0, 1, 0, time.UTC),
	}
}

// newTestFetcher wires a fetcher over a temporary media directory.
func newTestFetcher(t *testing.T, messages *fakeMessages, dl *stubDownloader) *mediaFetcher {
	t.Helper()
	return &mediaFetcher{
		messages: messages,
		dir:      t.TempDir(),
		maxBytes: 1024,
		allowed:  allowedMediaTypes([]string{"ptt", "audio", "image", "video", "document"}),
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		downloaderFor: func(string) (mediaDownloader, error) {
			return dl, nil
		},
	}
}

// hashedName is what a safe media file name looks like: the SHA-256 of the
// message id, never the id itself.
var hashedName = regexp.MustCompile(`^[0-9a-f]{64}(\.[a-z0-9]+)?$`)

// TestMediaPathNeverEscapesTheTenantDirectory is the load-bearing safety
// property. WhatsApp message ids arrive from the network, so a file name built
// from one verbatim would hand an attacker the filesystem.
func TestMediaPathNeverEscapesTheTenantDirectory(t *testing.T) {
	const root = "/srv/media"
	const tenantID = "tenant-a"
	tenantDir := filepath.Join(root, tenantID)

	tests := []struct {
		name      string
		messageID string
		wantErr   bool
	}{
		{name: "an ordinary whatsapp id", messageID: "3EB0C767D82B2C6E0F2A"},
		{name: "a relative traversal", messageID: "../../../etc/passwd"},
		{name: "a bare parent", messageID: ".."},
		{name: "a dot", messageID: "."},
		{name: "an absolute path", messageID: "/etc/shadow"},
		{name: "a windows-style traversal", messageID: `..\..\windows\system32`},
		{name: "a null byte", messageID: "abc\x00.png"},
		{name: "a newline", messageID: "abc\n../../x"},
		{name: "a separator in the middle", messageID: "abc/../../x"},
		{name: "an empty id", messageID: "", wantErr: true},
		{name: "a blank id", messageID: "   ", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := mediaPathFor(root, tenantID, tc.messageID, "image/jpeg")

			if tc.wantErr {
				if !errors.Is(err, ErrUnsafeMediaPath) {
					t.Fatalf("error = %v, want %v", err, ErrUnsafeMediaPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("mediaPathFor() error = %v", err)
			}
			if filepath.Dir(got) != tenantDir {
				t.Fatalf("path %q escaped the tenant directory %q", got, tenantDir)
			}
			if !hashedName.MatchString(filepath.Base(got)) {
				t.Errorf("file name %q is not a hash of the id", filepath.Base(got))
			}
			if strings.Contains(got, "..") {
				t.Errorf("path %q contains a traversal", got)
			}
		})
	}
}

// TestMediaPathRejectsAnUnsafeTenant keeps one tenant's directory from being
// spelled as another's, or as the root itself.
func TestMediaPathRejectsAnUnsafeTenant(t *testing.T) {
	tests := []string{
		"",
		"   ",
		"..",
		".",
		"../tenant-b",
		"/etc",
		"tenant/../../etc",
		"tenant\x00",
		strings.Repeat("a", 200),
	}

	for _, tenantID := range tests {
		t.Run("tenant "+tenantID, func(t *testing.T) {
			if _, err := mediaPathFor("/srv/media", tenantID, "msg-1", "image/jpeg"); !errors.Is(err, ErrUnsafeMediaPath) {
				t.Errorf("mediaPathFor(tenant %q) error = %v, want %v", tenantID, err, ErrUnsafeMediaPath)
			}
		})
	}
}

// TestMediaPathsOfDifferentTenantsNeverCollide is the isolation guarantee on
// disk: the same message id under two tenants is two files.
func TestMediaPathsOfDifferentTenantsNeverCollide(t *testing.T) {
	a, err := mediaPathFor("/srv/media", "tenant-a", "shared-id", "image/jpeg")
	if err != nil {
		t.Fatalf("mediaPathFor(tenant-a) error = %v", err)
	}
	b, err := mediaPathFor("/srv/media", "tenant-b", "shared-id", "image/jpeg")
	if err != nil {
		t.Fatalf("mediaPathFor(tenant-b) error = %v", err)
	}
	if a == b {
		t.Fatalf("both tenants share the media path %q", a)
	}
	if filepath.Dir(a) == filepath.Dir(b) {
		t.Errorf("both tenants share the media directory %q", filepath.Dir(a))
	}
}

// TestFetchMediaGatesOnTheTypeAllowlist proves references are stored for every
// media type but only the allowed ones are ever downloaded. Stickers and gifs
// are out by default: nobody asks an assistant to read a sticker.
func TestFetchMediaGatesOnTheTypeAllowlist(t *testing.T) {
	tests := []struct {
		name         string
		mediaType    string
		wantDownload bool
	}{
		{"a voice note is fetched", "ptt", true},
		{"audio is fetched", "audio", true},
		{"an image is fetched", "image", true},
		{"a video is fetched", "video", true},
		{"a document is fetched", "document", true},
		{"a sticker is not", "sticker", false},
		{"a gif is not", "gif", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := mediaRow("tenant-a", "msg-1", tc.mediaType, "application/octet-stream", 10)
			messages := newFakeMessages(row)
			dl := &stubDownloader{data: []byte("bytes")}
			fetcher := newTestFetcher(t, messages, dl)

			_, err := fetcher.fetch(context.Background(), "tenant-a", "msg-1")

			if tc.wantDownload {
				if err != nil {
					t.Fatalf("fetch() error = %v", err)
				}
				if dl.callCount() != 1 {
					t.Errorf("downloader called %d times, want 1", dl.callCount())
				}
				return
			}
			if !errors.Is(err, ErrMediaTypeNotAllowed) {
				t.Fatalf("error = %v, want %v", err, ErrMediaTypeNotAllowed)
			}
			if dl.callCount() != 0 {
				t.Errorf("a disallowed type was downloaded anyway (%d calls)", dl.callCount())
			}
		})
	}
}

// TestFetchMediaRefusesOversizeMedia is the shared-server guard: one video must
// not be able to fill the box.
func TestFetchMediaRefusesOversizeMedia(t *testing.T) {
	t.Run("the stored length is checked before downloading", func(t *testing.T) {
		row := mediaRow("tenant-a", "msg-1", "video", "video/mp4", 5000)
		messages := newFakeMessages(row)
		dl := &stubDownloader{data: []byte("never reached")}
		fetcher := newTestFetcher(t, messages, dl) // maxBytes = 1024

		_, err := fetcher.fetch(context.Background(), "tenant-a", "msg-1")
		if !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("error = %v, want %v", err, ErrMediaTooLarge)
		}
		if dl.callCount() != 0 {
			t.Errorf("oversize media was downloaded before the guard ran (%d calls)", dl.callCount())
		}
	})

	t.Run("a lying length is caught after downloading", func(t *testing.T) {
		// The declared length is what WhatsApp said, not a promise.
		row := mediaRow("tenant-a", "msg-1", "video", "video/mp4", 10)
		messages := newFakeMessages(row)
		dl := &stubDownloader{data: make([]byte, 4096)}
		fetcher := newTestFetcher(t, messages, dl)

		_, err := fetcher.fetch(context.Background(), "tenant-a", "msg-1")
		if !errors.Is(err, ErrMediaTooLarge) {
			t.Fatalf("error = %v, want %v", err, ErrMediaTooLarge)
		}
		stored, _ := messages.get("tenant-a", "msg-1")
		if stored.MediaPath != nil {
			t.Errorf("oversize media was written to %q", *stored.MediaPath)
		}
	})
}

// TestFetchMediaIsIdempotent is what makes the lazy model usable: asking twice
// costs one download.
func TestFetchMediaIsIdempotent(t *testing.T) {
	row := mediaRow("tenant-a", "msg-1", "ptt", "audio/ogg", 12)
	messages := newFakeMessages(row)
	dl := &stubDownloader{data: []byte("voice note")}
	fetcher := newTestFetcher(t, messages, dl)
	ctx := context.Background()

	first, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
	if err != nil {
		t.Fatalf("first fetch() error = %v", err)
	}
	if first.Status != string(store.MediaAvailable) {
		t.Errorf("status = %q, want %q", first.Status, store.MediaAvailable)
	}
	if first.MimeType != "audio/ogg" {
		t.Errorf("mime_type = %q, want audio/ogg", first.MimeType)
	}

	second, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
	if err != nil {
		t.Fatalf("second fetch() error = %v", err)
	}
	if second.Path != first.Path {
		t.Errorf("path = %q, want the first one %q", second.Path, first.Path)
	}
	if dl.callCount() != 1 {
		t.Errorf("downloader called %d times, want 1: the second fetch must be served from disk", dl.callCount())
	}

	written, err := os.ReadFile(first.Path)
	if err != nil {
		t.Fatalf("read the stored media: %v", err)
	}
	if string(written) != "voice note" {
		t.Errorf("stored bytes = %q, want the downloaded ones", written)
	}
}

// TestFetchMediaRedownloadsWhenTheFileIsGone covers the drift the database
// cannot see: the row says available, but somebody cleaned /data.
func TestFetchMediaRedownloadsWhenTheFileIsGone(t *testing.T) {
	row := mediaRow("tenant-a", "msg-1", "image", "image/jpeg", 12)
	messages := newFakeMessages(row)
	dl := &stubDownloader{data: []byte("the photo")}
	fetcher := newTestFetcher(t, messages, dl)
	ctx := context.Background()

	first, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
	if err != nil {
		t.Fatalf("first fetch() error = %v", err)
	}
	if err := os.Remove(first.Path); err != nil {
		t.Fatalf("remove the stored media: %v", err)
	}

	stored, _ := messages.get("tenant-a", "msg-1")
	if stored.MediaStatus == nil || *stored.MediaStatus != string(store.MediaAvailable) {
		t.Fatalf("precondition: media_status = %v, want available", stored.MediaStatus)
	}

	second, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
	if err != nil {
		t.Fatalf("second fetch() error = %v", err)
	}
	if dl.callCount() != 2 {
		t.Errorf("downloader called %d times, want 2: a missing file must be re-downloaded", dl.callCount())
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Errorf("the media was not written back: %v", err)
	}
}

// TestFetchMediaMarksExpiredMediaUnavailable is the honest half of the lazy
// model: WhatsApp drops media server-side, so a reference can outlive its bytes.
// Those three statuses are final and must never be retried.
func TestFetchMediaMarksExpiredMediaUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"403 forbidden", whatsmeow.ErrMediaDownloadFailedWith403},
		{"404 not found", whatsmeow.ErrMediaDownloadFailedWith404},
		{"410 gone", whatsmeow.ErrMediaDownloadFailedWith410},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row := mediaRow("tenant-a", "msg-1", "image", "image/jpeg", 12)
			messages := newFakeMessages(row)
			dl := &stubDownloader{err: tc.err}
			fetcher := newTestFetcher(t, messages, dl)
			ctx := context.Background()

			_, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
			if !errors.Is(err, ErrMediaUnavailable) {
				t.Fatalf("error = %v, want %v", err, ErrMediaUnavailable)
			}

			stored, _ := messages.get("tenant-a", "msg-1")
			if stored.MediaStatus == nil || *stored.MediaStatus != string(store.MediaUnavailable) {
				t.Errorf("media_status = %v, want %q", stored.MediaStatus, store.MediaUnavailable)
			}
			if stored.MediaError == nil || *stored.MediaError == "" {
				t.Error("media_error must say why the media is gone")
			}
			if stored.MediaFetchedAt == nil {
				t.Error("media_fetched_at must record the attempt")
			}

			// And the retry that must never reach the network.
			if _, err := fetcher.fetch(ctx, "tenant-a", "msg-1"); !errors.Is(err, ErrMediaUnavailable) {
				t.Fatalf("second error = %v, want %v", err, ErrMediaUnavailable)
			}
			if dl.callCount() != 1 {
				t.Errorf("downloader called %d times, want 1: expired media must never be retried", dl.callCount())
			}
		})
	}
}

// TestFetchMediaRecordsARetryableFailure is the other error class: something
// broke, the row is untouched apart from the bookkeeping, and asking again is
// allowed.
func TestFetchMediaRecordsARetryableFailure(t *testing.T) {
	row := mediaRow("tenant-a", "msg-1", "image", "image/jpeg", 12)
	messages := newFakeMessages(row)
	dl := &stubDownloader{err: errors.New("failed to refresh media connections")}
	fetcher := newTestFetcher(t, messages, dl)
	ctx := context.Background()

	_, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
	if err == nil {
		t.Fatal("fetch() should report the failure")
	}
	if errors.Is(err, ErrMediaUnavailable) {
		t.Fatal("a transport failure must not be reported as expired media")
	}

	stored, ok := messages.get("tenant-a", "msg-1")
	if !ok {
		t.Fatal("the row was lost by a failed fetch")
	}
	if stored.MediaStatus == nil || *stored.MediaStatus != string(store.MediaFailed) {
		t.Errorf("media_status = %v, want %q", stored.MediaStatus, store.MediaFailed)
	}

	// The row itself is intact: a failed download costs the attempt and nothing
	// else.
	if stored.Body != row.Body {
		t.Errorf("body = %q, want %q", stored.Body, row.Body)
	}
	if stored.DirectPath == nil || *stored.DirectPath != *row.DirectPath {
		t.Error("the download reference was lost by a failed fetch")
	}
	if !stored.Timestamp.Equal(row.Timestamp) {
		t.Errorf("timestamp = %v, want %v", stored.Timestamp, row.Timestamp)
	}
	if stored.MediaPath != nil {
		t.Errorf("media_path = %q, want nil after a failure", *stored.MediaPath)
	}

	// Retryable means retryable.
	dl.mu.Lock()
	dl.err = nil
	dl.data = []byte("it worked this time")
	dl.mu.Unlock()

	ref, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
	if err != nil {
		t.Fatalf("retry error = %v, want the fetch to succeed", err)
	}
	if ref.Status != string(store.MediaAvailable) {
		t.Errorf("status = %q, want %q", ref.Status, store.MediaAvailable)
	}
	if dl.callCount() != 2 {
		t.Errorf("downloader called %d times, want 2", dl.callCount())
	}
}

// TestFetchMediaIsTenantScoped is the multi-tenant guarantee at the fetch
// boundary: knowing another tenant's message id buys nothing.
func TestFetchMediaIsTenantScoped(t *testing.T) {
	theirs := mediaRow("tenant-b", "msg-b", "image", "image/jpeg", 12)
	messages := newFakeMessages(theirs)
	dl := &stubDownloader{data: []byte("tenant b's photo")}
	fetcher := newTestFetcher(t, messages, dl)
	ctx := context.Background()

	tests := []struct {
		name string
		id   string
	}{
		{"by the gateway id", "msg-b"},
		{"by the whatsapp id", theirs.WAMessageID},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fetcher.fetch(ctx, "tenant-a", tc.id)
			if !errors.Is(err, ErrUnknownMessage) {
				t.Fatalf("error = %v, want %v", err, ErrUnknownMessage)
			}
		})
	}
	if dl.callCount() != 0 {
		t.Errorf("tenant A triggered %d downloads of tenant B's media", dl.callCount())
	}
}

// TestFetchMediaWithoutAReference covers the rows that name a media type but
// carry no bytes at all, and the plain text ones.
func TestFetchMediaWithoutAReference(t *testing.T) {
	vcard := "vcard"
	tests := []struct {
		name string
		row  store.Message
	}{
		{
			name: "a plain text message",
			row: store.Message{
				ID: "msg-text", TenantID: "tenant-a", ChatJID: "c@s.whatsapp.net",
				WAMessageID: "WA-text", Direction: store.DirectionIn, Body: "hola",
			},
		},
		{
			name: "a contact card names a type but has no bytes",
			row: store.Message{
				ID: "msg-vcard", TenantID: "tenant-a", ChatJID: "c@s.whatsapp.net",
				WAMessageID: "WA-vcard", Direction: store.DirectionIn, MediaType: &vcard,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			messages := newFakeMessages(tc.row)
			dl := &stubDownloader{}
			fetcher := newTestFetcher(t, messages, dl)

			if _, err := fetcher.fetch(context.Background(), "tenant-a", tc.row.ID); !errors.Is(err, ErrNoMedia) {
				t.Fatalf("error = %v, want %v", err, ErrNoMedia)
			}
			if dl.callCount() != 0 {
				t.Errorf("a message with no attachment triggered %d downloads", dl.callCount())
			}
		})
	}
}

// TestFetchMediaUnknownMessage keeps a bad id from looking like a server fault.
func TestFetchMediaUnknownMessage(t *testing.T) {
	messages := newFakeMessages()
	fetcher := newTestFetcher(t, messages, &stubDownloader{})

	for _, id := range []string{"nothing-like-this", ""} {
		if _, err := fetcher.fetch(context.Background(), "tenant-a", id); !errors.Is(err, ErrUnknownMessage) {
			t.Errorf("fetch(%q) error = %v, want %v", id, err, ErrUnknownMessage)
		}
	}
}

// TestFetchMediaWritesInsideThePerTenantDirectory checks the layout on disk
// rather than in theory: tenant A's media must not be reachable as tenant B's.
func TestFetchMediaWritesInsideThePerTenantDirectory(t *testing.T) {
	rowA := mediaRow("tenant-a", "msg-1", "image", "image/jpeg", 12)
	rowB := mediaRow("tenant-b", "msg-1", "image", "image/jpeg", 12)
	messages := newFakeMessages(rowA, rowB)
	dl := &stubDownloader{data: []byte("photo")}
	fetcher := newTestFetcher(t, messages, dl)
	ctx := context.Background()

	refA, err := fetcher.fetch(ctx, "tenant-a", "msg-1")
	if err != nil {
		t.Fatalf("fetch(tenant-a) error = %v", err)
	}
	refB, err := fetcher.fetch(ctx, "tenant-b", "msg-1")
	if err != nil {
		t.Fatalf("fetch(tenant-b) error = %v", err)
	}

	if refA.Path == refB.Path {
		t.Fatalf("both tenants share the media file %q", refA.Path)
	}
	if filepath.Dir(refA.Path) != filepath.Join(fetcher.dir, "tenant-a") {
		t.Errorf("tenant A media landed in %q", filepath.Dir(refA.Path))
	}
	if filepath.Dir(refB.Path) != filepath.Join(fetcher.dir, "tenant-b") {
		t.Errorf("tenant B media landed in %q", filepath.Dir(refB.Path))
	}

	info, err := os.Stat(filepath.Dir(refA.Path))
	if err != nil {
		t.Fatalf("stat the tenant directory: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o750 {
		t.Errorf("tenant directory mode = %o, want 750", perm)
	}
}
