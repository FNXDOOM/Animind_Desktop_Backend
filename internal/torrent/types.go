// Package torrent talks to a Torznab indexer for anime release searches.
package torrent

import (
	"context"
	"errors"
	"time"

	"animind-backend-go/internal/scanner"
)

// ErrNotConfigured is returned by Search when no Torznab endpoint is configured.
var ErrNotConfigured = errors.New("torrent: indexer not configured")

// Query describes one search request. Resolution, Codec, MinSeeders, Batch and
// Sort are applied client-side by the search service; the Torznab request only
// carries Q, Season, Episode and Limit.
type Query struct {
	Q          string
	Season     *int
	Episode    *int
	Resolution *int
	Codec      *string
	MinSeeders *int
	Batch      string // "any" | "only" | "exclude"
	Sort       string // "relevance" | "seeders" | "size" | "date"
	Limit      int    // default 50, cap 100
}

// Result is one normalized search hit. Magnet is internal only and must never
// be serialized to HTTP responses.
type Result struct {
	ID          string
	InfoHash    string
	Title       string
	SizeBytes   int64
	Seeders     int
	Leechers    int
	Grabs       int
	PublishedAt time.Time
	Indexer     string
	Category    string // "anime" | "other"
	Parsed      scanner.TorrentTitle
	Health      string // excellent | good | poor | dead
	Trusted     bool
	Magnet      string
}

// Indexer is the source the search service queries.
type Indexer interface {
	Search(ctx context.Context, q Query) ([]Result, error)
}
