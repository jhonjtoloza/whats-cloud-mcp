package wa

import (
	"bytes"
	"testing"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// The reference fixtures. Real values are opaque blobs; these only have to be
// distinguishable from each other.
var (
	refDirectPath = "/v/t62.7118-24/30000000_111_222_n.enc?ccb=11-4"
	refMediaKey   = []byte{0x10, 0x11, 0x12}
	refEncSHA     = []byte{0x20, 0x21}
	refSHA        = []byte{0x30, 0x31}
)

func refImage() *waE2E.ImageMessage {
	return &waE2E.ImageMessage{
		Mimetype:      proto.String("image/jpeg"),
		DirectPath:    proto.String(refDirectPath),
		MediaKey:      refMediaKey,
		FileEncSHA256: refEncSHA,
		FileSHA256:    refSHA,
		FileLength:    proto.Uint64(123456),
	}
}

func refAudio(ptt bool) *waE2E.AudioMessage {
	return &waE2E.AudioMessage{
		Mimetype:      proto.String("audio/ogg; codecs=opus"),
		PTT:           proto.Bool(ptt),
		DirectPath:    proto.String(refDirectPath),
		MediaKey:      refMediaKey,
		FileEncSHA256: refEncSHA,
		FileSHA256:    refSHA,
		FileLength:    proto.Uint64(2048),
	}
}

func refVideo(gif bool) *waE2E.VideoMessage {
	return &waE2E.VideoMessage{
		Mimetype:      proto.String("video/mp4"),
		GifPlayback:   proto.Bool(gif),
		DirectPath:    proto.String(refDirectPath),
		MediaKey:      refMediaKey,
		FileEncSHA256: refEncSHA,
		FileSHA256:    refSHA,
		FileLength:    proto.Uint64(9000),
	}
}

func refDocument() *waE2E.DocumentMessage {
	return &waE2E.DocumentMessage{
		Mimetype:      proto.String("application/pdf"),
		DirectPath:    proto.String(refDirectPath),
		MediaKey:      refMediaKey,
		FileEncSHA256: refEncSHA,
		FileSHA256:    refSHA,
		FileLength:    proto.Uint64(70000),
	}
}

func refSticker() *waE2E.StickerMessage {
	return &waE2E.StickerMessage{
		Mimetype:      proto.String("image/webp"),
		DirectPath:    proto.String(refDirectPath),
		MediaKey:      refMediaKey,
		FileEncSHA256: refEncSHA,
		FileSHA256:    refSHA,
		FileLength:    proto.Uint64(512),
	}
}

// TestMediaReference is the shared unwrapper both persistence paths depend on.
//
// whatsmeow's own getDownloadableMessage is unexported, so this mirrors its
// behaviour: it must see through every wrapper historyMediaType already handles,
// or a view-once voice note would be stored as a message with no attachment at
// all.
func TestMediaReference(t *testing.T) {
	tests := []struct {
		name           string
		msg            *waE2E.Message
		wantMediaType  string
		wantMimeType   string
		wantMMSType    string
		wantLength     int64
		wantDownloadab bool
	}{
		{name: "a nil message carries nothing", msg: nil},
		{name: "a text message carries nothing", msg: &waE2E.Message{Conversation: proto.String("hola")}},
		{
			name:           "an image",
			msg:            &waE2E.Message{ImageMessage: refImage()},
			wantMediaType:  "image",
			wantMimeType:   "image/jpeg",
			wantMMSType:    "image",
			wantLength:     123456,
			wantDownloadab: true,
		},
		{
			name:           "a voice note is ptt but downloads as audio",
			msg:            &waE2E.Message{AudioMessage: refAudio(true)},
			wantMediaType:  "ptt",
			wantMimeType:   "audio/ogg; codecs=opus",
			wantMMSType:    "audio",
			wantLength:     2048,
			wantDownloadab: true,
		},
		{
			name:           "ordinary audio",
			msg:            &waE2E.Message{AudioMessage: refAudio(false)},
			wantMediaType:  "audio",
			wantMimeType:   "audio/ogg; codecs=opus",
			wantMMSType:    "audio",
			wantLength:     2048,
			wantDownloadab: true,
		},
		{
			name:           "a gif is a video on the wire",
			msg:            &waE2E.Message{VideoMessage: refVideo(true)},
			wantMediaType:  "gif",
			wantMimeType:   "video/mp4",
			wantMMSType:    "video",
			wantLength:     9000,
			wantDownloadab: true,
		},
		{
			name:           "a video",
			msg:            &waE2E.Message{VideoMessage: refVideo(false)},
			wantMediaType:  "video",
			wantMimeType:   "video/mp4",
			wantMMSType:    "video",
			wantLength:     9000,
			wantDownloadab: true,
		},
		{
			name:           "a document",
			msg:            &waE2E.Message{DocumentMessage: refDocument()},
			wantMediaType:  "document",
			wantMimeType:   "application/pdf",
			wantMMSType:    "document",
			wantLength:     70000,
			wantDownloadab: true,
		},
		{
			name:           "a sticker downloads with the image key",
			msg:            &waE2E.Message{StickerMessage: refSticker()},
			wantMediaType:  "sticker",
			wantMimeType:   "image/webp",
			wantMMSType:    "image",
			wantLength:     512,
			wantDownloadab: true,
		},

		// The wrappers. Each one hides the real message one level down.
		{
			name: "an ephemeral image",
			msg: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{ImageMessage: refImage()},
			}},
			wantMediaType: "image", wantMimeType: "image/jpeg", wantMMSType: "image",
			wantLength: 123456, wantDownloadab: true,
		},
		{
			name: "a view-once image",
			msg: &waE2E.Message{ViewOnceMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{ImageMessage: refImage()},
			}},
			wantMediaType: "image", wantMimeType: "image/jpeg", wantMMSType: "image",
			wantLength: 123456, wantDownloadab: true,
		},
		{
			name: "a view-once v2 voice note",
			msg: &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{AudioMessage: refAudio(true)},
			}},
			wantMediaType: "ptt", wantMimeType: "audio/ogg; codecs=opus", wantMMSType: "audio",
			wantLength: 2048, wantDownloadab: true,
		},
		{
			name: "a view-once v2 extension voice note",
			msg: &waE2E.Message{ViewOnceMessageV2Extension: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{AudioMessage: refAudio(true)},
			}},
			wantMediaType: "ptt", wantMimeType: "audio/ogg; codecs=opus", wantMMSType: "audio",
			wantLength: 2048, wantDownloadab: true,
		},
		{
			name: "a document with a caption",
			msg: &waE2E.Message{DocumentWithCaptionMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{DocumentMessage: refDocument()},
			}},
			wantMediaType: "document", wantMimeType: "application/pdf", wantMMSType: "document",
			wantLength: 70000, wantDownloadab: true,
		},
		{
			name: "wrappers nest",
			msg: &waE2E.Message{EphemeralMessage: &waE2E.FutureProofMessage{
				Message: &waE2E.Message{ViewOnceMessageV2: &waE2E.FutureProofMessage{
					Message: &waE2E.Message{ImageMessage: refImage()},
				}},
			}},
			wantMediaType: "image", wantMimeType: "image/jpeg", wantMMSType: "image",
			wantLength: 123456, wantDownloadab: true,
		},

		// Named media types that are not attachments at all: the vocabulary is
		// kept, but there is nothing to download.
		{
			name:          "a contact card names a type but has no bytes",
			msg:           &waE2E.Message{ContactMessage: &waE2E.ContactMessage{DisplayName: proto.String("Ana")}},
			wantMediaType: "vcard",
		},
		{
			name:          "a location names a type but has no bytes",
			msg:           &waE2E.Message{LocationMessage: &waE2E.LocationMessage{}},
			wantMediaType: "location",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mediaReference(tc.msg)

			if got.MediaType != tc.wantMediaType {
				t.Errorf("MediaType = %q, want %q", got.MediaType, tc.wantMediaType)
			}
			if got.MimeType != tc.wantMimeType {
				t.Errorf("MimeType = %q, want %q", got.MimeType, tc.wantMimeType)
			}
			if got.MMSType != tc.wantMMSType {
				t.Errorf("MMSType = %q, want %q", got.MMSType, tc.wantMMSType)
			}
			if got.FileLength != tc.wantLength {
				t.Errorf("FileLength = %d, want %d", got.FileLength, tc.wantLength)
			}
			if got.downloadable() != tc.wantDownloadab {
				t.Errorf("downloadable() = %v, want %v", got.downloadable(), tc.wantDownloadab)
			}
			if !tc.wantDownloadab {
				return
			}
			if got.DirectPath != refDirectPath {
				t.Errorf("DirectPath = %q, want %q", got.DirectPath, refDirectPath)
			}
			if !bytes.Equal(got.MediaKey, refMediaKey) {
				t.Errorf("MediaKey = %v, want %v", got.MediaKey, refMediaKey)
			}
			if !bytes.Equal(got.FileEncSHA256, refEncSHA) {
				t.Errorf("FileEncSHA256 = %v, want %v", got.FileEncSHA256, refEncSHA)
			}
			if !bytes.Equal(got.FileSHA256, refSHA) {
				t.Errorf("FileSHA256 = %v, want %v", got.FileSHA256, refSHA)
			}
		})
	}
}

// TestHistoryMediaTypeMatchesTheSharedUnwrapper is the invariant
// historyMediaType's comment asks for: the live and history paths must name the
// same attachment the same way, which is only guaranteed while one function
// decides it.
func TestHistoryMediaTypeMatchesTheSharedUnwrapper(t *testing.T) {
	messages := []*waE2E.Message{
		nil,
		{Conversation: proto.String("hola")},
		{ImageMessage: refImage()},
		{AudioMessage: refAudio(true)},
		{AudioMessage: refAudio(false)},
		{VideoMessage: refVideo(true)},
		{VideoMessage: refVideo(false)},
		{DocumentMessage: refDocument()},
		{StickerMessage: refSticker()},
		{ViewOnceMessageV2: &waE2E.FutureProofMessage{
			Message: &waE2E.Message{AudioMessage: refAudio(true)},
		}},
		{ContactMessage: &waE2E.ContactMessage{}},
		{LocationMessage: &waE2E.LocationMessage{}},
	}

	for i, msg := range messages {
		if got, want := historyMediaType(msg), mediaReference(msg).MediaType; got != want {
			t.Errorf("message %d: historyMediaType = %q, mediaReference = %q", i, got, want)
		}
	}
}
