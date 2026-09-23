package wa

import (
	"sync"
	"time"
)

// historyDelivery is what a pending on-demand backfill is woken up with once
// the matching ON_DEMAND history sync has been persisted.
type historyDelivery struct {
	// Inserted is how many rows were actually new.
	Inserted int
	// Delivered is how many messages the reply carried, new or not. It is what
	// tells a caller whether asking again is likely to yield more.
	Delivered int
	// Oldest is the timestamp of the oldest message now stored for the chat.
	Oldest time.Time
}

// pendingHistory tracks the on-demand history requests that are waiting for a
// reply.
//
// An on-demand request and its answer are two unrelated events: the request
// leaves through SendPeerMessage and the answer arrives later on the event
// handler as an *events.HistorySync. This registry is the rendezvous between
// them, keyed by tenant and chat.
type pendingHistory struct {
	mu      sync.Mutex
	waiters map[string]chan historyDelivery
}

func newPendingHistory() *pendingHistory {
	return &pendingHistory{waiters: make(map[string]chan historyDelivery)}
}

// historyKey identifies one pending request. The tenant is part of the key
// because two tenants may legitimately be backfilling the same chat JID.
func historyKey(tenantID, chatJID string) string {
	// A NUL separator cannot appear in either half, so no pair of different
	// (tenant, chat) values can collide on one key.
	return tenantID + "\x00" + chatJID
}

// register claims the key and returns the channel its reply will arrive on,
// plus a release function the caller MUST defer.
//
// release is what keeps a timed-out request from leaking: whether the reply
// arrives, the deadline passes or the caller's context is cancelled, the entry
// is removed by the same call. It is safe to call more than once.
func (p *pendingHistory) register(key string) (<-chan historyDelivery, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, exists := p.waiters[key]; exists {
		return nil, nil, ErrSyncInProgress
	}

	// Buffered, so deliver never blocks on a caller that has already given up.
	ch := make(chan historyDelivery, 1)
	p.waiters[key] = ch

	var once sync.Once
	release := func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			// Only drop the entry if it is still ours: a later request for the
			// same chat must not be cancelled by an earlier one's release.
			if current, ok := p.waiters[key]; ok && current == ch {
				delete(p.waiters, key)
			}
		})
	}
	return ch, release, nil
}

// deliver hands a reply to the waiter for key and reports whether one was
// there. A reply nobody is waiting for is dropped: a history sync may well
// arrive because the phone decided to push one, not because we asked.
func (p *pendingHistory) deliver(key string, d historyDelivery) bool {
	p.mu.Lock()
	ch, ok := p.waiters[key]
	if ok {
		delete(p.waiters, key)
	}
	p.mu.Unlock()

	if !ok {
		return false
	}
	// The channel is buffered and used exactly once, so this never blocks.
	ch <- d
	return true
}

// waiterCount reports how many requests are in flight. It exists so the tests
// can assert that nothing is left behind.
func (p *pendingHistory) waiterCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.waiters)
}
