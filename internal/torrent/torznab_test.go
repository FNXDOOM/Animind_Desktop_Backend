package torrent

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"animind-backend-go/internal/config"
)

const torznabDoc = `<?xml version="1.0" encoding="UTF-8"?>
<rss xmlns:torznab="http://torznab.com/schemas/2015/feed" version="2.0">
  <channel>
    <title>prowlarr</title>
    <item>
      <title>[SubsPlease] Sousou no Frieren - 01 (1080p) [10B0C3B4].mkv</title>
      <guid isPermaLink="false">https://idx/t/1</guid>
      <pubDate>Mon, 20 Oct 2026 10:00:00 +0000</pubDate>
      <size>1473283210</size>
      <link>https://idx/dl/1?k=abc</link>
      <enclosure url="magnet:?xt=urn:btih:ABCDEF0123456789ABCDEF0123456789ABCDEF01&amp;dn=x" length="1473283210" type="application/x-bittorrent"/>
      <torznab:attr name="seeders">412</torznab:attr>
      <torznab:attr name="peers">421</torznab:attr>
      <torznab:attr name="grabs">2300</torznab:attr>
      <torznab:attr name="infohash">abcdef0123456789abcdef0123456789abcdef01</torznab:attr>
      <torznab:attr name="indexer">prowlarr</torznab:attr>
      <torznab:attr name="category">5070</torznab:attr>
    </item>
  </channel>
</rss>`

func TestTorznabSearchRequestAndParse(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		io.WriteString(w, torznabDoc)
	}))
	defer srv.Close()
	idx := NewTorznabIndexer(config.TorrentConfig{URL: srv.URL, APIKey: "k"}, srv.Client())
	season, episode := 1, 5
	results, err := idx.Search(context.Background(), Query{
		Q: "Sousou no Frieren", Season: &season, Episode: &episode, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery.Get("t") != "tvsearch" || gotQuery.Get("season") != "1" || gotQuery.Get("ep") != "5" ||
		gotQuery.Get("apikey") != "k" || gotQuery.Get("cat") == "" {
		t.Fatalf("query form wrong: %v", gotQuery)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	r := results[0]
	if r.InfoHash != "abcdef0123456789abcdef0123456789abcdef01" {
		t.Fatalf("hash: %q", r.InfoHash)
	}
	if r.Seeders != 412 || r.Grabs != 2300 || r.SizeBytes != 1473283210 {
		t.Fatalf("attrs: %+v", r)
	}
	if r.Health != "excellent" || r.Category != "anime" || r.Indexer != "prowlarr" {
		t.Fatalf("derived: %+v", r)
	}
	if r.Parsed.Spec.Resolution == nil || *r.Parsed.Spec.Resolution != 1080 {
		t.Fatalf("parsed: %+v", r.Parsed)
	}
	if !strings.HasPrefix(r.Magnet, "magnet:?xt=urn:btih:") {
		t.Fatalf("magnet: %q", r.Magnet)
	}
}

func TestTorznabFailureModes(t *testing.T) {
	t.Run("500 status", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer srv.Close()
		idx := NewTorznabIndexer(config.TorrentConfig{URL: srv.URL, APIKey: "k"}, srv.Client())
		if _, err := idx.Search(context.Background(), Query{Q: "x"}); err == nil {
			t.Fatal("want an error for a 500 status")
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write(bytes.Repeat([]byte("<rss>"), 1<<20)) // 5 MiB, over the 4 MiB cap
		}))
		defer srv.Close()
		idx := NewTorznabIndexer(config.TorrentConfig{URL: srv.URL, APIKey: "k"}, srv.Client())
		_, err := idx.Search(context.Background(), Query{Q: "x"})
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("want a too-large error, got %v", err)
		}
	})

	t.Run("malformed xml", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "<rss><channel></rss>")
		}))
		defer srv.Close()
		idx := NewTorznabIndexer(config.TorrentConfig{URL: srv.URL, APIKey: "k"}, srv.Client())
		if _, err := idx.Search(context.Background(), Query{Q: "x"}); err == nil {
			t.Fatal("want an error for malformed xml")
		}
	})

	t.Run("missing attrs", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `<rss><channel><item><title>Solo title</title></item></channel></rss>`)
		}))
		defer srv.Close()
		idx := NewTorznabIndexer(config.TorrentConfig{URL: srv.URL, APIKey: "k"}, srv.Client())
		results, err := idx.Search(context.Background(), Query{Q: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 {
			t.Fatalf("want 1 result, got %d", len(results))
		}
		r := results[0]
		if r.Title != "Solo title" || r.InfoHash != "" || r.Magnet != "" || r.Trusted || r.Seeders != 0 {
			t.Fatalf("unexpected populated fields: %+v", r)
		}
		// Zero seeders still classify, they never panic.
		if r.Health != "dead" {
			t.Fatalf("health: %q", r.Health)
		}
	})

	t.Run("t=search default", func(t *testing.T) {
		var gotQuery url.Values
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.Query()
			io.WriteString(w, torznabDoc)
		}))
		defer srv.Close()
		idx := NewTorznabIndexer(config.TorrentConfig{URL: srv.URL, APIKey: "k"}, srv.Client())
		if _, err := idx.Search(context.Background(), Query{Q: "Frieren", Limit: 500}); err != nil {
			t.Fatal(err)
		}
		if gotQuery.Get("t") != "search" || gotQuery.Get("season") != "" || gotQuery.Get("ep") != "" {
			t.Fatalf("query form wrong: %v", gotQuery)
		}
		if gotQuery.Get("limit") != "100" {
			t.Fatalf("limit not capped at 100: %v", gotQuery)
		}
	})

	t.Run("timeout is set", func(t *testing.T) {
		idx := NewTorznabIndexer(config.TorrentConfig{URL: "http://idx", APIKey: "k"}, nil)
		if idx.client.Timeout != 10*time.Second {
			t.Fatalf("want the 10s default, got %v", idx.client.Timeout)
		}
		custom := &http.Client{Timeout: 3 * time.Second}
		idx = NewTorznabIndexer(config.TorrentConfig{URL: "http://idx", APIKey: "k"}, custom)
		if idx.client.Timeout != 3*time.Second {
			t.Fatalf("custom timeout clobbered: %v", idx.client.Timeout)
		}
	})
}
