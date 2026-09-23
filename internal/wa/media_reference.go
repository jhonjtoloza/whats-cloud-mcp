package wa

import (
	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/proto/waE2E"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// mediaRef is what a message says about its attachment without downloading it.
//
// MediaKey, FileEncSHA256 and FileSHA256 are key material: they decrypt the
// file on WhatsApp's CDN. They are never logged and never leave the gateway.
type mediaRef struct {
	// MediaType is the gateway's own vocabulary ("image", "ptt", "gif", ...),
	// the one whatsmeow puts on live message info.
	MediaType string
	// MMSType is what WhatsApp's media CDN calls the same thing ("image",
	// "audio", "video", "document"). It is a narrower set: a sticker and a
	// photo are both "image" there, and a voice note is plain "audio".
	MMSType       string
	MimeType      string
	DirectPath    string
	MediaKey      []byte
	FileEncSHA256 []byte
	FileSHA256    []byte
	FileLength    int64
}

// downloadable reports whether the reference is complete enough to fetch.
//
// The direct path is the one part no download can do without: whatsmeow rejects
// an empty path before it ever reaches the network. A contact card or a location
// has a media type and no bytes, and lands here as not downloadable.
func (r mediaRef) downloadable() bool { return r.DirectPath != "" }

// mediaReference unwraps a message down to its attachment.
//
// whatsmeow's own getDownloadableMessage is unexported, so this mirrors its
// behaviour rather than reaching into the library. It recurses through the same
// wrappers historyMediaType does — ephemeral, view-once v1/v2/v2-extension and
// document-with-caption — because a view-once voice note is a voice note, and
// storing it as a message with no attachment would quietly lose it.
//
// This is the single place both persistence paths derive media from, which is
// what keeps the live and history vocabularies comparable.
func mediaReference(msg *waProto.Message) mediaRef {
	if msg == nil {
		return mediaRef{}
	}

	switch {
	// Wrappers carry the real message inside them.
	case msg.GetEphemeralMessage() != nil:
		return mediaReference(msg.GetEphemeralMessage().GetMessage())
	case msg.GetViewOnceMessage() != nil:
		return mediaReference(msg.GetViewOnceMessage().GetMessage())
	case msg.GetViewOnceMessageV2() != nil:
		return mediaReference(msg.GetViewOnceMessageV2().GetMessage())
	case msg.GetViewOnceMessageV2Extension() != nil:
		return mediaReference(msg.GetViewOnceMessageV2Extension().GetMessage())
	case msg.GetDocumentWithCaptionMessage() != nil:
		return mediaReference(msg.GetDocumentWithCaptionMessage().GetMessage())

	case msg.GetImageMessage() != nil:
		part := msg.GetImageMessage()
		return downloadRef("image", part.GetMimetype(), part.GetFileLength(), part)

	case msg.GetStickerMessage() != nil:
		part := msg.GetStickerMessage()
		return downloadRef("sticker", part.GetMimetype(), part.GetFileLength(), part)

	case msg.GetDocumentMessage() != nil:
		part := msg.GetDocumentMessage()
		return downloadRef("document", part.GetMimetype(), part.GetFileLength(), part)

	case msg.GetAudioMessage() != nil:
		part := msg.GetAudioMessage()
		mediaType := "audio"
		if part.GetPTT() {
			mediaType = "ptt"
		}
		return downloadRef(mediaType, part.GetMimetype(), part.GetFileLength(), part)

	case msg.GetVideoMessage() != nil:
		part := msg.GetVideoMessage()
		mediaType := "video"
		if part.GetGifPlayback() {
			mediaType = "gif"
		}
		return downloadRef(mediaType, part.GetMimetype(), part.GetFileLength(), part)

	case msg.GetContactMessage() != nil:
		return mediaRef{MediaType: "vcard"}
	case msg.GetLocationMessage() != nil, msg.GetLiveLocationMessage() != nil:
		return mediaRef{MediaType: "location"}
	default:
		return mediaRef{}
	}
}

// downloadRef reads the download details off one attachment.
//
// The CDN type comes from whatsmeow.GetMediaType so the mapping stays the
// library's rather than ours: a sticker is fetched with the image key, and a
// voice note with the audio key.
func downloadRef(mediaType, mimeType string, length uint64, part whatsmeow.DownloadableMessage) mediaRef {
	return mediaRef{
		MediaType:     mediaType,
		MMSType:       mmsTypeFor(whatsmeow.GetMediaType(part)),
		MimeType:      mimeType,
		DirectPath:    part.GetDirectPath(),
		MediaKey:      part.GetMediaKey(),
		FileEncSHA256: part.GetFileEncSHA256(),
		FileSHA256:    part.GetFileSHA256(),
		FileLength:    int64(length),
	}
}

// mmsTypeFor mirrors whatsmeow's unexported mediaTypeToMMSType for the four
// types a conversation can carry.
//
// The library fills the mms type in itself when it is empty, so an unknown one
// is stored blank rather than guessed.
func mmsTypeFor(mediaType whatsmeow.MediaType) string {
	switch mediaType {
	case whatsmeow.MediaImage:
		return "image"
	case whatsmeow.MediaAudio:
		return "audio"
	case whatsmeow.MediaVideo:
		return "video"
	case whatsmeow.MediaDocument:
		return "document"
	default:
		return ""
	}
}

// applyMediaReference copies a reference onto the row about to be stored.
//
// A message with no attachment leaves every column NULL, and one that names a
// type without carrying bytes — a contact card, a location — records the type
// only. The bytes themselves are never fetched here: that is what FetchMedia is
// for, and it runs only when somebody asks.
func applyMediaReference(record *store.Message, ref mediaRef) {
	if ref.MediaType != "" {
		mediaType := ref.MediaType
		record.MediaType = &mediaType
	}
	if !ref.downloadable() {
		return
	}

	directPath := ref.DirectPath
	record.DirectPath = &directPath
	record.MediaKey = ref.MediaKey
	record.FileEncSHA256 = ref.FileEncSHA256
	record.FileSHA256 = ref.FileSHA256

	if ref.MimeType != "" {
		mimeType := ref.MimeType
		record.MimeType = &mimeType
	}
	if ref.MMSType != "" {
		mmsType := ref.MMSType
		record.MMSType = &mmsType
	}
	if ref.FileLength > 0 {
		length := ref.FileLength
		record.FileLength = &length
	}
}
