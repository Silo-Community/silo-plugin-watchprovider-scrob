package provider

import (
	"context"
	"strconv"
	"strings"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Silo syncs the account's named lists with collections on the profile. Every
// Scrob list counts except the watchlist, which syncs as Silo's watchlist,
// and the plugin's old "Silo - Watchlist" list. The collection key is the
// Scrob list id, which stays the same when the list is renamed.

const (
	errCollectionNeedsMedia     = "Scrob lists sync movies and series only"
	errCollectionNeedsID        = "Scrob list sync requires a movie or series with a TMDB or TVDB id"
	errCollectionTitleNotFound  = "Scrob could not find this title to add to the list"
	errCollectionUnknownList    = "The Scrob list no longer exists"
	errCollectionInvalidListKey = "Unknown Scrob list key"
)

// collectionLists returns the lists that sync as collections.
func collectionLists(ctx context.Context, client *apiClient) ([]scrobList, *pluginv1.WatchSyncFault) {
	var payload scrobListsResponse
	if fault := client.get(ctx, "/lists", nil, &payload); fault != nil {
		return nil, fault
	}
	lists := make([]scrobList, 0, len(payload.Lists))
	for _, list := range payload.Lists {
		if list.ID <= 0 || list.IsWatchlist || list.Name == watchlistName {
			continue
		}
		lists = append(lists, list)
	}
	return lists, nil
}

func collectionKey(listID int) string {
	return strconv.Itoa(listID)
}

// listCollections reports every list and its whole movies and series as one
// complete snapshot, as the contract requires.
func (s *Server) listCollections(ctx context.Context, client *apiClient) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	lists, fault := collectionLists(ctx, client)
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	response := &pluginv1.WatchSyncListRemoteStateResponse{CompleteSnapshot: true}
	for _, list := range lists {
		key := collectionKey(list.ID)
		response.Collections = append(response.Collections, &pluginv1.WatchSyncRemoteCollection{
			CollectionKey: key,
			Name:          list.Name,
			Description:   list.Description,
		})
		items, fault := watchlistItems(ctx, client, list.ID)
		if fault != nil {
			return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
		}
		for _, item := range items {
			if !watchlistMedia(item.Media) {
				continue
			}
			itemKey, media, ok := stateMediaFromScrob(item.Media)
			if !ok {
				continue
			}
			listed := &pluginv1.WatchSyncRemoteCollectionItemState{CollectionKey: key}
			if item.AddedAt != nil && !item.AddedAt.Time().IsZero() {
				listed.ListedAt = timestamppb.New(item.AddedAt.Time())
			}
			response.Items = append(response.Items, &pluginv1.WatchSyncRemoteState{
				ProviderItemKey: itemKey,
				Media:           media,
				Collection:      listed,
			})
		}
	}
	return response, nil
}

// writeCollection adds titles to or removes them from the lists the events
// name. Both are convergent: a title already there, or already gone, is
// NO_CHANGE. An event for a list that no longer exists is rejected.
func writeCollection(ctx context.Context, client *apiClient, events []*pluginv1.WatchSyncEvent, results *resultSet, removing bool) *pluginv1.WatchSyncFault {
	byList := make(map[int][]*pluginv1.WatchSyncEvent)
	order := make([]int, 0)
	for _, event := range events {
		kind := mediaType(event.GetMedia().GetMediaType())
		ids := idsFromExternal(event.GetMedia().GetExternalIds())
		listID, err := strconv.Atoi(strings.TrimSpace(event.GetCollectionKey()))
		switch {
		case err != nil || listID <= 0:
			results.reject(event, errCollectionInvalidListKey)
		case kind != "movie" && kind != "series":
			results.reject(event, errCollectionNeedsMedia)
		case ids.TMDB <= 0 && ids.TVDB <= 0:
			results.reject(event, errCollectionNeedsID)
		default:
			if _, seen := byList[listID]; !seen {
				order = append(order, listID)
			}
			byList[listID] = append(byList[listID], event)
		}
	}
	if len(order) == 0 {
		return nil
	}
	lists, fault := collectionLists(ctx, client)
	if fault != nil {
		for _, listID := range order {
			if fault := failAll(byList[listID], results, fault); fault != nil {
				return fault
			}
		}
		return nil
	}
	known := make(map[int]bool, len(lists))
	for _, list := range lists {
		known[list.ID] = true
	}
	for _, listID := range order {
		pending := byList[listID]
		if !known[listID] {
			for _, event := range pending {
				results.fail(event, permanentFault(errCollectionUnknownList))
			}
			continue
		}
		var fault *pluginv1.WatchSyncFault
		if removing {
			fault = removeListItems(ctx, client, listID, pending, results)
		} else {
			fault = addListItems(ctx, client, listID, pending, results, errCollectionTitleNotFound)
		}
		if fault != nil {
			return fault
		}
	}
	return nil
}
