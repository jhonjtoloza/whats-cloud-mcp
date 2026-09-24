package wa

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// fakeLIDs stands in for whatsmeow's LID store, mapping the bare user part of a
// LID onto the bare user part of a phone number, exactly as the
// whatsmeow_lid_map table does.
//
// It reproduces the real contract rather than a convenient one: an address that
// has no mapping comes back as the empty JID with a nil error, and a call with
// a non-LID JID is a programming error and fails.
type fakeLIDs struct {
	byLID map[string]string
	err   error
}

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if f.err != nil {
		return types.JID{}, f.err
	}
	if lid.Server != types.HiddenUserServer {
		return types.JID{}, errors.New("invalid GetPNForLID call with a non-LID JID")
	}
	pn, ok := f.byLID[lid.User]
	if !ok {
		return types.JID{}, nil
	}
	return types.NewJID(pn, types.DefaultUserServer), nil
}

// TestCanonicalJID pins the address every stored row is keyed by.
//
// WhatsApp addresses the same person two ways, by phone number and by LID, and
// storing whichever arrived splits one conversation in two. The canonical form
// is the phone number with no device suffix, and there are exactly three ways
// to reach it: the alternative address the event carried, the LID index, or
// nothing at all — in which case the raw LID is kept rather than an address
// being invented.
func TestCanonicalJID(t *testing.T) {
	lids := fakeLIDs{byLID: map[string]string{"15285906083900": "573004725680"}}

	tests := []struct {
		name          string
		jid           types.JID
		alt           types.JID
		lids          lidResolver
		want          string
		wantCanonical bool
	}{
		{
			name:          "a phone number is already canonical",
			jid:           types.NewJID("573004725680", types.DefaultUserServer),
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "the device suffix is always stripped",
			jid:           types.JID{User: "573114276555", Device: 87, Server: types.DefaultUserServer},
			lids:          lids,
			want:          "573114276555@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "the alternative address on the event wins",
			jid:           types.NewJID("15285906083900", types.HiddenUserServer),
			alt:           types.JID{User: "573004725680", Device: 12, Server: types.DefaultUserServer},
			lids:          fakeLIDs{},
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "a lid with no alternative address falls back to the lid index",
			jid:           types.JID{User: "15285906083900", Device: 3, Server: types.HiddenUserServer},
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "an unresolvable lid keeps its own address",
			jid:           types.NewJID("99999999999999", types.HiddenUserServer),
			lids:          lids,
			want:          "99999999999999@lid",
			wantCanonical: false,
		},
		{
			name:          "an unresolvable lid loses its device suffix all the same",
			jid:           types.JID{User: "99999999999999", Device: 5, Server: types.HiddenUserServer},
			lids:          lids,
			want:          "99999999999999@lid",
			wantCanonical: false,
		},
		{
			name:          "a failing lid index never invents an address",
			jid:           types.NewJID("15285906083900", types.HiddenUserServer),
			lids:          fakeLIDs{err: errors.New("database is locked")},
			want:          "15285906083900@lid",
			wantCanonical: false,
		},
		{
			name:          "no lid index at all keeps the raw address",
			jid:           types.NewJID("15285906083900", types.HiddenUserServer),
			lids:          nil,
			want:          "15285906083900@lid",
			wantCanonical: false,
		},
		{
			name:          "a group address is never rewritten",
			jid:           types.NewJID("120363000000000000", types.GroupServer),
			lids:          lids,
			want:          "120363000000000000@g.us",
			wantCanonical: true,
		},
		{
			name:          "an alternative address that is not a phone number is ignored",
			jid:           types.NewJID("15285906083900", types.HiddenUserServer),
			alt:           types.NewJID("15285906083900", types.HiddenUserServer),
			lids:          lids,
			want:          "573004725680@s.whatsapp.net",
			wantCanonical: true,
		},
		{
			name:          "an empty address stays empty",
			jid:           types.EmptyJID,
			lids:          lids,
			want:          "",
			wantCanonical: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, canonical := canonicalJID(context.Background(), tc.lids, tc.jid, tc.alt)

			if got.String() != tc.want {
				t.Errorf("canonicalJID(%s, %s) = %q, want %q", tc.jid, tc.alt, got.String(), tc.want)
			}
			if canonical != tc.wantCanonical {
				t.Errorf("canonicalJID(%s, %s) canonical = %v, want %v", tc.jid, tc.alt, canonical, tc.wantCanonical)
			}
		})
	}
}

