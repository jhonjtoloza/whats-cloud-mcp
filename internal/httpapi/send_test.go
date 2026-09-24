package httpapi_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
	"github.com/jhonjtoloza/whats-cloud-mcp/internal/wa"
)

// TestSendMessageStoresOneChatPerPerson is the outbound half of the address
// defect.
//
// The three strings below name one human being: the number as a person types
// it, the same number as an address, and the LID WhatsApp uses for them behind
// the scenes. The handler used to persist whichever one the caller sent, which
// created three conversations for one person and re-split exactly what the
// inbound fix merged. What the send reports is the single address all three
// have to land on.
func TestSendMessageStoresOneChatPerPerson(t *testing.T) {
	const canonical = "573004725680@s.whatsapp.net"

	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send", "messages:read"})

	// One human being, three ways of naming them. The send resolves all three
	// onto the same address, as the manager does with the LID index.
	destinations := map[string]string{
		"573004725680":       "WA-MSG-BARE",
		canonical:            "WA-MSG-PHONE",
		"15285906083900@lid": "WA-MSG-LID",
	}
	env.sessions.sendResults = map[string]wa.SentMessage{}
	for to, waMessageID := range destinations {
		env.sessions.sendResults[to] = wa.SentMessage{
			WAMessageID: waMessageID,
			ChatJID:     canonical,
			SenderJID:   fakeOwnJID,
			Timestamp:   fakeSentAt,
		}
	}

	for to := range destinations {
		rec := env.do(t, http.MethodPost, "/v1/messages", tenant.APIKey.Key, map[string]any{
			"to": to, "body": "hello",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("send to %q: status = %d, want 201 (body %s)", to, rec.Code, rec.Body.String())
		}
	}

	rec := env.do(t, http.MethodGet, "/v1/chats/"+canonical+"/messages", tenant.APIKey.Key, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	listed := decode[listMessagesView](t, rec)
	if len(listed.Messages) != len(destinations) {
		t.Fatalf("listing the chat by phone number returned %d messages, want %d",
			len(listed.Messages), len(destinations))
	}
	for i, msg := range listed.Messages {
		if msg.ChatJID != canonical {
			t.Errorf("messages[%d].chat_jid = %q, want %q", i, msg.ChatJID, canonical)
		}
		if msg.Direction != string(store.DirectionOut) {
			t.Errorf("messages[%d].direction = %q, want %q", i, msg.Direction, store.DirectionOut)
		}
	}
}

// TestSendMessagePersistsWhatTheSendReported covers the two remaining invented
// values: an outbound row used to have no sender at all, and a timestamp of
// time.Now() rather than the one WhatsApp recorded.
func TestSendMessagePersistsWhatTheSendReported(t *testing.T) {
	const canonical = "573004725680@s.whatsapp.net"

	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})
	env.sessions.sendResult = wa.SentMessage{
		WAMessageID: "WA-MSG-1",
		ChatJID:     canonical,
		SenderJID:   fakeOwnJID,
		Timestamp:   fakeSentAt,
	}

	// The caller types a bare number; the row must carry the address the send
	// resolved it to, not the string that was typed.
	rec := env.do(t, http.MethodPost, "/v1/messages", tenant.APIKey.Key, map[string]any{
		"to": "573004725680", "body": "hello",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	stored, err := env.db.Messages().ListByChat(context.Background(), tenant.TenantID, canonical, 10)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("persisted %d outbound messages, want 1", len(stored))
	}

	got := stored[0]
	if got.ChatJID != canonical {
		t.Errorf("ChatJID = %q, want the address the send resolved %q", got.ChatJID, canonical)
	}
	if got.SenderJID != fakeOwnJID {
		t.Errorf("SenderJID = %q, want the tenant's own address %q", got.SenderJID, fakeOwnJID)
	}
	if !got.Timestamp.Equal(fakeSentAt) {
		t.Errorf("Timestamp = %s, want the timestamp the send reported %s", got.Timestamp, fakeSentAt)
	}
	if time.Since(got.Timestamp) < time.Hour {
		t.Errorf("Timestamp = %s, want a stored value rather than the moment of the call", got.Timestamp)
	}
}

// TestSendMessageKeepsAnUnresolvableAddress pins the honest fallback: a LID no
// mapping covers is still sent to and still stored, under the only address
// anybody has for it. Nothing is invented and nothing is dropped.
func TestSendMessageKeepsAnUnresolvableAddress(t *testing.T) {
	const unresolvable = "99999999999999@lid"

	env := newTestEnv(t)
	tenant := env.createTenant(t, "Acme", []string{"messages:send"})
	env.sessions.sendResult = wa.SentMessage{
		WAMessageID: "WA-MSG-LID",
		ChatJID:     unresolvable,
		SenderJID:   fakeOwnJID,
		Timestamp:   fakeSentAt,
	}

	rec := env.do(t, http.MethodPost, "/v1/messages", tenant.APIKey.Key, map[string]any{
		"to": unresolvable, "body": "hello",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}

	stored, err := env.db.Messages().ListByChat(context.Background(), tenant.TenantID, unresolvable, 10)
	if err != nil {
		t.Fatalf("ListByChat() error = %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("persisted %d messages under %q, want 1", len(stored), unresolvable)
	}
	if stored[0].ChatJID != unresolvable {
		t.Errorf("ChatJID = %q, want the raw address %q", stored[0].ChatJID, unresolvable)
	}
	if stored[0].SenderJID != fakeOwnJID {
		t.Errorf("SenderJID = %q, want the tenant's own address %q", stored[0].SenderJID, fakeOwnJID)
	}
}

// listMessagesView decodes the chat listing, which embeds the stored row.
type listMessagesView struct {
	ChatJID  string `json:"chat_jid"`
	Messages []struct {
		ChatJID   string `json:"chat_jid"`
		SenderJID string `json:"sender_jid"`
		Direction string `json:"direction"`
	} `json:"messages"`
}
