package wa

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/jhonjtoloza/whats-cloud-mcp/internal/store"
)

// fakeChats is an in-memory store.Chats. The group name sync is the part of
// the manager that CAN be tested without a socket, and this is what keeps it
// that way: everything that talks to WhatsApp goes through groupLister.
type fakeChats struct {
	mu      sync.Mutex
	names   map[string]string // keyed by tenant id + "/" + chat jid
	batches int
}

func newFakeChats() *fakeChats {
	return &fakeChats{names: make(map[string]string)}
}

func (f *fakeChats) Upsert(_ context.Context, tenantID string, chat store.NamedChat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.names[tenantID+"/"+chat.ChatJID] = chat.Name
	return nil
}

func (f *fakeChats) UpsertBatch(_ context.Context, tenantID string, chats []store.NamedChat) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	for _, chat := range chats {
		f.names[tenantID+"/"+chat.ChatJID] = chat.Name
	}
	return nil
}

func (f *fakeChats) Search(context.Context, string, string, int) ([]store.NamedChat, error) {
	return nil, nil
}

func (f *fakeChats) name(tenantID, chatJID string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, ok := f.names[tenantID+"/"+chatJID]
	return name, ok
}

func (f *fakeChats) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.names)
}

// stubGroupLister stands in for the whatsmeow client.
type stubGroupLister struct {
	groups []*types.GroupInfo
	err    error
	calls  int
}

func (s *stubGroupLister) GetJoinedGroups(context.Context) ([]*types.GroupInfo, error) {
	s.calls++
	return s.groups, s.err
}

