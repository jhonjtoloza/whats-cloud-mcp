package wa

import (
	"context"

	"go.mau.fi/whatsmeow/types"
)

// lidResolver is the slice of whatsmeow's store.LIDStore the persistence path
// needs, narrowed to one method so the resolution rules can be tested without a
// live socket. The real implementation is client.Store.LIDs, backed by the
// whatsmeow_lid_map table in the gateway's own database file.
type lidResolver interface {
	// GetPNForLID returns the phone-number address behind a LID, or the empty
	// JID when whatsmeow has never seen the pair. An unknown mapping is not an
	// error.
	GetPNForLID(ctx context.Context, lid types.JID) (types.JID, error)
}

// canonicalJID reduces one WhatsApp address to the single form a row is stored
// under: a phone number, with no device suffix.
//
// WhatsApp addresses the same person two ways — "573004725680@s.whatsapp.net"
// and "15285906083900@lid" — and storing whichever one arrived splits one
// conversation into two, so a query by phone number silently misses the newest
// messages. The phone number is the canonical half because it is the address a
// human types, a contact list holds and an API caller sends to.
//
// Three sources are tried, in this order:
//
//   - alt, the alternative address the event itself carried. It travels WITH
//     the message, so it needs no lookup and cannot be stale.
//   - the LID index, which whatsmeow fills from the same events and keeps in
//     the very same SQLite file.
//   - nothing. The raw LID is then kept exactly as it is.
//
// The last case is deliberate: an address that cannot be resolved must never be
// invented and must never quietly become a second conversation under a made-up
// number. The returned bool reports whether the address is canonical, which is
// the only thing the caller can count.
//
// Every result is non-AD. The tenant's own JID carries a device
// ("573114276555:87@s.whatsapp.net"), and a row written with it can never be
// matched by a comparison against the plain address, which is a third way the
// same conversation splits.
func canonicalJID(ctx context.Context, lids lidResolver, jid, alt types.JID) (types.JID, bool) {
	if jid.IsEmpty() {
		return jid, true
	}
	// A group, a broadcast list, a newsletter and a phone number are all
	// already the address they will be stored under.
	if jid.Server != types.HiddenUserServer {
		return jid.ToNonAD(), true
	}

	if alt.Server == types.DefaultUserServer && alt.User != "" {
		return alt.ToNonAD(), true
	}

	if lids != nil {
		// GetPNForLID insists on a LID with no device, and answers with the
		// empty JID rather than an error when the pair is unknown.
		if pn, err := lids.GetPNForLID(ctx, jid.ToNonAD()); err == nil && !pn.IsEmpty() {
			return pn.ToNonAD(), true
		}
	}
	return jid.ToNonAD(), false
}

// chatAlt picks the alternative address of the conversation a live message
// belongs to.
//
// The asymmetry here is what makes the live path easy to get wrong. A direct
// conversation IS a person, so the alternative address of the chat travels on
// whichever end of the message that person is on: a message we received has
// Chat == Sender and carries SenderAlt, while a message we sent has Chat ==
// the recipient and carries RecipientAlt. Reading only RecipientAlt would leave
// every incoming LID chat unresolved, which is the overwhelming majority.
//
// A group has no alternative address at all: only its participants do.
func chatAlt(source types.MessageSource) types.JID {
	if source.IsGroup {
		return types.EmptyJID
	}
	if source.IsFromMe {
		return source.RecipientAlt
	}
	return source.SenderAlt
}
