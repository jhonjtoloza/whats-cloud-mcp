package wa

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/config"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// TestHistoryDecisionFor pins the volume filter.
//
// The rule that matters is the one that is easy to get wrong: a chat type
// outside the scope is NOT dropped, it is reduced to its anchor. WhatsApp
// backfills the messages immediately before a message we already know, so a
// chat with zero stored rows can never be backfilled on demand. Filtering a
// chat type out entirely would therefore not be "less history for now", it
// would be "no history, ever".
func TestHistoryDecisionFor(t *testing.T) {
	var (
		dm        = types.NewJID("573001234567", types.DefaultUserServer)
		lidDM     = types.NewJID("98765432109876", types.HiddenUserServer)
		group     = types.NewJID("120363000000000000", types.GroupServer)
		channel   = types.NewJID("120363111111111111", types.NewsletterServer)
		status    = types.StatusBroadcastJID
		broadcast = types.NewJID("123456789", types.BroadcastServer)
	)

	tests := []struct {
		name  string
		scope config.HistorySyncScope
		chat  types.JID
		want  historyDecision
	}{
		{"dm scope stores direct chats in full", config.HistorySyncScopeDM, dm, historyPersistAll},
		{"dm scope stores lid direct chats in full", config.HistorySyncScopeDM, lidDM, historyPersistAll},
		{"dm scope keeps only the group anchor", config.HistorySyncScopeDM, group, historyAnchorOnly},
		{"dm scope keeps only the broadcast anchor", config.HistorySyncScopeDM, broadcast, historyAnchorOnly},
		{"dm scope skips channels entirely", config.HistorySyncScopeDM, channel, historySkipChat},
		{"dm scope skips status updates entirely", config.HistorySyncScopeDM, status, historySkipChat},

		{"dm,group scope stores direct chats in full", config.HistorySyncScopeDMGroup, dm, historyPersistAll},
		{"dm,group scope stores groups in full", config.HistorySyncScopeDMGroup, group, historyPersistAll},
		{"dm,group scope keeps only the broadcast anchor", config.HistorySyncScopeDMGroup, broadcast, historyAnchorOnly},
		{"dm,group scope still skips channels", config.HistorySyncScopeDMGroup, channel, historySkipChat},
		{"dm,group scope still skips status updates", config.HistorySyncScopeDMGroup, status, historySkipChat},

		{"all scope stores direct chats in full", config.HistorySyncScopeAll, dm, historyPersistAll},
		{"all scope stores groups in full", config.HistorySyncScopeAll, group, historyPersistAll},
		{"all scope stores broadcasts in full", config.HistorySyncScopeAll, broadcast, historyPersistAll},
		{"all scope stores channels in full", config.HistorySyncScopeAll, channel, historyPersistAll},
		// Status updates expire after 24h and are not a conversation anybody
		// asks an assistant about; they are skipped under every scope.
		{"all scope still skips status updates", config.HistorySyncScopeAll, status, historySkipChat},

		// An unset scope must not accidentally be the widest one.
		{"an empty scope behaves like the dm default", "", group, historyAnchorOnly},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := historyDecisionFor(tc.scope, tc.chat); got != tc.want {
				t.Errorf("historyDecisionFor(%q, %s) = %v, want %v", tc.scope, tc.chat, got, tc.want)
			}
		})
	}
}

// historyMsg builds one history-sync message the way WhatsApp delivers it:
// a waWeb.WebMessageInfo, which is a different shape from a live
// events.Message and needs its own mapping.
func historyMsg(id string, fromMe bool, remote, participant string, at time.Time, msg *waE2E.Message) *waHistorySync.HistorySyncMsg {
	key := &waCommon.MessageKey{
		ID:        proto.String(id),
		FromMe:    proto.Bool(fromMe),
		RemoteJID: proto.String(remote),
	}
	if participant != "" {
		key.Participant = proto.String(participant)
	}
	return &waHistorySync.HistorySyncMsg{
		Message: &waWeb.WebMessageInfo{
			Key:              key,
			Message:          msg,
			MessageTimestamp: proto.Uint64(uint64(at.Unix())),
		},
	}
}

func textMsg(body string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(body)}
}

func TestHistoryRecordsAnchorOnly(t *testing.T) {
	var (
		tenantID = "tenant-a"
		ownJID   = types.NewJID("573001111111", types.DefaultUserServer)
		group    = types.NewJID("120363000000000000", types.GroupServer)
		base     = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	)

	messages := []*waHistorySync.HistorySyncMsg{
		historyMsg("g1", false, group.String(), "573002222222@s.whatsapp.net", base, textMsg("oldest")),
		historyMsg("g2", false, group.String(), "573002222222@s.whatsapp.net", base.Add(time.Hour), textMsg("middle")),
		historyMsg("g3", false, group.String(), "573003333333@s.whatsapp.net", base.Add(2*time.Hour), textMsg("newest")),
	}

	tests := []struct {
		name     string
		decision historyDecision
		wantIDs  []string
	}{
		{"an included chat keeps everything delivered", historyPersistAll, []string{"g1", "g2", "g3"}},
		// This is the load-bearing case: an excluded chat type still keeps its
		// most recent message so it can be backfilled on demand later.
		{"an excluded chat keeps its newest message as the anchor", historyAnchorOnly, []string{"g3"}},
		{"a skipped chat keeps nothing", historySkipChat, nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := historyRecords(tenantID, ownJID, group, tc.decision, messages)
			if len(got) != len(tc.wantIDs) {
				t.Fatalf("historyRecords() returned %d records, want %d", len(got), len(tc.wantIDs))
			}
			for i, want := range tc.wantIDs {
				if got[i].WAMessageID != want {
					t.Errorf("record[%d].WAMessageID = %q, want %q", i, got[i].WAMessageID, want)
				}
			}
		})
	}
}

