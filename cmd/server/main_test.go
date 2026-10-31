package main

import (
	"testing"

	"animind-backend-go/internal/config"
	"animind-backend-go/internal/httpx"
	"animind-backend-go/internal/torrent"
)

// TestTorrentWiringKeepsTypedNilOutOfTheSeam reproduces the production shape of
// a deployment without TORZNAB_URL end to end: buildTorrentService answers nil,
// and that nil must never be stored in Deps.Torrent. Any *torrent.Service —
// including a nil one — inside the Torrent interface is non-nil, so an
// unconditional assignment would defeat the handler's 501
// indexer_not_configured ruling and make /api/torrents/search panic instead.
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
