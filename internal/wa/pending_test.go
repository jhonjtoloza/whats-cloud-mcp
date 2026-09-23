package wa

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The pending registry is the only concurrency in the on-demand backfill path,
// and it is the part that can leak: a request that times out must take its own
// entry with it, or the map grows for every abandoned sync.
//
// It is pure Go with no socket in sight, so it is tested directly rather than
// through the session manager.

func TestPendingHistoryDelivers(t *testing.T) {
	pending := newPendingHistory()
	key := historyKey("tenant-a", "chat@s.whatsapp.net")

	ch, release, err := pending.register(key)
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	defer release()

	want := historyDelivery{Inserted: 7, Delivered: 50, Oldest: time.Unix(1700000000, 0).UTC()}
	if !pending.deliver(key, want) {
		t.Fatal("deliver() reported no waiter for a registered key")
	}

	select {
	case got := <-ch:
		if got != want {
			t.Errorf("delivered %+v, want %+v", got, want)
		}
	case <-time.After(time.Second):
		t.Fatal("the registered waiter was never woken up")
	}

	if n := pending.waiterCount(); n != 0 {
		t.Errorf("%d waiters left after delivery, want 0", n)
	}
}

// TestPendingHistoryReleaseCleansUp is the timeout path: when the caller gives
// up, nothing of it may stay behind.
func TestPendingHistoryReleaseCleansUp(t *testing.T) {
	pending := newPendingHistory()
	key := historyKey("tenant-a", "chat@s.whatsapp.net")

	if _, _, err := pending.register(key); err != nil {
		t.Fatalf("register() error = %v", err)
	}
	if n := pending.waiterCount(); n != 1 {
		t.Fatalf("%d waiters after register, want 1", n)
	}

	_, release, err := pending.register(historyKey("tenant-a", "other@s.whatsapp.net"))
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}
	release()

	if n := pending.waiterCount(); n != 1 {
		t.Errorf("%d waiters after releasing one of two, want 1", n)
	}

	// A late reply for an abandoned request must be dropped, not block and not
	// panic on a closed channel.
	if pending.deliver(historyKey("tenant-a", "other@s.whatsapp.net"), historyDelivery{Inserted: 1}) {
		t.Error("deliver() claimed to wake a waiter that had already given up")
	}
}

// TestPendingHistoryReleaseIsIdempotent guards the defer release() pattern:
// releasing twice, or releasing after a delivery, must stay harmless.
func TestPendingHistoryReleaseIsIdempotent(t *testing.T) {
	pending := newPendingHistory()
	key := historyKey("tenant-a", "chat@s.whatsapp.net")

	_, release, err := pending.register(key)
	if err != nil {
		t.Fatalf("register() error = %v", err)
	}

	pending.deliver(key, historyDelivery{Inserted: 3})
	release()
	release()

	if n := pending.waiterCount(); n != 0 {
		t.Errorf("%d waiters left, want 0", n)
	}
}

func TestPendingHistoryRejectsAConcurrentRequestForTheSameChat(t *testing.T) {
	pending := newPendingHistory()
	key := historyKey("tenant-a", "chat@s.whatsapp.net")

	_, release, err := pending.register(key)
	if err != nil {
		t.Fatalf("first register() error = %v", err)
	}
	defer release()

	if _, _, err := pending.register(key); !errors.Is(err, ErrSyncInProgress) {
		t.Errorf("second register() error = %v, want ErrSyncInProgress", err)
	}

	// The same chat of a DIFFERENT tenant is a different request.
	if _, otherRelease, err := pending.register(historyKey("tenant-b", "chat@s.whatsapp.net")); err != nil {
		t.Errorf("register() for another tenant error = %v, want success", err)
	} else {
		otherRelease()
	}
}

func TestPendingHistoryDeliverWithoutWaiter(t *testing.T) {
	pending := newPendingHistory()

	if pending.deliver(historyKey("tenant-a", "nobody@s.whatsapp.net"), historyDelivery{Inserted: 9}) {
		t.Error("deliver() claimed to wake a waiter that was never registered")
	}
	if n := pending.waiterCount(); n != 0 {
		t.Errorf("%d waiters after an unsolicited delivery, want 0", n)
	}
}

// TestPendingHistoryConcurrentChats runs many chats at once, which is the shape
// of several agents backfilling different conversations through one gateway.
func TestPendingHistoryConcurrentChats(t *testing.T) {
	const chats = 32

	pending := newPendingHistory()
	keys := make([]string, chats)
	for i := range keys {
		keys[i] = historyKey("tenant-a", "chat"+strconv.Itoa(i)+"@s.whatsapp.net")
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = make(map[string]int)
	)

	for i, key := range keys {
		ch, release, err := pending.register(key)
		if err != nil {
			t.Fatalf("register(%s) error = %v", key, err)
		}

		wg.Add(1)
		go func(key string, ch <-chan historyDelivery, release func()) {
			defer wg.Done()
			defer release()
			select {
			case got := <-ch:
				mu.Lock()
				seen[key] = got.Inserted
				mu.Unlock()
			case <-time.After(5 * time.Second):
			}
		}(key, ch, release)

		// Deliver from another goroutine, as handleEvent would.
		go func(key string, inserted int) {
			pending.deliver(key, historyDelivery{Inserted: inserted})
		}(key, i+1)
	}

	wg.Wait()

	for i, key := range keys {
		if seen[key] != i+1 {
			t.Errorf("chat %s received %d, want %d; a delivery went to the wrong waiter", key, seen[key], i+1)
		}
	}
	if n := pending.waiterCount(); n != 0 {
		t.Errorf("%d waiters left after every request finished, want 0", n)
	}
}
