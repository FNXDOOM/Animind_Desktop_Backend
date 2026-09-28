package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"animind-backend-go/internal/config"
)

// MyAnimeList is the fallback source. It answers with different field names and no
// AniList id, so the orchestrator normalises it into the same Metadata shape rather than
// making the scanner branch per provider.

const malAPIBase = "https://api.myanimelist.net/v2"

// malFields asks for exactly the subset the library stores. MAL bills itself on field
// count, so an unbounded request is both slower and unnecessary.
const malFields = "id,title,main_picture,alternative_titles,synopsis,mean,num_episodes,status,start_season,genres,studios"

// MALAnime is the node returned by the MAL search endpoint.
type MALAnime struct {
	ID      int    `json:"id"`
	Title   string `json:"title"`
	Picture struct {
		Medium *string `json:"medium"`
		Large  *string `json:"large"`
	} `json:"main_picture"`
	AlternativeTitles struct {
		Synonyms []string `json:"synonyms"`
		English  *string  `json:"en"`
		Japanese *string  `json:"ja"`
	} `json:"alternative_titles"`
	Synopsis    *string  `json:"synopsis"`
	Mean        *float64 `json:"mean"`
	NumEpisodes *int     `json:"num_episodes"`
	Status      *string  `json:"status"`
	StartSeason *struct {
		Year   *int    `json:"year"`
		Season *string `json:"season"`
	} `json:"start_season"`
	Genres []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"genres"`
	Studios []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"studios"`
}

// MALClient calls the MyAnimeList v2 API with a client id header.
type MALClient struct {
	http     *http.Client
	clientID string
	enabled  bool
	maxRetry int
}

// NewMAL builds the client. Missing credentials disable it with one log line rather than
// failing every scan.
func NewMAL(cfg *config.Config, logger *slog.Logger) *MALClient {
	c := &MALClient{
		http:     &http.Client{Timeout: cfg.Metadata.MALTimeout},
		clientID: cfg.Metadata.MALClientID,
		enabled:  cfg.Metadata.MALEnabled,
		maxRetry: cfg.Metadata.MALMaxAttempts,
	}
	if c.enabled && c.clientID == "" {
		slog.Info("MyAnimeList metadata fallback disabled: MYANIMELIST_CLIENT_ID is not set")
		c.enabled = false
	}
	return c
}

// FetchMeta searches for the best match on title.
func (c *MALClient) FetchMeta(ctx context.Context, title string) (*MALAnime, error) {
	if !c.enabled {
		return nil, nil
	}

	params := url.Values{}
	params.Set("q", title)
	params.Set("limit", "1")
	params.Set("nsfw", "true")
	params.Set("fields", malFields)
	endpoint := malAPIBase + "/anime?" + params.Encode()

	var lastErr error
	for attempt := 1; attempt <= c.maxRetry; attempt++ {
		anime, err := c.once(ctx, endpoint)
		if err == nil {
			return anime, nil
		}
		lastErr = err
		if !isTransient(err) || attempt == c.maxRetry {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(500*(1<<(attempt-1))) * time.Millisecond):
		}
	}

	slog.Warn("MyAnimeList metadata lookup failed",
		slog.String("title", title), slog.String("error", describe(lastErr)))
	return nil, lastErr
}

func (c *MALClient) once(ctx context.Context, endpoint string) (*MALAnime, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// MAL authenticates server-to-server searches with a client id header, not a bearer
	// token; without it the API answers 403.
	req.Header.Set("X-MAL-CLIENT-ID", c.clientID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("metadata: mal request: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: HTTP %d from MyAnimeList", ErrUpstreamRejected, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metadata: mal HTTP %d: %s", resp.StatusCode, trim(string(raw), 200))
	}

	var envelope struct {
		Data []struct {
			Node MALAnime `json:"node"`
		} `json:"data"`
		Message string `json:"message"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("metadata: decode mal response: %w", err)
	}
	if envelope.Message != "" || envelope.Error != "" {
		return nil, errors.New(trim(orEmpty(envelope.Message, envelope.Error), 200))
	}
	if len(envelope.Data) == 0 {
		return nil, nil
	}
	return &envelope.Data[0].Node, nil
}

func orEmpty(primary, fallback string) string {
	if primary != "" {
		return primary
	}
	return fallback
}
