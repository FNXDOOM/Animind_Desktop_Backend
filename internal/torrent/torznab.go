package torrent

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"animind-backend-go/internal/scanner"
)

// The structs stay minimal on purpose: encoding/xml skips unknown elements,
// and unqualified tags match by local name, so the torznab: namespace on
// <attr> needs no special handling.
type torznabFeed struct {
	Channel struct {
		Items []torznabItem `xml:"item"`
	} `xml:"channel"`
}

type torznabItem struct {
	Title     string `xml:"title"`
	GUID      string `xml:"guid"`
	Link      string `xml:"link"`
	Enclosure struct {
		URL    string `xml:"url,attr"`
		Length int64  `xml:"length,attr"`
		Type   string `xml:"type,attr"`
	} `xml:"enclosure"`
	PubDate string        `xml:"pubDate"`
	Size    int64         `xml:"size"`
	Attrs   []torznabAttr `xml:"attr"`
}

// torznabAttr reads the value from the standard `value` attribute or, as the
// canned fixture and some backends emit, from the element text.
type torznabAttr struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
	Chars string `xml:",chardata"`
}

func (a torznabAttr) text() string {
	if a.Value != "" {
		return a.Value
	}
	return strings.TrimSpace(a.Chars)
}

// parseTorznabFeed decodes and normalizes a Torznab RSS document.
func parseTorznabFeed(body []byte) ([]Result, error) {
	var feed torznabFeed
	if err := xml.NewDecoder(bytes.NewReader(body)).Decode(&feed); err != nil {
		return nil, fmt.Errorf("torrent: decode feed: %w", err)
	}
	results := make([]Result, 0, len(feed.Channel.Items))
	for _, item := range feed.Channel.Items {
		results = append(results, item.result())
	}
	return results, nil
}

// result normalizes one feed item; missing attrs leave the matching fields zero.
func (it torznabItem) result() Result {
	attrs := make(map[string]string, len(it.Attrs))
	for _, a := range it.Attrs {
		attrs[a.Name] = a.text()
	}
	magnet := pickMagnet(it)
	seeders := atoi(attrs["seeders"])
	r := Result{
		ID:          it.GUID,
		Title:       it.Title,
		SizeBytes:   it.Size,
		Seeders:     seeders,
		Leechers:    leechers(attrs, seeders),
		Grabs:       atoi(attrs["grabs"]),
		PublishedAt: parsePubDate(it.PubDate),
		Indexer:     attrs["indexer"],
		Category:    classifyCategory(attrs["category"]),
		Health:      healthFor(seeders),
		Trusted:     isTruthy(attrs["trusted"]),
		Magnet:      magnet,
	}
	if r.SizeBytes == 0 {
		r.SizeBytes = it.Enclosure.Length
	}
	r.InfoHash = infoHash(attrs["infohash"], magnet)
	r.Parsed = scanner.ParseTorrentTitle(it.Title)
	return r
}

// pickMagnet prefers enclosure, then link, then guid; only a magnet carrying a
// urn:btih: info hash counts.
func pickMagnet(it torznabItem) string {
	for _, candidate := range []string{it.Enclosure.URL, it.Link, it.GUID} {
		if isBtihMagnet(candidate) {
			return candidate
		}
	}
	return ""
}

func isBtihMagnet(raw string) bool {
	low := strings.ToLower(raw)
	return strings.HasPrefix(low, "magnet:?") && strings.Contains(low, "urn:btih:")
}

// infoHash prefers the infohash attr and falls back to the magnet's btih hash;
// both are lowercased hex.
func infoHash(attr, magnet string) string {
	if h := strings.ToLower(strings.TrimSpace(attr)); h != "" {
		return h
	}
	u, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	for _, xt := range u.Query()["xt"] {
		if h, ok := strings.CutPrefix(strings.ToLower(xt), "urn:btih:"); ok {
			return h
		}
	}
	return ""
}

// classifyCategory maps the Torznab 5000 block (5000-5099, anime and its
// subcategories) to "anime"; everything else, including no category, is "other".
func classifyCategory(cat string) string {
	if n, err := strconv.Atoi(strings.TrimSpace(cat)); err == nil && n >= 5000 && n < 5100 {
		return "anime"
	}
	return "other"
}

// healthFor buckets seeders: >=20 excellent, 5-19 good, 1-4 poor, 0 dead.
func healthFor(seeders int) string {
	switch {
	case seeders >= 20:
		return "excellent"
	case seeders >= 5:
		return "good"
	case seeders >= 1:
		return "poor"
	default:
		return "dead"
	}
}

// leechers prefers an explicit leechers attr; Torznab's peers attr counts all
// peers, so leechers fall out as peers minus seeders.
func leechers(attrs map[string]string, seeders int) int {
	if v := attrs["leechers"]; v != "" {
		return atoi(v)
	}
	if n := atoi(attrs["peers"]) - seeders; n > 0 {
		return n
	}
	return 0
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true":
		return true
	}
	return false
}

// parsePubDate reads the common RSS date layouts; anything else leaves the
// field zero rather than failing the whole feed.
func parsePubDate(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}
