package main

import (
	"testing"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/httpx"
	"animind-backend-go/internal/torrent"
)

// TestTorrentWiringKeepsTypedNilOutOfTheSeam covers a deployment without TORZNAB_URL: the nil
// service must never reach Deps.Torrent, or search panics instead of answering 501.
func TestTorrentWiringKeepsTypedNilOutOfTheSeam(t *testing.T) {
	cfg := &config.Config{} // TORZNAB_URL empty
	if got := buildTorrentService(cfg, nil, nil, nil); got != nil {
		t.Fatalf("buildTorrentService constructed a service without TORZNAB_URL: %v", got)
	}

	var deps httpx.Deps
	wireTorrent(&deps, nil) // the value production holds when the indexer is absent
	if deps.Torrent != nil {
		t.Fatal("a typed-nil *torrent.Service reached Deps.Torrent; /api/torrents/search would panic instead of answering 501 indexer_not_configured")
	}

	wireTorrent(&deps, &torrent.Service{})
	if deps.Torrent == nil {
		t.Fatal("a constructed service must reach Deps.Torrent")
	}
}