func TestHistoryRecordsMapping(t *testing.T) {
	var (
		tenantID = "tenant-a"
		ownJID   = types.NewJID("573001111111", types.DefaultUserServer)
		dm       = types.NewJID("573001234567", types.DefaultUserServer)
		group    = types.NewJID("120363000000000000", types.GroupServer)
		at       = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	)

	tests := []struct {
		name          string
		chat          types.JID
		msg           *waHistorySync.HistorySyncMsg
		wantSender    string
		wantDirection store.Direction
		wantBody      string
		wantMediaType string
	}{
		{
			name:          "an incoming direct message",
			chat:          dm,
			msg:           historyMsg("d1", false, dm.String(), "", at, textMsg("hello there")),
			wantSender:    dm.String(),
			wantDirection: store.DirectionIn,
			wantBody:      "hello there",
		},
		{
			name:          "an outgoing direct message is attributed to the tenant's own jid",
			chat:          dm,
			msg:           historyMsg("d2", true, dm.String(), "", at, textMsg("my reply")),
			wantSender:    ownJID.String(),
			wantDirection: store.DirectionOut,
			wantBody:      "my reply",
		},
		{
			name:          "a group message is attributed to its participant",
			chat:          group,
			msg:           historyMsg("g1", false, group.String(), "573009999999@s.whatsapp.net", at, textMsg("in the group")),
			wantSender:    "573009999999@s.whatsapp.net",
			wantDirection: store.DirectionIn,
			wantBody:      "in the group",
		},
		{
			name:          "an outgoing group message falls back to the tenant's own jid",
			chat:          group,
			msg:           historyMsg("g2", true, group.String(), "", at, textMsg("mine")),
			wantSender:    ownJID.String(),
			wantDirection: store.DirectionOut,
			wantBody:      "mine",
		},
		{
			name: "an image keeps its caption and records its media type",
			chat: dm,
			msg: historyMsg("d3", false, dm.String(), "", at, &waE2E.Message{
				ImageMessage: &waE2E.ImageMessage{Caption: proto.String("look at this")},
			}),
			wantSender:    dm.String(),
			wantDirection: store.DirectionIn,
			wantBody:      "look at this",
			wantMediaType: "image",
		},
		{
			name: "a voice note has no body but a media type",
			chat: dm,
			msg: historyMsg("d4", false, dm.String(), "", at, &waE2E.Message{
				AudioMessage: &waE2E.AudioMessage{PTT: proto.Bool(true)},
			}),
			wantSender:    dm.String(),
			wantDirection: store.DirectionIn,
			wantMediaType: "ptt",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := historyRecords(tenantID, ownJID, tc.chat, historyPersistAll, []*waHistorySync.HistorySyncMsg{tc.msg})
			if len(got) != 1 {
				t.Fatalf("historyRecords() returned %d records, want 1", len(got))
			}
			rec := got[0]

			if rec.TenantID != tenantID {
				t.Errorf("TenantID = %q, want %q", rec.TenantID, tenantID)
			}
			if rec.ChatJID != tc.chat.String() {
				t.Errorf("ChatJID = %q, want %q", rec.ChatJID, tc.chat.String())
			}
			if rec.SenderJID != tc.wantSender {
				t.Errorf("SenderJID = %q, want %q", rec.SenderJID, tc.wantSender)
			}
			if rec.Direction != tc.wantDirection {
				t.Errorf("Direction = %q, want %q", rec.Direction, tc.wantDirection)
			}
			if rec.Body != tc.wantBody {
				t.Errorf("Body = %q, want %q", rec.Body, tc.wantBody)
			}
			if !rec.Timestamp.Equal(at) {
				t.Errorf("Timestamp = %v, want %v", rec.Timestamp, at)
			}
			switch {
			case tc.wantMediaType == "" && rec.MediaType != nil:
				t.Errorf("MediaType = %q, want none", *rec.MediaType)
			case tc.wantMediaType != "" && rec.MediaType == nil:
				t.Errorf("MediaType = none, want %q", tc.wantMediaType)
			case tc.wantMediaType != "" && *rec.MediaType != tc.wantMediaType:
				t.Errorf("MediaType = %q, want %q", *rec.MediaType, tc.wantMediaType)
			}
			if rec.ID == "" {
				t.Error("ID is empty; every stored message needs one")
			}
		})
	}
}

// TestHistoryRecordsSkipsUnusableMessages keeps malformed history out of the
// database instead of letting it fail the whole batch.
func TestHistoryRecordsSkipsUnusableMessages(t *testing.T) {
	var (
		ownJID = types.NewJID("573001111111", types.DefaultUserServer)
		dm     = types.NewJID("573001234567", types.DefaultUserServer)
		at     = time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	)

	messages := []*waHistorySync.HistorySyncMsg{
		nil,
		{Message: nil},
		{Message: &waWeb.WebMessageInfo{}}, // no key at all
		historyMsg("", false, dm.String(), "", at, textMsg("no id")),         // no message id
		historyMsg("ok", false, dm.String(), "", at, textMsg("this one is")), // usable
	}

	got := historyRecords("tenant-a", ownJID, dm, historyPersistAll, messages)
	if len(got) != 1 {
		t.Fatalf("historyRecords() returned %d records, want 1 usable one", len(got))
	}
	if got[0].WAMessageID != "ok" {
		t.Errorf("kept %q, want the only usable message", got[0].WAMessageID)
	}
}
