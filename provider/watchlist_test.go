package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
)

// fakeLists is a Scrob server with lists, recording every write.
type fakeLists struct {
	lists  []scrobList
	items  map[int][]scrobListItem
	writes []string
	nextID int
	// addStatus, when set, answers every item add.
	addStatus int
	// watched holds Scrob's completed watches by movie TMDB id.
	watched map[string][]scrobItemEvent
	// native is the id of Scrob's native watchlist. Zero is a Scrob without
	// one, where /lists/watchlist fails validation as /lists/{list_id}.
	native int
}

func (f *fakeLists) serve(t *testing.T) (*apiClient, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/api/proxy")
		switch {
		case r.Method == http.MethodGet && path == "/history/item-events":
			events := f.watched[r.URL.Query().Get("tmdb_id")]
			_ = json.NewEncoder(w).Encode(scrobItemEventsResponse{Watched: len(events) > 0, Events: events})
		case r.Method == http.MethodGet && path == "/lists":
			_ = json.NewEncoder(w).Encode(scrobListsResponse{Lists: f.lists})
		case r.Method == http.MethodPost && path == "/lists":
			var in scrobListIn
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.nextID++
			f.lists = append(f.lists, scrobList{ID: f.nextID, Name: in.Name})
			f.writes = append(f.writes, "create "+in.Name)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(scrobList{ID: f.nextID, Name: in.Name})
		case r.Method == http.MethodGet && path == "/lists/watchlist":
			if f.native == 0 {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"detail":[]}`))
				return
			}
			_ = json.NewEncoder(w).Encode(scrobList{ID: f.native, Name: "Watchlist"})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/lists/"):
			id := atoi(strings.TrimPrefix(path, "/lists/"))
			_ = json.NewEncoder(w).Encode(scrobListDetail{Items: f.items[id]})
		case r.Method == http.MethodPost && strings.HasSuffix(path, "/items"):
			var in scrobListItemIn
			_ = json.NewDecoder(r.Body).Decode(&in)
			f.writes = append(f.writes, "add "+path+" "+in.MediaType)
			if f.addStatus != 0 {
				w.WriteHeader(f.addStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodDelete:
			f.writes = append(f.writes, "delete "+path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
		}
	}))
	client, err := newAPIClient(server.URL, "key", server.Client())
	if err != nil {
		server.Close()
		t.Fatalf("newAPIClient() error: %v", err)
	}
	return client, server.Close
}

func atoi(value string) int {
	n := 0
	for _, c := range value {
		n = n*10 + int(c-'0')
	}
	return n
}

func watchlistEvent(op pluginv1.WatchSyncOperation, kind pluginv1.WatchSyncMediaType, ids map[string]string) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:   "e1",
		Operation: op,
		Media:     &pluginv1.WatchSyncMedia{MediaType: kind, Title: "T", ExternalIds: ids},
	}
}

const (
	opAddToWatchlist      = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST
	opRemoveFromWatchlist = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_WATCHLIST
	movieType             = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE
	seriesType            = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
)

func writeOne(t *testing.T, client *apiClient, event *pluginv1.WatchSyncEvent, removing bool) *pluginv1.WatchSyncApplyResult {
	t.Helper()
	events := []*pluginv1.WatchSyncEvent{event}
	results := newResultSet(events)
	if fault := writeWatchlist(context.Background(), client, events, results, removing); fault != nil {
		t.Fatalf("writeWatchlist() fault: %v", fault)
	}
	return results.list()[0]
}

// Before the first export there is no list, and an empty complete snapshot
// would claim Silo's watchlist was emptied in Scrob. The traversal is an
// empty incremental one instead, and nothing is created by reading.
func TestListWatchlistWithoutTheListReportsNothing(t *testing.T) {
	f := &fakeLists{lists: []scrobList{{ID: 7, Name: "MDBList - Watchlist"}}}
	client, done := f.serve(t)
	defer done()

	resp, err := NewServer(nil).listWatchlist(context.Background(), client, false)
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("listWatchlist() = %v, %v", resp, err)
	}
	if resp.GetCompleteSnapshot() || len(resp.GetItems()) != 0 || len(f.writes) != 0 {
		t.Errorf("response = %+v, writes %v; want an empty incremental traversal and no writes", resp, f.writes)
	}
}

// The plugin's list is read as a complete snapshot of movies and whole
// series; a season, a person or another list's items are left out.
func TestListWatchlistReadsOnlyThePluginsList(t *testing.T) {
	season := 2
	f := &fakeLists{
		lists: []scrobList{{ID: 7, Name: "MDBList - Watchlist"}, {ID: 9, Name: watchlistName}},
		items: map[int][]scrobListItem{
			7: {{ID: 70, Media: scrobMedia{Type: "movie", TMDBID: 1}}},
			9: {
				{ID: 90, Media: scrobMedia{Type: "movie", TMDBID: 42, Title: "Heat"}, AddedAt: scrobTimePtr(timeUnix(100))},
				{ID: 91, Media: scrobMedia{Type: "series", TMDBID: 1399, TVDBID: 121361, Title: "GoT"}},
				{ID: 92, Media: scrobMedia{Type: "series", TMDBID: 1399, SeasonNumber: &season}},
				{ID: 93, Media: scrobMedia{Type: "person", TMDBID: 5}},
			},
		},
	}
	client, done := f.serve(t)
	defer done()

	resp, err := NewServer(nil).listWatchlist(context.Background(), client, false)
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("listWatchlist() = %v, %v", resp, err)
	}
	if !resp.GetCompleteSnapshot() {
		t.Errorf("CompleteSnapshot = false, want true once the list exists")
	}
	items := resp.GetItems()
	if len(items) != 2 {
		t.Fatalf("items = %d, want the movie and the whole series", len(items))
	}
	if items[0].GetWatchlist().GetListedAt().AsTime() != timeUnix(100) || items[1].GetMedia().GetMediaType() != seriesType {
		t.Errorf("items = %+v", items)
	}
}

// The first add creates the plugin's list; another list with a similar
// purpose, such as MDBList's, is left alone.
func TestWriteWatchlistCreatesTheListOnTheFirstAdd(t *testing.T) {
	f := &fakeLists{lists: []scrobList{{ID: 7, Name: "MDBList - Watchlist"}}, nextID: 20}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opAddToWatchlist, movieType, map[string]string{"tmdb": "42"}), false)
	want := []string{"create " + watchlistName, "add /lists/21/items movie"}
	if strings.Join(f.writes, ",") != strings.Join(want, ",") {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("status = %v, want APPLIED", r.GetStatus())
	}
}

// Scrob answers 409 for an item already on the list.
func TestWriteWatchlistTreatsAnItemAlreadyListedAsNoChange(t *testing.T) {
	f := &fakeLists{lists: []scrobList{{ID: 9, Name: watchlistName}}, addStatus: http.StatusConflict}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opAddToWatchlist, seriesType, map[string]string{"tvdb": "121361"}), false)
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Errorf("status = %v, want NO_CHANGE", r.GetStatus())
	}
}

// A removal deletes only the matching row of the plugin's list.
func TestWriteWatchlistRemovesTheMatchingItem(t *testing.T) {
	f := &fakeLists{
		lists: []scrobList{{ID: 9, Name: watchlistName}},
		items: map[int][]scrobListItem{9: {
			{ID: 90, Media: scrobMedia{Type: "movie", TMDBID: 42}},
			{ID: 91, Media: scrobMedia{Type: "series", TMDBID: 42}},
		}},
	}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opRemoveFromWatchlist, movieType, map[string]string{"tmdb": "42"}), true)
	if want := []string{"delete /lists/9/items/90"}; strings.Join(f.writes, ",") != strings.Join(want, ",") {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("status = %v, want APPLIED", r.GetStatus())
	}
}

// Removing from a list that does not exist yet changes nothing and creates
// nothing.
func TestWriteWatchlistRemovalWithoutTheListIsNoChange(t *testing.T) {
	f := &fakeLists{}
	client, done := f.serve(t)
	defer done()

	r := writeOne(t, client, watchlistEvent(opRemoveFromWatchlist, movieType, map[string]string{"tmdb": "42"}), true)
	if r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE || len(f.writes) != 0 {
		t.Errorf("status = %v, writes %v; want NO_CHANGE and no writes", r.GetStatus(), f.writes)
	}
}

// Episodes have no place on the list, and a title needs an id Scrob can use.
func TestWriteWatchlistRejectsWhatItCannotList(t *testing.T) {
	f := &fakeLists{}
	client, done := f.serve(t)
	defer done()

	for _, event := range []*pluginv1.WatchSyncEvent{
		watchlistEvent(opAddToWatchlist, pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE, map[string]string{"tmdb": "1"}),
		watchlistEvent(opAddToWatchlist, movieType, map[string]string{"imdb": "tt0113277"}),
	} {
		if r := writeOne(t, client, event, false); r.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
			t.Errorf("status = %v, want REJECTED", r.GetStatus())
		}
	}
	if len(f.writes) != 0 {
		t.Errorf("writes = %v, want none", f.writes)
	}
}

func timeUnix(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func watchedAt(sec int64) []scrobItemEvent {
	at := scrobTime(timeUnix(sec))
	return []scrobItemEvent{{ID: 1, WatchedAt: &at}}
}

// listedMovies is the plugin's list holding movie 42, listed at 100.
func listedMovies(watched map[string][]scrobItemEvent) *fakeLists {
	return &fakeLists{
		lists: []scrobList{{ID: 9, Name: watchlistName}},
		items: map[int][]scrobListItem{9: {
			{ID: 90, AddedAt: scrobTimePtr(timeUnix(100)), Media: scrobMedia{Type: "movie", TMDBID: 42}},
			{ID: 91, AddedAt: scrobTimePtr(timeUnix(100)), Media: scrobMedia{Type: "series", TMDBID: 1399}},
		}},
		watched: watched,
	}
}

// A movie watched after it was listed leaves the list and the snapshot, so
// Silo does not put it back on its watchlist. Series stay.
func TestListWatchlistRemovesAMovieWatchedSinceListing(t *testing.T) {
	f := listedMovies(map[string][]scrobItemEvent{"42": watchedAt(200), "1399": watchedAt(200)})
	client, done := f.serve(t)
	defer done()

	resp, err := NewServer(nil).listWatchlist(context.Background(), client, true)
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("listWatchlist() = %v, %v", resp, err)
	}
	if want := []string{"delete /lists/9/items/90"}; strings.Join(f.writes, ",") != strings.Join(want, ",") {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
	if len(resp.GetItems()) != 1 || resp.GetItems()[0].GetMedia().GetMediaType() != seriesType {
		t.Errorf("items = %+v, want only the series", resp.GetItems())
	}
}

// A movie listed again after a watch, to rewatch it, stays.
func TestListWatchlistKeepsAMovieWatchedBeforeListing(t *testing.T) {
	f := listedMovies(map[string][]scrobItemEvent{"42": watchedAt(50)})
	client, done := f.serve(t)
	defer done()

	resp, _ := NewServer(nil).listWatchlist(context.Background(), client, true)
	if len(f.writes) != 0 || len(resp.GetItems()) != 2 {
		t.Errorf("writes = %v, items = %d; want no removal and both items", f.writes, len(resp.GetItems()))
	}
}

// With the setting off, a watched movie stays listed.
func TestListWatchlistKeepsWatchedMoviesWhenTheSettingIsOff(t *testing.T) {
	f := listedMovies(map[string][]scrobItemEvent{"42": watchedAt(200)})
	client, done := f.serve(t)
	defer done()

	resp, _ := NewServer(nil).listWatchlist(context.Background(), client, false)
	if len(f.writes) != 0 || len(resp.GetItems()) != 2 {
		t.Errorf("writes = %v, items = %d; want no removal and both items", f.writes, len(resp.GetItems()))
	}
}

// The setting is on unless the connection turned it off, including on a host
// that sends no connection settings at all.
func TestConnectionSettingDefaultsWhenAbsent(t *testing.T) {
	auth := &pluginv1.WatchSyncAuthenticatedContext{}
	if !connectionSetting(auth, settingRemoveWatchedMovies, true) {
		t.Errorf("absent setting = false, want the default true")
	}
	auth.ConnectionSettings = map[string]string{settingRemoveWatchedMovies: "false"}
	if connectionSetting(auth, settingRemoveWatchedMovies, true) {
		t.Errorf("setting turned off = true, want false")
	}
}

// On a Scrob with a native watchlist, Silo's watchlist is that list: it is
// read as a complete snapshot even before any export, and adds go to it
// without creating a list of the plugin's own.
func TestNativeWatchlistIsUsedWhenScrobHasOne(t *testing.T) {
	f := &fakeLists{
		native: 3,
		lists:  []scrobList{{ID: 3, Name: "Watchlist"}, {ID: 7, Name: "MDBList - Watchlist (old)"}},
		items:  map[int][]scrobListItem{3: {{ID: 30, Media: scrobMedia{Type: "movie", TMDBID: 42}}}},
	}
	client, done := f.serve(t)
	defer done()

	resp, err := NewServer(nil).listWatchlist(context.Background(), client, false)
	if err != nil || resp.GetFault() != nil {
		t.Fatalf("listWatchlist() = %v, %v", resp, err)
	}
	if !resp.GetCompleteSnapshot() || len(resp.GetItems()) != 1 {
		t.Errorf("response = %+v, want the native watchlist as a complete snapshot", resp)
	}

	result := writeOne(t, client, watchlistEvent(opAddToWatchlist, movieType, map[string]string{"tmdb": "550"}), false)
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Errorf("status = %v, want APPLIED", result.GetStatus())
	}
	if len(f.writes) != 1 || f.writes[0] != "add /lists/3/items movie" {
		t.Errorf("writes = %v, want one add to the native watchlist and no list created", f.writes)
	}
}

// The plugin's old list is moved into the native watchlist and deleted.
func TestNativeWatchlistAdoptsTheOldPluginList(t *testing.T) {
	f := &fakeLists{
		native: 3,
		lists:  []scrobList{{ID: 3, Name: "Watchlist"}, {ID: 9, Name: watchlistName}},
		items: map[int][]scrobListItem{
			9: {
				{ID: 90, Media: scrobMedia{Type: "movie", TMDBID: 42}},
				{ID: 91, Media: scrobMedia{Type: "series", TVDBID: 121361}},
			},
		},
	}
	client, done := f.serve(t)
	defer done()

	id, fault := findWatchlist(context.Background(), client)
	if fault != nil || id != 3 {
		t.Fatalf("findWatchlist() = %d, %v; want the native watchlist", id, fault)
	}
	want := []string{"add /lists/3/items movie", "add /lists/3/items series", "delete /lists/9"}
	if strings.Join(f.writes, "; ") != strings.Join(want, "; ") {
		t.Errorf("writes = %v, want %v", f.writes, want)
	}
}

// A title that's already on the native watchlist doesn't stop the move.
func TestNativeWatchlistAdoptionTreatsAConflictAsMoved(t *testing.T) {
	f := &fakeLists{
		native:    3,
		addStatus: http.StatusConflict,
		lists:     []scrobList{{ID: 3, Name: "Watchlist"}, {ID: 9, Name: watchlistName}},
		items:     map[int][]scrobListItem{9: {{ID: 90, Media: scrobMedia{Type: "movie", TMDBID: 42}}}},
	}
	client, done := f.serve(t)
	defer done()

	if _, fault := findWatchlist(context.Background(), client); fault != nil {
		t.Fatalf("findWatchlist() fault: %v", fault)
	}
	if f.writes[len(f.writes)-1] != "delete /lists/9" {
		t.Errorf("writes = %v, want the old list deleted", f.writes)
	}
}

// A failed move, or a list holding something the plugin never adds, keeps
// the old list so nothing is lost; the move is retried on the next sync.
func TestNativeWatchlistAdoptionKeepsTheOldListWhenItCannotMoveEverything(t *testing.T) {
	season := 1
	cases := map[string]*fakeLists{
		"failed add": {
			native: 3, addStatus: http.StatusInternalServerError,
			lists: []scrobList{{ID: 3, Name: "Watchlist"}, {ID: 9, Name: watchlistName}},
			items: map[int][]scrobListItem{9: {{ID: 90, Media: scrobMedia{Type: "movie", TMDBID: 42}}}},
		},
		"season on the list": {
			native: 3,
			lists:  []scrobList{{ID: 3, Name: "Watchlist"}, {ID: 9, Name: watchlistName}},
			items:  map[int][]scrobListItem{9: {{ID: 90, Media: scrobMedia{Type: "series", TMDBID: 1399, SeasonNumber: &season}}}},
		},
	}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			client, done := f.serve(t)
			defer done()

			id, fault := findWatchlist(context.Background(), client)
			if fault != nil || id != 3 {
				t.Fatalf("findWatchlist() = %d, %v; want the native watchlist", id, fault)
			}
			for _, write := range f.writes {
				if write == "delete /lists/9" {
					t.Errorf("writes = %v, want the old list kept", f.writes)
				}
			}
		})
	}
}

// A Scrob without a native watchlist keeps the plugin's own list.
func TestOlderScrobKeepsThePluginList(t *testing.T) {
	f := &fakeLists{lists: []scrobList{{ID: 9, Name: watchlistName}}}
	client, done := f.serve(t)
	defer done()

	id, fault := findWatchlist(context.Background(), client)
	if fault != nil || id != 9 || len(f.writes) != 0 {
		t.Errorf("findWatchlist() = %d, %v, writes %v; want the plugin's list untouched", id, fault, f.writes)
	}
}
