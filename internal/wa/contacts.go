package wa

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// maxContactResults caps a lookup. A contact list runs to thousands of entries
// and the result is meant to be read by a model, so a broad query returns the
// best few rather than everything that matched.
const maxContactResults = 25

// FindContacts resolves a free-text query against the tenant's address book.
//
// The match is case-insensitive over every name WhatsApp gives a contact plus
// the user part of the JID, because a caller may reasonably search by "Ana", by
// a business name, or by a phone number.
//
// Two sources are consulted beyond the address book, because a contact the
// caller means may be missing from it:
//
//   - the chats already stored for the tenant, so a conversation with somebody
//     never saved as a contact is still findable;
//   - IsOnWhatsApp, when the query is a bare phone number the user has never
//     saved, since that is still a perfectly valid destination.
func (m *Manager) FindContacts(ctx context.Context, tenantID, query string) ([]Contact, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	m.mu.RLock()
	client := m.clients[tenantID]
	m.mu.RUnlock()

	if client == nil || !client.IsLoggedIn() {
		return nil, ErrNotPaired
	}

	contacts, err := client.Store.Contacts.GetAllContacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("wa: read contacts: %w", err)
	}

	needle := strings.ToLower(query)
	found := make(map[string]Contact)
	for jid, info := range contacts {
		names := []string{info.FullName, info.FirstName, info.PushName, info.BusinessName, jid.User}
		if !anyContains(names, needle) {
			continue
		}
		address := jid.ToNonAD().String()
		found[address] = Contact{JID: address, Name: contactName(info, jid)}
	}

	// A bare number the user never saved is absent from the address book but is
	// still a perfectly valid destination, so ask the server about it directly.
	// IsOnWhatsApp wants international format, hence the "+".
	if isAllDigits(query) {
		responses, err := client.IsOnWhatsApp(ctx, []string{"+" + query})
		if err != nil {
			// A failed server lookup is not a failed search: whatever the
			// address book matched is still a useful answer.
			m.logger.WarnContext(ctx, "could not check a phone number on WhatsApp",
				slog.String("tenant_id", tenantID), slog.String("error", err.Error()))
		}
		for _, response := range responses {
			if !response.IsIn || response.JID.IsEmpty() {
				continue
			}
			address := response.JID.ToNonAD().String()
			if _, exists := found[address]; exists {
				continue
			}
			name := query
			if response.VerifiedName != nil && response.VerifiedName.Details.GetVerifiedName() != "" {
				name = response.VerifiedName.Details.GetVerifiedName()
			}
			found[address] = Contact{JID: address, Name: name}
		}
	}

	// A chat we already store may not be in the address book at all: the
	// contact was never saved, or was deleted after the conversation happened.
	// It is still the most useful answer there is, because its messages are
	// already readable.
	storedChats, err := m.messages.SearchChats(ctx, tenantID, query, maxContactResults)
	if err != nil {
		return nil, err
	}
	for _, chatJID := range storedChats {
		if _, exists := found[chatJID]; exists {
			continue
		}
		name := chatJID
		if jid, err := types.ParseJID(chatJID); err == nil {
			name = jid.User
		}
		found[chatJID] = Contact{JID: chatJID, Name: name}
	}

	out := make([]Contact, 0, len(found))
	for _, contact := range found {
		out = append(out, contact)
	}
	// Map iteration is random, so the result is ordered deliberately: a caller
	// paging through the first few must see a stable list.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].JID < out[j].JID
	})
	if len(out) > maxContactResults {
		out = out[:maxContactResults]
	}

	// Whether messages are already stored decides the caller's next step: read
	// them, or backfill first. One indexed lookup per candidate is cheap and
	// exact, and the candidate list is capped just above.
	for i := range out {
		stored, err := m.messages.ListByChat(ctx, tenantID, out[i].JID, 1)
		if err != nil {
			return nil, err
		}
		out[i].HasMessages = len(stored) > 0
	}
	return out, nil
}

// contactName picks the most human of the names WhatsApp knows a contact by.
func contactName(info types.ContactInfo, jid types.JID) string {
	for _, name := range []string{info.FullName, info.BusinessName, info.PushName, info.FirstName} {
		if strings.TrimSpace(name) != "" {
			return name
		}
	}
	return jid.User
}

// isAllDigits reports whether the query is a bare phone number rather than a
// name. A query carrying "+", spaces or dashes is left to the address book
// match, which already searches the JID user part.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	return notDigits.FindStringIndex(s) == nil
}

func anyContains(values []string, needle string) bool {
	for _, value := range values {
		if value != "" && strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	return false
}
