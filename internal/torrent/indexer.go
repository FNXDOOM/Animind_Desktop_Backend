package torrent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"animind-backend-go/internal/config"
)

const (
	defaultLimit = 50
	maxLimit     = 100
	// The reader takes one byte past the cap so the overflow is detectable.
	maxResponseBytes = 4 << 20
	requestTimeout   = 10 * time.Second
)

// TorznabIndexer implements Indexer against a Torznab endpoint.
type TorznabIndexer struct {
	base   string
	apiKey string
	client *http.Client
}

// NewTorznabIndexer builds the client; a caller-supplied client without a
// timeout gets the 10s request deadline.
func NewTorznabIndexer(cfg config.TorrentConfig, client *http.Client) *TorznabIndexer {
	if client == nil {
		client = &http.Client{}
	}
	c := *client
	if c.Timeout == 0 {
		c.Timeout = requestTimeout
	}
	return &TorznabIndexer{base: cfg.URL, apiKey: cfg.APIKey, client: &c}
}

// Search queries the indexer and normalizes the feed.
func (idx *TorznabIndexer) Search(ctx context.Context, q Query) ([]Result, error) {
	if idx.base == "" || idx.apiKey == "" {
		return nil, ErrNotConfigured
	}
	// base is the full Torznab endpoint (…/api); only the query string is appended.
	u, err := url.Parse(idx.base)
	if err != nil {
		return nil, fmt.Errorf("torrent: parse torznab URL: %w", err)
	}
	params := u.Query()
	params.Set("apikey", idx.apiKey)
	// Every search this backend issues is an anime search; 5000/5070 are the
	// Torznab anime categories.
	params.Set("cat", "5000,5070")
	params.Set("limit", strconv.Itoa(clampLimit(q.Limit)))
	if q.Season != nil || q.Episode != nil {
		params.Set("t", "tvsearch")
		if q.Season != nil {
			params.Set("season", strconv.Itoa(*q.Season))
		}
		if q.Episode != nil {
			params.Set("ep", strconv.Itoa(*q.Episode))
		}
	} else {
		params.Set("t", "search")
	}
	params.Set("q", q.Q)
	u.RawQuery = params.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("torrent: build request: %w", err)
	}
	resp, err := idx.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("torrent: query indexer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("torrent: indexer returned status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("torrent: read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("torrent: response too large (%d byte cap)", maxResponseBytes)
	}
	return parseTorznabFeed(body)
}

// clampLimit applies the Query contract: unset means 50 and 100 is the cap.
func clampLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultLimit
	case limit > maxLimit:
		return maxLimit
	default:
		return limit
	}
}
