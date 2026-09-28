package metadata

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"animind-backend-go/internal/config"
)

// AniList is the primary metadata source. The scanner asks it for every new title so a
// folder name like "Frieren - 01" becomes a card with artwork, synopsis and studio.

// mediaQuery requests one anime matched by search, preferring the closest match.
const mediaQuery = `query($search: String!) {
  Media(search: $search, type: ANIME, sort: SEARCH_MATCH) {
    id
    title { romaji english }
    description(asHtml: false)
    coverImage { large medium }
    episodes
    genres
    averageScore
    status
    season
    seasonYear
    studios(isMain: true) { nodes { name } }
    trailer { id site thumbnail }
  }
}`

// Media is the AniList node the scanner needs.
type Media struct {
	ID    int `json:"id"`
	Title struct {
		Romaji  string  `json:"romaji"`
		English *string `json:"english"`
	} `json:"title"`
	Description *string `json:"description"`
	CoverImage  struct {
		Large  *string `json:"large"`
		Medium *string `json:"medium"`
	} `json:"coverImage"`
	Episodes   *int     `json:"episodes"`
	Genres     []string `json:"genres"`
	Average    *int     `json:"averageScore"`
	Status     string   `json:"status"`
	Season     *string  `json:"season"`
	SeasonYear *int     `json:"seasonYear"`
	Studios    struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"studios"`
	Trailer *struct {
		ID        string  `json:"id"`
		Site      string  `json:"site"`
		Thumbnail *string `json:"thumbnail"`
	} `json:"trailer"`
}

// RomajiTitle is the name to search with when no English title exists.
func (m *Media) RomajiTitle() string { return m.Title.Romaji }

// AniListClient calls the AniList GraphQL endpoint with retry and rate-limit respect.
type AniListClient struct {
	endpoint string
	http     *http.Client
	maxRetry int
	enabled  bool

	mu               sync.Mutex
	rateLimitedUntil time.Time
}

// NewAniList builds the client from configuration.
func NewAniList(cfg *config.Config, logger *slog.Logger) *AniListClient {
	return &AniListClient{
		endpoint: cfg.Metadata.AniListEndpoint,
		http:     &http.Client{Timeout: cfg.Metadata.AniListTimeout},
		maxRetry: cfg.Metadata.AniListMaxAttempts,
		enabled:  cfg.Metadata.AniListEnabled,
	}
}

// ErrUpstreamRejected reports a non-2xx answer from AniList. Callers log it and fall back
// to another source; the library scan must not fail because a metadata site is unhappy.
var ErrUpstreamRejected = errors.New("metadata: upstream rejected the request")

// FetchMeta looks up one title. A nil result means "no metadata available", which is a
// normal outcome rather than an error.
func (c *AniListClient) FetchMeta(ctx context.Context, title string) (*Media, error) {
	if !c.enabled {
		return nil, nil
	}
	if time.Now().Before(c.cooldownUntil()) {
		// A recent 429 means further requests would only deepen the penalty.
		return nil, nil
	}

	body, err := json.Marshal(map[string]any{"query": mediaQuery, "variables": map[string]string{"search": title}})
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= c.maxRetry; attempt++ {
		media, err := c.once(ctx, body)
		if err == nil {
			return media, nil
		}
		lastErr = err
		if !isTransient(err) || attempt == c.maxRetry {
			break
		}
		delay := time.Duration(300*(1<<(attempt-1))) * time.Millisecond
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	slog.Warn("AniList metadata lookup failed",
		slog.String("title", title), slog.String("error", describe(lastErr)))
	return nil, lastErr
}

func (c *AniListClient) once(ctx context.Context, body []byte) (*Media, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "AniMind/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metadata: anilist request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusTooManyRequests {
		c.noteRateLimited(resp.Header.Get("Retry-After"))
		return nil, fmt.Errorf("%w: HTTP 429 from AniList", ErrUpstreamRejected)
	}
	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: HTTP %d from AniList", ErrUpstreamRejected, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata: anilist HTTP %d: %s", resp.StatusCode, trim(string(raw), 200))
	}

	var envelope struct {
		Data struct {
			Media *Media `json:"Media"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("metadata: decode anilist response: %w", err)
	}
	if len(envelope.Errors) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrUpstreamRejected, envelope.Errors[0].Message)
	}
	return envelope.Data.Media, nil
}

// noteRateLimited records how long AniList asked us to wait. A missing or unparsable
// Retry-After falls back to a minute.
func (c *AniListClient) noteRateLimited(retryAfter string) {
	seconds := 60
	if parsed, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && parsed > 0 {
		seconds = parsed
	}
	until := time.Now().Add(time.Duration(seconds) * time.Second)

	c.mu.Lock()
	if until.After(c.rateLimitedUntil) {
		c.rateLimitedUntil = until
	}
	c.mu.Unlock()
}

func (c *AniListClient) cooldownUntil() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rateLimitedUntil
}

// isTransient identifies the failures worth retrying: throttling, upstream faults and
// transport errors. A 4xx other than 429 is a permanent answer for this title.
func isTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrUpstreamRejected) {
		return true
	}
	var netErr interface{ Timeout() bool }
	return errors.As(err, &netErr) && netErr.Timeout()
}

func describe(err error) string {
	if err == nil {
		return "unknown error"
	}
	return trim(err.Error(), 300)
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
