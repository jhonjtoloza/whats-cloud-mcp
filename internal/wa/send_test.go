package wa

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// TestSendDestination is the outbound half of the address defect.
//
// The inbound paths were fixed to fold the two addresses of one person onto the
// phone number; sending kept persisting whatever string the caller typed, so a
// send to "573004725680" filed a conversation the inbound path had just merged
// under "573004725680@s.whatsapp.net" into a third, separate chat. Every way of
// naming the same person has to converge here, before the message leaves and
// before the row is written.
func TestSendDestination(t *testing.T) {
	lids := fakeLIDs{byLID: map[string]string{"15285906083900": "573004725680"}}

	tests := []struct {
		name          string
		to            string
		lids          lidResolver
		want          string
		wantCanonical bool
		wantErr       error
	}{
		{
			name:          "a bare number gains the default user server",
			to:            "573004725680",
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "a number typed with punctuation reaches the same chat",
			to:            "+57 300 472-5680",
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "a full phone jid is already the stored address",
			to:            "573004725680@s.whatsapp.net",
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "a device suffix never reaches the stored address",
			to:            "573004725680:12@s.whatsapp.net",
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "a lid with a mapping converges on the phone number",
			to:            "15285906083900@lid",
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "an unresolvable lid keeps its own address",
			to:            "99999999999999@lid",
			lids:          lids,
			want:          "99999999999999@lid",
			wantCanonical: false,
		},
		{
			name:          "no lid index at all still sends to the raw address",
			to:            "15285906083900@lid",
			lids:          nil,
			want:          "15285906083900@lid",
			wantCanonical: false,
		},
		{
			name:          "a group address is never rewritten",
			to:            "120363000000000000@g.us",
			lids:          lids,
			want:          "120363000000000000@g.us",
			wantCanonical: true,
		},
		{
			name:    "an empty destination is refused",
			to:      "   ",
			lids:    lids,
			wantErr: ErrInvalidJID,
		},
		{
			name:    "a destination with no digits at all is refused",
			to:      "not-a-jid",
			lids:    lids,
			wantErr: ErrInvalidJID,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, canonical, err := sendDestination(context.Background(), tc.lids, tc.to)

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("sendDestination(%q) error = %v, want %v", tc.to, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("sendDestination(%q) error = %v", tc.to, err)
			}
			if got.String() != tc.want {
				t.Errorf("sendDestination(%q) = %q, want %q", tc.to, got.String(), tc.want)
			}
			if canonical != tc.wantCanonical {
				t.Errorf("sendDestination(%q) canonical = %v, want %v", tc.to, canonical, tc.wantCanonical)
			}
		})
	}
}

// TestSentRecord pins what a send reports back for persistence.
//
// Three things used to be invented at the call site: the chat address came from
// the caller's raw string, the sender was left empty, and the timestamp was
// time.Now(). Only the send knows the resolved address, the tenant's own JID and
// the moment WhatsApp recorded, so all three are read off the response here.
func TestSentRecord(t *testing.T) {
	var (
		chat     = types.NewJID("573004725680", types.DefaultUserServer)
		chatLID  = types.NewJID("15285906083900", types.HiddenUserServer)
		own      = types.NewJID("573114276555", types.DefaultUserServer)
		ownAD    = types.JID{User: "573114276555", Device: 87, Server: types.DefaultUserServer}
		serverAt = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	)

	tests := []struct {
		name          string
		resp          whatsmeow.SendResponse
		chat          types.JID
		own           types.JID
		wantChat      string
		wantSender    string
		wantID        string
		wantTimestamp time.Time
	}{
		{
			name:          "the server timestamp and id are reported as they arrived",
			resp:          whatsmeow.SendResponse{ID: "WA-1", Timestamp: serverAt, Chat: chat, Sender: own},
			chat:          chat,
			own:           own,
			wantChat:      "573004725680@s.whatsapp.net",
			wantSender:    "573114276555@s.whatsapp.net",
			wantID:        "WA-1",
			wantTimestamp: serverAt,
		},
		{
			name:          "the tenant's own device suffix never reaches the row",
			resp:          whatsmeow.SendResponse{ID: "WA-2", Timestamp: serverAt},
			chat:          chat,
			own:           ownAD,
			wantChat:      "573004725680@s.whatsapp.net",
			wantSender:    "573114276555@s.whatsapp.net",
			wantID:        "WA-2",
			wantTimestamp: serverAt,
		},
		{
			name: "the address we resolved wins over the one whatsmeow sent on",
			// whatsmeow may swap the destination for a LID on its way out, so
			// the response's own Chat would re-split the conversation.
			resp:          whatsmeow.SendResponse{ID: "WA-3", Timestamp: serverAt, Chat: chatLID},
			chat:          chat,
			own:           own,
			wantChat:      "573004725680@s.whatsapp.net",
			wantSender:    "573114276555@s.whatsapp.net",
			wantID:        "WA-3",
			wantTimestamp: serverAt,
		},
		{
			name:          "an unresolvable destination keeps its raw address",
			resp:          whatsmeow.SendResponse{ID: "WA-4", Timestamp: serverAt},
			chat:          types.NewJID("99999999999999", types.HiddenUserServer),
			own:           own,
			wantChat:      "99999999999999@lid",
			wantSender:    "573114276555@s.whatsapp.net",
			wantID:        "WA-4",
			wantTimestamp: serverAt,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sentRecord(tc.resp, tc.chat, tc.own)

			if got.ChatJID != tc.wantChat {
				t.Errorf("ChatJID = %q, want %q", got.ChatJID, tc.wantChat)
			}
			if got.SenderJID != tc.wantSender {
				t.Errorf("SenderJID = %q, want %q", got.SenderJID, tc.wantSender)
			}
			if got.WAMessageID != tc.wantID {
				t.Errorf("WAMessageID = %q, want %q", got.WAMessageID, tc.wantID)
			}
			if !got.Timestamp.Equal(tc.wantTimestamp) {
				t.Errorf("Timestamp = %s, want %s", got.Timestamp, tc.wantTimestamp)
			}
			if got.Timestamp.Location() != time.UTC {
				t.Errorf("Timestamp location = %s, want UTC", got.Timestamp.Location())
			}
		})
	}
}

// TestSentRecordWithoutAServerTimestamp keeps the fallback honest.
//
// The timestamp comes off the "t" attribute of the server's acknowledgement. A
// response that carries none leaves a zero time, and a zero timestamp would
// file the message at the beginning of the epoch, so the send time is used
// instead — and it is the only case where a clock of ours is.
func TestSentRecordWithoutAServerTimestamp(t *testing.T) {
	own := types.NewJID("573114276555", types.DefaultUserServer)
	chat := types.NewJID("573004725680", types.DefaultUserServer)

	before := time.Now().UTC()
	got := sentRecord(whatsmeow.SendResponse{ID: "WA-5"}, chat, own)
	after := time.Now().UTC()

	if got.Timestamp.Before(before) || got.Timestamp.After(after) {
		t.Errorf("Timestamp = %s, want it to fall back to the send time between %s and %s",
			got.Timestamp, before, after)
	}
}
