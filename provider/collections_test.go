package provider

import (
	"context"
	"net/http"
	"slices"
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// Every list except the watchlist and the plugin's old list syncs, empty ones
// included, with its whole movies and series keyed by the list id.
func TestListCollectionsReportsEveryOtherList(t *testing.T) {
	season := 1
	fake := &fakeLists{
		lists: []scrobList{
			{ID: 1, Name: "Watchlist", IsWatchlist: true},
			{ID: 2, Name: watchlistName},
			{ID: 3, Name: "Heists", Description: "Crime"},
			{ID: 4, Name: "Empty"},
		},
		items: map[int][]scrobListItem{
			1: {{ID: 10, Media: scrobMedia{Type: "movie", TMDBID: 1}}},
			2: {{ID: 20, Media: scrobMedia{Type: "movie", TMDBID: 2}}},
			3: {
				{ID: 30, Media: scrobMedia{Type: "movie", TMDBID: 42, Title: "Heat"}, AddedAt: scrobTimePtr(timeUnix(100))},
				{ID: 31, Media: scrobMedia{Type: "series", TMDBID: 1399, SeasonNumber: &season}},
			},
		},
	}
	client, closeServer := fake.serve(t)
	defer closeServer()

	response, err := NewServer(nil).listCollections(context.Background(), client)
	if err != nil || response.GetFault() != nil || !response.GetCompleteSnapshot() {
		t.Fatalf("response = %#v, err = %v", response, err)
	}
	var keys []string
	for _, collection := range response.GetCollections() {
		keys = append(keys, collection.GetCollectionKey()+"="+collection.GetName())
	}
	if !slices.Equal(keys, []string{"3=Heists", "4=Empty"}) {
		t.Fatalf("collections = %v", keys)
	}
	items := response.GetItems()
	if len(items) != 1 || items[0].GetCollection().GetCollectionKey() != "3" ||
		items[0].GetMedia().GetExternalIds()["tmdb"] != "42" || items[0].GetCollection().GetListedAt() == nil {
		t.Fatalf("items = %#v", items)
	}
}

func TestWriteCollectionAddsAndRemovesOnTheNamedList(t *testing.T) {
	fake := &fakeLists{
		lists: []scrobList{{ID: 3, Name: "Heists"}},
		items: map[int][]scrobListItem{3: {{ID: 30, Media: scrobMedia{Type: "movie", TMDBID: 42}}}},
	}
	client, closeServer := fake.serve(t)
	defer closeServer()
	event := func(op pluginv1.WatchSyncOperation, key, tmdb string) *pluginv1.WatchSyncEvent {
		e := watchlistEvent(op, movieType, map[string]string{"tmdb": tmdb})
		e.CollectionKey = key
		return e
	}
	add := pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_COLLECTION
	remove := pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_COLLECTION
	for _, tc := range []struct {
		event      *pluginv1.WatchSyncEvent
		removing   bool
		wantStatus pluginv1.WatchSyncApplyStatus
		wantCode   pluginv1.WatchSyncFaultCode
		wantWrite  string
	}{
		{event: event(add, "3", "550"), wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED, wantWrite: "add /lists/3/items movie"},
		{event: event(remove, "3", "42"), removing: true, wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED, wantWrite: "delete /lists/3/items/30"},
		{event: event(remove, "3", "7"), removing: true, wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE},
		{event: event(add, "9", "550"), wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT},
		{event: event(add, "watchlist", "550"), wantStatus: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED, wantCode: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
	} {
		fake.writes = nil
		results := newResultSet([]*pluginv1.WatchSyncEvent{tc.event})
		if fault := writeCollection(context.Background(), client, []*pluginv1.WatchSyncEvent{tc.event}, results, tc.removing); fault != nil {
			t.Fatalf("fault = %#v", fault)
		}
		result := results.list()[0]
		if result.GetStatus() != tc.wantStatus || result.GetFault().GetCode() != tc.wantCode {
			t.Fatalf("%s %s: result = %#v", tc.event.GetOperation(), tc.event.GetCollectionKey(), result)
		}
		if tc.wantWrite != "" && !slices.Contains(fake.writes, tc.wantWrite) {
			t.Fatalf("writes = %v, want %q", fake.writes, tc.wantWrite)
		}
	}
}

// Scrob answers 409 both for a title already on a list and for a list it
// won't let anyone edit, such as a copy of an MDBList list it mirrors. A
// title already there is NO_CHANGE; one that isn't is rejected as read-only,
// and so is a removal Scrob refuses.
func TestWriteCollectionRejectsEditsToAReadOnlyList(t *testing.T) {
	fake := &fakeLists{
		lists:        []scrobList{{ID: 3, Name: "MDBList copy"}},
		items:        map[int][]scrobListItem{3: {{ID: 30, Media: scrobMedia{Type: "movie", TMDBID: 42}}}},
		addStatus:    http.StatusConflict,
		deleteStatus: http.StatusConflict,
	}
	client, closeServer := fake.serve(t)
	defer closeServer()
	event := func(op pluginv1.WatchSyncOperation, id, tmdb string) *pluginv1.WatchSyncEvent {
		e := watchlistEvent(op, movieType, map[string]string{"tmdb": tmdb})
		e.EventId = id
		e.CollectionKey = "3"
		return e
	}
	addListed := event(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_COLLECTION, "listed", "42")
	addNew := event(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_COLLECTION, "new", "7")
	adds := []*pluginv1.WatchSyncEvent{addListed, addNew}
	results := newResultSet(adds)
	if fault := writeCollection(context.Background(), client, adds, results, false); fault != nil {
		t.Fatalf("add fault: %v", fault)
	}
	got := results.list()
	if got[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Errorf("listed title = %v, want NO_CHANGE", got[0].GetStatus())
	}
	if got[1].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("new title on a read-only list = %v, want REJECTED", got[1].GetStatus())
	}

	remove := []*pluginv1.WatchSyncEvent{event(pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_COLLECTION, "rm", "42")}
	results = newResultSet(remove)
	if fault := writeCollection(context.Background(), client, remove, results, true); fault != nil {
		t.Fatalf("remove fault: %v", fault)
	}
	if status := results.list()[0].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Errorf("removal from a read-only list = %v, want REJECTED", status)
	}
}