func newTestManager(chats store.Chats) *Manager {
	return &Manager{chats: chats, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func groupInfo(jid, name string) *types.GroupInfo {
	return &types.GroupInfo{
		JID:       types.JID{User: jid, Server: types.GroupServer},
		GroupName: types.GroupName{Name: name},
	}
}

func TestSyncGroupNamesStoresEveryJoinedGroup(t *testing.T) {
	chats := newFakeChats()
	manager := newTestManager(chats)
	lister := &stubGroupLister{groups: []*types.GroupInfo{
		groupInfo("120363424550223300", "obd2ip"),
		groupInfo("120363406613747745", "Weekly planning"),
	}}

	if err := manager.syncGroupNames(context.Background(), "tenant-a", lister); err != nil {
		t.Fatalf("syncGroupNames() error = %v", err)
	}

	if chats.batches != 1 {
		t.Errorf("syncGroupNames() wrote %d batches, want 1", chats.batches)
	}
	for jid, want := range map[string]string{
		"120363424550223300@g.us": "obd2ip",
		"120363406613747745@g.us": "Weekly planning",
	} {
		got, ok := chats.name("tenant-a", jid)
		if !ok {
			t.Errorf("syncGroupNames() stored no name for %s", jid)
			continue
		}
		if got != want {
			t.Errorf("syncGroupNames() name of %s = %q, want %q", jid, got, want)
		}
	}
}

// TestSyncGroupNamesReportsAFailedLookup keeps a failed round trip from being
// read as "this tenant is in no groups", which would leave every name stale
// with nothing in the log to say why.
func TestSyncGroupNamesReportsAFailedLookup(t *testing.T) {
	chats := newFakeChats()
	manager := newTestManager(chats)
	wantErr := errors.New("socket is gone")
	lister := &stubGroupLister{err: wantErr}

	err := manager.syncGroupNames(context.Background(), "tenant-a", lister)
	if !errors.Is(err, wantErr) {
		t.Fatalf("syncGroupNames() error = %v, want %v", err, wantErr)
	}
	if chats.count() != 0 {
		t.Errorf("syncGroupNames() wrote %d names after a failed lookup, want 0", chats.count())
	}
}

// TestGroupRenameEventReplacesTheStoredName is why the sync is not only run at
// connect time: a name that silently goes stale is worse than no name.
func TestGroupRenameEventReplacesTheStoredName(t *testing.T) {
	chats := newFakeChats()
	manager := newTestManager(chats)
	jid := types.JID{User: "120363424550223300", Server: types.GroupServer}

	manager.handleEvent("tenant-a", &events.GroupInfo{
		JID:  jid,
		Name: &types.GroupName{Name: "obd2ip colombia"},
	})

	got, ok := chats.name("tenant-a", jid.String())
	if !ok {
		t.Fatalf("a rename stored no name for %s", jid)
	}
	if got != "obd2ip colombia" {
		t.Errorf("stored name = %q, want %q", got, "obd2ip colombia")
	}
}

// TestGroupInfoEventWithoutARenameStoresNothing: the same event carries topic,
// membership and lock changes, and none of those touch the name.
func TestGroupInfoEventWithoutARenameStoresNothing(t *testing.T) {
	chats := newFakeChats()
	manager := newTestManager(chats)

	manager.handleEvent("tenant-a", &events.GroupInfo{
		JID:   types.JID{User: "120363424550223300", Server: types.GroupServer},
		Topic: &types.GroupTopic{Topic: "a new topic"},
	})

	if chats.count() != 0 {
		t.Errorf("a topic change wrote %d names, want 0", chats.count())
	}
}

func TestJoiningAGroupStoresItsName(t *testing.T) {
	chats := newFakeChats()
	manager := newTestManager(chats)
	jid := types.JID{User: "120363424550223300", Server: types.GroupServer}

	manager.handleEvent("tenant-a", &events.JoinedGroup{
		GroupInfo: types.GroupInfo{JID: jid, GroupName: types.GroupName{Name: "obd2ip"}},
	})

	got, ok := chats.name("tenant-a", jid.String())
	if !ok {
		t.Fatalf("joining a group stored no name for %s", jid)
	}
	if got != "obd2ip" {
		t.Errorf("stored name = %q, want %q", got, "obd2ip")
	}
}

// TestNamedChatsMergeIntoContactCandidates is the lookup the table exists for:
// a group found by name must come back as a candidate, without displacing the
// contacts the address book already matched.
func TestNamedChatsMergeIntoContactCandidates(t *testing.T) {
	found := map[string]Contact{
		"573004725680@s.whatsapp.net": {JID: "573004725680@s.whatsapp.net", Name: "Ana"},
		"120363406613747745@g.us":     {JID: "120363406613747745@g.us", Name: "120363406613747745"},
	}

	mergeNamedChats(found, []store.NamedChat{
		{ChatJID: "120363424550223300@g.us", Name: "obd2ip"},
		{ChatJID: "120363406613747745@g.us", Name: "Weekly planning"},
	})

	tests := []struct {
		jid  string
		want string
	}{
		{jid: "573004725680@s.whatsapp.net", want: "Ana"},
		// A name is strictly better than the numeric fallback, so it replaces it.
		{jid: "120363406613747745@g.us", want: "Weekly planning"},
		{jid: "120363424550223300@g.us", want: "obd2ip"},
	}
	for _, tc := range tests {
		got, ok := found[tc.jid]
		if !ok {
			t.Errorf("mergeNamedChats() dropped %s", tc.jid)
			continue
		}
		if got.Name != tc.want {
			t.Errorf("mergeNamedChats() name of %s = %q, want %q", tc.jid, got.Name, tc.want)
		}
	}
}

// TestMergeNamedChatsKeepsAContactName: the address book is the better source
// for a person, so a chat name must never overwrite a real contact name.
func TestMergeNamedChatsKeepsAContactName(t *testing.T) {
	found := map[string]Contact{
		"573004725680@s.whatsapp.net": {JID: "573004725680@s.whatsapp.net", Name: "Ana Pérez"},
	}

	mergeNamedChats(found, []store.NamedChat{
		{ChatJID: "573004725680@s.whatsapp.net", Name: "stale cached name"},
	})

	if got := found["573004725680@s.whatsapp.net"].Name; got != "Ana Pérez" {
		t.Errorf("mergeNamedChats() name = %q, want the address book name", got)
	}
}