// TestChatAlt covers the asymmetry that makes the live path easy to get wrong.
//
// A direct conversation IS a person, so the alternative address of the chat
// travels on whichever end of the message that person is on: a message we
// received has Chat == Sender and carries SenderAlt, while a message we sent
// has Chat == the recipient and carries RecipientAlt. Reading only one of the
// two would leave every incoming LID chat unresolved.
func TestChatAlt(t *testing.T) {
	var (
		peerLID = types.NewJID("15285906083900", types.HiddenUserServer)
		peerPN  = types.NewJID("573004725680", types.DefaultUserServer)
		ownJID  = types.NewJID("573114276555", types.DefaultUserServer)
		group   = types.NewJID("120363000000000000", types.GroupServer)
	)

	tests := []struct {
		name   string
		source types.MessageSource
		want   string
	}{
		{
			name:   "a received direct message carries the chat address as SenderAlt",
			source: types.MessageSource{Chat: peerLID, Sender: peerLID, SenderAlt: peerPN},
			want:   "573004725680@s.whatsapp.net",
		},
		{
			name: "a sent direct message carries the chat address as RecipientAlt",
			source: types.MessageSource{
				Chat: peerLID, Sender: ownJID, IsFromMe: true, RecipientAlt: peerPN,
			},
			want: "573004725680@s.whatsapp.net",
		},
		{
			name: "a group chat has no alternative address",
			source: types.MessageSource{
				Chat: group, Sender: peerLID, IsGroup: true, SenderAlt: peerPN,
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chatAlt(tc.source).String(); got != tc.want {
				t.Errorf("chatAlt() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLiveRecordDirection is the regression test for the second defect: the
// live path hardcoded store.DirectionIn and never read IsFromMe, so every
// message the tenant sent from their own phone was stored as received. The
// history path has always derived the direction from the message key, and that
// asymmetry is what hid the bug.
func TestLiveRecordDirection(t *testing.T) {
	var (
		chat   = types.NewJID("573004725680", types.DefaultUserServer)
		peer   = chat
		ownJID = types.NewJID("573114276555", types.DefaultUserServer)
		at     = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	)

	tests := []struct {
		name     string
		fromMe   bool
		sender   types.JID
		wantDir  store.Direction
		wantSend string
	}{
		{
			name:     "a received message is inbound",
			fromMe:   false,
			sender:   peer,
			wantDir:  store.DirectionIn,
			wantSend: "573004725680@s.whatsapp.net",
		},
		{
			name:     "a message the tenant sent is outbound",
			fromMe:   true,
			sender:   ownJID,
			wantDir:  store.DirectionOut,
			wantSend: "573114276555@s.whatsapp.net",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			evt := &events.Message{
				Info: types.MessageInfo{
					MessageSource: types.MessageSource{Chat: chat, Sender: tc.sender, IsFromMe: tc.fromMe},
					ID:            "wa-1",
					Timestamp:     at,
				},
				Message: textMsg("hello"),
			}

			record := liveRecord("tenant-a", evt, chat, tc.sender)

			if record.Direction != tc.wantDir {
				t.Errorf("Direction = %q, want %q", record.Direction, tc.wantDir)
			}
			if record.SenderJID != tc.wantSend {
				t.Errorf("SenderJID = %q, want %q", record.SenderJID, tc.wantSend)
			}
			if record.ChatJID != "573004725680@s.whatsapp.net" {
				t.Errorf("ChatJID = %q, want the canonical chat address", record.ChatJID)
			}
			if record.WAMessageID != "wa-1" {
				t.Errorf("WAMessageID = %q, want %q", record.WAMessageID, "wa-1")
			}
		})
	}
}

// TestLiveRecordStoresNonADAddresses pins the third normalisation axis: the
// tenant's own JID carries a device suffix (":87"), and a row written with it
// can never be matched by a string comparison against the plain number.
func TestLiveRecordStoresNonADAddresses(t *testing.T) {
	var (
		chat   = types.JID{User: "573004725680", Device: 12, Server: types.DefaultUserServer}
		sender = types.JID{User: "573114276555", Device: 87, Server: types.DefaultUserServer}
		at     = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	)

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: true},
			ID:            "wa-2",
			Timestamp:     at,
		},
		Message: textMsg("hello"),
	}

	record := liveRecord("tenant-a", evt, chat, sender)

	if record.ChatJID != "573004725680@s.whatsapp.net" {
		t.Errorf("ChatJID = %q, want it stored without a device", record.ChatJID)
	}
	if record.SenderJID != "573114276555@s.whatsapp.net" {
		t.Errorf("SenderJID = %q, want it stored without a device", record.SenderJID)
	}
}

// TestCanonicaliseSenders covers the other write path.
//
// A history sync stores rows too, so leaving it addressing people by LID would
// re-split the very conversations the migration merged. It has no alternative
// address to work from — a history message key carries one participant and
// nothing else — so the LID index is the only source here.
func TestCanonicaliseSenders(t *testing.T) {
	manager := &Manager{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	lids := fakeLIDs{byLID: map[string]string{"15285906083900": "573004725680"}}

	records := []store.Message{
		{SenderJID: "15285906083900@lid"},
		{SenderJID: "99999999999999@lid"},
		{SenderJID: "573114276555:87@s.whatsapp.net"},
		{SenderJID: "573002222222@s.whatsapp.net"},
		{SenderJID: ""},
	}
	want := []string{
		"573004725680@s.whatsapp.net",
		"99999999999999@lid",
		"573114276555@s.whatsapp.net",
		"573002222222@s.whatsapp.net",
		"",
	}

	manager.canonicaliseSenders(context.Background(), lids, "tenant-a", records)

	for i := range want {
		if records[i].SenderJID != want[i] {
			t.Errorf("record[%d].SenderJID = %q, want %q", i, records[i].SenderJID, want[i])
		}
	}
}

// TestLiveRecordResolvesTheSenderOfAGroupMessage keeps the group rule explicit:
// a group keeps its own @g.us address, and only the participant is resolved.
func TestLiveRecordResolvesTheSenderOfAGroupMessage(t *testing.T) {
	ctx := context.Background()
	lids := fakeLIDs{byLID: map[string]string{"15285906083900": "573004725680"}}

	group := types.NewJID("120363000000000000", types.GroupServer)
	sender := types.NewJID("15285906083900", types.HiddenUserServer)

	source := types.MessageSource{Chat: group, Sender: sender, IsGroup: true}
	chat, _ := canonicalJID(ctx, lids, source.Chat, chatAlt(source))
	resolved, _ := canonicalJID(ctx, lids, source.Sender, source.SenderAlt)

	evt := &events.Message{
		Info: types.MessageInfo{
			MessageSource: source,
			ID:            "wa-3",
			Timestamp:     time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
		},
		Message: textMsg("hello"),
	}

	record := liveRecord("tenant-a", evt, chat, resolved)

	if record.ChatJID != "120363000000000000@g.us" {
		t.Errorf("ChatJID = %q, want the group address to survive untouched", record.ChatJID)
	}
	if record.SenderJID != "573004725680@s.whatsapp.net" {
		t.Errorf("SenderJID = %q, want the participant resolved to a phone number", record.SenderJID)
	}
}
